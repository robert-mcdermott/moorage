// Package forge connects an app to a repository on a forge: a change there deploys, a pull request gets a preview of its
// own and is told how that went, and code from someone who cannot push there waits for someone here to approve it.
package forge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/gitserver"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/notify"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// maxEvent is the most a forge sends in one event.
const maxEvent = 25 << 20

// fetchTimeout is as long as a repository may take to arrive.
const fetchTimeout = 15 * time.Minute

// Where what has been brought from a forge is kept, apart from the branches that have been dealt with.
const (
	headsRef = "refs/moorage/forge/heads/"
	pullsRef = "refs/moorage/forge/pull/"
)

// InvalidError says what is wrong with what someone asked for.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// ErrNoConnection says that an app is not connected to a forge.
var ErrNoConnection = errors.New("this app is not connected to a repository")

// Keys keeps what a connection has to keep secret.
type Keys interface {
	SealForge(appID, name, value string) []byte
	OpenForge(appID, name string, sealed []byte) (string, error)
}

// Deliverer does with a change what a push does.
type Deliverer interface {
	Deliver(ctx context.Context, p *gitserver.Push) (store.Deployment, error)
}

// Service looks after apps' connections.
type Service struct {
	DB     *store.DB
	Repos  *gitserver.Repos
	Pushes Deliverer
	Keys   Keys
	Log    *slog.Logger
	// Client tells a forge things, and Fetcher fetches from one, which takes longer. Neither reaches what
	// [notifications] allow does not let it.
	Client, Fetcher *http.Client
	// EnvURL is where an environment is, and AppURL where its console page is.
	EnvURL func(app, env string) string
	AppURL func(app string) string
	// Tick is how often forges that have to be looked at are.
	Tick time.Duration
	// Audit keeps a record of what a forge's word did.
	Audit func(ctx context.Context, by, action, target, result string)
	// Life ends what goes on after a request has been answered.
	Life context.Context

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	looking map[string]bool
	going   sync.WaitGroup
}

// Run looks at the forges that have to be looked at, until Life ends. A server that stopped part of the way through a
// build has left a check saying that it is under way, which could hold a pull request up for ever, so it says otherwise.
func (s *Service) Run() {
	s.interrupted()
	s.catchUp()
	tick := s.Tick
	if tick == 0 {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-s.Life.Done():
			return
		case <-t.C:
			conns, err := s.DB.PolledForges(s.Life)
			if err != nil {
				if s.Life.Err() == nil {
					s.Log.Warn("read the forges to look at", "err", err)
				}
				continue
			}
			for _, c := range conns {
				s.startLook(c, false)
			}
		}
	}
}

// Wait waits a little for what is going on. A deployment that someone is following can outlast this, and what comes of
// it is seen to when the server next starts.
func (s *Service) Wait() {
	done := make(chan struct{})
	go func() {
		s.going.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// catchUp looks at the repositories that have been dealt with before. A forge that told of a push while the server was
// away does not tell of it again, and what had been told of when it stopped is not on record.
func (s *Service) catchUp() {
	conns, err := s.DB.AllForges(s.Life)
	if err != nil {
		s.Log.Warn("read the forges to catch up with", "err", err)
		return
	}
	for _, c := range conns {
		// Not one that has been connected and has heard nothing yet, as connecting deploys nothing. Nor is this a second try
		// at what failed, or a reason to replace what someone has since pushed to this server: only what changed there.
		if !c.LookedAt.IsZero() || !c.LastEventAt.IsZero() {
			s.startLook(c, false)
		}
	}
}

// startLook has a look go on by itself, as one forge that is slow, or one app that takes a while to build, is not to hold
// up the rest. A look that is still going is left to it.
func (s *Service) startLook(c store.ForgeConnection, again bool) {
	s.mu.Lock()
	if s.looking == nil {
		s.looking = map[string]bool{}
	}
	if s.looking[c.AppID] {
		s.mu.Unlock()
		return
	}
	s.looking[c.AppID] = true
	s.mu.Unlock()
	s.background(func(ctx context.Context) {
		defer func() {
			s.mu.Lock()
			delete(s.looking, c.AppID)
			s.mu.Unlock()
		}()
		s.look(ctx, c.AppID, again)
	})
}

func (s *Service) interrupted() {
	conns, err := s.DB.AllForges(s.Life)
	if err != nil {
		s.Log.Warn("read the forges", "err", err)
		return
	}
	for _, c := range conns {
		commits, err := s.DB.ForgeDeploying(s.Life, c.AppID)
		if err != nil {
			s.Log.Warn("read the deployments under way", "app", c.App, "err", err)
			continue
		}
		for _, commit := range commits {
			s.report(s.Life, s.remoteOf(c), commit, StateFailure, s.AppURL(c.App), "The server stopped during this deployment")
			if err := s.DB.ForgeDeployed(s.Life, c.AppID, commit); err != nil {
				s.Log.Warn("note a deployment that was interrupted", "app", c.App, "err", err)
			}
		}
	}
	pulls, err := s.DB.ForgePullsBuilding(s.Life)
	if err != nil {
		s.Log.Warn("read the pull requests being built", "err", err)
		return
	}
	for _, pull := range pulls {
		pull.State, pull.Error, pull.UpdatedAt = store.PullFailed, "The server stopped while this was being built. Push again, or build it again from the console.", time.Now().UTC()
		if err := s.DB.SetForgePull(s.Life, pull); err != nil {
			s.Log.Warn("note a pull request that was interrupted", "pull", pull.Number, "err", err)
			continue
		}
		if c, err := s.DB.ForgeOfAppID(s.Life, pull.AppID); err == nil {
			s.report(s.Life, s.remoteOf(c), pull.HeadSHA, StateFailure, s.AppURL(c.App), "The server stopped while this was being built")
		}
	}
}

func (s *Service) background(work func(ctx context.Context)) {
	s.going.Add(1)
	go func() {
		defer s.going.Done()
		work(s.Life)
	}()
}

// lockOf keeps what happens to one branch or pull request in order: two pushes in a row should deploy in that order. What
// happens to others goes on, as a preview that takes a while to build is not to hold up production.
func (s *Service) lockOf(appID, what string) func() {
	key := appID + "\x00" + what
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	l := s.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Connection is what someone asks for.
type Connection struct {
	URL, Kind, APIURL, Token, DefaultBranch string
	NoPreviews, Poll                        bool
}

// Connect keeps where an app's code is. It answers with the secret that a forge signs what it sends with, which is not
// shown again, and so is empty if the app was connected already: changing how it is connected leaves its hook as it is.
func (s *Service) Connect(ctx context.Context, appID, app string, c Connection, by string) (store.ForgeConnection, string, error) {
	where, err := Where(c.Kind, c.URL, c.APIURL)
	if err != nil {
		return store.ForgeConnection{}, "", err
	}
	repository := repositoryURL(where.Kind, c.URL)
	if c.Token != "" && (!notify.SafeForSecrets(repository) || (where.APIURL != "" && !notify.SafeForSecrets(where.APIURL))) {
		return store.ForgeConnection{}, "", &InvalidError{"A token is not sent in the clear to a forge across the internet: use its https address, for its API too."}
	}
	old, err := s.DB.ForgeOf(ctx, app)
	there := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return old, "", err
	}
	conn := store.ForgeConnection{AppID: appID, App: app, Kind: where.Kind, URL: repository, APIURL: where.APIURL, Repo: where.Repo, DefaultBranch: c.DefaultBranch, BranchPinned: c.DefaultBranch != "",
		Previews: !c.NoPreviews, Poll: c.Poll, CreatedBy: by, CreatedAt: time.Now().UTC()}
	secret := ""
	if there {
		conn.HookID, conn.SealedSecret = old.HookID, old.SealedSecret
		// A token goes with the forge it was given for: whoever may change the address should not get it sent elsewhere.
		if old.URL == conn.URL && old.APIURL == conn.APIURL {
			conn.SealedToken = old.SealedToken
		}
	} else {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return conn, "", err
		}
		secret = hex.EncodeToString(raw)
		conn.HookID = ids.New(ids.Hook)
		conn.SealedSecret = s.Keys.SealForge(appID, "secret", secret)
	}
	if c.Token != "" {
		conn.SealedToken = s.Keys.SealForge(appID, "token", c.Token)
	}
	// Finding its default branch shows whether it can be reached, too. A forge that cannot yet is not a reason to
	// refuse, and it can say so.
	ask, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	found, err := s.Repos.RemoteDefaultBranch(ask, s.fetchOptions(conn))
	if err != nil {
		conn.LastError = "The repository cannot be read: " + err.Error()
	}
	if conn.DefaultBranch == "" {
		conn.DefaultBranch = found
	}
	if conn.DefaultBranch == "" {
		conn.DefaultBranch = "main"
	}
	if err := s.DB.SetForge(ctx, conn); err != nil {
		return conn, "", err
	}
	return conn, secret, nil
}

// Disconnect lets go of a connection. What it deployed stays.
func (s *Service) Disconnect(ctx context.Context, app string) error {
	c, err := s.DB.ForgeOf(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNoConnection
	}
	if err != nil {
		return err
	}
	return s.DB.DeleteForge(ctx, c.AppID)
}

func (s *Service) remoteOf(c store.ForgeConnection) Remote {
	return Remote{Kind: c.Kind, APIURL: c.APIURL, Repo: c.Repo, Client: s.Client, Token: s.tokenOf(c)}
}

func (s *Service) tokenOf(c store.ForgeConnection) string {
	if len(c.SealedToken) == 0 {
		return ""
	}
	token, err := s.Keys.OpenForge(c.AppID, "token", c.SealedToken)
	if err != nil {
		s.Log.Warn("open a forge's token", "app", c.App, "err", err)
		return ""
	}
	return token
}

func (s *Service) fetchOptions(c store.ForgeConnection) gitserver.FetchOptions {
	return gitserver.FetchOptions{URL: c.URL, Token: s.tokenOf(c), Client: s.Fetcher, DefaultBranch: c.DefaultBranch}
}

// Hook takes what a forge sent to a hook's address, and answers with a status: what it asks for goes on from there, as a
// forge waits a few seconds at most.
func (s *Service) Hook(ctx context.Context, hook string, header http.Header, from io.Reader) int {
	c, err := s.DB.ForgeByHook(ctx, hook)
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound
	}
	if err != nil {
		s.Log.Warn("read a forge's connection", "err", err)
		return http.StatusInternalServerError
	}
	body, err := io.ReadAll(io.LimitReader(from, maxEvent+1))
	if err != nil {
		return http.StatusBadRequest
	}
	if len(body) > maxEvent {
		return http.StatusRequestEntityTooLarge
	}
	secret, err := s.Keys.OpenForge(c.AppID, "secret", c.SealedSecret)
	if err != nil {
		s.Log.Warn("open a forge's secret", "app", c.App, "err", err)
		return http.StatusInternalServerError
	}
	if !Verified(c.Kind, secret, header, body) {
		s.Log.Warn("a request to a forge's hook was not signed with its secret", "app", c.App)
		return http.StatusUnauthorized
	}
	event, err := ParseEvent(c.Kind, header, body)
	if err != nil {
		s.Log.Warn("read what a forge sent", "app", c.App, "err", err)
		return http.StatusBadRequest
	}
	if event.Kind != Ignored {
		s.background(func(ctx context.Context) { s.handle(ctx, c, event) })
	}
	return http.StatusAccepted
}

// handle does what an event asks for, one at a time for each branch or pull request.
func (s *Service) handle(ctx context.Context, c store.ForgeConnection, event Event) {
	what := "branch " + event.Branch
	if event.Kind != Push {
		what = previewBranch(event.Number)
	}
	defer s.lockOf(c.AppID, what)()
	// What came before this may have changed it, while this one waited, or taken it away. It goes by the app's ID, as
	// another app may have the name by now.
	c, err := s.DB.ForgeOfAppID(ctx, c.AppID)
	if err != nil {
		return
	}
	// A hook that goes on being sent from elsewhere after a change of repository, or one for a whole organization, is
	// about something else. What is in a pull request of that number here is not what was vouched for there.
	if event.Repository != "" && c.Repo != "" && !sameRepository(c.Repo, event.Repository) {
		s.note(ctx, c, "an event", fmt.Errorf("the forge told of %s, and this is connected to %s", event.Repository, c.Repo))
		return
	}
	if !c.BranchPinned && event.DefaultBranch != "" && event.DefaultBranch != c.DefaultBranch {
		c.DefaultBranch = event.DefaultBranch
		if err := s.DB.SetForgeDefaultBranch(ctx, c.AppID, c.DefaultBranch); err != nil {
			s.Log.Warn("note a forge's default branch", "app", c.App, "err", err)
		}
	}
	switch event.Kind {
	case Push:
		if event.Commit == "" {
			err = s.branchGone(ctx, c, event.Branch, forgeUser(c, event.Author))
		} else {
			err = s.pushed(ctx, c, event.Branch, forgeUser(c, event.Author))
		}
	case PullChanged:
		err = s.pullChanged(ctx, c, event)
	case PullClosed:
		err = s.pullClosed(ctx, c, event)
	}
	s.note(ctx, c, event.Kind, err)
}

func (s *Service) note(ctx context.Context, c store.ForgeConnection, what string, err error) {
	problem := ""
	if err != nil {
		problem = firstLine(err.Error())
		s.Log.Warn("what a forge told of went wrong", "app", c.App, "what", what, "err", err)
	}
	if err := s.DB.NoteForgeEvent(context.WithoutCancel(ctx), c.AppID, problem, time.Now()); err != nil {
		s.Log.Warn("note what a forge told of", "app", c.App, "err", err)
	}
}

// sameRepository says whether what a forge calls a repository is the one at this path, as a forge can be under a path of
// its own, which it would leave out.
func sameRepository(path, name string) bool {
	return strings.HasSuffix("/"+strings.ToLower(path), "/"+strings.ToLower(name))
}

// forgeUser is who a forge says did something, as told apart from an account here.
func forgeUser(c store.ForgeConnection, name string) string {
	if name == "" {
		return c.Kind
	}
	return c.Kind + ":" + name
}

// fetch brings a ref of the repository here, and says what commit it is at. What is deployed is what that says, and not
// what anyone said it would be.
func (s *Service) fetch(ctx context.Context, c store.ForgeConnection, from, to string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if err := s.Repos.Fetch(ctx, c.App, s.fetchOptions(c), "+"+from+":"+to); err != nil {
		return "", err
	}
	return s.Repos.Ref(c.App, to)
}

// branchRule says whether a branch gets a preview for being pushed. Hearing of pull requests, it does not: each has a
// preview of its own, and a branch that had one too would be built twice.
func branchRule(c store.ForgeConnection) string {
	if !c.Previews {
		return gitserver.PreviewNever
	}
	if c.Poll || c.Kind == Generic {
		return gitserver.PreviewAsManifest
	}
	return gitserver.PreviewNever
}

// keptNote catches why what was brought here is not deployed, as a push would tell whoever pushed.
type keptNote struct{ why, stays string }

func (k *keptNote) Write(line []byte) (int, error) {
	if _, why, found := strings.Cut(string(line), "is kept, not deployed: "); found {
		k.why = strings.TrimSpace(why)
	}
	if text := string(line); strings.Contains(text, "moorage  the preview ") && strings.Contains(text, " stays: ") {
		_, k.stays, _ = strings.Cut(strings.TrimSpace(text), "moorage  ")
	}
	return len(line), nil
}

// deliver has a change do what a push does: decide where it goes, and deploy it there. A deployment would make an app that
// is not there, so it looks first: this one may have been deleted or renamed while what it is deploying was being fetched.
func (s *Service) deliver(ctx context.Context, c store.ForgeConnection, u gitserver.Update, by, rule string, started func()) (store.Deployment, keptNote, error) {
	var kept keptNote
	current, err := s.DB.ForgeOfAppID(ctx, c.AppID)
	if err != nil {
		return store.Deployment{}, kept, err
	}
	if current.App != c.App {
		return store.Deployment{}, kept, fmt.Errorf("%s is called %s now, and the next change will go there", c.App, current.App)
	}
	p, done, err := s.Repos.PushFor(c.App, by, c.DefaultBranch, u)
	if err != nil {
		return store.Deployment{}, kept, err
	}
	defer done()
	p.Preview, p.Started, p.Progress = rule, started, &kept
	dep, err := s.Pushes.Deliver(ctx, p)
	result := "ok"
	if err != nil {
		result = "failed"
	}
	if s.Audit != nil && dep.ID != "" {
		s.Audit(context.WithoutCancel(ctx), by, "forge.deploy", c.App, result)
	}
	return dep, kept, err
}

var pullBranch = regexp.MustCompile(`^pr-[0-9]+$`)

// pushed brings a branch's change here, and deploys it if it should.
func (s *Service) pushed(ctx context.Context, c store.ForgeConnection, branch, by string) error {
	if pullBranch.MatchString(branch) {
		return fmt.Errorf("a branch called %s is left alone, as that is what a pull request's preview goes by", branch)
	}
	commit, err := s.fetch(ctx, c, "refs/heads/"+branch, headsRef+branch)
	if err != nil {
		return err
	}
	return s.deployBranch(ctx, c, branch, commit, by)
}

func (s *Service) deployBranch(ctx context.Context, c store.ForgeConnection, branch, commit, by string) error {
	old, _, _ := s.Repos.BranchHead(c.App, branch)
	if commit == old {
		return nil // what is here already: it told twice, or someone looked
	}
	remote := s.remoteOf(c)
	lasting := context.WithoutCancel(ctx)
	underWay := false
	dep, _, err := s.deliver(ctx, c, gitserver.Update{Ref: "refs/heads/" + branch, Old: old, New: commit}, by, branchRule(c), func() {
		underWay = true
		if err := s.DB.NoteForgeDeploying(lasting, c.AppID, commit); err != nil {
			s.Log.Warn("note a deployment", "app", c.App, "err", err)
		}
		s.report(ctx, remote, commit, StatePending, s.AppURL(c.App), "Deploying")
	})
	if underWay {
		defer func() {
			if err := s.DB.ForgeDeployed(lasting, c.AppID, commit); err != nil {
				s.Log.Warn("note a deployment", "app", c.App, "err", err)
			}
		}()
	}
	if err != nil {
		s.report(ctx, remote, commit, StateFailure, s.AppURL(c.App), firstLine(err.Error()))
		return err
	}
	if dep.ID != "" {
		s.report(ctx, remote, commit, StateSuccess, s.EnvURL(c.App, dep.Env), "Live")
	}
	return s.Repos.MoveBranch(c.App, branch, commit)
}

// branchGone lets go of a branch that is no more, with its preview if it had one.
func (s *Service) branchGone(ctx context.Context, c store.ForgeConnection, branch, by string) error {
	if !s.Repos.Exists(c.App) {
		return nil
	}
	old, _, _ := s.Repos.BranchHead(c.App, branch)
	if _, _, err := s.deliver(ctx, c, gitserver.Update{Ref: "refs/heads/" + branch, Old: old}, by, branchRule(c), nil); err != nil {
		return err
	}
	if err := s.Repos.RemoveRef(c.App, headsRef+branch); err != nil {
		return err
	}
	return s.Repos.MoveBranch(c.App, branch, "")
}

// report tells the forge how a commit stands, even as the server stops: a check left saying that something is under way
// can hold a pull request up for ever.
func (s *Service) report(ctx context.Context, r Remote, sha, state, target, description string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := r.SetStatus(ctx, sha, state, target, description); err != nil {
		s.Log.Warn("tell a forge how a commit went", "repo", r.Repo, "err", err)
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// previewBranch is what a pull request's preview goes by, as a branch would.
func previewBranch(number int) string { return "pr-" + strconv.Itoa(number) }

// pullRef is where a forge keeps the code of a pull request, wherever it came from.
func pullRef(kind string, number int) string {
	if kind == GitLab {
		return "refs/merge-requests/" + strconv.Itoa(number) + "/head"
	}
	return "refs/pull/" + strconv.Itoa(number) + "/head"
}

var commitName = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// pullChanged has a pull request wait for approval, if its code is from someone who may not push there, and otherwise
// builds it. Building someone's code runs it here, with what a preview is given, so it is not done on a stranger's say:
// approval is of one commit, and another needs another.
func (s *Service) pullChanged(ctx context.Context, c store.ForgeConnection, event Event) error {
	if !c.Previews || event.Number <= 0 {
		return nil
	}
	if !commitName.MatchString(event.Head) {
		return errors.New("the forge told of a pull request without saying which commit it is at")
	}
	pull, err := s.DB.ForgePull(ctx, c.AppID, event.Number)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && pull.State == store.PullClosed && !event.Reopened {
		return nil // delivered out of order, say
	}
	if err == nil && pull.State == store.PullLive && pull.HeadSHA == event.Head {
		// What is live already, as it may be told of twice, unless its preview has gone since.
		if gone, err := s.DB.ForgePreviewGone(ctx, pull); err != nil {
			return err
		} else if !gone {
			return nil
		}
	}
	pull.AppID, pull.Number, pull.Title, pull.Author, pull.FromFork, pull.HeadSHA, pull.Error, pull.UpdatedAt =
		c.AppID, event.Number, event.Title, event.Author, event.FromFork, event.Head, "", time.Now().UTC()
	remote := s.remoteOf(c)
	if event.Member && !event.FromFork {
		return s.buildPull(ctx, c, remote, pull, "")
	}
	if pull.ApprovedSHA == event.Head {
		return s.buildPull(ctx, c, remote, pull, event.Head)
	}
	return s.pullWaits(ctx, c, remote, pull)
}

func (s *Service) pullWaits(ctx context.Context, c store.ForgeConnection, remote Remote, pull store.ForgePull) error {
	pull.State, pull.ApprovedSHA, pull.ApprovedBy = store.PullWaiting, "", ""
	if err := s.DB.SetForgePull(ctx, pull); err != nil {
		return err
	}
	s.report(ctx, remote, pull.HeadSHA, StatePending, s.AppURL(c.App)+"/settings", "Waiting for someone to approve this commit")
	s.comment(ctx, remote, &pull, fmt.Sprintf("Commit `%s` has not been built: it comes from someone who cannot push to this repository, and building it runs their code on the server. A maintainer of **%s** can approve it, on [the app's settings](%s/settings), or with `moor forge approve %d %s --app %s`.", short(pull.HeadSHA), c.App, s.AppURL(c.App), pull.Number, short(pull.HeadSHA), c.App))
	return s.DB.SetForgePull(ctx, pull)
}

func (s *Service) comment(ctx context.Context, r Remote, pull *store.ForgePull, text string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	id, err := r.Comment(ctx, pull.Number, pull.CommentID, text)
	if err != nil {
		s.Log.Warn("comment on a pull request", "repo", r.Repo, "pull", pull.Number, "err", err)
		return
	}
	pull.CommentID = id
}

// buildPull builds a pull request's preview. If only one commit was approved, then what is there now has to be that one.
func (s *Service) buildPull(ctx context.Context, c store.ForgeConnection, remote Remote, pull store.ForgePull, approved string) error {
	pull.State = store.PullBuilding
	if err := s.DB.SetForgePull(ctx, pull); err != nil {
		return err
	}
	lasting := context.WithoutCancel(ctx)
	failed := func(err error) error {
		pull.State, pull.Error, pull.UpdatedAt = store.PullFailed, firstLine(err.Error()), time.Now().UTC()
		if ctx.Err() != nil {
			pull.Error = "The server stopped while this was being built. Push again, or build it again from the console."
		}
		s.report(ctx, remote, pull.HeadSHA, StateFailure, s.AppURL(c.App), pull.Error)
		s.comment(ctx, remote, &pull, fmt.Sprintf("The preview of `%s` failed: %s", short(pull.HeadSHA), pull.Error))
		if saved := s.DB.SetForgePull(lasting, pull); saved != nil {
			return saved
		}
		return err
	}
	commit, err := s.fetch(ctx, c, pullRef(c.Kind, pull.Number), pullsRef+strconv.Itoa(pull.Number))
	if err != nil {
		return failed(err)
	}
	if approved != "" && commit != approved {
		// It moved since someone looked at it. What it is at now has not been looked at.
		pull.HeadSHA = commit
		return s.pullWaits(ctx, c, remote, pull)
	}
	pull.HeadSHA = commit
	s.report(ctx, remote, commit, StatePending, s.AppURL(c.App), "Building a preview")
	branch := previewBranch(pull.Number)
	old, _, _ := s.Repos.BranchHead(c.App, branch)
	dep, kept, err := s.deliver(ctx, c, gitserver.Update{Ref: "refs/heads/" + branch, Old: old, New: commit}, forgeUser(c, pull.Author), gitserver.PreviewOnly, nil)
	if err != nil {
		return failed(err)
	}
	pull.UpdatedAt = time.Now().UTC()
	if dep.ID == "" {
		pull.State, pull.Error = store.PullSkipped, kept.why
		s.report(ctx, remote, commit, StateSuccess, s.AppURL(c.App), "Not deployed: "+kept.why)
		if err := s.DB.SetForgePull(lasting, pull); err != nil {
			return err
		}
		return s.Repos.MoveBranch(c.App, branch, commit)
	}
	pull.State, pull.Env = store.PullLive, dep.Env
	address := s.EnvURL(c.App, dep.Env)
	s.report(ctx, remote, commit, StateSuccess, address, "The preview is live")
	s.comment(ctx, remote, &pull, fmt.Sprintf("The preview of `%s` is live at %s", short(commit), address))
	if err := s.DB.SetForgePull(lasting, pull); err != nil {
		return err
	}
	return s.Repos.MoveBranch(c.App, branch, commit)
}

func short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

func (s *Service) pullClosed(ctx context.Context, c store.ForgeConnection, event Event) error {
	pull, err := s.DB.ForgePull(ctx, c.AppID, event.Number)
	if errors.Is(err, store.ErrNotFound) {
		// Heard of out of order, perhaps. It goes on record, so that hearing of its being opened does not build it.
		return s.DB.SetForgePull(ctx, store.ForgePull{AppID: c.AppID, Number: event.Number, Title: event.Title, Author: event.Author, FromFork: event.FromFork, HeadSHA: event.Head, State: store.PullClosed, UpdatedAt: time.Now().UTC()})
	}
	if err != nil {
		return err
	}
	branch := previewBranch(event.Number)
	if s.Repos.Exists(c.App) {
		old, _, _ := s.Repos.BranchHead(c.App, branch)
		_, kept, err := s.deliver(ctx, c, gitserver.Update{Ref: "refs/heads/" + branch, Old: old}, forgeUser(c, event.Author), gitserver.PreviewOnly, nil)
		if err != nil {
			return err
		}
		if kept.stays != "" {
			// It is not gone, and saying so would be wrong: the next time this is told of, or someone removing it, sees to it.
			return errors.New(kept.stays)
		}
		if err := s.Repos.MoveBranch(c.App, branch, ""); err != nil {
			return err
		}
		if err := s.Repos.RemoveRef(c.App, pullsRef+strconv.Itoa(event.Number)); err != nil {
			return err
		}
	}
	if pull.CommentID != "" {
		text := "This pull request is closed, and was not built."
		if pull.Env != "" {
			text = "This pull request is closed, and its preview has gone with it."
		}
		s.comment(ctx, s.remoteOf(c), &pull, text)
	}
	pull.State, pull.Env, pull.UpdatedAt = store.PullClosed, "", time.Now().UTC()
	return s.DB.SetForgePull(ctx, pull)
}

// CommitChangedError says that a pull request is not at the commit that someone was approving.
type CommitChangedError struct{ Now string }

func (e *CommitChangedError) Error() string {
	return "That pull request has moved on, to " + short(e.Now) + ". Look at that, and approve it if it is all right."
}

// Approve has a pull request built, at the commit that someone has looked at: if it is at another by now, that one is not
// what they answered for. It takes as much of the commit's name as is shown, of at least eight characters.
func (s *Service) Approve(ctx context.Context, app string, number int, commit, by string) (store.ForgePull, error) {
	c, err := s.DB.ForgeOf(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		return store.ForgePull{}, ErrNoConnection
	}
	if err != nil {
		return store.ForgePull{}, err
	}
	pull, err := s.DB.ForgePull(ctx, c.AppID, number)
	if err != nil {
		return pull, err
	}
	switch {
	case pull.State == store.PullClosed:
		return pull, &InvalidError{"That pull request is closed."}
	case pull.State == store.PullBuilding:
		return pull, &InvalidError{"That pull request is being built."}
	case len(commit) < 8:
		return pull, &InvalidError{"Say which commit you are approving, by at least its first eight characters, as moor forge pulls shows it."}
	case !strings.HasPrefix(pull.HeadSHA, strings.ToLower(commit)):
		return pull, &CommitChangedError{Now: pull.HeadSHA}
	}
	pull.ApprovedSHA, pull.ApprovedBy, pull.State, pull.Error, pull.UpdatedAt = pull.HeadSHA, by, store.PullBuilding, "", time.Now().UTC()
	if err := s.DB.SetForgePull(ctx, pull); err != nil {
		return pull, err
	}
	if s.Audit != nil {
		s.Audit(ctx, by, "forge.approve", fmt.Sprintf("%s#%d@%s", app, number, pull.HeadSHA), "ok")
	}
	s.background(func(ctx context.Context) {
		defer s.lockOf(c.AppID, previewBranch(number))()
		// While this waited, it may have been closed, or moved on, or the connection let go of.
		c, err := s.DB.ForgeOfAppID(ctx, c.AppID)
		if err != nil {
			return
		}
		now, err := s.DB.ForgePull(ctx, c.AppID, number)
		if err != nil || now.State != store.PullBuilding || now.ApprovedSHA != pull.ApprovedSHA {
			return
		}
		s.note(ctx, c, "approval", s.buildPull(ctx, c, s.remoteOf(c), now, now.ApprovedSHA))
	})
	return pull, nil
}

// Sync looks at the repository now, as polling does: this is how what is there already gets deployed, or a deployment
// that failed gets another try.
func (s *Service) Sync(ctx context.Context, app string) error {
	c, err := s.DB.ForgeOf(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNoConnection
	}
	if err != nil {
		return err
	}
	s.startLook(c, true)
	return nil
}

// look finds what has changed in a repository, when nothing is telling of that.
func (s *Service) look(ctx context.Context, appID string, again bool) {
	// By now it may have been let go of, or the app deleted, and fetching would make a repository for it again.
	c, err := s.DB.ForgeOfAppID(ctx, appID)
	if err != nil {
		return
	}
	settled, err := s.lookAt(ctx, c, again)
	if err == nil && !settled {
		// Nothing went wrong this time, as nothing was tried. What failed before still stands, and goes on saying so.
		if err := s.DB.NoteForgeHeard(context.WithoutCancel(ctx), c.AppID, time.Now()); err != nil {
			s.Log.Warn("note a look at a forge", "app", c.App, "err", err)
		}
		return
	}
	s.note(ctx, c, "a look", err)
}

// lookAt deals with whatever differs from the last time that it looked. The first time, that is everything, and only
// the default branch counts: a repository's old branches are not there to be deployed all at once. It says whether the
// default branch is where the repository has it, which is how it knows that nothing that failed still stands.
func (s *Service) lookAt(ctx context.Context, c store.ForgeConnection, again bool) (settled bool, _ error) {
	asking, stopAsking := context.WithTimeout(ctx, time.Minute)
	defer stopAsking()
	if name, err := s.Repos.RemoteDefaultBranch(asking, s.fetchOptions(c)); err != nil {
		return false, err
	} else if !c.BranchPinned && name != "" && name != c.DefaultBranch {
		c.DefaultBranch = name
		if err := s.DB.SetForgeDefaultBranch(ctx, c.AppID, name); err != nil {
			return false, err
		}
	}
	before, err := s.Repos.RefsUnder(c.App, headsRef)
	if err != nil {
		return false, err
	}
	fetching, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if err := s.Repos.Fetch(fetching, c.App, s.fetchOptions(c), "+refs/heads/*:"+headsRef+"*"); err != nil {
		return false, err
	}
	after, err := s.Repos.RefsUnder(c.App, headsRef)
	if err != nil {
		return false, err
	}
	branches := make([]string, 0, len(after))
	for branch := range after {
		branches = append(branches, branch)
	}
	slices.SortFunc(branches, func(a, b string) int {
		switch {
		case a == c.DefaultBranch:
			return -1
		case b == c.DefaultBranch:
			return 1
		}
		return strings.Compare(a, b)
	})
	by := forgeUser(c, "")
	first := c.LookedAt.IsZero()
	var problems []error
	for _, branch := range branches {
		isDefault := branch == c.DefaultBranch
		// Being asked to try again is for a branch that has been dealt with, and did not get to where it is now.
		head, _, _ := s.Repos.BranchHead(c.App, branch)
		retry := again && (isDefault || head != "")
		switch {
		case pullBranch.MatchString(branch):
			continue
		case first && !isDefault:
			continue
		case before[branch] == after[branch] && !retry:
			continue
		}
		// A hook may have got there first, while this one waited, and with something newer than this one found.
		unlock := s.lockOf(c.AppID, "branch "+branch)
		commit, err := s.Repos.Ref(c.App, headsRef+branch)
		if err == nil {
			err = s.deployBranch(ctx, c, branch, commit, by)
		}
		unlock()
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", branch, err))
		}
	}
	for branch := range before {
		if _, there := after[branch]; !there {
			unlock := s.lockOf(c.AppID, "branch "+branch)
			var err error
			if _, stillGone := s.Repos.Ref(c.App, headsRef+branch); stillGone != nil {
				err = s.branchGone(ctx, c, branch, by)
			}
			unlock()
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", branch, err))
			}
		}
	}
	if err := s.DB.NoteForgeLooked(context.WithoutCancel(ctx), c.AppID, time.Now()); err != nil {
		problems = append(problems, err)
	}
	if len(problems) > 0 {
		return false, errors.Join(problems...)
	}
	head, _, _ := s.Repos.BranchHead(c.App, c.DefaultBranch)
	return after[c.DefaultBranch] == "" || head == after[c.DefaultBranch], nil
}
