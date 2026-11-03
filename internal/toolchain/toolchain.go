// Package toolchain installs the language toolchains builds need — Go,
// Node.js, uv and Python — into the data directory, on demand and always
// verified (docs/04-build-and-deploy.md, "Toolchain manager"). A version
// may be partial ("24", "1.26"); it is resolved to the newest matching
// release, and the exact version is recorded in the build.
package toolchain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sources are the base URLs toolchains are downloaded from; mirrors can
// replace them.
type Sources struct {
	// Go serves the download index (?mode=json&include=all) and the files.
	Go string
	// Node serves index.json and v<version>/ directories with
	// SHASUMS256.txt.
	Node string
	// UV serves the releases of uv: latest (a redirect to the newest
	// release's tag) and download/<version>/<asset> with <asset>.sha256.
	UV string
	// Python, if set, is the mirror of Python builds handed to uv.
	Python string
}

// DefaultSources are the projects' official download locations.
var DefaultSources = Sources{
	Go:   "https://go.dev/dl/",
	Node: "https://nodejs.org/dist/",
	UV:   "https://github.com/astral-sh/uv/releases",
}

// requestError reports a version request no release can satisfy.
type requestError struct{ error }

func (e *requestError) Unwrap() error { return e.error }

// Installed is a toolchain ready to use.
type Installed struct {
	Name    string
	Version string
	// Dir is where it is installed.
	Dir string
	// Bin are the directories to put on PATH.
	Bin []string
	// Exe is the main program (the Python interpreter for python).
	Exe string
	// Cached reports that it was already installed.
	Cached bool
}

// Manager installs toolchains under one directory.
type Manager struct {
	dir      string
	cacheDir string
	src      Sources
	http     *http.Client
	goos     string
	goarch   string

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	indexes map[string]cached
}

type cached struct {
	at       time.Time
	releases []release
	files    map[string]file // by version string
}

type file struct {
	name   string
	url    string
	sha256 string // empty: read it from the provider's checksum file
	sumURL string
}

// indexTTL is how long a provider's list of releases is reused.
const indexTTL = time.Hour

// New returns a manager installing into dir. cacheDir holds uv's cache
// when it installs Python.
func New(dir, cacheDir string, src Sources) *Manager {
	if src.Go == "" {
		src.Go = DefaultSources.Go
	}
	if src.Node == "" {
		src.Node = DefaultSources.Node
	}
	if src.UV == "" {
		src.UV = DefaultSources.UV
	}
	return &Manager{
		dir: dir, cacheDir: cacheDir, src: src,
		http:    &http.Client{Timeout: 30 * time.Minute},
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
		locks:   map[string]*sync.Mutex{},
		indexes: map[string]cached{},
	}
}

// Dir returns where toolchains are installed.
func (m *Manager) Dir() string { return m.dir }

func (m *Manager) lock(name string) func() {
	m.mu.Lock()
	l := m.locks[name]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[name] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Ensure installs the newest release of name matching request, unless it
// is already installed, and returns it. uv must be given for python. Progress
// is written to log.
func (m *Manager) Ensure(ctx context.Context, name, request string, uv *Installed, log io.Writer) (Installed, error) {
	if name == "python" {
		if uv == nil {
			return Installed{}, errors.New("python is installed with uv, and uv is missing")
		}
		return m.ensurePython(ctx, *uv, request, log)
	}
	if name != "go" && name != "node" && name != "uv" {
		return Installed{}, fmt.Errorf("toolchain %s cannot be installed", name)
	}
	v, f, err := m.resolve(ctx, name, request)
	if err != nil {
		// Offline, or the provider is down: an installed version that
		// matches will do.
		var bad *requestError
		if errors.As(err, &bad) {
			return Installed{}, err
		}
		if in, ok := m.newestInstalled(name, request); ok {
			fmt.Fprintf(log, "%s: cannot reach the release list (%v); using the installed %s\n", name, err, in.Version)
			in.Cached = true
			return in, nil
		}
		return Installed{}, err
	}
	unlock := m.lock(name)
	defer unlock()
	if in, ok := m.Find(name, v.String()); ok {
		in.Cached = true
		return in, nil
	}
	if err := m.install(ctx, name, v.String(), f, log); err != nil {
		return Installed{}, err
	}
	in, ok := m.Find(name, v.String())
	if !ok {
		return Installed{}, fmt.Errorf("%s %s was installed but is incomplete", name, v)
	}
	return in, nil
}

// Find returns an installed toolchain.
func (m *Manager) Find(name, version string) (Installed, bool) {
	if name == "python" {
		return m.findPython(version)
	}
	dir := filepath.Join(m.dir, name, version)
	in := Installed{Name: name, Version: version, Dir: dir}
	switch name {
	case "go":
		in.Bin = []string{filepath.Join(dir, "bin")}
		in.Exe = filepath.Join(dir, "bin", m.exe("go"))
	case "node":
		in.Bin = []string{filepath.Join(dir, "bin")}
		in.Exe = filepath.Join(dir, "bin", "node")
		if m.goos == "windows" {
			in.Bin = []string{dir}
			in.Exe = filepath.Join(dir, "node.exe")
		}
	case "uv":
		in.Bin = []string{dir}
		in.Exe = filepath.Join(dir, m.exe("uv"))
	default:
		return Installed{}, false
	}
	if _, err := os.Stat(in.Exe); err != nil {
		return Installed{}, false
	}
	return in, true
}

// BinDirs returns the PATH directories of an installed toolchain, or nil.
func (m *Manager) BinDirs(name, version string) []string {
	in, ok := m.Find(name, version)
	if !ok {
		return nil
	}
	return in.Bin
}

func (m *Manager) exe(name string) string {
	if m.goos == "windows" {
		return name + ".exe"
	}
	return name
}

// Versions lists the installed versions of a toolchain, newest first.
func (m *Manager) Versions(name string) []string {
	if name == "python" {
		return m.pythonVersions()
	}
	entries, _ := os.ReadDir(filepath.Join(m.dir, name))
	var vs []version
	for _, e := range entries {
		if v, ok := parseVersion(e.Name()); ok && e.IsDir() {
			if _, ok := m.Find(name, e.Name()); ok {
				vs = append(vs, v)
			}
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[j].less(vs[i]) })
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.String()
	}
	return out
}

func (m *Manager) newestInstalled(name, request string) (Installed, bool) {
	var rels []release
	for _, s := range m.Versions(name) {
		v, _ := parseVersion(s)
		rels = append(rels, release{v: v})
	}
	if strings.HasPrefix(request, "lts") {
		request = "" // installed versions carry no LTS name
	}
	v, err := match(rels, request)
	if err != nil {
		return Installed{}, false
	}
	return m.Find(name, v.String())
}

// resolve finds the release matching request and its download.
func (m *Manager) resolve(ctx context.Context, name, request string) (version, file, error) {
	if name == "uv" {
		return m.resolveUV(ctx, request)
	}
	idx, err := m.index(ctx, name)
	if err != nil {
		return nil, file{}, err
	}
	v, err := match(idx.releases, request)
	if err != nil {
		return nil, file{}, &requestError{fmt.Errorf("%s: %w", name, err)}
	}
	f, ok := idx.files[v.String()]
	if !ok {
		return nil, file{}, fmt.Errorf("%s %s has no download for %s/%s", name, v, m.goos, m.goarch)
	}
	return v, f, nil
}

func (m *Manager) index(ctx context.Context, name string) (cached, error) {
	m.mu.Lock()
	c, ok := m.indexes[name]
	m.mu.Unlock()
	if ok && time.Since(c.at) < indexTTL {
		return c, nil
	}
	var err error
	switch name {
	case "go":
		c, err = m.goIndex(ctx)
	case "node":
		c, err = m.nodeIndex(ctx)
	}
	if err != nil {
		return cached{}, err
	}
	c.at = time.Now()
	m.mu.Lock()
	m.indexes[name] = c
	m.mu.Unlock()
	return c, nil
}

func (m *Manager) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v)
}

func (m *Manager) goIndex(ctx context.Context) (cached, error) {
	var list []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
		Files   []struct {
			Filename string `json:"filename"`
			OS       string `json:"os"`
			Arch     string `json:"arch"`
			SHA256   string `json:"sha256"`
			Kind     string `json:"kind"`
		} `json:"files"`
	}
	if err := m.getJSON(ctx, m.src.Go+"?mode=json&include=all", &list); err != nil {
		return cached{}, fmt.Errorf("the Go release list: %w", err)
	}
	c := cached{files: map[string]file{}}
	for _, r := range list {
		v, ok := parseVersion(r.Version)
		if !ok || !r.Stable {
			continue
		}
		for _, f := range r.Files {
			if f.OS == m.goos && f.Arch == m.goarch && f.Kind == "archive" && f.SHA256 != "" {
				c.releases = append(c.releases, release{v: v})
				c.files[v.String()] = file{name: f.Filename, url: m.src.Go + f.Filename, sha256: f.SHA256}
				break
			}
		}
	}
	return c, nil
}

func (m *Manager) nodePlatform() (string, string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[m.goarch]
	if arch == "" {
		return "", "", fmt.Errorf("Node.js is not available for %s", m.goarch)
	}
	switch m.goos {
	case "linux":
		return "linux-" + arch, ".tar.gz", nil
	case "darwin":
		return "darwin-" + arch, ".tar.gz", nil
	case "windows":
		return "win-" + arch, ".zip", nil
	}
	return "", "", fmt.Errorf("Node.js is not available for %s", m.goos)
}

func (m *Manager) nodeIndex(ctx context.Context) (cached, error) {
	platform, ext, err := m.nodePlatform()
	if err != nil {
		return cached{}, err
	}
	var list []struct {
		Version string          `json:"version"`
		LTS     json.RawMessage `json:"lts"`
		Files   []string        `json:"files"`
	}
	if err := m.getJSON(ctx, m.src.Node+"index.json", &list); err != nil {
		return cached{}, fmt.Errorf("the Node.js release list: %w", err)
	}
	// The index names files by platform and package type.
	want := map[string]string{"darwin-x64": "osx-x64-tar", "darwin-arm64": "osx-arm64-tar",
		"linux-x64": "linux-x64", "linux-arm64": "linux-arm64", "win-x64": "win-x64-zip", "win-arm64": "win-arm64-zip"}[platform]
	c := cached{files: map[string]file{}}
	for _, r := range list {
		v, ok := parseVersion(r.Version)
		if !ok {
			continue
		}
		found := false
		for _, f := range r.Files {
			found = found || f == want
		}
		if !found {
			continue
		}
		var lts string
		json.Unmarshal(r.LTS, &lts) // false for releases outside an LTS line
		c.releases = append(c.releases, release{v: v, lts: strings.ToLower(lts)})
		name := "node-v" + v.String() + "-" + platform + ext
		dir := m.src.Node + "v" + v.String() + "/"
		c.files[v.String()] = file{name: name, url: dir + name, sumURL: dir + "SHASUMS256.txt"}
	}
	return c, nil
}

func (m *Manager) uvAsset() (string, error) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[m.goarch]
	if arch == "" {
		return "", fmt.Errorf("uv is not available for %s", m.goarch)
	}
	switch m.goos {
	case "linux":
		// The musl builds are static and run on any Linux.
		return "uv-" + arch + "-unknown-linux-musl.tar.gz", nil
	case "darwin":
		return "uv-" + arch + "-apple-darwin.tar.gz", nil
	case "windows":
		return "uv-" + arch + "-pc-windows-msvc.zip", nil
	}
	return "", fmt.Errorf("uv is not available for %s", m.goos)
}

// resolveUV takes an exact version, or finds the latest release (when no
// version is requested, an installed uv is used as it is).
func (m *Manager) resolveUV(ctx context.Context, request string) (version, file, error) {
	asset, err := m.uvAsset()
	if err != nil {
		return nil, file{}, err
	}
	base := strings.TrimSuffix(m.src.UV, "/")
	if request == "" || request == "latest" {
		if installed := m.Versions("uv"); len(installed) > 0 && request == "" {
			v, _ := parseVersion(installed[0])
			return v, file{}, nil
		}
		tag, err := m.latestTag(ctx, base+"/latest")
		if err != nil {
			return nil, file{}, fmt.Errorf("the latest uv release: %w", err)
		}
		request = tag
	}
	v, ok := parseVersion(request)
	if !ok || len(v) != 3 {
		return nil, file{}, &requestError{fmt.Errorf("uv: give an exact version such as 0.13.7, not %q", request)}
	}
	url := base + "/download/" + v.String() + "/" + asset
	return v, file{name: asset, url: url, sumURL: url + ".sha256"}, nil
}

// latestTag follows a "latest release" link to the tag it redirects to.
func (m *Manager) latestTag(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	client := *m.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if loc == "" || i < 0 {
		return "", fmt.Errorf("%s did not redirect to a release (%s)", url, resp.Status)
	}
	return loc[i+len("/tag/"):], nil
}

// install downloads, verifies and unpacks a toolchain, then renames it into
// place.
func (m *Manager) install(ctx context.Context, name, ver string, f file, log io.Writer) error {
	downloads := filepath.Join(m.dir, ".downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return err
	}
	want := f.sha256
	if want == "" {
		var err error
		if want, err = m.checksum(ctx, f); err != nil {
			return err
		}
	}
	fmt.Fprintf(log, "downloading %s\n", f.url)
	archive := filepath.Join(downloads, randomName()+"-"+f.name)
	defer os.Remove(archive)
	got, size, err := m.download(ctx, f.url, archive)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%s failed verification: its SHA-256 is %s, the provider publishes %s", f.name, got, want)
	}
	fmt.Fprintf(log, "verified %s (%.1f MiB, SHA-256 %s…)\n", f.name, float64(size)/(1<<20), got[:16])
	tmp := filepath.Join(m.dir, name, ".tmp-"+randomName())
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extract(archive, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(m.dir, name, ver)); err != nil {
		return err
	}
	fmt.Fprintf(log, "installed %s %s\n", name, ver)
	return nil
}

// checksum reads a file's published SHA-256 from its checksum file: either
// SHASUMS256.txt ("<hash>  <name>" lines) or a <file>.sha256 next to it.
func (m *Manager) checksum(ctx context.Context, f file) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.sumURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the checksums of %s: %s", f.name, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == f.name && len(fields[0]) == 64 {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("the checksums at %s do not list %s", f.sumURL, f.name)
}

func (m *Manager) download(ctx context.Context, url, dst string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	out, err := os.Create(dst)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, 2<<30))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, fmt.Errorf("download %s: %w", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func randomName() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
