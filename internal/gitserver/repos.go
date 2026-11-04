package gitserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
)

// ErrNoRepository is returned for an app without a repository.
var ErrNoRepository = errors.New("no such repository")

// Repos keeps the apps' bare repositories, <dir>/<app>.git, and serialises
// the updates of each one's refs: the git library updates refs one by one,
// without a transaction (spike S1).
type Repos struct {
	dir string

	mu    sync.Mutex
	state map[string]*repoState
}

// repoState is a repository's lock, and the refs whose pushes are under way.
type repoState struct {
	mu   sync.Mutex
	busy map[string]bool
}

// NewRepos keeps repositories under dir.
func NewRepos(dir string) *Repos {
	return &Repos{dir: dir, state: map[string]*repoState{}}
}

// Path is where app's repository is.
func (rs *Repos) Path(app string) string { return filepath.Join(rs.dir, app+".git") }

// Exists reports whether app has a repository.
func (rs *Repos) Exists(app string) bool {
	_, err := os.Stat(filepath.Join(rs.Path(app), "HEAD"))
	return err == nil
}

// Export writes the tree of a commit of app's repository into dir, which must be
// empty: for building again what was pushed.
func (rs *Repos) Export(app, commit, dir string) (Exported, error) {
	st, err := rs.open(app)
	if err != nil {
		return Exported{}, err
	}
	defer closeStore(st)
	return export(st, plumbing.NewHash(commit), dir, DefaultExportLimits)
}

// BranchHead returns the newest commit of a branch of app's repository, and its message.
func (rs *Repos) BranchHead(app, branch string) (commit, message string, err error) {
	st, err := rs.open(app)
	if err != nil {
		return "", "", err
	}
	defer closeStore(st)
	ref, err := st.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return "", "", err
	}
	c, err := object.GetCommit(st, ref.Hash())
	if err != nil {
		return "", "", err
	}
	return ref.Hash().String(), c.Message, nil
}

// closeStore lets go of a repository's open files: elsewhere they would stop its directory
// from being moved or removed, as on Windows.
func closeStore(st storage.Storer) {
	if c, ok := st.(io.Closer); ok {
		c.Close()
	}
}

// open opens app's repository. It is opened again for every request, so
// that it sees the objects other pushes have added since. Whoever opens it closes it.
func (rs *Repos) open(app string) (storage.Storer, error) {
	if !rs.Exists(app) {
		return nil, ErrNoRepository
	}
	r, err := git.PlainOpen(rs.Path(app))
	if err != nil {
		return nil, fmt.Errorf("open the repository of %s: %w", app, err)
	}
	return r.Storer, nil
}

// create makes app's repository, whose default branch is main.
func (rs *Repos) create(app string) (storage.Storer, error) {
	if err := os.MkdirAll(rs.dir, 0o755); err != nil {
		return nil, err
	}
	r, err := git.PlainInit(rs.Path(app), true, git.WithDefaultBranch(plumbing.NewBranchReferenceName("main")))
	if errors.Is(err, git.ErrTargetDirNotEmpty) {
		return rs.open(app) // another push created it meanwhile
	}
	if err != nil {
		return nil, fmt.Errorf("create the repository of %s: %w", app, err)
	}
	return r.Storer, nil
}

// Remove deletes app's repository.
func (rs *Repos) Remove(app string) error {
	return os.RemoveAll(rs.Path(app))
}

func (rs *Repos) lock(app string) *repoState {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	s := rs.state[app]
	if s == nil {
		s = &repoState{busy: map[string]bool{}}
		rs.state[app] = s
	}
	return s
}

// defaultBranch returns the branch HEAD points to: the app's production
// branch.
func defaultBranch(st storage.Storer) string {
	head, err := st.Reference(plumbing.HEAD)
	if err != nil || head.Type() != plumbing.SymbolicReference {
		return "main"
	}
	return head.Target().Short()
}

// empty reports whether a repository has no refs but HEAD.
func empty(st storage.Storer) bool {
	refs, err := st.IterReferences()
	if err != nil {
		return false
	}
	defer refs.Close()
	found := false
	_ = refs.ForEach(func(r *plumbing.Reference) error {
		if r.Name() != plumbing.HEAD {
			found = true
			return storer.ErrStop
		}
		return nil
	})
	return !found
}

// setDefaultBranch points HEAD to branch.
func setDefaultBranch(st storage.Storer, branch string) error {
	return st.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch)))
}

// ValidRef says whether a name could be a ref.
func ValidRef(name string) bool { return plumbing.ReferenceName(name).Validate() == nil }

// FetchOptions say where a repository is, on a forge.
type FetchOptions struct {
	URL, Token string
	// Client is what to reach it with.
	Client *http.Client
	// DefaultBranch is what its default branch is, for whoever clones it from here.
	DefaultBranch string
}

func (o FetchOptions) clientOptions() []client.Option {
	var options []client.Option
	if o.Client != nil {
		options = append(options, client.WithHTTPClient(o.Client))
	}
	if o.Token != "" {
		options = append(options, client.WithHTTPAuth(&githttp.BasicAuth{Username: "x-access-token", Password: o.Token}))
	}
	return options
}

func (o FetchOptions) remote(st storage.Storer) *git.Remote {
	return git.NewRemote(st, &config.RemoteConfig{Name: "forge", URLs: []string{o.URL}})
}

// tidy takes a token out of an error, in case something has put it there.
func (o FetchOptions) tidy(err error) error {
	if err == nil || o.Token == "" || !strings.Contains(err.Error(), o.Token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), o.Token, "..."))
}

// Fetch brings refs of a remote repository into app's, which it makes if it is not there yet.
func (rs *Repos) Fetch(ctx context.Context, app string, o FetchOptions, refspecs ...string) error {
	specs := make([]config.RefSpec, len(refspecs))
	prune := false
	for i, spec := range refspecs {
		specs[i] = config.RefSpec(spec)
		if err := specs[i].Validate(); err != nil {
			return fmt.Errorf("fetch %s: %w", spec, err)
		}
		prune = prune || specs[i].IsWildcard()
	}
	state := rs.lock(app)
	state.mu.Lock()
	defer state.mu.Unlock()
	st, err := rs.create(app)
	if err != nil {
		return err
	}
	defer closeStore(st)
	if o.DefaultBranch != "" {
		if err := setDefaultBranch(st, o.DefaultBranch); err != nil {
			return err
		}
	}
	err = o.remote(st).FetchContext(ctx, &git.FetchOptions{RefSpecs: specs, Force: true, Prune: prune, Tags: plumbing.NoTags, ClientOptions: o.clientOptions()})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return o.tidy(err)
	}
	return nil
}

// RemoteDefaultBranch says what a remote repository's default branch is, or nothing if it does not say: it has no
// commits yet, say. That the repository could be read at all is worth knowing, too.
func (rs *Repos) RemoteDefaultBranch(ctx context.Context, o FetchOptions) (string, error) {
	refs, err := o.remote(memory.NewStorage()).ListContext(ctx, &git.ListOptions{ClientOptions: o.clientOptions()})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return "", nil
		}
		return "", o.tidy(err)
	}
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference && ref.Target().IsBranch() {
			return ref.Target().Short(), nil
		}
	}
	return "", nil
}

// Ref says which commit a ref of app's repository is at.
func (rs *Repos) Ref(app, name string) (string, error) {
	st, err := rs.open(app)
	if err != nil {
		return "", err
	}
	defer closeStore(st)
	ref, err := st.Reference(plumbing.ReferenceName(name))
	if err != nil {
		return "", err
	}
	return ref.Hash().String(), nil
}

// RefsUnder lists the refs below a prefix, by what follows it. A repository that is not there has none.
func (rs *Repos) RefsUnder(app, prefix string) (map[string]string, error) {
	st, err := rs.open(app)
	if errors.Is(err, ErrNoRepository) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closeStore(st)
	iter, err := st.IterReferences()
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	found := map[string]string{}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if rest, under := strings.CutPrefix(ref.Name().String(), prefix); under && ref.Type() == plumbing.HashReference {
			found[rest] = ref.Hash().String()
		}
		return nil
	})
	return found, err
}

// RemoveRef takes a ref of app's repository away.
func (rs *Repos) RemoveRef(app, name string) error {
	state := rs.lock(app)
	state.mu.Lock()
	defer state.mu.Unlock()
	st, err := rs.open(app)
	if errors.Is(err, ErrNoRepository) {
		return nil
	}
	if err != nil {
		return err
	}
	defer closeStore(st)
	return st.RemoveReference(plumbing.ReferenceName(name))
}

// MoveBranch has a branch of app's repository at a commit, or takes it away.
func (rs *Repos) MoveBranch(app, branch, commit string) error {
	if commit == "" {
		return rs.RemoveRef(app, plumbing.NewBranchReferenceName(branch).String())
	}
	state := rs.lock(app)
	state.mu.Lock()
	defer state.mu.Unlock()
	st, err := rs.open(app)
	if err != nil {
		return err
	}
	defer closeStore(st)
	return st.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), plumbing.NewHash(commit)))
}

// PushFor makes a push out of what was brought here some other way, to be dealt with as any push is. The function it
// answers with lets go of what it holds.
func (rs *Repos) PushFor(app, by, defaultBranch string, updates ...Update) (*Push, func(), error) {
	st, err := rs.open(app)
	if err != nil {
		return nil, nil, err
	}
	for i := range updates {
		if updates[i].New == "" {
			continue
		}
		if c, err := object.GetCommit(st, plumbing.NewHash(updates[i].New)); err == nil {
			updates[i].Message = c.Message
		}
	}
	return &Push{App: app, By: by, DefaultBranch: defaultBranch, Updates: updates, Progress: io.Discard, st: st}, func() { closeStore(st) }, nil
}
