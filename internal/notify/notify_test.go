package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// plainKeyring seals with nothing, for tests of what is not sealing: the server's own is tested in
// internal/server.
type plainKeyring struct{}

func (plainKeyring) SealChannel(_ string, value []byte) []byte           { return value }
func (plainKeyring) OpenChannel(_ string, sealed []byte) ([]byte, error) { return sealed, nil }

type received struct {
	body      []byte
	event     string
	signature string
}

// receiver is a webhook's other end.
type receiver struct {
	*httptest.Server
	mu   sync.Mutex
	got  []received
	fail int
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	rc := &receiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if rc.fail > 0 {
			rc.fail--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		rc.got = append(rc.got, received{body, r.Header.Get("X-Moorage-Event"), r.Header.Get("X-Moorage-Signature")})
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) messages() []map[string]any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make([]map[string]any, 0, len(rc.got))
	for _, g := range rc.got {
		var m map[string]any
		if err := json.Unmarshal(g.body, &m); err != nil {
			panic(err)
		}
		out = append(out, m)
	}
	return out
}

type fixture struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
	bus *events.Bus
	s   *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	db, err := store.Open(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	bus := events.New(db, log)
	s := &Service{
		DB: db, Bus: bus, Keyring: plainKeyring{}, Log: log, ConsoleURL: "http://moorage.test",
		Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, RetryAfter: time.Millisecond, Window: time.Minute,
		DiskUsage: func() (float64, error) { return 10, nil },
	}
	done := make(chan struct{})
	following := make(chan struct{}, 8)
	s.following = func() { following <- struct{}{} }
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	<-following
	t.Cleanup(func() {
		cancel()
		<-done
		s.Wait()
		db.Close()
	})
	return &fixture{t: t, ctx: ctx, db: db, bus: bus, s: s}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// channel adds a channel, and a rule of the whole server for it.
func (f *fixture) channel(kind, url string, kinds ...string) store.NotificationChannel {
	f.t.Helper()
	c, _, err := f.s.AddChannel(f.ctx, "c-"+ids.New(ids.Channel), kind, Config{URL: url}, "owner@example.com")
	if err != nil {
		f.t.Fatal(err)
	}
	if len(kinds) == 0 {
		kinds = []string{Everything}
	}
	if _, err := f.s.AddRule(f.ctx, c.ID, "", "", kinds, "owner@example.com"); err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) publish(kind, app string, data map[string]any) {
	f.bus.Publish(f.ctx, store.Event{Type: kind, App: app, Env: "production", Data: data})
}

func TestWebhookIsSignedAndSaysWhatHappened(t *testing.T) {
	f := newFixture(t)
	rc := newReceiver(t)
	c, secret, err := f.s.AddChannel(f.ctx, "hook", KindWebhook, Config{URL: rc.URL + "/hook"}, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 64 {
		t.Fatalf("a secret of %d characters", len(secret))
	}
	if _, err := f.s.AddRule(f.ctx, "hook", "", "", []string{events.DeploymentFailed}, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	f.publish(events.DeploymentFailed, "shop", map[string]any{"deployment": "dep_1", "error": "it did not build"})
	waitFor(t, "the message", func() bool { return len(rc.messages()) == 1 })

	m := rc.messages()[0]
	if m["type"] != events.DeploymentFailed || m["app"] != "shop" || m["env"] != "production" || m["link"] != "http://moorage.test/deployments/dep_1" {
		t.Fatalf("%v", m)
	}
	if data, _ := m["data"].(map[string]any); data["error"] != "it did not build" {
		t.Fatalf("%v", m)
	}
	rc.mu.Lock()
	got := rc.got[0]
	rc.mu.Unlock()
	if got.event != events.DeploymentFailed {
		t.Fatalf("%q", got.event)
	}
	stamp, _, _ := strings.Cut(strings.TrimPrefix(got.signature, "t="), ",")
	var unix int64
	fmt.Sscan(stamp, &unix)
	if time.Since(time.Unix(unix, 0)) > time.Minute {
		t.Fatalf("%q", got.signature)
	}
	if want := Sign(secret, time.Unix(unix, 0), got.body); got.signature != want {
		t.Fatalf("signed %q, want %q", got.signature, want)
	}
	if Sign("something else", time.Unix(unix, 0), got.body) == got.signature {
		t.Fatal("any secret signs it")
	}

	// It notes that it went.
	waitFor(t, "it to note that", func() bool {
		now, err := f.db.NotificationChannel(f.ctx, c.ID)
		return err == nil && !now.SentAt.IsZero() && now.Error == ""
	})
	// What it was not asked for does not go.
	f.publish(events.CronFailed, "shop", map[string]any{"job": "nightly"})
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.messages()); n != 1 {
		t.Fatalf("%d messages", n)
	}
}

func TestChatPayloads(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ kind, key string }{{KindSlack, "text"}, {KindTeams, "text"}, {KindDiscord, "content"}} {
		rc := newReceiver(t)
		c := f.channel(tc.kind, rc.URL)
		if _, err := f.s.Test(f.ctx, c.ID, "owner@example.com"); err != nil {
			t.Fatal(tc.kind, err)
		}
		got := rc.messages()
		if len(got) != 1 || !strings.Contains(fmt.Sprint(got[0][tc.key]), "this channel works") {
			t.Fatalf("%s: %v", tc.kind, got)
		}
	}
}

// TestMessagesCannotCallOut: what an app's build said can be in a message.
func TestMessagesCannotCallOut(t *testing.T) {
	s := &Service{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	m := Message{Kind: events.DeploymentFailed, App: "shop", Summary: "<!channel> @everyone it failed", At: time.Now()}
	rc := newReceiver(t)
	if err := s.deliver(context.Background(), KindSlack, Config{URL: rc.URL}, m); err != nil {
		t.Fatal(err)
	}
	if text := rc.messages()[0]["text"].(string); strings.Contains(text, "<!channel>") || !strings.Contains(text, "&lt;!channel&gt;") {
		t.Errorf("%q", text)
	}
	discord := newReceiver(t)
	if err := s.deliver(context.Background(), KindDiscord, Config{URL: discord.URL}, m); err != nil {
		t.Fatal(err)
	}
	mentions, _ := discord.messages()[0]["allowed_mentions"].(map[string]any)
	if parse, ok := mentions["parse"].([]any); !ok || len(parse) != 0 {
		t.Errorf("%v", discord.messages()[0])
	}
}

// TestOneMessageNotHundreds crash-loops an app, and gets one message, then a word that it recovered.
func TestOneMessageNotHundreds(t *testing.T) {
	f := newFixture(t)
	rc := newReceiver(t)
	f.channel(KindWebhook, rc.URL)
	exited := map[string]any{"process": "web", "instance": "ins_1"}

	// Two exits are not a crash loop.
	f.publish(events.InstanceExited, "shop", exited)
	f.publish(events.InstanceExited, "shop", exited)
	time.Sleep(50 * time.Millisecond)
	if got := rc.messages(); len(got) != 0 {
		t.Fatalf("%v", got)
	}

	for range 40 {
		f.publish(events.InstanceExited, "shop", exited)
	}
	waitFor(t, "the message", func() bool { return len(rc.messages()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	got := rc.messages()
	if len(got) != 1 || got[0]["type"] != CrashLoop || !strings.Contains(got[0]["summary"].(string), "web keeps crashing") {
		t.Fatalf("%v", got)
	}

	// Another app is another matter, and so is another process of it.
	for range 3 {
		f.publish(events.InstanceExited, "blog", exited)
		f.publish(events.InstanceExited, "shop", map[string]any{"process": "worker"})
	}
	waitFor(t, "the others' messages", func() bool { return len(rc.messages()) == 3 })

	// An instance that answers is not an end to crashing: another process, or the same one between
	// crashes, says nothing of it.
	f.publish(events.InstanceUnhealthy, "shop", map[string]any{"process": "web", "instance": "ins_2"})
	waitFor(t, "the unhealthy instance", func() bool { return len(rc.messages()) == 4 })
	f.publish(events.InstanceHealthy, "shop", map[string]any{"process": "web", "instance": "ins_2"})
	waitFor(t, "it to be healthy", func() bool { return len(rc.messages()) == 5 })
	if m := rc.messages()[4]; m["type"] != Recovered || m["data"].(map[string]any)["recovered_from"] != events.InstanceUnhealthy {
		t.Fatalf("%v", m)
	}

	// Not while it is still exiting, however long it has been going on.
	f.s.checkQuiet(f.ctx, time.Now().Add(quietAfter/2))
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.messages()); n != 5 {
		t.Fatalf("%d messages", n)
	}

	// Having stayed up for long enough, every process has stopped crashing, once.
	f.s.checkQuiet(f.ctx, time.Now().Add(quietAfter+time.Second))
	waitFor(t, "the word that they recovered", func() bool { return len(rc.messages()) == 8 })
	seen := map[string]bool{}
	for _, m := range rc.messages()[5:] {
		if m["type"] != Recovered || !strings.Contains(m["summary"].(string), "has not crashed") {
			t.Fatalf("%v", m)
		}
		seen[fmt.Sprint(m["app"], " ", strings.Fields(m["summary"].(string))[2])] = true
	}
	if !seen["shop web"] || !seen["shop worker"] || !seen["blog web"] {
		t.Fatalf("%v", seen)
	}
	f.s.checkQuiet(f.ctx, time.Now().Add(2*quietAfter))
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.messages()); n != 8 {
		t.Fatalf("%d messages", n)
	}

	// Going wrong again after it has recovered is news at once.
	for range 3 {
		f.publish(events.InstanceExited, "shop", exited)
	}
	waitFor(t, "the message that it is back", func() bool { return len(rc.messages()) == 9 })
}

// TestNewReleaseEndsACrashLoop: what kept crashing has gone.
func TestNewReleaseEndsACrashLoop(t *testing.T) {
	f := newFixture(t)
	rc := newReceiver(t)
	f.channel(KindWebhook, rc.URL, CrashLoop, events.InstanceUnhealthy)
	for range 3 {
		f.publish(events.InstanceExited, "shop", map[string]any{"process": "web"})
	}
	f.publish(events.InstanceUnhealthy, "shop", map[string]any{"process": "web", "instance": "ins_1"})
	waitFor(t, "the alerts", func() bool { return len(rc.messages()) == 2 })
	f.publish(events.DeploymentLive, "shop", map[string]any{"deployment": "dep_2"})
	waitFor(t, "the word that they are over", func() bool { return len(rc.messages()) == 4 })
	seen := map[any]bool{}
	for _, m := range rc.messages()[2:] {
		if m["type"] != Recovered {
			t.Fatalf("%v", m)
		}
		seen[m["data"].(map[string]any)["recovered_from"]] = true
	}
	if !seen[CrashLoop] || !seen[events.InstanceUnhealthy] {
		t.Fatalf("%v", seen)
	}
	// Exits from before it do not count towards the next.
	f.publish(events.InstanceExited, "shop", map[string]any{"process": "web"})
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.messages()); n != 4 {
		t.Fatalf("%d messages", n)
	}
}

// TestUnhealthyRecoversWhenAllDo: one instance being well says nothing of another.
func TestUnhealthyRecoversWhenAllDo(t *testing.T) {
	f := newFixture(t)
	rc := newReceiver(t)
	f.channel(KindWebhook, rc.URL, events.InstanceUnhealthy)
	f.publish(events.InstanceUnhealthy, "shop", map[string]any{"process": "web", "instance": "ins_1"})
	f.publish(events.InstanceUnhealthy, "shop", map[string]any{"process": "web", "instance": "ins_2"})
	waitFor(t, "the alert", func() bool { return len(rc.messages()) == 1 })
	f.publish(events.InstanceHealthy, "shop", map[string]any{"process": "web", "instance": "ins_1"})
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.messages()); n != 1 {
		t.Fatalf("%d messages", n)
	}
	f.publish(events.InstanceHealthy, "shop", map[string]any{"process": "web", "instance": "ins_2"})
	waitFor(t, "the word that they are well", func() bool { return len(rc.messages()) == 2 })
	// Flapping stays quiet: at most this, then that it went wrong again, for a while.
	for range 20 {
		f.publish(events.InstanceUnhealthy, "shop", map[string]any{"process": "web", "instance": "ins_1"})
		f.publish(events.InstanceHealthy, "shop", map[string]any{"process": "web", "instance": "ins_1"})
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(rc.messages()); n != 3 {
		t.Fatalf("%d messages", n)
	}
}

// TestDeploymentsAreNotHeldBack: someone asked for each one, and is to hear how it went.
func TestDeploymentsAreNotHeldBack(t *testing.T) {
	f := newFixture(t)
	rc := newReceiver(t)
	f.channel(KindWebhook, rc.URL, events.DeploymentFailed, events.DeploymentLive)
	publish := func(kind, id string) {
		f.publish(kind, "shop", map[string]any{"deployment": id})
	}
	publish(events.DeploymentFailed, "dep_1")
	publish(events.DeploymentLive, "dep_2")
	publish(events.DeploymentLive, "dep_3")
	publish(events.DeploymentFailed, "dep_4")
	publish(events.DeploymentFailed, "dep_5")
	publish(events.DeploymentLive, "dep_6")
	// Failed, live with a word that it recovered, live, failed, failed, live with another.
	waitFor(t, "them all", func() bool { return len(rc.messages()) == 8 })
	counts := map[string]int{}
	for _, m := range rc.messages() {
		counts[m["type"].(string)]++
	}
	if counts[events.DeploymentFailed] != 3 || counts[events.DeploymentLive] != 3 || counts[Recovered] != 2 {
		t.Fatalf("%v", counts)
	}
}

// TestHeldBackIsCounted says how many like it were held back, when the next one goes.
func TestHeldBackIsCounted(t *testing.T) {
	f := newFixture(t)
	f.s.Window = 150 * time.Millisecond
	rc := newReceiver(t)
	f.channel(KindWebhook, rc.URL, events.CronFailed)
	for range 4 {
		f.publish(events.CronFailed, "shop", map[string]any{"job": "nightly", "run": "run_1"})
	}
	waitFor(t, "the message", func() bool { return len(rc.messages()) == 1 })
	if link := rc.messages()[0]["link"]; link != "http://moorage.test/apps/shop/runs/run_1" {
		t.Fatalf("%v", link)
	}
	// Another job is another matter.
	f.publish(events.CronFailed, "shop", map[string]any{"job": "weekly"})
	waitFor(t, "the other job's", func() bool { return len(rc.messages()) == 2 })

	time.Sleep(200 * time.Millisecond)
	f.publish(events.CronFailed, "shop", map[string]any{"job": "nightly"})
	waitFor(t, "the next", func() bool { return len(rc.messages()) == 3 })
	if m := rc.messages()[2]; m["suppressed"] != float64(3) {
		t.Fatalf("%v", m)
	}
}

func TestDeploymentRecovers(t *testing.T) {
	f := newFixture(t)
	rc := newReceiver(t)
	f.channel(KindWebhook, rc.URL, events.DeploymentFailed)
	// A deployment that goes live with nothing wrong says nothing to this rule.
	f.publish(events.DeploymentLive, "shop", map[string]any{"deployment": "dep_0"})
	f.publish(events.DeploymentFailed, "shop", map[string]any{"deployment": "dep_1"})
	waitFor(t, "the failure", func() bool { return len(rc.messages()) == 1 })
	f.publish(events.DeploymentLive, "shop", map[string]any{"deployment": "dep_2"})
	waitFor(t, "the recovery", func() bool { return len(rc.messages()) == 2 })
	if m := rc.messages()[1]; m["type"] != Recovered || m["link"] != "http://moorage.test/deployments/dep_2" {
		t.Fatalf("%v", m)
	}
}

func TestRuleScopes(t *testing.T) {
	r := store.NotificationRule{Events: []string{events.CronFailed}}
	for _, tc := range []struct {
		project, app string
		kind         string
		ofApp        string
		ofProject    string
		want         bool
	}{
		{"", "", events.CronFailed, "shop", "web", true},
		{"", "", events.DeploymentFailed, "shop", "web", false},
		{"web", "", events.CronFailed, "shop", "web", true},
		{"data", "", events.CronFailed, "shop", "web", false},
		{"", "shop", events.CronFailed, "shop", "web", true},
		{"", "blog", events.CronFailed, "shop", "web", false},
		{"", "shop", events.CronFailed, "", "", false}, // what is of the server is not of an app
		{"web", "", events.CronFailed, "", "", false},
	} {
		r.Project, r.App = tc.project, tc.app
		if got := ruleApplies(r, tc.kind, tc.ofApp, tc.ofProject); got != tc.want {
			t.Errorf("%+v: %v", tc, got)
		}
	}
}

func TestRetriesAndNotesFailure(t *testing.T) {
	f := newFixture(t)
	rc := newReceiver(t)
	rc.fail = 2
	c := f.channel(KindWebhook, rc.URL, events.CronFailed)
	f.publish(events.CronFailed, "shop", map[string]any{"job": "nightly"})
	waitFor(t, "it to go, the third time", func() bool { return len(rc.messages()) == 1 })

	rc.mu.Lock()
	rc.fail = 100
	rc.mu.Unlock()
	f.publish(events.CronFailed, "shop", map[string]any{"job": "weekly"})
	waitFor(t, "it to say that it failed", func() bool {
		now, err := f.db.NotificationChannel(f.ctx, c.ID)
		return err == nil && strings.Contains(now.Error, "500")
	})
	rc.mu.Lock()
	rc.fail = 0
	rc.mu.Unlock()
	if _, err := f.s.Test(f.ctx, c.ID, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if now, _ := f.db.NotificationChannel(f.ctx, c.ID); now.Error != "" {
		t.Fatalf("%q", now.Error)
	}
}

// TestAddressesItMayNotReach refuses this machine, its link-local neighbours and metadata services,
// whatever the name says, unless allowed; a redirect goes nowhere either.
func TestAddressesItMayNotReach(t *testing.T) {
	rc := newReceiver(t)
	ctx := context.Background()
	s := &Service{}
	cfg := Config{URL: rc.URL, Secret: "s"}
	m := Message{Kind: "test", Summary: "x", At: time.Now()}
	err := s.deliver(ctx, KindWebhook, cfg, m)
	if err == nil || !strings.Contains(err.Error(), "may not reach 127.0.0.1") {
		t.Fatalf("%v", err)
	}
	if err := (&Service{Allow: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}).deliver(ctx, KindWebhook, cfg, m); err == nil {
		t.Fatal("allowing one range allowed another")
	}
	if len(rc.messages()) != 0 {
		t.Fatal("it got there")
	}
	// A name is no way round it.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(rc.URL, "http://"))
	if err := s.deliver(ctx, KindWebhook, Config{URL: "http://localhost:" + port, Secret: "s"}, m); err == nil || !strings.Contains(err.Error(), "may not reach") {
		t.Fatalf("%v", err)
	}
	for _, address := range []string{"169.254.169.254", "[fd00:ec2::254]", "100.100.100.200", "[::1]", "0.0.0.0", "224.0.0.1"} {
		err := s.deliver(ctx, KindWebhook, Config{URL: "http://" + address + ":9/", Secret: "s"}, m)
		if err == nil || !strings.Contains(err.Error(), "may not reach") {
			t.Errorf("%s: %v", address, err)
		}
	}
	// Nor this machine by another of its addresses.
	own, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range own {
		network, ok := a.(*net.IPNet)
		if !ok || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
			continue
		}
		address := net.JoinHostPort(network.IP.String(), port)
		if err := s.deliver(ctx, KindWebhook, Config{URL: "http://" + address, Secret: "s"}, m); err == nil || !strings.Contains(err.Error(), "may not reach") {
			t.Errorf("%s: %v", address, err)
		}
	}
	// Mail is held to the same.
	err = s.deliver(ctx, KindEmail, Config{Host: "127.0.0.1", Port: 25, From: "a@example.com", To: []string{"b@example.com"}}, m)
	if err == nil || !strings.Contains(err.Error(), "may not reach") {
		t.Fatalf("%v", err)
	}

	// Allowed, it goes. A redirect is an answer that is not success, and is not followed.
	allowed := &Service{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	if err := allowed.deliver(ctx, KindWebhook, cfg, m); err != nil {
		t.Fatal(err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, rc.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	before := len(rc.messages())
	if err := allowed.deliver(ctx, KindWebhook, Config{URL: redirect.URL, Secret: "s"}, m); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("%v", err)
	}
	if len(rc.messages()) != before {
		t.Fatal("it followed the redirect")
	}
}

// TestErrorsDoNotShowTheAddress: a chat webhook's address is its secret.
func TestErrorsDoNotShowTheAddress(t *testing.T) {
	s := &Service{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	err = s.deliver(context.Background(), KindSlack, Config{URL: "http://" + address + "/services/SECRET/PATH"}, Message{Summary: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
}

func TestCheckConfig(t *testing.T) {
	for _, tc := range []struct {
		kind string
		cfg  Config
		ok   bool
	}{
		{KindWebhook, Config{URL: "https://example.com/x"}, true},
		{KindSlack, Config{URL: "http://example.com/x"}, true},
		{KindWebhook, Config{URL: "ftp://example.com/x"}, false},
		{KindDiscord, Config{URL: ""}, false},
		{KindEmail, Config{Host: "smtp.example.com", From: "a@example.com", To: []string{"b@example.com"}}, true},
		{KindEmail, Config{Host: "smtp.example.com", From: "a@example.com"}, false},
		{KindEmail, Config{Host: "smtp.example.com", From: "a@example.com", To: []string{"b@example.com\r\nBcc: c@example.com"}}, false},
		{KindEmail, Config{Host: "smtp.example.com", From: "a\r\n@example.com", To: []string{"b@example.com"}}, false},
		{"pigeon", Config{URL: "https://example.com/x"}, false},
	} {
		if err := tc.cfg.Check(tc.kind); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// TestEmail sends through a small mail server: what it says, and that a line of a summary cannot
// reach the headers.
func TestEmail(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 test ESMTP\r\n")
		var data strings.Builder
		var inData bool
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				got <- data.String()
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					fmt.Fprint(conn, "250 kept\r\n")
					continue
				}
				data.WriteString(line)
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(conn, "250 test\r\n")
			case strings.HasPrefix(line, "DATA"):
				inData = true
				fmt.Fprint(conn, "354 go on\r\n")
			case strings.HasPrefix(line, "QUIT"):
				fmt.Fprint(conn, "221 bye\r\n")
				got <- data.String()
				return
			default:
				data.WriteString("> " + line)
				fmt.Fprint(conn, "250 ok\r\n")
			}
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	var n int
	fmt.Sscan(port, &n)
	s := &Service{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	cfg := Config{Host: host, Port: n, From: "moorage@example.com", To: []string{"ops@example.com", "oncall@example.com"}}
	m := Message{Kind: events.DeploymentFailed, App: "shop", Env: "production", Summary: "It failed\r\nBcc: evil@example.com", Link: "http://moorage.test/x", At: time.Now(), Suppressed: 2}
	if err := s.deliver(context.Background(), KindEmail, cfg, m); err != nil {
		t.Fatal(err)
	}
	mail := <-got
	for _, want := range []string{"MAIL FROM:<moorage@example.com>", "RCPT TO:<ops@example.com>", "RCPT TO:<oncall@example.com>", "To: ops@example.com, oncall@example.com", "Subject: shop: It failed  Bcc: evil@example.com\r\n", "and 2 more like it", "http://moorage.test/x"} {
		if !strings.Contains(mail, want) {
			t.Errorf("no %q in:\n%s", want, mail)
		}
	}
	headers, _, _ := strings.Cut(mail, "\r\n\r\n")
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("a summary reached the headers:\n%s", mail)
		}
	}
}

func TestDiskGoesLowAndRecovers(t *testing.T) {
	f := newFixture(t)
	rc := newReceiver(t)
	f.channel(KindWebhook, rc.URL, events.DiskLow)
	var mu sync.Mutex
	used := 50.0
	f.s.DiskUsage = func() (float64, error) {
		mu.Lock()
		defer mu.Unlock()
		return used, nil
	}
	set := func(v float64) {
		mu.Lock()
		used = v
		mu.Unlock()
		f.s.checkDisk(f.ctx)
	}
	set(70)
	time.Sleep(30 * time.Millisecond)
	if n := len(rc.messages()); n != 0 {
		t.Fatalf("%d", n)
	}
	set(93)
	waitFor(t, "the warning", func() bool { return len(rc.messages()) == 1 })
	if m := rc.messages()[0]; m["type"] != events.DiskLow || !strings.Contains(m["summary"].(string), "93%") || m["link"] != "http://moorage.test/settings/system" {
		t.Fatalf("%v", m)
	}
	set(88) // not yet: it would go back and forth around one figure
	set(91)
	time.Sleep(30 * time.Millisecond)
	if n := len(rc.messages()); n != 1 {
		t.Fatalf("%d", n)
	}
	set(70)
	waitFor(t, "the word that it has room", func() bool { return len(rc.messages()) == 2 })
	if m := rc.messages()[1]; m["type"] != Recovered {
		t.Fatalf("%v", m)
	}
}

// TestInbox tells the maintainers, the administrators and whoever pushed it, and nobody else.
func TestInbox(t *testing.T) {
	f := newFixture(t)
	add := func(email, role string) store.User {
		u := store.User{ID: ids.New(ids.User), Email: email, Name: email, Role: role, CreatedAt: time.Now().UTC()}
		if err := f.db.CreateUser(f.ctx, u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	admin := add("admin@example.com", store.RoleAdmin)
	maintainer := add("maintainer@example.com", store.RoleMember)
	pusher := add("pusher@example.com", store.RoleMember)
	other := add("other@example.com", store.RoleMember)
	bystander := add("bystander@example.com", store.RoleMember)
	if _, _, err := f.db.EnsureApp(f.ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	// The default project makes everyone its maintainer, unless told otherwise.
	if err := f.db.SetProject(f.ctx, store.DefaultProject, store.RoleMaintainer, true); err != nil {
		t.Fatal(err)
	}
	project, err := f.db.AppProject(f.ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetMember(f.ctx, project.Name, maintainer.ID, store.RoleMaintainer); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetMember(f.ctx, project.Name, other.ID, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	f.bus.Publish(f.ctx, store.Event{Type: events.DeploymentFailed, App: "shop", Env: "staging", Actor: pusher.Email, Data: map[string]any{"deployment": "dep_1", "error": "no"}})
	count := func(u store.User) int {
		list, err := f.db.Notifications(f.ctx, u.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	waitFor(t, "them to be told", func() bool {
		return count(admin) == 1 && count(maintainer) == 1 && count(pusher) == 1 && count(bystander) == 1
	})
	if n := count(other); n != 0 {
		t.Fatalf("a viewer was told %d things", n)
	}
	if err := f.db.SetProject(f.ctx, store.DefaultProject, "", true); err != nil {
		t.Fatal(err)
	}
	list, _ := f.db.Notifications(f.ctx, maintainer.ID, 10)
	if n := list[0]; n.Link != "/deployments/dep_1" || n.Env != "staging" || n.App != "shop" || !n.ReadAt.IsZero() {
		t.Fatalf("%+v", n)
	}
	if err := f.db.ReadNotifications(f.ctx, maintainer.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if list, _ = f.db.Notifications(f.ctx, maintainer.ID, 10); list[0].ReadAt.IsZero() {
		t.Fatal("not read")
	}
	if list, _ = f.db.Notifications(f.ctx, admin.ID, 10); !list[0].ReadAt.IsZero() {
		t.Fatal("someone else read it")
	}

	// A live deployment is for rules, not for the inbox; that it recovered is for both.
	f.bus.Publish(f.ctx, store.Event{Type: events.DeploymentLive, App: "shop", Env: "staging", Actor: pusher.Email, Data: map[string]any{"deployment": "dep_2"}})
	waitFor(t, "the word that it recovered", func() bool { return count(pusher) == 2 })
	list, _ = f.db.Notifications(f.ctx, pusher.ID, 10)
	if list[0].Kind != Recovered || Level(list[0].Kind) != "ok" {
		t.Fatalf("%+v", list[0])
	}

	// Of the server, only administrators.
	f.bus.Publish(f.ctx, store.Event{Type: events.DiskLow, Data: map[string]any{"percent": 95}})
	waitFor(t, "the admin to be told", func() bool { return count(admin) == 3 })
	if n := count(maintainer); n != 2 {
		t.Fatalf("%d", n)
	}
	if n := count(bystander); n != 1 {
		t.Fatalf("someone with no role was told %d things", n)
	}

	// Old ones go.
	if err := f.db.PruneNotifications(f.ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := count(admin); n != 0 {
		t.Fatalf("%d", n)
	}
}

func TestAddRuleRefusesNonsense(t *testing.T) {
	f := newFixture(t)
	c := f.channel(KindWebhook, "https://example.com/x")
	for _, tc := range []struct {
		project, app string
		kinds        []string
		want         string
	}{
		{"", "", nil, "something to ask for"},
		{"", "", []string{"nonsense"}, "not something"},
		{"default", "shop", []string{Everything}, "not both"},
		{"nothing", "", []string{Everything}, "no project"},
		{"", "nothing", []string{Everything}, "no app"},
	} {
		if _, err := f.s.AddRule(f.ctx, c.ID, tc.project, tc.app, tc.kinds, "owner@example.com"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	if _, err := f.s.AddRule(f.ctx, "no-such-channel", "", "", []string{Everything}, "owner@example.com"); err == nil {
		t.Fatal("a rule for no channel")
	}
	if _, _, err := f.s.AddChannel(f.ctx, c.Name, KindWebhook, Config{URL: "https://example.com/y"}, "owner@example.com"); err != store.ErrChannelTaken {
		t.Fatalf("%v", err)
	}
	if _, _, err := f.s.AddChannel(f.ctx, " \n", KindWebhook, Config{URL: "https://example.com/y"}, "owner@example.com"); err == nil {
		t.Fatal("no name")
	}
}

func TestMailSubjectIsEncoded(t *testing.T) {
	title := Message{App: "shop", Env: "staging", Summary: "It failed"}.title()
	if !strings.Contains(title, "·") {
		t.Fatalf("%q: this test is about a subject that is not ASCII", title)
	}
	if got := mime.QEncoding.Encode("utf-8", title); strings.Contains(got, "·") || !strings.HasPrefix(got, "=?utf-8?q?") {
		t.Fatalf("%q", got)
	}
}

// An IPv6 address can stand for an IPv4 one, and is judged as that one.
func TestAddressesThatStandForOthers(t *testing.T) {
	control := guardedDialer(nil).Control
	for _, address := range []string{
		"127.0.0.1:80", "[::1]:80", "[::ffff:127.0.0.1]:80",
		"[::7f00:1]:80", "[64:ff9b::7f00:1]:80", "[64:ff9b:1::7f00:1]:80", "[2002:7f00:1::]:80",
		"[::a9fe:a9fe]:80", "[64:ff9b::a9fe:a9fe]:80", "[2002:a9fe:a9fe::]:80",
	} {
		if err := control("tcp", address, nil); err == nil {
			t.Errorf("%s is allowed", address)
		}
	}
	for _, address := range []string{"8.8.8.8:80", "[2606:4700:4700::1111]:80", "[64:ff9b::808:808]:80", "[2002:808:808::]:80"} {
		if err := control("tcp", address, nil); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
}
