package gitserver

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage"

	"github.com/robert-mcdermott/moorage/internal/ignore"
)

// ExportLimits bound what one commit may write.
type ExportLimits struct {
	MaxFiles int
	MaxBytes int64
}

// DefaultExportLimits match the limits of an uploaded directory.
var DefaultExportLimits = ExportLimits{MaxFiles: 100_000, MaxBytes: 1 << 30}

// Exported says what an export wrote, and what it left out.
type Exported struct {
	Files int
	Bytes int64
	// Symlinks and Submodules are paths that were not written. (Neither
	// is what .moorignore excludes, nor node_modules, .venv and
	// __pycache__, which the build makes again.)
	Symlinks, Submodules []string
	// LFS lists files that are Git LFS pointers, not their content.
	LFS []string
}

// always is left out of every export, as it is of an uploaded directory:
// the build makes it again.
var always = map[string]bool{"node_modules": true, ".venv": true, "__pycache__": true}

const lfsPointer = "version https://git-lfs.github.com/spec/v1"

// export writes the tree of commit into dir, which must be empty.
func export(st storage.Storer, commit plumbing.Hash, dir string, lim ExportLimits) (Exported, error) {
	var ex Exported
	c, err := object.GetCommit(st, commit)
	if err != nil {
		return ex, fmt.Errorf("read commit %s: %w", commit, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return ex, fmt.Errorf("read the tree of %s: %w", commit, err)
	}
	if err := writeTree(st, tree, dir, "", &ex, lim); err != nil {
		return ex, err
	}
	return ex, prune(dir)
}

func writeTree(st storage.Storer, t *object.Tree, dir, rel string, ex *Exported, lim ExportLimits) error {
	for _, e := range t.Entries {
		name := path.Join(rel, e.Name)
		if e.Name == "." || e.Name == ".." || e.Name == ".git" || strings.ContainsAny(e.Name, `/\`) {
			return fmt.Errorf("the commit has an entry %q that cannot be written", name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch e.Mode {
		case filemode.Dir:
			if always[e.Name] {
				continue
			}
			sub, err := object.GetTree(st, e.Hash)
			if err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := writeTree(st, sub, dir, name, ex, lim); err != nil {
				return err
			}
		case filemode.Regular, filemode.Deprecated, filemode.Executable:
			if err := writeBlob(st, e, target, name, ex, lim); err != nil {
				return err
			}
		case filemode.Symlink:
			ex.Symlinks = append(ex.Symlinks, name)
		case filemode.Submodule:
			ex.Submodules = append(ex.Submodules, name)
		default:
			return fmt.Errorf("%s has an unknown file mode %o", name, e.Mode)
		}
	}
	return nil
}

func writeBlob(st storage.Storer, e object.TreeEntry, target, name string, ex *Exported, lim ExportLimits) error {
	blob, err := object.GetBlob(st, e.Hash)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	ex.Files++
	if ex.Files > lim.MaxFiles {
		return fmt.Errorf("the commit has more than %d files", lim.MaxFiles)
	}
	if ex.Bytes+blob.Size > lim.MaxBytes {
		return fmt.Errorf("the commit's files are larger than %d bytes", lim.MaxBytes)
	}
	perm := os.FileMode(0o644)
	if e.Mode == filemode.Executable {
		perm = 0o755
	}
	r, err := blob.Reader()
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	defer r.Close()
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	// A pointer file is small and starts with its version line.
	head := make([]byte, len(lfsPointer))
	n, _ := io.ReadFull(r, head)
	if blob.Size < 1024 && bytes.Equal(head[:n], []byte(lfsPointer)) {
		ex.LFS = append(ex.LFS, name)
	}
	written, err := f.Write(head[:n])
	if err == nil {
		var rest int64
		rest, err = io.Copy(f, r)
		written += int(rest)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	ex.Bytes += int64(written)
	return nil
}

// prune removes what the app's .moorignore files exclude, as an upload
// leaves it out.
func prune(dir string) error {
	m, err := ignore.ForTree(dir, ".moorignore")
	if err != nil || m.Empty() {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if m.Match(filepath.ToSlash(rel), d.IsDir()) {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
}
