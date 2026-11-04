package gitserver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage"
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
