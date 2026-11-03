package server

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

// TestSampleApps builds and deploys every sample app of testdata/apps from
// source, with real toolchains downloaded and verified (spike S3), deploys
// a change, and rolls it back. It needs the network, so it runs only with
// MOORAGE_TEST_BUILDS=1. MOORAGE_TEST_TOOLCHAINS, if set, is a directory
// where toolchains and caches are kept between runs.
func TestSampleApps(t *testing.T) {
	if os.Getenv("MOORAGE_TEST_BUILDS") == "" {
		t.Skip("set MOORAGE_TEST_BUILDS=1 to build the sample apps (it downloads toolchains)")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the sample apps are built on Linux and macOS")
	}
	cfg := testConfig(t)
	if keep := os.Getenv("MOORAGE_TEST_TOOLCHAINS"); keep != "" {
		for _, d := range []string{"toolchains", "cache"} {
			shared, err := filepath.Abs(filepath.Join(keep, d))
			if err != nil {
				t.Fatal(err)
			}
			os.MkdirAll(shared, 0o755)
			if err := os.Symlink(shared, filepath.Join(cfg.DataDir, d)); err != nil {
				t.Fatal(err)
			}
		}
	}
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)

	for _, s := range []struct {
		dir  string
		want string // in the response, besides the message
	}{
		{"go", "from go (go1.26."},
		{"python", "from python 3.13."},
		{"node", "from node v24."},
		{"node-static", "from a site built with node v"},
		{"command", "from a command build (go1.26."},
		{"binary", "from a binary"},
		{"static", "from a static site"},
	} {
		t.Run(s.dir, func(t *testing.T) {
			app := "sample-" + s.dir
			src := copyTree(t, filepath.Join("..", "..", "testdata", "apps", s.dir))
			deploy := func(version int) {
				t.Helper()
				start := time.Now()
				queued, _, err := c.DeploySource(context.Background(), app, "production", src, nil)
				if err != nil {
					t.Fatal(err)
				}
				var log strings.Builder
				d, err := c.FollowDeployment(context.Background(), queued.ID, func(l apitypes.LogLine) error {
					log.WriteString(l.Stream + " " + l.Text + "\n")
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if d.Status != "live" || d.Release == nil || d.Release.Version != version {
					t.Fatalf("deployment %d: %s: %s\n%s", version, d.Status, d.Error, log.String())
				}
				t.Logf("v%d deployed in %s:\n%s", version, time.Since(start).Round(100*time.Millisecond), log.String())
			}
			check := func(message string) {
				t.Helper()
				_, body := get(t, addrs.HTTP, app+".localhost", "/")
				if !strings.Contains(body, message+" "+s.want) {
					t.Errorf("served %q, want %q", body, message+" "+s.want)
				}
			}

			deploy(1)
			check("release one")
			bump(t, src)
			deploy(2)
			check("release two")

			start := time.Now()
			d, err := c.Rollback(context.Background(), app, "production", 1)
			if err != nil {
				t.Fatal(err)
			}
			took := time.Since(start)
			if d.Release.Version != 3 || took > 10*time.Second {
				t.Errorf("rollback: v%d in %s", d.Release.Version, took)
			}
			t.Logf("rolled back in %s", took.Round(10*time.Millisecond))
			check("release one")
		})
	}
	if data, err := os.ReadFile(filepath.Join(cfg.DataDir, "data", "sample-python", "production", "release-ran")); err != nil || string(data) != "v2" {
		t.Errorf("the python release command: %q %v", data, err)
	}
}

// copyTree copies a directory, keeping executable bits.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// bump changes "release one" into "release two" in an app's message.
func bump(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"message.txt", "index.html"} {
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		os.WriteFile(p, []byte(strings.ReplaceAll(string(data), "release one", "release two")), 0o644)
	}
}
