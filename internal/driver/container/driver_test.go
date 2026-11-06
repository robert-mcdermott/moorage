package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/shim"
)

// The image tests run: MOORAGE_TEST_CONTAINERS names the engine ("podman",
// "docker" or "auto") for the tests that need a real one.
const testImage = "docker.io/library/busybox:1.37"

func realEngine(t *testing.T) *Driver {
	t.Helper()
	setting := os.Getenv("MOORAGE_TEST_CONTAINERS")
	if setting == "" {
		t.Skip("set MOORAGE_TEST_CONTAINERS to run this against a container engine")
	}
	e, err := Find(setting)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := e.Pull(ctx, testImage, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	w := &testLog{t: t}
	d := New(e, "test-"+strconv.FormatInt(time.Now().UnixNano(), 36), slog.New(slog.NewTextHandler(w, nil)))
	t.Cleanup(func() {
		for _, id := range d.List() {
			d.Remove(id)
		}
		d.Shutdown()
		w.done.Store(true)
	})
	return d
}

// testLog shows the driver's log with the test's, until the test ends.
type testLog struct {
	t    *testing.T
	done atomic.Bool
}

func (w *testLog) Write(p []byte) (int, error) {
	if !w.done.Load() {
		w.t.Log(strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

var seq int

func testSpec(t *testing.T, command string) Spec {
	t.Helper()
	seq++
	id := fmt.Sprintf("ins_test%04d%d", seq, time.Now().UnixNano()%100000)
	return Spec{
		ID: id, App: "shop", Env: "production", Process: "web", Index: 1, Release: "v1",
		Image: testImage, Command: command, Restart: "always", StopTimeout: 2 * time.Second,
		LogFile: filepath.Join(t.TempDir(), "instance.log"), BackoffMin: 100 * time.Millisecond,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func logText(t *testing.T, s Spec) []logs.Line {
	t.Helper()
	lines, err := logs.Tail(s.LogFile, 100000)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func hasLine(t *testing.T, s Spec, stream, text string) bool {
	t.Helper()
	return slices.ContainsFunc(logText(t, s), func(l logs.Line) bool { return l.Stream == stream && l.Text == text })
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

func TestRunAndStop(t *testing.T) {
	d := realEngine(t)
	s := testSpec(t, `echo to-stdout; echo to-stderr >&2; echo "secret is $TOKEN"; echo "port $PORT"; exec httpd -f -p "$PORT"`)
	s.Vars = map[string]string{"PORT": "8080"}
	s.Secrets = map[string]string{"TOKEN": "correct-horse-battery"}
	s.HostPort, s.ContainerPort = freePort(t), 8080
	st, err := d.Start(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != shim.Running || st.AppPID == 0 {
		t.Errorf("after starting: %+v", st)
	}
	waitFor(t, "the output", func() bool { return hasLine(t, s, logs.Stdout, "port 8080") })
	if !hasLine(t, s, logs.Stdout, "to-stdout") || !hasLine(t, s, logs.Stderr, "to-stderr") {
		t.Errorf("the streams: %+v", logText(t, s))
	}
	if !hasLine(t, s, logs.Stdout, "secret is [secret]") {
		t.Errorf("a secret in the output: %+v", logText(t, s))
	}

	// It is on the loopback interface, and not published anywhere else.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.HostPort))
	waitFor(t, "the port", func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	out, err := d.engine.output(context.Background(), nil, "port", containerName(s))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "127.0.0.1:"+strconv.Itoa(s.HostPort)) || strings.Contains(string(out), "0.0.0.0") {
		t.Errorf("published: %s", out)
	}

	if got := d.List(); !slices.Contains(got, s.ID) {
		t.Errorf("List: %v", got)
	}
	if got, err := d.nameOf(s.ID); err != nil || got != containerName(s) {
		t.Errorf("its name: %q %v", got, err)
	}
	if st, alive, err := d.Status(s.ID); err != nil || !alive || st.Status != shim.Running {
		t.Errorf("Status: %+v %v %v", st, alive, err)
	}
	if err := d.Stop(s.ID, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := d.List(); slices.Contains(got, s.ID) {
		t.Errorf("after stopping, List: %v", got)
	}
	if _, _, err := d.Status(s.ID); err != ErrNotFound {
		t.Errorf("after stopping, Status: %v", err)
	}
}

func TestRestartPolicies(t *testing.T) {
	d := realEngine(t)
	t.Run("always", func(t *testing.T) {
		s := testSpec(t, "echo ran; exit 3")
		if _, err := d.Start(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "a restart", func() bool {
			st, _, err := d.Status(s.ID)
			return err == nil && st.Restarts >= 2 && st.LastExit != nil
		})
		st, alive, _ := d.Status(s.ID)
		if !alive || st.LastExit.Code != 3 {
			t.Errorf("%+v alive=%v", st, alive)
		}
		ran := 0
		for _, l := range logText(t, s) {
			if l.Stream == logs.Stdout && l.Text == "ran" {
				ran++
			}
		}
		// Each run's output once, not again on each restart.
		if ran > st.Restarts+1 {
			t.Errorf("ran %d times in the log after %d restarts", ran, st.Restarts)
		}
		d.Stop(s.ID, time.Second)
	})
	for _, c := range []struct {
		policy, command string
		code            int
	}{
		{"never", "(exit 3)", 3},
		{"on-failure", "(exit 0)", 0},
	} {
		t.Run(c.policy, func(t *testing.T) {
			s := testSpec(t, c.command)
			s.Restart = c.policy
			if _, err := d.Start(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the exit", func() bool {
				st, _, err := d.Status(s.ID)
				return err == nil && st.Status == shim.Exited
			})
			st, alive, _ := d.Status(s.ID)
			if alive || st.LastExit == nil || st.LastExit.Code != c.code || st.Restarts != 0 {
				t.Errorf("%+v alive=%v", st, alive)
			}
			d.Stop(s.ID, time.Second)
		})
	}
}

func TestOutOfMemory(t *testing.T) {
	d := realEngine(t)
	s := testSpec(t, "tail /dev/zero")
	s.Memory = 16 << 20
	s.Restart = "never"
	if _, err := d.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the exit", func() bool {
		st, _, err := d.Status(s.ID)
		return err == nil && st.Status == shim.Exited
	})
	st, _, _ := d.Status(s.ID)
	// Some engines report it; for the others, the log says what is likely.
	reported := st.LastExit != nil && st.LastExit.Signal == "OOM"
	if !reported && (st.LastExit == nil || st.LastExit.Signal != "KILL" || st.LastExit.Code != 137) {
		t.Errorf("%+v", st.LastExit)
	}
	said := "perhaps for going over its memory limit"
	if reported {
		said = "out of memory"
	}
	if !slices.ContainsFunc(logText(t, s), func(l logs.Line) bool { return strings.Contains(l.Text, said) }) {
		t.Errorf("the log does not say %q: %+v", said, logText(t, s))
	}
	d.Stop(s.ID, time.Second)
}

func TestRestartOnRequest(t *testing.T) {
	d := realEngine(t)
	s := testSpec(t, "echo started; exec sleep 600")
	s.StopTimeout = time.Second
	if _, err := d.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := d.Restart(s.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the restart", func() bool {
		st, _, err := d.Status(s.ID)
		return err == nil && st.Restarts == 1 && st.Status == shim.Running
	})
	st, _, _ := d.Status(s.ID)
	if st.LastExit != nil {
		t.Errorf("a restart on request is not an exit: %+v", st.LastExit)
	}
	d.Stop(s.ID, time.Second)
}

// TestAdoption is a server restarting: the container runs on, its output in
// between is recorded, and none is recorded twice.
func TestAdoption(t *testing.T) {
	d := realEngine(t)
	s := testSpec(t, `i=0; while true; do echo "line $i"; i=$((i+1)); sleep 0.05; done`)
	s.BackoffMin = time.Second
	if _, err := d.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the output", func() bool { return hasLine(t, s, logs.Stdout, "line 10") })
	d.Shutdown()
	if got := d.List(); !slices.Contains(got, s.ID) {
		t.Fatalf("the container did not outlive the server: %v", got)
	}
	time.Sleep(1500 * time.Millisecond)

	next := New(d.engine, d.server, d.log)
	st, err := next.Adopt(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != shim.Running {
		t.Errorf("adopted: %+v", st)
	}
	before := len(logText(t, s))
	waitFor(t, "more output", func() bool { return len(logText(t, s)) > before+40 })
	if err := next.Stop(s.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	var want int
	for _, l := range logText(t, s) {
		if l.Stream != logs.Stdout {
			continue
		}
		if l.Text != "line "+strconv.Itoa(want) {
			t.Fatalf("after line %d: %q", want-1, l.Text)
		}
		want++
	}
	if want < 50 {
		t.Errorf("only %d lines", want)
	}
}

// TestAdoptExited is a machine restarting: the container stopped with it.
func TestAdoptExited(t *testing.T) {
	d := realEngine(t)
	s := testSpec(t, "echo up; exec sleep 600")
	if _, err := d.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	d.Shutdown()
	if _, err := d.engine.output(context.Background(), nil, "stop", "-t", "1", containerName(s)); err != nil {
		t.Fatal(err)
	}
	next := New(d.engine, d.server, d.log)
	if _, err := next.Adopt(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the restart", func() bool {
		st, _, err := next.Status(s.ID)
		return err == nil && st.Status == shim.Running && st.Restarts == 1
	})
	next.Stop(s.ID, time.Second)

	if _, err := next.Adopt(context.Background(), s); err != ErrNotFound {
		t.Errorf("adopting what is gone: %v", err)
	}
}

func TestContainerRemovedBehindOurBack(t *testing.T) {
	d := realEngine(t)
	s := testSpec(t, "exec sleep 600")
	if _, err := d.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := d.engine.output(context.Background(), nil, "rm", "-f", containerName(s)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the driver to notice", func() bool {
		_, _, err := d.Status(s.ID)
		return err == ErrNotFound
	})
	d.Remove(s.ID)
}

func TestRunOnce(t *testing.T) {
	d := realEngine(t)
	s := testSpec(t, `echo out; echo err >&2; touch /data/made-it; echo "$MY_VAR"`)
	s.Process = "release"
	s.Vars = map[string]string{"MY_VAR": "from-the-release"}
	s.DataDir, s.Mount = t.TempDir(), "/data"
	var stdout, stderr bytes.Buffer
	if err := d.RunOnce(context.Background(), s, time.Minute, &stdout, &stderr); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if stdout.String() != "out\nfrom-the-release\n" || stderr.String() != "err\n" {
		t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(s.DataDir, "made-it")); err != nil {
		t.Errorf("the data directory: %v", err)
	}

	s = testSpec(t, "exit 7")
	s.Process = "release"
	if err := d.RunOnce(context.Background(), s, time.Minute, io.Discard, io.Discard); err == nil {
		t.Error("a failed command succeeded")
	}
	s = testSpec(t, "sleep 600")
	s.Process = "release"
	err := d.RunOnce(context.Background(), s, 2*time.Second, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("a timeout: %v", err)
	}
	if got, _ := d.nameOf(s.ID); got != "" {
		t.Errorf("left behind: %s", got)
	}

	// Removing its container is not its success.
	s = testSpec(t, "sleep 600")
	s.Process = "release"
	result := make(chan error, 1)
	go func() { result <- d.RunOnce(context.Background(), s, time.Minute, io.Discard, io.Discard) }()
	waitFor(t, "the container", func() bool {
		st, err := d.inspect(context.Background(), containerName(s))
		return err == nil && st.Running
	})
	if _, err := d.engine.output(context.Background(), nil, "rm", "-f", containerName(s)); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil {
		t.Error("it succeeded, with its container removed")
	}
}

func TestImages(t *testing.T) {
	d := realEngine(t)
	ctx := context.Background()
	info, err := d.engine.InspectImage(ctx, testImage)
	if err != nil {
		t.Fatal(err)
	}
	pinned := info.Pinned(testImage)
	if !strings.Contains(pinned, "busybox@sha256:") {
		t.Errorf("pinned: %s", pinned)
	}
	s := testSpec(t, "echo pinned")
	s.Image, s.Process = pinned, "release"
	var out bytes.Buffer
	if err := d.RunOnce(ctx, s, time.Minute, &out, io.Discard); err != nil || out.String() != "pinned\n" {
		t.Errorf("run by digest: %v %q", err, out.String())
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+testImage+"\nARG GREETING\nARG SECRET_ARG\nEXPOSE 9000\nEXPOSE 3000\nRUN echo \"$GREETING\" > /greeting && echo \"$SECRET_ARG\" > /secret-arg\nCMD [\"cat\", \"/greeting\", \"/secret-arg\"]\n"), 0o644)
	tag := "localhost/moorage/test:" + strings.ToLower(s.ID)
	var log bytes.Buffer
	err = d.engine.Build(ctx, BuildSpec{
		Dir: dir, File: "Containerfile", Context: ".", Tag: tag,
		Labels: map[string]string{labelManaged: "true"},
		Args:   map[string]string{"GREETING": "hello"},
		Vars:   map[string]string{"SECRET_ARG": "from-the-environment"},
	}, &log, &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	defer d.engine.RemoveImage(ctx, tag)
	built, err := d.engine.InspectImage(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if built.FirstExposedPort() != 3000 {
		t.Errorf("exposed: %+v", built.Config.ExposedPorts)
	}
	s = testSpec(t, "")
	s.Image, s.Process = tag, "release"
	out.Reset()
	if err := d.RunOnce(ctx, s, time.Minute, &out, io.Discard); err != nil || out.String() != "hello\nfrom-the-environment\n" {
		t.Errorf("the built image: %v %q", err, out.String())
	}

	// A command replaces an entrypoint, as well as a command.
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+testImage+"\nENTRYPOINT [\"/bin/echo\", \"from-the-entrypoint\"]\nCMD [\"and-its-command\"]\n"), 0o644)
	entry := tag + "-entrypoint"
	log.Reset()
	if err := d.engine.Build(ctx, BuildSpec{Dir: dir, File: "Containerfile", Context: ".", Tag: entry}, &log, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	defer d.engine.RemoveImage(ctx, entry)
	for command, want := range map[string]string{
		"":                       "from-the-entrypoint and-its-command\n",
		"/bin/echo plain words":  "plain words\n",
		`echo "in a shell, $HI"`: "in a shell, there\n",
	} {
		s = testSpec(t, command)
		s.Image, s.Process = entry, "release"
		s.Vars = map[string]string{"HI": "there"}
		out.Reset()
		if err := d.RunOnce(ctx, s, time.Minute, &out, io.Discard); err != nil || out.String() != want {
			t.Errorf("%q: %v %q", command, err, out.String())
		}
	}

	if err := d.engine.RemoveImage(ctx, tag); err != nil {
		t.Errorf("remove: %v", err)
	}
	if err := d.engine.RemoveImage(ctx, tag); err != nil {
		t.Errorf("remove again: %v", err)
	}
	if err := d.engine.Pull(ctx, "localhost:1/nothing:here", io.Discard, io.Discard); err == nil {
		t.Error("pulled nothing")
	}
}

// The rest needs no engine: a script records how it was called.

func fakeEngine(t *testing.T) (*Driver, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
{
  for a in "$@"; do printf '%s\n' "$a"; done
  printf 'SECRET=%s\n' "$SECRET_VALUE"
  printf 'AUTH=%s\n' "$REGISTRY_AUTH_FILE"
  if [ -n "$REGISTRY_AUTH_FILE" ]; then cat "$REGISTRY_AUTH_FILE"; echo; fi
  echo ---
} >> "` + calls + `"
case "$1" in
  --version) echo "podman version 5" ;;
  container) echo '{"Status":"running","Running":true,"Pid":42,"StartedAt":"2026-01-01T00:00:00Z"}' ;;
  wait) sleep 600 ;;
esac
`
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := Find(bin)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "podman" {
		t.Errorf("kind: %s", e.Kind)
	}
	d := New(e, "srv1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(d.Shutdown)
	return d, calls
}

func TestRunFlags(t *testing.T) {
	d, calls := fakeEngine(t)
	s := testSpec(t, "./server --port 8080")
	s.ID = "ins_01ABCDEFGHJK"
	s.Vars = map[string]string{"PORT": "8080", "EMPTY": ""}
	s.Secrets = map[string]string{"SECRET_VALUE": "hunter2hunter2"}
	s.HostPort, s.ContainerPort = 21000, 8080
	s.DataDir, s.Mount = "/srv/data", "/data"
	s.User, s.Memory, s.CPU, s.PIDs = "1000:1000", 512<<20, 1.5, 100
	s.StopSignal, s.StopTimeout = "SIGINT", 30*time.Second
	if _, err := d.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var run string
	for _, call := range strings.Split(string(data), "---\n") {
		if strings.HasPrefix(call, "run\n") {
			run = call
		}
	}
	got := strings.Split(run, "\n")
	for _, want := range [][]string{
		{"run", "-d", "--name", containerName(s), "--restart=no"},
		{"--label", "io.moorage.managed=true"},
		{"--label", "io.moorage.server=srv1"},
		{"--label", "io.moorage.instance=" + s.ID},
		{"--log-driver", "k8s-file", "--log-opt", "max-size=10m"},
		{"--init", "--security-opt", "no-new-privileges"},
		{"-e", "EMPTY="},
		{"-e", "PORT=8080"},
		{"-e", "SECRET_VALUE"},
		{"-v", "/srv/data:/data" + volumeOptions()},
		{"--user", "1000:1000"},
		{"--memory", "536870912"},
		{"--cpus", "1.5"},
		{"--pids-limit", "100"},
		{"-p", "127.0.0.1:21000:8080"},
		{"--stop-signal", "SIGINT", "--stop-timeout", "30"},
		{"--entrypoint", "./server", testImage, "--port", "8080"},
	} {
		if !containsRun(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
	// The secret reaches the engine's environment, and no command line.
	if strings.Contains(strings.ReplaceAll(run, "SECRET=hunter2hunter2", ""), "hunter2") {
		t.Errorf("a secret on the command line: %q", got)
	}
	if !strings.Contains(run, "SECRET=hunter2hunter2") {
		t.Errorf("the secret did not reach the engine: %q", got)
	}
}

func containsRun(haystack, needle []string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func TestCommands(t *testing.T) {
	for _, c := range []struct {
		command string
		want    []string
	}{
		{"", nil},
		{"./server --port 8080", []string{"./server", "--port", "8080"}},
		{"gunicorn app:main -b 0.0.0.0:8000", []string{"gunicorn", "app:main", "-b", "0.0.0.0:8000"}},
		{"server --flag=value", []string{"server", "--flag=value"}},
		{"httpd -p $PORT", []string{"/bin/sh", "-c", "httpd -p $PORT"}},
		{"a && b", []string{"/bin/sh", "-c", "a && b"}},
		{"FOO=bar server", []string{"/bin/sh", "-c", "FOO=bar server"}},
		{`echo "hi there"`, []string{"/bin/sh", "-c", `echo "hi there"`}},
		{"ls *.txt", []string{"/bin/sh", "-c", "ls *.txt"}},
	} {
		if got := command(Spec{Command: c.command}); !slices.Equal(got, c.want) {
			t.Errorf("%q: %q", c.command, got)
		}
	}
}

func TestEngineOwnedSecret(t *testing.T) {
	d, _ := fakeEngine(t)
	s := testSpec(t, "x")
	s.Secrets = map[string]string{"DOCKER_HOST": "tcp://secret"}
	_, err := d.Start(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") {
		t.Errorf("%v", err)
	}
}

func TestCredentialsFile(t *testing.T) {
	d, calls := fakeEngine(t)
	d.engine.TmpDir = t.TempDir()
	d.engine.Auth = func(context.Context) ([]byte, error) { return []byte(`{"auths":{"registry.example.com":{}}}`), nil }
	if err := d.engine.Pull(context.Background(), "registry.example.com/x:1", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(calls)
	if !strings.Contains(string(data), `{"auths":{"registry.example.com":{}}}`) {
		t.Errorf("the engine did not see the credentials: %s", data)
	}
	if entries, _ := os.ReadDir(d.engine.TmpDir); len(entries) != 0 {
		t.Errorf("credentials left on disk: %v", entries)
	}
}

func TestFindNoEngine(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Find("auto"); err != ErrNoEngine {
		t.Errorf("%v", err)
	}
	if _, err := Find("nothing-called-this"); err == nil {
		t.Error("found an engine that is not there")
	}
}

func TestPinned(t *testing.T) {
	info := ImageInfo{ID: "sha256:abc", RepoDigests: []string{"docker.io/library/busybox@sha256:111", "ghcr.io/x/y@sha256:222"}}
	for ref, want := range map[string]string{
		"busybox":                "docker.io/library/busybox@sha256:111",
		"busybox:1.37":           "docker.io/library/busybox@sha256:111",
		"ghcr.io/x/y:1":          "ghcr.io/x/y@sha256:222",
		"ghcr.io/x/y@sha256:333": "ghcr.io/x/y@sha256:333",
		"localhost:5000/other:1": "docker.io/library/busybox@sha256:111",
	} {
		if got := info.Pinned(ref); got != want {
			t.Errorf("%s: %s", ref, got)
		}
	}
	if got := (ImageInfo{ID: "sha256:abc"}).Pinned("local:1"); got != "sha256:abc" {
		t.Errorf("no digests: %s", got)
	}
}

func TestParseStats(t *testing.T) {
	got := parseStats("moorage-a\t0.12%\t12.5MiB / 7.6GiB\nmoorage-b\t150.0%\t1.5GB / 8GB\nsomeone-elses\t1%\t1MiB / 1GiB\nbroken line\n", map[string]string{"moorage-a": "ins_a", "moorage-b": "ins_b"})
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if u := got["ins_a"]; u.CPU != 0.12 || u.Memory != uint64(12.5*(1<<20)) {
		t.Errorf("a: %+v", u)
	}
	if u := got["ins_b"]; u.CPU != 150 || u.Memory != 1500000000 {
		t.Errorf("b: %+v", u)
	}
	for size, want := range map[string]uint64{"512B": 512, "1KiB": 1024, "2kB": 2000, "3MB": 3000000, "1TiB": 1 << 40, "nonsense": 0, "": 0} {
		if got := parseSize(size); got != want {
			t.Errorf("%q: %d, want %d", size, got, want)
		}
	}
}

func TestStats(t *testing.T) {
	d := realEngine(t)
	s := testSpec(t, `sleep 30`)
	if _, err := d.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	defer d.Remove(s.ID)
	stats, err := d.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := stats[s.ID]; !ok || u.Memory == 0 {
		t.Errorf("what it is using: %+v", stats)
	}
}

// An engine's command that has left something running must not hold up going away: what it left has the pipe that its
// output comes through, and could keep that for as long as it likes.
func TestWhatACommandLeavesBehindHoldsNothingUp(t *testing.T) {
	d, calls := fakeEngine(t)
	s := testSpec(t, "./server")
	s.ID = "ins_01ABCDEFGHJK"
	if _, err := d.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	// The fake's wait is a shell that starts a sleep: wait until it has.
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(calls)
		if strings.Contains(string(data), "wait\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("it never waited: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Shutdown()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("going away is held up by what a command left running")
	}
}
