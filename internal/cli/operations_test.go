package cli

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/doctor"
	"github.com/robert-mcdermott/moorage/internal/service"
)

// TestDeployImage goes through the command, to what the server receives: the
// image's name, and of the directory's files, the manifest alone.
func TestDeployImage(t *testing.T) {
	var image string
	var files []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/apps/shop/envs/production/deployments") {
			http.NotFound(w, r)
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			switch part.FormName() {
			case apitypes.ImagePart:
				data, _ := io.ReadAll(part)
				image = string(data)
			case apitypes.SourcePart:
				if image == "" {
					t.Error("the source came before the image: the server would build it")
				}
				gz, err := gzip.NewReader(part)
				if err != nil {
					t.Error(err)
					return
				}
				tr := tar.NewReader(gz)
				for {
					h, err := tr.Next()
					if err != nil {
						break
					}
					files = append(files, h.Name)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(apitypes.Deployment{ID: "dep_1", App: "shop", Status: "queued"})
	}))
	defer srv.Close()
	t.Setenv("MOOR_CONFIG", filepath.Join(t.TempDir(), "client.toml"))

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "moorage.toml"), []byte("[app]\nname = \"shop\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "not-sent.txt"), []byte("no"), 0o644)
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("no"), 0o644)

	code, out, errOut := run(t, "deploy", dir, "--image", "ghcr.io/acme/api:1.4", "--detach", "--server", srv.URL, "--token", "x")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	if image != "ghcr.io/acme/api:1.4" || len(files) != 1 || files[0] != "moorage.toml" {
		t.Errorf("image %q, files %v", image, files)
	}
	if !strings.Contains(out, "ghcr.io/acme/api:1.4") || !strings.Contains(out, "moor deployments follow dep_1") {
		t.Errorf("%s", out)
	}

	for _, args := range [][]string{
		{"--image", "-bad", "--app", "shop"},
		{"--image", "has space", "--app", "shop"},
		{"--image", "a:1", "--static", "--app", "shop"},
		{"--image", "a:1", "--artifact", "--app", "shop"},
	} {
		if code, _, _ := run(t, append([]string{"deploy", dir, "--server", srv.URL, "--token", "x"}, args...)...); code == 0 {
			t.Errorf("%v was accepted", args)
		}
	}
}

func localConfig(t *testing.T, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "server.toml")
	t.Setenv("MOORAGE_CONFIG", path)
	t.Setenv("MOOR_CONFIG", filepath.Join(dir, "client.toml"))
	all := append([]string{"server", "init", "--config", path, "--data-dir", filepath.Join(dir, "data")}, args...)
	if code, _, errOut := run(t, all...); code != 0 {
		t.Fatal(errOut)
	}
	return path
}

func freePort(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	data, _ := os.ReadFile(path)
	text := strings.ReplaceAll(string(data), "127.0.0.1:8080", addr)
	text = strings.ReplaceAll(text, `":2222"`, `""`)
	os.WriteFile(path, []byte(text), 0o644)
}

func TestDoctorCommand(t *testing.T) {
	path := localConfig(t, "--preset", "local")
	freePort(t, path)
	code, out, errOut := run(t, "server", "doctor", "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	var checks []doctor.Check
	if err := json.Unmarshal([]byte(out), &checks); err != nil {
		t.Fatal(err)
	}
	if checks[0].Name != "config" || checks[0].Detail != path || checks[1].Name != "server" || checks[1].Status != doctor.Warn {
		t.Errorf("%+v", checks[:2])
	}
	code, out, _ = run(t, "server", "doctor")
	if code != 0 || !strings.Contains(out, "! server") || !strings.Contains(out, "→ moor server start") || !strings.Contains(out, "warning") {
		t.Errorf("%d %s", code, out)
	}

	// Something else on its port is a problem, and the exit status says so.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	data, _ := os.ReadFile(path)
	cfg, err := config.Load(path, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(strings.ReplaceAll(string(data), cfg.Listen.HTTP, ln.Addr().String())), 0o644)
	code, out, errOut = run(t, "server", "doctor")
	if code == 0 || !strings.Contains(out, "✘ listen.http") || !strings.Contains(errOut, "has problems") {
		t.Errorf("%d %s %s", code, out, errOut)
	}

	t.Setenv("MOORAGE_CONFIG", filepath.Join(t.TempDir(), "none.toml"))
	if code, _, errOut := run(t, "server", "doctor"); code == 0 || !strings.Contains(errOut, "moor server init") {
		t.Errorf("%d %s", code, errOut)
	}
}

func TestProxyConfigCommand(t *testing.T) {
	localConfig(t, "--domain", "apps.example.com", "--behind-proxy")
	code, out, errOut := run(t, "server", "proxy-config", "nginx", "--cert", "/c.crt", "--key", "/c.key", "--https-port", "8443")
	if code != 0 || errOut != "" || !strings.Contains(out, "*.apps.example.com") || !strings.Contains(out, "listen 8443 ssl;") || !strings.Contains(out, "/c.crt") {
		t.Errorf("%d %q %s", code, errOut, out)
	}
	if code, _, errOut := run(t, "server", "proxy-config", "apache"); code == 0 || !strings.Contains(errOut, "not \"apache\"") {
		t.Errorf("%d %s", code, errOut)
	}
	if code, _, errOut := run(t, "server", "proxy-config"); code == 0 || errOut == "" {
		t.Errorf("no proxy named: %d", code)
	}

	// A server that terminates TLS itself gets the configuration, with a warning.
	localConfig(t, "--domain", "apps.example.com")
	code, out, errOut = run(t, "server", "proxy-config", "nginx")
	if code != 0 || !strings.Contains(errOut, "not set up to run behind a proxy") || !strings.Contains(errOut, "tls.mode") || !strings.Contains(out, "upstream moorage") {
		t.Errorf("%d %q", code, errOut)
	}
}

func setResolver(t *testing.T, goos string) string {
	t.Helper()
	oldDir, oldOS := resolverDir, resolverGOOS
	resolverDir, resolverGOOS = filepath.Join(t.TempDir(), "resolver"), goos
	t.Cleanup(func() { resolverDir, resolverGOOS = oldDir, oldOS })
	return resolverDir
}

func TestResolverCommands(t *testing.T) {
	dir := setResolver(t, "darwin")
	path := localConfig(t, "--preset", "local")
	before, _ := os.ReadFile(path)

	code, out, errOut := run(t, "server", "resolver", "install")
	if code != 0 || !strings.Contains(out, "Restart the server") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	rule, err := os.ReadFile(filepath.Join(dir, "localhost"))
	if err != nil || !strings.Contains(string(rule), "nameserver 127.0.0.1\nport 15353\n") {
		t.Errorf("%v %q", err, rule)
	}
	cfg, err := config.Load(path, os.Getenv)
	if err != nil || cfg.Listen.DNS != defaultDNSAddr {
		t.Errorf("%v %q", err, cfg.Listen.DNS)
	}

	// Again: it is in place, and the server need not restart.
	code, out, _ = run(t, "server", "resolver", "install")
	if code != 0 || strings.Contains(out, "Restart the server") {
		t.Errorf("%d %s", code, out)
	}
	if code, out, _ := run(t, "server", "resolver"); code != 0 || !strings.Contains(out, "is in place") || !strings.Contains(out, defaultDNSAddr) {
		t.Errorf("%d %s", code, out)
	}

	if code, _, errOut := run(t, "server", "resolver", "remove"); code != 0 {
		t.Fatal(errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "localhost")); !os.IsNotExist(err) {
		t.Errorf("the rule stayed: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("server.toml changed:\n%s", after)
	}
	if code, _, errOut := run(t, "server", "resolver", "remove"); code != 0 {
		t.Errorf("removing twice: %s", errOut)
	}
}

func TestResolverElsewhere(t *testing.T) {
	// A real domain takes a real record.
	dir := setResolver(t, "darwin")
	localConfig(t, "--domain", "apps.example.com")
	if code, _, errOut := run(t, "server", "resolver", "install"); code == 0 || !strings.Contains(errOut, "real DNS record") {
		t.Errorf("%d %s", code, errOut)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("it wrote a rule: %v", err)
	}

	localConfig(t, "--preset", "local")
	setResolver(t, "linux")
	if code, out, _ := run(t, "server", "resolver", "install"); code != 0 || !strings.Contains(out, "Nothing to install on Linux") {
		t.Errorf("%d %s", code, out)
	}
	setResolver(t, "plan9")
	if code, _, errOut := run(t, "server", "resolver", "install"); code == 0 || !strings.Contains(errOut, "not available on plan9") {
		t.Errorf("%d %s", code, errOut)
	}
}

// fakeManager is a service whose unit is a file of the test's.
type fakeManager struct {
	service.Manager
	file string
}

func (m fakeManager) File() string { return m.file }

func (m fakeManager) Definition() ([]byte, error) { return os.ReadFile(m.file) }

func TestStopLeavesAnotherServersService(t *testing.T) {
	unit := filepath.Join(t.TempDir(), "moorage.service")
	os.WriteFile(unit, []byte("ExecStart=/usr/local/bin/moor server run --config /etc/moorage/server.toml\n"), 0o644)
	m := fakeManager{file: unit}
	if !runsServer(m, "/etc/moorage/server.toml") {
		t.Error("its own configuration")
	}
	if runsServer(m, "/tmp/trying-it-out/server.toml") {
		t.Error("a server of another configuration would stop the service")
	}
	if runsServer(fakeManager{file: filepath.Join(t.TempDir(), "none")}, "/etc/moorage/server.toml") {
		t.Error("no unit, no service")
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	old := serviceFor
	serviceFor = func(bool) (service.Manager, error) { return nil, service.ErrUnsupported }
	defer func() { serviceFor = old }()
	path := localConfig(t, "--preset", "local")
	freePort(t, path)
	code, out, errOut := run(t, "server", "stop", "--all")
	if code != 0 || !strings.Contains(out, "not running") {
		t.Errorf("%d %s %s", code, out, errOut)
	}
}
