package metrics

import (
	"sort"
	"time"
)

// Export is the store as a push says it, with the same things in it as WriteProm.
type Export struct {
	Counters   []ExportValue
	Gauges     []ExportValue
	Histograms []ExportHistogram
}

// ExportValue is one number, with what it measures and what it is about.
type ExportValue struct {
	Name, Unit, Description string
	Labels                  map[string]string
	Value                   float64
}

// ExportHistogram is how long things took.
type ExportHistogram struct {
	Name, Unit, Description string
	Labels                  map[string]string
	Bounds                  []float64
	Counts                  []uint64
	Sum                     float64
	Count                   uint64
}

// ExportAll says what the store has now. host says whether to include the server itself.
func (s *Store) ExportAll(envs []PromEnv, host bool) Export {
	var out Export
	sort.Slice(envs, func(i, j int) bool { return envs[i].App+"/"+envs[i].Env < envs[j].App+"/"+envs[j].Env })
	bounds := BoundsSeconds()
	for _, e := range envs {
		about := func(more ...string) map[string]string {
			labels := map[string]string{"app": e.App, "env": e.Env}
			for i := 0; i+1 < len(more); i += 2 {
				labels[more[i]] = more[i+1]
			}
			return labels
		}
		if t, ok := s.TotalsOf(e.EnvID); ok {
			classes := make([]string, 0, len(t.Classes))
			for c := range t.Classes {
				classes = append(classes, c)
			}
			sort.Strings(classes)
			for _, c := range classes {
				out.Counters = append(out.Counters, ExportValue{"moorage.http.requests", "{request}", "Requests answered, by status class.", about("class", c), float64(t.Classes[c])})
			}
			out.Counters = append(out.Counters, ExportValue{"moorage.http.response.bytes", "By", "Bytes sent in answers.", about(), float64(t.Bytes)})
			out.Gauges = append(out.Gauges, ExportValue{"moorage.http.requests.in_flight", "{request}", "Requests going on now.", about(), float64(t.InFlight)})
			h := ExportHistogram{Name: "moorage.http.request.duration", Unit: "s", Description: "How long requests took.", Labels: about(), Bounds: bounds, Sum: t.SumSeconds}
			for _, c := range t.Buckets {
				h.Counts = append(h.Counts, c)
				h.Count += c
			}
			out.Histograms = append(out.Histograms, h)
		}
		s.mu.RLock()
		for _, m := range []struct{ metric, unit, help, name string }{
			{"moorage.instance.cpu", "%", "CPU an instance is using, in percent of one core.", CPU},
			{"moorage.instance.memory", "By", "Memory an instance is using.", Memory},
		} {
			parts := []string{}
			for k := range s.series {
				if k.env == e.EnvID && k.name == m.name {
					parts = append(parts, k.part)
				}
			}
			sort.Strings(parts)
			for _, part := range parts {
				r := s.series[seriesKey{e.EnvID, m.name, part}]
				if len(r.samples) > 0 {
					out.Gauges = append(out.Gauges, ExportValue{m.metric, m.unit, m.help, about("instance_name", part, "process", ProcessOf(part)), float64(r.samples[len(r.samples)-1].v)})
				}
			}
		}
		s.mu.RUnlock()
	}
	if host {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, m := range []struct{ metric, unit, help, name string }{
			{"moorage.host.load", "1", "The server's load average.", HostLoad},
			{"moorage.host.memory.used", "By", "Memory in use on the server.", HostMemory},
			{"moorage.host.disk.used", "%", "How full the data volume is.", HostDisk},
		} {
			if r := s.series[seriesKey{"", m.name, ""}]; r != nil && len(r.samples) > 0 {
				out.Gauges = append(out.Gauges, ExportValue{m.metric, m.unit, m.help, map[string]string{}, float64(r.samples[len(r.samples)-1].v)})
			}
		}
	}
	return out
}

// Recent says what an environment has been doing in the last while, for deciding whether to tell someone.
type Recent struct {
	// Requests and Errors are how many were answered, and how many of those with a 5xx.
	Requests, Errors float64
	// P95 is how long, in milliseconds, 95 in 100 of them took at the most, if there were any.
	P95    float64
	HasP95 bool
	// CPU and Memory are the last sample of each instance, by process.index.
	CPU, Memory map[string]float64
}

// RecentOf reads what an environment has been doing since a moment ago.
func (s *Store) RecentOf(envID string, since time.Time) Recent {
	out := Recent{CPU: map[string]float64{}, Memory: map[string]float64{}}
	s.mu.RLock()
	defer s.mu.RUnlock()
	merged := make(Histogram, HistogramSize)
	for k, r := range s.series {
		if k.env != envID {
			continue
		}
		switch k.name {
		case Requests:
			for _, sample := range r.samples {
				if sample.at.After(since) {
					out.Requests += float64(sample.v)
					if k.part == "5xx" {
						out.Errors += float64(sample.v)
					}
				}
			}
		case Latency:
			for _, h := range r.hists {
				if h.at.After(since) {
					for i, c := range h.h {
						merged[i] += c
					}
				}
			}
		case CPU, Memory:
			if n := len(r.samples); n > 0 && r.samples[n-1].at.After(since) {
				if k.name == CPU {
					out.CPU[k.part] = float64(r.samples[n-1].v)
				} else {
					out.Memory[k.part] = float64(r.samples[n-1].v)
				}
			}
		}
	}
	out.P95, out.HasP95 = merged.Quantile(0.95)
	return out
}
