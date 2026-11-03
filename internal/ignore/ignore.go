// Package ignore matches paths against gitignore-style patterns: the
// .gitignore and .moorignore files `moor deploy` honours, and the globs of
// [build].exclude.
package ignore

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Matcher holds patterns, each relative to the directory of the file it
// came from. Later patterns win, so a "!" pattern can bring a path back.
type Matcher struct {
	patterns []pattern
}

type pattern struct {
	base    string   // directory the pattern is relative to ("" for the top)
	parts   []string // the pattern split at "/"
	negate  bool
	dirOnly bool
	// anchored patterns match from base; others match at any depth.
	anchored bool
}

// New returns a matcher of patterns relative to the top directory.
func New(patterns ...string) *Matcher {
	m := &Matcher{}
	for _, p := range patterns {
		m.Add("", p)
	}
	return m
}

// Add adds one pattern line read from a file in directory base (a
// slash-separated path relative to the top, "" for the top itself).
// Blank lines and comments are ignored.
func (m *Matcher) Add(base, line string) {
	line = strings.TrimRight(line, "\r")
	if strings.HasSuffix(line, `\ `) {
		line = strings.TrimSuffix(line, `\ `) + " "
	} else {
		line = strings.TrimRight(line, " \t")
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	p := pattern{base: base}
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if strings.Contains(line, "/") {
		p.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	if line == "" {
		return
	}
	p.parts = strings.Split(line, "/")
	m.patterns = append(m.patterns, p)
}

// AddFile adds the patterns of a file (such as .gitignore) found in
// directory base; a missing file adds nothing.
func (m *Matcher) AddFile(base, file string) error {
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m.Add(base, sc.Text())
	}
	return sc.Err()
}

// Empty reports whether the matcher has no patterns.
func (m *Matcher) Empty() bool { return m == nil || len(m.patterns) == 0 }

// Match reports whether the slash-separated path rel (relative to the top)
// is ignored. isDir says whether it is a directory. A path inside an
// ignored directory is not checked here: walkers skip such directories.
func (m *Matcher) Match(rel string, isDir bool) bool {
	if m == nil {
		return false
	}
	ignored := false
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if p.matches(rel) {
			ignored = !p.negate
		}
	}
	return ignored
}

func (p pattern) matches(rel string) bool {
	if p.base != "" {
		var ok bool
		if rel, ok = strings.CutPrefix(rel, p.base+"/"); !ok {
			return false
		}
	}
	parts := strings.Split(rel, "/")
	if p.anchored {
		return matchParts(p.parts, parts)
	}
	// An unanchored pattern (no slash) matches the last element at any
	// depth.
	ok, _ := path.Match(p.parts[0], parts[len(parts)-1])
	return ok
}

// matchParts matches path elements against pattern elements, where "**"
// matches any number of elements.
func matchParts(pat, parts []string) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchParts(pat[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	if ok, _ := path.Match(pat[0], parts[0]); !ok {
		return false
	}
	return matchParts(pat[1:], parts[1:])
}

// ForTree returns a matcher with the patterns of every file named one of
// names (".gitignore", ".moorignore") under dir, each relative to its own
// directory, skipping directories the patterns already ignore.
func ForTree(dir string, names ...string) (*Matcher, error) {
	m := &Matcher{}
	var walk func(rel string) error
	walk = func(rel string) error {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		for _, n := range names {
			if err := m.AddFile(rel, filepath.Join(abs, n)); err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == ".git" {
				continue
			}
			child := path.Join(rel, e.Name())
			if m.Match(child, true) {
				continue
			}
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return m, nil
}
