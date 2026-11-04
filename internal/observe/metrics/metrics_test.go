package metrics

import (
	"context"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/store"
)

func testStore(t *testing.T) (*Store, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(context.Background(), filepath.Join(t.TempDir(), "backups")); err != nil {
		t.Fatal(err)
	}
	return New(db, slog.Default()), db
}

func TestHistogramQuantiles(t *testing.T) {
	h := make(Histogram, HistogramSize)
	if _, ok := h.Quantile(0.5); ok {
		t.Errorf("nothing has a quantile")
	}
	for range 90 {
		h[bucketOf(3)]++ // 2 to 4 ms
	}
	for range 10 {
		h[bucketOf(900)]++ // 512 to 1024 ms
	}
	if p50, _ := h.Quantile(0.5); p50 < 2 || p50 > 4 {
		t.Errorf("p50: %v", p50)
	}
	if p95, _ := h.Quantile(0.95); p95 < 512 || p95 > 1024 {
		t.Errorf("p95: %v", p95)
	}
	if bucketOf(1e9) != len(latencyBounds) {
		t.Errorf("what is longer than all of them goes in the last")
	}
	last := make(Histogram, HistogramSize)
	last[len(latencyBounds)] = 5
	if v, ok := last.Quantile(0.99); !ok || v != latencyBounds[len(latencyBounds)-1] {
		t.Errorf("the last bucket: %v", v)
	}
}

func TestLiveSamples(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(SampleEvery)
	for i := range 6 {
		s.Gauge("env_1", CPU, "web.1", float64(i*10), now.Add(-time.Duration(i)*SampleEvery))
	}
	s.Gauge("env_2", CPU, "web.1", 99, now)
	list, step, err := s.Query(ctx, "env_1", CPU, now.Add(-time.Minute), now, 0)
	if err != nil || step != SampleEvery || len(list) != 1 || list[0].Part != "web.1" || len(list[0].Points) != 6 {
		t.Fatalf("%+v %v %v", list, step, err)
	}
	if last := list[0].Points[5]; last.Avg != 0 || !last.At.Equal(now) {
		t.Errorf("the newest: %+v", last)
	}
	// Fewer points than there are samples.
	list, step, _ = s.Query(ctx, "env_1", CPU, now.Add(-time.Minute), now, 3)
	if step <= SampleEvery || len(list[0].Points) > 5 {
		t.Errorf("merged: %v %d", step, len(list[0].Points))
	}
}

func TestRequestsBecomeSamples(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	for _, status := range []int{200, 200, 204, 301, 404, 500, 503} {
		s.Begin("env_1")(status, 3*time.Millisecond, 100)
	}
	end := s.Begin("env_1")
	if totals, _ := s.TotalsOf("env_1"); totals.InFlight != 1 {
		t.Errorf("in flight: %d", totals.InFlight)
	}
	end(200, 800*time.Millisecond, 0)
	now := time.Now()
	s.Tick(now)
	list, _, err := s.Query(ctx, "env_1", Requests, now.Add(-time.Minute), now.Add(SampleEvery+time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	sums := map[string]float64{}
	for _, ser := range list {
		for _, p := range ser.Points {
			sums[ser.Part] += p.Avg
		}
	}
	if sums["2xx"] != 4 || sums["3xx"] != 1 || sums["4xx"] != 1 || sums["5xx"] != 2 {
		t.Errorf("by class: %v", sums)
	}
	// A count is nothing where nothing happened, not a gap.
	if len(list[0].Points) < 6 {
		t.Errorf("zeroes: %+v", list[0].Points)
	}
	q, _, err := s.Quantiles(ctx, "env_1", []float64{0.5, 0.99}, []string{"p50", "p99"}, now.Add(-time.Minute), now.Add(time.Second), 0)
	if err != nil || len(q) != 2 || len(q[0].Points) != 1 {
		t.Fatalf("%+v %v", q, err)
	}
	if q[0].Points[0].Avg > 8 || q[1].Points[0].Avg < 512 {
		t.Errorf("quantiles: %+v", q)
	}
	s.Tick(now.Add(time.Second)) // nothing has happened since
	if totals, _ := s.TotalsOf("env_1"); totals.Classes["2xx"] != 4 || totals.Bytes != 700 || totals.InFlight != 0 {
		t.Errorf("totals: %+v", totals)
	}
	if _, ok := s.TotalsOf("env_nobody"); ok {
		t.Errorf("totals of nobody")
	}
}

func TestFlushAndRollUp(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	// An hour ago and more, so that a query goes to the database.
	base := time.Now().Add(-5 * time.Hour).Truncate(time.Hour)
	s.flushed = base
	for m := range 60 {
		at := base.Add(time.Duration(m) * time.Minute)
		s.Gauge("env_1", CPU, "web.1", float64(m), at)
		s.Gauge("env_1", CPU, "web.1", float64(m)+1, at.Add(10*time.Second))
		s.mu.Lock()
		s.ringOf(seriesKey{"env_1", Requests, "2xx"}).add(sample{at, 5})
		h := make(Histogram, HistogramSize)
		h[bucketOf(10)] = 5
		s.ringOf(seriesKey{"env_1", Latency, ""}).addHist(histSample{at, h})
		s.mu.Unlock()
	}
	if err := s.Flush(ctx, base.Add(time.Hour+time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A second flush of the same minute adds nothing.
	if err := s.Flush(ctx, base.Add(time.Hour+time.Minute)); err != nil {
		t.Fatal(err)
	}
	from, to := base, base.Add(time.Hour)
	gauges, step, err := s.Query(ctx, "env_1", CPU, from, to, 0)
	if err != nil || step != time.Minute || len(gauges) != 1 || len(gauges[0].Points) < 60 {
		t.Fatalf("minutes: %v %d %v", step, len(gauges), err)
	}
	if p := gauges[0].Points[10]; p.Min != 10 || p.Max != 11 || p.Avg != 10.5 || p.Last != 11 {
		t.Errorf("a minute: %+v", p)
	}
	counts, _, _ := s.Query(ctx, "env_1", Requests, from, to, 0)
	total := 0.0
	for _, p := range counts[0].Points {
		total += p.Avg
	}
	if total != 300 {
		t.Errorf("the count: %v", total)
	}
	q, _, err := s.Quantiles(ctx, "env_1", []float64{0.5}, []string{"p50"}, from, to, 0)
	if err != nil || len(q[0].Points) < 60 || q[0].Points[0].Avg < 8 || q[0].Points[0].Avg > 16 {
		t.Errorf("minutes of latency: %+v %v", q, err)
	}
	// An hour's record, read from a year's range.
	yearAgo := time.Now().Add(-20 * 24 * time.Hour)
	old, step, err := s.Query(ctx, "env_1", CPU, yearAgo, time.Now(), 0)
	if err != nil || step != time.Hour || len(old) != 1 || len(old[0].Points) != 1 {
		t.Fatalf("hours: %v %+v %v", step, old, err)
	}
	if p := old[0].Points[0]; p.Min != 0 || p.Max != 60 || math.Abs(p.Avg-30) > 0.01 {
		t.Errorf("an hour: %+v", p)
	}
	oldCounts, _, _ := s.Query(ctx, "env_1", Requests, yearAgo, time.Now(), 0)
	total = 0
	for _, p := range oldCounts[0].Points {
		total += p.Avg
	}
	if total != 300 {
		t.Errorf("an hour's count: %v", total)
	}
	oldQ, _, err := s.Quantiles(ctx, "env_1", []float64{0.5}, []string{"p50"}, yearAgo, time.Now(), 0)
	if err != nil || len(oldQ[0].Points) != 1 {
		t.Errorf("an hour of latency: %+v %v", oldQ, err)
	}
}

func TestPrune(t *testing.T) {
	s, db := testStore(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-20 * 24 * time.Hour).Truncate(time.Hour)
	recent := now.Add(-time.Hour).Truncate(time.Hour)
	if err := db.AppendMetrics(ctx, []store.MetricRow{
		{EnvID: "env_1", Name: CPU, Part: "web.1", Tier: tierMinute, Bucket: old.Unix(), Data: appendPoint(nil, 0, Point{Avg: 1})},
		{EnvID: "env_1", Name: CPU, Part: "web.1", Tier: tierMinute, Bucket: recent.Unix(), Data: appendPoint(nil, 0, Point{Avg: 2})},
	}); err != nil {
		t.Fatal(err)
	}
	s.Gauge("env_1", CPU, "quiet", 1, now.Add(-3*time.Hour))
	s.Gauge("env_1", CPU, "busy", 1, now)
	if err := s.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	rows, err := db.MetricRows(ctx, "env_1", CPU, tierMinute, 0, now.Unix())
	if err != nil || len(rows) != 1 || rows[0].Bucket != recent.Unix() {
		t.Errorf("what is left: %+v %v", rows, err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.series[seriesKey{"env_1", CPU, "quiet"}] != nil || s.series[seriesKey{"env_1", CPU, "busy"}] == nil {
		t.Errorf("samples: %+v", s.series)
	}
}

func TestRingIsBounded(t *testing.T) {
	r := &ring{}
	at := time.Now()
	for i := range ringSize + 10 {
		r.add(sample{at.Add(time.Duration(i) * time.Second), float32(i)})
	}
	if len(r.samples) != ringSize || r.samples[0].v != 10 {
		t.Errorf("%d, oldest %v", len(r.samples), r.samples[0].v)
	}
}

// TestSlowPrometheusClientHoldsNothing: a client that stops reading holds up neither the collector nor
// the console.
func TestSlowPrometheusClientHoldsNothing(t *testing.T) {
	s := New(nil, slog.New(slog.DiscardHandler))
	now := time.Now()
	s.Gauge("e1", CPU, "web.1", 1, now)
	s.Gauge("e1", Memory, "web.1", 1, now)
	reader, writer := io.Pipe()
	defer reader.Close()
	go s.WriteProm(writer, []PromEnv{{"a", "production", "e1"}}, false)
	if _, err := reader.Read(make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		s.Gauge("e1", CPU, "web.1", 2, time.Now())
		s.Glance("e2", time.Now())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a client that stopped reading is holding up the store")
	}
}
