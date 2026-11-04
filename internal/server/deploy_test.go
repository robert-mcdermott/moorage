package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

// writeSite writes files into a new directory and returns it.
func writeSite(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func apiClient(t *testing.T, cfg config.Config, addrs Addrs) *client.Client {
	t.Helper()
	token, err := os.ReadFile(NewLayout(cfg.DataDir).AdminToken)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New("http://"+addrs.HTTP.String(), strings.TrimSpace(string(token)), "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDeployRollbackAndRestart(t *testing.T) {
	cfg := testConfig(t)
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()

	v1 := writeSite(t, map[string]string{"index.html": "one", "about.html": "about one"})
	d, stats, err := c.DeployStatic(ctx, "docs", "production", v1, manifest.Static{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.AppCreated || d.Release.Version != 1 || d.Release.Files != 2 || stats.Files != 2 {
		t.Errorf("first deploy: %+v, %+v", d, stats)
	}
	if !strings.HasPrefix(d.URL, "http://docs.localhost:") {
		t.Errorf("URL = %q", d.URL)
	}
	if _, body := get(t, addrs.HTTP, "docs.localhost", "/"); body != "one" {
		t.Errorf("v1 serves %q", body)
	}
	if _, body := get(t, addrs.HTTP, "docs.localhost", "/about"); body != "about one" {
		t.Errorf("v1 /about serves %q", body)
	}

	v2 := writeSite(t, map[string]string{"index.html": "two", "app.html": "spa"})
	if d, _, err = c.DeployStatic(ctx, "docs", "production", v2, manifest.Static{SPA: true}, nil); err != nil || d.Release.Version != 2 || d.AppCreated {
		t.Fatalf("second deploy: %+v, %v", d, err)
	}
	if _, body := get(t, addrs.HTTP, "docs.localhost", "/some/client/route"); body != "two" {
		t.Errorf("v2 SPA fallback serves %q", body)
	}

	apps, err := c.Apps(ctx)
	if err != nil || len(apps) != 1 || apps[0].Name != "docs" || apps[0].Production == nil || apps[0].Production.Version != 2 {
		t.Errorf("apps: %+v, %v", apps, err)
	}

	d, err = c.Rollback(ctx, "docs", "production", 0)
	if err != nil || d.Release.Version != 3 || d.Release.Reason != "rollback" {
		t.Fatalf("rollback: %+v, %v", d, err)
	}
	if _, body := get(t, addrs.HTTP, "docs.localhost", "/"); body != "one" {
		t.Errorf("after rollback serves %q", body)
	}
	if _, err := c.Rollback(ctx, "docs", "production", 9); err == nil {
		t.Error("rollback to a release that does not exist succeeded")
	}
	rels, err := c.Releases(ctx, "docs", "production")
	if err != nil || len(rels) != 3 || !rels[0].Current || rels[0].Version != 3 || rels[1].Current {
		t.Errorf("releases: %+v, %v", rels, err)
	}

	// A restarted server serves what was deployed.
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	addrs, _ = start(t, cfg)
	if _, body := get(t, addrs.HTTP, "docs.localhost", "/"); body != "one" {
		t.Errorf("after restart serves %q", body)
	}
}

func TestDeployRejectsBadRequests(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	site := writeSite(t, map[string]string{"index.html": "x"})

	var apiErr *client.APIError
	if _, _, err := c.DeployStatic(ctx, "Bad_Name", "production", site, manifest.Static{}, nil); !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Errorf("invalid app name: %v", err)
	}
	if _, _, err := c.DeployStatic(ctx, "docs", "Not_A_Name", site, manifest.Static{}, nil); !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Errorf("invalid environment name: %v", err)
	}
	bad := manifest.Static{Redirects: []manifest.Redirect{{From: "/a", To: "/b", Status: 404}}}
	if _, _, err := c.DeployStatic(ctx, "docs", "production", site, bad, nil); !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Errorf("invalid rules: %v", err)
	}
	empty := t.TempDir()
	if _, _, err := c.DeployStatic(ctx, "docs", "production", empty, manifest.Static{}, nil); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("empty upload: %v", err)
	}
	// Nothing was published, the app was not created, and no half-written
	// build is left behind.
	if apps, err := c.Apps(ctx); err != nil || len(apps) != 0 {
		t.Errorf("after failed deploys: apps %+v, %v", apps, err)
	}
	if _, body := get(t, addrs.HTTP, "docs.localhost", "/"); !strings.Contains(body, "There is no app named") {
		t.Errorf("after failed deploys the app serves %q", body)
	}
	matches, _ := filepath.Glob(filepath.Join(cfg.DataDir, "builds", "*", "*.partial"))
	if len(matches) != 0 {
		t.Errorf("half-written builds left: %v", matches)
	}

	wrong, err := client.New("http://"+addrs.HTTP.String(), "moor_wrong", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Apps(ctx); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("wrong token: %v", err)
	}
}

// TestZeroDowntimeSwaps is spike S4: while clients read a site without
// pause, deploys and rollbacks swap the routing table underneath them, and
// no request may fail or see anything but a complete version.
func TestZeroDowntimeSwaps(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	one := writeSite(t, map[string]string{"index.html": strings.Repeat("one ", 2000)})
	two := writeSite(t, map[string]string{"index.html": strings.Repeat("two ", 2000)})
	if _, _, err := c.DeployStatic(ctx, "docs", "production", one, manifest.Static{}, nil); err != nil {
		t.Fatal(err)
	}

	var requests, failures atomic.Int64
	var firstFailure atomic.Value
	stopLoad := make(chan struct{})
	var wg sync.WaitGroup
	transport := &http.Transport{MaxIdleConnsPerHost: 16}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/", nil)
				req.Host = "docs.localhost"
				res, err := httpClient.Do(req)
				requests.Add(1)
				if err != nil {
					failures.Add(1)
					firstFailure.CompareAndSwap(nil, err.Error())
					continue
				}
				body, err := io.ReadAll(res.Body)
				res.Body.Close()
				ok := err == nil && res.StatusCode == 200 &&
					(string(body) == strings.Repeat("one ", 2000) || string(body) == strings.Repeat("two ", 2000))
				if !ok {
					failures.Add(1)
					firstFailure.CompareAndSwap(nil, res.Status+" "+string(body[:min(len(body), 80)]))
				}
			}
		}()
	}
	// Each swap waits for the clients to make more requests, so that every
	// swap happens under load.
	awaitTraffic := func() {
		target := requests.Load() + 100
		deadline := time.Now().Add(10 * time.Second)
		for requests.Load() < target && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	for i := 0; i < 6; i++ {
		awaitTraffic()
		if _, _, err := c.DeployStatic(ctx, "docs", "production", two, manifest.Static{}, nil); err != nil {
			t.Error(err)
		}
		awaitTraffic()
		if _, err := c.Rollback(ctx, "docs", "production", 0); err != nil {
			t.Error(err)
		}
	}
	awaitTraffic()
	close(stopLoad)
	wg.Wait()
	t.Logf("%d requests during 12 swaps, %d failed", requests.Load(), failures.Load())
	if failures.Load() > 0 {
		t.Errorf("%d of %d requests failed during swaps; first: %v", failures.Load(), requests.Load(), firstFailure.Load())
	}
	if requests.Load() < 1200 {
		t.Errorf("only %d requests were made; the load was too light to show anything", requests.Load())
	}
}
