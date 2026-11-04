package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/gitserver"
	"github.com/robert-mcdermott/moorage/internal/store"
)

type forgeStatus struct{ Sha, State, Target, Description string }

// fakeForge is a forge, as far as a connection can tell: a git server, an API that remembers what it was told, and
// someone working on a repository there.
type fakeForge struct {
	t     *testing.T
	repos *gitserver.Repos
	work  *gitRepo
	srv   *httptest.Server
	// token, if any, is what reading takes.
	token string

	mu       sync.Mutex
	statuses []forgeStatus
	comments map[string]string
	reads    int
	calls    []string
}

func newFakeForge(t *testing.T) *fakeForge {
	t.Helper()
	if _, err := os.Stat(filepath.Join("..", "..", "testdata", "apps", "static", "index.html")); err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{t: t, repos: gitserver.NewRepos(t.TempDir()), comments: map[string]string{}}
	git := &gitserver.HTTP{Git: &gitserver.Server{Repos: f.repos, Log: slog.New(slog.DiscardHandler)}, Authorize: func(_ *http.Request, password, _ string, push bool) (string, error) {
		if push || (f.token != "" && password != f.token) {
			return "", gitserver.ErrForbidden
		}
		return "someone", nil
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/git/") {
			f.mu.Lock()
			f.reads++
			f.mu.Unlock()
			if _, _, has := r.BasicAuth(); !has {
				if f.token != "" {
					w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				r.SetBasicAuth("nobody", "")
			}
			git.ServeHTTP(w, r)
			return
		}
		f.api(w, r)
	}))
	t.Cleanup(f.srv.Close)
	home := t.TempDir()
	f.work = &gitRepo{t: t, dir: copyTree(t, filepath.Join("..", "..", "testdata", "apps", "static")), env: []string{
		"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}}
	f.work.must("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Dir(f.repos.Path("shop")), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := f.work.git("init", "-q", "--bare", "-b", "main", f.repos.Path("shop")); err != nil {
		t.Fatal(out, err)
	}
	return f
}

func (f *fakeForge) api(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	rest, ok := strings.CutPrefix(r.URL.Path, "/repos/git/shop/")
	switch {
	case !ok:
		w.WriteHeader(http.StatusNotFound)
	case strings.HasPrefix(rest, "statuses/"):
		f.statuses = append(f.statuses, forgeStatus{strings.TrimPrefix(rest, "statuses/"), body["state"], body["target_url"], body["description"]})
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(rest, "issues/comments/") && r.Method == http.MethodPatch:
		id := strings.TrimPrefix(rest, "issues/comments/")
		f.comments[id] = body["body"]
		fmt.Fprintf(w, `{"id": %s}`, id)
	case strings.HasSuffix(rest, "/comments") && r.Method == http.MethodPost:
		id := strconv.Itoa(1000 + len(f.comments))
		f.comments[id] = body["body"]
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id": %s}`, id)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeForge) repoURL() string { return f.srv.URL + "/git/shop.git" }

// commit has someone commit, to wherever the working tree is, and says what commit that is.
func (f *fakeForge) commit(content string, files ...string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.work.dir, "index.html"), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	for i := 0; i+1 < len(files); i += 2 {
		if err := os.WriteFile(filepath.Join(f.work.dir, files[i]), []byte(files[i+1]), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.work.must("add", ".")
	f.work.must("commit", "-q", "-m", content)
	return strings.TrimSpace(f.work.must("rev-parse", "HEAD"))
}

// publish has the forge's repository have a commit at a ref.
func (f *fakeForge) publish(commit, ref string) {
	f.t.Helper()
	f.work.must("push", "-q", "-f", f.repos.Path("shop"), commit+":"+ref)
}

func (f *fakeForge) statusesOf(sha string) []forgeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forgeStatus
	for _, s := range f.statuses {
		if s.Sha == sha {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeForge) lastStatus(sha string) forgeStatus {
	list := f.statusesOf(sha)
	if len(list) == 0 {
		return forgeStatus{}
	}
	return list[len(list)-1]
}

func (f *fakeForge) commentsNow() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.comments))
	for k, v := range f.comments {
		out[k] = v
	}
	return out
}

func (f *fakeForge) readsNow() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// forgeEnd is a server, with a connection to a forge, from the forge's end.
type forgeEnd struct {
	t      *testing.T
	cfg    config.Config
	addrs  Addrs
	c      *client.Client
	conn   apitypes.ForgeConnection
	secret string
}

func connectedToForge(t *testing.T, f *fakeForge, change func(*Options), in apitypes.ForgeConnect) *forgeEnd {
	t.Helper()
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	var addrs Addrs
	if change != nil {
		addrs = startWith(t, cfg, change)
	} else {
		addrs, _ = start(t, cfg)
	}
	e := &forgeEnd{t: t, cfg: cfg, addrs: addrs, c: apiClient(t, cfg, addrs)}
	if _, err := e.c.CreateApp(context.Background(), "shop", ""); err != nil {
		t.Fatal(err)
	}
	in.URL, in.Kind, in.APIURL = f.repoURL(), "github", f.srv.URL
	conn, err := e.c.ConnectForge(context.Background(), "shop", in)
	if err != nil {
		t.Fatal(err)
	}
	e.conn, e.secret = conn, conn.Secret
	return e
}

// send is the forge sending an event, signed.
func (e *forgeEnd) send(event string, payload any) int {
	e.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(e.secret))
	mac.Write(body)
	return e.post(event, "sha256="+hex.EncodeToString(mac.Sum(nil)), body)
}

func (e *forgeEnd) post(event, signature string, body []byte) int {
	e.t.Helper()
	u, err := url.Parse(e.conn.HookURL)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.postTo(u.Path, event, signature, body)
}

func (e *forgeEnd) postTo(path, event, signature string, body []byte) int {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+e.addrs.HTTP.String()+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Host = e.cfg.ConsoleHost
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signature)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

func pushEvent(branch, commit string) map[string]any {
	return map[string]any{"ref": "refs/heads/" + branch, "after": commit, "sender": map[string]any{"login": "ann"}}
}

func pullEvent(action string, number int, head string, fork bool, association string) map[string]any {
	headRepo := "git/shop"
	if fork {
		headRepo = "stranger/shop"
	}
	return map[string]any{"action": action, "number": number, "pull_request": map[string]any{
		"title": "A change", "user": map[string]any{"login": "ann"}, "author_association": association,
		"head": map[string]any{"sha": head, "repo": map[string]any{"full_name": headRepo}},
		"base": map[string]any{"repo": map[string]any{"full_name": "git/shop"}},
	}}
}

// serves says what is at an environment's address now.
func (e *forgeEnd) serves(env string) string {
	e.t.Helper()
	res, body := get(e.t, e.addrs.HTTP, e.cfg.EnvHost("shop", env), "/")
	if res.StatusCode != http.StatusOK {
		return ""
	}
	return body
}

func (e *forgeEnd) pull(number int) apitypes.ForgePull {
	e.t.Helper()
	pulls, err := e.c.ForgePulls(context.Background(), "shop")
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range pulls {
		if p.Number == number {
			return p
		}
	}
	return apitypes.ForgePull{}
}

func (e *forgeEnd) waitForPull(number int, state string) apitypes.ForgePull {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		p := e.pull(number)
		if p.State == state {
			return p
		}
		if time.Now().After(deadline) {
			conn, _ := e.c.Forge(context.Background(), "shop")
			e.t.Fatalf("pull request %d is %q, not %q (%s), and the connection says %q", number, p.State, state, p.Error, conn.LastError)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (e *forgeEnd) waitForServed(env, want string) {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(e.serves(env), want) {
		if time.Now().After(deadline) {
			conn, _ := e.c.Forge(context.Background(), "shop")
			e.t.Fatalf("%s serves %q, not %q, and the connection says %q", env, e.serves(env), want, conn.LastError)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (e *forgeEnd) deployments() []apitypes.Deployment {
	e.t.Helper()
	var all []apitypes.Deployment
	for env := range e.envs() {
		list, err := e.c.Deployments(context.Background(), "shop", env, 100)
		if err != nil {
			e.t.Fatal(err)
		}
		all = append(all, list...)
	}
	return all
}

func (e *forgeEnd) envs() map[string]bool {
	e.t.Helper()
	list, err := e.c.Environments(context.Background(), "shop")
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, env := range list {
		out[env.Name] = true
	}
	return out
}

func TestForgeConnection(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/trunk")
	f.work.must("--git-dir", f.repos.Path("shop"), "symbolic-ref", "HEAD", "refs/heads/trunk")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{Token: f.token})
	ctx := context.Background()

	if e.secret == "" || !e.conn.HasToken || e.conn.LastError != "" {
		t.Errorf("%+v", e.conn)
	}
	if e.conn.DefaultBranch != "trunk" {
		t.Errorf("it did not ask what the default branch is: %s", e.conn.DefaultBranch)
	}
	if !strings.Contains(e.conn.HookURL, "/api/v1/hooks/hok_") {
		t.Errorf("the hook is at %s", e.conn.HookURL)
	}
	again, err := e.c.Forge(ctx, "shop")
	if err != nil || again.Secret != "" {
		t.Errorf("the secret was shown twice: %+v %v", again, err)
	}
	if len(e.deployments()) != 0 {
		t.Error("connecting deployed something")
	}
	// Connecting again keeps its hook and its token, and does not show the secret.
	again, err = e.c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: f.srv.URL, NoPreviews: true})
	if err != nil || again.Secret != "" || again.HookURL != e.conn.HookURL || !again.HasToken || again.Previews {
		t.Errorf("%+v %v", again, err)
	}
	// A token goes no further than the forge it was given for.
	moved, err := e.c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: "http://127.0.0.1:1"})
	if err != nil || moved.HasToken {
		t.Errorf("the token went along to another API: %+v %v", moved, err)
	}
	if _, err := e.c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: f.srv.URL, Token: f.token}); err != nil {
		t.Fatal(err)
	}
	moved, err = e.c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL() + "/../other.git", Kind: "github", APIURL: f.srv.URL})
	if err != nil || moved.HasToken {
		t.Errorf("the token went along to another address: %+v %v", moved, err)
	}
	if _, err := e.c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: f.srv.URL, Token: f.token}); err != nil {
		t.Fatal(err)
	}
	// What cannot be right is refused.
	for _, bad := range []apitypes.ForgeConnect{
		{URL: "git@github.com:acme/shop.git"},
		{URL: "https://someone:a-token@github.com/acme/shop.git"},
		{URL: "https://git.example.com/acme/shop.git"},
		{URL: "http://git.example.com/acme/shop.git", Kind: "gitea", Token: "in the clear"},
	} {
		if _, err := e.c.ConnectForge(ctx, "shop", bad); status(err) != http.StatusUnprocessableEntity {
			t.Errorf("%+v: %v", bad, err)
		}
	}

	// Someone who is not the forge gets nothing.
	body, _ := json.Marshal(pushEvent("trunk", first))
	if code := e.postTo("/api/v1/hooks/hok_nothing", "push", "sha256=abc", body); code != http.StatusNotFound {
		t.Errorf("no such hook: %d", code)
	}
	if code := e.post("push", "sha256=abc", body); code != http.StatusUnauthorized {
		t.Errorf("a wrong signature: %d", code)
	}
	if code := e.post("push", "", body); code != http.StatusUnauthorized {
		t.Errorf("no signature: %d", code)
	}
	if code := e.send("ping", map[string]any{"zen": "hello"}); code != http.StatusAccepted {
		t.Errorf("a ping: %d", code)
	}

	// A push deploys what is in the repository, whatever the event says: it is the fetch that counts.
	if code := e.send("push", pushEvent("trunk", strings.Repeat("b", 40))); code != http.StatusAccepted {
		t.Fatalf("a push: %d", code)
	}
	e.waitForServed("production", "first")
	waitUntil(t, "the check to say it is live", func() bool { return f.lastStatus(first).State == "success" })
	got := f.statusesOf(first)
	if got[0].State != "pending" || !strings.Contains(f.lastStatus(first).Target, "shop") {
		t.Errorf("%+v", got)
	}
	deps := e.deployments()
	if len(deps) != 1 || deps[0].Commit != first || !strings.HasPrefix(deps[0].CreatedBy, "github:ann") {
		t.Errorf("%+v", deps)
	}

	// Told twice, it deploys once.
	e.send("push", pushEvent("trunk", first))
	second := f.commit("second")
	f.publish(second, "refs/heads/trunk")
	e.send("push", pushEvent("trunk", second))
	e.waitForServed("production", "second")
	if n := len(e.deployments()); n != 2 {
		t.Errorf("%d deployments", n)
	}

	// What will not deploy says so, and leaves things as they were.
	broken := f.commit("broken", "moorage.toml", "this is not toml")
	f.publish(broken, "refs/heads/trunk")
	e.send("push", pushEvent("trunk", broken))
	waitUntil(t, "the check to fail", func() bool { return f.lastStatus(broken).State == "failure" })
	waitUntil(t, "the connection to say so", func() bool {
		conn, err := e.c.Forge(ctx, "shop")
		return err == nil && conn.LastError != ""
	})
	if !strings.Contains(e.serves("production"), "second") {
		t.Error("what failed took the place of what worked")
	}

	// Letting go leaves what it deployed, and its hook is no more.
	if err := e.c.DisconnectForge(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Forge(ctx, "shop"); status(err) != http.StatusNotFound {
		t.Errorf("%v", err)
	}
	if code := e.send("push", pushEvent("trunk", second)); code != http.StatusNotFound {
		t.Errorf("a hook after disconnecting: %d", code)
	}
	if !strings.Contains(e.serves("production"), "second") {
		t.Error("disconnecting took the app away")
	}
}

func TestForgeBranches(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{Token: f.token})
	e.send("push", pushEvent("main", first))
	e.waitForServed("production", "first")

	// A branch has its preview from its pull request, and is not built for being pushed.
	f.work.must("checkout", "-q", "-b", "feature")
	feature := f.commit("feature")
	f.publish(feature, "refs/heads/feature")
	e.send("push", pushEvent("feature", feature))
	waitUntil(t, "it to look at that", func() bool {
		conn, err := e.c.Forge(context.Background(), "shop")
		return err == nil && !conn.LastEventAt.IsZero() && gitserver.NewRepos(NewLayout(e.cfg.DataDir).Repos).Exists("shop") && branchAt(e, "feature") == feature
	})
	if n := len(e.deployments()); n != 1 || e.envs()["feature"] {
		t.Errorf("%d deployments, in %v", n, e.envs())
	}
	if len(f.statusesOf(feature)) != 0 {
		t.Errorf("a check on what is not deployed: %+v", f.statusesOf(feature))
	}

	// One that an environment names goes to it.
	f.work.must("checkout", "-q", "-b", "develop", "main")
	develop := f.commit("develop", "moorage.toml", "[app]\nname = \"shop\"\n\n[environments.staging]\nbranch = \"develop\"\n")
	f.publish(develop, "refs/heads/develop")
	e.send("push", pushEvent("develop", develop))
	e.waitForServed("staging", "develop")
	if !strings.Contains(e.serves("production"), "first") {
		t.Error("that reached production")
	}

	// A branch going away takes nothing of this with it, and a branch going by the name of a preview is left alone.
	e.send("push", map[string]any{"ref": "refs/heads/feature", "after": strings.Repeat("0", 40), "deleted": true})
	waitUntil(t, "the branch to go", func() bool { return branchAt(e, "feature") == "" })
	f.publish(feature, "refs/heads/pr-12")
	e.send("push", pushEvent("pr-12", feature))
	waitUntil(t, "it to refuse that", func() bool {
		conn, err := e.c.Forge(context.Background(), "shop")
		return err == nil && strings.Contains(conn.LastError, "pr-12")
	})
	if e.envs()["pr-12"] {
		t.Error("a branch took the name of a preview")
	}
}

func branchAt(e *forgeEnd, branch string) string {
	commit, _, err := gitserver.NewRepos(NewLayout(e.cfg.DataDir).Repos).BranchHead("shop", branch)
	if err != nil {
		return ""
	}
	return commit
}

func TestForgePullRequests(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{Token: f.token})
	ctx := context.Background()

	// A member's pull request is built, and told where it went.
	f.work.must("checkout", "-q", "-b", "change")
	change := f.commit("the change")
	f.publish(change, "refs/pull/5/head")
	if code := e.send("pull_request", pullEvent("opened", 5, change, false, "MEMBER")); code != http.StatusAccepted {
		t.Fatal(code)
	}
	p := e.waitForPull(5, "live")
	if p.Env != "pr-5" || p.Commit != change || p.URL == "" || p.Author != "ann" || p.Title != "A change" {
		t.Errorf("%+v", p)
	}
	e.waitForServed("pr-5", "the change")
	waitUntil(t, "the check to pass", func() bool { return f.lastStatus(change).State == "success" })
	if got := f.lastStatus(change); got.Target != e.cfg.EnvURL("shop", "pr-5") {
		t.Errorf("%+v", got)
	}
	comments := f.commentsNow()
	if len(comments) != 1 {
		t.Fatalf("%v", comments)
	}
	for _, text := range comments {
		if !strings.Contains(text, e.cfg.EnvURL("shop", "pr-5")) || !strings.Contains(text, change[:8]) {
			t.Errorf("%s", text)
		}
	}
	if e.serves("production") != "" {
		t.Error("that reached production")
	}

	// Told of it again, its title having changed, nothing is built.
	n := len(e.deployments())
	e.send("pull_request", pullEvent("synchronize", 5, change, false, "MEMBER"))
	time.Sleep(200 * time.Millisecond)
	if len(e.deployments()) != n {
		t.Error("the same commit was built again")
	}

	// Another commit takes its place, and the same comment says so.
	next := f.commit("the next change")
	f.publish(next, "refs/pull/5/head")
	e.send("pull_request", pullEvent("synchronize", 5, next, false, "MEMBER"))
	e.waitForServed("pr-5", "the next change")
	waitUntil(t, "the comment to change", func() bool {
		comments := f.commentsNow()
		if len(comments) != 1 {
			t.Fatalf("%v", comments)
		}
		for _, text := range comments {
			return strings.Contains(text, next[:8])
		}
		return false
	})

	// Closed, its preview goes.
	e.send("pull_request", pullEvent("closed", 5, next, false, "MEMBER"))
	waitUntil(t, "the preview to go", func() bool { return !e.envs()["pr-5"] })
	waitUntil(t, "it to leave the list", func() bool { return e.pull(5).Number == 0 })
	if _, err := e.c.ApproveForgePull(ctx, "shop", 5, next); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("approving what is closed: %v", err)
	}
	if _, err := e.c.ApproveForgePull(ctx, "shop", 404, next); status(err) != http.StatusNotFound {
		t.Errorf("approving what is not there: %v", err)
	}
}

func TestForgeWaitsForApproval(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{Token: f.token})
	ctx := context.Background()
	moorage := gitserver.NewRepos(NewLayout(e.cfg.DataDir).Repos)

	f.work.must("checkout", "-q", "-b", "stranger")
	theirs := f.commit("a stranger's code")
	f.publish(theirs, "refs/pull/7/head")
	for name, event := range map[string]map[string]any{
		"from a fork, though a member's": pullEvent("opened", 7, theirs, true, "MEMBER"),
		"from someone no one knows":      pullEvent("opened", 7, theirs, false, "NONE"),
		"a first contribution":           pullEvent("opened", 7, theirs, false, "FIRST_TIME_CONTRIBUTOR"),
	} {
		reads := f.readsNow()
		e.send("pull_request", event)
		p := e.waitForPull(7, "waiting")
		if p.Commit != theirs {
			t.Errorf("%s: %+v", name, p)
		}
		waitUntil(t, "a check to say so", func() bool { return strings.Contains(f.lastStatus(theirs).Description, "approve") })
		if f.readsNow() != reads {
			t.Errorf("%s: it fetched before anyone approved", name)
		}
		if moorage.Exists("shop") {
			if commit, err := moorage.Ref("shop", "refs/moorage/forge/pull/7"); err == nil {
				t.Errorf("%s: their code is here: %s", name, commit)
			}
		}
		if len(e.deployments()) != 0 {
			t.Errorf("%s: it was built", name)
		}
	}
	comments := f.commentsNow()
	if len(comments) != 1 {
		t.Errorf("%v", comments)
	}
	for _, text := range comments {
		if !strings.Contains(text, "moor forge approve 7 "+theirs[:8]+" --app shop") {
			t.Errorf("%s", text)
		}
	}

	// Someone who may deploy, and not look after the app, may not vouch for it.
	owner := signIn(t, e.cfg, e.addrs)
	member := addTeammate(t, e.cfg, e.addrs, owner, "someone@example.com")
	if _, err := e.c.SetMember(ctx, "default", "someone@example.com", store.RoleDeployer); err != nil {
		t.Fatal(err)
	}
	memberClient := tokenClient(t, e.c, tokenOf(t, member, auth.ScopeDeploy))
	if _, err := memberClient.ApproveForgePull(ctx, "shop", 7, theirs); status(err) != http.StatusForbidden {
		t.Errorf("a deployer approving: %v", err)
	}
	if _, err := memberClient.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: "https://github.com/someone/else.git"}); status(err) != http.StatusForbidden {
		t.Errorf("a deployer connecting it elsewhere: %v", err)
	}
	if err := memberClient.DisconnectForge(ctx, "shop"); status(err) != http.StatusForbidden {
		t.Errorf("a deployer disconnecting it: %v", err)
	}
	if e.pull(7).State != "waiting" {
		t.Errorf("%+v", e.pull(7))
	}

	// An approval says which commit it is for, and is not for anything else.
	if _, err := e.c.ApproveForgePull(ctx, "shop", 7, ""); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("no commit: %v", err)
	}
	if _, err := e.c.ApproveForgePull(ctx, "shop", 7, theirs[:7]); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("too little of a commit: %v", err)
	}
	if _, err := e.c.ApproveForgePull(ctx, "shop", 7, strings.Repeat("c", 40)); status(err) != http.StatusConflict {
		t.Errorf("another commit: %v", err)
	}
	if e.pull(7).State != "waiting" {
		t.Errorf("%+v", e.pull(7))
	}

	// Approved, that commit is built.
	p, err := e.c.ApproveForgePull(ctx, "shop", 7, theirs[:8])
	if err != nil {
		t.Fatal(err)
	}
	if p.ApprovedBy == "" {
		t.Errorf("%+v", p)
	}
	e.waitForPull(7, "live")
	e.waitForServed("pr-7", "a stranger's code")
	entries, err := testAuth(t, e.cfg).DB.Audit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, entry := range entries {
		audited = audited || (entry.Action == "forge.approve" && strings.Contains(entry.Target, theirs))
	}
	if !audited {
		t.Errorf("no one has a record of who approved which commit: %+v", entries)
	}

	// Another commit is another matter, and what was built stays as it was.
	more := f.commit("more of a stranger's code")
	f.publish(more, "refs/pull/7/head")
	e.send("pull_request", pullEvent("synchronize", 7, more, true, "NONE"))
	e.waitForPull(7, "waiting")
	if !strings.Contains(e.serves("pr-7"), "a stranger's code") || strings.Contains(e.serves("pr-7"), "more of") {
		t.Errorf("%s", e.serves("pr-7"))
	}

	// Someone looks at what is there, and a commit is pushed and told of before they say so. It is not what they looked at.
	seen := e.pull(7).Commit
	pushedLast := f.commit("pushed as they looked")
	f.publish(pushedLast, "refs/pull/7/head")
	e.send("pull_request", pullEvent("synchronize", 7, pushedLast, true, "NONE"))
	waitUntil(t, "it to hear of that", func() bool { return e.pull(7).Commit == pushedLast })
	if _, err := e.c.ApproveForgePull(ctx, "shop", 7, seen); status(err) != http.StatusConflict {
		t.Errorf("approving what it has moved on from: %v", err)
	}
	if strings.Contains(e.serves("pr-7"), "pushed as they looked") {
		t.Error("what no one approved was built")
	}

	// What is approved is what someone saw: a commit that arrives in the meantime, not yet told of, is not built.
	sneaked := f.commit("sneaked in")
	f.publish(sneaked, "refs/pull/7/head")
	if _, err := e.c.ApproveForgePull(ctx, "shop", 7, pushedLast); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "it to see that", func() bool { p := e.pull(7); return p.State == "waiting" && p.Commit == sneaked })
	if strings.Contains(e.serves("pr-7"), "sneaked") {
		t.Error("what no one approved was built")
	}
	// And that it approved one commit is no approval of the commit after it, told of by another event.
	e.send("pull_request", pullEvent("synchronize", 7, sneaked, true, "NONE"))
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(e.serves("pr-7"), "sneaked") || e.pull(7).State != "waiting" {
		t.Errorf("%+v", e.pull(7))
	}
}

func TestForgePullHasNoSayInWhereItGoes(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{Token: f.token})

	// Its manifest names an environment for the branch that it knows it will go by. It goes to its preview.
	f.work.must("checkout", "-q", "-b", "cheeky")
	cheeky := f.commit("cheeky", "moorage.toml", "[app]\nname = \"shop\"\n\n[environments.staging]\nbranch = \"pr-9\"\n\n[previews]\nbranches = [\"feature/*\"]\n")
	f.publish(cheeky, "refs/pull/9/head")
	e.send("pull_request", pullEvent("opened", 9, cheeky, false, "MEMBER"))
	p := e.waitForPull(9, "live")
	if p.Env != "pr-9" || e.envs()["staging"] {
		t.Errorf("%+v, in %v", p, e.envs())
	}

	// It may ask for no preview, and that is all.
	f.work.must("checkout", "-q", "-b", "none", "main")
	none := f.commit("none", "moorage.toml", "[app]\nname = \"shop\"\n\n[previews]\nenabled = false\n")
	f.publish(none, "refs/pull/10/head")
	e.send("pull_request", pullEvent("opened", 10, none, false, "MEMBER"))
	p = e.waitForPull(10, "skipped")
	if !strings.Contains(p.Error, "previews are turned off") {
		t.Errorf("%+v", p)
	}
	waitUntil(t, "the check to pass", func() bool { return f.lastStatus(none).State == "success" })
	if !strings.Contains(f.lastStatus(none).Description, "previews are turned off") {
		t.Errorf("%+v", f.lastStatus(none))
	}

	// No previews, no pull requests.
	if _, err := e.c.ConnectForge(context.Background(), "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: f.srv.URL, NoPreviews: true}); err != nil {
		t.Fatal(err)
	}
	e.send("pull_request", pullEvent("opened", 11, none, false, "MEMBER"))
	time.Sleep(200 * time.Millisecond)
	if p := e.pull(11); p.Number != 0 {
		t.Errorf("%+v", p)
	}
}

func TestForgePolling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake forge's bare repository is held open for a moment longer there")
	}
	f := newFakeForge(t)
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	f.work.must("checkout", "-q", "-b", "old")
	f.publish(f.commit("old"), "refs/heads/old")
	f.work.must("checkout", "-q", "main")
	e := connectedToForge(t, f, func(o *Options) { o.ForgeTick = 100 * time.Millisecond }, apitypes.ForgeConnect{Poll: true})
	if e.conn.HasToken || !e.conn.Poll {
		t.Errorf("%+v", e.conn)
	}

	// The first look deploys the default branch, and no old branches.
	e.waitForServed("production", "first")
	time.Sleep(300 * time.Millisecond)
	if n := len(e.deployments()); n != 1 || e.envs()["old"] {
		t.Errorf("%d deployments, in %v", n, e.envs())
	}

	// After that, what changes deploys.
	second := f.commit("second")
	f.publish(second, "refs/heads/main")
	e.waitForServed("production", "second")

	// With no pull requests to hear of, a new branch has a preview as a push here would.
	f.work.must("checkout", "-q", "-b", "feature")
	f.publish(f.commit("feature"), "refs/heads/feature")
	e.waitForServed("feature", "feature")
	f.work.must("checkout", "-q", "main")

	// Going away, it takes its preview with it.
	f.work.must("--git-dir", f.repos.Path("shop"), "update-ref", "-d", "refs/heads/feature")
	waitUntil(t, "the preview to go", func() bool { return !e.envs()["feature"] })

	// What fails is not tried over and over, and asking tries it again.
	broken := f.commit("broken", "moorage.toml", "this is not toml")
	f.publish(broken, "refs/heads/main")
	waitUntil(t, "it to fail", func() bool {
		conn, err := e.c.Forge(context.Background(), "shop")
		return err == nil && conn.LastError != ""
	})
	time.Sleep(400 * time.Millisecond)
	if !strings.Contains(e.serves("production"), "second") {
		t.Error("what failed took the place of what worked")
	}
	f.work.must("rm", "-q", "-f", "moorage.toml")
	if err := e.c.SyncForge(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	fixed := f.commit("fixed")
	f.publish(fixed, "refs/heads/main")
	e.waitForServed("production", "fixed")
	if err := e.c.SyncForge(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
}

func TestForgeSyncDeploysWhatIsThere(t *testing.T) {
	f := newFakeForge(t)
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{})
	if err := e.c.SyncForge(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	e.waitForServed("production", "first")
	if err := e.c.SyncForge(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(e.deployments()); n != 1 {
		t.Errorf("nothing had changed, and it deployed %d times", n)
	}
}

func TestForgeCannotBeReached(t *testing.T) {
	f := newFakeForge(t)
	f.token = "the right one"
	f.publish(f.commit("first"), "refs/heads/main")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{Token: "the wrong one"})
	if !strings.Contains(e.conn.LastError, "cannot be read") || strings.Contains(e.conn.LastError, "the wrong one") {
		t.Errorf("%q", e.conn.LastError)
	}
	if e.conn.DefaultBranch != "main" {
		t.Errorf("%s", e.conn.DefaultBranch)
	}
	// What this server may not reach, it does not reach, whatever someone says.
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 here")
	}
	listener.Close()
	conn, err := e.c.ConnectForge(context.Background(), "shop", apitypes.ForgeConnect{URL: "http://" + listener.Addr().String() + "/a/b.git", Kind: "gitea"})
	if err != nil || !strings.Contains(conn.LastError, "may not reach") {
		t.Errorf("%+v %v", conn, err)
	}
}

func TestForgeAfterARestart(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	if _, err := c.CreateApp(ctx, "shop", ""); err != nil {
		t.Fatal(err)
	}
	conn, err := c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: f.srv.URL, Token: f.token})
	if err != nil {
		t.Fatal(err)
	}
	db := testAuth(t, cfg).DB
	stored, err := db.ForgeOf(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.SealedToken, []byte(f.token)) || bytes.Contains(stored.SealedSecret, []byte(conn.Secret)) {
		t.Error("what is secret is not sealed")
	}
	// The server stops part of the way through a build, and a check is left saying that it is under way.
	if err := db.SetForgePull(ctx, store.ForgePull{AppID: stored.AppID, Number: 3, HeadSHA: first, State: store.PullBuilding, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	addrs, _ = start(t, cfg)
	c = apiClient(t, cfg, addrs)
	e := &forgeEnd{t: t, cfg: cfg, addrs: addrs, c: c, conn: conn, secret: conn.Secret}
	if p := e.waitForPull(3, "failed"); !strings.Contains(p.Error, "stopped") {
		t.Errorf("%+v", p)
	}
	waitUntil(t, "the check to say so", func() bool { return f.lastStatus(first).State == "failure" })

	// It builds again if asked, and its hook still works.
	f.publish(first, "refs/pull/3/head")
	if _, err := c.ApproveForgePull(ctx, "shop", 3, first); err != nil {
		t.Fatal(err)
	}
	e.waitForPull(3, "live")
	e.send("push", pushEvent("main", first))
	e.waitForServed("production", "first")
}

func TestForgeInTheConsole(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	if _, err := c.CreateApp(ctx, "shop", ""); err != nil {
		t.Fatal(err)
	}
	res, body := owner.get("/apps/shop/settings")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Repository address") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}

	// What cannot be right is said, and what was typed stays, other than a token.
	res, body = owner.post("/apps/shop/settings/forge/connect", url.Values{"csrf": {owner.token()}, "url": {"https://git.example.com/acme/shop.git"}, "token": {"typed by someone"}, "previews": {"1"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "kind of forge") || !strings.Contains(body, "git.example.com") || strings.Contains(body, "typed by someone") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, _ := owner.post("/apps/shop/settings/forge/connect", url.Values{"url": {f.repoURL()}, "kind": {"github"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", res.StatusCode)
	}

	// Connecting shows the secret, once, and never the token.
	res, body = owner.post("/apps/shop/settings/forge/connect", url.Values{"csrf": {owner.token()}, "url": {f.repoURL()}, "kind": {"github"}, "token": {f.token}, "previews": {"1"}})
	if res.StatusCode != http.StatusCreated || !strings.Contains(body, "Give your forge this webhook") || strings.Contains(body, f.token) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	secret := regexp.MustCompile(`[0-9a-f]{64}`).FindString(body)
	if secret == "" {
		t.Fatalf("no secret in %s", body)
	}
	res, body = owner.get("/apps/shop/settings")
	if res.StatusCode != http.StatusOK || strings.Contains(body, secret) || strings.Contains(body, f.token) || !strings.Contains(body, f.repoURL()) || !strings.Contains(body, "/api/v1/hooks/hok_") {
		t.Errorf("%d %s", res.StatusCode, body)
	}

	// The API has no connection's URL set to the fake forge's API, so it keeps to this one's.
	conn, err := c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: f.srv.URL, Token: f.token})
	if err != nil {
		t.Fatal(err)
	}
	e := &forgeEnd{t: t, cfg: cfg, addrs: addrs, c: c, conn: conn, secret: secret}

	// A pull request that is waiting shows, and can be approved.
	f.work.must("checkout", "-q", "-b", "stranger")
	theirs := f.commit("a stranger's <b>code</b>")
	f.publish(theirs, "refs/pull/7/head")
	e.send("pull_request", pullEvent("opened", 7, theirs, true, "NONE"))
	e.waitForPull(7, "waiting")
	res, body = owner.get("/apps/shop/settings")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Waiting for approval") || !strings.Contains(body, "from a fork") || !strings.Contains(body, theirs[:8]) || !strings.Contains(body, "Approve and build") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, body := owner.post("/apps/shop/settings/forge/pulls/7/approve", url.Values{"csrf": {owner.token()}, "commit": {strings.Repeat("c", 40)}}); res.StatusCode != http.StatusConflict || !strings.Contains(body, "moved on") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, _ := owner.post("/apps/shop/settings/forge/pulls/7/approve", url.Values{"csrf": {owner.token()}, "commit": {theirs}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", res.StatusCode)
	}
	e.waitForPull(7, "live")
	res, body = owner.get("/apps/shop/settings")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, ">Live<") || !strings.Contains(body, "pr-7") {
		t.Errorf("%d %s", res.StatusCode, body)
	}

	// Someone who may not look after it does not see this, or get to do it.
	member := addTeammate(t, cfg, addrs, owner, "someone@example.com")
	if _, err := c.SetMember(ctx, "default", "someone@example.com", store.RoleDeployer); err != nil {
		t.Fatal(err)
	}
	if res, _ := member.post("/apps/shop/settings/forge/pulls/7/approve", url.Values{"csrf": {member.token()}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a deployer approving: %d", res.StatusCode)
	}
	if res, _ := member.get("/apps/shop/settings"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a deployer looking: %d", res.StatusCode)
	}

	// Looking, and letting go.
	if res, _ := owner.post("/apps/shop/settings/forge/sync", url.Values{"csrf": {owner.token()}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	e.waitForServed("production", "first")
	if res, _ := owner.post("/apps/shop/settings/forge/disconnect", url.Values{"csrf": {owner.token()}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	res, body = owner.get("/apps/shop/settings")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Repository address") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
}

func TestForgeKeepsToItsRepositoryAndBranch(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	second := f.commit("on release")
	f.publish(second, "refs/heads/release")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{Token: f.token, DefaultBranch: "release"})
	ctx := context.Background()
	if e.conn.DefaultBranch != "release" {
		t.Fatalf("%+v", e.conn)
	}

	// What it says is production stays so, whatever the forge says its default branch is.
	push := pushEvent("release", second)
	push["repository"] = map[string]any{"full_name": "git/shop", "default_branch": "main"}
	e.send("push", push)
	e.waitForServed("production", "on release")
	if conn, err := e.c.Forge(ctx, "shop"); err != nil || conn.DefaultBranch != "release" {
		t.Errorf("%+v %v", conn, err)
	}
	push = pushEvent("main", first)
	push["repository"] = map[string]any{"full_name": "git/shop", "default_branch": "main"}
	e.send("push", push)
	waitUntil(t, "it to keep the branch", func() bool { return branchAt(e, "main") == first })
	if !strings.Contains(e.serves("production"), "on release") {
		t.Error("the forge's default branch reached production")
	}

	// A hook that goes on from another repository is about that one.
	elsewhere := pullEvent("opened", 44, second, false, "MEMBER")
	elsewhere["repository"] = map[string]any{"full_name": "someone/else"}
	f.publish(second, "refs/pull/44/head")
	reads := f.readsNow()
	e.send("pull_request", elsewhere)
	time.Sleep(200 * time.Millisecond)
	if e.pull(44).Number != 0 || f.readsNow() != reads {
		t.Errorf("it acted on another repository: %+v", e.pull(44))
	}
	push = pushEvent("main", first)
	push["repository"] = map[string]any{"full_name": "someone/else"}
	third := f.commit("not told of")
	f.publish(third, "refs/heads/release")
	push = pushEvent("release", third)
	push["repository"] = map[string]any{"full_name": "someone/else"}
	e.send("push", push)
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(e.serves("production"), "not told of") {
		t.Error("a push to another repository deployed")
	}

	// The same name, written another way, is the same repository.
	push = pushEvent("release", third)
	push["repository"] = map[string]any{"full_name": "GIT/Shop"}
	e.send("push", push)
	e.waitForServed("production", "not told of")

	// What is told of one that is closed does not bring it back, unless it is opened again.
	change := f.commit("the change")
	f.publish(change, "refs/pull/5/head")
	e.send("pull_request", pullEvent("opened", 5, change, false, "MEMBER"))
	e.waitForPull(5, "live")
	e.send("pull_request", pullEvent("closed", 5, change, false, "MEMBER"))
	waitUntil(t, "the preview to go", func() bool { return !e.envs()["pr-5"] })
	waitUntil(t, "it to leave the list", func() bool { return e.pull(5).Number == 0 })
	e.send("pull_request", pullEvent("synchronize", 5, change, false, "MEMBER"))
	time.Sleep(200 * time.Millisecond)
	if e.pull(5).Number != 0 || e.envs()["pr-5"] {
		t.Errorf("a late delivery brought it back: %+v", e.pull(5))
	}
	e.send("pull_request", pullEvent("reopened", 5, change, false, "MEMBER"))
	e.waitForPull(5, "live")

	// A token is not sent in the clear to an API, any more than to the repository.
	if _, err := e.c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "gitea", APIURL: "http://git.example.com/api/v1", Token: "a-token"}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("%v", err)
	}
}

func TestForgeDeletedWhileItWaits(t *testing.T) {
	f := newFakeForge(t)
	f.publish(f.commit("first"), "refs/heads/main")
	e := connectedToForge(t, f, func(o *Options) { o.ForgeTick = 50 * time.Millisecond }, apitypes.ForgeConnect{Poll: true})
	e.waitForServed("production", "first")
	if err := e.c.DeleteApp(context.Background(), "shop", false); err != nil {
		t.Fatal(err)
	}
	// Nothing makes it again.
	time.Sleep(500 * time.Millisecond)
	apps, err := e.c.Apps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		if a.Name == "shop" {
			t.Errorf("it came back: %+v", a)
		}
	}
}

func TestForgeCatchesUpAfterARestart(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	if _, err := c.CreateApp(ctx, "shop", ""); err != nil {
		t.Fatal(err)
	}
	conn, err := c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: f.srv.URL, Token: f.token})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SyncForge(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	e := &forgeEnd{t: t, cfg: cfg, addrs: addrs, c: c, conn: conn, secret: conn.Secret}
	e.waitForServed("production", "first")
	waitUntil(t, "the look to be noted", func() bool {
		stored, err := testAuth(t, cfg).DB.ForgeOf(ctx, "shop")
		return err == nil && !stored.LookedAt.IsZero()
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// A push while it is away, of which no one is told.
	second := f.commit("while it was away")
	f.publish(second, "refs/heads/main")
	addrs, _ = start(t, cfg)
	e.addrs, e.c = addrs, apiClient(t, cfg, addrs)
	e.waitForServed("production", "while it was away")
	waitUntil(t, "the check to pass", func() bool { return f.lastStatus(second).State == "success" })
}

func TestForgeCatchesUpWithOnlyAHook(t *testing.T) {
	f := newFakeForge(t)
	f.token = "a-token-of-the-forge"
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	if _, err := c.CreateApp(ctx, "shop", ""); err != nil {
		t.Fatal(err)
	}
	conn, err := c.ConnectForge(ctx, "shop", apitypes.ForgeConnect{URL: f.repoURL(), Kind: "github", APIURL: f.srv.URL, Token: f.token})
	if err != nil {
		t.Fatal(err)
	}
	e := &forgeEnd{t: t, cfg: cfg, addrs: addrs, c: c, conn: conn, secret: conn.Secret}
	e.send("push", pushEvent("main", first))
	e.waitForServed("production", "first")

	// What it was told of, that failed, is not tried again when it next starts.
	broken := f.commit("broken", "moorage.toml", "this is not toml")
	f.publish(broken, "refs/heads/main")
	e.send("push", pushEvent("main", broken))
	waitUntil(t, "it to fail", func() bool { return f.lastStatus(broken).State == "failure" })
	failures := len(f.statusesOf(broken))
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	addrs, stop = start(t, cfg)
	e.addrs, e.c = addrs, apiClient(t, cfg, addrs)
	time.Sleep(500 * time.Millisecond)
	if n := len(f.statusesOf(broken)); n != failures {
		t.Errorf("what failed was tried again: %d checks, from %d", n, failures)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// What it was not told of, as it was not there, is what it looks for.
	f.work.must("rm", "-q", "-f", "moorage.toml")
	second := f.commit("while it was away")
	f.publish(second, "refs/heads/main")
	addrs, _ = start(t, cfg)
	e.addrs, e.c = addrs, apiClient(t, cfg, addrs)
	e.waitForServed("production", "while it was away")
}

func TestForgeUnderAPathOfItsOwn(t *testing.T) {
	f := newFakeForge(t)
	first := f.commit("first")
	f.publish(first, "refs/heads/main")
	e := connectedToForge(t, f, nil, apitypes.ForgeConnect{})
	// The repository's address is /git/shop.git, and a forge under /git would call it shop.
	push := pushEvent("main", first)
	push["repository"] = map[string]any{"full_name": "shop"}
	e.send("push", push)
	e.waitForServed("production", "first")
	// And another is said to be, where someone can see.
	push = pushEvent("main", first)
	push["repository"] = map[string]any{"full_name": "acme/shops"}
	e.send("push", push)
	waitUntil(t, "it to say so", func() bool {
		conn, err := e.c.Forge(context.Background(), "shop")
		return err == nil && strings.Contains(conn.LastError, "acme/shops")
	})

	// A pull request closed before it is heard of as opened is not built.
	f.publish(first, "refs/pull/8/head")
	e.send("pull_request", pullEvent("closed", 8, first, false, "MEMBER"))
	time.Sleep(100 * time.Millisecond)
	e.send("pull_request", pullEvent("opened", 8, first, false, "MEMBER"))
	time.Sleep(300 * time.Millisecond)
	if e.envs()["pr-8"] {
		t.Error("it built what was closed")
	}
}
