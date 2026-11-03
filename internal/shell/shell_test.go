package shell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSimple(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + filepath.Dir(exe), "PORT=8123", "NAME=world"}
	name := filepath.Base(exe)
	path, argv, runEnv, ok, err := Simple(`MODE=fast `+name+` --port $PORT "hello $NAME" 'lit $X'`, env, dir)
	if err != nil || !ok {
		t.Fatalf("Simple: ok %v, %v", ok, err)
	}
	if path != exe {
		t.Errorf("path = %q, want %q", path, exe)
	}
	want := []string{name, "--port", "8123", "hello world", "lit $X"}
	if strings.Join(argv, "|") != strings.Join(want, "|") {
		t.Errorf("argv = %q", argv)
	}
	if runEnv[len(runEnv)-1] != "MODE=fast" {
		t.Errorf("assignment not in the environment: %v", runEnv)
	}

	for _, complex := range []string{
		"a && b",
		"a | b",
		"a > out.txt",
		"echo $(date)",
		"cd web; ./server",
		"",
	} {
		if _, _, _, ok, _ := Simple(complex, env, dir); ok {
			t.Errorf("Simple(%q) = ok", complex)
		}
	}
	if _, _, _, _, err := Simple("no-such-program-anywhere", env, dir); err == nil {
		t.Error("a missing program was not reported")
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	status, err := Run(context.Background(), `X=1; if [ "$X" = 1 ]; then echo "x is $X"; fi; exit 3`, nil, dir, nil, &out, &out)
	if err != nil || status != 3 || strings.TrimSpace(out.String()) != "x is 1" {
		t.Errorf("Run: status %d, %v, output %q", status, err, out.String())
	}
	out.Reset()
	if status, err := Run(context.Background(), `cd sub 2>/dev/null || echo missing`, nil, dir, nil, &out, &out); err != nil || status != 0 || strings.TrimSpace(out.String()) != "missing" {
		t.Errorf("Run with ||: %d %v %q", status, err, out.String())
	}
	if runtime.GOOS != "windows" {
		out.Reset()
		if status, _ := Run(context.Background(), `printf 'a\nb\n' | wc -l`, []string{"PATH=" + os.Getenv("PATH")}, dir, nil, &out, &out); status != 0 || strings.TrimSpace(out.String()) != "2" {
			t.Errorf("pipeline: %d %q", status, out.String())
		}
	}
}
