package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
)

// eventStream is a raw client of the stream, as a browser's EventSource is.
type eventStream struct {
	t      *testing.T
	res    *http.Response
	events chan apitypes.Event
	ids    chan string
}

func openStream(t *testing.T, url, token, lastID string) *eventStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &eventStream{t: t, res: res, events: make(chan apitypes.Event, 256)}
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		close(s.events)
		return s
	}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var e apitypes.Event
				if json.Unmarshal([]byte(data), &e) == nil {
					s.events <- e
				}
			}
		}
	}()
	return s
}

func (s *eventStream) next(wantType string) apitypes.Event {
	s.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case e, ok := <-s.events:
			if !ok {
				s.t.Fatalf("the stream ended, waiting for %s", wantType)
			}
			if e.Type == wantType {
				return e
			}
		case <-deadline:
			s.t.Fatalf("timed out waiting for %s", wantType)
		}
	}
}

func typesOf(list []apitypes.Event) string {
	var types []string
	for _, e := range list {
		types = append(types, e.Type)
	}
	return strings.Join(types, " ")
}

// TestEvents follows a server as things happen to it: over the stream, in the list,
// and by reconnecting after having missed some.
func TestEvents(t *testing.T) {
	cfg := testConfig(t)
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	token, err := os.ReadFile(NewLayout(cfg.DataDir).AdminToken)
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.TrimSpace(string(token))
	base := "http://" + addrs.HTTP.String() + "/api/v1/events/stream"

	all := openStream(t, base, admin, "")
	if ct := all.res.Header.Get("Content-Type"); ct != apitypes.SSEType {
		t.Errorf("content type: %s", ct)
	}
	if all.res.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("%v", all.res.Header)
	}
	onlyOther := openStream(t, base+"?app=other", admin, "")

	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	live := all.next("deployment.live")
	if live.App != "shop" || live.Env != "production" || live.Level != "ok" || !strings.HasPrefix(live.Summary, "Deployed v1") {
		t.Errorf("%+v", live)
	}
	if url, _ := live.Data["url"].(string); !strings.Contains(url, "shop.") {
		t.Errorf("%+v", live.Data)
	}

	// Variables: their names, never their values.
	const value = "a-value-that-must-not-travel"
	setVars(t, c, "shop", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "API_KEY", Value: value, Secret: true}}})
	changed := all.next("config.changed")
	if !strings.Contains(changed.Summary, "API_KEY") {
		t.Errorf("%+v", changed)
	}
	released := all.next("release.created")
	if released.Data["reason"] != "config" {
		t.Errorf("%+v", released)
	}
	if data, _ := json.Marshal(changed); strings.Contains(string(data), value) {
		t.Errorf("the value travelled: %s", data)
	}

	if _, err := c.Rollback(ctx, "shop", "production", 1); err != nil {
		t.Fatal(err)
	}
	if back := all.next("release.created"); back.Data["reason"] != "rollback" || !strings.HasPrefix(back.Summary, "Rolled back") {
		t.Errorf("%+v", back)
	}

	// A failure says why.
	failed := appPlan(nil)
	web := failed.Processes["web"]
	web.Command = "exit 3"
	failed.Processes["web"] = web
	if _, err := deployApp(t, c, "shop", failed); err == nil {
		t.Fatal("a deploy that should fail did not")
	}
	if bad := all.next("deployment.failed"); bad.Level != "fail" || !strings.Contains(bad.Summary, "exited") {
		t.Errorf("%+v", bad)
	}

	// Another app's follower saw none of it.
	select {
	case e := <-onlyOther.events:
		t.Errorf("a follower of another app got: %+v", e)
	default:
	}

	// The list: newest first, by app, in pages, and what is not worth keeping is not.
	list, err := c.Events(ctx, "shop", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(typesOf(list), "deployment.failed") || !strings.Contains(typesOf(list), "config.changed") {
		t.Errorf("%s", typesOf(list))
	}
	if strings.Contains(typesOf(list), "deployment.started") {
		t.Errorf("kept: %s", typesOf(list))
	}
	if list[0].Type != "deployment.failed" {
		t.Errorf("not newest first: %s", typesOf(list))
	}
	for _, e := range list {
		if e.App != "shop" {
			t.Errorf("another app's: %+v", e)
		}
	}
	server, err := c.Events(ctx, "", "", 500)
	if err == nil {
		t.Errorf("a limit over the most: %d", len(server))
	}
	server, err = c.Events(ctx, "", "", 200)
	if err != nil || !strings.Contains(typesOf(server), "server.started") {
		t.Errorf("%v %s", err, typesOf(server))
	}

	// Reconnecting replays what it missed, from its last event, as a browser does.
	older := list[len(list)-1]
	again := openStream(t, base, admin, older.ID)
	var replayed []string
	for i := 0; i < len(list)-1; i++ {
		select {
		case e, ok := <-again.events:
			if !ok {
				t.Fatal("the replay ended")
			}
			replayed = append(replayed, e.Type)
		case <-time.After(10 * time.Second):
			t.Fatalf("the replay stopped after %v", replayed)
		}
	}
	if replayed[len(replayed)-1] != "deployment.failed" || replayed[0] == older.Type {
		t.Errorf("oldest first: %v", replayed)
	}
	// The same, through the query, for a client that cannot set the header.
	viaQuery := openStream(t, base+"?after="+older.ID, admin, "")
	select {
	case e := <-viaQuery.events:
		if e.ID <= older.ID {
			t.Errorf("%+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Error("nothing after the query's event")
	}

	// A token for one app sees that app and no other.
	shopOnly, err := c.CreateToken(ctx, apitypes.NewToken{Name: "shop", Scopes: []string{"read"}, App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := client.New("http://"+addrs.HTTP.String(), shopOnly.Secret, "")
	if err != nil {
		t.Fatal(err)
	}
	mine, err := restricted.Events(ctx, "", "", 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range mine {
		if e.App != "shop" {
			t.Errorf("a token for shop saw: %+v", e)
		}
	}
	if len(mine) == 0 {
		t.Error("a token for shop saw nothing")
	}
	if _, err := restricted.Events(ctx, "other", "", 10); err == nil {
		t.Error("a token for shop could read another app's")
	}
	if s := openStream(t, base+"?app=other", shopOnly.Secret, ""); s.res.StatusCode != http.StatusForbidden {
		t.Errorf("following another app: %d", s.res.StatusCode)
	}
	if s := openStream(t, base, "moor_wrong", ""); s.res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credentials: %d", s.res.StatusCode)
	}

	// A server stopping does not wait for its followers.
	began := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("stopping took %s, with followers", took)
	}
	select {
	case _, ok := <-drained(all.events):
		if ok {
			t.Error("the stream did not end")
		}
	case <-time.After(5 * time.Second):
		t.Error("the stream did not end")
	}
}

// drained passes on the end of a stream, past whatever is left in it.
func drained(events chan apitypes.Event) <-chan apitypes.Event {
	out := make(chan apitypes.Event)
	go func() {
		for range events {
		}
		close(out)
	}()
	return out
}
