package native

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/shell"
	"github.com/robert-mcdermott/moorage/internal/shim"
	"github.com/robert-mcdermott/moorage/internal/testapp"
)

func TestMain(m *testing.M) {
	switch os.Getenv("MOOR_TEST_ROLE") {
	case "shim":
		os.Exit(shim.Main(os.Args[1:]))
	case "sh":
		os.Exit(shell.Main(os.Args[1:]))
	case "app":
		os.Exit(testapp.Main())
	}
	os.Exit(m.Run())
}

func setup(t *testing.T) (*Driver, shim.Spec, int) {
	t.Helper()
	exe, _ := os.Executable()
	dir := t.TempDir()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	d := New(filepath.Join(dir, "run"), []string{exe}, []string{"MOOR_TEST_ROLE=shim"})
	env := []string{fmt.Sprintf("PORT=%d", port)}
	for _, k := range []string{"PATH", "SystemRoot", "WINDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	spec := shim.Spec{
		ID:          "ins_a",
		Dir:         dir,
		Command:     "MOOR_TEST_ROLE=app '" + exe + "'",
		Env:         env,
		Shell:       []string{exe, "-c"},
		ShellEnv:    []string{"MOOR_TEST_ROLE=sh"},
		LogFile:     filepath.Join(dir, "logs", "ins_a.log"),
		StopTimeout: 5 * time.Second,
		BackoffMin:  50 * time.Millisecond,
	}
	return d, spec, port
}

func serving(port int) bool {
	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == 200
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStartStatusStop(t *testing.T) {
	d, spec, port := setup(t)
	st, err := d.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Stop(spec.ID, time.Second) })
	if st.Status != shim.Running || st.AppPID == 0 {
		t.Errorf("after Start: %+v", st)
	}
	waitFor(t, "the app", func() bool { return serving(port) })

	// A new driver on the same directory (a restarted server) finds the
	// instance and talks to its shim.
	again := New(d.dir, d.shimCmd, d.shimEnv)
	if ids := again.List(); len(ids) != 1 || ids[0] != spec.ID {
		t.Errorf("List = %v", ids)
	}
	st2, alive, err := again.Status(spec.ID)
	if err != nil || !alive || st2.AppPID != st.AppPID {
		t.Errorf("Status from a new driver: %+v alive %v %v", st2, alive, err)
	}
	if err := again.Restart(spec.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the restarted app", func() bool {
		st3, _, _ := again.Status(spec.ID)
		return st3.AppPID != st.AppPID && st3.Status == shim.Running && serving(port)
	})

	if err := again.Stop(spec.ID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if serving(port) {
		t.Error("the app still answers after Stop")
	}
	if _, _, err := again.Status(spec.ID); err != ErrNotFound {
		t.Errorf("Status after Stop: %v", err)
	}
}

func TestStartFailure(t *testing.T) {
	d, spec, _ := setup(t)
	spec.Command = "no-such-program-for-moorage"
	spec.Restart = "never"
	st, err := d.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != shim.Exited || st.LastExit == nil || st.LastExit.Code != 127 {
		t.Errorf("state = %+v", st)
	}
}

func TestDeadShimLeavesNoApp(t *testing.T) {
	d, spec, port := setup(t)
	st, err := d.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Stop(spec.ID, time.Second) })
	waitFor(t, "the app", func() bool { return serving(port) })

	shimProc, err := os.FindProcess(st.ShimPID)
	if err != nil {
		t.Fatal(err)
	}
	shimProc.Kill()
	waitFor(t, "the shim to be reported dead", func() bool { _, alive, _ := d.Status(spec.ID); return !alive })
	// Linux and Windows take the app down with its shim; on macOS the
	// driver does it.
	last, _, _ := d.Status(spec.ID)
	d.CleanupOrphan(last)
	waitFor(t, "the app to be gone", func() bool { return !serving(port) })
}
