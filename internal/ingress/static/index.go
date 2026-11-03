// Package static serves a published directory of files: a static site, or
// the static part of an app. Its rules come from the manifest's [static]
// table (docs/03-manifest.md).
package static

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Index describes every file of a published site. It is computed once when
// the site is published, so serving needs no directory scans and has strong
// ETags from content hashes.
type Index struct {
	Files map[string]File `json:"files"`
	// dirs holds every directory that contains a file ("" is the root).
	dirs map[string]bool
}

// File is one file of a site, keyed in Index.Files by its slash path
// relative to the site's root ("assets/app.js").
type File struct {
	Size int64 `json:"size"`
	// Hash is the hex SHA-256 of the content.
	Hash string `json:"sha256"`
	// Br and Gz report pre-compressed siblings (name.br, name.gz).
	Br bool `json:"br,omitempty"`
	Gz bool `json:"gz,omitempty"`
}

// BuildIndex walks root and hashes every regular file.
func BuildIndex(root string) (*Index, error) {
	idx := &Index{Files: map[string]File{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		idx.Files[filepath.ToSlash(rel)] = File{Size: n, Hash: hex.EncodeToString(h.Sum(nil))}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name, f := range idx.Files {
		_, f.Br = idx.Files[name+".br"]
		_, f.Gz = idx.Files[name+".gz"]
		idx.Files[name] = f
	}
	idx.init()
	return idx, nil
}

// ReadIndex loads an index written by WriteIndex.
func ReadIndex(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	if idx.Files == nil {
		idx.Files = map[string]File{}
	}
	idx.init()
	return &idx, nil
}

// WriteIndex saves the index as JSON.
func (idx *Index) WriteIndex(path string) error {
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// TotalSize is the size of all files.
func (idx *Index) TotalSize() int64 {
	var n int64
	for _, f := range idx.Files {
		n += f.Size
	}
	return n
}

func (idx *Index) init() {
	idx.dirs = map[string]bool{"": true}
	for name := range idx.Files {
		for {
			i := strings.LastIndexByte(name, '/')
			if i < 0 {
				break
			}
			name = name[:i]
			if idx.dirs[name] {
				break
			}
			idx.dirs[name] = true
		}
	}
}

func (idx *Index) file(name string) (File, bool) {
	f, ok := idx.Files[name]
	return f, ok
}

func (idx *Index) isDir(name string) bool {
	return idx.dirs[strings.TrimSuffix(name, "/")]
}
