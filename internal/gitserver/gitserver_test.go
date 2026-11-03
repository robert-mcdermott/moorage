package gitserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/revlist"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// receiver records pushes, and refuses or holds them as a test asks.
type receiver struct {
	mu     sync.Mutex
	pushes []*Push
	// refuse, when set, is the error a push gets.
	refuse error
	// hold, when set, keeps a push of the branch waiting until it closes.
	hold     map[string]chan struct{}
	progress string
}

func (rc *receiver) Receive(ctx context.Context, p *Push) error {
	rc.mu.Lock()
	rc.pushes = append(rc.pushes, p)
	refuse, progress := rc.refuse, rc.progress
	var hold chan struct{}
	for _, u := range p.Updates {
		if h := rc.hold[u.Branch()]; h != nil {
			hold = h
		}
	}
	rc.mu.Unlock()
	if progress != "" {
		fmt.Fprintln(p.Progress, progress)
	}
	if hold != nil {
		<-hold
	}
	return refuse
}

func (rc *receiver) last() *Push {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.pushes) == 0 {
		return nil
	}
	return rc.pushes[len(rc.pushes)-1]
}

type fixture struct {
	git     *Server
	rc      *receiver
	sshAddr string
	httpURL string
	client  ssh.Signer
	hostKey ssh.PublicKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{rc: &receiver{hold: map[string]chan struct{}{}}}
	f.git = &Server{Repos: NewRepos(t.TempDir()), Receiver: f.rc}
	host, err := LoadHostKey(filepath.Join(t.TempDir(), "keys", "ssh_host_ed25519_key"))
	if err != nil {
		t.Fatal(err)
	}
	f.hostKey = host.PublicKey()
	if f.client, err = LoadHostKey(filepath.Join(t.TempDir(), "client_key")); err != nil {
		t.Fatal(err)
	}
	allowed := ssh.FingerprintSHA256(f.client.PublicKey())
	srv := &SSH{Git: f.git, HostKey: host, Key: func(_ context.Context, k ssh.PublicKey) (string, bool) {
		return "admin", ssh.FingerprintSHA256(k) == allowed
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	f.sshAddr = ln.Addr().String()

	h := &HTTP{Git: f.git, Password: func(_ *http.Request, _, password string) (string, bool) {
		return "admin", password == "secret"
	}}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	f.httpURL = ts.URL
	return f
}

func (f *fixture) sshURL(app string) string { return "ssh://git@" + f.sshAddr + "/" + app + ".git" }

func (f *fixture) sshAuth(signer ssh.Signer) client.Option {
	return client.WithSSHAuth(&gitssh.PublicKeys{
		User: "git", Signer: signer,
		HostKeyCallbackHelper: gitssh.HostKeyCallbackHelper{HostKeyCallback: ssh.FixedHostKey(f.hostKey)},
	})
}

// work is a working repository with commits to push.
type work struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

func newWork(t *testing.T) *work {
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false, git.WithDefaultBranch(plumbing.NewBranchReferenceName("main")))
	if err != nil {
		t.Fatal(err)
	}
	return &work{t: t, dir: dir, repo: r}
}

func (w *work) commit(message string, files map[string]string) plumbing.Hash {
	w.t.Helper()
	for name, content := range files {
		p := filepath.Join(w.dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
	wt, err := w.repo.Worktree()
	if err != nil {
		w.t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		w.t.Fatal(err)
	}
	h, err := wt.Commit(message, &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		w.t.Fatal(err)
	}
	return h
}

func (w *work) push(url string, auth client.Option, opts ...func(*git.PushOptions)) (string, error) {
	var progress bytes.Buffer
	w.repo.DeleteRemote("moorage")
	if _, err := w.repo.CreateRemote(&config.RemoteConfig{Name: "moorage", URLs: []string{url}}); err != nil {
		w.t.Fatal(err)
	}
	o := &git.PushOptions{
		RemoteName: "moorage", ClientOptions: []client.Option{auth}, Progress: &progress,
		RefSpecs: []config.RefSpec{"refs/heads/*:refs/heads/*"},
	}
	for _, opt := range opts {
		opt(o)
	}
	// A push that waits for the client to close its side would hang: the
	// library's client keeps the connection open (spike S1).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := w.repo.PushContext(ctx, o)
	if errors.Is(err, context.DeadlineExceeded) {
		w.t.Fatal("the push hung")
	}
	return progress.String(), err
}

func (f *fixture) ref(t *testing.T, app, ref string) plumbing.Hash {
	t.Helper()
	st, err := f.git.Repos.open(app)
	if err != nil {
		return plumbing.ZeroHash
	}
	r, err := st.Reference(plumbing.ReferenceName(ref))
	if err != nil {
		return plumbing.ZeroHash
	}
	return r.Hash()
}

func TestPushOverSSH(t *testing.T) {
	f := newFixture(t)
	w := newWork(t)
	first := w.commit("first commit", map[string]string{"index.html": "hello"})
	f.rc.progress = "remote says hello"

	progress, err := w.push(f.sshURL("site"), f.sshAuth(f.client), func(o *git.PushOptions) { o.Options = []string{"detach", "env=staging"} })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress, "remote says hello") {
		t.Errorf("progress: %q", progress)
	}
	p := f.rc.last()
	if p == nil || p.App != "site" || p.By != "admin" || !p.Created || p.DefaultBranch != "main" {
		t.Fatalf("push: %+v", p)
	}
	if len(p.Updates) != 1 || p.Updates[0].Ref != "refs/heads/main" || p.Updates[0].Old != "" ||
		p.Updates[0].New != first.String() || strings.TrimSpace(p.Updates[0].Message) != "first commit" {
		t.Errorf("updates: %+v", p.Updates)
	}
	if _, ok := p.Option("detach"); !ok {
		t.Error("no detach option")
	}
	if env, _ := p.Option("env"); env != "staging" {
		t.Errorf("env = %q", env)
	}
	if got := f.ref(t, "site", "refs/heads/main"); got != first {
		t.Errorf("main = %s, want %s", got, first)
	}

	second := w.commit("second", map[string]string{"about.html": "about"})
	if _, err := w.push(f.sshURL("site"), f.sshAuth(f.client)); err != nil {
		t.Fatal(err)
	}
	if p := f.rc.last(); p.Created || p.Updates[0].Old != first.String() {
		t.Errorf("second push: %+v", p)
	}

	// The repository clones back.
	clone, err := git.PlainClone(t.TempDir(), &git.CloneOptions{URL: f.sshURL("site"), ClientOptions: []client.Option{f.sshAuth(f.client)}})
	if err != nil {
		t.Fatal(err)
	}
	head, _ := clone.Head()
	if head.Hash() != second {
		t.Errorf("clone's HEAD = %s, want %s", head.Hash(), second)
	}
}

func TestUnknownKeysAndCommands(t *testing.T) {
	f := newFixture(t)
	stranger, err := LoadHostKey(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	w := newWork(t)
	w.commit("one", map[string]string{"a": "a"})
	if _, err := w.push(f.sshURL("site"), f.sshAuth(stranger)); err == nil {
		t.Error("a push with an unknown key was accepted")
	}
	if f.git.Repos.Exists("site") {
		t.Error("an unknown key created a repository")
	}

	// A shell or another command gets nothing.
	conn, err := ssh.Dial("tcp", f.sshAddr, &ssh.ClientConfig{
		User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(f.client)}, HostKeyCallback: ssh.FixedHostKey(f.hostKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, cmd := range []string{"ls /", "git-upload-archive '/site.git'", "git-receive-pack '/../etc.git'"} {
		s, err := conn.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		out, err := s.CombinedOutput(cmd)
		if err == nil {
			t.Errorf("%q succeeded: %s", cmd, out)
		}
	}
	if _, _, err := conn.OpenChannel("direct-tcpip", nil); err == nil {
		t.Error("port forwarding was accepted")
	}
}

func TestRefusedPush(t *testing.T) {
	f := newFixture(t)
	w := newWork(t)
	first := w.commit("one", map[string]string{"a": "a"})
	if _, err := w.push(f.sshURL("site"), f.sshAuth(f.client)); err != nil {
		t.Fatal(err)
	}
	w.commit("two", map[string]string{"b": "b"})
	f.rc.refuse = errors.New("deployment dep_x failed: the build failed")
	_, err := w.push(f.sshURL("site"), f.sshAuth(f.client))
	if err == nil || !strings.Contains(err.Error(), "deployment dep_x failed") {
		t.Fatalf("push: %v", err)
	}
	if got := f.ref(t, "site", "refs/heads/main"); got != first {
		t.Errorf("main moved to %s after a refused push", got)
	}
}

func TestPushOverHTTP(t *testing.T) {
	f := newFixture(t)
	w := newWork(t)
	first := w.commit("one", map[string]string{"a": "a"})
	url := f.httpURL + "/git/site.git"
	if _, err := w.push(url, client.WithHTTPAuth(&githttp.BasicAuth{Username: "moor", Password: "wrong"})); err == nil {
		t.Error("a push with a wrong token was accepted")
	}
	auth := client.WithHTTPAuth(&githttp.BasicAuth{Username: "moor", Password: "secret"})
	if _, err := w.push(url, auth); err != nil {
		t.Fatal(err)
	}
	if got := f.ref(t, "site", "refs/heads/main"); got != first {
		t.Errorf("main = %s", got)
	}
	clone, err := git.PlainClone(t.TempDir(), &git.CloneOptions{URL: url, ClientOptions: []client.Option{auth}})
	if err != nil {
		t.Fatal(err)
	}
	if head, _ := clone.Head(); head.Hash() != first {
		t.Errorf("clone's HEAD = %s", head.Hash())
	}
	res, err := http.Get(f.httpURL + "/git/site.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a fetch without credentials: %d", res.StatusCode)
	}
}

func TestConcurrentPushes(t *testing.T) {
	f := newFixture(t)
	w := newWork(t)
	w.commit("one", map[string]string{"a": "a"})
	if _, err := w.push(f.sshURL("site"), f.sshAuth(f.client)); err != nil {
		t.Fatal(err)
	}
	other := newWork(t)
	if _, err := other.repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{f.sshURL("site")}}); err != nil {
		t.Fatal(err)
	}
	if err := other.repo.Fetch(&git.FetchOptions{ClientOptions: []client.Option{f.sshAuth(f.client)}, RefSpecs: []config.RefSpec{"refs/heads/main:refs/heads/main"}}); err != nil {
		t.Fatal(err)
	}

	// A push to main waits in its deployment.
	release := make(chan struct{})
	f.rc.mu.Lock()
	f.rc.hold["main"] = release
	f.rc.mu.Unlock()
	w.commit("two", map[string]string{"b": "b"})
	done := make(chan error)
	go func() {
		_, err := w.push(f.sshURL("site"), f.sshAuth(f.client), func(o *git.PushOptions) { o.RefSpecs = []config.RefSpec{"refs/heads/main:refs/heads/main"} })
		done <- err
	}()
	for p := f.rc.last(); p == nil || strings.TrimSpace(p.Updates[0].Message) != "two"; p = f.rc.last() {
		time.Sleep(10 * time.Millisecond)
	}

	// Another push to main is refused meanwhile; one to another branch is
	// not held up.
	wt, _ := other.repo.Worktree()
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main")}); err != nil {
		t.Fatal(err)
	}
	other.commit("three", map[string]string{"c": "c"})
	_, err := other.push(f.sshURL("site"), f.sshAuth(f.client), func(o *git.PushOptions) { o.RefSpecs = []config.RefSpec{"refs/heads/main:refs/heads/main"} })
	if err == nil || !strings.Contains(err.Error(), "another push to main") {
		t.Errorf("a second push to main: %v", err)
	}
	if _, err := other.push(f.sshURL("site"), f.sshAuth(f.client), func(o *git.PushOptions) { o.RefSpecs = []config.RefSpec{"refs/heads/main:refs/heads/feature"} }); err != nil {
		t.Errorf("a push to another branch: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Errorf("the held push: %v", err)
	}
	// main has moved: a push from where main was is stale.
	other.commit("four", map[string]string{"d": "d"})
	_, err = other.push(f.sshURL("site"), f.sshAuth(f.client), func(o *git.PushOptions) {
		o.RefSpecs = []config.RefSpec{"+refs/heads/main:refs/heads/main"}
		o.Force = true
	})
	if err != nil && !strings.Contains(err.Error(), "fetch") {
		t.Errorf("a forced push: %v", err)
	}
}

// TestClientLeaves checks that a push whose client leaves during its
// deployment (Ctrl-C) still moves the branch once the deployment succeeds.
func TestClientLeaves(t *testing.T) {
	f := newFixture(t)
	w := newWork(t)
	head := w.commit("one", map[string]string{"a": "a"})
	release := make(chan struct{})
	f.rc.hold["main"] = release
	w.repo.CreateRemote(&config.RemoteConfig{Name: "moorage", URLs: []string{f.sshURL("site")}})
	ctx, leave := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.repo.PushContext(ctx, &git.PushOptions{RemoteName: "moorage", ClientOptions: []client.Option{f.sshAuth(f.client)}})
	}()
	for f.rc.last() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	leave()
	<-done
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for f.ref(t, "site", "refs/heads/main") != head {
		if time.Now().After(deadline) {
			t.Fatal("main did not move after the client left")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDefaultBranch(t *testing.T) {
	f := newFixture(t)
	w := newWork(t)
	w.commit("one", map[string]string{"a": "a"})
	push := func(spec string) error {
		_, err := w.push(f.sshURL("site"), f.sshAuth(f.client), func(o *git.PushOptions) { o.RefSpecs = []config.RefSpec{config.RefSpec(spec)} })
		return err
	}
	// The first push names the default branch when it has no main.
	if err := push("refs/heads/main:refs/heads/trunk"); err != nil {
		t.Fatal(err)
	}
	if p := f.rc.last(); p.DefaultBranch != "trunk" {
		t.Errorf("default branch %q", p.DefaultBranch)
	}
	if err := push("refs/heads/main:refs/heads/other"); err != nil {
		t.Fatal(err)
	}
	if p := f.rc.last(); p.DefaultBranch != "trunk" || p.Updates[0].Branch() != "other" {
		t.Errorf("second push: %+v", p)
	}
	if err := push(":refs/heads/trunk"); err == nil || !strings.Contains(err.Error(), "production branch") {
		t.Errorf("deleting the default branch: %v", err)
	}
	if err := push(":refs/heads/other"); err != nil {
		t.Errorf("deleting another branch: %v", err)
	}
	if p := f.rc.last(); !p.Updates[0].Deletion() {
		t.Errorf("deletion: %+v", p.Updates)
	}
	if err := push("refs/heads/main:refs/notes/x"); err == nil {
		t.Error("a ref outside branches and tags was accepted")
	}
}

func TestExport(t *testing.T) {
	f := newFixture(t)
	w := newWork(t)
	if runtime.GOOS != "windows" {
		os.WriteFile(filepath.Join(w.dir, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
		os.Symlink("index.html", filepath.Join(w.dir, "link.html"))
	}
	head := w.commit("files", map[string]string{
		"index.html":              "hello",
		"app/main.py":             "print(1)",
		"secrets/key":             "k",
		".moorignore":             "secrets/\n",
		"node_modules/x/index.js": "x",
		"big.bin":                 lfsPointer + "\noid sha256:abc\nsize 3\n",
	})
	if _, err := w.push(f.sshURL("site"), f.sshAuth(f.client)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ex, err := f.rc.last().Export(head.String(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"index.html", "app/main.py", ".moorignore", "big.bin"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s was not written: %v", want, err)
		}
	}
	for _, gone := range []string{"secrets", "node_modules", "link.html"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s was written", gone)
		}
	}
	if len(ex.LFS) != 1 || ex.LFS[0] != "big.bin" {
		t.Errorf("LFS: %v", ex.LFS)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("run.sh is not executable: %v %v", fi, err)
		}
		if len(ex.Symlinks) != 1 || ex.Symlinks[0] != "link.html" {
			t.Errorf("symlinks: %v", ex.Symlinks)
		}
	}
}

// TestRequestReader checks that a push's request ends at the pack's
// checksum, though the client keeps its side open, and that a push larger
// than the limit is refused.
func TestRequestReader(t *testing.T) {
	w := newWork(t)
	head := w.commit("one", map[string]string{"a": strings.Repeat("content ", 5000), "b": "bee"})
	hashes, err := revlist.Objects(w.repo.Storer, []plumbing.Hash{head}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, w.repo.Storer, false).Encode(hashes, 10); err != nil {
		t.Fatal(err)
	}
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	var req bytes.Buffer
	req.WriteString(pkt(strings.Repeat("0", 40) + " " + head.String() + " refs/heads/main\x00report-status side-band-64k push-options\n"))
	req.WriteString("0000")
	req.WriteString(pkt("detach\n"))
	req.WriteString("0000")
	req.Write(pack.Bytes())

	for _, tc := range []struct {
		name  string
		input []byte
		limit int64
		err   error
	}{
		{"a push", req.Bytes(), 0, nil},
		{"a push followed by more", append(append([]byte{}, req.Bytes()...), "junk"...), 0, nil},
		{"a flush alone", []byte("0000"), 0, nil},
		{"a deletion", []byte(pkt(head.String()+" "+strings.Repeat("0", 40)+" refs/heads/x\x00report-status\n") + "0000"), 0, nil},
		{"a push over the limit", req.Bytes(), int64(req.Len() / 2), ErrPackTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr, pw := io.Pipe()
			go pw.Write(tc.input) // and never closes
			defer pw.Close()
			got := make(chan []byte)
			errc := make(chan error, 1)
			go func() {
				b, err := io.ReadAll(requestReader(pr, tc.limit))
				errc <- err
				got <- b
			}()
			select {
			case err := <-errc:
				b := <-got
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				want := tc.input
				if tc.name == "a push followed by more" {
					want = req.Bytes()
				}
				if tc.err == nil && !bytes.Equal(b, want) {
					t.Errorf("read %d bytes, want %d", len(b), len(want))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the request did not end")
			}
		})
	}
}

// TestPushWithTheConnectionOpen pushes as a client that keeps its side of
// the connection open while it waits for the answer (spike S1): the git
// library alone would wait for the end of the stream forever.
func TestPushWithTheConnectionOpen(t *testing.T) {
	f := newFixture(t)
	w := newWork(t)
	head := w.commit("one", map[string]string{"a": strings.Repeat("content ", 5000)})
	hashes, err := revlist.Objects(w.repo.Storer, []plumbing.Hash{head}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, w.repo.Storer, false).Encode(hashes, 10); err != nil {
		t.Fatal(err)
	}

	conn, err := ssh.Dial("tcp", f.sshAddr, &ssh.ClientConfig{
		User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(f.client)}, HostKeyCallback: ssh.FixedHostKey(f.hostKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, _ := s.StdinPipe()
	stdout, _ := s.StdoutPipe()
	if err := s.Start("git-receive-pack '/site.git'"); err != nil {
		t.Fatal(err)
	}
	readLines := func() ([]string, error) { // pkt-lines up to a flush
		var lines []string
		hdr := make([]byte, 4)
		for {
			if _, err := io.ReadFull(stdout, hdr); err != nil {
				return lines, err
			}
			var n int
			fmt.Sscanf(string(hdr), "%04x", &n)
			if n == 0 {
				return lines, nil
			}
			payload := make([]byte, n-4)
			if _, err := io.ReadFull(stdout, payload); err != nil {
				return lines, err
			}
			lines = append(lines, strings.TrimSpace(string(payload)))
		}
	}
	if _, err := readLines(); err != nil { // the advertisement
		t.Fatal(err)
	}
	cmd := strings.Repeat("0", 40) + " " + head.String() + " refs/heads/main\x00report-status\n"
	fmt.Fprintf(stdin, "%04x%s0000", len(cmd)+4, cmd)
	stdin.Write(pack.Bytes()) // and stdin stays open

	report := make(chan []string, 1)
	go func() {
		lines, _ := readLines()
		report <- lines
	}()
	select {
	case lines := <-report:
		if len(lines) != 2 || lines[0] != "unpack ok" || lines[1] != "ok refs/heads/main" {
			t.Errorf("report: %q", lines)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer while the client kept its side open")
	}
	if got := f.ref(t, "site", "refs/heads/main"); got != head {
		t.Errorf("main = %s", got)
	}
}

func TestParseCommand(t *testing.T) {
	for cmd, want := range map[string]string{
		"git-receive-pack '/site.git'": "git-receive-pack site",
		"git-upload-pack 'site.git'":   "git-upload-pack site",
		"git-upload-pack '/site'":      "git-upload-pack site",
		"git receive-pack '/site.git'": "git-receive-pack site",
		"git-receive-pack '/a/b.git'":  "",
		"git-upload-archive '/site'":   "",
		"sh -c ls":                     "",
		"git-receive-pack '/../x.git'": "",
	} {
		service, app, err := parseCommand(cmd)
		got := service + " " + app
		if err != nil {
			got = ""
		}
		if got != want {
			t.Errorf("parseCommand(%q) = %q, %v; want %q", cmd, got, err, want)
		}
	}
}
