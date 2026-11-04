package applogs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/deploy"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/reconcile"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func TestDescribe(t *testing.T) {
	for _, tc := range []struct{ text, level, message string }{
		{`plain words`, "", ""},
		{`{"level":"error","msg":"it broke","code":7}`, "error", "it broke  code=7"},
		{`{"severity":"WARNING","message":"careful","time":"2026-11-03T00:00:00Z"}`, "warn", "careful"},
		{`{"level":"info","event":"started","port":80,"host":"a"}`, "info", "started  host=a  port=80"},
		{`{"just":"data"}`, "", ""},
		{`{not json`, "", ""},
		{`ERROR: could not reach the database`, "error", ""},
		{`2026/11/03 12:00:00 [warn] slow`, "warn", ""},
		{`time=now level=DEBUG msg=hi`, "debug", ""},
		{`this line is much longer than the part looked at, and it happens to mention an error`, "", ""},
		{`no errors here`, "", ""},
	} {
		level, message := describe(tc.text)
		if level != tc.level || message != tc.message {
			t.Errorf("%s: %q %q", tc.text, level, message)
		}
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	zone := time.FixedZone("x", -8*3600)
	for in, want := range map[string]time.Time{
		"":                          {},
		"15m":                       now.Add(-15 * time.Minute),
		"2h":                        now.Add(-2 * time.Hour),
		"7d":                        now.Add(-7 * 24 * time.Hour),
		"2026-11-03T15:04:05Z":      time.Date(2026, 11, 3, 15, 4, 5, 0, time.UTC),
		"2026-11-03T15:04:05+01:00": time.Date(2026, 11, 3, 14, 4, 5, 0, time.UTC),
		"2026-11-03T15:04":          time.Date(2026, 11, 3, 15, 4, 0, 0, zone),
		"2026-11-03":                time.Date(2026, 11, 3, 0, 0, 0, 0, zone),
	} {
		got, err := ParseTime(in, now, zone)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"soon", "-5m", "12", "xd"} {
		if _, err := ParseTime(bad, now, zone); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

func TestSplitSources(t *testing.T) {
	if got, err := SplitSources("stderr, build"); err != nil || len(got) != 2 || got[1] != Build {
		t.Errorf("%v %v", got, err)
	}
	if got, err := SplitSources(""); err != nil || got != nil {
		t.Errorf("%v %v", got, err)
	}
	if _, err := SplitSources("stdout,nothing"); err == nil || !strings.Contains(err.Error(), "nothing") {
		t.Errorf("%v", err)
	}
}

// fixture is a data directory with an app's logs.
type fixture struct {
	t    *testing.T
	r    Reader
	base time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(context.Background(), filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, r: Reader{DB: db, DataDir: dir}, base: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
}

func (f *fixture) at(second int) time.Time { return f.base.Add(time.Duration(second) * time.Second) }

// write adds lines to a file: "second stream text".
func (f *fixture) write(path string, lines ...string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()
	for _, line := range lines {
		var second int
		var stream string
		parts := strings.SplitN(line, " ", 3)
		fmt.Sscanf(parts[0], "%d", &second)
		stream = parts[1]
		fmt.Fprint(file, logs.Format(f.at(second), stream, parts[2]))
	}
}

func (f *fixture) instance(name string) string {
	return filepath.Join(reconcile.LogDir(f.r.DataDir, "shop", "production"), name)
}

func texts(entries []Entry) string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Who() + ":" + e.Display()
	}
	return strings.Join(out, " | ")
}

func TestRead(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.write(f.instance("web.1.ins_a.log"), "1 O one", "3 E three", "5 M restarted", `7 O {"level":"error","msg":"seven"}`)
	f.write(f.instance("web.2.ins_b.log"), "2 O two", "4 O four")
	f.write(f.instance("worker.1.ins_c.log"), "6 O six")
	f.write(deploy.LogPath(f.r.DataDir, "shop", "dep_1"), "8 M ▸ build", "9 O built")
	access := filepath.Join(f.r.DataDir, "logs", "router", "shop", "production.log")
	if err := os.MkdirAll(filepath.Dir(access), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(access, []byte(`{"time":"2026-11-03T12:00:10Z","method":"GET","path":"/x","status":500,"bytes":3,"ms":1.5,"client":"::1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		q    Query
		want string
		more bool
	}{
		"merged by time":                     {Query{}, "web.1:one | web.2:two | web.1:three | web.2:four | web.1:restarted | worker.1:six | web.1:seven  ", false},
		"the newest":                         {Query{Tail: 2}, "worker.1:six | web.1:seven", true},
		"one process":                        {Query{Process: "worker"}, "worker.1:six", false},
		"one instance":                       {Query{Process: "web", Instance: "2"}, "web.2:two | web.2:four", false},
		"by its ID":                          {Query{Instance: "ins_b"}, "web.2:two | web.2:four", false},
		"standard error":                     {Query{Sources: []string{Stderr}}, "web.1:three", false},
		"Moorage's notes":                    {Query{Sources: []string{Moorage}}, "web.1:restarted", false},
		"builds":                             {Query{Sources: []string{Build}}, "build:▸ build | build:built", false},
		"requests":                           {Query{Sources: []string{Access}}, "access:GET /x 500  1.5 ms  3 B  ::1", false},
		"search":                             {Query{Grep: "THR"}, "web.1:three", false},
		"search what the line says":          {Query{Grep: `"level":"error"`}, "web.1:seven", false},
		"problems":                           {Query{Problems: true}, "web.1:three | web.1:seven", false},
		"a request that failed is a problem": {Query{Problems: true, Sources: []string{Access}}, "access:GET /x 500  1.5 ms  3 B  ::1", false},
		"since":                              {Query{Since: f.at(6)}, "worker.1:six | web.1:seven", false},
		"until":                              {Query{Until: f.at(2)}, "web.1:one | web.2:two", false},
		"after":                              {Query{After: f.at(6)}, "web.1:seven", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, more, err := f.r.Read(ctx, "shop", "production", tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(texts(got)) != strings.TrimSpace(tc.want) || more != tc.more {
				t.Errorf("%q, more: %v", texts(got), more)
			}
		})
	}

	got, _, err := f.r.Read(ctx, "shop", "production", Query{Sources: []string{Access}})
	if err != nil || len(got) != 1 || got[0].Level != "error" || got[0].Stream() != logs.Stdout {
		t.Errorf("%+v %v", got, err)
	}
	if list := f.r.Instances("shop", "production"); strings.Join(list, ",") != "web.1,web.2,worker.1" {
		t.Errorf("%v", list)
	}
	if got, _, err := f.r.Read(ctx, "nothing", "production", Query{}); err != nil || len(got) != 0 {
		t.Errorf("an app with no logs: %v %v", got, err)
	}
}

func TestReadRotated(t *testing.T) {
	f := newFixture(t)
	path := f.instance("web.1.ins_a.log")
	f.write(path+".1", "1 O old")
	f.write(path, "2 O new")
	got, _, err := f.r.Read(context.Background(), "shop", "production", Query{})
	if err != nil || texts(got) != "web.1:old | web.1:new" {
		t.Errorf("%q %v", texts(got), err)
	}
}

func TestReadEvents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.r.DB.EnsureApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := f.r.DB.AddEvent(ctx, store.Event{ID: "evt_1", At: f.at(1), Type: "deployment.failed", App: "shop", Env: "production", Data: map[string]any{"error": "it broke"}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := f.r.Read(ctx, "shop", "production", Query{Sources: []string{Events}})
	if err != nil || len(got) != 1 || got[0].Who() != "events" || got[0].Level != "error" || !strings.Contains(got[0].Text, "it broke") {
		t.Errorf("%+v %v", got, err)
	}
}

func TestFollow(t *testing.T) {
	f := newFixture(t)
	first := f.instance("web.1.ins_a.log")
	f.write(first, "1 O history")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		done <- f.r.Follow(ctx, "shop", "production", Query{Tail: 10, Grep: "keep"}, func(e Entry) error {
			lines <- e.Who() + ":" + e.Text
			return nil
		})
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-lines:
			if got != want {
				t.Errorf("%q, not %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %q", want)
		}
	}
	f.write(first, "2 O skip this", "3 O keep this")
	expect("web.1:keep this")
	// An instance that starts later is followed from its start.
	f.write(f.instance("web.2.ins_b.log"), "4 O keep that")
	expect("web.2:keep that")
	cancel()
	if err := <-done; err != nil {
		t.Errorf("%v", err)
	}

	// Coming back after a moment gets what was missed, and nothing twice.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() {
		f.r.Follow(ctx2, "shop", "production", Query{Tail: 10, After: f.at(3)}, func(e Entry) error {
			lines <- e.Who() + ":" + e.Text
			return nil
		})
	}()
	expect("web.2:keep that")
	f.write(first, "5 O new")
	expect("web.1:new")
}
