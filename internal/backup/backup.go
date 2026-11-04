// Package backup makes and restores backups of a server (spec/08-security.md, "Backups and
// recovery"): its database, repositories, builds, apps' data and certificates, as one archive.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/store"
)

// Format is the version of the archive.
const Format = 1

const (
	manifestName = "manifest.json"
	endName      = "complete.json"
	databaseName = "moorage.db"
	suffix       = ".tar.gz"
	digestSuffix = ".sha256"
	namePrefix   = "moorage-"
)

// Manifest says what an archive is. It comes first, so that a restore finds out early.
type Manifest struct {
	Format  int    `json:"format"`
	Version string `json:"version"`
	// Schema is the version of the database's schema.
	Schema    int       `json:"schema"`
	CreatedAt time.Time `json:"created_at"`
	// KeyFingerprint says which master key its secrets need, and IncludesKeys whether it has it.
	KeyFingerprint string `json:"key_fingerprint"`
	IncludesKeys   bool   `json:"includes_keys"`
	Domain         string `json:"domain,omitempty"`
}

// Summary comes last: an archive without it did not finish.
type Summary struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Changed lists files that changed while they were read.
	Changed []string `json:"changed,omitempty"`
}

// Options say what to back up.
type Options struct {
	DataDir string
	// Keys seals the private keys among the certificates, so that an archive is no use without the master key.
	// With none, and no IncludeKeys, they are left out.
	Keys Keyed
	// IncludeKeys adds the master key and other keys, which then have to be kept as safe as they are.
	IncludeKeys    bool
	KeyFingerprint string
	Version        string
	Domain         string
	// Now, if set, overrides the time (tests).
	Now func() time.Time
	// Log, if set, hears what it is doing.
	Log func(format string, args ...any)

	name string
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// parts are what is in an archive, of what a data directory holds. Logs, toolchains and caches are
// made again as they are needed, and the keys are apart.
var parts = []string{"repos", "builds", "data", "certs"}

// Write writes an archive to w, and says what it held.
func Write(ctx context.Context, w io.Writer, o Options) (Manifest, Summary, error) {
	var sum Summary
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	if o.IncludeKeys {
		// Saying that it holds the master key is what makes it restorable with nothing else, so it had better.
		if _, err := os.Lstat(filepath.Join(o.DataDir, "keys", "master.key")); err != nil {
			return Manifest{}, sum, &RefusedError{"--include-keys needs the master key in the data directory, and this server keeps it elsewhere: keep that safe yourself, and leave out --include-keys"}
		}
	}

	// The database comes first, so that nothing in it is newer than what follows.
	if err := os.MkdirAll(filepath.Join(o.DataDir, "tmp"), 0o700); err != nil {
		return Manifest{}, sum, err
	}
	tmp, err := os.MkdirTemp(filepath.Join(o.DataDir, "tmp"), "backup-")
	if err != nil {
		return Manifest{}, sum, err
	}
	defer os.RemoveAll(tmp)
	snapshot := filepath.Join(tmp, databaseName)
	o.log("the database")
	schema, err := store.SnapshotDatabase(ctx, filepath.Join(o.DataDir, databaseName), snapshot)
	if err != nil {
		return Manifest{}, sum, fmt.Errorf("snapshot the database: %w", err)
	}
	if err := store.MarkBackupInSnapshot(ctx, snapshot, o.name, o.now()); err != nil {
		return Manifest{}, sum, fmt.Errorf("snapshot the database: %w", err)
	}
	m := Manifest{Format: Format, Version: o.Version, Schema: schema, CreatedAt: o.now(), KeyFingerprint: o.KeyFingerprint, IncludesKeys: o.IncludeKeys, Domain: o.Domain}
	if err := writeJSON(tw, manifestName, m, o.now()); err != nil {
		return m, sum, err
	}
	if err := addOne(ctx, tw, tmp, databaseName, &sum); err != nil {
		return m, sum, err
	}
	list := parts
	if o.IncludeKeys {
		list = append(append([]string{}, parts...), "keys")
	}
	for _, part := range list {
		o.log("%s", part)
		if err := addPart(ctx, tw, o, part, &sum); err != nil {
			return m, sum, fmt.Errorf("%s: %w", part, err)
		}
	}
	if err := writeJSON(tw, endName, sum, o.now()); err != nil {
		return m, sum, err
	}
	if err := tw.Close(); err != nil {
		return m, sum, err
	}
	return m, sum, gz.Close()
}

func writeJSON(tw *tar.Writer, name string, v any, at time.Time) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: at, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

// addPart adds one of the parts of a data directory. An app's data is its own to change, as it is being read,
// so that each environment's is read through a handle that cannot leave it: a link an app swaps in, to the
// master key or to another app's files, leads nowhere.
func addPart(ctx context.Context, tw *tar.Writer, o Options, part string, sum *Summary) error {
	top := filepath.Join(o.DataDir, part)
	switch part {
	case "data":
		root, err := openRoot(top)
		if root == nil {
			return err
		}
		defer root.Close()
		if err := addFile(ctx, tw, root, ".", part, sum, nil); err != nil {
			return err
		}
		apps, err := os.ReadDir(top)
		if err != nil {
			return err
		}
		for _, app := range apps {
			name := path.Join(part, app.Name())
			if err := addFile(ctx, tw, root, app.Name(), name, sum, nil); err != nil {
				return err
			}
			if !app.IsDir() {
				continue
			}
			envs, err := os.ReadDir(filepath.Join(top, app.Name()))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			for _, env := range envs {
				below := path.Join(name, env.Name())
				if !env.IsDir() {
					if err := addFile(ctx, tw, root, filepath.Join(app.Name(), env.Name()), below, sum, nil); err != nil {
						return err
					}
					continue
				}
				if err := addTree(ctx, tw, filepath.Join(top, app.Name(), env.Name()), below, sum, nil); err != nil {
					return err
				}
			}
		}
		return nil
	case "certs":
		if o.IncludeKeys {
			return addTree(ctx, tw, top, part, sum, nil)
		}
		return addTree(ctx, tw, top, part, sum, sealerFor(o.Keys))
	}
	return addTree(ctx, tw, top, part, sum, nil)
}

// openRoot opens a directory to read from, if it is there.
func openRoot(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return root, err
}

// addTree adds a directory and all that is in it.
func addTree(ctx context.Context, tw *tar.Writer, dir, name string, sum *Summary, seal sealer) error {
	root, err := openRoot(dir)
	if root == nil {
		return err
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // it has gone since the directory was read
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() && strings.HasSuffix(d.Name(), ".partial") {
			return filepath.SkipDir // a build still being made
		}
		return addFile(ctx, tw, root, filepath.FromSlash(rel), path.Join(name, rel), sum, seal)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// addOne adds a single file of a directory.
func addOne(ctx context.Context, tw *tar.Writer, dir, name string, sum *Summary) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return addFile(ctx, tw, root, name, name, sum, nil)
}

func addFile(ctx context.Context, tw *tar.Writer, root *os.Root, rel, name string, sum *Summary, seal sealer) error {
	info, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	link := ""
	switch {
	case info.Mode().IsRegular(), info.IsDir():
	case info.Mode()&fs.ModeSymlink != 0:
		if link, err = root.Readlink(rel); err != nil {
			return err
		}
	default:
		return nil // a socket or a pipe means nothing later
	}
	if !info.Mode().IsRegular() {
		h, err := headerOf(info, link, name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			h.Name += "/"
		}
		return tw.WriteHeader(h)
	}

	// It is opened with no way of following a link or waiting on a pipe, and what counts from here is what was
	// opened: what an app has put at this name since it was looked at is not what was looked at.
	f, err := root.OpenFile(rel, os.O_RDONLY|safeOpen, 0)
	if err != nil {
		now, statErr := root.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(statErr, fs.ErrNotExist) {
			return nil
		}
		if statErr == nil && !now.Mode().IsRegular() {
			sum.Changed = append(sum.Changed, name)
			return nil
		}
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		sum.Changed = append(sum.Changed, name)
		return nil
	}
	h, err := headerOf(opened, "", name)
	if err != nil {
		return err
	}
	if seal != nil && !strings.HasSuffix(name, ".crt") {
		return addSealed(tw, f, h, sum, seal)
	}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	copied, err := io.CopyN(tw, &contextReader{ctx, f}, h.Size)
	changed := false
	if errors.Is(err, io.EOF) {
		// It shrank while being read. The archive has been told the size, so that the rest is zeros.
		if _, err := io.CopyN(tw, zeros{}, h.Size-copied); err != nil {
			return err
		}
		changed = true
	} else if err != nil {
		return err
	}
	if after, err := f.Stat(); err == nil && (after.Size() != h.Size || !after.ModTime().Equal(opened.ModTime())) {
		changed = true
	}
	if changed {
		sum.Changed = append(sum.Changed, name)
	}
	sum.Files++
	sum.Bytes += h.Size
	return nil
}

func headerOf(info fs.FileInfo, link, name string) (*tar.Header, error) {
	h, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return nil, err
	}
	h.Name = name
	h.Uname, h.Gname = "", ""
	h.Format = tar.FormatPAX
	return h, nil
}

// sealedSuffix marks a file that an archive holds sealed with the master key.
const sealedSuffix = ".sealed"

// sealer seals what should not be readable from an archive alone.
type sealer func(plain []byte) ([]byte, error)

// sealerFor seals with the master key, and with none, leaves things out: a nil key gives a sealer that says so.
func sealerFor(key Keyed) sealer {
	if key == nil {
		return func([]byte) ([]byte, error) { return nil, nil }
	}
	return func(plain []byte) ([]byte, error) {
		aead, err := keysAEAD(key)
		if err != nil {
			return nil, err
		}
		nonce := make([]byte, aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
		return aead.Seal(nonce, nonce, plain, []byte(sealedSuffix)), nil
	}
}

func keysAEAD(key Keyed) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key.Derive("backup private keys v1"))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// openSealed opens what sealerFor sealed.
func openSealed(key Keyed, sealed []byte) ([]byte, error) {
	aead, err := keysAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("it is too short")
	}
	return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(sealedSuffix))
}

// addSealed adds a file sealed. Those it is for are small.
func addSealed(tw *tar.Writer, f io.Reader, h *tar.Header, sum *Summary, seal sealer) error {
	plain, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil {
		return err
	}
	sealed, err := seal(plain)
	if err != nil {
		return err
	}
	if sealed == nil {
		return nil
	}
	h.Name += sealedSuffix
	h.Size = int64(len(sealed))
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if _, err := tw.Write(sealed); err != nil {
		return err
	}
	sum.Files++
	sum.Bytes += h.Size
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Result says what a backup did.
type Result struct {
	Name string
	Size int64
	Manifest
	Summary
}

// Run makes a backup at a destination, with a file beside it that says what it should hash to.
func Run(ctx context.Context, dest Destination, o Options) (Result, error) {
	name := namePrefix + o.now().Format("20060102T150405Z") + suffix
	o.name = name
	reader, writer := io.Pipe()
	hash := sha256.New()
	counted := &counter{}
	type done struct {
		m   Manifest
		s   Summary
		err error
	}
	finished := make(chan done, 1)
	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		m, s, err := Write(writeCtx, io.MultiWriter(writer, hash, counted), o)
		writer.CloseWithError(err)
		finished <- done{m, s, err}
	}()
	err := dest.Put(ctx, name, reader)
	if err != nil {
		reader.CloseWithError(err) // what writes gets the same, and stops
	}
	result := <-finished
	if err == nil {
		err = result.err
	}
	if err != nil {
		_ = dest.Delete(context.WithoutCancel(ctx), name)
		return Result{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil)) + "  " + name + "\n"
	if err := dest.Put(ctx, name+digestSuffix, strings.NewReader(digest)); err != nil {
		return Result{}, fmt.Errorf("write what it hashes to: %w", err)
	}
	return Result{Name: name, Size: counted.n, Manifest: result.m, Summary: result.s}, nil
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// Prune keeps the newest backups at a destination, and lets go of the rest.
func Prune(ctx context.Context, dest Destination, keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	stored, err := dest.List(ctx)
	if err != nil {
		return nil, err
	}
	var gone []string
	// Names start with the time, and the list is newest first.
	for i, s := range stored {
		if i < keep {
			continue
		}
		if err := dest.Delete(ctx, s.Name); err != nil {
			return gone, err
		}
		gone = append(gone, s.Name)
		if err := dest.Delete(ctx, s.Name+digestSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return gone, err
		}
	}
	return gone, nil
}

// isBackupName says whether a name is one of ours.
func isBackupName(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(base, namePrefix) && strings.HasSuffix(base, suffix)
}
