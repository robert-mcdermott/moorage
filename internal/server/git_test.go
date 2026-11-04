package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

// gitRepo is a working repository pushed with the git command line.
type gitRepo struct {
	t   *testing.T
	dir string
	env []string
}

func (g *gitRepo) git(args ...string) (string, error) {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (g *gitRepo) must(args ...string) string {
	g.t.Helper()
	out, err := g.git(args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TestGitPush deploys with git push over SSH and HTTPS, with the git and
// ssh command lines: a push deploys the production branch and streams the
// deploy, a failed deploy refuses the push, and the repository clones back.
func TestGitPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if _, err := exec.LookPath("ssh"); err != nil || runtime.GOOS == "windows" {
		t.Skip("pushing over SSH is tested with OpenSSH on Linux and macOS")
	}
	cfg := testConfig(t)
	cfg.Listen.SSH = "127.0.0.1:0"
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()

	// A key of the client's, registered; the server's host key, trusted.
	home := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "test")
	keyFile := filepath.Join(home, "id_ed25519")
	os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600)
	sshPub, _ := ssh.NewPublicKey(pub)
	if _, err := c.AddSSHKey(ctx, apitypes.NewSSHKey{Name: "laptop", PublicKey: string(ssh.MarshalAuthorizedKey(sshPub))}); err != nil {
		t.Fatal(err)
	}
	hostKey, err := os.ReadFile(NewLayout(cfg.DataDir).SSHHostKey)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.ParsePrivateKey(hostKey)
	_, port, _ := net.SplitHostPort(addrs.SSH.String())
	known := filepath.Join(home, "known_hosts")
	os.WriteFile(known, []byte("[127.0.0.1]:"+port+" "+string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), 0o600)

	repo := &gitRepo{t: t, dir: copyTree(t, filepath.Join("..", "..", "testdata", "apps", "static")), env: []string{
		"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_SSH_COMMAND=ssh -i " + keyFile + " -o IdentitiesOnly=yes -o UserKnownHostsFile=" + known + " -o StrictHostKeyChecking=yes",
	}}
	repo.must("init", "-q", "-b", "main")
	repo.must("add", ".")
	repo.must("commit", "-q", "-m", "First version")
	remote := "ssh://git@127.0.0.1:" + port + "/gitsite.git"
	repo.must("remote", "add", "moorage", remote)

	// The first push creates the app and deploys it.
	out := repo.must("push", "moorage", "main")
	for _, want := range []string{"remote: moorage  created the app gitsite", "remote: moorage  gitsite · production · dep_", "remote: ▸ source     main @ ", "First version", "remote: ✔ live"} {
		if !strings.Contains(out, want) {
			t.Errorf("the push's output lacks %q:\n%s", want, out)
		}
	}
	if _, body := get(t, addrs.HTTP, "gitsite.localhost", "/"); !strings.Contains(body, "release one from a static site") {
		t.Errorf("the app serves %q", body)
	}
	head := strings.TrimSpace(repo.must("rev-parse", "HEAD"))
	deps, err := c.Deployments(ctx, "gitsite", "production", 5)
	if err != nil || len(deps) != 1 || deps[0].Source != "git-push" || deps[0].Ref != "refs/heads/main" || deps[0].Commit != head || deps[0].Message != "First version" {
		t.Fatalf("deployments: %+v %v", deps, err)
	}

	// Building the commit again: the same commit, through a new build.
	again, err := c.Redeploy(ctx, "gitsite", "production")
	if err != nil {
		t.Fatal(err)
	}
	if again, err = c.FollowDeployment(ctx, again.ID, func(apitypes.LogLine) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if again.Status != "live" || again.Source != "git-push" || again.Commit != head || again.Build == deps[0].Build || again.Release == nil || again.Release.Version != 2 {
		t.Errorf("redeployed: %+v", again)
	}
	if _, body := get(t, addrs.HTTP, "gitsite.localhost", "/"); !strings.Contains(body, "release one from a static site") {
		t.Errorf("the app serves %q", body)
	}

	// A deploy that fails refuses the push: main stays where it was.
	os.WriteFile(filepath.Join(repo.dir, "moorage.toml"), []byte("[app]\nruntime = \"static\"\n\n[static]\ndir = \"missing\"\n"), 0o644)
	repo.must("add", ".")
	repo.must("commit", "-q", "-m", "Broken")
	out, err = repo.git("push", "moorage", "main")
	if err == nil || !strings.Contains(out, "remote rejected") || !strings.Contains(out, "deployment dep_") {
		t.Errorf("a failing deploy: %v\n%s", err, out)
	}
	if remoteHead := strings.Fields(repo.must("ls-remote", "moorage", "refs/heads/main")); len(remoteHead) == 0 || remoteHead[0] != head {
		t.Errorf("main moved after a failed deploy: %v", remoteHead)
	}

	// A commit that asks to skip the deploy moves the branch without one;
	// a branch other than main is kept, not deployed.
	repo.must("commit", "-q", "--allow-empty", "-m", "Notes [skip deploy]")
	if out := repo.must("push", "moorage", "main"); !strings.Contains(out, "skip the deploy") {
		t.Errorf("[skip deploy]: %s", out)
	}
	repo.must("switch", "-q", "-c", "feature")
	if out := repo.must("push", "moorage", "feature"); !strings.Contains(out, "feature is kept, not deployed") {
		t.Errorf("another branch: %s", out)
	}
	if deps, _ := c.Deployments(ctx, "gitsite", "production", 5); len(deps) != 3 {
		t.Errorf("%d deployments, want 3 (the first, its rebuild and the failed one)", len(deps))
	}

	// Over HTTPS, with the token as the password; the repository clones
	// back over both.
	token, _ := os.ReadFile(NewLayout(cfg.DataDir).AdminToken)
	_, httpPort, _ := net.SplitHostPort(addrs.HTTP.String())
	resolve := "http.curloptResolve=moorage.localhost:" + httpPort + ":127.0.0.1"
	httpRemote := "http://moor:" + strings.TrimSpace(string(token)) + "@moorage.localhost:" + httpPort + "/git/gitsite.git"
	repo.must("switch", "-q", "main")
	os.Remove(filepath.Join(repo.dir, "moorage.toml"))
	os.WriteFile(filepath.Join(repo.dir, "index.html"), []byte("<p>release two, pushed over HTTPS</p>\n"), 0o644)
	repo.must("add", "-A")
	repo.must("commit", "-q", "-m", "Second version")
	if out := repo.must("-c", resolve, "push", httpRemote, "main"); !strings.Contains(out, "remote: ✔ live") {
		t.Errorf("the push over HTTPS: %s", out)
	}
	if _, body := get(t, addrs.HTTP, "gitsite.localhost", "/"); !strings.Contains(body, "release two") {
		t.Errorf("after the push over HTTPS the app serves %q", body)
	}
	if out, err := repo.git("-c", resolve, "push", "http://moor:wrong@moorage.localhost:"+httpPort+"/git/gitsite.git", "main"); err == nil {
		t.Errorf("a push with a wrong token was accepted: %s", out)
	}
	for _, url := range []string{remote, httpRemote} {
		dir := filepath.Join(t.TempDir(), "clone")
		clone := &gitRepo{t: t, dir: filepath.Dir(dir), env: repo.env}
		clone.must("-c", resolve, "clone", "-q", url, dir)
		if data, err := os.ReadFile(filepath.Join(dir, "index.html")); err != nil || !strings.Contains(string(data), "release two") {
			t.Errorf("a clone over %s: %q %v", url[:4], data, err)
		}
	}
	if keys, _ := c.SSHKeys(ctx); len(keys) != 1 || keys[0].LastUsedAt.IsZero() {
		t.Errorf("keys: %+v", keys)
	}
}
