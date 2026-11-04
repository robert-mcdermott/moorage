package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// PromEnv is an environment to expose.
type PromEnv struct{ App, Env, EnvID string }

// escapeLabel makes a value fit to go between quotes.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// WriteProm writes the store as Prometheus's text format says: counts since the server started, and
// the last samples. host says whether to include the server itself.
func (s *Store) WriteProm(w io.Writer, envs []PromEnv, host bool) {
	sort.Slice(envs, func(i, j int) bool { return envs[i].App+"/"+envs[i].Env < envs[j].App+"/"+envs[j].Env })
	labels := func(e PromEnv) string {
		return `app="` + escapeLabel(e.App) + `",env="` + escapeLabel(e.Env) + `"`
	}
	fmt.Fprintln(w, "# HELP moorage_http_requests_total Requests answered, by status class.")
	fmt.Fprintln(w, "# TYPE moorage_http_requests_total counter")
	for _, e := range envs {
		t, ok := s.TotalsOf(e.EnvID)
		if !ok {
			continue
		}
		classes := make([]string, 0, len(t.Classes))
		for c := range t.Classes {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		for _, c := range classes {
			fmt.Fprintf(w, "moorage_http_requests_total{%s,class=\"%s\"} %d\n", labels(e), c, t.Classes[c])
		}
	}
	fmt.Fprintln(w, "# HELP moorage_http_response_bytes_total Bytes sent in answers.")
	fmt.Fprintln(w, "# TYPE moorage_http_response_bytes_total counter")
	for _, e := range envs {
		if t, ok := s.TotalsOf(e.EnvID); ok {
			fmt.Fprintf(w, "moorage_http_response_bytes_total{%s} %d\n", labels(e), t.Bytes)
		}
	}
	fmt.Fprintln(w, "# HELP moorage_http_requests_in_flight Requests going on now.")
	fmt.Fprintln(w, "# TYPE moorage_http_requests_in_flight gauge")
	for _, e := range envs {
		if t, ok := s.TotalsOf(e.EnvID); ok {
			fmt.Fprintf(w, "moorage_http_requests_in_flight{%s} %d\n", labels(e), t.InFlight)
		}
	}
	fmt.Fprintln(w, "# HELP moorage_http_request_duration_seconds How long requests took.")
	fmt.Fprintln(w, "# TYPE moorage_http_request_duration_seconds histogram")
	bounds := BoundsSeconds()
	for _, e := range envs {
		t, ok := s.TotalsOf(e.EnvID)
		if !ok {
			continue
		}
		var running uint64
		for i, c := range t.Buckets {
			running += c
			le := "+Inf"
			if i < len(bounds) {
				le = strconv.FormatFloat(bounds[i], 'f', -1, 64)
			}
			fmt.Fprintf(w, "moorage_http_request_duration_seconds_bucket{%s,le=\"%s\"} %d\n", labels(e), le, running)
		}
		fmt.Fprintf(w, "moorage_http_request_duration_seconds_sum{%s} %g\n", labels(e), t.SumSeconds)
		fmt.Fprintf(w, "moorage_http_request_duration_seconds_count{%s} %d\n", labels(e), running)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	gauge := func(metric, help, name string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", metric, help, metric)
		for _, e := range envs {
			parts := []string{}
			for k := range s.series {
				if k.env == e.EnvID && k.name == name {
					parts = append(parts, k.part)
				}
			}
			sort.Strings(parts)
			for _, part := range parts {
				r := s.series[seriesKey{e.EnvID, name, part}]
				if len(r.samples) == 0 {
					continue
				}
				last := r.samples[len(r.samples)-1]
				fmt.Fprintf(w, "%s{%s,instance_name=\"%s\",process=\"%s\"} %g\n", metric, labels(e), escapeLabel(part), escapeLabel(ProcessOf(part)), last.v)
			}
		}
	}
	gauge("moorage_instance_cpu_percent", "CPU an instance is using, in percent of one core.", CPU)
	gauge("moorage_instance_memory_bytes", "Memory an instance is using.", Memory)
	if !host {
		return
	}
	for _, m := range []struct{ metric, help, name string }{
		{"moorage_host_load", "The server's load average.", HostLoad},
		{"moorage_host_memory_used_bytes", "Memory in use on the server.", HostMemory},
		{"moorage_host_disk_used_percent", "How full the data volume is.", HostDisk},
	} {
		r := s.series[seriesKey{"", m.name, ""}]
		if r == nil || len(r.samples) == 0 {
			continue
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", m.metric, m.help, m.metric, m.metric, r.samples[len(r.samples)-1].v)
	}
}
