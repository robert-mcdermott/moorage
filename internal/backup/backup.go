// Package backup makes and restores backups of a server (spec/08-security.md, "Backups and
// recovery"): its database, repositories, builds, apps' data and certificates, as one archive.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
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

	// The database comes first, so that nothing in it is newer than what follows.
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
	if err := addFile(ctx, tw, snapshot, databaseName, &sum); err != nil {
		return m, sum, err
	}
	list := parts
	if o.IncludeKeys {
		list = append(append([]string{}, parts...), "keys")
	}
	for _, part := range list {
		o.log("%s", part)
		if err := addTree(ctx, tw, o.DataDir, part, &sum); err != nil {
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

func addTree(ctx context.Context, tw *tar.Writer, root, part string, sum *Summary) error {
	top := filepath.Join(root, part)
	err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // it has gone since the directory was read
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() && strings.HasSuffix(d.Name(), ".partial") {
			return filepath.SkipDir // a build still being made
		}
		return addFile(ctx, tw, p, name, sum)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func addFile(ctx context.Context, tw *tar.Writer, p, name string, sum *Summary) error {
	info, err := os.Lstat(p)
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
		if link, err = os.Readlink(p); err != nil {
			return err
		}
	default:
		return nil // a socket or a pipe means nothing later
	}
	h, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	h.Name = name
	if info.IsDir() {
		h.Name += "/"
	}
	h.Uname, h.Gname = "", ""
	h.Format = tar.FormatPAX
	if !info.Mode().IsRegular() {
		return tw.WriteHeader(h)
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	copied, err := io.CopyN(tw, &contextReader{ctx, f}, h.Size)
	if errors.Is(err, io.EOF) {
		// It shrank while being read. The archive has been told the size, so that the rest is zeros.
		if _, err := io.CopyN(tw, zeros{}, h.Size-copied); err != nil {
			return err
		}
		sum.Changed = append(sum.Changed, name)
	} else if err != nil {
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
func Prune(ctx context.Context, dest Destination, keep int) error {
	if keep <= 0 {
		return nil
	}
	stored, err := dest.List(ctx)
	if err != nil {
		return err
	}
	// Names start with the time, and the list is newest first.
	for i, s := range stored {
		if i < keep {
			continue
		}
		if err := dest.Delete(ctx, s.Name); err != nil {
			return err
		}
		if err := dest.Delete(ctx, s.Name+digestSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// isBackupName says whether a name is one of ours.
func isBackupName(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(base, namePrefix) && strings.HasSuffix(base, suffix)
}
