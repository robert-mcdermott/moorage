package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/testroles"
)

// fakeDNS answers from a table: names with a CNAME, and names with addresses.
type fakeDNS struct{ cnames, addrs map[string]string }

func (f fakeDNS) LookupCNAME(_ context.Context, host string) (string, error) {
	if c, ok := f.cnames[host]; ok {
		return c + ".", nil
	}
	return "", errors.New("no such host")
}

func (f fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if a, ok := f.addrs[host]; ok {
		return []net.IPAddr{{IP: net.ParseIP(a)}}, nil
	}
	return nil, errors.New("no such host")
}

// startWith runs a server with options a test wants.
func startWith(t *testing.T, cfg config.Config, change func(*Options)) Addrs {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan Addrs, 1)
	done := make(chan error, 1)
	log, logDone := testLogger(t)
	opts := testOptions(func(a Addrs) { ready <- a })
	change(&opts)
	go func() { done <- Run(ctx, cfg, log, opts) }()
	t.Cleanup(func() {
		cancel()
		<-done
		logDone()
		testroles.StopInstances(cfg.DataDir)
	})
	select {
	case a := <-ready:
		return a
	case err := <-done:
		t.Fatalf("the server did not start: %v", err)
	}
	return Addrs{}
}

func TestCustomDomains(t *testing.T) {
	cfg := testConfig(t)
	dns := fakeDNS{
		cnames: map[string]string{"portal.example.test": "shop.localhost", "wrong.example.test": "elsewhere.example.net"},
		addrs: map[string]string{
			"shop.localhost": "192.0.2.10", "moorage.localhost": "192.0.2.10", "tools.example.test": "192.0.2.10", "wrong.example.test": "198.51.100.1",
		},
	}
	addrs := startWith(t, cfg, func(o *Options) { o.Resolver = dns })
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	ctx := context.Background()
	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"TESTAPP_BODY": "the shop"})); err != nil {
		t.Fatal(err)
	}
	if _, err := deployApp(t, c, "reports", appPlan(map[string]string{"TESTAPP_BODY": "the reports"})); err != nil {
		t.Fatal(err)
	}

	// What makes no sense is refused.
	for _, bad := range []string{"not a name", "localhost", "shop.localhost", "moorage.localhost", "x.example.test/.moorage", "x.example.test/a/../b"} {
		if _, err := c.AddDomain(ctx, "shop", "production", apitypes.NewDomain{Address: bad}); status(err) != http.StatusUnprocessableEntity {
			t.Errorf("attaching %q: %v", bad, err)
		}
	}

	// A CNAME to it, and at once it serves.
	d, err := c.AddDomain(ctx, "shop", "production", apitypes.NewDomain{Address: "Portal.Example.Test"})
	if err != nil || !d.Verified || d.Address != "portal.example.test" || !strings.Contains(d.Record, "CNAME shop.localhost") {
		t.Fatalf("a CNAME: %+v %v", d, err)
	}
	if res, body := get(t, addrs.HTTP, "portal.example.test", "/"); res.StatusCode != http.StatusOK || body != "the shop" {
		t.Errorf("at its own domain: %d %q", res.StatusCode, body)
	}
	if res, body := get(t, addrs.HTTP, "shop.localhost", "/"); res.StatusCode != http.StatusOK || body != "the shop" {
		t.Errorf("at its own hostname still: %d %q", res.StatusCode, body)
	}

	// Pointing elsewhere it serves nothing, until it points here.
	d, err = c.AddDomain(ctx, "shop", "production", apitypes.NewDomain{Address: "wrong.example.test"})
	if err != nil || d.Verified || !strings.Contains(d.Problem, "198.51.100.1") {
		t.Fatalf("pointing elsewhere: %+v %v", d, err)
	}
	if res, _ := get(t, addrs.HTTP, "wrong.example.test", "/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("pointing elsewhere, served: %d", res.StatusCode)
	}
	dns.cnames["wrong.example.test"] = "shop.localhost"
	if d, err = c.VerifyDomain(ctx, "shop", "production", "wrong.example.test"); err != nil || !d.Verified {
		t.Errorf("once it points here: %+v %v", d, err)
	}
	if res, _ := get(t, addrs.HTTP, "wrong.example.test", "/"); res.StatusCode != http.StatusOK {
		t.Errorf("once it points here, served: %d", res.StatusCode)
	}

	// Attached twice, nothing changes.
	if _, err := c.AddDomain(ctx, "reports", "production", apitypes.NewDomain{Address: "portal.example.test"}); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("attached already: %v", err)
	}

	// Two apps on paths of one hostname, the longest first, by their addresses matching the server's.
	if d, err := c.AddDomain(ctx, "reports", "production", apitypes.NewDomain{Address: "tools.example.test/reports", StripPrefix: true}); err != nil || !d.Verified {
		t.Fatalf("a path: %+v %v", d, err)
	}
	if d, err := c.AddDomain(ctx, "shop", "production", apitypes.NewDomain{Address: "tools.example.test/reports/shop"}); err != nil || !d.Verified {
		t.Fatalf("a longer path: %+v %v", d, err)
	}
	if res, body := get(t, addrs.HTTP, "tools.example.test", "/reports/path"); res.StatusCode != http.StatusOK || body != "/path" {
		t.Errorf("a path taken away: %d %q", res.StatusCode, body)
	}
	if _, body := get(t, addrs.HTTP, "tools.example.test", "/reports/shop/"); body != "the shop" {
		t.Errorf("the longer path: %q", body)
	}
	if res, _ := get(t, addrs.HTTP, "tools.example.test", "/reportsx/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a path that only begins the same: %d", res.StatusCode)
	}
	if res, _ := get(t, addrs.HTTP, "tools.example.test", "/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("what no path serves: %d", res.StatusCode)
	}
	if res, _ := get(t, addrs.HTTP, "tools.example.test", "/reports"); res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/reports/" {
		t.Errorf("the path without its slash: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	_, heard := get(t, addrs.HTTP, "tools.example.test", "/reports/headers")
	contains(t, "what the app hears", heard, "X-Forwarded-Prefix: /reports", "Host: tools.example.test")

	// What it says of paths is made to fit, and only that.
	res, _ := get(t, addrs.HTTP, "tools.example.test", "/reports/redirect?to=/elsewhere")
	if res.Header.Get("Location") != "/reports/elsewhere" || !strings.Contains(res.Header.Get("Set-Cookie"), "Path=/reports/") {
		t.Errorf("a redirect and a cookie: %v", res.Header)
	}
	if res, _ := get(t, addrs.HTTP, "tools.example.test", "/reports/redirect?to=https://example.org/x"); res.Header.Get("Location") != "https://example.org/x" {
		t.Errorf("a redirect elsewhere: %v", res.Header)
	}

	// A gate goes with the environment, wherever it is served, and each has its own cookie.
	setGate(t, c, "reports", "production", apitypes.AccessChange{Mode: "password", Password: "correct horse"})
	v := newVisitor(t, addrs, "tools.example.test")
	res, page := v.get("/reports/")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a gate on a path: %d", res.StatusCode)
	}
	contains(t, "its form", page, `action="/reports/.moorage/gate/password"`, `value="/reports/"`)
	if _, body := v.get("/reports/shop/"); body != "the shop" {
		t.Errorf("the other app on that hostname has no gate: %q", body)
	}
	res, _ = v.do(http.MethodPost, "/reports/.moorage/gate/password", url.Values{"password": {"correct horse"}, "return": {"/reports/path"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/reports/path" {
		t.Fatalf("the password on a path: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res, body := v.get("/reports/path"); res.StatusCode != http.StatusOK || body != "/path" {
		t.Errorf("through the gate on a path: %d %q", res.StatusCode, body)
	}
	res, _ = v.do(http.MethodPost, "/reports/.moorage/gate/password", url.Values{"password": {"correct horse"}, "return": {"/elsewhere/"}}, nil)
	if res.Header.Get("Location") != "/reports/" {
		t.Errorf("going back outside its path: %s", res.Header.Get("Location"))
	}
	if res, _ := get(t, addrs.HTTP, "tools.example.test", "/reports/.moorage/other"); res.StatusCode != http.StatusNotFound {
		t.Errorf("under /.moorage/ on a path: %d", res.StatusCode)
	}

	// The console attaches and detaches them, and says what DNS needs.
	token := b.token()
	_, page = b.get("/apps/shop/access")
	contains(t, "the page", page, "portal.example.test", "CNAME shop.localhost", "Serving", "Attach it")
	if res, body := b.post("/apps/shop/domains", url.Values{"csrf": {token}, "address": {"new.example.test"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("attaching in the console: %d\n%s", res.StatusCode, body)
	}
	_, page = b.get("/apps/shop/access")
	contains(t, "the page", page, "new.example.test", "Not pointing here yet", "Look again")
	if res, body := b.post("/apps/shop/domains", url.Values{"csrf": {token}, "address": {"not a name"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a bad name in the console: %d\n%s", res.StatusCode, body)
	}
	if res, _ := b.post("/apps/shop/domains/remove", url.Values{"csrf": {token}, "address": {"portal.example.test"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("detaching: %d", res.StatusCode)
	}
	if res, _ := get(t, addrs.HTTP, "portal.example.test", "/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("after detaching: %d", res.StatusCode)
	}

	// A domain that points here later needs no one to ask.
	dns.cnames["new.example.test"] = "shop.localhost"
	if list, err := c.Domains(ctx, "shop", "production"); err != nil || len(list) == 0 {
		t.Errorf("the list: %+v %v", list, err)
	}
}

// TestPathRouting serves every app at a path of one hostname, with the console on its own.
func TestPathRouting(t *testing.T) {
	cfg := testConfig(t)
	cfg.Routing = "path"
	cfg.Domain = "apps.localhost"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	same := cfg
	same.ConsoleHost = same.Domain
	if err := same.Validate(); err == nil || !strings.Contains(err.Error(), "console_host") {
		t.Errorf("apps on the console's origin: %v", err)
	}
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	b := signIn(t, cfg, addrs)
	const host = "apps.localhost"

	deploy := func(app string, c *client.Client, files map[string]string) apitypes.Deployment {
		t.Helper()
		return deployedSource(t, c, app, files)
	}
	d := deploy("shop", c, map[string]string{"moorage.toml": "[app]\nruntime = \"command\"\nstrip_prefix = true\n\n[processes.web]\ncommand = " + tomlString(testroles.AppCommand()) + "\nhealth = { path = \"/health\", interval = \"100ms\", startup = \"30s\" }\n\n[env]\nTESTAPP_BODY = \"the shop\"\n"})
	if d.URL != "http://apps.localhost:0/shop" && !strings.HasSuffix(d.URL, "/shop") {
		t.Errorf("its address: %s", d.URL)
	}
	if res, body := get(t, addrs.HTTP, host, "/shop/"); res.StatusCode != http.StatusOK || body != "the shop" {
		t.Errorf("at its path: %d %q", res.StatusCode, body)
	}
	if _, body := get(t, addrs.HTTP, host, "/shop/env?name=MOORAGE_BASE_PATH"); body != "/shop" {
		t.Errorf("its base path: %q", body)
	}
	if _, body := get(t, addrs.HTTP, host, "/shop/env?name=MOORAGE_URL"); !strings.HasSuffix(body, "/shop") {
		t.Errorf("its URL: %q", body)
	}
	if res, _ := get(t, addrs.HTTP, host, "/nothing/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("no such app: %d", res.StatusCode)
	}
	if res, _ := get(t, addrs.HTTP, host, "/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("the top: %d", res.StatusCode)
	}

	// A static site, whose own redirects fit.
	deploySite(t, c, "docs")
	if res, _ := get(t, addrs.HTTP, host, "/docs/"); res.StatusCode != http.StatusOK {
		t.Errorf("a static site at its path: %d", res.StatusCode)
	}
	if res, _ := get(t, addrs.HTTP, host, "/docs"); res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/docs/" {
		t.Errorf("a static site without its slash: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// Named environments are at paths of their own too.
	d2, _, err := c.DeploySource(context.Background(), "shop", "staging", writeSite(t, map[string]string{"moorage.toml": "[app]\nruntime = \"command\"\nstrip_prefix = true\n\n[processes.web]\ncommand = " + tomlString(testroles.AppCommand()) + "\nhealth = { path = \"/health\", interval = \"100ms\", startup = \"30s\" }\n\n[env]\nTESTAPP_BODY = \"staging\"\n"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := followed(t, c, d2.ID); got.Status != apitypes.StatusLive {
		t.Fatalf("staging: %+v", got)
	}
	if res, body := get(t, addrs.HTTP, host, "/shop--staging/"); res.StatusCode != http.StatusOK || body != "staging" {
		t.Errorf("a named environment: %d %q", res.StatusCode, body)
	}
	if res, _ := get(t, addrs.HTTP, host, "/shop--staging/"); !strings.Contains(res.Header.Get("X-Robots-Tag"), "noindex") {
		t.Errorf("no indexing: %v", res.Header)
	}

	// The console says where it is, and goes on being on its own.
	_, page := b.get("/apps/shop")
	contains(t, "its page", page, "apps.localhost")
	if res, _ := get(t, addrs.HTTP, cfg.ConsoleHost, "/shop/"); res.StatusCode == http.StatusOK && res.Header.Get("Server") == "apps" {
		t.Errorf("the console serves apps: %d", res.StatusCode)
	}
}
