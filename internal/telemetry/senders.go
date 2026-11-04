package telemetry

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
)

// The kinds of sink.
const (
	KindSyslog = "syslog"
	KindOTLP   = "otlp"
	KindHTTP   = "http"
)

// Line is one line that an app said.
type Line struct {
	Time              time.Time
	App, Env          string
	Process, Instance string
	Stream, Text      string
}

// sender gets things to one sink.
type sender interface {
	sendLogs(ctx context.Context, lines []Line) error
	sendMetrics(ctx context.Context, e metrics.Export, started, now time.Time) error
	close()
}

// postJSON sends something, and says what went wrong. It leaves out the address and anything else that could have a secret in.
func postJSON(ctx context.Context, client *http.Client, address string, headers map[string]string, doc any) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "moorage")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := client.Do(req)
	if err != nil {
		var plain *url.Error
		if errors.As(err, &plain) {
			return plain.Err
		}
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("it answered %s", res.Status)
	}
	return nil
}

// httpSender sends what apps say as JSON, in batches.
type httpSender struct {
	address string
	headers map[string]string
	client  *http.Client
	server  string
}

type httpLine struct {
	Time     time.Time `json:"time"`
	App      string    `json:"app"`
	Env      string    `json:"env"`
	Process  string    `json:"process"`
	Instance string    `json:"instance"`
	Stream   string    `json:"stream"`
	Text     string    `json:"text"`
}

func (h *httpSender) sendLogs(ctx context.Context, lines []Line) error {
	out := make([]httpLine, len(lines))
	for i, l := range lines {
		out[i] = httpLine{l.Time, l.App, l.Env, l.Process, l.Instance, l.Stream, l.Text}
	}
	return postJSON(ctx, h.client, h.address, h.headers, map[string]any{"server": h.server, "lines": out})
}

func (h *httpSender) sendMetrics(context.Context, metrics.Export, time.Time, time.Time) error {
	return nil
}

func (h *httpSender) close() {}

// otlpSender speaks OTLP over HTTP, as JSON, which every collector has to take.
type otlpSender struct {
	base    string
	headers map[string]string
	client  *http.Client
	server  string
	version string
}

type otlpValue struct {
	StringValue string `json:"stringValue"`
}

type otlpAttribute struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

func attributes(pairs ...string) []otlpAttribute {
	out := make([]otlpAttribute, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			out = append(out, otlpAttribute{pairs[i], otlpValue{pairs[i+1]}})
		}
	}
	return out
}

func labelsOf(labels map[string]string) []otlpAttribute {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]otlpAttribute, 0, len(keys))
	for _, k := range keys {
		out = append(out, otlpAttribute{k, otlpValue{labels[k]}})
	}
	return out
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func (o *otlpSender) sendLogs(ctx context.Context, lines []Line) error {
	type resourceKey struct{ app, env, process, instance string }
	type logRecord struct {
		TimeUnixNano string          `json:"timeUnixNano"`
		Body         otlpValue       `json:"body"`
		Attributes   []otlpAttribute `json:"attributes"`
	}
	order := []resourceKey{}
	records := map[resourceKey][]logRecord{}
	for _, l := range lines {
		key := resourceKey{l.App, l.Env, l.Process, l.Instance}
		if _, seen := records[key]; !seen {
			order = append(order, key)
		}
		stream := "stdout"
		if l.Stream == "E" {
			stream = "stderr"
		}
		records[key] = append(records[key], logRecord{nanos(l.Time), otlpValue{l.Text}, attributes("log.iostream", stream)})
	}
	var resources []map[string]any
	for _, key := range order {
		resources = append(resources, map[string]any{
			"resource": map[string]any{"attributes": attributes("service.name", key.app, "deployment.environment.name", key.env, "service.instance.id", key.instance, "moorage.process", key.process, "host.name", o.server)},
			"scopeLogs": []map[string]any{{
				"scope":      map[string]any{"name": "moorage", "version": o.version},
				"logRecords": records[key],
			}},
		})
	}
	return postJSON(ctx, o.client, o.base+"/v1/logs", o.headers, map[string]any{"resourceLogs": resources})
}

func (o *otlpSender) sendMetrics(ctx context.Context, e metrics.Export, started, now time.Time) error {
	byName := map[string]map[string]any{}
	var order []string
	point := func(name, unit, description, kind string, extra map[string]any, dataPoint map[string]any) {
		m, seen := byName[name]
		if !seen {
			detail := map[string]any{"dataPoints": []map[string]any{}}
			for k, v := range extra {
				detail[k] = v
			}
			m = map[string]any{"name": name, "unit": unit, "description": description, kind: detail}
			byName[name] = m
			order = append(order, name)
		}
		detail := m[kind].(map[string]any)
		detail["dataPoints"] = append(detail["dataPoints"].([]map[string]any), dataPoint)
	}
	const cumulative = 2
	for _, c := range e.Counters {
		point(c.Name, c.Unit, c.Description, "sum", map[string]any{"aggregationTemporality": cumulative, "isMonotonic": true}, map[string]any{
			"startTimeUnixNano": nanos(started), "timeUnixNano": nanos(now), "asInt": strconv.FormatUint(uint64(c.Value), 10), "attributes": labelsOf(c.Labels),
		})
	}
	for _, g := range e.Gauges {
		point(g.Name, g.Unit, g.Description, "gauge", nil, map[string]any{"timeUnixNano": nanos(now), "asDouble": g.Value, "attributes": labelsOf(g.Labels)})
	}
	for _, h := range e.Histograms {
		counts := make([]string, len(h.Counts))
		for i, c := range h.Counts {
			counts[i] = strconv.FormatUint(c, 10)
		}
		point(h.Name, h.Unit, h.Description, "histogram", map[string]any{"aggregationTemporality": cumulative}, map[string]any{
			"startTimeUnixNano": nanos(started), "timeUnixNano": nanos(now), "count": strconv.FormatUint(h.Count, 10), "sum": h.Sum,
			"bucketCounts": counts, "explicitBounds": h.Bounds, "attributes": labelsOf(h.Labels),
		})
	}
	if len(order) == 0 {
		return nil
	}
	list := make([]map[string]any, 0, len(order))
	for _, name := range order {
		list = append(list, byName[name])
	}
	return postJSON(ctx, o.client, o.base+"/v1/metrics", o.headers, map[string]any{"resourceMetrics": []map[string]any{{
		"resource":     map[string]any{"attributes": attributes("service.name", "moorage", "host.name", o.server)},
		"scopeMetrics": []map[string]any{{"scope": map[string]any{"name": "moorage", "version": o.version}, "metrics": list}},
	}}})
}

func (o *otlpSender) close() {}

// syslogSender writes RFC 5424 messages: over UDP one to a datagram, and over TCP and TLS counted by RFC 6587.
type syslogSender struct {
	network, address string
	useTLS           bool
	dial             func(ctx context.Context, network, address string) (net.Conn, error)
	server           string
	conn             net.Conn
}

// Syslog's facility for what is not the system's (local0), with the severities for informational and error.
const (
	syslogInfo  = 16*8 + 6
	syslogError = 16*8 + 3
	// maxSyslogDatagram is what a datagram can take with no fear of being cut up on the way.
	maxSyslogDatagram = 1400
	// maxSyslogLine is what a stream can take of a line, which is where what is long gets cut.
	maxSyslogLine = 64 << 10
)

// nilValue is what RFC 5424 has in place of a field with nothing in it. Its fields are printable ASCII, with no spaces.
func syslogField(s string, limit int) string {
	if s == "" {
		return "-"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(out) < limit; i++ {
		if c := s[i]; c > 32 && c < 127 {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

func (s *syslogSender) format(l Line) []byte {
	pri := syslogInfo
	if l.Stream == "E" {
		pri = syslogError
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "<%d>1 %s %s %s %s %s - ", pri, l.Time.UTC().Format("2006-01-02T15:04:05.000000Z"),
		syslogField(s.server, 255), syslogField(l.App, 48), syslogField(l.Instance, 128), syslogField(l.Env, 32))
	b.WriteString(strings.ToValidUTF8(l.Text, "�"))
	limit := maxSyslogLine
	if s.network == "udp" {
		limit = maxSyslogDatagram
	}
	if b.Len() > limit {
		b.Truncate(limit)
	}
	return b.Bytes()
}

func (s *syslogSender) connect(ctx context.Context) error {
	if s.conn != nil {
		return nil
	}
	dial, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := s.dial(dial, s.network, s.address)
	if err != nil {
		return err
	}
	if s.useTLS {
		host, _, err := net.SplitHostPort(s.address)
		if err != nil {
			conn.Close()
			return err
		}
		wrapped := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := wrapped.HandshakeContext(dial); err != nil {
			conn.Close()
			return err
		}
		conn = wrapped
	}
	s.conn = conn
	return nil
}

func (s *syslogSender) sendLogs(ctx context.Context, lines []Line) error {
	if err := s.connect(ctx); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		s.conn.SetWriteDeadline(deadline)
	}
	var stream bytes.Buffer
	for _, l := range lines {
		message := s.format(l)
		if s.network == "udp" {
			if _, err := s.conn.Write(message); err != nil {
				s.close()
				return err
			}
			continue
		}
		fmt.Fprintf(&stream, "%d ", len(message))
		stream.Write(message)
	}
	if s.network != "udp" {
		if _, err := s.conn.Write(stream.Bytes()); err != nil {
			s.close()
			return err
		}
	}
	return nil
}

func (s *syslogSender) sendMetrics(context.Context, metrics.Export, time.Time, time.Time) error {
	return nil
}

func (s *syslogSender) close() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}
