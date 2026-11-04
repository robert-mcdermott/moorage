//go:build linux

package isolate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test binary plays the helper, as moor does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		os.Exit(Main(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestKernelAtLeast(t *testing.T) {
	for release, want := range map[string]bool{"5.7.0": true, "5.6.19": false, "6.1.0-13-amd64": true, "4.19.0": false, "5.15.0-91-generic": true, "5.7": true, "garbage": false} {
		if got := kernelAtLeast(release, 5, 7); got != want {
			t.Errorf("%s: %v", release, got)
		}
	}
}

func runIsolated(t *testing.T, s *Spec, name string, args ...string) (string, error) {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s here", name)
	}
	cmd := exec.Command(path, args...)
	release, err := Wrap(cmd, s)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSandbox confines a process to what its rules name. It needs no privileges, so that it runs as
// whoever runs the tests: the check on a real server tries the rest.
func TestSandbox(t *testing.T) {
	if landlockVersion() == 0 {
		t.Skip("this kernel has no Landlock")
	}
	root := t.TempDir()
	readable, writable, other := filepath.Join(root, "readable"), filepath.Join(root, "writable"), filepath.Join(root, "other")
	for _, d := range []string{readable, writable, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "file"), []byte("contents of "+filepath.Base(d)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := &Spec{Sandbox: true, Port: NoPortRule, ReadOnly: append(append([]string{}, systemRead...), readable, self), ReadWrite: append(append([]string{}, systemWrite...), writable)}

	if out, err := runIsolated(t, spec, "cat", filepath.Join(readable, "file")); err != nil || !strings.Contains(out, "contents of readable") {
		t.Errorf("what it may read: %q %v", out, err)
	}
	if out, err := runIsolated(t, spec, "cat", filepath.Join(other, "file")); err == nil || !strings.Contains(out, "denied") {
		t.Errorf("what it may not read: %q %v", out, err)
	}
	if out, err := runIsolated(t, spec, "touch", filepath.Join(readable, "new")); err == nil || !strings.Contains(out, "denied") {
		t.Errorf("writing what it may only read: %q %v", out, err)
	}
	if out, err := runIsolated(t, spec, "touch", filepath.Join(writable, "new")); err != nil {
		t.Errorf("writing what it may: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(writable, "new")); err != nil {
		t.Error(err)
	}
	if out, err := runIsolated(t, spec, "sh", "-c", "grep NoNewPrivs /proc/self/status"); err != nil || !strings.Contains(out, "1") {
		t.Errorf("no_new_privs: %q %v", out, err)
	}
	// Nothing it starts escapes.
	if out, err := runIsolated(t, spec, "sh", "-c", "cat "+filepath.Join(other, "file")); err == nil || !strings.Contains(out, "denied") {
		t.Errorf("what it starts: %q %v", out, err)
	}
	// And this process, which started it, is as it was.
	if _, err := os.ReadFile(filepath.Join(other, "file")); err != nil {
		t.Error(err)
	}
}

func TestHelperSaysWhatWentWrong(t *testing.T) {
	cmd := exec.Command("/nowhere/at/all")
	release, err := Wrap(cmd, &Spec{Sandbox: true, Port: NoPortRule, ReadOnly: []string{"/"}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "cannot run /nowhere/at/all") {
		t.Errorf("%q %v", out, err)
	}
}

func TestFieldOf(t *testing.T) {
	text := "low 0\nhigh 0\nmax 3\noom 1\noom_kill 2\noom_group_kill 1\n"
	if got, ok := fieldOf(text, "oom_kill"); !ok || got != "2" {
		t.Errorf("%q %v", got, ok)
	}
	if _, ok := fieldOf(text, "nothing"); ok {
		t.Error("found what is not there")
	}
}
