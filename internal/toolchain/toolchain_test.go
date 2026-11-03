package toolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	var rels []release
	for _, s := range []string{"20.19.5", "22.20.0", "22.21.1", "24.10.0", "24.11.1", "25.1.0", "26.0.0"} {
		v, _ := parseVersion(s)
		lts := ""
		switch v[0] {
		case 20:
			lts = "iron"
		case 22:
			lts = "jod"
		case 24:
			lts = "krypton"
		}
		rels = append(rels, release{v: v, lts: lts})
	}
	for req, want := range map[string]string{
		"": "26.0.0", "latest": "26.0.0", "lts": "24.11.1", "lts/*": "24.11.1", "lts/jod": "22.21.1",
		"24": "24.11.1", "24.10": "24.10.0", "22.20.0": "22.20.0", "v24": "24.11.1", "24.x": "24.11.1",
		"^22.20.1": "22.21.1", "~22.20": "22.20.0", ">=21": "24.11.1", ">=25": "26.0.0",
	} {
		got, err := match(rels, req)
		if err != nil || got.String() != want {
			t.Errorf("match(%q) = %v, %v; want %s", req, got, err, want)
		}
	}
	for _, req := range []string{"23", "lts/argon", ">=27", "banana"} {
		if v, err := match(rels, req); err == nil {
			t.Errorf("match(%q) = %v", req, v)
		}
	}
}

// archiveOf builds a .tar.gz (or, for a name ending in .zip, a zip) of
// files under top/. A value starting with "->" is a symbolic link.
func archiveOf(t *testing.T, name, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&buf)
		for p, content := range files {
			if strings.HasPrefix(content, "->") {
				continue
			}
			w, _ := zw.CreateHeader(&zip.FileHeader{Name: top + p, Method: zip.Deflate})
			io.WriteString(w, content)
		}
		zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if top != "" {
		tw.WriteHeader(&tar.Header{Name: top, Typeflag: tar.TypeDir, Mode: 0o755})
	}
	for p, content := range files {
		if target, ok := strings.CutPrefix(content, "->"); ok {
			tw.WriteHeader(&tar.Header{Name: top + p, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: top + p, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))})
		io.WriteString(tw, content)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

type fakeProvider struct {
	files    map[string][]byte
	requests []string
	down     bool
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.URL.Path)
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/uv/latest" {
		http.Redirect(w, r, "/uv/tag/0.13.7", http.StatusFound)
		return
	}
	data, ok := f.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(data)
}

func newFake(t *testing.T) (*fakeProvider, *Manager) {
	f := &fakeProvider{files: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	m := New(filepath.Join(dir, "toolchains"), filepath.Join(dir, "cache"), Sources{
		Go: srv.URL + "/go/", Node: srv.URL + "/node/", UV: srv.URL + "/uv",
	})
	return f, m
}

func TestGo(t *testing.T) {
	f, m := newFake(t)
	type goFile struct {
		Filename string `json:"filename"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		SHA256   string `json:"sha256"`
		Kind     string `json:"kind"`
	}
	type goRelease struct {
		Version string   `json:"version"`
		Stable  bool     `json:"stable"`
		Files   []goFile `json:"files"`
	}
	var index []goRelease
	for _, v := range []string{"1.25.4", "1.26.1", "1.26.2", "1.27rc1"} {
		name := fmt.Sprintf("go%s.%s-%s.tar.gz", v, runtime.GOOS, runtime.GOARCH)
		data := archiveOf(t, name, "go/", map[string]string{"bin/" + exe("go"): "go " + v, "VERSION": "go" + v})
		f.files["/go/"+name] = data
		index = append(index, goRelease{Version: "go" + v, Stable: !strings.Contains(v, "rc"),
			Files: []goFile{{Filename: name, OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: sum(data), Kind: "archive"}}})
	}
	f.files["/go/"], _ = json.Marshal(index)

	var log bytes.Buffer
	in, err := m.Ensure(context.Background(), "go", "1.26", nil, &log)
	if err != nil {
		t.Fatal(err)
	}
	if in.Version != "1.26.2" || in.Cached {
		t.Errorf("installed %+v", in)
	}
	if data, _ := os.ReadFile(in.Exe); string(data) != "go 1.26.2" {
		t.Errorf("go binary: %q", data)
	}
	if !strings.Contains(log.String(), "verified") {
		t.Errorf("log:\n%s", log.String())
	}
	in, err = m.Ensure(context.Background(), "go", "1.26.2", nil, io.Discard)
	if err != nil || !in.Cached {
		t.Errorf("second ensure: %+v %v", in, err)
	}
	if got := m.Versions("go"); len(got) != 1 || got[0] != "1.26.2" {
		t.Errorf("versions: %v", got)
	}

	// A file that does not match its published checksum is refused.
	name := fmt.Sprintf("go1.25.4.%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	f.files["/go/"+name] = archiveOf(t, name, "go/", map[string]string{"bin/" + exe("go"): "tampered"})
	_, err = m.Ensure(context.Background(), "go", "1.25", nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Errorf("tampered download: %v", err)
	}
	if _, ok := m.Find("go", "1.25.4"); ok {
		t.Error("a tampered toolchain was installed")
	}

	// Offline, an installed version matching the request is used.
	m.indexes = map[string]cached{}
	f.down = true
	log.Reset()
	in, err = m.Ensure(context.Background(), "go", "1.26", nil, &log)
	if err != nil || in.Version != "1.26.2" || !strings.Contains(log.String(), "using the installed") {
		t.Errorf("offline: %+v %v\n%s", in, err, log.String())
	}
	if _, err := m.Ensure(context.Background(), "go", "1.25", nil, io.Discard); err == nil {
		t.Error("offline without a matching install succeeded")
	}
}

func TestNodeAndUV(t *testing.T) {
	f, m := newFake(t)
	platform, ext, err := m.nodePlatform()
	if err != nil {
		t.Skip(err)
	}
	key := map[string]string{"darwin": "osx-" + strings.TrimPrefix(platform, "darwin-") + "-tar", "linux": platform, "windows": platform + "-zip"}[runtime.GOOS]
	var index []map[string]any
	sums := map[string]string{}
	for _, r := range []struct{ v, lts string }{{"24.11.1", "Krypton"}, {"25.2.0", ""}} {
		name := "node-v" + r.v + "-" + platform + ext
		files := map[string]string{"bin/node": "node " + r.v, "lib/node_modules/npm/bin/npm-cli.js": "npm", "bin/npm": "->../lib/node_modules/npm/bin/npm-cli.js"}
		if runtime.GOOS == "windows" {
			files = map[string]string{"node.exe": "node " + r.v}
		}
		data := archiveOf(t, name, "node-v"+r.v+"-"+platform+"/", files)
		f.files["/node/v"+r.v+"/"+name] = data
		f.files["/node/v"+r.v+"/SHASUMS256.txt"] = []byte(sum(data) + "  " + name + "\n" + strings.Repeat("0", 64) + "  other.tar.gz\n")
		sums[r.v] = sum(data)
		var lts any = false
		if r.lts != "" {
			lts = r.lts
		}
		index = append(index, map[string]any{"version": "v" + r.v, "lts": lts, "files": []string{key, "src"}})
	}
	f.files["/node/index.json"], _ = json.Marshal(index)

	in, err := m.Ensure(context.Background(), "node", "lts", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if in.Version != "24.11.1" {
		t.Errorf("lts: %+v", in)
	}
	if runtime.GOOS != "windows" {
		if target, err := os.Readlink(filepath.Join(in.Dir, "bin", "npm")); err != nil || target != "../lib/node_modules/npm/bin/npm-cli.js" {
			t.Errorf("npm link: %q %v", target, err)
		}
	}

	asset, _ := m.uvAsset()
	files := map[string]string{"uv": "uv", "uvx": "uvx"}
	top := strings.TrimSuffix(strings.TrimSuffix(asset, ".tar.gz"), ".zip") + "/"
	if runtime.GOOS == "windows" {
		files, top = map[string]string{"uv.exe": "uv", "uvx.exe": "uvx"}, ""
	}
	data := archiveOf(t, asset, top, files)
	f.files["/uv/download/0.13.7/"+asset] = data
	f.files["/uv/download/0.13.7/"+asset+".sha256"] = []byte(sum(data) + "  " + asset + "\n")
	uv, err := m.Ensure(context.Background(), "uv", "", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if uv.Version != "0.13.7" || filepath.Base(uv.Exe) != exe("uv") {
		t.Errorf("uv: %+v", uv)
	}
	// Without a version, an installed uv is used without asking for the
	// latest release again.
	before := len(f.requests)
	if uv, err = m.Ensure(context.Background(), "uv", "", nil, io.Discard); err != nil || !uv.Cached || len(f.requests) != before {
		t.Errorf("installed uv: %+v %v, %d new requests", uv, err, len(f.requests)-before)
	}
	if _, err := m.Ensure(context.Background(), "uv", "0.13", nil, io.Discard); err == nil {
		t.Error("partial uv version accepted")
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	for name, files := range map[string]map[string]string{
		"link.tar.gz":  {"bin/evil": "->../../../etc/passwd"},
		"abs.tar.gz":   {"bin/evil": "->/etc/passwd"},
		"mixed.tar.gz": {"a/x": "1", "b/y": "2"},
	} {
		archive := filepath.Join(dir, name)
		os.WriteFile(archive, archiveOf(t, name, "top/", files), 0o644)
		target := filepath.Join(dir, name+".out")
		os.MkdirAll(target, 0o755)
		err := extract(archive, target)
		if name == "mixed.tar.gz" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s was extracted", name)
		}
	}
}
