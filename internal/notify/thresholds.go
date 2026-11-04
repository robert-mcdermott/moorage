package notify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/events"
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// What a threshold can be about.
const (
	ThresholdErrorRate = "error-rate"
	ThresholdLatency   = "latency-p95"
	ThresholdMemory    = "memory"
	ThresholdCPU       = "cpu"
)

// MaxThresholdMinutes is the longest that something can be over a threshold before it is told of.
const MaxThresholdMinutes = 24 * 60

// fewRequests is too few to call it an error rate: one error in two requests is not a sign of much.
const fewRequests = 5

// ThresholdKinds lists what a threshold can be about, with what its number means.
var ThresholdKinds = []struct{ Metric, Label, Unit string }{
	{ThresholdErrorRate, "Error rate", "% of answers"},
	{ThresholdLatency, "Latency (95th percentile)", "ms"},
	{ThresholdMemory, "Memory", "% of its limit"},
	{ThresholdCPU, "CPU", "% of one core"},
}

// ThresholdLabel says what a threshold is about, in words.
func ThresholdLabel(metric string) string {
	for _, k := range ThresholdKinds {
		if k.Metric == metric {
			return k.Label
		}
	}
	return metric
}

// ThresholdCheck says what is wrong with a threshold, if anything.
func ThresholdCheck(metric string, above float64, minutes int) string {
	known := false
	for _, k := range ThresholdKinds {
		known = known || k.Metric == metric
	}
	switch {
	case !known:
		names := make([]string, 0, len(ThresholdKinds))
		for _, k := range ThresholdKinds {
			names = append(names, k.Metric)
		}
		return "A threshold is about one of " + strings.Join(names, ", ") + "."
	case above <= 0:
		return "A threshold is a number above nothing."
	case (metric == ThresholdErrorRate || metric == ThresholdMemory) && above > 100:
		return "That is a percentage, of 100 at the most."
	case minutes < 1 || minutes > MaxThresholdMinutes:
		return fmt.Sprintf("It has to go on for at least a minute, and at most %d.", MaxThresholdMinutes)
	}
	return ""
}

// thresholdState is how a threshold stands.
type thresholdState struct {
	// since is when it went over, if it is over.
	since  time.Time
	firing bool
	value  float64
	known  bool
}

// ThresholdStatus says how a threshold stands: the last value it was compared with, if there was one.
func (s *Service) ThresholdStatus(id string) (value float64, known, firing bool) {
	s.thresholdMu.Lock()
	defer s.thresholdMu.Unlock()
	st := s.thresholdStates[id]
	if st == nil {
		return 0, false, false
	}
	return st.value, st.known, st.firing
}

func (s *Service) watchThresholds(ctx context.Context) {
	if s.Metrics == nil {
		return
	}
	every := s.ThresholdTick
	if every == 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.checkThresholds(ctx, now.UTC(), every)
		}
	}
}

// valueOf says what a threshold is to be compared with. Nothing to compare is not a problem: an app that is not being
// asked anything has no error rate, and one with no limit is not near it.
func valueOf(metric string, recent metrics.Recent, limits map[string]int64) (float64, bool) {
	highest := func(values map[string]float64, of func(part string, v float64) (float64, bool)) (float64, bool) {
		top, found := 0.0, false
		for part, v := range values {
			if value, ok := of(part, v); ok && (!found || value > top) {
				top, found = value, true
			}
		}
		return top, found
	}
	switch metric {
	case ThresholdErrorRate:
		if recent.Requests < fewRequests {
			return 0, false
		}
		return 100 * recent.Errors / recent.Requests, true
	case ThresholdLatency:
		return recent.P95, recent.HasP95
	case ThresholdCPU:
		return highest(recent.CPU, func(_ string, v float64) (float64, bool) { return v, true })
	case ThresholdMemory:
		return highest(recent.Memory, func(part string, v float64) (float64, bool) {
			limit := limits[part]
			if limit <= 0 {
				return 0, false
			}
			return 100 * v / float64(limit), true
		})
	}
	return 0, false
}

// checkThresholds compares each threshold with the last while, and tells of one that has been crossed for as long as it
// says, and then of its coming right. A minute that says nothing, with no requests in it, counts for neither.
func (s *Service) checkThresholds(ctx context.Context, now time.Time, every time.Duration) {
	rows, err := s.DB.AllAlertThresholds(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Warn("read the thresholds", "err", err)
		}
		return
	}
	limits := map[string]map[string]int64{}
	if s.Instances != nil {
		for _, in := range s.Instances(ctx) {
			if limits[in.EnvID] == nil {
				limits[in.EnvID] = map[string]int64{}
			}
			limits[in.EnvID][in.Part] = in.MemoryLimit
		}
	}
	minute := s.ThresholdMinute
	if minute == 0 {
		minute = time.Minute
	}
	since := now.Add(-max(every, minute))
	keep := make(map[string]bool, len(rows))
	for _, t := range rows {
		keep[t.ID] = true
		value, known := valueOf(t.Metric, s.Metrics.RecentOf(t.EnvID, since), limits[t.EnvID])
		s.thresholdMu.Lock()
		if s.thresholdStates == nil {
			s.thresholdStates = map[string]*thresholdState{}
		}
		st := s.thresholdStates[t.ID]
		if st == nil {
			st = &thresholdState{}
			s.thresholdStates[t.ID] = st
		}
		st.value, st.known = value, known
		var crossed, cleared bool
		if known {
			if value > t.Above {
				if st.since.IsZero() {
					st.since = now
				}
				crossed = !st.firing && now.Sub(st.since)+every >= time.Duration(t.Minutes)*minute
				if crossed {
					st.firing = true
				}
			} else {
				st.since = time.Time{}
				cleared = st.firing
				st.firing = false
			}
		}
		s.thresholdMu.Unlock()
		data := map[string]any{"threshold": t.ID, "metric": t.Metric, "value": roundTo(value), "above": t.Above, "minutes": t.Minutes}
		switch {
		case crossed:
			s.Bus.Publish(ctx, store.Event{Type: events.ThresholdCrossed, App: t.App, Env: t.Env, At: now, Data: data})
		case cleared:
			s.Bus.Publish(ctx, store.Event{Type: events.ThresholdOK, App: t.App, Env: t.Env, At: now, Data: data})
		}
	}
	s.thresholdMu.Lock()
	for id := range s.thresholdStates {
		if !keep[id] {
			delete(s.thresholdStates, id)
		}
	}
	s.thresholdMu.Unlock()
}

func roundTo(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
