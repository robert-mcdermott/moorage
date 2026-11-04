package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
)

func TestChecking(t *testing.T) {
	good := []Form{
		{Name: "central", Kind: KindSyslog, Address: "udp://logs:514", Logs: true},
		{Name: "central", Kind: KindSyslog, Address: "tcp://logs.example.com:601", Logs: true},
		{Name: "central", Kind: KindSyslog, Address: "tls://[::1]:6514", Logs: true},
		{Name: "c", Kind: KindOTLP, Address: "https://otel.example.com:4318", Logs: true, Metrics: true},
		{Name: "c", Kind: KindOTLP, Address: "https://otel.example.com:4318", Metrics: true, Headers: map[string]string{"Authorization": "Bearer x"}},
		{Name: "c", Kind: KindOTLP, Address: "http://otel.lan:4318", Logs: true, Headers: map[string]string{"X-Key": "x"}},
		{Name: "c", Kind: KindHTTP, Address: "http://example.com/in", Logs: true},
	}
	for _, f := range good {
		if err := f.Check(); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
	bad := []Form{
		{Name: "", Kind: KindHTTP, Address: "https://x/", Logs: true},
		{Name: "a b", Kind: KindHTTP, Address: "https://x/", Logs: true},
		{Name: "a", Kind: "kafka", Address: "https://x/", Logs: true},
		{Name: "a", Kind: KindHTTP, Address: "https://x/"},
		{Name: "a", Kind: KindHTTP, Address: "ftp://x/", Logs: true},
		{Name: "a", Kind: KindHTTP, Address: "file:///etc/passwd", Logs: true},
		{Name: "a", Kind: KindHTTP, Address: "https://me:secret@x/", Logs: true},
		{Name: "a", Kind: KindHTTP, Address: "https://x/", Metrics: true},
		{Name: "a", Kind: KindHTTP, Address: "http://x.example.com/", Logs: true, Headers: map[string]string{"Authorization": "in the clear"}},
		{Name: "a", Kind: KindHTTP, Address: "https://x/", Logs: true, Headers: map[string]string{"Bad Name": "x"}},
		{Name: "a", Kind: KindHTTP, Address: "https://x/", Logs: true, Headers: map[string]string{"X": "two\r\nlines: y"}},
		{Name: "a", Kind: KindSyslog, Address: "logs:514", Logs: true},
		{Name: "a", Kind: KindSyslog, Address: "udp://logs", Logs: true},
		{Name: "a", Kind: KindSyslog, Address: "https://logs:514", Logs: true},
		{Name: "a", Kind: KindSyslog, Address: "udp://logs:514", Metrics: true},
		{Name: "a", Kind: KindSyslog, Address: "udp://logs:514", Logs: true, Headers: map[string]string{"X": "y"}},
	}
	for _, f := range bad {
		if err := f.Check(); err == nil {
			t.Errorf("%+v was taken", f)
		}
	}
}

var at = time.Date(2026, 11, 4, 12, 30, 15, 123456000, time.UTC)

func TestSyslogFormat(t *testing.T) {
	s := &syslogSender{network: "tcp", server: "web one"}
	got := string(s.format(Line{Time: at, App: "shop", Env: "production", Process: "web", Instance: "ins_1", Stream: "O", Text: "hello there"}))
	want := "<134>1 2026-11-04T12:30:15.123456Z web_one shop ins_1 production - hello there"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
	got = string(s.format(Line{Time: at, App: "shop", Stream: "E", Text: "bad"}))
	if !strings.HasPrefix(got, "<131>1 ") || !strings.Contains(got, " - - bad") {
		t.Errorf("%s", got)
	}
	udp := &syslogSender{network: "udp"}
	if n := len(udp.format(Line{Time: at, Text: strings.Repeat("x", 100000)})); n > maxSyslogDatagram {
		t.Errorf("a datagram of %d", n)
	}
	if n := len(s.format(Line{Time: at, Text: strings.Repeat("x", 1<<20)})); n > maxSyslogLine {
		t.Errorf("a line of %d", n)
	}
	got = string(s.format(Line{Time: at, Text: "not \xff valid"}))
	if !strings.Contains(got, "not � valid") {
		t.Errorf("%q", got)
	}
}

func TestSyslogOverAStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	got := make(chan string, 10)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			size, err := r.ReadString(' ')
			if err != nil {
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(size))
			if err != nil {
				got <- "bad count: " + size
				return
			}
			message := make([]byte, n)
			if _, err := io.ReadFull(r, message); err != nil {
				got <- "short: " + err.Error()
				return
			}
			got <- string(message)
		}
	}()
	s := &syslogSender{network: "tcp", address: listener.Addr().String(), server: "here", dial: (&net.Dialer{}).DialContext}
	defer s.close()
	ctx := context.Background()
	if err := s.sendLogs(ctx, []Line{{Time: at, App: "shop", Text: "one\nwith a new line"}, {Time: at, App: "shop", Text: "two"}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one\nwith a new line", "two"} {
		select {
		case message := <-got:
			if !strings.HasSuffix(message, want) {
				t.Errorf("%q", message)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("nothing arrived")
		}
	}
	// A connection that has gone is made again, on the next try.
	s.conn.Close()
	s.sendLogs(ctx, []Line{{Time: at, Text: "lost"}})
	if s.conn != nil {
		t.Error("it held on to a connection that failed")
	}
}

type heardRequest struct {
	path, auth string
	body       map[string]any
}

func collector(t *testing.T, status int) (*httptest.Server, func() []heardRequest) {
	var mu sync.Mutex
	var heard []heardRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("not JSON: %s", data)
		}
		mu.Lock()
		heard = append(heard, heardRequest{r.URL.Path, r.Header.Get("Authorization"), body})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []heardRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]heardRequest(nil), heard...)
	}
}

// find walks down a document that was decoded from JSON.
func find(doc any, path ...any) any {
	for _, step := range path {
		switch k := step.(type) {
		case string:
			m, ok := doc.(map[string]any)
			if !ok {
				return nil
			}
			doc = m[k]
		case int:
			list, ok := doc.([]any)
			if !ok || k >= len(list) {
				return nil
			}
			doc = list[k]
		}
	}
	return doc
}

func TestOTLPLogs(t *testing.T) {
	srv, heard := collector(t, http.StatusOK)
	s := &otlpSender{base: srv.URL, headers: map[string]string{"Authorization": "Bearer x"}, client: srv.Client(), server: "here", version: "1.2.3"}
	err := s.sendLogs(context.Background(), []Line{
		{Time: at, App: "shop", Env: "production", Process: "web", Instance: "ins_1", Stream: "O", Text: "one"},
		{Time: at.Add(time.Second), App: "shop", Env: "production", Process: "web", Instance: "ins_1", Stream: "E", Text: "two"},
		{Time: at, App: "blog", Env: "staging", Process: "web", Instance: "ins_2", Stream: "O", Text: "three"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := heard()
	if len(got) != 1 || got[0].path != "/v1/logs" || got[0].auth != "Bearer x" {
		t.Fatalf("%+v", got)
	}
	body := got[0].body
	if n := len(find(body, "resourceLogs").([]any)); n != 2 {
		t.Fatalf("%d resources: %v", n, body)
	}
	attributes := map[string]string{}
	for _, a := range find(body, "resourceLogs", 0, "resource", "attributes").([]any) {
		attributes[find(a, "key").(string)] = find(a, "value", "stringValue").(string)
	}
	if attributes["service.name"] != "shop" || attributes["deployment.environment.name"] != "production" || attributes["service.instance.id"] != "ins_1" || attributes["host.name"] != "here" {
		t.Errorf("%v", attributes)
	}
	records := find(body, "resourceLogs", 0, "scopeLogs", 0, "logRecords").([]any)
	if len(records) != 2 || find(records, 0, "body", "stringValue") != "one" || find(records, 0, "timeUnixNano") != strconv.FormatInt(at.UnixNano(), 10) {
		t.Errorf("%v", records)
	}
	if find(records, 1, "attributes", 0, "key") != "log.iostream" || find(records, 1, "attributes", 0, "value", "stringValue") != "stderr" {
		t.Errorf("%v", records)
	}
}

func TestOTLPMetrics(t *testing.T) {
	srv, heard := collector(t, http.StatusOK)
	s := &otlpSender{base: srv.URL, client: srv.Client(), server: "here", version: "1.2.3"}
	e := metrics.Export{
		Counters: []metrics.ExportValue{
			{Name: "moorage.http.requests", Unit: "{request}", Description: "d", Labels: map[string]string{"app": "shop", "class": "2xx"}, Value: 7},
			{Name: "moorage.http.requests", Unit: "{request}", Description: "d", Labels: map[string]string{"app": "shop", "class": "5xx"}, Value: 1},
		},
		Gauges:     []metrics.ExportValue{{Name: "moorage.host.load", Unit: "1", Description: "d", Labels: map[string]string{}, Value: 0.5}},
		Histograms: []metrics.ExportHistogram{{Name: "moorage.http.request.duration", Unit: "s", Description: "d", Labels: map[string]string{"app": "shop"}, Bounds: []float64{0.1, 0.2}, Counts: []uint64{1, 2, 3}, Sum: 1.5, Count: 6}},
	}
	if err := s.sendMetrics(context.Background(), e, at.Add(-time.Hour), at); err != nil {
		t.Fatal(err)
	}
	got := heard()
	if len(got) != 1 || got[0].path != "/v1/metrics" {
		t.Fatalf("%+v", got)
	}
	list := find(got[0].body, "resourceMetrics", 0, "scopeMetrics", 0, "metrics").([]any)
	if len(list) != 3 {
		t.Fatalf("%v", list)
	}
	counter := list[0]
	if find(counter, "name") != "moorage.http.requests" || find(counter, "sum", "isMonotonic") != true || find(counter, "sum", "aggregationTemporality") != float64(2) {
		t.Errorf("%v", counter)
	}
	points := find(counter, "sum", "dataPoints").([]any)
	if len(points) != 2 || find(points, 0, "asInt") != "7" {
		t.Errorf("%v", points)
	}
	if find(list[1], "gauge", "dataPoints", 0, "asDouble") != 0.5 {
		t.Errorf("%v", list[1])
	}
	h := find(list[2], "histogram", "dataPoints", 0)
	if find(h, "count") != "6" || find(h, "sum") != 1.5 || find(h, "bucketCounts", 2) != "3" || find(h, "explicitBounds", 1) != 0.2 {
		t.Errorf("%v", h)
	}
	if err := s.sendMetrics(context.Background(), metrics.Export{}, at, at); err != nil || len(heard()) != 1 {
		t.Errorf("nothing to say was said: %v", err)
	}
}

func TestSenderFailures(t *testing.T) {
	srv, _ := collector(t, http.StatusForbidden)
	secret := &httpSender{address: srv.URL + "/in?key=a-secret-in-the-address", client: srv.Client(), server: "here"}
	err := secret.sendLogs(context.Background(), []Line{{Time: at, Text: "x"}})
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "a-secret") {
		t.Errorf("%v", err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	address := gone.URL + "/in?key=a-secret-in-the-address"
	gone.Close()
	err = (&httpSender{address: address, client: http.DefaultClient}).sendLogs(context.Background(), []Line{{Time: at, Text: "x"}})
	if err == nil || strings.Contains(err.Error(), "a-secret") {
		t.Errorf("%v", err)
	}
}
