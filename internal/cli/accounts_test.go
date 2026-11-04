package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/server"
)

const testPassword = "a long and unusual passphrase"

// lockedBuffer is written by a running command and read by the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServer runs a server of the local preset in this process.
func startServer(t *testing.T) (base string, cfg config.Config) {
	t.Helper()
	cfg, err := config.Preset("local", config.PresetOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen.HTTP, cfg.Listen.SSH = "127.0.0.1:0", ""
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan server.Addrs, 1)
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, cfg, slog.New(slog.DiscardHandler), server.Options{Ready: func(a server.Addrs) { ready <- a }})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case a := <-ready:
		return "http://" + a.HTTP.String(), cfg
	case err := <-done:
		t.Fatalf("the server did not start: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the server did not start in time")
	}
	return "", cfg
}

func browserClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := run(t, args...)
	if code != 0 {
		t.Fatalf("moor %s: exit %d: %s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out + errOut
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// approve signs in as email in a browser and approves a CLI's sign-in.
func approve(t *testing.T, base, email, userCode, decision string) {
	t.Helper()
	b := browserClient()
	res, err := b.PostForm(base+"/login", url.Values{"email": {email}, "password": {testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("signing in: %d", res.StatusCode)
	}
	res, err = b.Get(base + "/device?code=" + userCode)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	m := csrfField.FindSubmatch(page)
	if m == nil {
		t.Fatalf("the approval page: %d %s", res.StatusCode, page)
	}
	res, err = b.PostForm(base+"/device", url.Values{"csrf": {string(m[1])}, "code": {userCode}, "decision": {decision}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the decision: %d", res.StatusCode)
	}
}

var userCodeLine = regexp.MustCompile(`(?m)^  ([A-Z]{4}-[A-Z]{4})$`)

// login runs moor login, approving its sign-in in a browser with decision.
func login(t *testing.T, base, email, decision string) (int, string) {
	t.Helper()
	var out, errOut lockedBuffer
	code := make(chan int, 1)
	go func() {
		code <- mainWith([]string{"login", base, "--no-browser", "--name", "test"}, strings.NewReader(""), &out, &errOut)
	}()
	deadline := time.Now().Add(15 * time.Second)
	var userCode string
	for userCode == "" {
		if m := userCodeLine.FindStringSubmatch(out.String()); m != nil {
			userCode = m[1]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no code from moor login: %s %s", out.String(), errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	approve(t, base, email, userCode, decision)
	select {
	case c := <-code:
		return c, out.String() + errOut.String()
	case <-time.After(30 * time.Second):
		t.Fatalf("moor login did not end: %s", out.String())
	}
	return 0, ""
}

// TestAccountsJourney follows a server's accounts through the command line:
// claiming, signing in, users, tokens, keys and contexts.
func TestAccountsJourney(t *testing.T) {
	t.Setenv("MOOR_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	base, cfg := startServer(t)
	layout := server.NewLayout(cfg.DataDir)
	adminToken, err := os.ReadFile(layout.AdminToken)
	if err != nil {
		t.Fatal(err)
	}
	local := []string{"--server", base, "--token", strings.TrimSpace(string(adminToken))}

	// Before the claim: the local administrator, and no sign-in.
	if out := mustRun(t, append([]string{"whoami"}, local...)...); !strings.Contains(out, "local administrator") {
		t.Errorf("whoami: %s", out)
	}
	if code, out, errOut := run(t, "login", base, "--no-browser"); code == 0 || !strings.Contains(out+errOut, "claim it first") {
		t.Errorf("login before the claim: %d %s %s", code, out, errOut)
	}

	claim, err := os.ReadFile(layout.ClaimCode)
	if err != nil {
		t.Fatal(err)
	}
	res, err := browserClient().PostForm(base+"/setup", url.Values{
		"code": {strings.TrimSpace(string(claim))}, "email": {"admin@example.com"}, "password": {testPassword}, "confirm": {testPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the claim: %d", res.StatusCode)
	}

	// Signing in, with approval in a browser.
	code, out := login(t, base, "admin@example.com", "approve")
	if code != 0 || !strings.Contains(out, "Signed in to "+base+" as admin@example.com (context test)") {
		t.Fatalf("login: %d %s", code, out)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(os.Getenv("MOOR_CONFIG")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("the configuration's mode: %v %v", fi, err)
		}
	}
	// Later commands use the context, with no flags.
	if out := mustRun(t, "whoami"); !strings.Contains(out, "admin@example.com (admin) on "+base) {
		t.Errorf("whoami: %s", out)
	}
	if out := mustRun(t, "context"); !strings.Contains(out, "test") || !strings.Contains(out, base) || !strings.Contains(out, "admin@example.com") {
		t.Errorf("context: %s", out)
	}
	if out := mustRun(t, "tokens"); !strings.Contains(out, "moor login") {
		t.Errorf("the sign-in's token is not listed: %s", out)
	}

	// Users.
	out = mustRun(t, "server", "users", "create", "Ada@Example.com", "--name", "Ada")
	if !strings.Contains(out, "Created ada@example.com (member)") || !strings.Contains(out, "/invite?code=") {
		t.Fatalf("users create: %s", out)
	}
	link := regexp.MustCompile(`invite\?code=(\S+)`).FindStringSubmatch(out)[1]
	res, err = browserClient().PostForm(base+"/invite", url.Values{"code": {link}, "password": {testPassword}, "confirm": {testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the invitation: %d", res.StatusCode)
	}
	if out := mustRun(t, "server", "users"); !strings.Contains(out, "ada@example.com") || !strings.Contains(out, "admin@example.com") {
		t.Errorf("users: %s", out)
	}
	if code, out, errOut := run(t, "server", "users", "role", "admin@example.com", "member"); code == 0 || !strings.Contains(out+errOut, "last admin") {
		t.Errorf("demoting the last admin: %d %s%s", code, out, errOut)
	}
	if out := mustRun(t, "server", "users", "invite", "ada@example.com"); !strings.Contains(out, "/invite?code=") {
		t.Errorf("users invite: %s", out)
	}
	mustRun(t, "server", "users", "role", "ada@example.com", "admin")
	mustRun(t, "server", "users", "role", "ada@example.com", "member")

	// A deploy, attributed to who is signed in.
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "index.html"), []byte("<p>hello</p>"), 0o644)
	mustRun(t, "deploy", "--static", site, "--app", "site")

	// Tokens: the secret goes to standard output alone, for scripts.
	code, secret, errOut := run(t, "tokens", "create", "ci", "--scope", "read", "--app", "site", "--expires", "1h")
	secret = strings.TrimSpace(secret)
	if code != 0 || !strings.HasPrefix(secret, "moor_") || !strings.Contains(errOut, "only time") {
		t.Fatalf("tokens create: %d %q %s", code, secret, errOut)
	}
	if out := mustRun(t, "tokens"); !strings.Contains(out, "ci") || !strings.Contains(out, "read") || !strings.Contains(out, "site") {
		t.Errorf("tokens: %s", out)
	}
	limited := []string{"--server", base, "--token", secret}
	if out := mustRun(t, append([]string{"whoami"}, limited...)...); !strings.Contains(out, "Token scopes: read") || !strings.Contains(out, "restricted to the app site") {
		t.Errorf("whoami with the token: %s", out)
	}
	if out := mustRun(t, append([]string{"apps"}, limited...)...); !strings.Contains(out, "site") {
		t.Errorf("apps with a read token: %s", out)
	}
	if code, out, errOut := run(t, append([]string{"deploy", "--static", site, "--app", "site"}, limited...)...); code == 0 || !strings.Contains(out+errOut, "deploy scope") {
		t.Errorf("deploying with a read token: %d %s%s", code, out, errOut)
	}
	mustRun(t, "tokens", "revoke", "ci")
	if code, _, _ := run(t, append([]string{"whoami"}, limited...)...); code == 0 {
		t.Error("a revoked token still works")
	}
	if code, _, _ := run(t, "tokens", "create", "bad", "--expires", "soon"); code == 0 {
		t.Error("a nonsense expiry was accepted")
	}

	// Keys belong to who is signed in.
	key := filepath.Join(t.TempDir(), "k.pub")
	os.WriteFile(key, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ2sKBUMbwT7mhfcOf0rs7CR2PVB7LpKsIzfKfYjCKk6 laptop\n"), 0o644)
	if out := mustRun(t, "keys", "add", key); !strings.Contains(out, "as laptop") {
		t.Errorf("keys add: %s", out)
	}
	if out := mustRun(t, "keys"); !strings.Contains(out, "laptop") {
		t.Errorf("keys: %s", out)
	}
	if out := mustRun(t, append([]string{"keys"}, local...)...); strings.Contains(out, "laptop") {
		t.Errorf("the local administrator sees another's key: %s", out)
	}

	// Pasting a token is the way in without a browser.
	code, secret, _ = run(t, "tokens", "create", "paste", "--scope", "admin")
	if code != 0 {
		t.Fatal("a token to paste")
	}
	if out := mustRunInput(t, secret, "login", base, "--with-token", "--name", "pasted"); !strings.Contains(out, "(context pasted)") {
		t.Errorf("login --with-token: %s", out)
	}
	if code, out := runInput(t, "moor_wrong\n", "login", base, "--with-token", "--name", "wrong"); code == 0 || !strings.Contains(out, "refused the token") {
		t.Errorf("login with a wrong token: %d %s", code, out)
	}
	if out := mustRun(t, "context"); !strings.Contains(out, "* ") {
		t.Errorf("context: %s", out)
	}
	mustRun(t, "context", "use", "test")
	if code, _, _ := run(t, "context", "use", "nope"); code == 0 {
		t.Error("using an unknown context")
	}
	mustRun(t, "context", "rm", "pasted")

	// Denied in the browser, the sign-in fails.
	if code, out := login(t, base, "admin@example.com", "deny"); code == 0 || !strings.Contains(out, "denied") {
		t.Errorf("denied: %d %s", code, out)
	}

	// Signing out revokes the token.
	config, _ := loadClientConfig()
	token := config.Contexts["test"].Token
	if out := mustRun(t, "logout"); !strings.Contains(out, "Signed out of "+base) {
		t.Errorf("logout: %s", out)
	}
	if code, _, _ := run(t, "whoami", "--server", base, "--token", token); code == 0 {
		t.Error("the token works after signing out")
	}
	if code, _, _ := run(t, "logout"); code == 0 {
		t.Error("signing out twice")
	}

	// The local administrator keeps working on the server's own machine.
	if out := mustRun(t, append([]string{"whoami"}, local...)...); !strings.Contains(out, "local administrator") {
		t.Errorf("the local administrator: %s", out)
	}
}

// TestConnectionOrder checks which server and token a command uses.
func TestConnectionOrder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOOR_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("MOOR_SERVER", "")
	t.Setenv("MOOR_TOKEN", "")
	t.Setenv("MOOR_CA_CERT", "")
	// The server configured on this machine, by $MOORAGE_CONFIG.
	cfgFile, data := filepath.Join(dir, "server.toml"), filepath.Join(dir, "data")
	if code, out, errOut := run(t, "server", "init", "--preset", "local", "--config", cfgFile, "--data-dir", data); code != 0 {
		t.Fatalf("init: %s%s", out, errOut)
	}
	layout := server.NewLayout(data)
	os.MkdirAll(filepath.Dir(layout.AdminToken), 0o700)
	os.WriteFile(layout.AdminToken, []byte("moor_local\n"), 0o600)
	saved := &clientConfig{Current: "work", Contexts: map[string]clientContext{
		"work":  {Server: "https://work.example.com", Token: "moor_work", CACert: "work.pem"},
		"other": {Server: "https://other.example.com", Token: "moor_other"},
	}}
	if err := saved.save(); err != nil {
		t.Fatal(err)
	}
	check := func(name string, conn connection, server, token, ca string) {
		t.Helper()
		s, tok, c, err := conn.resolve()
		if err != nil || s != server || tok != token || c != ca {
			t.Errorf("%s: %q %q %q %v; want %q %q %q", name, s, tok, c, err, server, token, ca)
		}
	}
	check("the current context", connection{}, "https://work.example.com", "moor_work", "work.pem")
	check("a server with a context", connection{server: "https://other.example.com/"}, "https://other.example.com", "moor_other", "")
	check("a server and token", connection{server: "https://x.example.com", token: "moor_x"}, "https://x.example.com", "moor_x", "")
	t.Setenv("MOOR_SERVER", "https://env.example.com")
	t.Setenv("MOOR_TOKEN", "moor_env")
	check("the environment", connection{}, "https://env.example.com", "moor_env", "")
	t.Setenv("MOOR_SERVER", "")
	t.Setenv("MOOR_TOKEN", "")
	// $MOORAGE_CONFIG names a server on purpose: it beats the current context.
	t.Setenv("MOORAGE_CONFIG", cfgFile)
	check("an explicit local server", connection{}, "http://127.0.0.1:8080", "moor_local", "")
}

func TestInitPrintsTheClaimLink(t *testing.T) {
	dir := t.TempDir()
	cfgFile, data := filepath.Join(dir, "server.toml"), filepath.Join(dir, "data")
	code, out, errOut := run(t, "server", "init", "--preset", "local", "--config", cfgFile, "--data-dir", data)
	if code != 0 {
		t.Fatalf("init: %d %s%s", code, out, errOut)
	}
	layout := server.NewLayout(data)
	claim, err := os.ReadFile(layout.ClaimCode)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/setup?code="+strings.TrimSpace(string(claim))) {
		t.Errorf("the output lacks the link: %s", out)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(layout.ClaimCode); fi.Mode().Perm() != 0o600 {
			t.Errorf("the code's mode: %v", fi.Mode())
		}
	}
	// A data directory with a database has a server already.
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "moorage.db"), nil, 0o600)
	code, out, _ = run(t, "server", "init", "--preset", "local", "--config", filepath.Join(dir, "other.toml"), "--data-dir", dir2)
	if code != 0 || strings.Contains(out, "/setup?code=") {
		t.Errorf("init beside a database: %d %s", code, out)
	}
}

func mustRunInput(t *testing.T, input string, args ...string) string {
	t.Helper()
	code, out := runInput(t, input, args...)
	if code != 0 {
		t.Fatalf("moor %s: exit %d: %s", strings.Join(args, " "), code, out)
	}
	return out
}

func runInput(t *testing.T, input string, args ...string) (int, string) {
	t.Helper()
	code, out, errOut := runWithInput(t, input, args...)
	return code, out + errOut
}
