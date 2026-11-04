package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests run the installers as they are, against a release in a directory,
// with the network and the machine's own settings left out of it: nothing outside
// a test's own directories changes. CI runs them for real, on throwaway runners.

// The test binary plays the releases' binaries: a copy with an identity on its end
// says that, when asked for its version. Nothing else tells two copies apart.
const (
	fakeRole   = "MOOR_TEST_FAKE_BINARY"
	fakeMarker = "\n#MOOR-TEST-IDENTITY:"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeRole) != "" {
		playBinary()
	}
	os.Exit(m.Run())
}

func playBinary() {
	exe, err := os.Executable()
	if err != nil {
		os.Exit(3)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		os.Exit(3)
	}
	identity := ""
	if i := bytes.LastIndex(data, []byte(fakeMarker)); i >= 0 {
		identity = string(data[i+len(fakeMarker):])
	}
	switch {
	case len(os.Args) > 1 && os.Args[1] == "version":
		if identity == "fails" {
			os.Exit(1)
		}
		fmt.Println(identity)
		os.Exit(0)
	case len(os.Args) > 1 && os.Args[1] == "serve":
		// A running server: holds its file open until told to stop.
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(2)
}

// fakeBinary returns the bytes of a binary that reports this identity.
func fakeBinary(t *testing.T, identity string) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return append(data[:len(data):len(data)], []byte(fakeMarker+identity)...)
}

// release is a directory that the stand-in for downloading serves from.
type release struct {
	t   *testing.T
	dir string
}

func newRelease(t *testing.T, asset, identity string) *release {
	t.Helper()
	r := &release{t: t, dir: t.TempDir()}
	r.put(asset, fakeBinary(t, identity))
	r.sums(asset)
	return r
}

func (r *release) put(name string, data []byte) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), data, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *release) sums(asset string) {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, asset))
	if err != nil {
		r.t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	r.put("SHA256SUMS", []byte(hex.EncodeToString(sum[:])+"  "+asset+"\n"+strings.Repeat("0", 64)+"  moor-other-file\n"))
}

func (r *release) requests() string {
	data, _ := os.ReadFile(filepath.Join(r.dir, ".requests"))
	return string(data)
}

const standInCurl = `#!/bin/sh
out=""
url=""
printf '%s\n' "$*" >> "$FAKE_RELEASE/.requests"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) out=$2; shift 2 ;;
    --proto|--proto-redir|--retry|--connect-timeout) shift 2 ;;
    --*) shift ;;
    *) url=$1; shift ;;
  esac
done
name=${url##*/}
[ -f "$FAKE_RELEASE/$name" ] || exit 22
cp "$FAKE_RELEASE/$name" "$out"
`

const standInUname = `#!/bin/sh
case "$1" in
  -s) printf '%s\n' "${FAKE_OS:-Linux}" ;;
  -m) printf '%s\n' "${FAKE_ARCH:-x86_64}" ;;
esac
`

// shellRun is what a run of install.sh left.
type shellRun struct {
	err    error
	stdout string
	stderr string
}

func (r shellRun) all() string { return r.stdout + r.stderr }

type shellInstall struct {
	t       *testing.T
	release *release
	home    string
	shell   string
	path    string
	env     []string
}

func newShellInstall(t *testing.T, r *release) *shellInstall {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	return &shellInstall{t: t, release: r, home: t.TempDir(), shell: "sh", path: standIns(t) + string(os.PathListSeparator) + os.Getenv("PATH")}
}

// standIns is a directory of programs to come first on the PATH.
func standIns(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{"curl": standInCurl, "uname": standInUname} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// run pipes the script into a shell, as curl does: env holds the settings a pipe
// allows, and args are given to the script.
func (s *shellInstall) run(env []string, args ...string) shellRun {
	s.t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command(s.shell, append([]string{"-s", "--"}, args...)...)
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = append([]string{
		"HOME=" + s.home, "PATH=" + s.path, "FAKE_RELEASE=" + s.release.dir, fakeRole + "=1", "TMPDIR=" + s.t.TempDir(),
	}, s.env...)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return shellRun{err: err, stdout: stdout.String(), stderr: stderr.String()}
}

func versionOf(t *testing.T, path string) string {
	t.Helper()
	cmd := exec.Command(path, "version")
	cmd.Env = append(os.Environ(), fakeRole+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s version: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

func TestInstallShFirstTime(t *testing.T) {
	for _, shell := range []string{"sh", "bash", "dash"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("no %s here", shell)
			}
			r := newRelease(t, "moor-linux-amd64", "moor 0.2.0 (abc) go1.26 linux/amd64")
			s := newShellInstall(t, r)
			s.shell = shell
			got := s.run(nil)
			if got.err != nil {
				t.Fatalf("%v\n%s", got.err, got.all())
			}
			moor := filepath.Join(s.home, ".local", "bin", "moor")
			fi, err := os.Stat(moor)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o755 {
				t.Errorf("mode %o", fi.Mode().Perm())
			}
			if v := versionOf(t, moor); !strings.HasPrefix(v, "moor 0.2.0 ") {
				t.Errorf("installed: %q", v)
			}
			for _, want := range []string{
				"Checksum verified", "moor 0.2.0 (abc)", "Installed at " + moor,
				"export PATH=", "server init --preset local", "/docs/install.md",
			} {
				if !strings.Contains(got.all(), want) {
					t.Errorf("no %q in:\n%s", want, got.all())
				}
			}
			reqs := r.requests()
			for _, want := range []string{
				"--proto =https", "--proto-redir =https", "--tlsv1.2", "--fail",
				"https://github.com/robert-mcdermott/moorage/releases/latest/download/moor-linux-amd64",
				"https://github.com/robert-mcdermott/moorage/releases/latest/download/SHA256SUMS",
			} {
				if !strings.Contains(reqs, want) {
					t.Errorf("no %q in the requests:\n%s", want, reqs)
				}
			}
			entries, _ := os.ReadDir(filepath.Dir(moor))
			if len(entries) != 1 {
				t.Errorf("left beside it: %v", entries)
			}
		})
	}
}

func TestInstallShPicksTheFileForThisComputer(t *testing.T) {
	for _, c := range []struct{ goos, goarch, asset string }{
		{"Linux", "x86_64", "moor-linux-amd64"},
		{"Linux", "aarch64", "moor-linux-arm64"},
		{"Darwin", "arm64", "moor-darwin-arm64"},
		{"Darwin", "x86_64", "moor-darwin-amd64"},
	} {
		r := newRelease(t, c.asset, "moor 0.2.0")
		s := newShellInstall(t, r)
		if got := s.run([]string{"FAKE_OS=" + c.goos, "FAKE_ARCH=" + c.goarch}); got.err != nil {
			t.Errorf("%s/%s: %v\n%s", c.goos, c.goarch, got.err, got.all())
		}
	}
	r := newRelease(t, "moor-linux-amd64", "moor 0.2.0")
	s := newShellInstall(t, r)
	if got := s.run([]string{"FAKE_OS=FreeBSD"}); got.err == nil || !strings.Contains(got.stderr, "unsupported operating system: FreeBSD") {
		t.Errorf("%v %s", got.err, got.all())
	}
	if got := s.run([]string{"FAKE_ARCH=riscv64"}); got.err == nil || !strings.Contains(got.stderr, "unsupported architecture: riscv64") {
		t.Errorf("%v %s", got.err, got.all())
	}
}

func TestInstallShSettings(t *testing.T) {
	r := newRelease(t, "moor-linux-amd64", "moor 0.1.0 (old)")
	s := newShellInstall(t, r)

	// A version, from the environment or the command line, with or without its v.
	dir := filepath.Join(t.TempDir(), "made", "for", "it")
	for _, how := range []struct {
		env  []string
		args []string
	}{
		{env: []string{"MOOR_VERSION=v0.1.0", "MOOR_INSTALL_DIR=" + dir}},
		{env: []string{"MOOR_VERSION=0.1.0", "MOOR_INSTALL_DIR=" + dir}},
		{args: []string{"--version", "v0.1.0", "--install-dir", dir}},
	} {
		got := s.run(how.env, how.args...)
		if got.err != nil {
			t.Fatalf("%+v: %v\n%s", how, got.err, got.all())
		}
		if !strings.Contains(r.requests(), "/releases/download/v0.1.0/moor-linux-amd64") {
			t.Errorf("%+v: %s", how, r.requests())
		}
		if v := versionOf(t, filepath.Join(dir, "moor")); v != "moor 0.1.0 (old)" {
			t.Errorf("%+v: %q", how, v)
		}
	}

	// On the PATH, it says to run it by name.
	s.path = dir + string(os.PathListSeparator) + s.path
	got := s.run([]string{"MOOR_INSTALL_DIR=" + dir})
	if got.err != nil || strings.Contains(got.stderr, "export PATH") || !strings.Contains(got.stdout, "\n  moor server init") {
		t.Errorf("%v\n%s", got.err, got.all())
	}

	// A fork.
	got = s.run([]string{"MOOR_REPOSITORY=someone/fork", "MOOR_INSTALL_DIR=" + dir})
	if got.err != nil || !strings.Contains(r.requests(), "github.com/someone/fork/releases") {
		t.Errorf("%v %s", got.err, r.requests())
	}

	for name, c := range map[string]struct {
		env  []string
		args []string
		want string
	}{
		"a relative directory":  {args: []string{"--install-dir", "bin"}, want: "must be an absolute path"},
		"a version with a typo": {args: []string{"--version", "latest1"}, want: "invalid release version"},
		"a shell in a version":  {env: []string{"MOOR_VERSION=v1.0.0;id"}, want: "invalid release version"},
		"a bad repository":      {env: []string{"MOOR_REPOSITORY=a/b/c"}, want: "owner/repository"},
		"a path in a repo":      {env: []string{"MOOR_REPOSITORY=../../etc"}, want: "owner/repository"},
		"an unknown option":     {args: []string{"--sudo"}, want: "unknown argument"},
		"a missing value":       {args: []string{"--version"}, want: "requires a value"},
	} {
		before := r.requests()
		got := s.run(c.env, c.args...)
		if got.err == nil || !strings.Contains(got.stderr, c.want) {
			t.Errorf("%s: %v\n%s", name, got.err, got.all())
		}
		if r.requests() != before {
			t.Errorf("%s: it downloaded before checking", name)
		}
	}

	if got := s.run(nil, "--help"); got.err != nil || !strings.Contains(got.stdout, "MOOR_INSTALL_DIR") {
		t.Errorf("%v\n%s", got.err, got.all())
	}
}

// TestInstallShRefusals keeps what is installed in place for anything wrong with
// what was downloaded.
func TestInstallShRefusals(t *testing.T) {
	const asset = "moor-linux-amd64"
	for name, c := range map[string]struct {
		change func(r *release)
		env    []string
		want   string
	}{
		"a damaged download": {
			change: func(r *release) { r.put(asset, []byte("damaged")) },
			want:   "checksum verification failed",
		},
		"no entry in the checksums": {
			change: func(r *release) { r.put("SHA256SUMS", []byte(strings.Repeat("a", 64)+"  something-else\n")) },
			want:   "exactly one entry",
		},
		"two entries": {
			change: func(r *release) {
				line := strings.Repeat("a", 64) + "  " + asset + "\n"
				r.put("SHA256SUMS", []byte(line+line))
			},
			want: "exactly one entry",
		},
		"a checksum that is not one": {
			change: func(r *release) { r.put("SHA256SUMS", []byte("nothex  "+asset+"\n")) },
			want:   "invalid SHA-256",
		},
		"a checksum with something else in it": {
			change: func(r *release) { r.put("SHA256SUMS", []byte(strings.Repeat("z", 64)+"  "+asset+"\n")) },
			want:   "invalid SHA-256",
		},
		"no binary to download": {
			change: func(r *release) { os.Remove(filepath.Join(r.dir, asset)) },
			want:   "failed to download " + asset,
		},
		"no checksums to download": {
			change: func(r *release) { os.Remove(filepath.Join(r.dir, "SHA256SUMS")) },
			want:   "failed to download SHA256SUMS",
		},
		"a binary that does not run": {
			change: func(r *release) {
				r.put(asset, []byte("#!/bin/sh\nexit 9\n"))
				r.sums(asset)
			},
			want: "did not pass its version check",
		},
		"a binary that fails its version check": {
			change: func(r *release) {
				r.put(asset, fakeBinary(r.t, "fails"))
				r.sums(asset)
			},
			want: "did not pass its version check",
		},
		"a binary that is something else": {
			change: func(r *release) {
				r.put(asset, fakeBinary(r.t, "cytoplasm 1.0"))
				r.sums(asset)
			},
			want: "unexpected version: cytoplasm 1.0",
		},
		"a binary of another version than asked for": {
			change: func(r *release) {
				r.put(asset, fakeBinary(r.t, "moor 0.3.0"))
				r.sums(asset)
			},
			env:  []string{"MOOR_VERSION=v0.2.0"},
			want: "unexpected version: moor 0.3.0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, asset, "moor 0.2.0")
			c.change(r)
			s := newShellInstall(t, r)
			dir := t.TempDir()
			moor := filepath.Join(dir, "moor")
			existing := fakeBinary(t, "moor 0.1.0 (the one in place)")
			if err := os.WriteFile(moor, existing, 0o755); err != nil {
				t.Fatal(err)
			}
			got := s.run(append([]string{"MOOR_INSTALL_DIR=" + dir}, c.env...))
			if got.err == nil || !strings.Contains(got.stderr, c.want) {
				t.Errorf("%v\n%s", got.err, got.all())
			}
			if v := versionOf(t, moor); v != "moor 0.1.0 (the one in place)" {
				t.Errorf("the copy in place became %q", v)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Errorf("left beside it: %v", entries)
			}
		})
	}
}

func TestInstallShUpgrade(t *testing.T) {
	r := newRelease(t, "moor-linux-amd64", "moor 0.2.0 (new)")
	s := newShellInstall(t, r)
	dir := t.TempDir()
	moor := filepath.Join(dir, "moor")
	if err := os.WriteFile(moor, fakeBinary(t, "moor 0.1.0 (old)"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A server running from the old file goes on, and the new file takes its name.
	server := exec.Command(moor, "serve")
	server.Env = append(os.Environ(), fakeRole+"=1")
	stdin, err := server.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- server.Wait() }()
	defer func() {
		stdin.Close()
		<-exited
	}()

	got := s.run([]string{"MOOR_INSTALL_DIR=" + dir})
	if got.err != nil {
		t.Fatalf("%v\n%s", got.err, got.all())
	}
	if v := versionOf(t, moor); v != "moor 0.2.0 (new)" {
		t.Errorf("installed: %q", v)
	}
	for _, want := range []string{"Upgraded from 0.1.0", "server restart", "CHANGELOG.md"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("no %q in:\n%s", want, got.stdout)
		}
	}
	if strings.Contains(got.stdout, "server init") {
		t.Errorf("it told someone upgrading to set up a server:\n%s", got.stdout)
	}
	select {
	case err := <-exited:
		t.Errorf("the running server stopped: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The same version again is no upgrade.
	got = s.run([]string{"MOOR_INSTALL_DIR=" + dir})
	if got.err != nil || strings.Contains(got.stdout, "Upgraded") {
		t.Errorf("%v\n%s", got.err, got.all())
	}
}

func TestInstallShSaysWhichMoorRuns(t *testing.T) {
	r := newRelease(t, "moor-linux-amd64", "moor 0.2.0")
	s := newShellInstall(t, r)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "moor"), fakeBinary(t, "moor 0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// Another one comes first.
	s.path = other + string(os.PathListSeparator) + dir + string(os.PathListSeparator) + s.path
	got := s.run([]string{"MOOR_INSTALL_DIR=" + dir})
	if got.err != nil || !strings.Contains(got.stderr, "another moor comes first on your PATH: "+filepath.Join(other, "moor")) {
		t.Errorf("%v\n%s", got.err, got.all())
	}
	if !strings.Contains(got.stdout, filepath.Join(dir, "moor")+" server init") {
		t.Errorf("it should name the one it installed:\n%s", got.stdout)
	}

	// Only another one is on the PATH.
	s.path = standIns(t) + string(os.PathListSeparator) + other + string(os.PathListSeparator) + os.Getenv("PATH")
	got = s.run([]string{"MOOR_INSTALL_DIR=" + dir})
	if got.err != nil || !strings.Contains(got.stderr, "another moor is on your PATH, and this one is not") {
		t.Errorf("%v\n%s", got.err, got.all())
	}
}

// The PowerShell installer, on Windows, with Invoke-WebRequest standing in.

const standInDownload = `
function Invoke-WebRequest {
    param($Uri, $OutFile, [switch]$UseBasicParsing)
    Add-Content -LiteralPath (Join-Path $env:FAKE_RELEASE '.requests') -Value $Uri
    $name = $Uri.Substring($Uri.LastIndexOf('/') + 1)
    $source = Join-Path $env:FAKE_RELEASE $name
    if (-not (Test-Path -LiteralPath $source)) { throw "404: $name" }
    Copy-Item -LiteralPath $source -Destination $OutFile
}
`

type psRun struct {
	err    error
	output string
}

func runPowerShell(t *testing.T, r *release, call string) psRun {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 is for Windows")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	program := "$ErrorActionPreference = 'Stop'\n. '" + script + "'\n" + standInDownload + "\n" + call + "\n"
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", program)
	cmd.Env = append(os.Environ(), "FAKE_RELEASE="+r.dir, fakeRole+"=1")
	out, err := cmd.CombinedOutput()
	return psRun{err: err, output: string(out)}
}

func TestInstallPs1FirstTime(t *testing.T) {
	r := newRelease(t, "moor-windows-amd64.exe", "moor 0.2.0 (abc)")
	dir := filepath.Join(t.TempDir(), "made", "for", "it")
	got := runPowerShell(t, r, "Invoke-MoorInstall -InstallDir '"+dir+"' -Architecture amd64 -NoPathUpdate")
	if got.err != nil {
		t.Fatalf("%v\n%s", got.err, got.output)
	}
	if v := versionOf(t, filepath.Join(dir, "moor.exe")); v != "moor 0.2.0 (abc)" {
		t.Errorf("installed: %q", v)
	}
	for _, want := range []string{"Checksum verified", "moor 0.2.0 (abc)", "server init --preset local", "not on PATH"} {
		if !strings.Contains(got.output, want) {
			t.Errorf("no %q in:\n%s", want, got.output)
		}
	}
	if !strings.Contains(r.requests(), "/releases/latest/download/moor-windows-amd64.exe") {
		t.Errorf("%s", r.requests())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left beside it: %v", entries)
	}
}

func TestInstallPs1PicksTheFile(t *testing.T) {
	for arch, asset := range map[string]string{"arm64": "moor-windows-arm64.exe", "x64": "moor-windows-amd64.exe"} {
		r := newRelease(t, asset, "moor 0.2.0")
		got := runPowerShell(t, r, "Invoke-MoorInstall -InstallDir '"+t.TempDir()+"' -Architecture "+arch+" -NoPathUpdate")
		if got.err != nil || !strings.Contains(r.requests(), asset) {
			t.Errorf("%s: %v\n%s", arch, got.err, got.output)
		}
	}
	// This machine's own, by the installer's probes.
	r := newRelease(t, "moor-windows-amd64.exe", "moor 0.2.0")
	got := runPowerShell(t, r, "Write-Output (Get-MoorAsset)")
	want := "moor-windows-" + runtime.GOARCH + ".exe"
	if got.err != nil || !strings.Contains(got.output, want) {
		t.Errorf("%v\n%s, want %s", got.err, got.output, want)
	}
}

func TestInstallPs1Refusals(t *testing.T) {
	const asset = "moor-windows-amd64.exe"
	for name, c := range map[string]struct {
		change func(r *release)
		args   string
		want   string
	}{
		"a damaged download": {
			change: func(r *release) { r.put(asset, []byte("damaged")) },
			want:   "Checksum verification failed",
		},
		"no entry in the checksums": {
			change: func(r *release) { r.put("SHA256SUMS", []byte(strings.Repeat("a", 64)+"  something-else\n")) },
			want:   "exactly one entry",
		},
		"no checksums to download": {
			change: func(r *release) { os.Remove(filepath.Join(r.dir, "SHA256SUMS")) },
			want:   "Could not download SHA256SUMS",
		},
		"a binary that fails its version check": {
			change: func(r *release) {
				r.put(asset, fakeBinary(r.t, "fails"))
				r.sums(asset)
			},
			want: "did not pass its version check",
		},
		"a binary of another version than asked for": {
			change: func(r *release) {
				r.put(asset, fakeBinary(r.t, "moor 0.3.0"))
				r.sums(asset)
			},
			args: "-Version v0.2.0",
			want: "unexpected version: moor 0.3.0",
		},
		"a version with a typo": {change: func(*release) {}, args: "-Version latest1", want: "Invalid release version"},
		"a bad repository":      {change: func(*release) {}, args: "-Repository a/b/c", want: "owner/repository"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, asset, "moor 0.2.0")
			c.change(r)
			dir := t.TempDir()
			moor := filepath.Join(dir, "moor.exe")
			if err := os.WriteFile(moor, fakeBinary(t, "moor 0.1.0 (the one in place)"), 0o755); err != nil {
				t.Fatal(err)
			}
			got := runPowerShell(t, r, "Invoke-MoorInstall -InstallDir '"+dir+"' -Architecture amd64 -NoPathUpdate "+c.args)
			if got.err == nil || !strings.Contains(got.output, c.want) {
				t.Errorf("%v\n%s", got.err, got.output)
			}
			if v := versionOf(t, moor); v != "moor 0.1.0 (the one in place)" {
				t.Errorf("the copy in place became %q", v)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Errorf("left beside it: %v", entries)
			}
		})
	}
}

// TestInstallPs1UpgradeWhileRunning is why it renames: Windows will not replace a
// program that is running, and a server's supervisors run from this file for as
// long as its apps do.
func TestInstallPs1UpgradeWhileRunning(t *testing.T) {
	r := newRelease(t, "moor-windows-amd64.exe", "moor 0.2.0 (new)")
	dir := t.TempDir()
	moor := filepath.Join(dir, "moor.exe")
	if err := os.WriteFile(moor, fakeBinary(t, "moor 0.1.0 (old)"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 is for Windows")
	}
	server := exec.Command(moor, "serve")
	server.Env = append(os.Environ(), fakeRole+"=1")
	stdin, err := server.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- server.Wait() }()

	call := "Invoke-MoorInstall -InstallDir '" + dir + "' -Architecture amd64 -NoPathUpdate"
	got := runPowerShell(t, r, call)
	if got.err != nil {
		stdin.Close()
		<-exited
		t.Fatalf("%v\n%s", got.err, got.output)
	}
	if v := versionOf(t, moor); v != "moor 0.2.0 (new)" {
		t.Errorf("installed: %q", v)
	}
	if !strings.Contains(got.output, "Upgraded from 0.1.0") {
		t.Errorf("%s", got.output)
	}
	select {
	case err := <-exited:
		t.Errorf("the running server stopped: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	aside, _ := filepath.Glob(filepath.Join(dir, "moor.previous.*.exe"))
	if len(aside) != 1 {
		t.Errorf("the old file, while it runs: %v", aside)
	}

	// Once nothing runs it, the next install clears it away.
	stdin.Close()
	<-exited
	if got := runPowerShell(t, r, call); got.err != nil {
		t.Fatalf("%v\n%s", got.err, got.output)
	}
	if aside, _ := filepath.Glob(filepath.Join(dir, "moor.previous.*.exe")); len(aside) != 0 {
		t.Errorf("left behind: %v", aside)
	}
}
