package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/testroles"
)

// pushRepo is a repository that pushes over HTTP with the server's admin token.
func pushRepo(t *testing.T, cfg config.Config, addrs Addrs, app string, files map[string]string) (*gitRepo, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := &gitRepo{t: t, dir: writeSite(t, files), env: []string{
		"HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}}
	repo.must("init", "-q", "-b", "main")
	repo.must("add", ".")
	repo.must("commit", "-q", "-m", "First")
	token, err := os.ReadFile(NewLayout(cfg.DataDir).AdminToken)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(addrs.HTTP.String())
	remote := "http://moor:" + strings.TrimSpace(string(token)) + "@moorage.localhost:" + port + "/git/" + app + ".git"
	repo.must("remote", "add", "moorage", remote)
	return repo, []string{"-c", "http.curloptResolve=moorage.localhost:" + port + ":127.0.0.1"}
}

func (g *gitRepo) write(name, content string) {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.dir, name), []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

// commitAndPush commits everything and pushes a ref, and returns what the push said.
func (g *gitRepo) commitAndPush(resolve []string, message string, push ...string) string {
	g.t.Helper()
	g.must("add", "-A")
	g.must("commit", "-q", "--allow-empty", "-m", message)
	return g.must(append(append(append([]string{}, resolve...), "push", "moorage"), push...)...)
}

// environmentManifest is an app that says which environment it is, by its own [env] and overrides.
func environmentManifest(body, more string) string {
	return `[app]
runtime = "command"

[processes.web]
command = ` + tomlString(testroles.AppCommand()) + `
health = { path = "/health", interval = "100ms", startup = "30s" }

[env]
TESTAPP_BODY = ` + tomlString(body) + `

[release]
on_create = 'echo seeded > "$MOORAGE_DATA_DIR/seeded"'

[deploy]
drain = "1s"
` + more
}

// TestPreviewsAndEnvironments takes an app with real instances through branches, environments,
// promotions and locks, by git push, the API and the console.
func TestPreviewsAndEnvironments(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	token := b.token()
	ctx := context.Background()
	serves := func(host, want string) {
		t.Helper()
		if res, body := get(t, addrs.HTTP, host, "/"); res.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d %q, want %q", host, res.StatusCode, body, want)
		}
	}
	envVar := func(host, name string) string {
		t.Helper()
		_, body := get(t, addrs.HTTP, host, "/env?name="+name)
		return body
	}

	// Production, from main, with variables for previews to inherit or not.
	repo, resolve := pushRepo(t, cfg, addrs, "shop", map[string]string{"moorage.toml": environmentManifest("from main", "")})
	if out := repo.must(append(append([]string{}, resolve...), "push", "moorage", "main")...); !strings.Contains(out, "✔ live") {
		t.Fatalf("the first push: %s", out)
	}
	serves("shop.localhost", "from main")
	if res, _ := get(t, addrs.HTTP, "shop.localhost", "/"); res.Header.Get("X-Robots-Tag") != "" {
		t.Errorf("production asks not to be indexed: %v", res.Header)
	}
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{
		{Name: "SHARED", Value: "from production"},
		{Name: "SEALED", Value: "a-sealed-value", Secret: true},
		{Name: "LIVE_KEY", Value: "only-production", Secret: true, ProductionOnly: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if envVar("shop.localhost", "LIVE_KEY") != "only-production" || envVar("shop.localhost", "MOORAGE_ENV_KIND") != "production" {
		t.Errorf("production's variables: %q", envVar("shop.localhost", "LIVE_KEY"))
	}

	// A branch makes a preview, at its own address, with what it inherits.
	repo.must("switch", "-q", "-c", "feature/new-pricing")
	repo.write("moorage.toml", environmentManifest("from a branch", "[environments.preview]\nenv = { ONLY_PREVIEWS = \"yes\" }\n"))
	out := repo.commitAndPush(resolve, "New pricing", "feature/new-pricing")
	if !strings.Contains(out, "✔ live") || !strings.Contains(out, "shop · feature-new-pricing") {
		t.Fatalf("the push of a branch: %s", out)
	}
	const preview = "shop--feature-new-pricing.localhost"
	serves(preview, "from a branch")
	serves("shop.localhost", "from main")
	if res, _ := get(t, addrs.HTTP, preview, "/"); !strings.Contains(res.Header.Get("X-Robots-Tag"), "noindex") {
		t.Errorf("a preview may be indexed: %v", res.Header)
	}
	for name, want := range map[string]string{
		"SHARED": "from production", "SEALED": "a-sealed-value", "LIVE_KEY": "", "ONLY_PREVIEWS": "yes",
		"MOORAGE_ENV": "feature-new-pricing", "MOORAGE_ENV_KIND": "preview", "MOORAGE_URL": "http://" + preview + ":0",
	} {
		if got := envVar(preview, name); got != want {
			t.Errorf("a preview's %s: %q, want %q", name, got, want)
		}
	}
	dataDir := filepath.Join(cfg.DataDir, "data", "shop")
	if !exists(filepath.Join(dataDir, "feature-new-pricing", "seeded")) {
		t.Errorf("on_create did not run for the new environment")
	}

	// It says so to the API and the console, and a further push releases to it.
	list, err := c.Previews(ctx)
	if err != nil || len(list) != 1 || list[0].Branch != "feature/new-pricing" || list[0].Kind != "preview" || list[0].ExpiresAt.Before(time.Now().Add(6*24*time.Hour)) {
		t.Errorf("the previews: %+v %v", list, err)
	}
	vars, err := c.Vars(ctx, "shop", "feature-new-pricing")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, v := range vars {
		sources[v.Name] = v.Source
	}
	if sources["SHARED"] != "production" || sources["LIVE_KEY"] != "" {
		t.Errorf("where its variables come from: %v", sources)
	}
	_, page := b.get("/previews")
	contains(t, "the previews page", page, "feature-new-pricing", "feature/new-pricing", preview, "Destroy", "Pin", "Healthy")
	_, page = b.get("/apps/shop?env=feature-new-pricing")
	contains(t, "the preview's page", page, "shop · feature-new-pricing", preview, "web.1", `?env=feature-new-pricing`, `<option value="feature-new-pricing" selected>`)
	for _, path := range []string{"/deployments", "/logs", "/variables", "/variables/edit", "/deploy"} {
		res, page := b.get("/apps/shop" + path + "?env=feature-new-pricing")
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s of the preview: %d\n%s", path, res.StatusCode, page)
		}
		contains(t, path+" of the preview", page, "shop · feature-new-pricing")
	}
	_, page = b.get("/apps/shop/variables?env=feature-new-pricing")
	contains(t, "its variables", page, "Inherited from production", "SHARED")
	if res, _ := b.get("/apps/shop?env=nothing-here"); res.StatusCode != http.StatusNotFound {
		t.Errorf("an environment that isn't there: %d", res.StatusCode)
	}
	repo.write("moorage.toml", environmentManifest("from a branch, again", "[environments.preview]\nenv = { ONLY_PREVIEWS = \"yes\" }\n"))
	if out := repo.commitAndPush(resolve, "More pricing", "feature/new-pricing"); !strings.Contains(out, "✔ live") {
		t.Errorf("the second push: %s", out)
	}
	serves(preview, "from a branch, again")
	if e, err := c.Environment(ctx, "shop", "feature-new-pricing"); err != nil || e.Release == nil || e.Release.Version != 2 {
		t.Errorf("after the second push: %+v %v", e, err)
	}

	// The console acts on the environment its address names: scale and restart here leave production be.
	before := instances(t, c, "shop")
	if res, body := b.post("/apps/shop/scale?env=feature-new-pricing", url.Values{"csrf": {token}, "process.web": {"2"}}); res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "env=feature-new-pricing") {
		t.Errorf("scaling a preview: %d %s\n%s", res.StatusCode, res.Header.Get("Location"), body)
	}
	if e, err := c.Environment(ctx, "shop", "feature-new-pricing"); err != nil || len(e.Instances) != 2 {
		t.Errorf("the preview's instances: %+v %v", e.Instances, err)
	}
	if after := instances(t, c, "shop"); len(after) != len(before) || after[0].PID != before[0].PID {
		t.Errorf("production moved: %+v, then %+v", before, after)
	}

	// Pinning is by API or console, and destroying by deleting its branch.
	pinned, err := c.PinEnvironment(ctx, "shop", "feature-new-pricing", true)
	if err != nil || !pinned.Pinned || !pinned.ExpiresAt.IsZero() {
		t.Errorf("pinned: %+v %v", pinned, err)
	}
	if res, _ := b.post("/apps/shop/environments/feature-new-pricing/pin", url.Values{"csrf": {token}, "from": {"previews"}}); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/previews?done=unpinned" {
		t.Errorf("unpinned in the console: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if out := repo.must(append(append([]string{}, resolve...), "push", "moorage", "--delete", "feature/new-pricing")...); !strings.Contains(out, "destroyed the preview feature-new-pricing") {
		t.Errorf("deleting the branch: %s", out)
	}
	waitUntil(t, "the preview to stop answering", func() bool {
		res, _ := get(t, addrs.HTTP, preview, "/")
		return res.StatusCode == http.StatusNotFound
	})
	if exists(filepath.Join(dataDir, "feature-new-pricing")) {
		t.Errorf("the preview's data stayed")
	}
	serves("shop.localhost", "from main")

	// A named environment: declared, made by its first push, with its own overrides.
	repo.must("switch", "-q", "main")
	staging := "[environments.staging]\nbranch = \"develop\"\nenv = { WHERE = \"staging\" }\nprocesses.web.instances = 2\n"
	repo.must("switch", "-q", "-c", "develop")
	repo.write("moorage.toml", environmentManifest("from develop", staging))
	if out := repo.commitAndPush(resolve, "Develop", "develop"); !strings.Contains(out, "✔ live") || !strings.Contains(out, "shop · staging") {
		t.Fatalf("the push of develop: %s", out)
	}
	serves("shop--staging.localhost", "from develop")
	e, err := c.Environment(ctx, "shop", "staging")
	if err != nil || e.Kind != "named" || e.Branch != "develop" || len(e.Instances) != 2 {
		t.Errorf("staging: %+v %v", e, err)
	}
	if envVar("shop--staging.localhost", "WHERE") != "staging" || envVar("shop--staging.localhost", "SHARED") != "" || envVar("shop--staging.localhost", "MOORAGE_ENV_KIND") != "named" {
		t.Errorf("staging's variables: WHERE=%q SHARED=%q", envVar("shop--staging.localhost", "WHERE"), envVar("shop--staging.localhost", "SHARED"))
	}

	// Promoting releases the same build, with production's variables and its own overrides.
	if _, err := c.ChangeVars(ctx, "shop", "staging", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "BUILT_WITH", Value: "staging", Build: true}}, NoRestart: true}); err != nil {
		t.Fatal(err)
	}
	stagingEnv, _ := c.Environment(ctx, "shop", "staging")
	promoted, err := c.Promote(ctx, "shop", "staging", "production")
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Release == nil || promoted.Release.Build != stagingEnv.Release.Build {
		t.Errorf("not the same build: %+v, %+v", promoted.Release, stagingEnv.Release)
	}
	if len(promoted.Warnings) != 1 || !strings.Contains(promoted.Warnings[0], "BUILT_WITH") {
		t.Errorf("the warning about build variables: %v", promoted.Warnings)
	}
	serves("shop.localhost", "from develop")
	if envVar("shop.localhost", "LIVE_KEY") != "only-production" || envVar("shop.localhost", "WHERE") != "" {
		t.Errorf("production's variables after a promotion: %q %q", envVar("shop.localhost", "LIVE_KEY"), envVar("shop.localhost", "WHERE"))
	}
	_, page = b.get("/apps/shop/environments")
	contains(t, "the environments page", page, "staging", "production", "branch develop", "Promote", "shop--staging.localhost", "Lock", "Make an environment", "Preview a branch")

	// A lock stops deployments, and not rollbacks or variables.
	locked, err := c.LockEnvironment(ctx, "shop", "production")
	if err != nil || locked.LockedBy == "" {
		t.Errorf("locked: %+v %v", locked, err)
	}
	repo.must("switch", "-q", "main")
	repo.write("moorage.toml", environmentManifest("held back", ""))
	if out := repo.commitAndPush(resolve, "Held back", "main"); !strings.Contains(out, "main is kept, not deployed") || !strings.Contains(out, "locked") {
		t.Errorf("a push to a locked environment: %s", out)
	}
	serves("shop.localhost", "from develop")
	var problem *client.APIError
	if _, err := c.Promote(ctx, "shop", "staging", "production"); !errors.As(err, &problem) || problem.Status != http.StatusConflict {
		t.Errorf("a promotion to a locked environment: %v", err)
	}
	if _, err := c.Redeploy(ctx, "shop", "production"); !errors.As(err, &problem) || problem.Status != http.StatusConflict {
		t.Errorf("a redeploy of a locked environment: %v", err)
	}
	_, page = b.get("/apps/shop")
	contains(t, "the locked app", page, "This environment is locked by")
	rolled, err := c.Rollback(ctx, "shop", "production", 0)
	if err != nil {
		t.Fatalf("a rollback of a locked environment: %v", err)
	}
	if _, page := b.get("/deployments/" + rolled.ID); strings.Contains(page, "lock-offer") {
		t.Errorf("a locked environment is offered a lock")
	}
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "CALM", Value: "yes"}}}); err != nil {
		t.Errorf("a variable of a locked environment: %v", err)
	}
	if _, err := c.UnlockEnvironment(ctx, "shop", "production"); err != nil {
		t.Errorf("unlocking: %v", err)
	}
	repo.write("moorage.toml", environmentManifest("released at last", ""))
	if out := repo.commitAndPush(resolve, "Released", "main"); !strings.Contains(out, "✔ live") {
		t.Errorf("a push after unlocking: %s", out)
	}
	serves("shop.localhost", "released at last")

	// A rollback offers the lock that keeps it, on the page it goes to.
	rolled, err = c.Rollback(ctx, "shop", "production", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, page = b.get("/deployments/" + rolled.ID)
	contains(t, "after a rollback", page, "The next deployment would replace this rollback", "/apps/shop/environments/production/lock")

	// What cannot be done says so.
	if err := c.DeleteEnvironment(ctx, "shop", "production"); !errors.As(err, &problem) || problem.Status != http.StatusConflict {
		t.Errorf("deleting production: %v", err)
	}
	if _, err := c.CreateEnvironment(ctx, "shop", apitypes.NewEnvironment{Name: "Not--Valid"}); !errors.As(err, &problem) || problem.Status != http.StatusUnprocessableEntity {
		t.Errorf("a bad name: %v", err)
	}
	if _, err := c.CreateEnvironment(ctx, "shop", apitypes.NewEnvironment{Name: "staging"}); !errors.As(err, &problem) || problem.Status != http.StatusConflict {
		t.Errorf("a name in use: %v", err)
	}
	if _, err := c.CreatePreview(ctx, "shop", "no-such-branch"); !errors.As(err, &problem) || problem.Status != http.StatusNotFound {
		t.Errorf("a preview of a branch that isn't here: %v", err)
	}

	// The console makes and removes them too; a preview of a branch that is here deploys.
	if res, body := b.post("/apps/shop/environments", url.Values{"csrf": {token}, "name": {"sandbox"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("making an environment: %d\n%s", res.StatusCode, body)
	}
	if res, body := b.post("/apps/shop/environments/sandbox/delete", url.Values{"csrf": {token}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("removing it: %d\n%s", res.StatusCode, body)
	}
	if res, _ := b.get("/apps/shop?env=sandbox"); res.StatusCode != http.StatusNotFound {
		t.Errorf("after removing it: %d", res.StatusCode)
	}
	repo.must("switch", "-q", "-c", "made-by-hand")
	repo.write("moorage.toml", environmentManifest("[skip deploy] made by hand", ""))
	repo.commitAndPush(resolve, "Not yet [skip deploy]", "made-by-hand")
	queued, err := c.CreatePreview(ctx, "shop", "made-by-hand")
	if err != nil {
		t.Fatal(err)
	}
	if d := followed(t, c, queued.ID); d.Status != apitypes.StatusLive || d.Env != "made-by-hand" {
		t.Errorf("a preview made by hand: %+v", d)
	}

	// A rename takes every environment along.
	if _, err := c.RenameApp(ctx, "shop", "store"); err != nil {
		t.Fatal(err)
	}
	serves("store--staging.localhost", "from develop")
	serves("store--made-by-hand.localhost", "made by hand")
	if res, _ := get(t, addrs.HTTP, "shop--staging.localhost", "/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("under the old name: %d", res.StatusCode)
	}

	// Deleting the app ends all of them.
	if err := c.DeleteApp(ctx, "store", false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "every environment to stop answering", func() bool {
		res, _ := get(t, addrs.HTTP, "store--staging.localhost", "/")
		return res.StatusCode == http.StatusNotFound
	})
	if left, err := c.Previews(ctx); err != nil || len(left) != 0 {
		t.Errorf("previews of a deleted app: %+v %v", left, err)
	}
}

// TestPreviewsExpire lets previews run out of time, by their manifest's [previews] ttl: one
// that is being used and one that is pinned stay, and one left alone goes.
func TestPreviewsExpire(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	site := map[string]string{"index.html": "<p>hello</p>\n", "moorage.toml": "[app]\nruntime = \"static\"\n\n[previews]\nttl = \"2s\"\n"}
	repo, resolve := pushRepo(t, cfg, addrs, "docs", site)
	repo.must(append(append([]string{}, resolve...), "push", "moorage", "main")...)
	for _, branch := range []string{"left-alone", "in-use", "kept"} {
		repo.must("switch", "-q", "-c", branch, "main")
		repo.commitAndPush(resolve, "On "+branch, branch)
	}
	if _, err := c.PinEnvironment(ctx, "docs", "kept", true); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "a preview to expire", func() bool {
		get(t, addrs.HTTP, "docs--in-use.localhost", "/")
		_, err := c.Environment(ctx, "docs", "left-alone")
		return status(err) == http.StatusNotFound
	})
	if res, _ := get(t, addrs.HTTP, "docs--left-alone.localhost", "/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("the preview that went: %d", res.StatusCode)
	}
	for _, env := range []string{"in-use", "kept"} {
		if e, err := c.Environment(ctx, "docs", env); err != nil || e.Name != env {
			t.Errorf("%s: %+v %v", env, e, err)
		}
	}
	if res, _ := get(t, addrs.HTTP, "docs--in-use.localhost", "/"); res.StatusCode != http.StatusOK {
		t.Errorf("the preview in use: %d", res.StatusCode)
	}
	events, err := c.Events(ctx, "docs", "left-alone", 5)
	if err != nil || !strings.Contains(typesOf(events), "environment.deleted") {
		t.Errorf("the event of its going: %s %v", typesOf(events), err)
	}
}

// TestPreviewLimits refuses the eleventh preview, and says what [previews] leaves out.
func TestPreviewLimits(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	site := map[string]string{"index.html": "<p>hello</p>\n", "moorage.toml": "[app]\nruntime = \"static\"\n\n[previews]\nbranches = [\"*\", \"!bots-*\"]\n"}
	repo, resolve := pushRepo(t, cfg, addrs, "docs", site)
	repo.must(append(append([]string{}, resolve...), "push", "moorage", "main")...)
	for i := range 10 {
		branch := fmt.Sprintf("work-%d", i)
		repo.must("switch", "-q", "-c", branch, "main")
		if out := repo.commitAndPush(resolve, "On "+branch, branch); !strings.Contains(out, "✔ live") {
			t.Fatalf("%s: %s", branch, out)
		}
	}
	repo.must("switch", "-q", "-c", "one-more", "main")
	if out := repo.commitAndPush(resolve, "One more", "one-more"); !strings.Contains(out, "one-more is kept, not deployed") || !strings.Contains(out, "10") {
		t.Errorf("the eleventh: %s", out)
	}
	repo.must("switch", "-q", "-c", "bots-update", "main")
	if out := repo.commitAndPush(resolve, "Bots", "bots-update"); !strings.Contains(out, "bots-update is kept, not deployed") {
		t.Errorf("a branch left out: %s", out)
	}
	if list, err := c.Previews(context.Background()); err != nil || len(list) != 10 {
		t.Errorf("%d previews, %v", len(list), err)
	}
	// Going on with one that is there is no new preview.
	repo.must("switch", "-q", "work-3")
	if out := repo.commitAndPush(resolve, "More", "work-3"); !strings.Contains(out, "✔ live") {
		t.Errorf("another push of a preview that is there: %s", out)
	}
}

// TestEnvironmentsByRole has each role do what it may, and no more.
func TestEnvironmentsByRole(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	admin := signIn(t, cfg, addrs)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deploySite(t, c, "docs")
	if _, err := c.CreateEnvironment(ctx, "docs", apitypes.NewEnvironment{Name: "staging"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateProject(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MoveApp(ctx, "docs", "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangeProject(ctx, "team", apitypes.ProjectChange{Everyone: new("none")}); err != nil {
		t.Fatal(err)
	}
	roleUser := func(email, role string) *consoleUser {
		u := addTeammate(t, cfg, addrs, admin, email)
		if _, err := c.SetMember(ctx, "team", email, role); err != nil {
			t.Fatal(err)
		}
		return u
	}
	viewer, deployer, maintainer := roleUser("viewer@example.com", "viewer"), roleUser("deployer@example.com", "deployer"), roleUser("maintainer@example.com", "maintainer")

	_, page := viewer.get("/apps/docs/environments")
	contains(t, "what a viewer sees", page, "staging")
	for _, hidden := range []string{"Make an environment", ">Promote<", ">Lock<", ">Remove<"} {
		if strings.Contains(page, hidden) {
			t.Errorf("a viewer sees %q", hidden)
		}
	}
	if res, _ := viewer.post("/apps/docs/environments/staging/lock", url.Values{"csrf": {viewer.token()}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer locking: %d", res.StatusCode)
	}

	_, page = deployer.get("/apps/docs/environments")
	contains(t, "what a deployer sees", page, ">Lock<", "Make an environment")
	if strings.Contains(page, ">Remove<") {
		t.Errorf("a deployer can remove an environment")
	}
	if res, _ := deployer.post("/apps/docs/environments/staging/lock", url.Values{"csrf": {deployer.token()}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a deployer locking: %d", res.StatusCode)
	}
	if res, _ := deployer.post("/apps/docs/environments/staging/delete", url.Values{"csrf": {deployer.token()}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a deployer removing: %d", res.StatusCode)
	}

	// A preview goes with its branch, so that a deployer may destroy it too.
	repo, resolve := pushRepo(t, cfg, addrs, "docs", map[string]string{"index.html": "<p>hello</p>\n", "moorage.toml": "[app]\nruntime = \"static\"\n"})
	repo.must(append(append([]string{}, resolve...), "push", "moorage", "main")...)
	repo.must("switch", "-q", "-c", "a-branch")
	if out := repo.must(append(append([]string{}, resolve...), "push", "moorage", "a-branch")...); !strings.Contains(out, "✔ live") {
		t.Fatal(out)
	}
	_, page = deployer.get("/previews")
	contains(t, "a deployer's previews", page, "a-branch", ">Destroy<")
	_, page = viewer.get("/previews")
	contains(t, "a viewer's previews", page, "a-branch")
	if strings.Contains(page, ">Destroy<") {
		t.Errorf("a viewer sees Destroy")
	}
	if res, _ := viewer.post("/apps/docs/environments/a-branch/delete", url.Values{"csrf": {viewer.token()}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer destroying: %d", res.StatusCode)
	}
	if res, _ := deployer.post("/apps/docs/environments/a-branch/delete", url.Values{"csrf": {deployer.token()}, "from": {"previews"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a deployer destroying a preview: %d", res.StatusCode)
	}

	_, page = maintainer.get("/apps/docs/environments")
	contains(t, "what a maintainer sees", page, ">Remove<", ">Unlock<", "Locked by deployer@example.com")
	if res, _ := maintainer.post("/apps/docs/environments/staging/delete", url.Values{"csrf": {maintainer.token()}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a maintainer removing: %d", res.StatusCode)
	}
}

// TestDeclaredEnvironments makes what a manifest names, ready for its branch.
func TestDeclaredEnvironments(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	manifest := "[app]\nruntime = \"static\"\n\n[environments.qa]\nbranch = \"testing\"\n"
	repo, resolve := pushRepo(t, cfg, addrs, "docs", map[string]string{"index.html": "<p>hello</p>\n", "moorage.toml": manifest})
	repo.must(append(append([]string{}, resolve...), "push", "moorage", "main")...)
	e, err := c.Environment(ctx, "docs", "qa")
	if err != nil || e.Kind != "named" || e.Branch != "testing" || e.Release != nil {
		t.Errorf("what the manifest named: %+v %v", e, err)
	}
	if res, _ := get(t, addrs.HTTP, "docs--qa.localhost", "/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("before it has a release: %d", res.StatusCode)
	}
	_, page := b.get("/apps/docs/environments")
	contains(t, "the environments page", page, "qa", "Not deployed", "branch testing")
	res, page := b.get("/apps/docs?env=qa")
	if res.StatusCode != http.StatusOK {
		t.Errorf("its page: %d", res.StatusCode)
	}
	contains(t, "its page", page, "docs · qa", "pushes to <code>testing</code> deploy here")
	_, page = b.get("/activity")
	contains(t, "the activity", page, "docs · qa", `href="/apps/docs?env=qa"`)
	_, page = b.get("/")
	contains(t, "the list of apps", page, "+1 env")

	repo.must("switch", "-q", "-c", "testing")
	if out := repo.commitAndPush(resolve, "To test", "testing"); !strings.Contains(out, "docs · qa") || !strings.Contains(out, "✔ live") {
		t.Errorf("the branch's push: %s", out)
	}
	if res, _ := get(t, addrs.HTTP, "docs--qa.localhost", "/"); res.StatusCode != http.StatusOK {
		t.Errorf("after its push: %d", res.StatusCode)
	}
}
