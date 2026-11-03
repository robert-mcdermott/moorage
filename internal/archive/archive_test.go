package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"index.html":          "<h1>hi</h1>",
		"assets/app.js":       "console.log(1)",
		"assets/deep/x.css":   "body{}",
		".git/HEAD":           "ref: refs/heads/main",
		"node_modules/a/b.js": "skip me",
		"empty-dir/.gitkeep":  "",
	})
	if runtime.GOOS != "windows" {
		if err := os.Symlink("index.html", filepath.Join(src, "link.html")); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	files, size, skipped, err := Write(&buf, src, func(rel string, d fs.DirEntry) bool { return rel == "node_modules" })
	if err != nil {
		t.Fatal(err)
	}
	if files != 4 || size != int64(len("<h1>hi</h1>")+len("console.log(1)")+len("body{}")) {
		t.Errorf("Write packed %d files, %d bytes", files, size)
	}
	if runtime.GOOS != "windows" && (len(skipped) != 1 || skipped[0] != "link.html") {
		t.Errorf("skipped = %v, want the symlink", skipped)
	}

	dst := t.TempDir()
	n, total, err := Extract(&buf, dst, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if n != files || total != size {
		t.Errorf("Extract: %d files, %d bytes; packed %d, %d", n, total, files, size)
	}
	got, err := os.ReadFile(filepath.Join(dst, "assets", "deep", "x.css"))
	if err != nil || string(got) != "body{}" {
		t.Errorf("assets/deep/x.css = %q, %v", got, err)
	}
	for _, absent := range []string{".git", "node_modules", "link.html"} {
		if _, err := os.Lstat(filepath.Join(dst, absent)); err == nil {
			t.Errorf("%s was packed", absent)
		}
	}
}

// tarball builds an archive from raw headers.
func tarball(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h := h
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractRejectsEscapes(t *testing.T) {
	cases := map[string][]tar.Header{
		"absolute":  {{Typeflag: tar.TypeReg, Name: "/etc/passwd", Size: 1}},
		"dotdot":    {{Typeflag: tar.TypeReg, Name: "a/../../evil", Size: 1}},
		"backslash": {{Typeflag: tar.TypeReg, Name: `..\evil`, Size: 1}},
		"symlink":   {{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc"}},
		"hardlink":  {{Typeflag: tar.TypeLink, Name: "hard", Linkname: "index.html"}},
		"duplicate": {{Typeflag: tar.TypeReg, Name: "a", Size: 1}, {Typeflag: tar.TypeReg, Name: "./a", Size: 1}},
	}
	for name, entries := range cases {
		dir := t.TempDir()
		if _, _, err := Extract(bytes.NewReader(tarball(t, entries...)), dir, DefaultLimits); err == nil {
			t.Errorf("%s: archive accepted", name)
		}
	}
	// Nothing was written outside the destination.
	if _, err := os.Stat(filepath.Join(filepath.Dir(t.TempDir()), "evil")); err == nil {
		t.Error("an archive wrote outside its destination")
	}
}

func TestExtractLimits(t *testing.T) {
	many := tarball(t, tar.Header{Typeflag: tar.TypeReg, Name: "a", Size: 1}, tar.Header{Typeflag: tar.TypeReg, Name: "b", Size: 1})
	if _, _, err := Extract(bytes.NewReader(many), t.TempDir(), Limits{MaxFiles: 1, MaxBytes: 100}); err == nil || !strings.Contains(err.Error(), "more than 1 files") {
		t.Errorf("file limit: %v", err)
	}
	big := tarball(t, tar.Header{Typeflag: tar.TypeReg, Name: "a", Size: 50})
	if _, _, err := Extract(bytes.NewReader(big), t.TempDir(), Limits{MaxFiles: 10, MaxBytes: 10}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("size limit: %v", err)
	}
	if _, _, err := Extract(strings.NewReader("not gzip"), t.TempDir(), DefaultLimits); err == nil {
		t.Error("garbage accepted")
	}
}
