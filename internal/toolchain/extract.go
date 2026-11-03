package toolchain

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// extract unpacks a .tar.gz or .zip archive into dir, dropping the single
// top-level directory the archive's entries share, if they share one.
// Symbolic links are kept when they point inside dir (Node.js ships its npm
// commands as links); anything that would land outside dir is refused.
func extract(archive, dir string) error {
	var err error
	switch {
	case strings.HasSuffix(archive, ".zip"):
		err = extractZip(archive, dir)
	default:
		err = extractTar(archive, dir)
	}
	if err != nil {
		return fmt.Errorf("unpack %s: %w", filepath.Base(archive), err)
	}
	return nil
}

// entryPath cleans an archive entry's name and removes the prefix shared by
// all entries; it returns "" for the prefix itself.
func entryPath(name, prefix string) (string, error) {
	name = strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, `\`, "/")), "/")
	if name == "" || name == "." {
		return "", nil
	}
	if prefix != "" {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			if name+"/" == prefix {
				return "", nil
			}
			return "", fmt.Errorf("%s is outside %s", name, prefix)
		}
		name = rest
	}
	return name, nil
}

// commonPrefix returns "top/" if every entry is the directory top or sits
// under it.
func commonPrefix(names []string) string {
	top, nested := "", false
	for _, n := range names {
		n = strings.Trim(path.Clean("/"+strings.ReplaceAll(n, `\`, "/")), "/")
		first, rest, _ := strings.Cut(n, "/")
		if top == "" {
			top = first
		} else if first != top {
			return ""
		}
		nested = nested || rest != ""
	}
	if !nested {
		return ""
	}
	return top + "/"
}

func inside(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func extractTar(archive, dir string) error {
	names, err := tarNames(archive)
	if err != nil {
		return err
	}
	prefix := commonPrefix(names)
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := entryPath(h.Name, prefix)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(target, tr, h.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if path.IsAbs(h.Linkname) || !inside(dir, filepath.Join(filepath.Dir(target), filepath.FromSlash(h.Linkname))) {
				return fmt.Errorf("%s links outside the toolchain", name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			src, err := entryPath(h.Linkname, prefix)
			if err != nil || src == "" {
				return fmt.Errorf("%s: bad hard link", name)
			}
			if err := os.Link(filepath.Join(dir, filepath.FromSlash(src)), target); err != nil {
				return err
			}
		}
	}
}

func tarNames(archive string) ([]string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := h.Name
		if h.Typeflag == tar.TypeDir && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		names = append(names, name)
	}
}

func extractZip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	prefix := commonPrefix(names)
	for _, f := range zr.File {
		name, err := entryPath(f.Name, prefix)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue // zip archives of toolchains for Windows carry no links that matter
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(target, rc, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
