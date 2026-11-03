// Package archive packs a directory into a gzip-compressed tar stream for
// upload, and unpacks such a stream on the server without letting it write
// outside its destination.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bound what an archive may contain.
type Limits struct {
	MaxFiles int
	MaxBytes int64
}

// DefaultLimits allow 100,000 files and 1 GiB of content.
var DefaultLimits = Limits{MaxFiles: 100_000, MaxBytes: 1 << 30}

// Extract unpacks a gzip-compressed tar stream into dir, which must exist.
// Files keep their executable bit. Only regular files and directories are
// allowed: absolute names, names
// leaving dir, links, devices and duplicate files are errors, and so is
// exceeding lim. It returns the number of files and their total size.
func Extract(r io.Reader, dir string, lim Limits) (files int, size int64, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, fmt.Errorf("not a gzip stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, size, nil
		}
		if err != nil {
			return files, size, fmt.Errorf("read archive: %w", err)
		}
		name, err := cleanName(h.Name)
		if err != nil {
			return files, size, err
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if name == "." {
				continue
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, size, err
			}
		case tar.TypeReg:
			files++
			if files > lim.MaxFiles {
				return files, size, fmt.Errorf("archive has more than %d files", lim.MaxFiles)
			}
			if h.Size < 0 || size+h.Size > lim.MaxBytes {
				return files, size, fmt.Errorf("archive content is larger than %d bytes", lim.MaxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, size, err
			}
			perm := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				perm = 0o755 // executables stay executable
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
			if errors.Is(err, fs.ErrExist) {
				return files, size, fmt.Errorf("archive contains %q twice", name)
			}
			if err != nil {
				return files, size, err
			}
			n, err := io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return files, size, fmt.Errorf("extract %q: %w", name, err)
			}
			if n != h.Size {
				return files, size, fmt.Errorf("extract %q: archive ends early", name)
			}
			size += n
		default:
			return files, size, fmt.Errorf("archive entry %q is not a regular file or directory (links are not allowed)", h.Name)
		}
	}
}

// cleanName returns a tar entry's name as a clean relative slash path, or an
// error if it is absolute or leaves the destination.
func cleanName(name string) (string, error) {
	if strings.Contains(name, `\`) {
		return "", fmt.Errorf("archive entry %q contains a backslash", name)
	}
	if path.IsAbs(name) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("archive entry %q is an absolute path", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q leaves the destination", name)
	}
	return clean, nil
}

// Write packs the regular files and directories under dir into w as a
// gzip-compressed tar stream. Directories named .git are left out, and so is
// anything skip returns true for (rel is a slash path relative to dir).
// Symbolic links are left out and reported through skipped.
func Write(w io.Writer, dir string, skip func(rel string, d fs.DirEntry) bool) (files int, size int64, skipped []string, err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if skip != nil && skip(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o755, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			mode := int64(0o644)
			if info.Mode().Perm()&0o111 != 0 {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: mode, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			n, err := io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
			if n != info.Size() {
				return fmt.Errorf("%s changed while it was being packed", p)
			}
			files++
			size += n
		default:
			skipped = append(skipped, rel)
		}
		return nil
	})
	if err != nil {
		return files, size, skipped, err
	}
	if err := tw.Close(); err != nil {
		return files, size, skipped, err
	}
	return files, size, skipped, gz.Close()
}
