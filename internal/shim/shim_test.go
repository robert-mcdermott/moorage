package shim

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/shell"
	"github.com/robert-mcdermott/moorage/internal/testapp"
)

// The test binary also plays the shim, the embedded shell and the app.
func TestMain(m *testing.M) {
	switch os.Getenv("MOOR_TEST_ROLE") {
	case "shim":
		os.Exit(Main(os.Args[1:]))
	case "sh":
		os.Exit(shell.Main(os.Args[1:]))
	case "app":
		os.Exit(testapp.Main())
	}
	os.Exit(m.Run())
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type harness struct {
	t      *testing.T
	spec   Spec
	cmd    *exec.Cmd
	port   int
	exe    string
	exited chan struct{} // closed when the shim process has exited
}

// appCommand runs the test binary as the app.
func appCommand(exe string) string {
	return "MOOR_TEST_ROLE=app '" + exe + "'"
}

func newHarness(t *testing.T, mutate func(*Spec)) *harness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &harness{t: t, exe: exe, port: freePort(t), exited: make(chan struct{})}
	h.spec = Spec{
		ID:          "ins_test",
		Dir:         dir,
		Command:     appCommand(exe),
		Env:         append(baseEnv(), fmt.Sprintf("PORT=%d", h.port), "MOORAGE_RELEASE=v1"),
		Shell:       []string{exe, "-c"},
		ShellEnv:    []string{"MOOR_TEST_ROLE=sh"},
		LogFile:     filepath.Join(dir, "logs", "ins_test.log"),
		StateFile:   filepath.Join(dir, "ins_test.state.json"),
		Token:       "secret",
		StopTimeout: 5 * time.Second,
		BackoffMin:  50 * time.Millisecond,
		BackoffMax:  200 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&h.spec)
	}
	specPath := filepath.Join(dir, "ins_test.spec.json")
	if err := WriteSpec(specPath, h.spec); err != nil {
		t.Fatal(err)
	}
	h.cmd = Command([]string{exe}, specPath)
	h.cmd.Env = append(os.Environ(), "MOOR_TEST_ROLE=shim")
	h.cmd.Stdout, h.cmd.Stderr = os.Stderr, os.Stderr
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { h.cmd.Wait(); close(h.exited) }()
	t.Cleanup(func() {
		if st, err := ReadState(h.spec.StateFile); err == nil && !st.Final() {
			Call(st.Addr, Request{Token: "secret", Op: "stop"}, 10*time.Second)
		}
		select {
		case <-h.exited:
		case <-time.After(10 * time.Second):
			h.cmd.Process.Kill()
		}
	})
	return h
}

// baseEnv is what an app needs from the environment to run on every OS.
func baseEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "SystemRoot", "WINDIR", "TEMP", "TMP"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (h *harness) state() State {
	st, _ := ReadState(h.spec.StateFile)
	return st
}

func (h *harness) waitStatus(status string) State {
	h.t.Helper()
	var st State
	waitFor(h.t, "status "+status, 15*time.Second, func() bool { st = h.state(); return st.Status == status })
	return st
}

func (h *harness) call(op string) (Response, error) {
	return Call(h.state().Addr, Request{Token: "secret", Op: op}, 15*time.Second)
}

func (h *harness) get(path string) (string, error) {
	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", h.port, path))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

func (h *harness) waitServing() {
	h.t.Helper()
	waitFor(h.t, "the app to answer", 15*time.Second, func() bool { _, err := h.get("/health"); return err == nil })
}

func (h *harness) log() string {
	data, _ := os.ReadFile(h.spec.LogFile)
	return string(data)
}

func TestRunAndStop(t *testing.T) {
	h := newHarness(t, nil)
	st := h.waitStatus(Running)
	h.waitServing()
	if body, _ := h.get("/"); body != "hello from v1" {
		t.Errorf("app answered %q", body)
	}
	if st.AppPID == 0 || st.ShimPID == 0 || st.Addr == "" {
		t.Errorf("state = %+v", st)
	}
	if _, err := Call(st.Addr, Request{Token: "wrong", Op: "status"}, 5*time.Second); err == nil {
		t.Error("a command with the wrong token was accepted")
	}
	res, err := h.call("stop")
	if err != nil || res.State.Status != Stopped {
		t.Fatalf("stop: %+v, %v", res, err)
	}
	select {
	case <-h.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the shim did not exit after stop")
	}
	if _, err := h.get("/"); err == nil {
		t.Error("the app still answers after stop")
	}
	log := h.log()
	for _, want := range []string{" M started (pid", " O listening on", " M stopping on request", " M exited"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestRestartsCrashedApp(t *testing.T) {
	h := newHarness(t, nil)
	first := h.waitStatus(Running)
	h.waitServing()
	h.get("/crash")
	waitFor(t, "a restart", 15*time.Second, func() bool {
		st := h.state()
		return st.Status == Running && st.AppPID != first.AppPID && st.Restarts == 1
	})
	h.waitServing()
	st := h.state()
	if st.LastExit == nil || st.LastExit.Code != 3 {
		t.Errorf("last exit = %+v", st.LastExit)
	}
	if log := h.log(); !strings.Contains(log, "exited with status 3") || !strings.Contains(log, "restarting in") || !strings.Contains(log, " E testapp: crashing as asked") {
		t.Errorf("log:\n%s", log)
	}
}

func TestRestartOnRequest(t *testing.T) {
	h := newHarness(t, nil)
	first := h.waitStatus(Running)
	h.waitServing()
	if _, err := h.call("restart"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new process", 15*time.Second, func() bool {
		st := h.state()
		return st.Status == Running && st.AppPID != first.AppPID
	})
	h.waitServing()
}

func TestRestartPolicyNever(t *testing.T) {
	h := newHarness(t, func(s *Spec) {
		s.Restart = "never"
		s.Env = append(s.Env, "TESTAPP_FAIL=1")
	})
	st := h.waitStatus(Exited)
	if st.LastExit == nil || st.LastExit.Code != 1 {
		t.Errorf("last exit = %+v", st.LastExit)
	}
	select {
	case <-h.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the shim did not exit")
	}
}

func TestCrashLoop(t *testing.T) {
	h := newHarness(t, func(s *Spec) {
		s.Env = append(s.Env, "TESTAPP_FAIL=1")
		s.CrashLimit = 3
		s.CrashBackoff = time.Hour
	})
	h.waitStatus(Crashed)
	// A stop is answered during the long backoff.
	if res, err := h.call("stop"); err != nil || res.State.Status != Stopped {
		t.Errorf("stop during backoff: %+v %v", res, err)
	}
}

func TestForcedStop(t *testing.T) {
	h := newHarness(t, func(s *Spec) {
		s.Env = append(s.Env, "TESTAPP_IGNORE_TERM=1")
		s.StopTimeout = 300 * time.Millisecond
	})
	h.waitStatus(Running)
	h.waitServing()
	if _, err := h.call("stop"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.log(), "did not stop within 300ms") {
		t.Errorf("log:\n%s", h.log())
	}
	if _, err := h.get("/"); err == nil {
		t.Error("the app still answers")
	}
}

func TestShellCommand(t *testing.T) {
	h := newHarness(t, func(s *Spec) {
		s.Command = "echo before && " + appCommand(s.Shell[0])
	})
	h.waitStatus(Running)
	h.waitServing()
	if !strings.Contains(h.log(), " O before") {
		t.Errorf("log:\n%s", h.log())
	}
	if _, err := h.call("stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.get("/"); err == nil {
		t.Error("the app run through the shell still answers after stop")
	}
}

func TestKilledShimTakesItsApp(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("on macOS the server cleans up after a dead shim (see the driver's tests)")
	}
	h := newHarness(t, nil)
	h.waitStatus(Running)
	h.waitServing()
	h.cmd.Process.Kill()
	<-h.exited
	waitFor(t, "the app to die with its shim", 15*time.Second, func() bool { _, err := h.get("/"); return err != nil })
}
