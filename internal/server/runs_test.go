package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/testroles"
)

func appManifest(more string) map[string]string {
	return map[string]string{"moorage.toml": `[app]
runtime = "command"

[processes.web]
command = ` + tomlString(testroles.AppCommand()) + `
health = { path = "/health", interval = "100ms", startup = "30s" }

[processes.worker]
command = ` + tomlString(testroles.AppCommand()) + `

[env]
TESTAPP_BODY = "here"

[deploy]
drain = "1s"
` + more}
}

func followedRun(t *testing.T, c *client.Client, app, id string) (apitypes.Run, string, string) {
	t.Helper()
	var out, errs strings.Builder
	run, err := c.FollowRun(context.Background(), app, id, func(l apitypes.LogLine) error {
		if l.Stream == "E" {
			errs.WriteString(l.Text + "\n")
		} else {
			out.WriteString(l.Text + "\n")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return run, out.String(), errs.String()
}

// TestRunningACommand runs commands in the current release, and shows what they get to know.
func TestRunningACommand(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	if _, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "echo hi"}); status(err) == 0 {
		t.Errorf("a command for an app that is not there: %v", err)
	}
	deployedSource(t, c, "shop", appManifest(""))
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "API_KEY", Value: "a-long-secret-value", Secret: true}, {Name: "PLAIN", Value: "a plain value"}}}); err != nil {
		t.Fatal(err)
	}

	// It has the release's variables and data, and its output is the app's to keep from view.
	started, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: `echo "$PLAIN"; echo "$MOORAGE_PROCESS"; echo "$API_KEY"; echo to-stderr 1>&2; echo made > "$MOORAGE_DATA_DIR/from-a-run"`})
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != "running" || started.CreatedBy == "" || started.Version != 2 {
		t.Errorf("as it started: %+v", started)
	}
	run, out, errs := followedRun(t, c, "shop", started.ID)
	if run.Status != "succeeded" || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Errorf("how it ended: %+v", run)
	}
	contains(t, "what it said", out, "a plain value", "run")
	contains(t, "what it said on stderr", errs, "to-stderr")
	if strings.Contains(out, "a-long-secret-value") {
		t.Errorf("a secret in what it said: %s", out)
	}
	if !exists(filepath.Join(cfg.DataDir, "data", "shop", "production", "from-a-run")) {
		t.Errorf("it did not get the data directory")
	}

	// A command that fails says how.
	failing, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if run, _, _ := followedRun(t, c, "shop", failing.ID); run.Status != "failed" || run.ExitCode == nil || *run.ExitCode != 3 || !strings.Contains(run.Error, "3") {
		t.Errorf("a failure: %+v", run)
	}

	// What is not sensible is refused.
	if _, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "  "}); status(err) != http.StatusConflict {
		t.Errorf("no command: %v", err)
	}
	if _, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "echo", Timeout: "soon"}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("a bad timeout: %v", err)
	}
	if _, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "echo", Timeout: "9d"}); status(err) != http.StatusConflict {
		t.Errorf("too long a timeout: %v", err)
	}
	deploySite(t, c, "site")
	if _, err := c.StartRun(ctx, "site", "production", apitypes.NewRun{Command: "echo"}); status(err) != http.StatusConflict {
		t.Errorf("a static site: %v", err)
	}

	// Stopping one, and a time limit.
	if runtime.GOOS != "windows" {
		long, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "sleep 30"})
		if err != nil {
			t.Fatal(err)
		}
		begun := time.Now()
		stopped, err := c.StopRun(ctx, "shop", long.ID)
		if err != nil || stopped.Status != "canceled" || time.Since(begun) > 15*time.Second {
			t.Errorf("stopping one: %+v %v, in %v", stopped, err, time.Since(begun))
		}
		short, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "sleep 30", Timeout: "1s"})
		if err != nil {
			t.Fatal(err)
		}
		if run, _, _ := followedRun(t, c, "shop", short.ID); run.Status != "failed" || time.Since(begun) > 20*time.Second || !strings.Contains(run.Error, "ran out of time") || run.ExitCode != nil {
			t.Errorf("past its limit: %+v", run)
		}
	}

	// The console has them, and starts them.
	token := b.token()
	res, _ := b.post("/apps/shop/runs", url.Values{"csrf": {token}, "command": {`echo from the console`}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a command from the console: %d", res.StatusCode)
	}
	runPath := res.Header.Get("Location")
	waitUntil(t, "the command to end", func() bool {
		_, page := b.get(runPath)
		return strings.Contains(page, "Succeeded")
	})
	_, page := b.get(runPath)
	contains(t, "its page", page, "from the console", "echo from the console", "exit status 0", "owner@example.com")
	_, page = b.get("/apps/shop/cron")
	contains(t, "the list", page, "A command", "Succeeded", "Failed", "No cron jobs here", "Run a command")
	if res, _ := b.post("/apps/shop/runs", url.Values{"csrf": {token}, "command": {" "}}); res.StatusCode != http.StatusConflict {
		t.Errorf("no command, from the console: %d", res.StatusCode)
	}
	if res, _ := b.get("/apps/shop/runs/run_nothing"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a run that is not there: %d", res.StatusCode)
	}

	// A run belongs to its app: it is not there by any other.
	if _, err := c.Run(ctx, "site", started.ID); status(err) != http.StatusNotFound {
		t.Errorf("a run by another app: %v", err)
	}

	// A viewer sees, and does not run.
	if _, err := c.CreateProject(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MoveApp(ctx, "shop", "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangeProject(ctx, "team", apitypes.ProjectChange{Everyone: new("none")}); err != nil {
		t.Fatal(err)
	}
	viewer := addTeammate(t, cfg, addrs, b, "viewer@example.com")
	if _, err := c.SetMember(ctx, "team", "viewer@example.com", "viewer"); err != nil {
		t.Fatal(err)
	}
	_, page = viewer.get("/apps/shop/cron")
	if strings.Contains(page, "Run a command") {
		t.Errorf("a viewer is offered to run a command")
	}
	if res, _ := viewer.post("/apps/shop/runs", url.Values{"csrf": {viewer.token()}, "command": {"echo no"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer running a command: %d", res.StatusCode)
	}
}

// TestCronJobs lets jobs run by themselves, and shows what comes of it.
func TestCronJobs(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(`
[[cron]]
name = "ticker"
schedule = "@every 1s"
command = 'echo tick >> "$MOORAGE_DATA_DIR/ticks"; echo "$MOORAGE_PROCESS"'

[[cron]]
name = "broken"
schedule = "@every 1s"
command = "exit 7"

[[cron]]
name = "later"
schedule = "0 3 * * *"
timezone = "Europe/Paris"
command = "echo at three"

[[cron]]
name = "elsewhere"
schedule = "@every 1s"
command = "echo not here"
environments = ["staging"]
`))
	jobs, err := c.CronJobs(ctx, "shop", "production")
	if err != nil || len(jobs) != 3 {
		t.Fatalf("the jobs: %+v %v", jobs, err)
	}
	for _, j := range jobs {
		if j.Name == "later" && (j.Timezone != "Europe/Paris" || j.Next.IsZero() || j.Next.In(time.FixedZone("x", 0)).Minute() != 0) {
			t.Errorf("a job with a clock: %+v", j)
		}
	}

	// They run by themselves, as what they are, with what they said kept.
	waitUntil(t, "a job to run", func() bool {
		runs, err := c.Runs(ctx, "shop", "production", "ticker")
		return err == nil && len(runs) >= 2 && runs[1].Status == "succeeded"
	})
	data, err := os.ReadFile(filepath.Join(cfg.DataDir, "data", "shop", "production", "ticks"))
	if err != nil || !strings.Contains(string(data), "tick") {
		t.Errorf("what it did: %q %v", data, err)
	}
	runs, _ := c.Runs(ctx, "shop", "production", "ticker")
	last := runs[1]
	if last.Trigger != "schedule" || last.CreatedBy != "" || last.Job != "ticker" {
		t.Errorf("a run by the schedule: %+v", last)
	}
	if _, out, _ := followedRun(t, c, "shop", last.ID); !strings.Contains(out, "cron-ticker") {
		t.Errorf("what it was told it is: %q", out)
	}

	// One that fails says so, in what happened.
	waitUntil(t, "a job to fail", func() bool {
		runs, err := c.Runs(ctx, "shop", "production", "broken")
		return err == nil && len(runs) > 0 && runs[len(runs)-1].Status == "failed" && runs[len(runs)-1].ExitCode != nil && *runs[len(runs)-1].ExitCode == 7
	})
	waitUntil(t, "the event of its failing", func() bool {
		events, err := c.Events(ctx, "shop", "production", 30)
		if err != nil {
			return false
		}
		for _, e := range events {
			if e.Type == "cron.failed" && strings.Contains(e.Summary, "broken") && strings.Contains(e.Summary, "7") {
				return true
			}
		}
		return false
	})

	// One for another environment does not run here.
	if runs, _ := c.Runs(ctx, "shop", "production", "elsewhere"); len(runs) != 0 {
		t.Errorf("a job of another environment: %+v", runs)
	}

	// Anyone can run one now, whatever its schedule says, and it is marked as theirs.
	run, err := c.RunCronJob(ctx, "shop", "production", "later")
	if err != nil {
		t.Fatal(err)
	}
	if got, out, _ := followedRun(t, c, "shop", run.ID); got.Status != "succeeded" || got.Trigger != "manual" || got.CreatedBy == "" || !strings.Contains(out, "at three") {
		t.Errorf("a job run by hand: %+v %q", got, out)
	}
	if _, err := c.RunCronJob(ctx, "shop", "production", "nothing"); status(err) != http.StatusNotFound {
		t.Errorf("a job that is not there: %v", err)
	}

	// The console shows them, and does the same.
	_, page := b.get("/apps/shop/cron")
	contains(t, "the cron page", page, "ticker", "broken", "later", "0 3 * * *", "Europe/Paris", "Run now", "Succeeded", "Failed", "The schedule")
	if strings.Contains(page, "elsewhere") {
		t.Errorf("a job of another environment is shown")
	}
	res, _ := b.post("/apps/shop/cron/later/run", url.Values{"csrf": {b.token()}})
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "/runs/run_") {
		t.Errorf("Run now: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// A new release without them takes them away, and what is going on is left to end.
	deployedSource(t, c, "shop", appManifest(""))
	if jobs, err := c.CronJobs(ctx, "shop", "production"); err != nil || len(jobs) != 0 {
		t.Errorf("after a release without them: %+v %v", jobs, err)
	}
	waitUntil(t, "the last runs to end", func() bool {
		runs, err := c.Runs(ctx, "shop", "production", "")
		if err != nil {
			return false
		}
		for _, r := range runs {
			if r.Status == "running" {
				return false
			}
		}
		return true
	})
	settled, _ := c.Runs(ctx, "shop", "production", "ticker")
	time.Sleep(1500 * time.Millisecond)
	if again, _ := c.Runs(ctx, "shop", "production", "ticker"); len(again) != len(settled) {
		t.Errorf("a job of an earlier release ran: %d, then %d", len(settled), len(again))
	}

	// Deleting the app takes its runs along.
	if err := c.DeleteApp(ctx, "shop", false); err != nil {
		t.Fatal(err)
	}
}

// TestCronOverlap does as a job asks when the run before it is still going.
func TestCronOverlap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(`
[[cron]]
name = "skips"
schedule = "@every 1s"
command = "sleep 4"

[[cron]]
name = "replaces"
schedule = "@every 1s"
overlap = "replace"
command = "sleep 4"

[[cron]]
name = "allows"
schedule = "@every 1s"
overlap = "allow"
command = "sleep 4"
`))
	count := func(job, want string) int {
		runs, err := c.Runs(ctx, "shop", "production", job)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range runs {
			if r.Status == want {
				n++
			}
		}
		return n
	}
	waitUntil(t, "a run to be skipped", func() bool { return count("skips", "skipped") >= 1 })
	if n := count("skips", "running"); n != 1 {
		t.Errorf("skipping: %d going at once", n)
	}
	waitUntil(t, "a run to be replaced", func() bool { return count("replaces", "canceled") >= 1 })
	if n := count("replaces", "running"); n > 1 {
		t.Errorf("replacing: %d going at once", n)
	}
	waitUntil(t, "runs to go together", func() bool { return count("allows", "running") >= 2 })
}

// TestRestartOneProcess replaces one process, and leaves the others be.
func TestRestartOneProcess(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	pids := func() map[string]int {
		out := map[string]int{}
		for _, in := range instances(t, c, "shop") {
			out[in.Process] = in.PID
		}
		return out
	}
	before := pids()
	if _, err := c.RestartProcess(ctx, "shop", "production", "worker"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the old worker to go", func() bool { return len(instances(t, c, "shop")) == 2 })
	after := pids()
	if after["worker"] == before["worker"] || after["web"] != before["web"] {
		t.Errorf("before %v, after %v", before, after)
	}
	if res, body := get(t, addrs.HTTP, "shop.localhost", "/"); res.StatusCode != http.StatusOK || body != "here" {
		t.Errorf("the app still serves: %d %q", res.StatusCode, body)
	}
	if _, err := c.RestartProcess(ctx, "shop", "production", "web"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the old web process to go", func() bool { return len(instances(t, c, "shop")) == 2 })
	again := pids()
	if again["web"] == after["web"] || again["worker"] != after["worker"] {
		t.Errorf("before %v, after %v", after, again)
	}
	if res, _ := get(t, addrs.HTTP, "shop.localhost", "/"); res.StatusCode != http.StatusOK {
		t.Errorf("the app serves after its web process: %d", res.StatusCode)
	}
	if _, err := c.RestartProcess(ctx, "shop", "production", "nothing"); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("a process that is not there: %v", err)
	}
	_, page := b.get("/apps/shop")
	contains(t, "its page", page, "Restart only")
	before = pids()
	if res, _ := b.post("/apps/shop/restart", url.Values{"csrf": {b.token()}, "process": {"worker"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("from the console: %d", res.StatusCode)
	}
	waitUntil(t, "the old worker to go", func() bool { return len(instances(t, c, "shop")) == 2 })
	if after := pids(); after["worker"] == before["worker"] || after["web"] != before["web"] {
		t.Errorf("from the console, before %v, after %v", before, after)
	}
}

// TestDeclaredVariables fails early for what an app needs, and makes what it asks to have made.
func TestDeclaredVariables(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	declared := appManifest(`
[vars.PAYMENTS_KEY]
required = true
secret = true
description = "Sandbox key for the payments provider"

[vars.SESSION_KEY]
generate = "secret"

[vars.INSTALL_ID]
generate = "uuid"

[vars.FROM_THE_MANIFEST]
required = true

[vars.OPTIONAL_THING]
description = "Only if you want it"

[env]
FROM_THE_MANIFEST = "here"
`)
	declared["moorage.toml"] = strings.Replace(declared["moorage.toml"], "[env]\nTESTAPP_BODY = \"here\"\n", "", 1)
	declared["moorage.toml"] = strings.Replace(declared["moorage.toml"], "FROM_THE_MANIFEST = \"here\"", "FROM_THE_MANIFEST = \"here\"\nTESTAPP_BODY = \"here\"", 1)

	d, log := deploySource(t, c, "shop", declared)
	if d.Status != apitypes.StatusFailed || !strings.Contains(d.Error, "PAYMENTS_KEY") || strings.Contains(d.Error, "FROM_THE_MANIFEST") {
		t.Fatalf("with one missing: %+v\n%s", d, log)
	}
	vars, err := c.Vars(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	_ = vars

	// A secret must be one.
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "PAYMENTS_KEY", Value: "in the clear"}}, NoRestart: true}); err != nil {
		t.Fatal(err)
	}
	if d, log := deploySource(t, c, "shop", declared); d.Status != apitypes.StatusFailed || !strings.Contains(d.Error, "secret") {
		t.Fatalf("not a secret: %+v\n%s", d, log)
	}
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "PAYMENTS_KEY", Value: "a-sealed-value", Secret: true}}, NoRestart: true}); err != nil {
		t.Fatal(err)
	}
	d = deployedSource(t, c, "shop", declared)

	// What it asked to have made is there, and stays the same.
	session := func(host string) string {
		t.Helper()
		_, body := get(t, addrs.HTTP, host, "/env?name=SESSION_KEY")
		return body
	}
	first := session("shop.localhost")
	if len(first) < 40 {
		t.Errorf("a generated secret: %q", first)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/env?name=INSTALL_ID"); len(body) != 36 || strings.Count(body, "-") != 4 {
		t.Errorf("a generated uuid: %q", body)
	}
	deployedSource(t, c, "shop", declared)
	if again := session("shop.localhost"); again != first {
		t.Errorf("a generated secret changed: %q, then %q", first, again)
	}
	vars, err = c.Vars(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]apitypes.Var{}
	for _, v := range vars {
		seen[v.Name+"/"+v.Source] = v
	}
	if v := seen["SESSION_KEY/env"]; !v.Secret || v.Value != "" || !v.Declared {
		t.Errorf("a generated secret, listed: %+v", v)
	}
	if v := seen["OPTIONAL_THING/declared"]; !v.Declared || v.Missing || v.Description != "Only if you want it" {
		t.Errorf("one that is not required: %+v", v)
	}

	// Another environment gets its own, and needs its own required ones.
	deployed, _, err := c.DeploySource(ctx, "shop", "staging", writeSite(t, declared), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := followed(t, c, deployed.ID); got.Status != apitypes.StatusFailed || !strings.Contains(got.Error, "PAYMENTS_KEY") {
		t.Errorf("another environment without what it needs: %+v", got)
	}
	if _, err := c.ChangeVars(ctx, "shop", "staging", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "PAYMENTS_KEY", Value: "another-sealed-value", Secret: true}}, NoRestart: true}); err != nil {
		t.Fatal(err)
	}
	deployed, _, err = c.DeploySource(ctx, "shop", "staging", writeSite(t, declared), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := followed(t, c, deployed.ID); got.Status != apitypes.StatusLive {
		t.Fatalf("another environment: %+v", got)
	}
	if other := session("shop--staging.localhost"); other == first || len(other) < 40 {
		t.Errorf("another environment's: %q", other)
	}

	// Rolling back does not stop to ask.
	if _, err := c.ChangeVars(ctx, "shop", "production", apitypes.VarsChange{Unset: []string{"PAYMENTS_KEY"}, NoRestart: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rollback(ctx, "shop", "production", 0); err != nil {
		t.Errorf("a rollback with a required variable gone: %v", err)
	}

	// The console has a checklist of them.
	_, page := b.get("/apps/shop/variables")
	contains(t, "the page", page, "PAYMENTS_KEY", "Missing", "Sandbox key for the payments provider", "OPTIONAL_THING", "Only if you want it")
}

// TestWatchPaths deploys only what a manifest says matters.
func TestWatchPaths(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	repo, resolve := pushRepo(t, cfg, addrs, "docs", map[string]string{
		"index.html": "<p>one</p>\n", "notes/a.md": "notes\n", "moorage.toml": "[app]\nruntime = \"static\"\nwatch = [\"*.html\", \"site/*\"]\n",
	})
	if out := repo.must(append(append([]string{}, resolve...), "push", "moorage", "main")...); !strings.Contains(out, "✔ live") {
		t.Fatalf("the first push always deploys: %s", out)
	}
	count := func() int {
		deps, err := c.Deployments(ctx, "docs", "production", 20)
		if err != nil {
			t.Fatal(err)
		}
		return len(deps)
	}
	repo.write("notes/a.md", "more notes\n")
	if out := repo.commitAndPush(resolve, "Notes", "main"); !strings.Contains(out, "kept, not deployed") || !strings.Contains(out, "watch") {
		t.Errorf("a push of what does not matter: %s", out)
	}
	if n := count(); n != 1 {
		t.Errorf("%d deployments after that", n)
	}
	repo.write("index.html", "<p>two</p>\n")
	if out := repo.commitAndPush(resolve, "Page", "main"); !strings.Contains(out, "✔ live") {
		t.Errorf("a push of what matters: %s", out)
	}
	if err := os.MkdirAll(filepath.Join(repo.dir, "site", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.write("site/deep/b.css", "a {}\n")
	if out := repo.commitAndPush(resolve, "Styles", "main"); !strings.Contains(out, "✔ live") {
		t.Errorf("a push under a path that matters: %s", out)
	}
	// Its own manifest always matters.
	repo.write("moorage.toml", "[app]\nruntime = \"static\"\nwatch = [\"site/*\"]\n")
	if out := repo.commitAndPush(resolve, "Watch less", "main"); !strings.Contains(out, "✔ live") {
		t.Errorf("a push of the manifest: %s", out)
	}
	if n := count(); n != 4 {
		t.Errorf("%d deployments in the end", n)
	}
}

// TestCommandsRunAsTheirEnvironment: a command in staging has staging's variables and not
// production's, whether someone asked for it, a job started it or a deployment did.
func TestCommandsRunAsTheirEnvironment(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", map[string]string{"moorage.toml": `[app]
runtime = "command"

[processes.web]
command = ` + tomlString(testroles.AppCommand()) + `
health = { path = "/health", interval = "100ms", startup = "30s" }

[env]
WHICH = "base-value"

[environments.staging.env]
WHICH = "staging-value"

[release]
command = "echo released with $WHICH"

[deploy]
drain = "1s"
`})
	dep, err := c.Promote(ctx, "shop", "production", "staging")
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "staging to serve", func() bool {
		res, _ := get(t, addrs.HTTP, "shop--staging.localhost", "/health")
		return res.StatusCode == http.StatusOK
	})
	log, err := os.ReadFile(deploy.LogPath(cfg.DataDir, "shop", dep.ID))
	if err != nil || !strings.Contains(string(log), "released with staging-value") {
		t.Errorf("the release command: %v\n%s", err, log)
	}
	started, err := c.StartRun(ctx, "shop", "staging", apitypes.NewRun{Command: `echo "WHICH=$WHICH"`})
	if err != nil {
		t.Fatal(err)
	}
	if run, out, errs := followedRun(t, c, "shop", started.ID); run.Status != "succeeded" || !strings.Contains(out, "WHICH=staging-value") {
		t.Errorf("in staging: %+v %q %q", run, out, errs)
	}
	started, err = c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: `echo "WHICH=$WHICH"`})
	if err != nil {
		t.Fatal(err)
	}
	if run, out, errs := followedRun(t, c, "shop", started.ID); run.Status != "succeeded" || !strings.Contains(out, "WHICH=base-value") {
		t.Errorf("in production: %+v %q %q", run, out, errs)
	}
}

// TestCommandsHaveTheirOwnTemporaryDirectory: the first of two commands to end leaves the other's.
func TestCommandsHaveTheirOwnTemporaryDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	long, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: `echo kept > "$TMPDIR/mine"; sleep 2; cat "$TMPDIR/mine"`})
	if err != nil {
		t.Fatal(err)
	}
	short, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: `echo done`})
	if err != nil {
		t.Fatal(err)
	}
	followedRun(t, c, "shop", short.ID)
	if run, out, errs := followedRun(t, c, "shop", long.ID); run.Status != "succeeded" || !strings.Contains(out, "kept") {
		t.Errorf("%+v %q %q", run, out, errs)
	}
}

// TestCronTimeoutHasALimit says so when deploying, rather than the job never running.
func TestCronTimeoutHasALimit(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	d, log := deploySource(t, c, "shop", appManifest(`
[[cron]]
name = "weekly"
schedule = "@every 1h"
command = "echo ran"
timeout = "48h"
`))
	if d.Status != apitypes.StatusFailed || !strings.Contains(d.Error+log, "a day at the most") {
		t.Errorf("%+v\n%s", d, log)
	}
}

// TestStreamsEndWithAccess ends what someone is following when their token goes.
func TestStreamsEndWithAccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	long, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.StopRun(ctx, "shop", long.ID)
	mine := tokenClient(t, c, tokenOf(t, owner, "read"))
	ended := make(chan error, 1)
	go func() {
		_, err := mine.FollowRun(ctx, "shop", long.ID, func(apitypes.LogLine) error { return nil })
		ended <- err
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-ended:
		t.Fatalf("it ended by itself: %v", err)
	default:
	}
	manager := tokenClient(t, c, tokenOf(t, owner, "admin"))
	all, err := manager.Tokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range all {
		if tok.Name == "for read" {
			if _, err := manager.RevokeToken(ctx, tok.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("following went on with a revoked token")
	}
}

// TestMemoryLimitWithoutTheKernel restarts an instance that stays over its limit, where nothing in the
// kernel would, and says that it ran out of memory.
func TestMemoryLimitWithoutTheKernel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Job Object holds it to its memory there: see TestLimitsOnWindows")
	}
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(`
[resources]
memory = "1MiB"
cpu = 0.5
`))
	waitUntil(t, "it to be restarted for its memory", func() bool {
		list, err := c.Instances(ctx, "shop", "production")
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range list {
			if in.Restarts > 0 {
				return true
			}
		}
		return false
	})
	events, err := c.Events(ctx, "shop", "production", 100)
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, e := range events {
		if e.Type == "instance.exited" && strings.Contains(e.Summary, "out of memory") {
			said = true
		}
	}
	if !said {
		t.Errorf("nothing says that it ran out of memory: %+v", events)
	}
	env, err := c.Environment(ctx, "shop", "production")
	if err != nil || env.Isolation == nil || env.Isolation.Memory != 1<<20 || env.Isolation.CPU != 0.5 || env.Isolation.Enforced || env.Isolation.Level != "none" {
		t.Errorf("%+v %v", env.Isolation, err)
	}
	if _, page := b.get("/apps/shop"); !strings.Contains(page, "this server cannot enforce") {
		t.Errorf("the app's page does not say so:\n%s", page)
	}
}

// TestLimitsOnWindows has a Job Object hold an instance to what it says, and the page says so.
func TestLimitsOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Job Objects are Windows' own")
	}
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	deployedSource(t, c, "shop", appManifest(`
[resources]
memory = "256MiB"
cpu = 0.5
pids = 32
`))
	env, err := c.Environment(context.Background(), "shop", "production")
	if err != nil || env.Isolation == nil || !env.Isolation.Enforced || env.Isolation.Memory != 256<<20 {
		t.Errorf("%+v %v", env.Isolation, err)
	}
	if _, page := b.get("/apps/shop"); !strings.Contains(page, "is refused") {
		t.Errorf("the app's page does not say so:\n%s", page)
	}
}
