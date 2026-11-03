package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Main(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != 0 || !strings.HasPrefix(out, "moor "+buildinfo.Version) {
		t.Errorf("version: code %d, output %q", code, out)
	}
	code, flagOut, _ := run(t, "--version")
	if code != 0 || flagOut != out {
		t.Errorf("--version: code %d, output %q, want %q", code, flagOut, out)
	}
	code, jsonOut, _ := run(t, "version", "--json")
	var info buildinfo.Info
	if code != 0 || json.Unmarshal([]byte(jsonOut), &info) != nil || info.Version != buildinfo.Version {
		t.Errorf("version --json: code %d, output %q", code, jsonOut)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"nonsense"},
		{"version", "--nonsense"},
		{"server", "init", "extra-argument"},
	} {
		code, _, errOut := run(t, args...)
		if code != exitUsage || !strings.Contains(errOut, "--help") {
			t.Errorf("moor %s: code %d, stderr %q; want %d and a pointer to --help", strings.Join(args, " "), code, errOut, exitUsage)
		}
	}
}

func TestServerInitLocal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "server.toml")
	dataDir := filepath.Join(dir, "data")
	code, out, errOut := run(t, "server", "init", "--preset", "local", "--config", path, "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("init failed: %s", errOut)
	}
	for _, want := range []string{path, "http://moorage.localhost:8080", "http://<app>.localhost:8080", "moor server run --config"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output does not mention %q:\n%s", want, out)
		}
	}
	cfg, err := config.Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != dataDir || cfg.Domain != "localhost" {
		t.Errorf("written configuration: %+v", cfg)
	}

	code, _, errOut = run(t, "server", "init", "--preset", "local", "--config", path, "--data-dir", dataDir)
	if code != exitError || !strings.Contains(errOut, "--force") {
		t.Errorf("second init: code %d, stderr %q", code, errOut)
	}
	if code, _, errOut = run(t, "server", "init", "--preset", "local", "--config", path, "--data-dir", dataDir, "--force"); code != 0 {
		t.Errorf("init --force: %s", errOut)
	}
}

func TestServerInitLan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.toml")
	code, _, errOut := run(t, "server", "init", "--config", path, "--data-dir", dir)
	if code != exitError || !strings.Contains(errOut, "--domain") {
		t.Errorf("lan without --domain: code %d, stderr %q", code, errOut)
	}

	code, out, errOut := run(t, "server", "init", "--config", path, "--data-dir", dir, "--domain", "apps.example.com")
	if code != 0 {
		t.Fatalf("lan init failed: %s", errOut)
	}
	for _, want := range []string{"https://moorage.apps.example.com", "https://<app>.apps.example.com", "wildcard DNS record, *.apps.example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("lan init output does not mention %q:\n%s", want, out)
		}
	}

	code, out, errOut = run(t, "server", "init", "--config", path, "--data-dir", dir, "--domain", "apps.example.com", "--behind-proxy", "--force")
	if code != 0 {
		t.Fatalf("behind-proxy init failed: %s", errOut)
	}
	if !strings.Contains(out, "TLS proxy at http://127.0.0.1:8080") || !strings.Contains(out, "https://moorage.apps.example.com\n") {
		t.Errorf("behind-proxy output:\n%s", out)
	}
}

func TestServerRunWithoutConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "server.toml")
	code, _, errOut := run(t, "server", "run", "--config", missing)
	if code != exitError || !strings.Contains(errOut, "moor server init") {
		t.Errorf("run without a configuration: code %d, stderr %q", code, errOut)
	}
}

func TestServerRunRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(path, []byte("domain = \"apps.example.com\"\n[listen]\nhttp = \"nope\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, "server", "run", "--config", path)
	if code != exitError || !strings.Contains(errOut, "data_dir is required") || !strings.Contains(errOut, "listen.http") {
		t.Errorf("run with an invalid configuration: code %d, stderr %q", code, errOut)
	}
}
