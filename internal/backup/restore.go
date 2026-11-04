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

// RefusedError says why an archive cannot be restored, with what to do about it.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// RestoreOptions say how to restore.
type RestoreOptions struct {
	DataDir string
	// KeyFingerprint says which master key this machine has, if any. Without it, or with another,
	// an archive with no keys of its own is refused: its secrets could not be opened.
	KeyFingerprint string
	// Force replaces what the data directory holds, which is moved aside rather than lost.
	Force bool
	// BeforeSwap, if set, runs once the archive has all arrived and is good, before anything changes:
	// what is running from the data directory has to stop by then.
	BeforeSwap func() error
	Now        func() time.Time
	Log        func(format string, args ...any)
}

// Restored says what a restore did.
type Restored struct {
	Manifest
	Summary
	// Aside is where what was replaced now is.
	Aside string
}

// Verify reads an archive through, and says what it holds, without changing anything. digest, if
// not empty, is what it should hash to.
func Verify(ctx context.Context, r io.Reader, digest string) (Manifest, Summary, error) {
	hash := sha256.New()
	m, sum, err := read(ctx, io.TeeReader(r, hash), nil, func(*tar.Header, io.Reader) error { return nil })
	if err != nil {
		return m, sum, err
	}
	if _, err := io.Copy(hash, r); err != nil {
		return m, sum, err
	}
	if digest != "" && hex.EncodeToString(hash.Sum(nil)) != digest {
		return m, sum, &RefusedError{"it does not hash to what was noted when it was made, so it has been changed or damaged"}
	}
	return m, sum, nil
}

// read goes through an archive, checking that it is one and is complete. Each entry goes to entry,
// after the manifest has said that it can be read at all, and accepted has had its say.
func read(ctx context.Context, r io.Reader, accepted func(Manifest) error, entry func(*tar.Header, io.Reader) error) (Manifest, Summary, error) {
	var m Manifest
	var sum Summary
	gz, err := gzip.NewReader(r)
	if err != nil {
		return m, sum, &RefusedError{"this is not a backup of a Moorage server"}
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	first, complete := true, false
	for {
		if err := ctx.Err(); err != nil {
			return m, sum, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, sum, &RefusedError{"this backup is damaged, or did not finish: " + err.Error()}
		}
		if first {
			if h.Name != manifestName {
				return m, sum, &RefusedError{"this is not a backup of a Moorage server"}
			}
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
				return m, sum, &RefusedError{"this backup says nothing readable about itself"}
			}
			if m.Format > Format {
				return m, sum, &RefusedError{fmt.Sprintf("this backup was made by a newer Moorage, %s: upgrade this one first", m.Version)}
			}
			latest, err := store.LatestSchema()
			if err != nil {
				return m, sum, err
			}
			if m.Schema > latest {
				return m, sum, &RefusedError{fmt.Sprintf("this backup was made by a newer Moorage, %s: upgrade this one first", m.Version)}
			}
			if accepted != nil {
				if err := accepted(m); err != nil {
					return m, sum, err
				}
			}
			first = false
			continue
		}
		if h.Name == endName {
			if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&sum); err != nil {
				return m, sum, &RefusedError{"the end of this backup cannot be read"}
			}
			complete = true
			continue
		}
		if err := entry(h, tr); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) {
				return m, sum, &RefusedError{"this backup is damaged, or did not finish: " + err.Error()}
			}
			return m, sum, err
		}
	}
	if !complete {
		return m, sum, &RefusedError{"this backup did not finish: it has no end"}
	}
	return m, sum, nil
}

// Restore replaces what a data directory holds with what an archive does. It goes to one side first,
// so that a damaged archive changes nothing, and then what was there is moved aside rather than lost.
// The server has to be stopped.
func Restore(ctx context.Context, r io.Reader, digest string, o RestoreOptions) (Restored, error) {
	var out Restored
	log := func(format string, args ...any) {
		if o.Log != nil {
			o.Log(format, args...)
		}
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	if _, err := os.Stat(filepath.Join(o.DataDir, databaseName)); err == nil && !o.Force {
		return out, &RefusedError{fmt.Sprintf("%s already holds a server: --force moves what it holds aside and restores over it", o.DataDir)}
	}
	if err := os.MkdirAll(filepath.Join(o.DataDir, "tmp"), 0o700); err != nil {
		return out, err
	}
	staging, err := os.MkdirTemp(filepath.Join(o.DataDir, "tmp"), "restore-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(staging)
	root, err := os.OpenRoot(staging)
	if err != nil {
		return out, err
	}
	defer root.Close()

	hash := sha256.New()
	type deferred struct {
		name string
		mode fs.FileMode
		at   time.Time
	}
	var dirs []deferred
	asRoot := os.Geteuid() == 0
	// That it could not be opened is said at once, and not after an hour of downloading it.
	accepted := func(m Manifest) error {
		if m.IncludesKeys {
			return nil
		}
		switch {
		case o.KeyFingerprint == "":
			return &RefusedError{"this backup needs the master key it was made with, which it does not include: put it in place first (keys/master.key in the data directory, or $MOORAGE_MASTER_KEY_FILE)"}
		case o.KeyFingerprint != m.KeyFingerprint:
			return &RefusedError{"this backup was made with a different master key, so that its secrets could not be opened: put that key in place first (keys/master.key in the data directory, or $MOORAGE_MASTER_KEY_FILE)"}
		}
		return nil
	}
	log("restoring")
	m, sum, err := read(ctx, io.TeeReader(r, hash), accepted, func(h *tar.Header, content io.Reader) error {
		name, err := cleanEntry(h.Name)
		if err != nil {
			return err
		}
		if dir := path.Dir(name); dir != "." {
			if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
				return err
			}
		}
		target := filepath.FromSlash(name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(target, 0o755); err != nil {
				return err
			}
			// Last, since a directory may not let even its owner add to it.
			dirs = append(dirs, deferred{target, fs.FileMode(h.Mode).Perm(), h.ModTime})
		case tar.TypeSymlink:
			if err := root.Symlink(h.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			f, err := root.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, content); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := root.Chmod(target, fs.FileMode(h.Mode).Perm()); err != nil {
				return err
			}
			_ = root.Chtimes(target, h.ModTime, h.ModTime)
		default:
			return nil
		}
		if asRoot {
			if err := root.Lchown(target, h.Uid, h.Gid); err != nil {
				return err
			}
		}
		return nil
	})
	out.Manifest, out.Summary = m, sum
	if err != nil {
		return out, err
	}
	// What follows the end of the archive still counts towards what it hashes to.
	if _, err := io.Copy(hash, r); err != nil {
		return out, err
	}
	if digest != "" && hex.EncodeToString(hash.Sum(nil)) != digest {
		return out, &RefusedError{"it does not hash to what was noted when it was made, so it has been changed or damaged"}
	}
	for i := len(dirs) - 1; i >= 0; i-- { // the deepest first, while the way down is still open
		if err := root.Chmod(dirs[i].name, dirs[i].mode); err != nil {
			return out, err
		}
		_ = root.Chtimes(dirs[i].name, dirs[i].at, dirs[i].at)
	}

	// Everything has arrived. What was there goes aside, and what arrived takes its place.
	if o.BeforeSwap != nil {
		if err := o.BeforeSwap(); err != nil {
			return out, err
		}
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return out, err
	}
	aside := filepath.Join(o.DataDir, "before-restore-"+now().UTC().Format("20060102T150405Z"))
	move := func(name string) error {
		from := filepath.Join(o.DataDir, name)
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err := os.MkdirAll(aside, 0o700); err != nil {
			return err
		}
		out.Aside = aside
		return os.Rename(from, filepath.Join(aside, name))
	}
	for _, name := range []string{databaseName + "-wal", databaseName + "-shm"} {
		if err := move(name); err != nil {
			return out, err
		}
	}
	for _, e := range entries {
		if err := move(e.Name()); err != nil {
			return out, err
		}
		if err := os.Rename(filepath.Join(staging, e.Name()), filepath.Join(o.DataDir, e.Name())); err != nil {
			return out, fmt.Errorf("move %s into place: %w", e.Name(), err)
		}
	}
	log("%d files", sum.Files)
	return out, nil
}

// cleanEntry refuses a name that would land outside the data directory. os.Root would stop it anyway.
func cleanEntry(name string) (string, error) {
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if !filepath.IsLocal(filepath.FromSlash(clean)) || clean == "." {
		return "", &RefusedError{fmt.Sprintf("this backup names %q, which is not in a data directory", name)}
	}
	return clean, nil
}
