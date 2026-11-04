package metrics

import (
	"context"
	"sort"
	"time"
)

// A chart of the view is one measure, on one axis: what it is, what its numbers are, and its lines.
type Chart struct {
	Key, Title string
	// Unit is rate, ms, percent or bytes.
	Unit  string
	Lines []Line
}

// Line is a line of a chart. A gap is where there is no point.
type Line struct {
	Name   string
	Points []Point
}

// MaxLines is how many lines a chart has: with more, instances fold into their processes.
const MaxLines = 8

// View reads what an environment's page shows, over a range: requests a minute, in all and those
// the app failed, how long they took, and what its instances used.
func (s *Store) View(ctx context.Context, envID string, from, to time.Time, maxPoints int) ([]Chart, time.Duration, error) {
	requests, step, err := s.Query(ctx, envID, Requests, from, to, maxPoints)
	if err != nil {
		return nil, step, err
	}
	perMinute := time.Minute.Seconds() / step.Seconds()
	var all, failed []Point
	for _, ser := range requests {
		for i, p := range ser.Points {
			if i >= len(all) {
				all = append(all, Point{At: p.At})
				failed = append(failed, Point{At: p.At})
			}
			all[i].Avg += p.Avg * perMinute
			if ser.Part == "5xx" {
				failed[i].Avg += p.Avg * perMinute
			}
		}
	}
	reqLines := []Line{}
	if len(all) > 0 {
		reqLines = append(reqLines, Line{Name: "All", Points: all}, Line{Name: "Failed (5xx)", Points: failed})
	}
	latency, _, err := s.Quantiles(ctx, envID, []float64{0.5, 0.95}, []string{"p50", "p95"}, from, to, maxPoints)
	if err != nil {
		return nil, step, err
	}
	latLines := []Line{}
	for _, ser := range latency {
		if len(ser.Points) > 0 {
			latLines = append(latLines, Line{Name: ser.Part, Points: ser.Points})
		}
	}
	cpu, _, err := s.Query(ctx, envID, CPU, from, to, maxPoints)
	if err != nil {
		return nil, step, err
	}
	memory, _, err := s.Query(ctx, envID, Memory, from, to, maxPoints)
	if err != nil {
		return nil, step, err
	}
	return []Chart{
		{Key: "requests", Title: "Requests a minute", Unit: "rate", Lines: reqLines},
		{Key: "latency", Title: "Response time", Unit: "ms", Lines: latLines},
		{Key: "cpu", Title: "CPU", Unit: "percent", Lines: foldLines(cpu)},
		{Key: "memory", Title: "Memory", Unit: "bytes", Lines: foldLines(memory)},
	}, step, nil
}

// foldLines has a line for each instance, or if there would be too many, for each process, added up.
func foldLines(list []Series) []Line {
	if len(list) <= MaxLines {
		out := make([]Line, 0, len(list))
		for _, ser := range list {
			out = append(out, Line{Name: ser.Part, Points: ser.Points})
		}
		return out
	}
	byProcess := map[string]map[int64]*Point{}
	for _, ser := range list {
		name := ProcessOf(ser.Part)
		if byProcess[name] == nil {
			byProcess[name] = map[int64]*Point{}
		}
		for _, p := range ser.Points {
			key := p.At.Unix()
			sum := byProcess[name][key]
			if sum == nil {
				sum = &Point{At: p.At}
				byProcess[name][key] = sum
			}
			sum.Min += p.Min
			sum.Avg += p.Avg
			sum.Max += p.Max
			sum.Last += p.Last
		}
	}
	names := make([]string, 0, len(byProcess))
	for name := range byProcess {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Line, 0, len(names))
	for _, name := range names {
		line := Line{Name: name}
		for _, p := range byProcess[name] {
			line.Points = append(line.Points, *p)
		}
		sort.Slice(line.Points, func(i, j int) bool { return line.Points[i].At.Before(line.Points[j].At) })
		out = append(out, line)
	}
	return out
}

// Now is what an environment is doing lately, for a glance: the last values of a view's charts, and
// little lines of the last hour.
type Now struct {
	RequestsPerMinute float64
	P95               float64
	HasP95            bool
	CPU               float64
	Memory            float64
	HasUsage          bool
	Requests, Latency []float64
	CPULine, MemLine  []float64
}

// Glance reads the last hour of an environment, from memory alone, so that a list of apps is cheap.
func (s *Store) Glance(envID string, now time.Time) Now {
	from := now.Add(-time.Hour)
	const points = 60
	step := time.Minute
	out := Now{Requests: make([]float64, points)}
	index := func(at time.Time) int {
		i := int(at.Sub(from) / step)
		if i < 0 || i >= points {
			return -1
		}
		return i
	}
	cpu, mem := make([]float64, points), make([]float64, points)
	cpuN, memN := make([]int, points), make([]int, points)
	merged := make([]Histogram, points)
	var usageAt time.Time
	s.mu.RLock()
	for k, r := range s.series {
		if k.env != envID {
			continue
		}
		switch k.name {
		case Requests:
			for _, sm := range r.samples {
				if i := index(sm.at); i >= 0 {
					out.Requests[i] += float64(sm.v)
				}
			}
		case Latency:
			for _, hs := range r.hists {
				if i := index(hs.at); i >= 0 {
					if merged[i] == nil {
						merged[i] = make(Histogram, HistogramSize)
					}
					for b, c := range hs.h {
						merged[i][b] += c
					}
				}
			}
		case CPU, Memory:
			for _, sm := range r.samples {
				i := index(sm.at)
				if i < 0 {
					continue
				}
				if k.name == CPU {
					cpu[i] += float64(sm.v)
					cpuN[i]++
				} else {
					mem[i] += float64(sm.v)
					memN[i]++
				}
				if sm.at.After(usageAt) {
					usageAt = sm.at
				}
			}
		}
	}
	s.mu.RUnlock()
	// How many instances each minute had samples from, roughly: six samples a minute.
	perMinute := float64(time.Minute / SampleEvery)
	for i := range cpu {
		if cpuN[i] > 0 {
			cpu[i] /= perMinute
		}
		if memN[i] > 0 {
			mem[i] /= perMinute
		}
	}
	out.CPULine, out.MemLine = cpu, mem
	out.Latency = make([]float64, points)
	var total Histogram
	for i, h := range merged {
		if h == nil {
			continue
		}
		if v, ok := h.Quantile(0.95); ok {
			out.Latency[i] = v
		}
		if i >= points-5 {
			if total == nil {
				total = make(Histogram, HistogramSize)
			}
			for b, c := range h {
				total[b] += c
			}
		}
	}
	if total != nil {
		out.P95, out.HasP95 = total.Quantile(0.95)
	}
	sum := 0.0
	for _, v := range out.Requests[points-5:] {
		sum += v
	}
	out.RequestsPerMinute = sum / 5
	if !usageAt.IsZero() && now.Sub(usageAt) < 2*time.Minute {
		out.HasUsage = true
		for i := points - 1; i >= 0; i-- {
			if memN[i] > 0 {
				out.Memory = mem[i]
				out.CPU = cpu[i]
				break
			}
		}
	}
	return out
}
