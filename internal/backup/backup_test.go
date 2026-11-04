package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/store"
)

// newServerData makes a data directory such as a server has.
func newServerData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"tmp", "repos/shop.git", "builds/shop/bld_1/app", "builds/shop/bld_2.partial/app", "data/shop/production", "certs", "keys", "logs", "toolchains/go", "cache"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		"repos/shop.git/HEAD":                "ref: refs/heads/main\n",
		"builds/shop/bld_1/app/run.sh":       "#!/bin/sh\n",
		"builds/shop/bld_2.partial/app/half": "half a build",
		"data/shop/production/notes.txt":     "written by the app",
		"certs/ca.crt":                       "a certificate",
		"keys/master.key":                    "the master key",
		"logs/server.log":                    "noise",
		"toolchains/go/bin":                  "a toolchain",
		"cache/thing":                        "a cache",
	} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(dir, "builds/shop/bld_1/app/run.sh"), 0o555); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("notes.txt", filepath.Join(dir, "data/shop/production/link")); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.EnsureApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func options(dir string) Options {
	return Options{DataDir: dir, KeyFingerprint: "abc", Version: "0.6.0", Domain: "apps.test"}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBackUpAndRestore(t *testing.T) {
	ctx := context.Background()
	from := newServerData(t)
	dest := &Local{Dir: filepath.Join(t.TempDir(), "backups")}
	result, err := Run(ctx, dest, options(from))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Name, "moorage-") || result.Size == 0 || result.Files == 0 || result.Schema == 0 {
		t.Fatalf("%+v", result)
	}
	r, digest, err := Open(ctx, filepath.Join(dest.Dir, result.Name), Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(digest) != 64 {
		t.Fatalf("%q", digest)
	}
	m, sum, err := Verify(ctx, r, digest)
	if err != nil || m.Version != "0.6.0" || m.KeyFingerprint != "abc" || m.IncludesKeys || sum.Files != result.Files {
		t.Fatalf("%+v %+v %v", m, sum, err)
	}

	to := t.TempDir()
	r2, digest, err := Open(ctx, filepath.Join(dest.Dir, result.Name), Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	swapped := false
	restored, err := Restore(ctx, r2, digest, RestoreOptions{DataDir: to, KeyFingerprint: "abc", BeforeSwap: func() error { swapped = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !swapped || restored.Aside != "" {
		t.Errorf("%v %+v", swapped, restored)
	}
	for path, want := range map[string]string{
		"repos/shop.git/HEAD":            "ref: refs/heads/main\n",
		"builds/shop/bld_1/app/run.sh":   "#!/bin/sh\n",
		"data/shop/production/notes.txt": "written by the app",
		"certs/ca.crt":                   "a certificate",
	} {
		if got := readFile(t, filepath.Join(to, path)); got != want {
			t.Errorf("%s: %q", path, got)
		}
	}
	// What is made again, what was not finished, and the keys stay out of it.
	for _, path := range []string{"logs", "toolchains", "cache", "keys", "builds/shop/bld_2.partial"} {
		if _, err := os.Stat(filepath.Join(to, path)); err == nil {
			t.Errorf("%s is in the backup", path)
		}
	}
	// The database is one, and holds what it held.
	db, err := store.Open(ctx, filepath.Join(to, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if names, err := db.AppNames(ctx); err != nil || len(names) != 1 || names[0] != "shop" {
		t.Errorf("%v %v", names, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(to, "builds/shop/bld_1/app/run.sh"))
		if err != nil || fi.Mode().Perm() != 0o555 {
			t.Errorf("%v %v", fi, err)
		}
		if target, err := os.Readlink(filepath.Join(to, "data/shop/production/link")); err != nil || target != "notes.txt" {
			t.Errorf("%q %v", target, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(to, "tmp"))
	if len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func archiveOf(t *testing.T, dir string, o Options) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, _, err := Write(context.Background(), &buf, o); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func refusedSaying(t *testing.T, err error, want string) {
	t.Helper()
	var refused *RefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Msg, want) {
		t.Errorf("%v, want it refused saying %q", err, want)
	}
}

func TestRestoreRefuses(t *testing.T) {
	ctx := context.Background()
	from := newServerData(t)
	good := archiveOf(t, from, options(from))
	digest := func(b []byte) string {
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:])
	}
	restoreTo := func(to string, archive []byte, o RestoreOptions) error {
		o.DataDir = to
		_, err := Restore(ctx, bytes.NewReader(archive), "", o)
		return err
	}

	// Not a backup, half a backup, and one that has been changed.
	refusedSaying(t, restoreTo(t.TempDir(), []byte("nonsense"), RestoreOptions{KeyFingerprint: "abc"}), "not a backup")
	refusedSaying(t, restoreTo(t.TempDir(), good[:len(good)/2], RestoreOptions{KeyFingerprint: "abc"}), "did not finish")
	to := t.TempDir()
	_, err := Restore(ctx, bytes.NewReader(good), digest([]byte("something else")), RestoreOptions{DataDir: to, KeyFingerprint: "abc"})
	refusedSaying(t, err, "changed or damaged")
	if _, err := os.Stat(filepath.Join(to, "moorage.db")); err == nil {
		t.Error("a damaged backup changed the data directory")
	}
	if _, _, err := Verify(ctx, bytes.NewReader(good), digest(good)); err != nil {
		t.Errorf("the good one: %v", err)
	}

	// Without the master key its secrets would be lost, and that is said before anything is read.
	refusedSaying(t, restoreTo(t.TempDir(), good, RestoreOptions{}), "master key")
	refusedSaying(t, restoreTo(t.TempDir(), good, RestoreOptions{KeyFingerprint: "another"}), "different master key")

	// A server that is there is not replaced by accident, and is kept aside when asked.
	there := t.TempDir()
	os.WriteFile(filepath.Join(there, "moorage.db"), []byte("what was there"), 0o600)
	os.MkdirAll(filepath.Join(there, "data", "old"), 0o755)
	refusedSaying(t, restoreTo(there, good, RestoreOptions{KeyFingerprint: "abc"}), "--force")
	if got := readFile(t, filepath.Join(there, "moorage.db")); got != "what was there" {
		t.Errorf("%q", got)
	}
	restored, err := Restore(ctx, bytes.NewReader(good), "", RestoreOptions{DataDir: there, KeyFingerprint: "abc", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Aside == "" || readFile(t, filepath.Join(restored.Aside, "moorage.db")) != "what was there" {
		t.Errorf("%+v", restored)
	}
	if _, err := os.Stat(filepath.Join(restored.Aside, "data", "old")); err != nil {
		t.Error(err)
	}
	if readFile(t, filepath.Join(there, "data/shop/production/notes.txt")) != "written by the app" {
		t.Error("not restored")
	}

	// A backup from something newer.
	refusedSaying(t, restoreTo(t.TempDir(), crafted(t, Manifest{Format: Format + 1, Version: "9.0"}, nil), RestoreOptions{KeyFingerprint: "abc"}), "newer")
	refusedSaying(t, restoreTo(t.TempDir(), crafted(t, Manifest{Format: Format, Version: "9.0", Schema: 100000}, nil), RestoreOptions{KeyFingerprint: "abc"}), "newer")

	// With keys of its own, it needs none on this machine, and puts them in place.
	withKeys := options(from)
	withKeys.IncludeKeys = true
	whole := t.TempDir()
	if err := restoreTo(whole, archiveOf(t, from, withKeys), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(whole, "keys/master.key")); got != "the master key" {
		t.Errorf("%q", got)
	}
}

// crafted makes an archive by hand, of what no backup would hold.
func crafted(t *testing.T, m Manifest, entries []tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := writeJSON(tw, manifestName, m, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, h := range entries {
		h := h
		body := "evil"
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(body))
		}
	}
	if err := writeJSON(tw, endName, Summary{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// TestRestoreStaysInItsDirectory: a backup is a file that came from somewhere, and may be run as root.
func TestRestoreStaysInItsDirectory(t *testing.T) {
	ctx := context.Background()
	m := Manifest{Format: Format, IncludesKeys: true}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]tar.Header{
		"up and out":         {{Name: "../../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"from the top":       {{Name: outside + "/evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"deeper, up and out": {{Name: "data/../../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
	}
	if runtime.GOOS != "windows" {
		cases["through a link"] = []tar.Header{
			{Name: "data", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777},
			{Name: "data/evil", Typeflag: tar.TypeReg, Mode: 0o644},
		}
		cases["a link out, then a file over it"] = []tar.Header{
			{Name: "repos/link", Typeflag: tar.TypeSymlink, Linkname: outside + "/evil", Mode: 0o777},
			{Name: "repos/link", Typeflag: tar.TypeReg, Mode: 0o644},
		}
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			to := t.TempDir()
			_, err := Restore(ctx, bytes.NewReader(crafted(t, m, entries)), "", RestoreOptions{DataDir: to})
			if err == nil {
				t.Error("it was restored")
			}
			if _, err := os.Stat(filepath.Join(outside, "evil")); err == nil {
				t.Fatal("it wrote outside the data directory")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(to), "evil")); err == nil {
				t.Fatal("it wrote outside the data directory")
			}
		})
	}
}

func TestPruneKeepsTheNewest(t *testing.T) {
	ctx := context.Background()
	dest := &Local{Dir: t.TempDir()}
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("moorage-2026110%dT000000Z.tar.gz", i)
		dest.Put(ctx, name, strings.NewReader("x"))
		dest.Put(ctx, name+digestSuffix, strings.NewReader("y"))
	}
	dest.Put(ctx, "something-else.txt", strings.NewReader("mine"))
	if _, err := Prune(ctx, dest, 2); err != nil {
		t.Fatal(err)
	}
	list, err := dest.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "moorage-20261105T000000Z.tar.gz" || list[1].Name != "moorage-20261104T000000Z.tar.gz" {
		t.Errorf("%+v %v", list, err)
	}
	entries, _ := os.ReadDir(dest.Dir)
	if len(entries) != 5 { // two with what they hash to, and what is not ours
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%v", names)
	}
	if _, err := Prune(ctx, dest, 0); err != nil { // keeps them all
		t.Fatal(err)
	}
	if list, _ := dest.List(ctx); len(list) != 2 {
		t.Errorf("%+v", list)
	}
}

// A destination that fails leaves nothing that looks like a backup.
func TestLocalPutIsAllOrNothing(t *testing.T) {
	dest := &Local{Dir: t.TempDir()}
	err := dest.Put(context.Background(), "moorage-x.tar.gz", io.MultiReader(strings.NewReader("half"), failingReader{}))
	if err == nil {
		t.Fatal("no error")
	}
	if entries, _ := os.ReadDir(dest.Dir); len(entries) != 0 {
		t.Errorf("%v", entries)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the disk went") }

func TestParse(t *testing.T) {
	if _, err := Parse("", Credentials{}); err == nil {
		t.Error("nowhere")
	}
	if _, err := Parse("s3://", Credentials{AccessKey: "a", SecretKey: "b"}); err == nil {
		t.Error("no bucket")
	}
	if _, err := Parse("s3://bucket", Credentials{}); err == nil {
		t.Error("no credentials")
	}
	d, err := Parse("s3://bucket/some/prefix/", Credentials{AccessKey: "a", SecretKey: "b"})
	if err != nil || d.String() != "s3://bucket/some/prefix" {
		t.Errorf("%v %v", d, err)
	}
	if l, err := Parse("relative/path", Credentials{}); err != nil || !filepath.IsAbs(l.String()) {
		t.Errorf("%v %v", l, err)
	}
}

// TestSignature checks signing against the example in AWS's own test suite.
func TestSignature(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Date", "20150830T123600Z")
	empty := sha256.Sum256(nil)
	auth, sig := signature(req, hex.EncodeToString(empty[:]), "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "service", "20150830T123600Z", "20150830")
	if sig != "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31" {
		t.Errorf("%s", auth)
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, ") {
		t.Errorf("%s", auth)
	}
}

// fakeS3 is enough of a service to back up to, and checks each request's signature.
type fakeS3 struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	parts   int
	aborted int
	failAt  int
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{t: t, objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha256.Sum256(body)
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		f.t.Errorf("%s %s: the content's hash is wrong", r.Method, r.URL)
	}
	stamp := r.Header.Get("X-Amz-Date")
	check := r.Clone(r.Context())
	check.Header.Del("Authorization")
	_, sig := signature(check, r.Header.Get("X-Amz-Content-Sha256"), "access", "secret", "us-east-1", "s3", stamp, stamp[:8])
	if !strings.HasSuffix(r.Header.Get("Authorization"), "Signature="+sig) {
		f.t.Errorf("%s %s: the signature is wrong: %s", r.Method, r.URL, r.Header.Get("Authorization"))
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := "upload-" + strconv.Itoa(len(f.uploads)+1)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		f.parts++
		if f.failAt > 0 && f.parts >= f.failAt {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "<Error><Code>SlowDown</Code><Message>not now</Message></Error>")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var done struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				ETag   string `xml:"ETag"`
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &done); err != nil {
			f.t.Errorf("%v: %s", err, body)
		}
		var whole []byte
		for i, p := range done.Parts {
			if p.Number != i+1 || p.ETag != fmt.Sprintf(`"etag-%d"`, i+1) {
				f.t.Errorf("part %d: %+v", i, p)
			}
			whole = append(whole, f.uploads[q.Get("uploadId")][p.Number]...)
		}
		f.objects[key] = whole
		fmt.Fprint(w, "<CompleteMultipartUploadResult></CompleteMultipartUploadResult>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.aborted++
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.objects[key] = body
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		var names []string
		for name := range f.objects {
			if strings.HasPrefix(name, q.Get("prefix")) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		fmt.Fprint(w, "<ListBucketResult>")
		for _, name := range names {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-11-04T00:00:00Z</LastModified></Contents>", name, len(f.objects[name]))
		}
		fmt.Fprint(w, "</ListBucketResult>")
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(data)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("%s %s", r.Method, r.URL)
	}
}

func TestS3(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeS3(t)
	dest, err := Parse("s3://bucket/some/prefix", Credentials{Endpoint: srv.URL, AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	dest.(*S3).Pause = time.Millisecond
	// Small, and in parts.
	if err := dest.Put(ctx, "moorage-small.tar.gz", strings.NewReader("small")); err != nil {
		t.Fatal(err)
	}
	if string(f.objects["some/prefix/moorage-small.tar.gz"]) != "small" {
		t.Errorf("%v", f.objects)
	}
	big := make([]byte, 2*partSize+1234)
	rand.Read(big)
	if err := dest.Put(ctx, "moorage-big.tar.gz", bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.objects["some/prefix/moorage-big.tar.gz"], big) || f.parts != 3 {
		t.Errorf("%d bytes in %d parts", len(f.objects["some/prefix/moorage-big.tar.gz"]), f.parts)
	}
	// Exactly a part's worth is one part, with nothing empty after it.
	exact := make([]byte, 2*partSize)
	f.parts = 0
	if err := dest.Put(ctx, "moorage-exact.tar.gz", bytes.NewReader(exact)); err != nil {
		t.Fatal(err)
	}
	if f.parts != 2 {
		t.Errorf("%d parts", f.parts)
	}
	f.objects["some/prefix/other.txt"] = []byte("not ours")
	list, err := dest.List(ctx)
	if err != nil || len(list) != 3 || list[0].Name != "moorage-small.tar.gz" {
		t.Errorf("%+v %v", list, err)
	}
	r, err := dest.Get(ctx, "moorage-small.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "small" {
		t.Errorf("%q", data)
	}
	if _, err := dest.Get(ctx, "nothing"); err == nil {
		t.Error("nothing was found")
	}
	if err := dest.Delete(ctx, "moorage-small.tar.gz"); err != nil {
		t.Fatal(err)
	}

	// One that fails halfway is aborted, leaves nothing, and says what the service said.
	f.parts, f.failAt = 0, 2
	err = dest.Put(ctx, "moorage-failed.tar.gz", bytes.NewReader(big))
	if err == nil || !strings.Contains(err.Error(), "SlowDown") || !strings.Contains(err.Error(), "not now") {
		t.Errorf("%v", err)
	}
	if f.aborted != 1 {
		t.Errorf("%d aborted", f.aborted)
	}
	if _, ok := f.objects["some/prefix/moorage-failed.tar.gz"]; ok {
		t.Error("what failed is there")
	}
	f.failAt = 0

	// A whole backup, there and back.
	from := newServerData(t)
	result, err := Run(ctx, dest, options(from))
	if err != nil {
		t.Fatal(err)
	}
	opened, digest, err := Open(ctx, "s3://bucket/some/prefix/"+result.Name, Credentials{Endpoint: srv.URL, AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if digest == "" {
		t.Error("nothing says what it should hash to")
	}
	to := t.TempDir()
	if _, err := Restore(ctx, opened, digest, RestoreOptions{DataDir: to, KeyFingerprint: "abc"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(to, "data/shop/production/notes.txt")); got != "written by the app" {
		t.Errorf("%q", got)
	}
}

// AWS has buckets as hostnames, and a service of one's own as paths.
func TestS3Addresses(t *testing.T) {
	aws := &S3{Bucket: "b", Credentials: Credentials{Region: "eu-west-1"}}
	if u := aws.url("k", nil); u.String() != "https://b.s3.eu-west-1.amazonaws.com/k" {
		t.Errorf("%s", u)
	}
	own := &S3{Bucket: "b", Credentials: Credentials{Endpoint: "http://minio.local:9000"}}
	if u := own.url("k", nil); u.String() != "http://minio.local:9000/b/k" {
		t.Errorf("%s", u)
	}
}

func TestManifestReadsBack(t *testing.T) {
	data, err := json.Marshal(Manifest{Format: 1, Schema: 2, IncludesKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil || m.Schema != 2 || !m.IncludesKeys {
		t.Errorf("%+v %v", m, err)
	}
}

func archiveOfAServer(t *testing.T) []byte {
	t.Helper()
	from := newServerData(t)
	return archiveOf(t, from, options(from))
}

// serverIn makes a data directory that holds a server, by restoring one into it.
func serverIn(t *testing.T, there string, archive []byte, at time.Time) Restored {
	t.Helper()
	restored, err := Restore(context.Background(), bytes.NewReader(archive), "", RestoreOptions{
		DataDir: there, KeyFingerprint: "abc", Force: true, Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	return restored
}

// TestRestoresWithinASecond: the name of what is kept has the second in it, and the drill found that a
// fast machine can do two in one.
func TestRestoresWithinASecond(t *testing.T) {
	good := archiveOfAServer(t)
	there := t.TempDir()
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	serverIn(t, there, good, at)
	first := serverIn(t, there, good, at)
	second := serverIn(t, there, good, at)
	if first.Aside == "" || second.Aside == "" || first.Aside == second.Aside {
		t.Errorf("%q and %q", first.Aside, second.Aside)
	}
	for _, aside := range []string{first.Aside, second.Aside} {
		if _, err := os.Stat(filepath.Join(aside, databaseName)); err != nil {
			t.Error(err)
		}
	}
}

// TestRestoreOverNothing says nothing was kept when there was nothing to keep: starting a command
// makes empty directories, and those are not a server that was there.
func TestRestoreOverNothing(t *testing.T) {
	good := archiveOfAServer(t)
	there := t.TempDir()
	for _, name := range []string{"builds", "certs", "repos"} {
		if err := os.Mkdir(filepath.Join(there, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := Restore(context.Background(), bytes.NewReader(good), "", RestoreOptions{DataDir: there, KeyFingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Aside != "" {
		t.Errorf("kept %s", restored.Aside)
	}
	if names, _ := filepath.Glob(filepath.Join(there, "before-restore-*")); len(names) != 0 {
		t.Errorf("%v", names)
	}
}

// TestFailedRestorePutsItBack leaves the server as it was, and not half of each.
func TestFailedRestorePutsItBack(t *testing.T) {
	good := archiveOfAServer(t)
	there := t.TempDir()
	serverIn(t, there, good, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	marker := filepath.Join(there, databaseName)
	if err := os.WriteFile(marker, []byte("what was there"), 0o600); err != nil {
		t.Fatal(err)
	}
	// This comes before the database, so that it has been replaced by the time that fails.
	mine := filepath.Join(there, "data", "mine")
	if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(there, "certs")
	if err := os.RemoveAll(empty); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}

	renameInto = func(from, to string) error {
		if filepath.Base(to) == databaseName {
			return errors.New("the disk has had enough")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renameInto = os.Rename })
	restored, err := Restore(context.Background(), bytes.NewReader(good), "", RestoreOptions{DataDir: there, KeyFingerprint: "abc", Force: true})
	if err == nil || !strings.Contains(err.Error(), "had enough") {
		t.Fatalf("%v", err)
	}
	if restored.Aside != "" {
		t.Errorf("it says it kept %s", restored.Aside)
	}
	if got := readFile(t, marker); got != "what was there" {
		t.Errorf("%q", got)
	}
	if got := readFile(t, mine); got != "mine" {
		t.Errorf("%q", got)
	}
	if names, _ := filepath.Glob(filepath.Join(there, "before-restore-*")); len(names) != 0 {
		t.Errorf("%v", names)
	}
	if info, err := os.Stat(empty); err != nil || !info.IsDir() {
		t.Errorf("%v %v", info, err)
	}
}

type testKey string

func (k testKey) Derive(purpose string) []byte {
	sum := sha256.Sum256([]byte(string(k) + purpose))
	return sum[:]
}

// entriesOf lists what an archive holds, and what.
func entriesOf(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(body)
	}
}

// TestPrivateKeysAreSealed: what an archive holds of a server's private keys is no use without the master key, and
// comes back with it, so that browsers that trust the server's own authority still do.
func TestPrivateKeysAreSealed(t *testing.T) {
	from := newServerData(t)
	if err := os.MkdirAll(filepath.Join(from, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "certs", "ca.key"), []byte("THE PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "certs", "ca.crt"), []byte("the public one"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := options(from)
	o.Keys = testKey("the master key")
	archive := archiveOf(t, from, o)
	entries := entriesOf(t, archive)
	for name, body := range entries {
		if strings.Contains(body, "THE PRIVATE KEY") {
			t.Errorf("%s holds it", name)
		}
	}
	if entries["certs/ca.crt"] != "the public one" {
		t.Errorf("%q", entries["certs/ca.crt"])
	}
	if _, ok := entries["certs/ca.key.sealed"]; !ok {
		t.Errorf("%v", entries)
	}

	to := t.TempDir()
	_, err := Restore(context.Background(), bytes.NewReader(archive), "", RestoreOptions{DataDir: to, KeyFingerprint: "abc", Keys: testKey("the master key")})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(to, "certs", "ca.key")); got != "THE PRIVATE KEY" {
		t.Errorf("%q", got)
	}
	// Tampered with, or with another key, it does not open.
	_, err = Restore(context.Background(), bytes.NewReader(archive), "", RestoreOptions{DataDir: t.TempDir(), KeyFingerprint: "abc", Keys: testKey("another")})
	refusedSaying(t, err, "does not")

	// With no key to seal with, they are left out rather than left in the open.
	o.Keys = nil
	entries = entriesOf(t, archiveOf(t, from, o))
	if _, ok := entries["certs/ca.key"]; ok {
		t.Errorf("%v", entries)
	}
	if _, ok := entries["certs/ca.key.sealed"]; ok {
		t.Errorf("%v", entries)
	}
}

// TestBackupThroughAnAppsLinks: an app can turn what it owns into a link while it is being read, and a backup
// should end up with its own files, and nobody else's.
func TestBackupThroughAnAppsLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("apps do not run as anyone else here")
	}
	from := newServerData(t)
	secret := filepath.Join(from, "keys", "master.key")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("THE MASTER KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(from, "data", "shop", "production")
	other := filepath.Join(from, "data", "other", "production")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "theirs"), []byte("ANOTHER APP'S"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Links it has left there are kept as links, and read as nothing more.
	for name, to := range map[string]string{"key": secret, "dir": filepath.Dir(secret), "next": "../../other/production"} {
		if err := os.Symlink(to, filepath.Join(env, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Swapped while a backup goes on. Many tries give it every chance.
	file := filepath.Join(env, "flip")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			os.Remove(file)
			if i%2 == 0 {
				os.WriteFile(file, []byte("mine"), 0o644)
			} else {
				os.Symlink(secret, file)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for range 40 {
		for name, body := range entriesOf(t, archiveOf(t, from, options(from))) {
			if strings.Contains(body, "THE MASTER KEY") || (strings.Contains(body, "ANOTHER APP'S") && !strings.HasPrefix(name, "data/other/")) {
				t.Fatalf("%s holds what it should not: %q", name, body)
			}
		}
	}
}

// TestRestoreOnlyWhatABackupHolds: an archive can come from anywhere, and a server running as root starts on what it
// restores: nothing that tells it what to run, whose it is, or where a directory really is.
func TestRestoreOnlyWhatABackupHolds(t *testing.T) {
	m := Manifest{Format: Format, Version: "0.6.0", KeyFingerprint: "abc"}
	reg := func(name string) tar.Header { return tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644} }
	link := func(name, to string) tar.Header {
		return tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: to, Mode: 0o777}
	}
	for _, c := range []struct {
		what  string
		m     Manifest
		entry tar.Header
	}{
		{"how to go back from an upgrade", m, reg("upgrades/last.json")},
		{"what the shims run", m, reg("run/shims/x.spec.json")},
		{"a toolchain", m, reg("toolchains/node/22/bin/node")},
		{"the logs", m, link("logs", "/etc")},
		{"keys it says it does not have", m, reg("keys/admin-token")},
		{"a link for keys", Manifest{Format: Format, Version: "0.6.0", IncludesKeys: true}, link("keys/master.key", "/etc/shadow")},
		{"a link for an environment", m, link("data/shop/production", "/etc")},
		{"a link for an app's data", m, link("data/shop", "/etc")},
		{"a link for a build", m, link("builds/shop/b1", "/etc")},
		{"a link for the database", m, link("moorage.db", "/etc/passwd")},
		{"a link in the certificates", m, link("certs/ca.key", "/etc/shadow")},
	} {
		t.Run(c.what, func(t *testing.T) {
			to := t.TempDir()
			_, err := Restore(context.Background(), bytes.NewReader(crafted(t, c.m, []tar.Header{c.entry})), "", RestoreOptions{DataDir: to, KeyFingerprint: "abc"})
			refusedSaying(t, err, "no backup should")
		})
	}
	// An app's own links, deeper down, are its own business.
	to := t.TempDir()
	_, err := Restore(context.Background(), bytes.NewReader(crafted(t, m, []tar.Header{
		{Name: "data/shop/production/", Typeflag: tar.TypeDir, Mode: 0o755},
		link("data/shop/production/current", "releases/1"),
	})), "", RestoreOptions{DataDir: to, KeyFingerprint: "abc"})
	if err != nil {
		t.Errorf("%v", err)
	}
}

func TestIncludeKeysNeedsTheKeyHere(t *testing.T) {
	from := newServerData(t)
	os.Remove(filepath.Join(from, "keys", "master.key"))
	o := options(from)
	o.IncludeKeys = true
	var buf bytes.Buffer
	_, _, err := Write(context.Background(), &buf, o)
	refusedSaying(t, err, "keeps it elsewhere")
}

// TestS3TriesAgain: a service asks to be tried again now and then, and that should not cost a whole backup.
func TestS3TriesAgain(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s := &S3{Bucket: "b", Credentials: Credentials{Endpoint: srv.URL, AccessKey: "a", SecretKey: "s"}, Pause: time.Millisecond}
	if err := s.Put(context.Background(), "x", strings.NewReader("small")); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("%d", calls)
	}
	// One that goes on failing fails, in the end.
	calls = -100
	if err := s.Put(context.Background(), "x", strings.NewReader("small")); err == nil {
		t.Error("it said it worked")
	}
	if calls != -100+requestTries {
		t.Errorf("%d", calls)
	}
}

// TestS3ThatGoesQuiet: a service that stops answering should not hold up a backup for ever.
func TestS3ThatGoesQuiet(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	s := &S3{Bucket: "b", Credentials: Credentials{Endpoint: srv.URL, AccessKey: "a", SecretKey: "s"}, Pause: time.Millisecond, Timeout: 50 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- s.Put(context.Background(), "x", strings.NewReader("small")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("it said it worked")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("still waiting")
	}
}
