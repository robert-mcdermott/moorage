package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/testroles"
)

// sleepyManifest is an app that sleeps after a moment of nothing, with a worker that stays up.
func sleepyManifest(start string) map[string]string {
	return map[string]string{"moorage.toml": `[app]
runtime = "command"

[processes.web]
command = ` + tomlString(testroles.AppCommand()) + `
health = { path = "/health", interval = "100ms", startup = "30s" }

[processes.worker]
command = ` + tomlString(testroles.AppCommand()) + `
sleep = false

[env]
TESTAPP_BODY = "good morning"
TESTAPP_START_DELAY = "` + start + `"

[environments.production]
sleep_after = "1s"

[deploy]
drain = "1s"
`}
}

func deployedSource(t *testing.T, c *client.Client, app string, files map[string]string) apitypes.Deployment {
	t.Helper()
	d, log := deploySource(t, c, app, files)
	if d.Status != apitypes.StatusLive {
		t.Fatalf("the deployment: %+v\n%s", d, log)
	}
	return d
}

func asleep(t *testing.T, c *client.Client, app string) bool {
	t.Helper()
	e, err := c.Environment(context.Background(), app, "production")
	if err != nil {
		t.Fatal(err)
	}
	return e.Asleep
}

func processes(ins []apitypes.Instance) string {
	var list []string
	for _, in := range ins {
		list = append(list, in.Process)
	}
	return strings.Join(list, " ")
}

func getWith(t *testing.T, addr string, host, path, accept string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	req.Host = host
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res, body.String()
}

// TestSleepAndWake leaves an app idle until it sleeps, and wakes it in each way.
func TestSleepAndWake(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", sleepyManifest("0s"))

	// Idle, it goes to sleep, and the process that said not to stays up.
	waitUntil(t, "the app to go to sleep", func() bool { return asleep(t, c, "shop") })
	waitUntil(t, "its web process to stop", func() bool { return processes(instances(t, c, "shop")) == "worker" })
	worker := instances(t, c, "shop")[0].PID
	_, page := b.get("/apps/shop")
	contains(t, "its page", page, "Sleeping", "It is asleep", ">Wake<", "worker.1")
	_, page = b.get("/")
	contains(t, "the list of apps", page, "Sleeping")
	_, page = b.get("/apps/shop/environments")
	contains(t, "its environments", page, "Sleeping", ">Wake<")
	_, page = b.get("/activity")
	contains(t, "what happened", page, "production went to sleep: nothing was using it")

	// A request wakes it, waits for it, and gets its answer.
	begun := time.Now()
	if res, body := get(t, addrs.HTTP, "shop.localhost", "/"); res.StatusCode != http.StatusOK || body != "good morning" {
		t.Errorf("the request that woke it: %d %q", res.StatusCode, body)
	}
	t.Logf("it woke in %v", time.Since(begun))
	if asleep(t, c, "shop") {
		t.Errorf("it is still asleep")
	}
	if ins := instances(t, c, "shop"); len(ins) != 2 {
		t.Errorf("its instances: %+v", ins)
	}
	for _, in := range instances(t, c, "shop") {
		if in.Process == "worker" && in.PID != worker {
			t.Errorf("the worker was restarted: %d, then %d", worker, in.PID)
		}
	}
	events, err := c.Events(ctx, "shop", "production", 5)
	if err != nil || !strings.Contains(typesOf(events), "environment.woke") {
		t.Errorf("the event of its waking: %s %v", typesOf(events), err)
	}

	// While a long request is going on it stays awake, however idle it looks.
	done := make(chan string, 1)
	go func() {
		_, body := get(t, addrs.HTTP, "shop.localhost", "/slow?ms=3000")
		done <- body
	}()
	time.Sleep(2500 * time.Millisecond)
	if asleep(t, c, "shop") {
		t.Errorf("it went to sleep in the middle of a request")
	}
	<-done
	waitUntil(t, "the app to go to sleep afterwards", func() bool { return asleep(t, c, "shop") })

	// It stays asleep across a change of the server, and a deployment wakes it.
	// With so little idle time, it may be back asleep by the next look on a slow machine, so what waking it answers is what counts.
	if woken, err := c.WakeEnvironment(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	} else if woken.Asleep {
		t.Errorf("after moor wake: %+v", woken)
	}
	waitUntil(t, "the app to go to sleep once more", func() bool { return asleep(t, c, "shop") })
	deployedSource(t, c, "shop", sleepyManifest("0s"))
	if len(instances(t, c, "shop")) != 2 {
		t.Errorf("after a deployment: %+v", instances(t, c, "shop"))
	}
}

// TestWakingUp gives clients what each of them can use when an app is slow to wake.
func TestWakingUp(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	token := b.token()
	ctx := context.Background()
	files := sleepyManifest("3s")
	files["moorage.toml"] = strings.Replace(files["moorage.toml"], "sleep_after = \"1s\"", "sleep_after = \"off\"", 1)
	deployedSource(t, c, "slow", files)

	if res, body := b.post("/apps/slow/sleep", url.Values{"csrf": {token}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("putting it to sleep: %d\n%s", res.StatusCode, body)
	}
	if !asleep(t, c, "slow") {
		t.Fatalf("it is not asleep")
	}

	// A browser is told what is happening, by a page that reloads itself.
	res, body := getWith(t, addrs.HTTP.String(), "slow.localhost", "/", "text/html,application/xhtml+xml")
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Errorf("a browser: %d %v", res.StatusCode, res.Header)
	}
	contains(t, "the page", body, "is waking up", `http-equiv="refresh"`)

	// Anything else waits, so that a client that cannot read a page gets its answer.
	res, body = getWith(t, addrs.HTTP.String(), "slow.localhost", "/", "application/json")
	if res.StatusCode != http.StatusOK || body != "good morning" {
		t.Errorf("an API client: %d %q", res.StatusCode, body)
	}
	if res, body := getWith(t, addrs.HTTP.String(), "slow.localhost", "/", "text/html"); res.StatusCode != http.StatusOK || body != "good morning" {
		t.Errorf("the browser's reload, once it is ready: %d %q", res.StatusCode, body)
	}

	// The console's buttons, and what cannot sleep.
	if res, _ := b.post("/apps/slow/sleep", url.Values{"csrf": {token}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("sleep: %d", res.StatusCode)
	}
	if res, body := b.post("/apps/slow/wake", url.Values{"csrf": {token}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("wake: %d\n%s", res.StatusCode, body)
	}
	if ins := instances(t, c, "slow"); len(ins) != 2 || asleep(t, c, "slow") {
		t.Errorf("after waking it: %+v", ins)
	}
	deploySite(t, c, "site")
	if _, err := c.SleepEnvironment(ctx, "site", "production"); status(err) != http.StatusConflict {
		t.Errorf("a static site: %v", err)
	}
	if _, err := c.SleepEnvironment(ctx, "nothing", "production"); status(err) == 0 {
		t.Errorf("an app that isn't there: %v", err)
	}
}

// TestSleepSurvivesARestart keeps an environment asleep when the server starts again.
func TestSleepSurvivesARestart(t *testing.T) {
	cfg := testConfig(t)
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	files := sleepyManifest("0s")
	files["moorage.toml"] = strings.Replace(files["moorage.toml"], "sleep_after = \"1s\"", "sleep_after = \"off\"", 1)
	deployedSource(t, c, "shop", files)
	if _, err := c.SleepEnvironment(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	addrs, _ = start(t, cfg)
	c = apiClient(t, cfg, addrs)
	if !asleep(t, c, "shop") {
		t.Fatalf("it woke with the server")
	}
	time.Sleep(600 * time.Millisecond) // some ticks of what starts what is missing
	if got := processes(instances(t, c, "shop")); got != "worker" {
		t.Errorf("after the restart it runs: %q", got)
	}
	if res, body := get(t, addrs.HTTP, "shop.localhost", "/"); res.StatusCode != http.StatusOK || body != "good morning" {
		t.Errorf("the request that woke it: %d %q", res.StatusCode, body)
	}
}
