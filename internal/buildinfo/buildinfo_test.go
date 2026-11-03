package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.Version != Version {
		t.Errorf("Version = %q, want %q", info.Version, Version)
	}
	if info.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("Platform = %q", info.Platform)
	}
	if !strings.HasPrefix(info.GoVersion, "go") {
		t.Errorf("GoVersion = %q", info.GoVersion)
	}
}

func TestString(t *testing.T) {
	cases := []struct {
		info Info
		want string
	}{
		{
			Info{Version: "0.1.0", GoVersion: "go1.26.6", Platform: "linux/amd64"},
			"moor 0.1.0 go1.26.6 linux/amd64",
		},
		{
			Info{Version: "0.1.0", Commit: "3f2a9c1d0b7e55aa", Time: "2026-11-02T18:44:39Z", GoVersion: "go1.26.6", Platform: "darwin/arm64"},
			"moor 0.1.0 (3f2a9c1d0b7e, 2026-11-02T18:44:39Z) go1.26.6 darwin/arm64",
		},
		{
			Info{Version: "0.1.0-dev", Commit: "abc", Modified: true, GoVersion: "go1.26.6", Platform: "windows/arm64"},
			"moor 0.1.0-dev (abc+modified) go1.26.6 windows/arm64",
		},
	}
	for _, c := range cases {
		if got := c.info.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}
