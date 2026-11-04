package console

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// asset is a file of the console's, under a name that changes with its content: a
// browser may keep it for good, and never sees a stale one after an upgrade.
type asset struct {
	name        string // console.css
	fingerprint string // console.3f2a9c1b77.css
	content     []byte
	contentType string
}

func loadAssets(source fs.FS) (map[string]asset, error) {
	entries, err := fs.ReadDir(source, "assets")
	if err != nil {
		return nil, err
	}
	out := map[string]asset{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		content, err := fs.ReadFile(source, path.Join("assets", e.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		ext := path.Ext(e.Name())
		out[e.Name()] = asset{
			name:        e.Name(),
			fingerprint: strings.TrimSuffix(e.Name(), ext) + "." + hex.EncodeToString(sum[:5]) + ext,
			content:     content,
			contentType: mime.TypeByExtension(ext),
		}
	}
	return out, nil
}

// assetURL is the address of an asset. A missing one fails the page, in a test.
func (c *Console) assetURL(name string) string {
	if c.dev {
		return "/assets/" + name
	}
	a, ok := c.assets[name]
	if !ok {
		panic("the console has no asset " + name)
	}
	return "/assets/" + a.fingerprint
}

func (c *Console) serveAsset(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	if c.dev {
		content, err := fs.ReadFile(c.source, path.Join("assets", path.Base(file)))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h.Set("Content-Type", mime.TypeByExtension(path.Ext(file)))
		h.Set("Cache-Control", "no-store")
		w.Write(content)
		return
	}
	for _, a := range c.assets {
		if a.fingerprint == file {
			h.Set("Content-Type", a.contentType)
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Write(a.content)
			return
		}
	}
	http.NotFound(w, r)
}
