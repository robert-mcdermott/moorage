package events

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/store"
)

func newBus(t *testing.T) (*Bus, *store.DB) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	return New(db, slog.New(slog.NewTextHandler(io.Discard, nil))), db
}

func TestPublishRecordsAndPassesOn(t *testing.T) {
	b, db := newBus(t)
	ctx := context.Background()
	sub := b.Subscribe()
	defer sub.Close()
	b.Publish(ctx, store.Event{Type: ConfigChanged, App: "shop", Env: "production", Actor: "ada@example.com",
		Data: map[string]any{"set": []string{"API_KEY"}, "unset": []string{"OLD"}}})

	select {
	case e := <-sub.C:
		if e.ID == "" || e.At.IsZero() || e.App != "shop" {
			t.Errorf("passed on: %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing passed on")
	}
	stored, err := db.Events(ctx, store.EventFilter{})
	if err != nil || len(stored) != 1 {
		t.Fatalf("%v %+v", err, stored)
	}
	// Read back from JSON, it says the same.
	if got := Summary(stored[0]); got != "Variables changed by ada@example.com: API_KEY, OLD" {
		t.Errorf("%q", got)
	}
}

func TestTransientEventsAreNotKept(t *testing.T) {
	b, db := newBus(t)
	ctx := context.Background()
	sub := b.Subscribe()
	defer sub.Close()
	b.Publish(ctx, store.Event{Type: DeploymentStarted, App: "shop"})
	select {
	case e := <-sub.C:
		if e.Type != DeploymentStarted {
			t.Errorf("%+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing passed on")
	}
	if list, err := db.Events(ctx, store.EventFilter{}); err != nil || len(list) != 0 {
		t.Errorf("kept: %v %+v", err, list)
	}
}

func TestFilters(t *testing.T) {
	b, db := newBus(t)
	ctx := context.Background()
	for _, app := range []string{"a", "b", "a", "b", ""} {
		b.Publish(ctx, store.Event{Type: ServerStarted, App: app, Env: "production"})
	}
	all, err := db.Events(ctx, store.EventFilter{})
	if err != nil || len(all) != 5 {
		t.Fatalf("%v %d", err, len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID <= all[i].ID {
			t.Errorf("not newest first: %v", all)
		}
	}
	only, _ := db.Events(ctx, store.EventFilter{App: "a"})
	if len(only) != 2 {
		t.Errorf("by app: %d", len(only))
	}
	older, _ := db.Events(ctx, store.EventFilter{Before: all[1].ID})
	if len(older) != 3 || older[0].ID != all[2].ID {
		t.Errorf("before: %+v", older)
	}
	newer, _ := db.Events(ctx, store.EventFilter{After: all[2].ID})
	if len(newer) != 2 || newer[0].ID != all[1].ID || newer[1].ID != all[0].ID {
		t.Errorf("after, oldest first: %+v", newer)
	}
	limited, _ := db.Events(ctx, store.EventFilter{Limit: 2})
	if len(limited) != 2 {
		t.Errorf("limit: %d", len(limited))
	}
}

func TestASlowFollowerLosesItsSubscription(t *testing.T) {
	b, _ := newBus(t)
	ctx := context.Background()
	slow := b.Subscribe()
	defer slow.Close()
	for range buffer + 1 {
		b.Publish(ctx, store.Event{Type: ServerStarted})
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != buffer {
		t.Errorf("read %d", n)
	}
	// Publishing goes on, with nobody blocked.
	b.Publish(ctx, store.Event{Type: ServerStarted})
}

func TestCloseEndsFollowers(t *testing.T) {
	b, _ := newBus(t)
	sub := b.Subscribe()
	b.Close()
	if _, ok := <-sub.C; ok {
		t.Error("a follower of a closed bus got something")
	}
	sub.Close() // closing it after is harmless
	late := b.Subscribe()
	if _, ok := <-late.C; ok {
		t.Error("a late follower got something")
	}
	var none *Bus
	none.Publish(context.Background(), store.Event{Type: ServerStarted})
	none.Close()
}

func TestPrune(t *testing.T) {
	b, db := newBus(t)
	ctx := context.Background()
	old := store.Event{ID: "evt_old", At: time.Now().Add(-Retention - time.Hour), Type: ServerStarted}
	if err := db.AddEvent(ctx, old); err != nil {
		t.Fatal(err)
	}
	b.Publish(ctx, store.Event{Type: ServerStarted})
	stop, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { b.Prune(stop, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, _ := db.Events(ctx, store.EventFilter{})
		if len(list) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not pruned: %+v", list)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestSummaries(t *testing.T) {
	for _, c := range []struct {
		e    store.Event
		want string
	}{
		{store.Event{Type: DeploymentQueued, Actor: "ada", Data: map[string]any{"source": "push main@abc1234"}}, "Deployment queued by ada: push main@abc1234"},
		{store.Event{Type: DeploymentLive, Actor: "ada", Data: map[string]any{"version": 8}}, "Deployed v8 by ada"},
		{store.Event{Type: DeploymentFailed, Data: map[string]any{"error": "the build failed"}}, "Deployment failed: the build failed"},
		{store.Event{Type: ReleaseCreated, Actor: "ada", Data: map[string]any{"version": float64(3), "reason": "rollback"}}, "Rolled back by ada: v3 now serves"},
		{store.Event{Type: InstanceExited, Data: map[string]any{"process": "web", "index": 2, "code": 137, "signal": "OOM"}}, "web.2 exited (out of memory) and restarted"},
		{store.Event{Type: InstanceExited, Data: map[string]any{"process": "web", "index": 1, "code": 3}}, "web.1 exited (status 3) and restarted"},
		{store.Event{Type: InstanceUnhealthy, Data: map[string]any{"process": "worker", "index": 1}}, "worker.1 is unhealthy, and gets no traffic"},
		{store.Event{Type: ServerStarted, Data: map[string]any{"version": "0.3.0"}}, "The server started, version 0.3.0"},
		{store.Event{Type: "something.new"}, "something.new"},
	} {
		if got := Summary(c.e); got != c.want {
			t.Errorf("%s: %q, want %q", c.e.Type, got, c.want)
		}
	}
	if strings.Contains(Summary(store.Event{Type: ConfigChanged, Data: map[string]any{"set": []string{"A"}}}), "value") {
		t.Error("a change of variables speaks of values")
	}
}

func TestWhatIsSaidOfAnEngine(t *testing.T) {
	if got := Summary(store.Event{Type: EngineOK}); got != "The container engine is answering again" {
		t.Error(got)
	}
	if got := Summary(store.Event{Type: EngineOK, Data: map[string]any{"not_needed": true}}); got != "Nothing here needs the container engine any more" {
		t.Error(got)
	}
}
