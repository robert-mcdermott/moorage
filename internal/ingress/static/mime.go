package static

import (
	"path"
	"strings"
)

// mimeTypes is Moorage's own table of content types, so that a site is
// served the same way on every operating system (the standard library would
// consult the host's MIME database). Unknown types are sniffed.
var mimeTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".cjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".xml":         "application/xml",
	".rss":         "application/rss+xml",
	".atom":        "application/atom+xml",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
	".csv":         "text/csv; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".bmp":         "image/bmp",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".eot":         "application/vnd.ms-fontobject",
	".wasm":        "application/wasm",
	".pdf":         "application/pdf",
	".zip":         "application/zip",
	".gz":          "application/gzip",
	".tar":         "application/x-tar",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
	".mp3":         "audio/mpeg",
	".ogg":         "audio/ogg",
	".wav":         "audio/wav",
	".yaml":        "application/yaml",
	".yml":         "application/yaml",
	".toml":        "application/toml",
}

func contentType(name string) string {
	return mimeTypes[strings.ToLower(path.Ext(name))]
}

func isHTML(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".html" || ext == ".htm"
}

// isHashed reports whether a file name carries a content hash, such as
// app.3f2a9c1d.js or index-BdU2kXq9.js, so that it can be cached forever.
// The test is conservative: a hex run of 8+ characters with a digit and a
// letter, or a run of 8+ characters mixing upper case, lower case and digits,
// between a separator (. or -) and a dot.
func isHashed(name string) bool {
	base := path.Base(name)
	for i := 0; i < len(base); i++ {
		if base[i] != '.' && base[i] != '-' {
			continue
		}
		j := i + 1
		for j < len(base) && isWordChar(base[j]) {
			j++
		}
		if j < len(base) && base[j] == '.' && j-i-1 >= 8 && looksLikeHash(base[i+1:j]) {
			return true
		}
	}
	return false
}

func isWordChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func looksLikeHash(s string) bool {
	var lower, upper, digit, nonHex bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'z':
			lower = true
			if c > 'f' {
				nonHex = true
			}
		case c >= 'A' && c <= 'Z':
			upper = true
		default:
			nonHex = true
		}
	}
	if !upper && !nonHex && digit && lower {
		return true // lowercase hex
	}
	return upper && lower && digit
}
