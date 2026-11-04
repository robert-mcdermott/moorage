package build

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// aBuildThatRuns builds a site from a manifest, with a build step that does as a package's install script can.
func aBuildThatRuns(t *testing.T, manifest string) (string, Result, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("links are made by a shell here")
	}
	data := t.TempDir()
	job := filepath.Join(data, "builds", "site", "b1")
	app := filepath.Join(job, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "moorage.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	b := &Builder{DataDir: data, Shell: []string{"/bin/sh", "-c"}}
	res, err := b.Build(context.Background(), Job{App: "site", Env: "production", BuildID: "b1", Dir: job, Log: &log})
	return job, res, err
}

// TestBuildCannotHaveTheIndexWrittenElsewhere: where the server runs as root, a link left where the index goes
// would have it write over whatever the link leads to, such as the master key.
func TestBuildCannotHaveTheIndexWrittenElsewhere(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "only-root-may-change-this")
	if err := os.WriteFile(victim, []byte("as it was"), 0o600); err != nil {
		t.Fatal(err)
	}
	job, _, err := aBuildThatRuns(t, fmt.Sprintf(`[app]
runtime = "static"
[build]
command = 'mkdir dist; echo hi > dist/index.html; ln -s %s ../index.json'
[static]
dir = "dist"
`, victim))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "as it was" {
		t.Errorf("it wrote through the link: %q %v", got, err)
	}
	info, err := os.Lstat(filepath.Join(job, "index.json"))
	if err != nil || !info.Mode().IsRegular() {
		t.Errorf("the index: %v %v", info, err)
	}
}

// TestSiteCannotBeSomewhereElse: a link on the way to a site's directory would publish whatever it leads to, with
// what the server may read.
func TestSiteCannotBeSomewhereElse(t *testing.T) {
	server := t.TempDir()
	if err := os.Mkdir(filepath.Join(server, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server, "keys", "master.key"), []byte("the master key"), 0o600); err != nil {
		t.Fatal(err)
	}
	job, _, err := aBuildThatRuns(t, fmt.Sprintf(`[app]
runtime = "static"
[build]
command = 'ln -s %s x'
[static]
dir = "x/keys"
`, server))
	if err == nil || !strings.Contains(err.Error(), "leads outside the build") {
		t.Errorf("%v", err)
	}
	if body, err := os.ReadFile(filepath.Join(job, "index.json")); err == nil {
		t.Errorf("it made an index: %s", body)
	}
}

// TestSiteCanHaveALinkInside is what is left of links: one that stays in the build.
func TestSiteCanHaveALinkInside(t *testing.T) {
	_, res, err := aBuildThatRuns(t, `[app]
runtime = "static"
[build]
command = 'mkdir real; echo hi > real/index.html; ln -s real dist'
[static]
dir = "dist"
`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Root == "" {
		t.Errorf("%+v", res.Plan)
	}
}
