// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
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
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")

	st := Probe(ctx, "auto")
	if _, onPath := lookEngines(); !onPath && (st.State != Absent || st.Ready() || !strings.Contains(st.Problem(), "Nothing else needs one")) {
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

// lookEngines says whether this machine has an engine of its own that the path used here could find.
func lookEngines() (string, bool) {
	for _, d := range []string{"/bin", "/usr/bin"} {
		for _, n := range []string{"podman", "docker"} {
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				return filepath.Join(d, n), true
			}
		}
	}
	return "", false
}

// A daemon can hang with its socket open, and nothing here should wait for it.
func TestProbeOfAnEngineThatHangs(t *testing.T) {
	dir := t.TempDir()
	hung := engineScript(t, dir, "docker", "exec sleep 30")
	began := time.Now()
	st := Probe(context.Background(), hung)
	if took := time.Since(began); took > probeLimit+waitForDescendants+time.Second {
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
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	if _, onPath := lookEngines(); onPath {
		t.Skip("this machine has an engine of its own on the path that this needs to be empty")
	}
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
	if st := s.Status(ctx); !st.Ready() || s.Driver() != before || len(pinned) != 1 || pinned[0] != podman {
		t.Errorf("started: %+v %v", st, pinned)
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
