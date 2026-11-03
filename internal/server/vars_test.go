package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
)

// secretValue is long enough to be redacted from logs.
const secretValue = "s3cr3t-api-key-0f9a7c"

func setVars(t *testing.T, c *client.Client, app string, ch apitypes.VarsChange) apitypes.VarsResult {
	t.Helper()
	res, err := c.ChangeVars(context.Background(), app, "production", ch)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// filesHolding returns the files under dir whose bytes contain s.
func filesHolding(t *testing.T, dir, s string) []string {
	t.Helper()
	var found []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(p); err == nil && bytes.Contains(data, []byte(s)) {
			found = append(found, strings.TrimPrefix(p, dir))
		}
		return nil
	})
	return found
}

func envOf(t *testing.T, addrs Addrs, app, name string) string {
	t.Helper()
	_, body := get(t, addrs.HTTP, app+".localhost", "/env?name="+name)
	return body
}

func TestVariablesAreReleases(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	plan := appPlan(map[string]string{"TESTAPP_BODY": "from the manifest", "TESTAPP_PRINT": "API_KEY"})
	web := plan.Processes["web"]
	two := 2
	web.Instances = &two
	plan.Processes["web"] = web
	if _, err := deployApp(t, c, "shop", plan); err != nil {
		t.Fatal(err)
	}

	// A change of variables is a release, rolled out under load without a
	// failed request.
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
				if res.StatusCode != 200 || (string(body) != "from the manifest" && string(body) != "from the server") {
					failures.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("%d %.80s", res.StatusCode, body))
				}
			}
		}()
	}
	res := setVars(t, c, "shop", apitypes.VarsChange{Set: []apitypes.VarSet{
		{Name: "TESTAPP_BODY", Value: "from the server"},
		{Name: "API_KEY", Value: secretValue, Secret: true},
	}})
	close(stop)
	wg.Wait()
	t.Logf("%d requests across a change of variables; %d failed", requests.Load(), failures.Load())
	if failures.Load() > 0 {
		t.Errorf("%d of %d requests failed; first: %v", failures.Load(), requests.Load(), first.Load())
	}
	if res.Deployment == nil || res.Deployment.Release.Version != 2 || res.Deployment.Source != "config" {
		t.Fatalf("result: %+v", res)
	}
	rels, _ := c.Releases(ctx, "shop", "production")
	if rels[0].Reason != "config" || rels[0].Build != rels[1].Build {
		t.Errorf("the change should release the same build: %+v", rels)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "from the server" {
		t.Errorf("served %q", body)
	}
	if got := envOf(t, addrs, "shop", "API_KEY"); got != secretValue {
		t.Errorf("the app sees API_KEY=%q", got)
	}

	// The secret is never shown, and no file of the server holds it.
	vars, err := c.Vars(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]apitypes.Var{}
	for _, v := range vars {
		byName[v.Name+"/"+v.Source] = v
	}
	if v := byName["API_KEY/env"]; !v.Secret || v.Value != "" {
		t.Errorf("API_KEY is listed as %+v", v)
	}
	if v := byName["TESTAPP_BODY/manifest"]; !v.Overridden || v.Value != "from the manifest" {
		t.Errorf("the manifest's TESTAPP_BODY is listed as %+v", v)
	}
	if found := filesHolding(t, cfg.DataDir, secretValue); len(found) > 0 {
		t.Errorf("files holding the secret in the clear: %v", found)
	}
	logs := filesHolding(t, cfg.DataDir, "API_KEY is [secret]")
	if len(logs) == 0 {
		t.Error("the app printed its secret, and no log shows it redacted")
	}

	// A restart keeps the variables; --no-restart keeps a change for later.
	if _, err := c.Restart(ctx, "shop", "production"); err != nil {
		t.Fatal(err)
	}
	if got := envOf(t, addrs, "shop", "API_KEY"); got != secretValue {
		t.Errorf("after a restart the app sees API_KEY=%q", got)
	}
	staged := setVars(t, c, "shop", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "LATER", Value: "1"}}, NoRestart: true})
	if staged.Deployment != nil {
		t.Errorf("--no-restart released: %+v", staged.Deployment)
	}
	if got := envOf(t, addrs, "shop", "LATER"); got != "" {
		t.Errorf("a staged variable reached the app: %q", got)
	}

	// Rolling back keeps the current variables unless asked otherwise.
	d, err := c.RollbackWith(ctx, "shop", "production", apitypes.RollbackRequest{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "from the server" || envOf(t, addrs, "shop", "LATER") != "1" {
		t.Errorf("a rollback to v1 (v%d) serves %q", d.Release.Version, body)
	}
	if _, err := c.RollbackWith(ctx, "shop", "production", apitypes.RollbackRequest{Version: 1, WithConfig: true}); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "from the manifest" || envOf(t, addrs, "shop", "API_KEY") != "" {
		t.Errorf("a rollback to v1 with its configuration serves %q", body)
	}

	// Unsetting reports names that were not set.
	res = setVars(t, c, "shop", apitypes.VarsChange{Unset: []string{"TESTAPP_BODY", "NOPE"}})
	if len(res.NotSet) != 1 || res.NotSet[0] != "NOPE" || res.Deployment == nil {
		t.Errorf("unset: %+v", res)
	}

	for _, bad := range []apitypes.VarsChange{
		{Set: []apitypes.VarSet{{Name: "PORT", Value: "1"}}},
		{Set: []apitypes.VarSet{{Name: "MOORAGE_APP", Value: "x"}}},
		{Set: []apitypes.VarSet{{Name: "2BAD", Value: "x"}}},
		{Set: []apitypes.VarSet{{Name: "A", Value: "1"}}, Unset: []string{"A"}},
		{},
	} {
		var apiErr *client.APIError
		if _, err := c.ChangeVars(ctx, "shop", "production", bad); !errors.As(err, &apiErr) || apiErr.Status != 422 {
			t.Errorf("change %+v: %v", bad, err)
		}
	}
}

func TestVariablesBeforeTheFirstDeploy(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	res := setVars(t, c, "fresh", apitypes.VarsChange{AllEnvs: true, Set: []apitypes.VarSet{{Name: "TESTAPP_BODY", Value: "configured first"}}})
	if !res.AppCreated || res.Deployment != nil {
		t.Errorf("result: %+v", res)
	}
	if _, err := deployApp(t, c, "fresh", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, addrs.HTTP, "fresh.localhost", "/"); body != "configured first" {
		t.Errorf("served %q", body)
	}
}

func TestBuildVariablesAndTheMasterKey(t *testing.T) {
	cfg := testConfig(t)
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	setVars(t, c, "shop", apitypes.VarsChange{Set: []apitypes.VarSet{
		{Name: "BUILD_TOKEN", Value: secretValue, Secret: true, Build: true},
		{Name: "TESTAPP_BODY", Value: "built"},
	}})
	d, log := deploySource(t, c, "shop", map[string]string{
		"moorage.toml": sourceManifest("ignored", `echo "token $BUILD_TOKEN" && echo "$BUILD_TOKEN" > token.txt`, okRelease),
	})
	if d.Status != "live" {
		t.Fatalf("deployment: %+v\n%s", d, log)
	}
	if !strings.Contains(log, "token [secret]") || strings.Contains(log, secretValue) {
		t.Errorf("the build log does not hide the secret:\n%s", log)
	}
	token, _ := os.ReadFile(filepath.Join(cfg.DataDir, "builds", "shop", d.Build, "app", "token.txt"))
	if strings.TrimSpace(string(token)) != secretValue {
		t.Errorf("the build saw BUILD_TOKEN=%q", token)
	}
	if _, body := get(t, addrs.HTTP, "shop.localhost", "/"); body != "built" {
		t.Errorf("served %q", body)
	}

	// A new server with the same key reads the secrets; one without the key
	// refuses to start.
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	addrs, stop = start(t, cfg)
	c = apiClient(t, cfg, addrs)
	if _, err := c.Restart(context.Background(), "shop", "production"); err != nil {
		t.Fatalf("restart after the server restarted: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(cfg.DataDir, "keys", "master.key"), filepath.Join(cfg.DataDir, "master.key.saved"))
	log2, done := testLogger(t)
	defer done()
	err := Run(context.Background(), cfg, log2, testOptions(nil))
	if err == nil || !strings.Contains(err.Error(), "master key") || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a server without its master key: %v", err)
	}
}
