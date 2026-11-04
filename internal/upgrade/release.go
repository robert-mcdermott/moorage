package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
)

// DefaultBase is where the project publishes its releases.
const DefaultBase = "https://github.com/robert-mcdermott/moorage/releases"

// SumsName is what a release publishes beside its binaries, to check them against.
const SumsName = "SHA256SUMS"

// maxBinary is more than a release should ever be, so that a source cannot fill the disk.
const maxBinary = 1 << 30

// AssetName is the name of a release's binary for a platform.
func AssetName(goos, goarch string) string {
	name := "moor-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// checksumOf finds a file's checksum, which has to be listed once.
func checksumOf(sums []byte, name string) (string, error) {
	var found []string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			found = append(found, strings.ToLower(fields[0]))
		}
	}
	if len(found) != 1 {
		return "", &RefusedError{fmt.Sprintf("the release's checksums list %s %d times, and not once", name, len(found))}
	}
	if raw, err := hex.DecodeString(found[0]); err != nil || len(raw) != sha256.Size {
		return "", &RefusedError{fmt.Sprintf("the release's checksum for %s is not a SHA-256", name)}
	}
	return found[0], nil
}

// CompareVersions orders two versions, as -1, 0 or 1. A version with a suffix, such as 1.2.0-rc1,
// comes before the one without.
func CompareVersions(a, b string) (int, error) {
	parse := func(v string) (nums [3]int, pre string, err error) {
		v = strings.TrimPrefix(v, "v")
		v, pre, _ = strings.Cut(v, "-")
		parts := strings.Split(v, ".")
		if len(parts) != 3 {
			return nums, "", fmt.Errorf("%q is not a version", v)
		}
		for i, part := range parts {
			if nums[i], err = strconv.Atoi(part); err != nil || nums[i] < 0 {
				return nums, "", fmt.Errorf("%q is not a version", v)
			}
		}
		return nums, pre, nil
	}
	x, xPre, err := parse(a)
	if err != nil {
		return 0, err
	}
	y, yPre, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := range x {
		switch {
		case x[i] < y[i]:
			return -1, nil
		case x[i] > y[i]:
			return 1, nil
		}
	}
	switch {
	case xPre == yPre:
		return 0, nil
	case xPre == "":
		return 1, nil
	case yPre == "":
		return -1, nil
	case xPre < yPre:
		return -1, nil
	}
	return 1, nil
}

// Source is where a release comes from.
type Source struct {
	// Base is the address of the project's releases or, with Flat, an address or directory that holds
	// one release's files. Empty is the project's own.
	Base string
	Flat bool
	// Client is used for downloads, and a default that follows proxies otherwise.
	Client *http.Client
}

func (s Source) base() string {
	if s.Base == "" {
		return DefaultBase
	}
	return s.Base
}

func (s Source) isURL() bool {
	return strings.HasPrefix(s.base(), "https://") || strings.HasPrefix(s.base(), "http://")
}

// check refuses plain HTTP from anywhere but this machine: a checksum that comes the same way would be changed along
// with what it is for.
func (s Source) check() error {
	if !s.isURL() || strings.HasPrefix(s.base(), "https://") {
		return nil
	}
	u, err := url.Parse(s.base())
	if err != nil {
		return err
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	return &RefusedError{"a release cannot come over plain HTTP, except from this machine: use https, or a directory"}
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport, Timeout: 30 * time.Minute}
}

func (s Source) request(ctx context.Context, target string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", "moor/"+buildinfo.Version)
	}
	return req, err
}

// Latest asks which version is the newest. A release's page is only followed as far as its address,
// which costs none of the limited requests that the project's API allows.
func (s Source) Latest(ctx context.Context) (string, error) {
	if s.Flat {
		return "", nil // what is there is what the binary says it is
	}
	req, err := s.request(ctx, s.base()+"/latest")
	if err != nil {
		return "", err
	}
	client := *s.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask which release is the newest: %w", err)
	}
	defer res.Body.Close()
	_, tag, found := strings.Cut(res.Header.Get("Location"), "/tag/")
	if !found || !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("ask which release is the newest: %s said %s, and no release", s.base(), res.Status)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// open starts reading a file of a release. It returns fs.ErrNotExist in a wrap if there is none.
func (s Source) open(ctx context.Context, version, name string) (io.ReadCloser, error) {
	if !s.isURL() {
		return os.Open(filepath.Join(s.base(), name))
	}
	target := strings.TrimSuffix(s.base(), "/") + "/" + name
	if !s.Flat {
		target = s.base() + "/download/v" + version + "/" + name
	}
	req, err := s.request(ctx, target)
	if err != nil {
		return nil, err
	}
	res, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	switch res.StatusCode {
	case http.StatusOK:
		return res.Body, nil
	case http.StatusNotFound:
		res.Body.Close()
		return nil, fmt.Errorf("download %s: %w", name, os.ErrNotExist)
	}
	res.Body.Close()
	return nil, fmt.Errorf("download %s: %s said %s", name, s.base(), res.Status)
}

func (s Source) small(ctx context.Context, version, name string) ([]byte, error) {
	r, err := s.open(ctx, version, name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	body, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	return body, nil
}

// staged is a binary that has arrived, passed its checks and is ready to take over.
type staged struct {
	Path    string
	Version string
}

// stage brings a release's binary into dir. Nothing it brings is run until it matches its checksum, which comes over
// HTTPS from the same place, as with the installer: it catches damage, and a change on the way, and says nothing
// of the place itself.
func (s Source) stage(ctx context.Context, dir, version, goos, goarch string, mode os.FileMode, say func(string, ...any)) (staged, error) {
	var out staged
	sums, err := s.small(ctx, version, SumsName)
	if errors.Is(err, os.ErrNotExist) {
		if version != "" && !s.Flat {
			return out, &RefusedError{fmt.Sprintf("there is no release %s", version)}
		}
		return out, &RefusedError{fmt.Sprintf("%s has no %s", s.base(), SumsName)}
	}
	if err != nil {
		return out, err
	}
	asset := AssetName(goos, goarch)
	want, err := checksumOf(sums, asset)
	if err != nil {
		return out, err
	}
	say("downloading %s", asset)
	r, err := s.open(ctx, version, asset)
	if err != nil {
		return out, err
	}
	defer r.Close()
	out.Path = filepath.Join(dir, asset)
	f, err := os.OpenFile(out.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return out, err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(r, maxBinary+1))
	if err == nil {
		err = f.Sync()
	}
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return out, fmt.Errorf("download %s: %w", asset, err)
	}
	if n > maxBinary {
		return out, &RefusedError{fmt.Sprintf("%s is too large to be a release", asset)}
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return out, &RefusedError{fmt.Sprintf("%s does not match its checksum, so it was changed or damaged on the way: nothing has changed", asset)}
	}
	if err := os.Chmod(out.Path, mode); err != nil {
		return out, err
	}

	// Having passed, it can say what it is, which also shows that it runs on this machine at all.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, out.Path, "version", "--json")
	said, err := cmd.Output()
	if err != nil {
		return out, &RefusedError{fmt.Sprintf("the new binary does not run on this machine (%v): nothing has changed", err)}
	}
	var info buildinfo.Info
	if err := json.Unmarshal(said, &info); err != nil {
		return out, &RefusedError{fmt.Sprintf("the new binary does not say what it is: %v", err)}
	}
	if platform := goos + "/" + goarch; info.Platform != platform {
		return out, &RefusedError{fmt.Sprintf("the new binary is for %s, and this is %s", info.Platform, platform)}
	}
	if version != "" && info.Version != version {
		return out, &RefusedError{fmt.Sprintf("the binary in release %s says that it is %s", version, info.Version)}
	}
	out.Version = info.Version
	return out, nil
}
