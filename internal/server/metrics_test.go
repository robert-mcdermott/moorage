package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

func chartOf(m apitypes.Metrics, key string) apitypes.MetricChart {
	for _, c := range m.Charts {
		if c.Key == key {
			return c
		}
	}
	return apitypes.MetricChart{}
}

// TestMetrics serves requests, and sees them come out as numbers, charts and Prometheus's text.
func TestMetrics(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	deployedSource(t, c, "shop", appManifest(""))
	for range 30 {
		get(t, addrs.HTTP, "shop.localhost", "/")
	}
	get(t, addrs.HTTP, "shop.localhost", "/nothing-here") // the app's own answer, whatever it is
	get(t, addrs.HTTP, "shop.localhost", "/slow?ms=250")

	// They arrive, in the live range, with what instances use.
	var m apitypes.Metrics
	waitUntil(t, "the numbers to come in", func() bool {
		var err error
		m, err = c.Metrics(ctx, "shop", "production", "live")
		if err != nil {
			t.Fatal(err)
		}
		if len(chartOf(m, "requests").Lines) == 0 || len(chartOf(m, "latency").Lines) == 0 || len(chartOf(m, "cpu").Lines) == 0 || len(chartOf(m, "memory").Lines) == 0 {
			return false
		}
		// All the requests, once the step they were in has ended.
		total := 0.0
		for _, p := range chartOf(m, "requests").Lines[0].Points {
			total += p.Value * float64(m.Step) / 60
		}
		return total >= 30
	})
	if m.Step <= 0 || m.From.After(m.To) {
		t.Errorf("the range: %+v", m)
	}
	latency := chartOf(m, "latency")
	if latency.Unit != "ms" || latency.Lines[0].Name != "p50" || latency.Lines[1].Name != "p95" {
		t.Errorf("response times: %+v", latency)
	}
	memory := chartOf(m, "memory")
	if memory.Unit != "bytes" || len(memory.Lines) != 2 {
		t.Fatalf("memory, by instance: %+v", memory)
	}
	names := memory.Lines[0].Name + " " + memory.Lines[1].Name
	if names != "web.1 worker.1" {
		t.Errorf("its instances: %s", names)
	}
	if last := memory.Lines[0].Points[len(memory.Lines[0].Points)-1]; last.Value < 1<<20 {
		t.Errorf("memory in use: %+v", last)
	}
	if len(m.Deployments) != 1 || m.Deployments[0].Version != 1 {
		t.Errorf("the release on the chart: %+v", m.Deployments)
	}
	if _, err := c.Metrics(ctx, "shop", "production", "forever"); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("a range that is not one: %v", err)
	}
	if _, err := c.Metrics(ctx, "nothing", "production", "1h"); status(err) != http.StatusNotFound {
		t.Errorf("an app that is not there: %v", err)
	}

	// The console has them as charts, whose numbers are there to read without any script.
	res, page := b.get("/apps/shop/metrics?range=live")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the page: %d", res.StatusCode)
	}
	contains(t, "the charts", page, "Requests a minute", "Response time", "CPU", "Memory", "<svg", "chart-line", "data-chart=", "The numbers", "web.1", "p95", "chart-mark")
	for _, span := range []string{"1h", "24h", "7d", "30d"} {
		if res, body := b.get("/apps/shop/metrics?range=" + span); res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d\n%s", span, res.StatusCode, body)
		}
	}
	if res, _ := b.get("/apps/shop/metrics?range=forever"); res.StatusCode != http.StatusOK {
		t.Errorf("a range that is not one goes to the hour: %d", res.StatusCode)
	}
	_, page = b.get("/apps/shop")
	contains(t, "its page", page, "The last hour", "95% answered within", "class=\"spark\"")
	_, page = b.get("/")
	contains(t, "the list of apps", page, "class=\"spark\"", "/min")

	// A static site has requests, and no instances to speak of.
	deploySite(t, c, "site")
	get(t, addrs.HTTP, "site.localhost", "/")
	waitUntil(t, "a static site's numbers", func() bool {
		m, err := c.Metrics(ctx, "site", "production", "live")
		if err != nil {
			t.Fatal(err)
		}
		return len(chartOf(m, "requests").Lines) > 0 && len(chartOf(m, "cpu").Lines) == 0
	})
	_, page = b.get("/apps/site")
	if strings.Contains(page, ">Memory<") {
		t.Errorf("a static site's memory")
	}

	// Prometheus reads them with a token, of what the token can see.
	prom := func(token string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/metrics", nil)
		req.Host = cfg.ConsoleHost
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	if code, _ := prom(""); code != http.StatusUnauthorized {
		t.Errorf("without a token: %d", code)
	}
	if code, _ := prom("moor_nonsense"); code != http.StatusUnauthorized {
		t.Errorf("with a bad token: %d", code)
	}
	code, body := prom(tokenOf(t, b, "read"))
	if code != http.StatusOK {
		t.Fatalf("with a token: %d %s", code, body)
	}
	contains(t, "what Prometheus reads", body,
		`moorage_http_requests_total{app="shop",env="production",class="2xx"}`,
		`moorage_http_request_duration_seconds_bucket{app="shop",env="production",le="+Inf"}`,
		`moorage_http_request_duration_seconds_count{app="shop",env="production"}`,
		`moorage_instance_memory_bytes{app="shop",env="production",instance_name="web.1",process="web"}`,
		`# TYPE moorage_http_requests_total counter`,
		"moorage_host_disk_used_percent")
	if gate := tokenOf(t, b, "gate"); gate != "" {
		if code, _ := prom(gate); code != http.StatusUnauthorized {
			t.Errorf("with a token that cannot read: %d", code)
		}
	}

	// Someone who sees one app sees that one, and nothing of the server.
	if _, err := c.CreateProject(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MoveApp(ctx, "shop", "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangeProject(ctx, "team", apitypes.ProjectChange{Everyone: new("none")}); err != nil {
		t.Fatal(err)
	}
	other := addTeammate(t, cfg, addrs, b, "other@example.com")
	if _, err := c.ChangeProject(ctx, "default", apitypes.ProjectChange{Everyone: new("none")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetMember(ctx, "default", "other@example.com", "viewer"); err != nil {
		t.Fatal(err)
	}
	code, body = prom(tokenOf(t, other, "read"))
	if code != http.StatusOK || strings.Contains(body, `app="shop"`) || strings.Contains(body, "moorage_host_") {
		t.Errorf("what another sees: %d\n%s", code, body)
	}
	if !strings.Contains(body, `app="site"`) {
		t.Errorf("what another sees lacks their own app:\n%s", body)
	}

}
