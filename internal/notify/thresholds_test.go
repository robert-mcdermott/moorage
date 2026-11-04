package notify

import (
	"errors"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func TestValueOfAThreshold(t *testing.T) {
	few := metrics.Recent{Requests: 4, Errors: 4}
	some := metrics.Recent{Requests: 200, Errors: 10, P95: 340, HasP95: true, CPU: map[string]float64{"web.1": 40, "web.2": 75}, Memory: map[string]float64{"web.1": 400, "web.2": 900, "worker.1": 100}}
	limits := map[string]int64{"web.1": 1000, "web.2": 1000}
	for _, c := range []struct {
		metric string
		recent metrics.Recent
		want   float64
		known  bool
	}{
		{ThresholdErrorRate, some, 5, true},
		{ThresholdErrorRate, few, 0, false},
		{ThresholdErrorRate, metrics.Recent{}, 0, false},
		{ThresholdLatency, some, 340, true},
		{ThresholdLatency, few, 0, false},
		{ThresholdCPU, some, 75, true},
		{ThresholdCPU, few, 0, false},
		{ThresholdMemory, some, 90, true}, // worker.1 has no limit to be near
		{ThresholdMemory, metrics.Recent{Memory: map[string]float64{"worker.1": 100}}, 0, false},
	} {
		got, known := valueOf(c.metric, c.recent, limits)
		if got != c.want || known != c.known {
			t.Errorf("%s of %+v: %v %v", c.metric, c.recent, got, known)
		}
	}
}

func TestCheckingAThreshold(t *testing.T) {
	for _, c := range []struct {
		metric  string
		above   float64
		minutes int
		ok      bool
	}{
		{ThresholdErrorRate, 5, 5, true},
		{ThresholdLatency, 800, 1, true},
		{ThresholdLatency, 80000, 1440, true},
		{ThresholdCPU, 350, 5, true},
		{ThresholdMemory, 90, 5, true},
		{"disk", 90, 5, false},
		{ThresholdErrorRate, 0, 5, false},
		{ThresholdErrorRate, -1, 5, false},
		{ThresholdErrorRate, 101, 5, false},
		{ThresholdMemory, 101, 5, false},
		{ThresholdLatency, 800, 0, false},
		{ThresholdLatency, 800, 1441, false},
	} {
		if why := ThresholdCheck(c.metric, c.above, c.minutes); (why == "") != c.ok {
			t.Errorf("%+v: %q", c, why)
		}
	}
}

func TestThresholdsTellWhenTheyAreCrossedAndCleared(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.db.EnsureApp(f.ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateEnvironment(f.ctx, "shop", "production", store.KindProduction, ""); err != nil {
		t.Fatal(err)
	}
	e, err := f.db.Environment(f.ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New(f.db, f.s.Log)
	f.s.Metrics = m
	if err := f.db.AddAlertThreshold(f.ctx, store.AlertThreshold{ID: "thr_1", EnvID: e.ID, Metric: ThresholdErrorRate, Above: 10, Minutes: 3, CreatedBy: "ann", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddAlertThreshold(f.ctx, store.AlertThreshold{ID: "thr_2", EnvID: e.ID, Metric: ThresholdErrorRate, Above: 10, Minutes: 9, CreatedBy: "ann", CreatedAt: time.Now()}); !errors.Is(err, store.ErrThresholdExists) {
		t.Errorf("the same one again: %v", err)
	}
	sub := f.bus.Subscribe()
	defer sub.Close()
	now := time.Now().UTC().Truncate(time.Minute)
	minute := func(ok, failed int) {
		for range ok {
			m.Begin(e.ID)(200, time.Millisecond, 0)
		}
		for range failed {
			m.Begin(e.ID)(500, time.Millisecond, 0)
		}
		now = now.Add(time.Minute)
		m.Tick(now.Add(-time.Second))
		f.s.checkThresholds(f.ctx, now, time.Minute)
	}
	received := func() []store.Event {
		var out []store.Event
		for {
			select {
			case e := <-sub.C:
				if e.Type == events.ThresholdCrossed || e.Type == events.ThresholdOK {
					out = append(out, e)
				}
			case <-time.After(100 * time.Millisecond):
				return out
			}
		}
	}

	// All is well, and then a minute is bad, which is not yet three.
	minute(100, 0)
	minute(50, 50)
	minute(50, 50)
	if got := received(); len(got) != 0 {
		t.Fatalf("too soon: %+v", got)
	}
	if value, known, firing := f.s.ThresholdStatus("thr_1"); !known || value != 50 || firing {
		t.Errorf("%v %v %v", value, known, firing)
	}

	// A good minute starts it again.
	minute(100, 0)
	minute(50, 50)
	minute(50, 50)
	if got := received(); len(got) != 0 {
		t.Fatalf("it did not start again: %+v", got)
	}
	// Three in a row is enough, and it tells once.
	minute(50, 50)
	got := received()
	if len(got) != 1 || got[0].Type != events.ThresholdCrossed || got[0].App != "shop" || got[0].Env != "production" {
		t.Fatalf("%+v", got)
	}
	if text := events.Summary(got[0]); text != "The error rate has been above 10% for 3 minutes, and is 50%" {
		t.Errorf("%q", text)
	}
	minute(50, 50)
	minute(50, 50)
	if got := received(); len(got) != 0 {
		t.Errorf("it told again: %+v", got)
	}

	// No requests says nothing either way.
	minute(0, 0)
	if got := received(); len(got) != 0 {
		t.Errorf("%+v", got)
	}
	if _, _, firing := f.s.ThresholdStatus("thr_1"); !firing {
		t.Error("a quiet minute cleared it")
	}

	// A good minute clears it.
	minute(100, 0)
	got = received()
	if len(got) != 1 || got[0].Type != events.ThresholdOK {
		t.Fatalf("%+v", got)
	}
	if text := events.Summary(got[0]); text != "The error rate is back under 10%, at 0%" {
		t.Errorf("%q", text)
	}

	// Removed, it says nothing further.
	if err := f.db.DeleteAlertThreshold(f.ctx, e.ID, "thr_1"); err != nil {
		t.Fatal(err)
	}
	minute(0, 100)
	if _, known, _ := f.s.ThresholdStatus("thr_1"); known {
		t.Error("it kept what it knew of one that has gone")
	}
}
