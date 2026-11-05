package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/store"
	"net/http"
)

const boxImage = "docker.io/library/busybox:1.37"

// containerEngine is the engine the tests use, named by
// MOORAGE_TEST_CONTAINERS: they need one, and the network for its images.
func containerEngine(t *testing.T) *container.Engine {
	t.Helper()
	setting := os.Getenv("MOORAGE_TEST_CONTAINERS")
	if setting == "" {
		t.Skip("set MOORAGE_TEST_CONTAINERS to run this against a container engine")
	}
	e, err := container.Find(setting)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// startContainers runs a server with the engine, and leaves no container or
// image of its behind.
func startContainers(t *testing.T, e *container.Engine) (config.Config, Addrs, func() error, *client.Client) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Containers.Engine = e.Bin
	t.Cleanup(func() { removeContainers(t, e, cfg) })
	addrs, stop := start(t, cfg)
	return cfg, addrs, stop, apiClient(t, cfg, addrs)
}

func removeContainers(t *testing.T, e *container.Engine, cfg config.Config) {
	out, err := exec.Command(e.Bin, "ps", "-aq", "--filter", "label=io.moorage.managed=true", "--filter", "label=io.moorage.server="+ContainerLabel(cfg)).Output()
	if err != nil {
		t.Logf("list the containers: %v", err)
	}
	for _, id := range strings.Fields(string(out)) {
		exec.Command(e.Bin, "rm", "-f", id).Run()
	}
	images, _ := exec.Command(e.Bin, "images", "-q", "--filter", "reference=localhost/moorage/*").Output()
	for _, id := range strings.Fields(string(images)) {
		exec.Command(e.Bin, "image", "rm", "-f", id).Run()
	}
}

func engineOutput(t *testing.T, e *container.Engine, args ...string) string {
	t.Helper()
	out, err := exec.Command(e.Bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", e.Bin, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// boxedApp is an app in an image of its own.
func boxedApp(body, extra string) map[string]string {
	return map[string]string{
		"moorage.toml": `[app]
runtime = "dockerfile"

[container]
user = "1000"

[resources]
memory = "64MiB"
pids = 64

[release]
command = 'echo "$MOORAGE_RELEASE" > "$MOORAGE_DATA_DIR/migrated-$MOORAGE_RELEASE"'

[processes.web]
stop_timeout = "2s"
health = { path = "/index.html", interval = "200ms", startup = "60s" }

[deploy]
drain = "100ms"
keep_releases = 3

[env]
FROM_ENV_TABLE = "yes"
` + extra,
		"Containerfile": `FROM ` + boxImage + `
RUN mkdir -p /www && chmod 777 /www
COPY index.html /www/index.html
COPY start.sh /start.sh
EXPOSE 9000
CMD ["/bin/sh", "/start.sh"]
`,
		"index.html": body,
		"start.sh": `env | sort > /www/env.txt
id -u > /www/uid.txt
echo "started with token=$API_TOKEN"
echo "to stderr" >&2
touch "$MOORAGE_DATA_DIR/started-$MOORAGE_RELEASE"
exec httpd -f -p "$PORT" -h /www
`,
	}
}

func containerName(t *testing.T, e *container.Engine, cfg config.Config, instance string) string {
	t.Helper()
	return engineOutput(t, e, "ps", "-a", "--filter", "label=io.moorage.server="+ContainerLabel(cfg), "--filter", "label=io.moorage.instance="+instance, "--format", "{{.Names}}")
}

func appLog(t *testing.T, cfg config.Config, app string) string {
	t.Helper()
	var all strings.Builder
	root := filepath.Join(cfg.DataDir, "logs", "apps", app)
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			all.Write(data)
		}
		return nil
	})
	return all.String()
}

// TestContainerApp takes an app with an image of its own through its life: a
// build, a release command, variables and secrets, a new release, a rollback, a
// server restart, a crash, and deploys that fail.
func TestContainerApp(t *testing.T) {
	e := containerEngine(t)
	cfg, addrs, stop, c := startContainers(t, e)
	ctx := context.Background()
	host := "boxed." + cfg.Domain
	const secret = "correct-horse-battery-staple"
	setVars(t, c, "boxed", apitypes.VarsChange{Set: []apitypes.VarSet{{Name: "API_TOKEN", Value: secret, Secret: true}, {Name: "PLAIN", Value: "visible"}}, NoRestart: true})

	d, log := deploySource(t, c, "boxed", boxedApp("first", ""))
	if d.Status != "live" || d.Release == nil || d.Release.Version != 1 {
		t.Fatalf("deployment: %+v\n%s", d, log)
	}
	for _, want := range []string{"detected   dockerfile · Containerfile", "▸ build", "▸ image      localhost/moorage/boxed:", "▸ release", "✔ live"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in the log:\n%s", want, log)
		}
	}
	if _, body := get(t, addrs.HTTP, host, "/index.html"); body != "first" {
		t.Errorf("served: %q", body)
	}

	// The environment is the container's, not the server's.
	_, env := get(t, addrs.HTTP, host, "/env.txt")
	for _, want := range []string{
		"PORT=9000\n", "HOST=0.0.0.0\n", "MOORAGE_DATA_DIR=/data\n", "MOORAGE_RELEASE=v1\n", "MOORAGE_APP=boxed\n",
		"MOORAGE_PROCESS=web\n", "FROM_ENV_TABLE=yes\n", "PLAIN=visible\n", "API_TOKEN=" + secret + "\n",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("no %q in its environment:\n%s", want, env)
		}
	}
	if strings.Contains(env, "MOORAGE_BIN") || strings.Contains(env, cfg.DataDir) {
		t.Errorf("the server's paths in the container's environment:\n%s", env)
	}
	if _, uid := get(t, addrs.HTTP, host, "/uid.txt"); strings.TrimSpace(uid) != "1000" {
		t.Errorf("runs as %q", uid)
	}

	// Its output is in its log, with the secret removed, in both streams.
	waitUntil(t, "the app's output", func() bool { return strings.Contains(appLog(t, cfg, "boxed"), "to stderr") })
	applog := appLog(t, cfg, "boxed")
	if !strings.Contains(applog, "started with token=[secret]") {
		t.Errorf("the log:\n%s", applog)
	}
	if hits := filesHolding(t, cfg.DataDir, secret); len(hits) != 0 {
		t.Errorf("the secret is on disk in the clear: %v", hits)
	}

	// The release command ran with the data directory, as a user of the image.
	data := filepath.Join(cfg.DataDir, "data", "boxed", "production")
	if got, _ := os.ReadFile(filepath.Join(data, "migrated-v1")); strings.TrimSpace(string(got)) != "v1" {
		t.Errorf("the release command: %q", got)
	}
	if _, err := os.Stat(filepath.Join(data, "started-v1")); err != nil {
		t.Errorf("the app, in its data directory: %v", err)
	}

	// How it runs.
	first := instances(t, c, "boxed")[0]
	if first.Status != "running" || !first.Healthy || first.Port == 0 || first.PID == 0 {
		t.Errorf("instance: %+v", first)
	}
	name := containerName(t, e, cfg, first.ID)
	if !strings.HasPrefix(name, "moorage-boxed-production-web-1-") {
		t.Errorf("name: %q", name)
	}
	ports := engineOutput(t, e, "port", name)
	if !strings.Contains(ports, "127.0.0.1:"+strconv.Itoa(first.Port)) || strings.Contains(ports, "0.0.0.0") {
		t.Errorf("published: %s", ports)
	}
	if got := engineOutput(t, e, "inspect", "--format", `{{index .Config.Labels "io.moorage.release"}} {{index .Config.Labels "io.moorage.app"}}`, name); got != "v1 boxed" {
		t.Errorf("labels: %s", got)
	}

	// A new release replaces it; the old container goes.
	d, log = deploySource(t, c, "boxed", boxedApp("second", ""))
	if d.Status != "live" || d.Release.Version != 2 {
		t.Fatalf("second deployment: %+v\n%s", d, log)
	}
	if _, body := get(t, addrs.HTTP, host, "/index.html"); body != "second" {
		t.Errorf("served: %q", body)
	}
	if got := containerName(t, e, cfg, first.ID); got != "" {
		t.Errorf("the old container is still there: %s", got)
	}

	// A rollback runs the earlier image.
	if _, err := c.Rollback(ctx, "boxed", "production", 1); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, addrs.HTTP, host, "/index.html"); body != "first" {
		t.Errorf("after a rollback: %q", body)
	}

	// A restart of the server leaves the container running, and adopts it.
	before := instances(t, c, "boxed")[0]
	beforeName := containerName(t, e, cfg, before.ID)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := engineOutput(t, e, "inspect", "--format", "{{.State.Running}}", beforeName); got != "true" {
		t.Errorf("the container stopped with the server: %s", got)
	}
	addrs, stop = start(t, cfg)
	c = apiClient(t, cfg, addrs)
	after := instances(t, c, "boxed")
	if len(after) != 1 || after[0].ID != before.ID || after[0].Status != "running" {
		t.Fatalf("after a restart: %+v, before %+v", after, before)
	}
	waitUntil(t, "the adopted container to serve", func() bool {
		_, body := get(t, addrs.HTTP, host, "/index.html")
		return body == "first"
	})
	// Its log is not recorded twice.
	if n := strings.Count(appLog(t, cfg, "boxed"), "to stderr"); n != 3 {
		t.Errorf("three containers wrote a line each, and the log has %d", n)
	}

	// A crash: the container is started again, with the same address.
	engineOutput(t, e, "kill", beforeName)
	waitUntil(t, "the restart", func() bool {
		ins := instances(t, c, "boxed")
		return len(ins) == 1 && ins[0].ID == before.ID && ins[0].Restarts >= 1 && ins[0].Status == "running" && ins[0].LastExit != nil
	})
	waitUntil(t, "it to serve again", func() bool {
		_, body := get(t, addrs.HTTP, host, "/index.html")
		return body == "first"
	})

	// A container that exits fails the deploy, with what it said. The last good
	// release keeps serving.
	bad := boxedApp("bad", "")
	bad["start.sh"] = "echo the-reason-it-failed\nexit 3\n"
	d, log = deploySource(t, c, "boxed", bad)
	if d.Status != "failed" || !strings.Contains(d.Error, "exited") || !strings.Contains(strings.Join(d.Output, "\n"), "the-reason-it-failed") {
		t.Errorf("a crashing container: %+v\n%s", d, log)
	}
	if _, body := get(t, addrs.HTTP, host, "/index.html"); body != "first" {
		t.Errorf("after a failed deploy: %q", body)
	}

	// So do a Containerfile that does not build, and a failed release command.
	broken := boxedApp("broken", "")
	broken["Containerfile"] = "FROM " + boxImage + "\nRUN echo failing-step && exit 5\n"
	d, log = deploySource(t, c, "boxed", broken)
	if d.Status != "failed" || !strings.Contains(d.Error, "did not build") || !strings.Contains(log, "failing-step") {
		t.Errorf("a broken Containerfile: %+v\n%s", d, log)
	}
	norelease := boxedApp("norelease", "")
	norelease["moorage.toml"] = strings.Replace(norelease["moorage.toml"], `echo "$MOORAGE_RELEASE" > "$MOORAGE_DATA_DIR/migrated-$MOORAGE_RELEASE"`, "echo cannot-migrate-$API_TOKEN; exit 9", 1)
	d, log = deploySource(t, c, "boxed", norelease)
	if d.Status != "failed" || !strings.Contains(d.Error, "release command") || !strings.Contains(log, "cannot-migrate-[secret]") || strings.Contains(log, secret) {
		t.Errorf("a failed release command: %+v\n%s", d, log)
	}
	if _, body := get(t, addrs.HTTP, host, "/index.html"); body != "first" {
		t.Errorf("after failed deploys: %q", body)
	}

	// Old images go with their releases. keep_releases counts what is kept.
	for i := 3; i <= 6; i++ {
		if d, log := deploySource(t, c, "boxed", boxedApp("again", "")); d.Status != "live" {
			t.Fatalf("deploy %d: %+v\n%s", i, d, log)
		}
	}
	tags := engineOutput(t, e, "images", "--filter", "reference=localhost/moorage/boxed", "--format", "{{.Tag}}")
	if n := len(strings.Fields(tags)); n == 0 || n > 4 {
		t.Errorf("images kept: %q", tags)
	}
}

// TestContainerImageDeploy runs somebody else's image, pinned to its digest:
// with nothing but its name, a port and a command from the manifest.
func TestContainerImageDeploy(t *testing.T) {
	e := containerEngine(t)
	cfg, addrs, _, c := startContainers(t, e)
	ctx := context.Background()
	host := "image." + cfg.Domain

	dir := writeSite(t, map[string]string{
		"moorage.toml": `[app]
name = "image"

[container]
port = 8123

[processes.web]
command = 'echo served > /tmp/index.html && exec httpd -f -p "$PORT" -h /tmp'
health = { path = "/index.html", interval = "200ms", startup = "60s" }
`,
		"not-sent.txt": "only the manifest goes with an image",
	})
	onlyManifest := func(rel string, d fs.DirEntry) bool { return d.IsDir() || rel != "moorage.toml" }
	queued, _, err := c.DeployImage(ctx, "image", "production", boxImage, dir, onlyManifest)
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	d, err := c.FollowDeployment(ctx, queued.ID, func(l apitypes.LogLine) error {
		log.WriteString(l.Text + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "live" || d.Source != "image" || d.Ref != boxImage {
		t.Fatalf("deployment: %+v\n%s", d, log.String())
	}
	if !strings.Contains(log.String(), "▸ pull       "+boxImage) || !strings.Contains(log.String(), "pinned to ") {
		t.Errorf("the log:\n%s", log.String())
	}
	if _, body := get(t, addrs.HTTP, host, "/index.html"); strings.TrimSpace(body) != "served" {
		t.Errorf("served: %q", body)
	}
	in := instances(t, c, "image")[0]
	name := containerName(t, e, cfg, in.ID)
	image := engineOutput(t, e, "inspect", "--format", "{{.Config.Image}}", name)
	if !strings.Contains(image, "@sha256:") {
		t.Errorf("not pinned to a digest: %s", image)
	}
	if got := engineOutput(t, e, "port", name); !strings.HasPrefix(got, "8123/tcp") {
		t.Errorf("the manifest's port: %s", got)
	}

	// Deploying the image's name again, with the manifest it came with: its port.
	again, err := c.Redeploy(ctx, "image", "production")
	if err != nil {
		t.Fatal(err)
	}
	if again, err = c.FollowDeployment(ctx, again.ID, func(apitypes.LogLine) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if again.Status != "live" || again.Source != "image" || again.Ref != boxImage {
		t.Errorf("redeployed: %+v", again)
	}
	if got := engineOutput(t, e, "port", containerName(t, e, cfg, instances(t, c, "image")[0].ID)); !strings.HasPrefix(got, "8123/tcp") {
		t.Errorf("the manifest's port, after a redeploy: %s", got)
	}

	// Nothing in the image, and no manifest: the app needs a name, and the image
	// must exist.
	empty := t.TempDir()
	queued, _, err = c.DeployImage(ctx, "nothing", "production", "localhost:1/no/such:image", empty, onlyManifest)
	if err != nil {
		t.Fatal(err)
	}
	d, err = c.FollowDeployment(ctx, queued.ID, func(apitypes.LogLine) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "failed" || !strings.Contains(d.Error, "could not pull") {
		t.Errorf("an image that does not exist: %+v", d)
	}
}

// TestContainerSlowToListen is a published port that accepts connections before
// the app in the container listens: a TCP health check must not be fooled.
func TestContainerSlowToListen(t *testing.T) {
	e := containerEngine(t)
	cfg, addrs, _, c := startContainers(t, e)
	host := "slow." + cfg.Domain
	dir := writeSite(t, map[string]string{"moorage.toml": `[app]
name = "slow"

[processes.web]
command = 'sleep 4; echo ready > /tmp/index.html; exec httpd -f -p "$PORT" -h /tmp'
health = { interval = "200ms", startup = "60s" }
`})
	skip := func(rel string, d fs.DirEntry) bool { return d.IsDir() || rel != "moorage.toml" }
	started := time.Now()
	queued, _, err := c.DeployImage(context.Background(), "slow", "production", boxImage, dir, skip)
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.FollowDeployment(context.Background(), queued.ID, func(apitypes.LogLine) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "live" {
		t.Fatalf("%+v", d)
	}
	if took := time.Since(started); took < 4*time.Second {
		t.Errorf("live after %s, before the app listened", took)
	}
	res, body := get(t, addrs.HTTP, host, "/index.html")
	if res.StatusCode != 200 || strings.TrimSpace(body) != "ready" {
		t.Errorf("as soon as it was live: %d %q", res.StatusCode, body)
	}
}

// TestContainerWorkerAndLimits has a process that does not listen, and an app
// that runs out of memory.
func TestContainerWorkerAndLimits(t *testing.T) {
	e := containerEngine(t)
	_, _, _, c := startContainers(t, e)
	dir := writeSite(t, map[string]string{"moorage.toml": `[app]
name = "workers"

[resources]
memory = "16MiB"

[processes.worker]
command = 'echo working; exec sleep 600'
health = { min_uptime = "1s" }

[processes.hog]
command = 'sleep 5; tail /dev/zero'
restart = "never"
health = { min_uptime = "1s" }
`})
	skip := func(rel string, d fs.DirEntry) bool { return d.IsDir() || rel != "moorage.toml" }
	queued, _, err := c.DeployImage(context.Background(), "workers", "production", boxImage, dir, skip)
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.FollowDeployment(context.Background(), queued.ID, func(apitypes.LogLine) error { return nil })
	if err != nil || d.Status != "live" {
		t.Fatalf("%v %+v", err, d)
	}
	for _, in := range instances(t, c, "workers") {
		if in.Port != 0 {
			t.Errorf("a process that does not listen has a port: %+v", in)
		}
	}
	waitUntil(t, "the memory limit", func() bool {
		for _, in := range instances(t, c, "workers") {
			// Some engines report running out of memory; others only the kill.
			if in.Process == "hog" && in.Status == "exited" && in.LastExit != nil && (in.LastExit.Signal == "OOM" || in.LastExit.Signal == "KILL") {
				return true
			}
		}
		return false
	})
}

// The rest needs no engine.

func TestNoEngineSaysSo(t *testing.T) {
	cfg := testConfig(t)
	cfg.Containers.Engine = "no-such-engine"
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	d, log := deploySource(t, c, "boxed", map[string]string{"Containerfile": "FROM x\n"})
	if d.Status != "failed" || !strings.Contains(d.Error, "no container engine") {
		t.Errorf("%+v\n%s", d, log)
	}
	// Other apps do not need one.
	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Errorf("a native app: %v", err)
	}
}

// fakeEngine is a script that records the credentials a pull is given, and
// finds no image.
func fakeEngine(t *testing.T) (bin, seen string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	dir := t.TempDir()
	bin, seen = filepath.Join(dir, "podman"), filepath.Join(dir, "seen")
	script := `#!/bin/sh
case "$1" in
  --version) echo "podman version 5" ;;
  ps) ;;
  pull) cat "$REGISTRY_AUTH_FILE" > "` + seen + `"; ls -l "$REGISTRY_AUTH_FILE" >> "` + seen + `.mode"; exit 1 ;;
  *) echo "not found" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, seen
}

func TestRegistryCredentials(t *testing.T) {
	bin, seen := fakeEngine(t)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	const password = "registry-password-for-the-test"

	if got, err := c.Registries(ctx); err != nil || len(got) != 0 {
		t.Errorf("at first: %v %v", got, err)
	}
	reg, err := c.RegistryLogin(ctx, "Registry.Example.com:5000", apitypes.RegistryLogin{Username: "ci", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	if reg.Host != "registry.example.com:5000" || reg.Username != "ci" {
		t.Errorf("%+v", reg)
	}
	if _, err := c.RegistryLogin(ctx, "ghcr.io", apitypes.RegistryLogin{Username: "first", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegistryLogin(ctx, "ghcr.io", apitypes.RegistryLogin{Username: "second", Password: "y"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Registries(ctx)
	if err != nil || len(got) != 2 || got[0].Host != "ghcr.io" || got[0].Username != "second" {
		t.Errorf("listed: %+v %v", got, err)
	}
	for _, bad := range []struct {
		host string
		l    apitypes.RegistryLogin
	}{
		{"not a host", apitypes.RegistryLogin{Username: "u", Password: "p"}},
		{"ghcr.io", apitypes.RegistryLogin{Password: "p"}},
		{"ghcr.io", apitypes.RegistryLogin{Username: "u"}},
	} {
		if _, err := c.RegistryLogin(ctx, bad.host, bad.l); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}

	// The password is sealed on the server, and a pull gets it in a file that
	// is gone afterwards.
	if hits := filesHolding(t, cfg.DataDir, password); len(hits) != 0 {
		t.Errorf("the password is on disk: %v", hits)
	}
	queued, _, err := c.DeployImage(ctx, "private", "production", "registry.example.com:5000/x:1", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.FollowDeployment(ctx, queued.ID, func(apitypes.LogLine) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "failed" || !strings.Contains(d.Error, "moor registry login") {
		t.Errorf("%+v", d)
	}
	data, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	want := base64.StdEncoding.EncodeToString([]byte("ci:" + password))
	if file.Auths["registry.example.com:5000"].Auth != want {
		t.Errorf("the engine saw: %s", data)
	}
	if mode, _ := os.ReadFile(seen + ".mode"); !strings.HasPrefix(string(mode), "-rw-------") {
		t.Errorf("its file: %s", mode)
	}
	if hits := filesHolding(t, cfg.DataDir, want); len(hits) != 0 {
		t.Errorf("credentials left behind: %v", hits)
	}

	if err := c.RegistryLogout(ctx, "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	if err := c.RegistryLogout(ctx, "ghcr.io"); err == nil {
		t.Error("logged out twice")
	}

	// They survive a restart, and count as sealed with the master key: losing
	// the key is an error, not a new key.
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	has, err := db.HasSealed(ctx)
	db.Close()
	if err != nil || !has {
		t.Errorf("sealed: %v %v", has, err)
	}
	key := NewLayout(cfg.DataDir).MasterKey
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	log, done := testLogger(t)
	defer done()
	err = Run(ctx, cfg, log, testOptions(func(Addrs) {}))
	if err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("without the master key: %v", err)
	}
}

// wrappedEngine is the real engine behind a script that can pretend that its daemon is down, and writes down what it was asked.
func wrappedEngine(t *testing.T, e *container.Engine) (bin, down, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, down, calls = filepath.Join(dir, e.Kind), filepath.Join(dir, "down"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$1" >> "` + calls + `"
if [ "$1" != "--version" ] && [ -e "` + down + `" ]; then
  echo "Cannot connect to the daemon" >&2
  exit 125
fi
exec "` + e.Bin + `" "$@"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, down, calls
}

// A daemon that is down says nothing of what it was running, so its instances are not recorded as stopped, and nothing is started to take their place.
func TestContainerEngineDownAtStart(t *testing.T) {
	e := containerEngine(t)
	bin, down, calls := wrappedEngine(t, e)
	cfg := testConfig(t)
	cfg.Containers.Engine = bin
	t.Cleanup(func() { removeContainers(t, e, cfg) })
	addrs, stop := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	host := "boxed." + cfg.Domain
	d, log := deploySource(t, c, "boxed", boxedApp("first", ""))
	if d.Status != "live" {
		t.Fatalf("%+v\n%s", d, log)
	}
	before := instances(t, c, "boxed")
	if len(before) != 1 {
		t.Fatalf("%+v", before)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(down, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	addrs, stop = start(t, cfg)
	t.Cleanup(func() { stop() })
	c = apiClient(t, cfg, addrs)
	authn := testAuth(t, cfg)
	lives, err := authn.DB.LiveInstances(ctx)
	if err != nil || len(lives) != 1 || lives[0].ID != before[0].ID {
		t.Errorf("its record: %+v %v", lives, err)
	}
	info, err := c.ServerInfo(ctx)
	if err != nil || info.Containers.State != "not answering" || !strings.Contains(info.Containers.Problem, "start it") {
		t.Errorf("what the server says: %+v %v", info.Containers, err)
	}
	waitUntil(t, "an event saying so", func() bool {
		list, err := c.Events(ctx, "", "", 100)
		if err != nil {
			return false
		}
		for _, e := range list {
			if e.Type == "server.engine_down" {
				return true
			}
		}
		return false
	})
	// Nothing is started to fail every few seconds.
	time.Sleep(2 * time.Second)
	if data, _ := os.ReadFile(calls); strings.Contains(string(data), "run") {
		t.Errorf("it tried to start another:\n%s", data)
	}
	// A deployment of something in a container says what to do.
	failed, _ := deploySource(t, c, "boxed", boxedApp("second", ""))
	if failed.Status != "failed" || !strings.Contains(failed.Error, "not answering") {
		t.Errorf("deploying: %+v", failed)
	}

	// With the daemon back, it finds the same container, and needs no restart.
	if err := os.Remove(down); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the app to be served", func() bool {
		res, body := get(t, addrs.HTTP, host, "/index.html")
		return res.StatusCode == http.StatusOK && body == "first"
	})
	if after := instances(t, c, "boxed"); len(after) != 1 || after[0].ID != before[0].ID {
		t.Errorf("before %+v, after %+v", before, after)
	}
	out, err := exec.Command(e.Bin, "ps", "-q", "--filter", "label=io.moorage.instance="+before[0].ID).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Errorf("the container: %q %v", out, err)
	}
}
