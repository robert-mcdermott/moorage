// Package metrics keeps what environments and the server did, in tiers (spec/07-observability.md,
// "Metrics"): ten-second samples in memory for two hours, minutes in the database for two
// weeks, and hours for a year. Latencies are fixed log-scale histograms, which merge across time, so
// that percentiles stay right after a roll-up.
package metrics

import (
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robert-mcdermott/moorage/internal/store"
)

// Names of the series.
const (
	CPU        = "cpu"           // percent of one core, by process.index
	Memory     = "memory"        // bytes, by process.index
	Requests   = "http.requests" // a count, by status class
	BytesOut   = "http.bytes"    // a count
	Latency    = "http.latency"  // a histogram
	HostLoad   = "host.load"
	HostMemory = "host.memory" // bytes in use
	HostDisk   = "host.disk"   // percent of the data volume in use
)

const (
	// SampleEvery is how far apart samples are.
	SampleEvery = 10 * time.Second
	ringSize    = 720 // two hours of them

	tierMinute = 1
	tierHour   = 2

	keepMinutes = 14 * 24 * time.Hour
	keepHours   = 365 * 24 * time.Hour
)

// latencyBounds are the histogram's upper bounds, in milliseconds, each twice the one before.
// Anything longer goes in the last bucket.
var latencyBounds = func() []float64 {
	out := make([]float64, 17)
	v := 1.0
	for i := range out {
		out[i] = v
		v *= 2
	}
	return out
}()

// HistogramSize is how many buckets a histogram has.
var HistogramSize = len(latencyBounds) + 1

// Histogram counts latencies.
type Histogram []uint32

func bucketOf(ms float64) int {
	for i, b := range latencyBounds {
		if ms <= b {
			return i
		}
	}
	return len(latencyBounds)
}

// Quantile estimates a quantile of a histogram, in milliseconds, within its bucket.
func (h Histogram) Quantile(q float64) (float64, bool) {
	var total uint64
	for _, c := range h {
		total += uint64(c)
	}
	if total == 0 {
		return 0, false
	}
	rank := q * float64(total)
	var seen uint64
	for i, c := range h {
		if c == 0 {
			continue
		}
		if float64(seen)+float64(c) >= rank {
			low, high := 0.0, latencyBounds[len(latencyBounds)-1]
			if i > 0 {
				low = latencyBounds[i-1]
			}
			if i < len(latencyBounds) {
				high = latencyBounds[i]
			} else {
				return high, true
			}
			return low + (high-low)*(rank-float64(seen))/float64(c), true
		}
		seen += uint64(c)
	}
	return latencyBounds[len(latencyBounds)-1], true
}

func isCounter(name string) bool { return name == Requests || name == BytesOut }

// envCounters are what requests leave behind, without a lock.
type envCounters struct {
	classes  [6]atomic.Uint64 // since the last tick, by status/100
	bytes    atomic.Uint64
	latency  []atomic.Uint32
	inFlight atomic.Int64
	// Totals since the server started, for Prometheus.
	totals       [6]atomic.Uint64
	totalBytes   atomic.Uint64
	totalLatency []atomic.Uint64
	totalMillis  atomic.Uint64
}

func newEnvCounters() *envCounters {
	return &envCounters{latency: make([]atomic.Uint32, HistogramSize), totalLatency: make([]atomic.Uint64, HistogramSize)}
}

type sample struct {
	at time.Time
	v  float32
}

type histSample struct {
	at time.Time
	h  Histogram
}

// ring holds a series' samples, the oldest going first.
type ring struct {
	samples []sample
	hists   []histSample
}

func (r *ring) add(s sample) {
	if len(r.samples) >= ringSize {
		copy(r.samples, r.samples[1:])
		r.samples = r.samples[:ringSize-1]
	}
	r.samples = append(r.samples, s)
}

func (r *ring) addHist(s histSample) {
	if len(r.hists) >= ringSize {
		copy(r.hists, r.hists[1:])
		r.hists = r.hists[:ringSize-1]
	}
	r.hists = append(r.hists, s)
}

type seriesKey struct{ env, name, part string }

// Store keeps the metrics.
type Store struct {
	db  *store.DB
	log *slog.Logger

	counters sync.Map // of *envCounters, by environment ID

	mu      sync.RWMutex
	series  map[seriesKey]*ring
	flushed time.Time // the minute flushed last
	rolled  time.Time // the hour rolled up last
}

// New returns a store.
func New(db *store.DB, log *slog.Logger) *Store {
	return &Store{db: db, log: log, series: map[seriesKey]*ring{}}
}

func (s *Store) countersOf(envID string) *envCounters {
	if c, ok := s.counters.Load(envID); ok {
		return c.(*envCounters)
	}
	c, _ := s.counters.LoadOrStore(envID, newEnvCounters())
	return c.(*envCounters)
}

// Begin notes a request starting, and the function it returns notes it ending.
func (s *Store) Begin(envID string) func(status int, took time.Duration, bytes int64) {
	c := s.countersOf(envID)
	c.inFlight.Add(1)
	return func(status int, took time.Duration, bytes int64) {
		c.inFlight.Add(-1)
		class := status / 100
		if class < 1 || class > 5 {
			class = 0
		}
		c.classes[class].Add(1)
		c.totals[class].Add(1)
		if bytes > 0 {
			c.bytes.Add(uint64(bytes))
			c.totalBytes.Add(uint64(bytes))
		}
		if status == http.StatusSwitchingProtocols {
			return // a WebSocket's length says nothing of how fast anything answered
		}
		ms := float64(took) / float64(time.Millisecond)
		i := bucketOf(ms)
		c.latency[i].Add(1)
		c.totalLatency[i].Add(1)
		c.totalMillis.Add(uint64(ms))
	}
}

// Gauge records a sample of something that has a value at any moment.
func (s *Store) Gauge(envID, name, part string, v float64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ringOf(seriesKey{envID, name, part}).add(sample{at, float32(v)})
}

func (s *Store) ringOf(k seriesKey) *ring {
	r := s.series[k]
	if r == nil {
		r = &ring{}
		s.series[k] = r
	}
	return r
}

var classNames = [6]string{"other", "1xx", "2xx", "3xx", "4xx", "5xx"}

// Tick turns what requests left behind into samples. Counts of nothing are not kept, since a query
// takes what is not there for nothing.
func (s *Store) Tick(at time.Time) {
	s.counters.Range(func(key, value any) bool {
		envID, c := key.(string), value.(*envCounters)
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range c.classes {
			if n := c.classes[i].Swap(0); n > 0 {
				s.ringOf(seriesKey{envID, Requests, classNames[i]}).add(sample{at, float32(n)})
			}
		}
		if n := c.bytes.Swap(0); n > 0 {
			s.ringOf(seriesKey{envID, BytesOut, ""}).add(sample{at, float32(n)})
		}
		var h Histogram
		for i := range c.latency {
			if n := c.latency[i].Swap(0); n > 0 {
				if h == nil {
					h = make(Histogram, HistogramSize)
				}
				h[i] = n
			}
		}
		if h != nil {
			s.ringOf(seriesKey{envID, Latency, ""}).addHist(histSample{at, h})
		}
		return true
	})
}

// A record of a gauge or a count: its slot, then min, avg, max and last. A count has its sum in all four.
const pointSize = 1 + 4*4

func appendPoint(b []byte, slot int, p Point) []byte {
	b = append(b, byte(slot))
	for _, v := range []float64{p.Min, p.Avg, p.Max, p.Last} {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(float32(v)))
	}
	return b
}

func decodePoints(data []byte, fn func(slot int, p Point)) {
	for len(data) >= pointSize {
		f := func(i int) float64 {
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(data[1+4*i:])))
		}
		fn(int(data[0]), Point{Min: f(0), Avg: f(1), Max: f(2), Last: f(3)})
		data = data[pointSize:]
	}
}

func histRecordSize() int { return 1 + 4*HistogramSize }

func appendHist(b []byte, slot int, h Histogram) []byte {
	b = append(b, byte(slot))
	for _, c := range h {
		b = binary.LittleEndian.AppendUint32(b, c)
	}
	return b
}

func decodeHists(data []byte, fn func(slot int, h Histogram)) {
	size := histRecordSize()
	for len(data) >= size {
		h := make(Histogram, HistogramSize)
		for i := range h {
			h[i] = binary.LittleEndian.Uint32(data[1+4*i:])
		}
		fn(int(data[0]), h)
		data = data[size:]
	}
}

// Flush writes what happened in the whole minutes since the last flush, as one transaction, and
// rolls up an hour when one has ended.
func (s *Store) Flush(ctx context.Context, now time.Time) error {
	minute := now.Truncate(time.Minute)
	s.mu.Lock()
	from := s.flushed
	if from.IsZero() {
		from = minute.Add(-time.Minute)
	}
	if !minute.After(from) {
		s.mu.Unlock()
		return nil
	}
	var rows []store.MetricRow
	for at := from; at.Before(minute); at = at.Add(time.Minute) {
		rows = append(rows, s.minuteRows(at)...)
	}
	s.flushed = minute
	s.mu.Unlock()
	if err := s.db.AppendMetrics(ctx, rows); err != nil {
		return err
	}
	return s.rollDue(ctx, minute.Truncate(time.Hour))
}

const rolledSetting = "metrics_rolled_hour"

// rollDue rolls up the hours that have ended and have not been: a server that was down catches up,
// and none is done twice.
func (s *Store) rollDue(ctx context.Context, current time.Time) error {
	if s.rolled.IsZero() {
		var unix int64
		found, err := s.db.Setting(ctx, rolledSetting, &unix)
		if err != nil {
			return err
		}
		if found {
			s.rolled = time.Unix(unix, 0)
		} else {
			s.rolled = current.Add(-2 * time.Hour)
		}
		if oldest := current.Add(-keepMinutes); s.rolled.Before(oldest) {
			s.rolled = oldest
		}
	}
	for hour := s.rolled.Add(time.Hour); hour.Before(current); hour = hour.Add(time.Hour) {
		if err := s.rollHour(ctx, hour); err != nil {
			return err
		}
		if err := s.db.PutSetting(ctx, rolledSetting, hour.Unix(), "moorage"); err != nil {
			return err
		}
		s.rolled = hour
	}
	return nil
}

// minuteRows summarizes one minute of every series. It takes s.mu being held.
func (s *Store) minuteRows(at time.Time) []store.MetricRow {
	hour := at.Truncate(time.Hour)
	slot := int(at.Sub(hour) / time.Minute)
	end := at.Add(time.Minute)
	var rows []store.MetricRow
	for k, r := range s.series {
		if k.name == Latency {
			var merged Histogram
			for _, hs := range r.hists {
				if hs.at.Before(at) || !hs.at.Before(end) {
					continue
				}
				if merged == nil {
					merged = make(Histogram, HistogramSize)
				}
				for i, c := range hs.h {
					merged[i] += c
				}
			}
			if merged != nil {
				rows = append(rows, store.MetricRow{EnvID: k.env, Name: k.name, Part: k.part, Tier: tierMinute, Bucket: hour.Unix(), Data: appendHist(nil, slot, merged)})
			}
			continue
		}
		var p Point
		n := 0
		for _, sm := range r.samples {
			if sm.at.Before(at) || !sm.at.Before(end) {
				continue
			}
			v := float64(sm.v)
			if n == 0 || v < p.Min {
				p.Min = v
			}
			if n == 0 || v > p.Max {
				p.Max = v
			}
			p.Avg += v
			p.Last = v
			n++
		}
		if n == 0 {
			continue
		}
		if isCounter(k.name) {
			p.Min, p.Max, p.Last = p.Avg, p.Avg, p.Avg
		} else {
			p.Avg /= float64(n)
		}
		rows = append(rows, store.MetricRow{EnvID: k.env, Name: k.name, Part: k.part, Tier: tierMinute, Bucket: hour.Unix(), Data: appendPoint(nil, slot, p)})
	}
	return rows
}

// rollHour makes an hour's record of every series from its minutes.
func (s *Store) rollHour(ctx context.Context, hour time.Time) error {
	rows, err := s.db.MetricRowsOfBucket(ctx, tierMinute, hour.Unix())
	if err != nil {
		return err
	}
	day := hour.Truncate(24 * time.Hour)
	slot := int(hour.Sub(day) / time.Hour)
	out := make([]store.MetricRow, 0, len(rows))
	for _, r := range rows {
		row := store.MetricRow{EnvID: r.EnvID, Name: r.Name, Part: r.Part, Tier: tierHour, Bucket: day.Unix()}
		if r.Name == Latency {
			merged := make(Histogram, HistogramSize)
			decodeHists(r.Data, func(_ int, h Histogram) {
				for i, c := range h {
					merged[i] += c
				}
			})
			row.Data = appendHist(nil, slot, merged)
		} else {
			var p Point
			n := 0
			decodePoints(r.Data, func(_ int, m Point) {
				if n == 0 || m.Min < p.Min {
					p.Min = m.Min
				}
				if n == 0 || m.Max > p.Max {
					p.Max = m.Max
				}
				p.Avg += m.Avg
				p.Last = m.Last
				n++
			})
			if n == 0 {
				continue
			}
			if isCounter(r.Name) {
				p.Min, p.Max, p.Last = p.Avg, p.Avg, p.Avg
			} else {
				p.Avg /= float64(n)
			}
			row.Data = appendPoint(nil, slot, p)
		}
		out = append(out, row)
	}
	return s.db.AppendMetrics(ctx, out)
}

// Prune lets go of what has been kept long enough, and of the samples of series that have gone quiet.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	if err := s.db.PruneMetrics(ctx, tierMinute, now.Add(-keepMinutes).Truncate(time.Hour).Unix()); err != nil {
		return err
	}
	if err := s.db.PruneMetrics(ctx, tierHour, now.Add(-keepHours).Truncate(24*time.Hour).Unix()); err != nil {
		return err
	}
	cutoff := now.Add(-time.Duration(ringSize) * SampleEvery)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, r := range s.series {
		if k.name == Latency {
			if len(r.hists) == 0 || r.hists[len(r.hists)-1].at.Before(cutoff) {
				delete(s.series, k)
			}
		} else if len(r.samples) == 0 || r.samples[len(r.samples)-1].at.Before(cutoff) {
			delete(s.series, k)
		}
	}
	return nil
}

// Point is a series at a moment. Of a count, Avg is how many there were in that step.
type Point struct {
	At                  time.Time
	Min, Avg, Max, Last float64
}

// Series is a part of what a name measures: a process.index, or a status class.
type Series struct {
	Part   string
	Points []Point
}

// resolution chooses where a range is answered from, and how far apart its points are.
func resolution(from, to time.Time, now time.Time, maxPoints int) (tier int, step time.Duration) {
	span := to.Sub(from)
	switch {
	case now.Sub(from) <= time.Duration(ringSize)*SampleEvery:
		tier, step = 0, SampleEvery
	case now.Sub(from) <= keepMinutes:
		tier, step = tierMinute, time.Minute
	default:
		tier, step = tierHour, time.Hour
	}
	if maxPoints > 0 {
		for span/step > time.Duration(maxPoints) {
			step *= 2
		}
	}
	return tier, step
}

// Query reads a name's series over a range, with no more than about maxPoints points. A count is
// zero wherever nothing happened, and a gauge has a gap where nothing was running.
func (s *Store) Query(ctx context.Context, envID, name string, from, to time.Time, maxPoints int) ([]Series, time.Duration, error) {
	now := time.Now()
	tier, step := resolution(from, to, now, maxPoints)
	type acc struct {
		min, max, sum, last float64
		n                   int
		at                  time.Time
	}
	parts := map[string]map[int64]*acc{}
	add := func(part string, at time.Time, min, avg, max, last float64) {
		if at.Before(from) || at.After(to) {
			return
		}
		key := at.Truncate(step).Unix()
		buckets := parts[part]
		if buckets == nil {
			buckets = map[int64]*acc{}
			parts[part] = buckets
		}
		a := buckets[key]
		if a == nil {
			a = &acc{min: min, max: max}
			buckets[key] = a
		}
		if min < a.min {
			a.min = min
		}
		if max > a.max {
			a.max = max
		}
		a.sum += avg
		a.n++
		if !at.Before(a.at) {
			a.at, a.last = at, last
		}
	}
	switch tier {
	case 0:
		s.mu.RLock()
		for k, r := range s.series {
			if k.env != envID || k.name != name {
				continue
			}
			for _, sm := range r.samples {
				v := float64(sm.v)
				add(k.part, sm.at, v, v, v, v)
			}
		}
		s.mu.RUnlock()
	default:
		rows, err := s.rows(ctx, envID, name, tier, from, to)
		if err != nil {
			return nil, step, err
		}
		for _, r := range rows {
			base, unit := time.Unix(r.Bucket, 0), time.Minute
			if tier == tierHour {
				unit = time.Hour
			}
			decodePoints(r.Data, func(slot int, p Point) {
				add(r.Part, base.Add(time.Duration(slot)*unit), p.Min, p.Avg, p.Max, p.Last)
			})
		}
	}
	counter := isCounter(name)
	names := make([]string, 0, len(parts))
	for part := range parts {
		names = append(names, part)
	}
	sort.Strings(names)
	out := make([]Series, 0, len(names))
	for _, part := range names {
		ser := Series{Part: part}
		if counter {
			// The step that is still going on is left out: it would look like everything stopped.
			for at := from.Truncate(step); at.Before(to.Truncate(step)); at = at.Add(step) {
				p := Point{At: at}
				if a := parts[part][at.Unix()]; a != nil {
					p.Avg, p.Min, p.Max, p.Last = a.sum, a.sum, a.sum, a.sum
				}
				ser.Points = append(ser.Points, p)
			}
		} else {
			keys := make([]int64, 0, len(parts[part]))
			for k := range parts[part] {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			for _, k := range keys {
				a := parts[part][k]
				ser.Points = append(ser.Points, Point{At: time.Unix(k, 0), Min: a.min, Avg: a.sum / float64(a.n), Max: a.max, Last: a.last})
			}
		}
		out = append(out, ser)
	}
	return out, step, nil
}

func (s *Store) rows(ctx context.Context, envID, name string, tier int, from, to time.Time) ([]store.MetricRow, error) {
	unit := time.Hour
	if tier == tierHour {
		unit = 24 * time.Hour
	}
	return s.db.MetricRows(ctx, envID, name, tier, from.Truncate(unit).Unix(), to.Unix())
}

// Quantiles reads latency's quantiles over a range, in milliseconds, each quantile as a Series
// named as a percentile, such as p95. Nothing is there where nothing happened.
func (s *Store) Quantiles(ctx context.Context, envID string, qs []float64, names []string, from, to time.Time, maxPoints int) ([]Series, time.Duration, error) {
	now := time.Now()
	tier, step := resolution(from, to, now, maxPoints)
	buckets := map[int64]Histogram{}
	add := func(at time.Time, h Histogram) {
		if at.Before(from) || at.After(to) {
			return
		}
		key := at.Truncate(step).Unix()
		merged := buckets[key]
		if merged == nil {
			merged = make(Histogram, HistogramSize)
			buckets[key] = merged
		}
		for i, c := range h {
			merged[i] += c
		}
	}
	if tier == 0 {
		s.mu.RLock()
		if r := s.series[seriesKey{envID, Latency, ""}]; r != nil {
			for _, hs := range r.hists {
				add(hs.at, hs.h)
			}
		}
		s.mu.RUnlock()
	} else {
		rows, err := s.rows(ctx, envID, Latency, tier, from, to)
		if err != nil {
			return nil, step, err
		}
		for _, r := range rows {
			base, unit := time.Unix(r.Bucket, 0), time.Minute
			if tier == tierHour {
				unit = time.Hour
			}
			decodeHists(r.Data, func(slot int, h Histogram) { add(base.Add(time.Duration(slot)*unit), h) })
		}
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Series, len(qs))
	for i := range qs {
		out[i].Part = names[i]
		for _, k := range keys {
			if v, ok := buckets[k].Quantile(qs[i]); ok {
				out[i].Points = append(out[i].Points, Point{At: time.Unix(k, 0), Min: v, Avg: v, Max: v, Last: v})
			}
		}
	}
	return out, step, nil
}

// Totals are what Prometheus reads of an environment: counts since the server started.
type Totals struct {
	Classes  map[string]uint64
	Bytes    uint64
	InFlight int64
	// Buckets is latency, by histogram bucket, and SumSeconds all of it added up.
	Buckets    []uint64
	SumSeconds float64
}

// TotalsOf reads an environment's totals, if it has had any requests since the server started.
func (s *Store) TotalsOf(envID string) (Totals, bool) {
	v, ok := s.counters.Load(envID)
	if !ok {
		return Totals{}, false
	}
	c := v.(*envCounters)
	t := Totals{Classes: map[string]uint64{}, Bytes: c.totalBytes.Load(), InFlight: c.inFlight.Load(), SumSeconds: float64(c.totalMillis.Load()) / 1000}
	for i := range c.totals {
		if n := c.totals[i].Load(); n > 0 {
			t.Classes[classNames[i]] = n
		}
	}
	for i := range c.totalLatency {
		t.Buckets = append(t.Buckets, c.totalLatency[i].Load())
	}
	return t, true
}

// BoundsSeconds are the histogram's upper bounds, in seconds.
func BoundsSeconds() []float64 {
	out := make([]float64, len(latencyBounds))
	for i, b := range latencyBounds {
		out[i] = b / 1000
	}
	return out
}
