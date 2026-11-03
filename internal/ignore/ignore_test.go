package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatch(t *testing.T) {
	m := New("node_modules/", "*.log", "/build", "docs/**/*.tmp", "!keep.log", "cache/")
	for rel, want := range map[string]bool{
		"node_modules":          true,
		"web/node_modules":      true,
		"app.log":               true,
		"logs/app.log":          true,
		"keep.log":              false,
		"build":                 true,
		"src/build":             false,
		"docs/a/b/c.tmp":        true,
		"docs/c.tmp":            true,
		"other/c.tmp":           false,
		"main.go":               false,
		"cache":                 true,
		"node_modules_not/x.js": false,
	} {
		isDir := rel == "node_modules" || rel == "web/node_modules" || rel == "build" || rel == "src/build" || rel == "cache"
		if got := m.Match(rel, isDir); got != want {
			t.Errorf("Match(%q) = %v, want %v", rel, got, want)
		}
	}
	if New("cache/").Match("cache", false) {
		t.Error("a directory pattern matched a file")
	}
}

func TestForTree(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	write(".gitignore", "# comment\n*.pyc\n.venv/\n")
	write("web/.gitignore", "dist/\n/local.txt\n")
	write(".moorignore", "tests/\n")
	write(".venv/.gitignore", "!*.pyc\n") // inside an ignored directory: not read
	m, err := ForTree(dir, ".gitignore", ".moorignore")
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"a.pyc":         true,
		"web/dist":      true,
		"dist":          false,
		"web/local.txt": true,
		"local.txt":     false,
		"tests":         true,
		".venv":         true,
	} {
		isDir := rel == "web/dist" || rel == "dist" || rel == "tests" || rel == ".venv"
		if got := m.Match(rel, isDir); got != want {
			t.Errorf("Match(%q) = %v, want %v", rel, got, want)
		}
	}
}
