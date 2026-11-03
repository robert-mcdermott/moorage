package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/testroles"
)

func duration(s string) manifest.Duration {
	var d manifest.Duration
	if err := d.UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return d
}

// appPlan runs the test binary as the app's web process, plus any extra
// environment.
func appPlan(env map[string]string) manifest.Plan {
	return manifest.Plan{
		Kind: manifest.KindProcess,
		Processes: map[string]manifest.Process{
			manifest.Web: {
				Command: testroles.AppCommand(),
				Health:  manifest.Health{Path: "/health", Interval: duration("100ms"), Threshold: 2, Startup: duration("30s")},
			},
		},
		Deploy: manifest.Deploy{Drain: duration("5s")},
		Env:    env,
	}
}

// deployApp deploys appPlan as app (the upload holds a placeholder file:
// the app is the test binary itself).
func deployApp(t *testing.T, c *client.Client, app string, plan manifest.Plan) (apitypes.Deployment, error) {
	t.Helper()
	dir := writeSite(t, map[string]string{"README": "the app is the test binary"})
	d, _, err := c.Deploy(context.Background(), app, "production", dir, plan, nil)
	return d, err
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func instances(t *testing.T, c *client.Client, app string) []apitypes.Instance {
	t.Helper()
	ins, err := c.Instances(context.Background(), app, "production")
	if err != nil {
		t.Fatal(err)
	}
	return ins
}

func TestArtifactDeploy(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)

	d, err := deployApp(t, c, "shop", appPlan(nil))
	if err != nil {
		t.Fatal(err)
	}
	if d.Release.Version != 1 || !d.AppCreated {
		t.Errorf("deployment = %+v", d)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "hello from v1" {
		t.Errorf("served %q", body)
	}
	ins := instances(t, c, "shop")
	if len(ins) != 1 || ins[0].Status != "running" || !ins[0].Healthy || ins[0].Process != "web" || ins[0].PID == 0 {
		t.Errorf("instances = %+v", ins)
	}

	// Forwarded headers: set by Moorage, not by the client (not a trusted
	// proxy); X-Moorage-* headers from clients are removed.
	req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/headers", nil)
	req.Host = "shop.localhost"
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Moorage-User", "forged@example.com")
	req.Header.Set("X-Request-Id", "forged")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	h := string(body)
	for _, want := range []string{"X-Forwarded-For: 127.0.0.1\n", "X-Forwarded-Proto: http\n", "X-Forwarded-Host: shop.localhost\n", "X-Moorage-User: \n", "Host: shop.localhost\n"} {
		if !strings.Contains(h, want) {
			t.Errorf("headers seen by the app lack %q:\n%s", want, h)
		}
	}
	rid := res.Header.Get("X-Request-Id")
	if rid == "" || rid == "forged" || !strings.Contains(h, "X-Request-Id: "+rid) {
		t.Errorf("request ID %q not set by Moorage:\n%s", rid, h)
	}

	// Logs, through the API, and the access log.
	var lines []apitypes.LogLine
	err = c.Logs(context.Background(), "shop", "production", "", 50, false, func(l apitypes.LogLine) error {
		lines = append(lines, l)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, l := range lines {
		joined += l.Process + " " + l.Stream + " " + l.Text + "\n"
	}
	if !strings.Contains(joined, "web O listening on") || !strings.Contains(joined, "web M started (pid") {
		t.Errorf("logs:\n%s", joined)
	}
	access, err := os.ReadFile(filepath.Join(cfg.DataDir, "logs", "router", "shop", "production.log"))
	if err != nil || !strings.Contains(string(access), `"path":"/headers"`) || !strings.Contains(string(access), `"instance":"ins_`) {
		t.Errorf("access log: %v\n%s", err, access)
	}
}

func TestFailedDeployKeepsTheRelease(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	_, err := deployApp(t, c, "shop", appPlan(map[string]string{"TESTAPP_FAIL": "1"}))
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "rollout_failed" || !strings.Contains(err.Error(), "failing as asked") {
		t.Fatalf("failed deploy: %v", err)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "hello from v1" {
		t.Errorf("after a failed deploy served %q", body)
	}
	rels, _ := c.Releases(context.Background(), "shop", "production")
	if len(rels) != 1 || !rels[0].Current {
		t.Errorf("releases after a failed deploy: %+v", rels)
	}
	if ins := instances(t, c, "shop"); len(ins) != 1 || ins[0].Release != 1 {
		t.Errorf("instances after a failed deploy: %+v", ins)
	}
}

// TestRollingChangesUnderLoad: deploys, a restart and a rollback while
// clients read the app without pause; no request may fail.
func TestRollingChangesUnderLoad(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	plan := appPlan(nil)
	web := plan.Processes["web"]
	two := 2
	web.Instances = &two
	plan.Processes["web"] = web
	if _, err := deployApp(t, c, "shop", plan); err != nil {
		t.Fatal(err)
	}

	var requests, failures atomic.Int64
	var first atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	transport := &http.Transport{MaxIdleConnsPerHost: 16}
	defer transport.CloseIdleConnections()
	hc := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/", nil)
				req.Host = "shop.localhost"
				res, err := hc.Do(req)
				requests.Add(1)
				if err != nil {
					failures.Add(1)
					first.CompareAndSwap(nil, err.Error())
					continue
				}
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if res.StatusCode != 200 || !strings.HasPrefix(string(body), "hello from v") {
					failures.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("%d %.80s", res.StatusCode, body))
				}
			}
		}()
	}
	ctx := context.Background()
	if _, err := deployApp(t, c, "shop", plan); err != nil {
		t.Error(err)
	}
	if _, err := c.Restart(ctx, "shop", "production"); err != nil {
		t.Error(err)
	}
	if _, err := c.Rollback(ctx, "shop", "production", 0); err != nil {
		t.Error(err)
	}
	close(stop)
	wg.Wait()
	t.Logf("%d requests across a deploy, a restart and a rollback; %d failed", requests.Load(), failures.Load())
	if failures.Load() > 0 {
		t.Errorf("%d of %d requests failed; first: %v", failures.Load(), requests.Load(), first.Load())
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "hello from v3" {
		t.Errorf("after the rollback served %q", body)
	}
	if ins := instances(t, c, "shop"); len(ins) != 2 || ins[0].Release != 3 || ins[1].Release != 3 {
		t.Errorf("instances after the rollback: %+v", ins)
	}
}

// TestAppsOutliveTheServer is spike S2: stopping the server leaves its apps
// running, and a new server adopts them.
func TestAppsOutliveTheServer(t *testing.T) {
	cfg := testConfig(t)
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	before := instances(t, c, "shop")[0]
	if rss := shimRSS(t, cfg); rss != "" {
		t.Logf("shim resident memory: %s KiB", rss)
	}

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	direct := fmt.Sprintf("http://127.0.0.1:%d/pid", before.Port)
	res, err := http.Get(direct)
	if err != nil {
		t.Fatalf("the app stopped with the server: %v", err)
	}
	res.Body.Close()

	addrs, _ = start(t, cfg)
	c = apiClient(t, cfg, addrs)
	after := instances(t, c, "shop")
	if len(after) != 1 || after[0].PID != before.PID || after[0].ID != before.ID || after[0].Status != "running" {
		t.Errorf("after a server restart: %+v, before %+v", after, before)
	}
	waitUntil(t, "the adopted instance to serve", func() bool {
		_, body := get(t, addrs.HTTP, "shop.localhost", "/")
		return body == "hello from v1"
	})
}

// shimRSS returns the resident memory of a running shim, where ps exists.
func shimRSS(t *testing.T, cfg config.Config) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(cfg.DataDir, "run", "shims", "*.state.json"))
	if len(matches) == 0 {
		return ""
	}
	data, _ := os.ReadFile(matches[0])
	var pid int
	if i := strings.Index(string(data), `"shim_pid":`); i >= 0 {
		fmt.Sscanf(string(data)[i+len(`"shim_pid":`):], "%d", &pid)
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestDeadShimIsReplaced(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	before := instances(t, c, "shop")[0]
	data, err := os.ReadFile(filepath.Join(cfg.DataDir, "run", "shims", before.ID+".state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shimPID int
	fmt.Sscanf(string(data)[strings.Index(string(data), `"shim_pid":`)+len(`"shim_pid":`):], "%d", &shimPID)
	p, _ := os.FindProcess(shimPID)
	p.Kill()

	waitUntil(t, "a replacement instance", func() bool {
		ins := instances(t, c, "shop")
		return len(ins) == 1 && ins[0].ID != before.ID && ins[0].Healthy
	})
	// The old app is gone, whatever the operating system.
	waitUntil(t, "the old app to be gone", func() bool {
		_, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", before.Port))
		return err != nil
	})
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "hello from v1" {
		t.Errorf("served %q", body)
	}
}

func TestCrashAndSickInstances(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	before := instances(t, c, "shop")[0]

	// A crash: the shim restarts the app in place.
	get(t, addrs.HTTP, "shop.localhost", "/crash")
	waitUntil(t, "the app to come back", func() bool {
		ins := instances(t, c, "shop")
		return len(ins) == 1 && ins[0].ID == before.ID && ins[0].PID != before.PID && ins[0].Restarts == 1 && ins[0].Healthy
	})

	// Failing health checks: no traffic, then a restart. (Right after a
	// crash the restarted process may not listen yet; wait until it answers.)
	waitUntil(t, "the restarted app to answer", func() bool {
		res, body := get(t, addrs.HTTP, "shop.localhost", "/")
		return res.StatusCode == 200 && body == "hello from v1"
	})
	afterCrash := instances(t, c, "shop")[0]
	waitUntil(t, "the app to be made sick", func() bool {
		_, body := get(t, addrs.HTTP, "shop.localhost", "/sick")
		return body == "now sick"
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		ins := instances(t, c, "shop")
		if len(ins) == 1 && ins[0].PID != afterCrash.PID && ins[0].Healthy {
			break
		}
		if time.Now().After(deadline) {
			logText, _ := os.ReadFile(filepath.Join(cfg.DataDir, "logs", "router", "shop", "production.log"))
			appLogs, _ := filepath.Glob(filepath.Join(cfg.DataDir, "logs", "apps", "shop", "production", "*.log"))
			for _, f := range appLogs {
				b, _ := os.ReadFile(f)
				logText = append(logText, b...)
			}
			t.Fatalf("the sick instance was not restarted: %+v (before: %+v)\n%s", ins, afterCrash, logText)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "hello from v1" {
		t.Errorf("after recovering served %q", body)
	}
}

func TestUpgradeAndWorker(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	plan := appPlan(nil)
	plan.Processes["worker"] = manifest.Process{Command: testroles.AppCommand(), Health: manifest.Health{MinUptime: duration("200ms")}}
	if _, err := deployApp(t, c, "shop", plan); err != nil {
		t.Fatal(err)
	}
	ins := instances(t, c, "shop")
	if len(ins) != 2 || ins[0].Process != "web" || ins[1].Process != "worker" || ins[1].Port != 0 || ins[1].Status != "running" {
		t.Errorf("instances: %+v", ins)
	}

	// A proxied protocol upgrade.
	conn, err := net.Dial("tcp", addrs.HTTP.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /upgrade HTTP/1.1\r\nHost: shop.localhost\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade answered %q", status)
	}
	for line, _ := br.ReadString('\n'); line != "\r\n" && line != ""; line, _ = br.ReadString('\n') {
	}
	fmt.Fprintf(conn, "ping\n")
	if echo, _ := br.ReadString('\n'); echo != "echo: ping\n" {
		t.Errorf("echo = %q", echo)
	}

	// Deploying a static site to the app retires its processes.
	site := writeSite(t, map[string]string{"index.html": "static now"})
	if _, _, err := c.DeployStatic(context.Background(), "shop", "production", site, manifest.Static{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "static now" {
		t.Errorf("served %q", body)
	}
	if ins := instances(t, c, "shop"); len(ins) != 0 {
		t.Errorf("processes still run after a static deploy: %+v", ins)
	}
}
