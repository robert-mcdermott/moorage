package logs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app", "ins.log")
	f, err := Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 10; i++ {
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	files := Files(path)
	if len(files) != 3 || files[0] != path+".2" || files[2] != path {
		t.Fatalf("files = %v", files)
	}
	for _, p := range files {
		if fi, _ := os.Stat(p); fi.Size() > 100 {
			t.Errorf("%s is %d bytes", p, fi.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("more files kept than asked")
	}
}

func TestWriterSplitsLines(t *testing.T) {
	var b strings.Builder
	w := NewWriter(&b, Stdout)
	fixed := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return fixed }
	w.Write([]byte("one\r\ntw"))
	w.Write([]byte("o\nthree"))
	w.Write([]byte(strings.Repeat("y", MaxLine+5)))
	w.Flush()
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("%d lines:\n%s", len(lines), b.String()[:200])
	}
	first := Parse(lines[0])
	if !first.Time.Equal(fixed) || first.Stream != Stdout || first.Text != "one" {
		t.Errorf("first line = %+v", first)
	}
	if Parse(lines[1]).Text != "two" {
		t.Errorf("second line = %q", lines[1])
	}
	if got := Parse(lines[2]).Text; got != "three"+strings.Repeat("y", MaxLine-5) {
		t.Errorf("long line not split at MaxLine: %d characters", len(got))
	}
	if got := Parse(lines[3]).Text; got != strings.Repeat("y", 10) {
		t.Errorf("rest of the long line = %q", got)
	}
}

func TestTailAndFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ins.log")
	f, err := Open(path, 200, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	now := time.Now()
	for i := 0; i < 12; i++ {
		f.Write([]byte(Format(now, Stdout, "line "+string(rune('a'+i)))))
	}
	lines, err := Tail(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 5 || lines[0].Text != "line h" || lines[4].Text != "line l" {
		t.Errorf("tail = %+v", lines)
	}

	fol := NewFollower(path)
	if got, _ := fol.Read(); len(got) != 0 {
		t.Errorf("follower started with %d lines", len(got))
	}
	f.Write([]byte(Format(now, Stderr, "new one")))
	f.Write([]byte("2026-11-02T12:00:00.000000Z O part"))
	got, err := fol.Read()
	if err != nil || len(got) != 1 || got[0].Text != "new one" || got[0].Stream != Stderr {
		t.Errorf("follow = %+v, %v", got, err)
	}
	f.Write([]byte("ial\n"))
	if got, _ := fol.Read(); len(got) != 1 || got[0].Text != "partial" {
		t.Errorf("partial line = %+v", got)
	}
	// Rotation: the follower starts again at the top of the new file.
	for i := 0; i < 8; i++ {
		f.Write([]byte(Format(now, Stdout, "after rotation")))
	}
	got, _ = fol.Read()
	if len(got) == 0 || got[len(got)-1].Text != "after rotation" {
		t.Errorf("after rotation = %+v", got)
	}
}

func TestParseForeignLine(t *testing.T) {
	if l := Parse("not a log line"); !l.Time.IsZero() || l.Text != "not a log line" {
		t.Errorf("Parse = %+v", l)
	}
}

func TestWriterRedacts(t *testing.T) {
	var out strings.Builder
	w := NewWriter(&out, Stdout).Redact("hunter2-password", "abc")
	io.WriteString(w, "connecting with hunter2-password\nshort abc stays\n")
	io.WriteString(w, "tail hunter2-password")
	w.Flush()
	got := out.String()
	if strings.Contains(got, "hunter2") || strings.Count(got, "[secret]") != 2 || !strings.Contains(got, "short abc stays") {
		t.Errorf("redacted output:\n%s", got)
	}
}

func TestFollowerGetsWhatWasWrittenBeforeARotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(Format(time.Now(), Stdout, "one")), 0o644); err != nil {
		t.Fatal(err)
	}
	f := FollowFromStart(path)
	lines, err := f.Read()
	if err != nil || len(lines) != 1 {
		t.Fatalf("%+v %v", lines, err)
	}
	// Written to, and moved aside, with nothing having looked in between.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(Format(time.Now(), Stdout, "two"))
	file.Close()
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	// What takes its place is longer already than the old one was when it was last read.
	long := Format(time.Now(), Stdout, "three, which is a good deal longer than what was there before") + Format(time.Now(), Stdout, "four, and so is this")
	if err := os.WriteFile(path, []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err = f.Read()
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, l := range lines {
		said = append(said, strings.SplitN(l.Text, ",", 2)[0])
	}
	if strings.Join(said, " ") != "two three four" {
		t.Errorf("%q", said)
	}
}

func TestFollowSinceReadsWhatRotatedWhileNoOneLooked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	before := time.Now().Add(-time.Hour)
	if err := os.WriteFile(path+".2", []byte(Format(before, Stdout, "long ago")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path+".2", before, before); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte(Format(time.Now(), Stdout, "lately")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(Format(time.Now(), Stdout, "now")), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := FollowSince(path, time.Now().Add(-time.Minute)).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].Text != "lately" || lines[1].Text != "now" {
		t.Errorf("%+v", lines)
	}
}
