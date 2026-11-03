package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

const testPassword = "a long and unusual passphrase"

// claimServer creates the server's first account through its setup page.
func claimServer(t *testing.T, cfg config.Config, addrs Addrs, email string) {
	t.Helper()
	code, err := os.ReadFile(NewLayout(cfg.DataDir).ClaimCode)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.PostForm("http://"+addrs.HTTP.String()+"/setup", url.Values{
		"code": {strings.TrimSpace(string(code))}, "email": {email}, "password": {testPassword}, "confirm": {testPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Request.Response == nil || res.StatusCode != http.StatusOK {
		t.Fatalf("claiming: %d", res.StatusCode)
	}
}

// browser is a client with cookies, that signs in through the pages.
func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// userClient signs in as an existing user, through the API's login, and returns
// an API client with a new admin-scope token of theirs.
func userClient(t *testing.T, addrs Addrs, email string) *client.Client {
	t.Helper()
	base := "http://" + addrs.HTTP.String()
	b := browser(t)
	body, _ := json.Marshal(apitypes.LoginRequest{Email: email, Password: testPassword})
	res, err := b.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var login apitypes.LoginResult
	json.NewDecoder(res.Body).Decode(&login)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || login.User.Email != email {
		t.Fatalf("signing in as %s: %d", email, res.StatusCode)
	}
	body, _ = json.Marshal(apitypes.NewToken{Name: "test", Scopes: []string{"admin"}})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/me/tokens", bytes.NewReader(body))
	req.Header.Set("X-CSRF-Token", login.CSRFToken)
	res, err = b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created apitypes.CreatedToken
	json.NewDecoder(res.Body).Decode(&created)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("a token for %s: %d", email, res.StatusCode)
	}
	c, err := client.New(base, created.Secret, "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// addMember creates a member through the API and accepts their invitation.
func addMember(t *testing.T, admin *client.Client, addrs Addrs, email string) *client.Client {
	t.Helper()
	inv, err := admin.CreateUser(context.Background(), apitypes.NewUser{Email: email})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(inv.URL)
	res, err := browser(t).PostForm("http://"+addrs.HTTP.String()+"/invite", url.Values{
		"code": {u.Query().Get("code")}, "password": {testPassword}, "confirm": {testPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("accepting %s's invitation: %d", email, res.StatusCode)
	}
	return userClient(t, addrs, email)
}

func deploySite(t *testing.T, c *client.Client, app string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>"+app+"</p>"), 0o644)
	if _, _, err := c.DeployStatic(context.Background(), app, "production", dir, manifest.Static{}, nil); err != nil {
		t.Fatalf("deploying %s: %v", app, err)
	}
}

func status(err error) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

func TestUnclaimedServerAndClaim(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	if _, body := get(t, addrs.HTTP, "moorage.localhost", "/"); !strings.Contains(body, "no accounts yet") {
		t.Errorf("the start page: %s", body)
	}
	// The local admin token works before the claim, and after.
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	if me, err := c.Me(ctx); err != nil || !me.Local || me.Role != "admin" {
		t.Fatalf("the local administrator: %+v %v", me, err)
	}
	anonymous, _ := client.New("http://"+addrs.HTTP.String(), "", "")
	if _, err := anonymous.StartDevice(ctx, "moor"); status(err) != http.StatusConflict {
		t.Errorf("a CLI's sign-in before the claim: %v", err)
	}

	claimServer(t, cfg, addrs, "admin@example.com")
	if _, err := os.Stat(NewLayout(cfg.DataDir).ClaimCode); !os.IsNotExist(err) {
		t.Errorf("the claim code remains: %v", err)
	}
	if me, err := c.Me(ctx); err != nil || !me.Local {
		t.Errorf("the local administrator after the claim: %+v %v", me, err)
	}
	admin := userClient(t, addrs, "admin@example.com")
	me, err := admin.Me(ctx)
	if err != nil || me.Local || me.Email != "admin@example.com" || me.Role != "admin" {
		t.Errorf("the first user: %+v %v", me, err)
	}
	// The OpenAPI document is served to anyone.
	res, err := http.Get("http://" + addrs.HTTP.String() + "/api/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(`"openapi": "3.1.0"`)) {
		t.Errorf("the OpenAPI document: %d", res.StatusCode)
	}
}

func TestListsAndRestrictedTokens(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	claimServer(t, cfg, addrs, "admin@example.com")
	ctx := context.Background()
	admin := userClient(t, addrs, "admin@example.com")
	member := addMember(t, admin, addrs, "member@example.com")

	// Deployments record who did them.
	for _, app := range []string{"alpha", "bravo", "charlie"} {
		deploySite(t, member, app)
	}
	rels, err := admin.Releases(ctx, "alpha", "production")
	if err != nil || len(rels) != 1 || rels[0].CreatedBy != "member@example.com" {
		t.Errorf("the release's author: %+v %v", rels, err)
	}

	// Lists come in pages, and the client follows them.
	token := func(c *client.Client, t2 apitypes.NewToken) *client.Client {
		created, err := c.CreateToken(ctx, t2)
		if err != nil {
			t.Fatalf("a token: %v", err)
		}
		next, _ := client.New("http://"+addrs.HTTP.String(), created.Secret, "")
		return next
	}
	apps, err := admin.Apps(ctx)
	if err != nil || len(apps) != 3 {
		t.Fatalf("all the apps: %+v %v", apps, err)
	}
	secret, err := admin.CreateToken(ctx, apitypes.NewToken{Name: "paging", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	var page apitypes.Page[apitypes.App]
	getJSON(t, addrs, secret.Secret, "/api/v1/apps?limit=2", &page)
	if len(page.Items) != 2 || page.Items[0].Name != "alpha" || page.NextCursor == "" {
		t.Fatalf("the first page: %+v", page)
	}
	cursor := page.NextCursor
	page = apitypes.Page[apitypes.App]{}
	getJSON(t, addrs, secret.Secret, "/api/v1/apps?limit=2&cursor="+cursor, &page)
	if len(page.Items) != 1 || page.Items[0].Name != "charlie" || page.NextCursor != "" {
		t.Errorf("the last page: %+v", page)
	}
	for _, bad := range []string{"/api/v1/apps?limit=0", "/api/v1/apps?limit=999", "/api/v1/apps?cursor=not-base64!"} {
		if code := getStatus(t, addrs, secret.Secret, bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}

	// A token for one app sees that app, and nothing else.
	shop := token(member, apitypes.NewToken{Name: "alpha only", Scopes: []string{"deploy"}, App: "alpha"})
	seen, err := shop.Apps(ctx)
	if err != nil || len(seen) != 1 || seen[0].Name != "alpha" {
		t.Errorf("its apps: %+v %v", seen, err)
	}
	if _, err := shop.Releases(ctx, "bravo", "production"); status(err) != http.StatusForbidden {
		t.Errorf("another app's releases: %v", err)
	}
	deploySite(t, shop, "alpha")
	deps, err := member.Deployments(ctx, "bravo", "production", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) > 0 {
		if _, err := shop.Deployment(ctx, deps[0].ID); status(err) != http.StatusNotFound {
			t.Errorf("another app's deployment: %v", err)
		}
	}
	if _, err := member.CreateToken(ctx, apitypes.NewToken{Name: "x", Scopes: []string{"read"}, App: "nope"}); status(err) != http.StatusNotFound {
		t.Errorf("a token for an unknown app: %v", err)
	}
	if _, err := member.CreateToken(ctx, apitypes.NewToken{Name: "y", Scopes: []string{"everything"}}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("an unknown scope: %v", err)
	}
	var apiErr *client.APIError
	_, err = member.CreateToken(ctx, apitypes.NewToken{Name: "alpha only", Scopes: []string{"read"}})
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Code != "name_taken" {
		t.Errorf("a name in use: %v", err)
	}
	_, err = member.CreateToken(ctx, apitypes.NewToken{Name: "z"})
	if !errors.As(err, &apiErr) || len(apiErr.Errors) != 1 || apiErr.Errors[0].Field != "scopes" {
		t.Errorf("no scopes: %+v", err)
	}

	// Members do not manage users; the last admin stays.
	if _, err := member.Users(ctx); status(err) != http.StatusForbidden {
		t.Errorf("a member listing users: %v", err)
	}
	if _, err := member.CreateUser(ctx, apitypes.NewUser{Email: "x@example.com"}); status(err) != http.StatusForbidden {
		t.Errorf("a member creating a user: %v", err)
	}
	demote := "member"
	if _, err := admin.ChangeUser(ctx, "admin@example.com", apitypes.UserChange{Role: &demote}); status(err) != http.StatusConflict {
		t.Errorf("demoting the last admin: %v", err)
	}
	if _, err := admin.CreateUser(ctx, apitypes.NewUser{Email: "member@example.com"}); status(err) != http.StatusConflict {
		t.Errorf("a taken address: %v", err)
	}
	_, err = admin.CreateUser(ctx, apitypes.NewUser{Email: "not an address"})
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity || len(apiErr.Errors) != 1 || apiErr.Errors[0].Field != "email" {
		t.Errorf("a bad address: %+v", err)
	}
	users, err := admin.Users(ctx)
	if err != nil || len(users) != 2 {
		t.Errorf("the users: %+v %v", users, err)
	}

	// Disabling a user stops their tokens at once; enabling restores them.
	disabled := true
	if _, err := admin.ChangeUser(ctx, "member@example.com", apitypes.UserChange{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := member.Me(ctx); status(err) != http.StatusUnauthorized {
		t.Errorf("a disabled user's token: %v", err)
	}
	disabled = false
	admin.ChangeUser(ctx, "member@example.com", apitypes.UserChange{Disabled: &disabled})
	if _, err := member.Me(ctx); err != nil {
		t.Errorf("after enabling: %v", err)
	}

	// Signing out with a token revokes it.
	if err := shop.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := shop.Me(ctx); status(err) != http.StatusUnauthorized {
		t.Errorf("a revoked token: %v", err)
	}
	if _, err := member.RevokeToken(ctx, "nothing"); status(err) != http.StatusNotFound {
		t.Errorf("revoking nothing: %v", err)
	}
}

func rawGet(t *testing.T, addrs Addrs, token, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, data
}

func getStatus(t *testing.T, addrs Addrs, token, path string) int {
	t.Helper()
	code, _ := rawGet(t, addrs, token, path)
	return code
}

func getJSON(t *testing.T, addrs Addrs, token, path string, v any) {
	t.Helper()
	code, data := rawGet(t, addrs, token, path)
	if code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, code, data)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

// TestPushAsUsers pushes with the git and ssh command lines as users: each
// push is theirs, a read-only token cannot push, and a disabled user's key and
// token stop working.
func TestPushAsUsers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if _, err := exec.LookPath("ssh"); err != nil || runtime.GOOS == "windows" {
		t.Skip("pushing over SSH is tested with OpenSSH on Linux and macOS")
	}
	cfg := testConfig(t)
	cfg.Listen.SSH = "127.0.0.1:0"
	addrs, _ := start(t, cfg)
	ctx := context.Background()
	claimServer(t, cfg, addrs, "admin@example.com")
	admin := userClient(t, addrs, "admin@example.com")
	member := addMember(t, admin, addrs, "member@example.com")

	home := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "member")
	keyFile := filepath.Join(home, "id_ed25519")
	os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600)
	sshPub, _ := ssh.NewPublicKey(pub)
	authorized := string(ssh.MarshalAuthorizedKey(sshPub))
	if _, err := member.AddSSHKey(ctx, apitypes.NewSSHKey{Name: "laptop", PublicKey: authorized}); err != nil {
		t.Fatal(err)
	}
	// A key has one owner.
	if _, err := admin.AddSSHKey(ctx, apitypes.NewSSHKey{Name: "same", PublicKey: authorized}); status(err) != http.StatusConflict {
		t.Errorf("another user adding the same key: %v", err)
	}
	if keys, _ := admin.SSHKeys(ctx); len(keys) != 0 {
		t.Errorf("the admin's keys: %+v", keys)
	}
	_, port, _ := net.SplitHostPort(addrs.SSH.String())

	repo := &gitRepo{t: t, dir: copyTree(t, filepath.Join("..", "..", "testdata", "apps", "static")), env: []string{
		"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_SSH_COMMAND=ssh -i " + keyFile + " -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=" + filepath.Join(home, "known_hosts"),
	}}
	repo.must("init", "-q", "-b", "main")
	repo.must("add", ".")
	repo.must("commit", "-q", "-m", "First")
	remote := "ssh://git@127.0.0.1:" + port + "/teamsite.git"

	if out := repo.must("push", remote, "main"); !strings.Contains(out, "✔ live") {
		t.Errorf("a member's push: %s", out)
	}
	deps, err := admin.Deployments(ctx, "teamsite", "production", 1)
	if err != nil || len(deps) != 1 || deps[0].CreatedBy != "member@example.com" {
		t.Errorf("who pushed: %+v %v", deps, err)
	}

	// Over HTTPS: a read token fetches, and cannot push; a token for another
	// app reaches nothing here; a deploy token pushes.
	_, httpPort, _ := net.SplitHostPort(addrs.HTTP.String())
	resolve := "http.curloptResolve=moorage.localhost:" + httpPort + ":127.0.0.1"
	httpRemote := func(secret string) string {
		return "http://moor:" + secret + "@moorage.localhost:" + httpPort + "/git/teamsite.git"
	}
	secretFor := func(name, app string, scopes ...string) string {
		created, err := member.CreateToken(ctx, apitypes.NewToken{Name: name, Scopes: scopes, App: app})
		if err != nil {
			t.Fatal(err)
		}
		return created.Secret
	}
	read := secretFor("read", "", "read")
	if out, err := repo.git("-c", resolve, "ls-remote", httpRemote(read)); err != nil || !strings.Contains(out, "refs/heads/main") {
		t.Errorf("fetching with a read token: %v %s", err, out)
	}
	repo.must("commit", "-q", "--allow-empty", "-m", "Second")
	if out, err := repo.git("-c", resolve, "push", httpRemote(read), "main"); err == nil {
		t.Errorf("pushing with a read token: %s", out)
	}
	deploySite(t, member, "elsewhere")
	elsewhere := secretFor("elsewhere", "elsewhere", "deploy")
	if out, err := repo.git("-c", resolve, "push", httpRemote(elsewhere), "main"); err == nil {
		t.Errorf("pushing with another app's token: %s", out)
	}
	deployToken := secretFor("deploy", "teamsite", "deploy")
	if out, err := repo.git("-c", resolve, "push", httpRemote(deployToken), "main"); err != nil || !strings.Contains(out, "✔ live") {
		t.Errorf("pushing with a deploy token: %v %s", err, out)
	}

	// A disabled user can push no more, by key or by token.
	disabled := true
	if _, err := admin.ChangeUser(ctx, "member@example.com", apitypes.UserChange{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	repo.must("commit", "-q", "--allow-empty", "-m", "Third")
	if out, err := repo.git("push", remote, "main"); err == nil {
		t.Errorf("a disabled user's key: %s", out)
	}
	if out, err := repo.git("-c", resolve, "push", httpRemote(deployToken), "main"); err == nil {
		t.Errorf("a disabled user's token: %s", out)
	}
}
