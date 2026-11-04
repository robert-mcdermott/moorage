package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestWalkRoot(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"a/b/c.txt", "a/d.txt", "skip/e.txt", "f.txt"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	err := WalkRoot(dir, func(at *os.Root, name, rel string, isDir bool) error {
		seen = append(seen, rel)
		if _, err := at.Lstat(name); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
		if name == "skip" {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, " "); got != ". a a/b a/b/c.txt a/d.txt f.txt skip" {
		t.Errorf("%s", got)
	}
	if err := WalkRoot(filepath.Join(dir, "not-there"), func(*os.Root, string, string, bool) error {
		t.Error("something is there")
		return nil
	}); err != nil {
		t.Errorf("%v", err)
	}
}

// TestWalkRootWhileThingsComeAndGo: an app making and removing temporary directories should not stop a walk.
func TestWalkRootWhileThingsComeAndGo(t *testing.T) {
	dir := t.TempDir()
	for i := range 300 {
		if err := os.MkdirAll(filepath.Join(dir, "keep", string(rune('a'+i%26))+strings.Repeat("x", i%7)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		churn := filepath.Join(dir, "keep", "churn", "deep")
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.MkdirAll(churn, 0o755)
			os.WriteFile(filepath.Join(churn, "f"), nil, 0o644)
			os.RemoveAll(filepath.Dir(churn))
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for range 200 {
		err := WalkRoot(dir, func(at *os.Root, name, _ string, _ bool) error {
			_, err := at.Lstat(name)
			return err
		})
		if err != nil {
			t.Fatalf("%v", err)
		}
	}
}

// TestWalkRootWithNamesThatAreNotText: a name need not be valid text, and one must not stop a walk.
func TestWalkRootWithNamesThatAreNotText(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("other systems refuse such names")
	}
	dir := t.TempDir()
	odd := filepath.Join(dir, "caf\xe9")
	if err := os.Mkdir(odd, 0o755); err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(filepath.Join(odd, "inside"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	found := false
	if err := WalkRoot(dir, func(_ *os.Root, name, _ string, _ bool) error {
		found = found || name == "inside"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("it did not go in")
	}
}

func TestWalkRootStaysInside(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "theirs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Skip(err)
	}
	if err := WalkRoot(dir, func(_ *os.Root, name, _ string, _ bool) error {
		if name == "theirs" {
			t.Error("it followed a link out")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
