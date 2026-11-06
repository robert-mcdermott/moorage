// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// engineScript writes a program that answers as an engine does. ps says whether a daemon is behind it.
func engineScript(t *testing.T, dir, name, ps string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo \"" + name + " version 5\" ;;\n  ps) " + ps + " ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	answers = "exit 0"
	refuses = `echo "Cannot connect to the daemon" >&2; exit 1`
)

func TestProbe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Nothing else, so that an engine this machine has is not the one found. What the scripts run is built into a shell.
	t.Setenv("PATH", dir)

	st := Probe(ctx, "auto")
	if st.State != Absent || st.Ready() || !strings.Contains(st.Problem(), "Nothing else needs one") {
		t.Errorf("none: %+v", st)
	}
	if st := Probe(ctx, "no-such-engine"); st.State != Absent || !strings.Contains(st.Reason, "no-such-engine") {
		t.Errorf("one that is named and not there: %+v", st)
	}

	// Installed, with nothing behind it: this is what looks present and is not.
	podman := engineScript(t, dir, "podman", refuses)
	st = Probe(ctx, "auto")
	if st.State != NotAnswering || st.Kind != "podman" || st.Bin != podman || !strings.Contains(st.Reason, "Cannot connect") || !strings.Contains(st.Problem(), "podman machine start") {
		t.Errorf("not answering: %+v", st)
	}
	if st.Err() == nil {
		t.Error("that is an error")
	}

	// One that is ready is preferred to the first found.
	docker := engineScript(t, dir, "docker", answers)
	if st := Probe(ctx, "auto"); st.State != Ready || st.Bin != docker || st.Kind != "docker" || st.Err() != nil {
		t.Errorf("one of each: %+v", st)
	}
	if st := Probe(ctx, podman); st.State != NotAnswering || st.Bin != podman {
		t.Errorf("one that is named goes by that name: %+v", st)
	}
}

// A daemon can hang with its socket open, and nothing here should wait for it.
func TestProbeOfAnEngineThatHangs(t *testing.T) {
	dir := t.TempDir()
	hung := engineScript(t, dir, "docker", "exec /bin/sleep 30")
	began := time.Now()
	st := Probe(context.Background(), hung)
	if took := time.Since(began); took > probeTimeout()+waitForDescendants+time.Second {
		t.Errorf("it took %v", took)
	}
	if st.State != NotAnswering {
		t.Errorf("%+v", st)
	}
}

func TestSource(t *testing.T) {
	ctx := context.Background()
	var nothing *Source
	if st := nothing.Status(ctx); st.State != Absent || nothing.Engine() != nil || nothing.Driver() != nil {
		t.Errorf("no source: %+v", st)
	}

	dir := t.TempDir()
	t.Setenv("PATH", dir)
	var pinned []string
	drivers := 0
	s := NewSource(SourceOptions{
		Kept:      time.Hour,
		Prepare:   func(e *Engine) { e.TmpDir = "prepared" },
		NewDriver: func(e *Engine) *Driver { drivers++; return &Driver{engine: e} },
		Pin:       func(bin string) { pinned = append(pinned, bin) },
	})
	// Asking for what there is asks nothing of anyone.
	if s.Engine() != nil || s.Driver() != nil || drivers != 0 {
		t.Error("it found something before it was asked")
	}
	if st := s.Status(ctx); st.State != Absent || s.Driver() != nil {
		t.Errorf("none: %+v", st)
	}

	// An answer stands for a while, and installing one needs no restart once it has passed.
	podman := engineScript(t, dir, "podman", refuses)
	if st := s.Status(ctx); st.State != Absent {
		t.Errorf("the answer did not stand: %+v", st)
	}
	s.checked = time.Time{}
	if st := s.Status(ctx); st.State != NotAnswering || s.Engine() == nil || s.Engine().TmpDir != "prepared" || s.Driver() == nil {
		t.Errorf("installed: %+v", st)
	}
	if len(pinned) != 0 {
		t.Errorf("one that has not answered was recorded: %v", pinned)
	}

	// Starting it needs no restart either, and it keeps the same driver.
	before := s.Driver()
	engineScript(t, dir, "podman", answers)
	s.checked = time.Time{}
	if st := s.Status(ctx); !st.Ready() || s.Driver() != before || len(pinned) != 0 {
		t.Errorf("started: %+v %v", st, pinned)
	}
	// Answering chooses nothing: making or running something under it does.
	if e := s.Claim(); e == nil || e.Bin != podman {
		t.Errorf("claimed: %+v", e)
	}
	s.Claim()
	if len(pinned) != 1 || pinned[0] != podman {
		t.Errorf("claimed: %v", pinned)
	}

	// What it ran stays with it, even when another is answering and it is not.
	engineScript(t, dir, "podman", refuses)
	engineScript(t, dir, "docker", answers)
	s.checked = time.Time{}
	if st := s.Status(ctx); st.State != NotAnswering || st.Bin != podman || s.Driver() != before || len(pinned) != 1 {
		t.Errorf("after it stopped: %+v %v", st, pinned)
	}

	// And it goes on doing so after the server restarts.
	again := NewSource(SourceOptions{Prefer: podman})
	if st := again.Status(ctx); st.State != NotAnswering || st.Bin != podman {
		t.Errorf("preferred: %+v", st)
	}
	// With nothing recorded, and one of them answering, that is the one.
	fresh := NewSource(SourceOptions{})
	if st := fresh.Status(ctx); !st.Ready() || st.Kind != "docker" {
		t.Errorf("fresh: %+v", st)
	}
}

// TestOneThatHasMadeSomethingIsKeptTo: an image is in the store of the engine that built it. Docker can answer first as a machine starts, and Podman half a minute
// later, in the middle of a deployment, and the image is not there for what comes next.
func TestOneThatHasMadeSomethingIsKeptTo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	engineScript(t, dir, "podman", "exit 1")
	docker := engineScript(t, dir, "docker", answers)
	s := NewSource(SourceOptions{Kept: time.Nanosecond, NewDriver: func(e *Engine) *Driver { return &Driver{engine: e} }})
	if st := s.Status(ctx); st.Bin != docker {
		t.Fatalf("%+v", st)
	}
	if e := s.Claim(); e == nil || e.Bin != docker {
		t.Fatalf("%+v", e)
	}
	engineScript(t, dir, "podman", answers)
	if st := s.Status(ctx); st.Bin != docker || s.Engine().Bin != docker {
		t.Errorf("with the other answering too: %+v", st)
	}
	var nothing *Source
	if nothing.Claim() != nil {
		t.Error("no source")
	}
}

// TestOneThatOnlyAnsweredIsNotKeptTo: until something has been made under one, a server takes whichever answers, so that one found by chance does not hold it.
func TestOneThatOnlyAnsweredIsNotKeptTo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	docker := engineScript(t, dir, "docker", answers)
	s := NewSource(SourceOptions{Kept: time.Nanosecond})
	if st := s.Status(ctx); st.Bin != docker {
		t.Fatalf("%+v", st)
	}
	engineScript(t, dir, "docker", refuses)
	podman := engineScript(t, dir, "podman", answers)
	if st := s.Status(ctx); st.Bin != podman || !st.Ready() {
		t.Errorf("after that one stopped, with another answering: %+v", st)
	}
}

// TestADriverStaysWithItsProgram: an engine whose --version fails for a moment is not another engine, and what is tracked would be lost with its driver.
func TestADriverStaysWithItsProgram(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	podman := engineScript(t, dir, "podman", answers)
	drivers := 0
	s := NewSource(SourceOptions{Kept: time.Nanosecond, NewDriver: func(e *Engine) *Driver { drivers++; return &Driver{engine: e} }})
	if st := s.Status(ctx); st.Kind != "podman" || drivers != 1 {
		t.Fatalf("%+v %d", st, drivers)
	}
	if err := os.WriteFile(podman, []byte("#!/bin/sh\necho nope >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(ctx); st.State != NotAnswering || st.Kind != "podman" || drivers != 1 {
		t.Errorf("when it would not say: %+v, %d drivers", st, drivers)
	}
}

// TestOneThatHangsHoldsUpNothing: a daemon can hang with its socket open, and what only wants to know should not wait for it.
func TestOneThatHangsHoldsUpNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	engineScript(t, dir, "podman", answers)
	s := NewSource(SourceOptions{Kept: time.Nanosecond, NewDriver: func(e *Engine) *Driver { return &Driver{engine: e} }})
	if st := s.Status(ctx); !st.Ready() {
		t.Fatalf("%+v", st)
	}
	engineScript(t, dir, "podman", "exec /bin/sleep 30")
	done := make(chan Status, 1)
	go func() { done <- s.Status(ctx) }()
	// Long enough for that to be asking.
	time.Sleep(200 * time.Millisecond)
	began := time.Now()
	if s.Engine() == nil || s.Driver() == nil {
		t.Error("no engine, while it was being asked")
	}
	if st := s.Known(); !st.Ready() {
		t.Errorf("what was last found: %+v", st)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("that took %s", took)
	}
	// Whoever else asks can stop waiting.
	cut, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	began = time.Now()
	if st := s.Status(cut); !st.Ready() {
		t.Errorf("as last found: %+v", st)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("that took %s", took)
	}
	select {
	case st := <-done:
		if st.State != NotAnswering {
			t.Errorf("%+v", st)
		}
	case <-time.After(3 * probeTimeout()):
		t.Error("the probe did not end")
	}
}

// TestSomeoneWhoLeftSaysNothingOfTheEngine: an answer cut short by whoever asked is not an answer, even the first.
func TestSomeoneWhoLeftSaysNothingOfTheEngine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	engineScript(t, dir, "podman", answers)
	s := NewSource(SourceOptions{})
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if st := s.Status(gone); st.Ready() {
		t.Errorf("with no one to tell: %+v", st)
	}
	if st := s.Status(context.Background()); !st.Ready() {
		t.Errorf("that was kept: %+v", st)
	}
}

// TestWhatWasLastFoundDoesNotAsk says what it can, and has it found out behind the scenes.
func TestWhatWasLastFoundDoesNotAsk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	var nothing *Source
	if st := nothing.Known(); st.State != Absent {
		t.Errorf("no source: %+v", st)
	}
	// Before anything has answered, it waits for no more than a moment.
	engineScript(t, dir, "podman", "exec /bin/sleep 30")
	s := NewSource(SourceOptions{Kept: time.Hour})
	began := time.Now()
	if st := s.Known(); st.State != Unknown || st.Ready() || st.Problem() != "" {
		t.Errorf("as yet: %+v", st)
	}
	if took := time.Since(began); took > 2*firstAnswerWait {
		t.Errorf("that took %s", took)
	}
	engineScript(t, dir, "podman", answers)
	fresh := NewSource(SourceOptions{Kept: time.Hour})
	if st := fresh.Known(); !st.Ready() {
		t.Errorf("one that answers at once: %+v", st)
	}
}

// TestWhatIsMadeHereIsNotLeftBehind: a one-off command has no record of an instance, and should not be taken for something a previous server left.
func TestWhatIsMadeHereIsNotLeftBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in\n  run) sleep 1 ;;\n  container) echo '{\"ExitCode\":0}' ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(&Engine{Bin: bin, Kind: "docker"}, "test", slog.Default())
	done := make(chan error, 1)
	go func() {
		done <- d.RunOnce(context.Background(), Spec{ID: "ins_oneoff", App: "a", Env: "b", Process: "release", Image: "x", Command: "true"}, time.Minute, io.Discard, io.Discard)
	}()
	waitFor(t, "it to start", func() bool { return d.Launched("ins_oneoff") })
	if d.Launched("ins_other") {
		t.Error("what it did not start")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if d.Launched("ins_oneoff") {
		t.Error("it has ended")
	}
}

// TestTheEngineCannotHoldThingsUp: what stops and removes a container is bounded, as the alternative is an environment held for as long as a daemon hangs.
func TestTheEngineCannotHoldThingsUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(&Engine{Bin: bin, Kind: "docker"}, "test", slog.Default())
	began := time.Now()
	if _, err := d.tell(200*time.Millisecond, "stop", "x"); err == nil {
		t.Error("it answered")
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("that took %s", took)
	}
}

// TestADaemonThatIsDownIsNotAContainerThatHasGone: taking the one for the other marks what is running as lost, and starts another beside it.
func TestADaemonThatIsDownIsNotAContainerThatHasGone(t *testing.T) {
	for said, gone := range map[string]bool{
		"Error response from daemon: No such container: moorage-connect-production-web-1-x":                 true,
		"Error: No such object: moorage-a-b":                                                                true,
		`Error: no container with name or ID "x" found: no such container`:                                  true,
		"Error: x: image not known":                                                                         true,
		"Error: No such image: example/app":                                                                 true,
		"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?": false,
		`Cannot connect to Podman. Error: unable to connect to Podman socket: Get "http://d/v5/libpod/_ping": dial unix /run/user/1000/podman/podman.sock: connect: no such file or directory`:              false,
		"failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory": false,
		"error during connect: Get http://x: open //./pipe/docker_engine: The system cannot find the file specified.":                                                                                       false,
		"dial tcp: lookup engine.example.com: no such host": false,
		"something else went wrong":                         false,
	} {
		if got := IsNotFound(&EngineError{Stderr: said}); got != gone {
			t.Errorf("%q: %v", said, got)
		}
	}
}

// TestANoStandsForLessThanAYes: someone who has just started Docker Desktop should not have to wait to be believed.
func TestANoStandsForLessThanAYes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	engineScript(t, dir, "podman", refuses)
	s := NewSource(SourceOptions{Kept: time.Hour})
	if st := s.Status(ctx); st.State != NotAnswering {
		t.Fatalf("%+v", st)
	}
	engineScript(t, dir, "podman", answers)
	if st := s.Status(ctx); st.State != NotAnswering {
		t.Errorf("at once: %+v", st)
	}
	s.mu.Lock()
	s.checked = time.Now().Add(-2 * notReadyKept)
	s.mu.Unlock()
	if st := s.Status(ctx); !st.Ready() {
		t.Errorf("a little later: %+v", st)
	}
	s.mu.Lock()
	s.checked = time.Now().Add(-2 * notReadyKept)
	s.mu.Unlock()
	engineScript(t, dir, "podman", refuses)
	if st := s.Status(ctx); !st.Ready() {
		t.Errorf("a yes stands for longer: %+v", st)
	}
}

// TestASlowFirstAnswerIsAnAnswer: rootless Podman takes a while, the first time on a machine that has just started.
func TestASlowFirstAnswerIsAnAnswer(t *testing.T) {
	if defaultProbeLimit < 10*time.Second {
		t.Errorf("%s is not long enough for that", defaultProbeLimit)
	}
	probeLimit.Store(int64(6 * time.Second))
	defer probeLimit.Store(int64(2 * time.Second))
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	engineScript(t, dir, "podman", "/bin/sleep 3")
	s := NewSource(SourceOptions{})
	if st := s.Status(context.Background()); !st.Ready() {
		t.Errorf("%+v", st)
	}
}

func init() {
	// These try engines that hang, and need not wait for as long as a real one is given.
	probeLimit.Store(int64(2 * time.Second))
}
