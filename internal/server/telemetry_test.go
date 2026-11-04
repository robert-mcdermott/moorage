package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// fakeCollector remembers what it is sent.
type fakeCollector struct {
	srv    *httptest.Server
	status atomic.Int32
	hits   atomic.Int32
	mu     sync.Mutex
	paths  []string
	auth   []string
	bodies [][]byte
}

func newFakeCollector(t *testing.T) *fakeCollector {
	f := &fakeCollector{}
	f.status.Store(http.StatusOK)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		status := int(f.status.Load())
		if status == http.StatusOK {
			f.mu.Lock()
			f.paths = append(f.paths, r.URL.Path)
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			f.bodies = append(f.bodies, body)
			f.mu.Unlock()
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCollector) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(bytes.Join(f.bodies, []byte("\n")))
}

func (f *fakeCollector) has(text string) bool { return strings.Contains(f.all(), text) }

func TestTelemetrySinks(t *testing.T) {
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs := startWith(t, cfg, func(o *Options) {
		o.TelemetryTick = 30 * time.Millisecond
		o.TelemetryMetricsEvery = 100 * time.Millisecond
		o.MetricsEvery = 50 * time.Millisecond
	})
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	deployedSource(t, c, "blog", appManifest(""))

	everything := newFakeCollector(t)
	if k, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "everything", Kind: "http", Address: everything.srv.URL + "/in", Logs: true}); err != nil || k.Kind != "http" || !k.Logs {
		t.Fatalf("%+v %v", k, err)
	}
	onlyBlog := newFakeCollector(t)
	if _, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "only-blog", Kind: "http", Address: onlyBlog.srv.URL, Logs: true, App: "blog"}); err != nil {
		t.Fatal(err)
	}
	otlp := newFakeCollector(t)
	k, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "otlp", Kind: "otlp", Address: otlp.srv.URL, Logs: true, Metrics: true, Headers: map[string]string{"Authorization": "Bearer a-sealed-value"}})
	if err != nil || !k.HasHeaders {
		t.Fatalf("%+v %v", k, err)
	}

	// Sinks start at about now: what was said before they were there is not what they asked for.
	list, err := c.TelemetrySinks(ctx)
	if err != nil || len(list) != 3 || list[0].Name != "everything" {
		t.Fatalf("%+v %v", list, err)
	}
	said, _ := json.Marshal(list)
	if strings.Contains(string(said), "a-sealed-value") {
		t.Error("a header came back")
	}

	// What a command says goes where it should.
	run, err := c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "echo said-by-shop"})
	if err != nil {
		t.Fatal(err)
	}
	followedRun(t, c, "shop", run.ID)
	run, err = c.StartRun(ctx, "blog", "production", apitypes.NewRun{Command: "echo said-by-blog"})
	if err != nil {
		t.Fatal(err)
	}
	followedRun(t, c, "blog", run.ID)
	waitUntil(t, "the line to arrive", func() bool { return everything.has("said-by-shop") && everything.has("said-by-blog") })
	waitUntil(t, "it to reach the one for that app", func() bool { return onlyBlog.has("said-by-blog") })
	if onlyBlog.has("said-by-shop") {
		t.Error("it had another app's lines")
	}
	waitUntil(t, "OTLP", func() bool { return otlp.has("said-by-shop") })
	if !everything.has(`"app":"shop"`) || !everything.has(`"env":"production"`) || !everything.has(`"process":"run"`) {
		t.Errorf("%s", everything.all())
	}
	otlp.mu.Lock()
	logged := false
	for i, path := range otlp.paths {
		logged = logged || (path == "/v1/logs" && otlp.auth[i] == "Bearer a-sealed-value")
	}
	otlp.mu.Unlock()
	if !logged {
		t.Error("no header on what reached the collector")
	}

	// Metrics go there too, and only there.
	waitUntil(t, "the metrics", func() bool { return otlp.has("resourceMetrics") })
	if everything.has("resourceMetrics") {
		t.Error("a sink for logs had metrics")
	}

	// One that will not have what it is sent is not held up by it, and says how many it let go.
	fussy := newFakeCollector(t)
	fussy.status.Store(http.StatusRequestEntityTooLarge)
	if _, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "fussy", Kind: "http", Address: fussy.srv.URL, Logs: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	run, err = c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "echo too-much"})
	if err != nil {
		t.Fatal(err)
	}
	followedRun(t, c, "shop", run.ID)
	waitUntil(t, "it to let it go", func() bool {
		list, err := c.TelemetrySinks(ctx)
		if err != nil {
			return false
		}
		for _, k := range list {
			if k.Name == "fussy" {
				return k.Dropped > 0 && strings.Contains(k.LastError, "413") && k.LastSentAt.IsZero()
			}
		}
		return false
	})
	if err := c.RemoveTelemetrySink(ctx, "fussy"); err != nil {
		t.Fatal(err)
	}

	// One that refuses everything is not sent everything, or there would be none of it left for when that has been put right.
	refusing := newFakeCollector(t)
	refusing.status.Store(http.StatusBadRequest)
	if _, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "refusing", Kind: "http", Address: refusing.srv.URL, Logs: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if runtime.GOOS != "windows" {
		run, err = c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "sh -c 'i=0; while [ $i -lt 40 ]; do echo refused-$i; i=$((i+1)); done'"})
		if err != nil {
			t.Fatal(err)
		}
		followedRun(t, c, "shop", run.ID)
		time.Sleep(time.Second)
		if n := refusing.hits.Load(); n > 20 {
			t.Errorf("it was sent %d requests in about a second", n)
		}
	}
	if err := c.RemoveTelemetrySink(ctx, "refusing"); err != nil {
		t.Fatal(err)
	}

	// Trying it says how that went.
	if err := c.TestTelemetrySink(ctx, "everything"); err != nil {
		t.Errorf("%v", err)
	}
	if !everything.has("A test from Moorage") {
		t.Errorf("%s", everything.all())
	}

	// A sink that is down says so, and gets what it missed when it is back.
	everything.status.Store(http.StatusServiceUnavailable)
	if err := c.TestTelemetrySink(ctx, "everything"); status(err) != http.StatusBadGateway {
		t.Errorf("%v", err)
	}
	run, err = c.StartRun(ctx, "shop", "production", apitypes.NewRun{Command: "echo while-it-was-down"})
	if err != nil {
		t.Fatal(err)
	}
	followedRun(t, c, "shop", run.ID)
	waitUntil(t, "it to say that it is failing", func() bool {
		list, err := c.TelemetrySinks(ctx)
		return err == nil && list[0].LastError != ""
	})
	if list, _ := c.TelemetrySinks(ctx); !strings.Contains(list[0].LastError, "503") || strings.Contains(list[0].LastError, everything.srv.URL) {
		t.Errorf("%+v", list[0])
	}
	if !otlp.has("while-it-was-down") {
		waitUntil(t, "the others to go on regardless", func() bool { return otlp.has("while-it-was-down") })
	}
	everything.status.Store(http.StatusOK)
	waitUntil(t, "it to catch up", func() bool { return everything.has("while-it-was-down") })
	waitUntil(t, "it to stop saying so", func() bool {
		list, err := c.TelemetrySinks(ctx)
		return err == nil && list[0].LastError == "" && !list[0].LastSentAt.IsZero()
	})

	// Headers stay when it is saved again, unless it is going somewhere else.
	if k, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "otlp", Kind: "otlp", Address: otlp.srv.URL, Logs: true}); err != nil || !k.HasHeaders || k.Metrics {
		t.Errorf("%+v %v", k, err)
	}
	if k, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "otlp", Kind: "otlp", Address: everything.srv.URL, Logs: true}); err != nil || k.HasHeaders {
		t.Errorf("a header went along to another address: %+v %v", k, err)
	}

	// What cannot be right is refused, and it is for an administrator.
	for _, bad := range []apitypes.NewTelemetrySink{
		{Name: "bad", Kind: "kafka", Address: "https://x", Logs: true},
		{Name: "bad", Kind: "http", Address: "https://x"},
		{Name: "bad", Kind: "http", Address: "https://x", Logs: true, App: "nothing"},
		{Name: "bad", Kind: "http", Address: "http://x.example.com", Logs: true, Headers: map[string]string{"A": "b"}},
		{Name: "bad", Kind: "syslog", Address: "x", Logs: true},
	} {
		if _, err := c.SetTelemetrySink(ctx, bad); status(err) != http.StatusUnprocessableEntity {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	owner := signIn(t, cfg, addrs)
	member := addTeammate(t, cfg, addrs, owner, "someone@example.com")
	if res, _ := member.get("/settings/telemetry"); res.StatusCode != http.StatusForbidden {
		t.Errorf("a member's page: %d", res.StatusCode)
	}
	if _, err := tokenClient(t, c, tokenOf(t, member, "deploy")).TelemetrySinks(ctx); status(err) != http.StatusForbidden {
		t.Errorf("a member: %v", err)
	}

	// What this server may not reach, it does not.
	if _, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "inside", Kind: "http", Address: "http://[::1]:9/", Logs: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.TestTelemetrySink(ctx, "inside"); status(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "may not reach") {
		t.Errorf("%v", err)
	}

	// The server's configuration has them, and none of what is sealed.
	exported, err := c.ServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kept apitypes.ConfigSink
	for _, k := range exported.Sinks {
		if k.Name == "only-blog" {
			kept = k
		}
	}
	if kept.App != "blog" || !kept.Logs || kept.Kind != "http" {
		t.Errorf("%+v", exported.Sinks)
	}
	report, err := c.ImportServerConfig(ctx, apitypes.ServerConfig{Format: exported.Format, Sinks: []apitypes.ConfigSink{
		kept,
		{Name: "from-a-file", Kind: "http", Address: everything.srv.URL, Logs: true, Headers: map[string]string{"X-Key": "a-value"}},
		{Name: "waiting", Kind: "http", Address: everything.srv.URL, Logs: true, Headers: map[string]string{"X-Key": "${MOORAGE_SINK_WAITING_X_KEY}"}},
		{Name: "wrong", Kind: "kafka", Address: everything.srv.URL, Logs: true},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, item := range report.Items {
		actions[item.Name] = item.Action
	}
	if actions["only-blog"] != apitypes.ConfigSame || actions["from-a-file"] != apitypes.ConfigCreate || actions["waiting"] != apitypes.ConfigSkip || actions["wrong"] != apitypes.ConfigFailed {
		t.Errorf("%+v", report.Items)
	}
	exported, err = c.ServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range exported.Sinks {
		if k.Name == "from-a-file" && k.Headers["X-Key"] != "${MOORAGE_SINK_FROM_A_FILE_X_KEY}" {
			t.Errorf("%+v", k)
		}
	}
	if err := c.RemoveTelemetrySink(ctx, "from-a-file"); err != nil {
		t.Fatal(err)
	}

	// Going, it says no more.
	if err := c.RemoveTelemetrySink(ctx, "only-blog"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveTelemetrySink(ctx, "only-blog"); status(err) != http.StatusNotFound {
		t.Errorf("%v", err)
	}
	time.Sleep(100 * time.Millisecond)
	run, err = c.StartRun(ctx, "blog", "production", apitypes.NewRun{Command: "echo after-it-went"})
	if err != nil {
		t.Fatal(err)
	}
	followedRun(t, c, "blog", run.ID)
	waitUntil(t, "the line to reach another", func() bool { return everything.has("after-it-went") })
	if onlyBlog.has("after-it-went") {
		t.Error("a sink that had gone was sent something")
	}
}

func TestTelemetryThroughARestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("what this runs uses sh")
	}
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "ticker", map[string]string{"moorage.toml": `[app]
runtime = "command"

[processes.worker]
command = "sh -c 'i=0; while true; do i=$((i+1)); echo tick-$i; sleep 0.1; done'"
`})
	sink := newFakeCollector(t)
	if _, err := c.SetTelemetrySink(ctx, apitypes.NewTelemetrySink{Name: "sink", Kind: "http", Address: sink.srv.URL, Logs: true}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the ticks to arrive", func() bool { return len(ticks(sink.all())) > 3 })
	// Give it a moment to note how far it has read, which it does as it stops.
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	seenBefore := ticks(sink.all())
	if len(seenBefore) == 0 {
		t.Fatal("nothing arrived")
	}
	last := seenBefore[len(seenBefore)-1]
	time.Sleep(time.Second)
	addrs, _ = start(t, cfg)
	c = apiClient(t, cfg, addrs)
	waitUntil(t, "the ticks to go on", func() bool { all := ticks(sink.all()); return all[len(all)-1] > last+15 })
	all := ticks(sink.all())
	seen := map[int]int{}
	for _, n := range all {
		seen[n]++
		if seen[n] > 1 {
			t.Errorf("tick %d arrived twice", n)
		}
	}
	missing := 0
	for n := all[0]; n <= all[len(all)-1]; n++ {
		if seen[n] == 0 {
			missing++
		}
	}
	// The app went on while the server was not there, and what it said then was in its log.
	if missing > 15 {
		t.Errorf("%d of %d ticks did not arrive", missing, len(all))
	}
}

var tickLine = regexp.MustCompile(`tick-(\d+)`)

func ticks(text string) []int {
	var out []int
	for _, m := range tickLine.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

func TestThresholds(t *testing.T) {
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs := startWith(t, cfg, func(o *Options) {
		o.ThresholdTick = 30 * time.Millisecond
		o.ThresholdMinute = 100 * time.Millisecond
		o.MetricsEvery = 20 * time.Millisecond
	})
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))

	// A webhook hears of it, as any other notification.
	var mu sync.Mutex
	var heard []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		heard = append(heard, string(body))
		mu.Unlock()
	}))
	defer hook.Close()
	res, body := owner.post("/settings/notifications/channels", url.Values{"csrf": {owner.token()}, "name": {"hook"}, "kind": {"webhook"}, "url": {hook.URL}})
	if res.StatusCode >= 400 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	channels, err := testAuth(t, cfg).DB.NotificationChannels(ctx)
	if err != nil || len(channels) != 1 {
		t.Fatalf("%v %v", channels, err)
	}
	if res, body := owner.post("/settings/notifications/rules", url.Values{"csrf": {owner.token()}, "channel": {channels[0].ID}, "events": {"threshold.crossed"}}); res.StatusCode >= 400 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}

	// Adding and listing.
	if _, err := c.AddThreshold(ctx, "shop", "production", apitypes.NewThreshold{Metric: "disk", Above: 90}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("%v", err)
	}
	if _, err := c.AddThreshold(ctx, "shop", "nothing", apitypes.NewThreshold{Metric: "cpu", Above: 90}); status(err) != http.StatusNotFound {
		t.Errorf("%v", err)
	}
	made, err := c.AddThreshold(ctx, "shop", "production", apitypes.NewThreshold{Metric: "latency-p95", Above: 0.0001, Minutes: 1})
	if err != nil || made.Metric != "latency-p95" || made.Label == "" {
		t.Fatalf("%+v %v", made, err)
	}
	if _, err := c.AddThreshold(ctx, "shop", "production", apitypes.NewThreshold{Metric: "latency-p95", Above: 0.0001, Minutes: 7}); status(err) != http.StatusConflict {
		t.Errorf("the same again: %v", err)
	}

	// Requests make it go over, for as long as it says.
	stopAsking := make(chan struct{})
	var asking sync.WaitGroup
	asking.Add(1)
	go func() {
		defer asking.Done()
		for {
			select {
			case <-stopAsking:
				return
			case <-time.After(10 * time.Millisecond):
				get(t, addrs.HTTP, cfg.EnvHost("shop", "production"), "/")
			}
		}
	}()
	waitUntil(t, "it to be crossed", func() bool {
		list, err := c.Thresholds(ctx, "shop", "production")
		return err == nil && len(list) == 1 && list[0].Firing && list[0].Value != nil
	})
	close(stopAsking)
	asking.Wait()
	waitUntil(t, "someone to be told", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, message := range heard {
			if strings.Contains(message, "The 95th percentile of latency has been above") {
				return true
			}
		}
		return false
	})

	// The page shows it, and can add and remove them.
	res, body = owner.get("/apps/shop/metrics")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "When to tell someone") || !strings.Contains(body, "Crossed") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	res, _ = owner.post("/apps/shop/metrics/thresholds", url.Values{"csrf": {owner.token()}, "metric": {"cpu"}, "above": {"150"}, "minutes": {"5"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	res, _ = owner.post("/apps/shop/metrics/thresholds", url.Values{"csrf": {owner.token()}, "metric": {"nothing"}, "above": {"150"}, "minutes": {"5"}})
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "threshold=") {
		t.Errorf("%d %v", res.StatusCode, res.Header)
	}
	list, err := c.Thresholds(ctx, "shop", "production")
	if err != nil || len(list) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	res, _ = owner.post("/apps/shop/metrics/thresholds/remove", url.Values{"csrf": {owner.token()}, "id": {list[1].ID}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if err := c.RemoveThreshold(ctx, "shop", "production", list[0].ID); err != nil {
		t.Errorf("%v", err)
	}
	if err := c.RemoveThreshold(ctx, "shop", "production", list[0].ID); status(err) != http.StatusNotFound {
		t.Errorf("%v", err)
	}

	// Who may: a deployer looks, and does not change it.
	member := addTeammate(t, cfg, addrs, owner, "someone@example.com")
	if _, err := c.SetMember(ctx, "default", "someone@example.com", store.RoleDeployer); err != nil {
		t.Fatal(err)
	}
	memberClient := tokenClient(t, c, tokenOf(t, member, "deploy"))
	if _, err := memberClient.Thresholds(ctx, "shop", "production"); err != nil {
		t.Errorf("%v", err)
	}
	if _, err := memberClient.AddThreshold(ctx, "shop", "production", apitypes.NewThreshold{Metric: "cpu", Above: 90}); status(err) != http.StatusForbidden {
		t.Errorf("%v", err)
	}
	if res, _ := member.post("/apps/shop/metrics/thresholds", url.Values{"csrf": {member.token()}, "metric": {"cpu"}, "above": {"90"}, "minutes": {"5"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}
}

func TestTelemetryInTheConsole(t *testing.T) {
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs, _ := start(t, cfg)
	owner := signIn(t, cfg, addrs)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	sink := newFakeCollector(t)

	res, body := owner.get("/settings/telemetry")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Nothing is being sent anywhere else") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = owner.post("/settings/telemetry", url.Values{"csrf": {owner.token()}, "name": {"bad name"}, "kind": {"http"}, "address": {sink.srv.URL}, "logs": {"1"}, "headers": {"Authorization: Bearer typed-by-someone"}})
	if res.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, "typed-by-someone") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	res, body = owner.post("/settings/telemetry", url.Values{"csrf": {owner.token()}, "name": {"central"}, "kind": {"http"}, "address": {sink.srv.URL}, "logs": {"1"}, "headers": {"Authorization: Bearer typed-by-someone"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = owner.get("/settings/telemetry")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "central") || !strings.Contains(body, "with headers") || strings.Contains(body, "typed-by-someone") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if list, err := c.TelemetrySinks(ctx); err != nil || len(list) != 1 || !list[0].HasHeaders {
		t.Errorf("%+v %v", list, err)
	}
	res, body = owner.post("/settings/telemetry/test", url.Values{"csrf": {owner.token()}, "name": {"central"}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "got what was sent") || !sink.has("A test from Moorage") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	sink.status.Store(http.StatusServiceUnavailable)
	res, body = owner.post("/settings/telemetry/test", url.Values{"csrf": {owner.token()}, "name": {"central"}})
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(body, "could not be reached") {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	if res, _ := owner.post("/settings/telemetry/remove", url.Values{"name": {"central"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", res.StatusCode)
	}
	if res, _ := owner.post("/settings/telemetry/remove", url.Values{"csrf": {owner.token()}, "name": {"central"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d", res.StatusCode)
	}
	if list, err := c.TelemetrySinks(ctx); err != nil || len(list) != 0 {
		t.Errorf("%+v %v", list, err)
	}
}
