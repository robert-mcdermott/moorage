package static

import (
	"html/template"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/manifest"
)

// Site serves one published directory according to its [static] rules.
type Site struct {
	root      string
	idx       *Index
	rules     manifest.Static
	redirects []redirectRule
	// notFound answers when the site has no page of its own for a request.
	notFound http.Handler
}

// NewSite serves the files of root described by idx. notFound handles
// requests the site has no page for.
func NewSite(root string, idx *Index, rules manifest.Static, notFound http.Handler) *Site {
	rules.Defaults()
	if rules.NotFound == "" {
		if _, ok := idx.file("404.html"); ok {
			rules.NotFound = "404.html"
		}
	}
	s := &Site{root: root, idx: idx, rules: rules, notFound: notFound}
	for _, r := range rules.Redirects {
		s.redirects = append(s.redirects, compileRedirect(r))
	}
	return s
}

// resolution is what a request path maps to: a file to serve, a canonical
// URL to redirect to, a directory to list, or nothing.
type resolution struct {
	file     string
	redirect string
	listing  bool
	dir      string
}

func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	// Nothing has a name like this, and redirecting to it would send a browser elsewhere: it reads a backslash as a slash and
	// leaves out tabs and newlines, so that /\\evil.example and /<tab>/evil.example are both another site.
	if strings.ContainsFunc(p, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f }) {
		s.serveNotFound(w, r, "/")
		return
	}
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	if clean != p {
		s.redirect(w, r, clean, http.StatusMovedPermanently)
		return
	}
	if s.applyRedirects(w, r, clean) {
		return
	}
	if s.hides(clean) {
		s.serveNotFound(w, r, clean)
		return
	}
	res := s.resolve(clean)
	switch {
	case res.redirect != "":
		s.redirect(w, r, res.redirect, http.StatusMovedPermanently)
	case res.file != "":
		s.serveFile(w, r, clean, res.file, http.StatusOK)
	case res.listing:
		s.serveListing(w, r, res.dir)
	default:
		s.serveNotFound(w, r, clean)
	}
}

// hides says whether a path goes through a dotfile, which is left behind by accident far more often than meant to be served:
// .env, .git, .DS_Store. What is in .well-known is meant to be found.
func (s *Site) hides(clean string) bool {
	if s.rules.Dotfiles {
		return false
	}
	for _, part := range strings.Split(strings.Trim(clean, "/"), "/") {
		if strings.HasPrefix(part, ".") && part != ".well-known" {
			return true
		}
	}
	return false
}

// resolve maps a clean request path to a file, following the clean URL and
// trailing slash rules.
func (s *Site) resolve(clean string) resolution {
	slash := strings.HasSuffix(clean, "/")
	name := strings.Trim(clean, "/")
	cleanURLs := *s.rules.CleanURLs
	trailing := s.rules.TrailingSlash
	exists := func(n string) bool { _, ok := s.idx.file(n); return ok }
	index := path.Join(name, "index.html")

	if slash || name == "" {
		switch {
		case exists(index):
			if trailing == "never" && name != "" {
				return resolution{redirect: "/" + name}
			}
			return resolution{file: index}
		case name != "" && cleanURLs && exists(name+".html"):
			if trailing == "always" {
				return resolution{file: name + ".html"}
			}
			return resolution{redirect: "/" + name}
		case s.rules.Listing && s.idx.isDir(name):
			return resolution{listing: true, dir: name}
		case name != "" && exists(name):
			return resolution{redirect: "/" + name}
		}
		return resolution{}
	}

	if exists(name) {
		if cleanURLs && isHTML(name) {
			return resolution{redirect: s.canonical(name)}
		}
		return resolution{file: name}
	}
	if cleanURLs && exists(name+".html") {
		if trailing == "always" {
			return resolution{redirect: "/" + name + "/"}
		}
		return resolution{file: name + ".html"}
	}
	if s.idx.isDir(name) {
		if trailing != "never" {
			return resolution{redirect: "/" + name + "/"}
		}
		if exists(index) {
			return resolution{file: index}
		}
		if s.rules.Listing {
			return resolution{listing: true, dir: name}
		}
	}
	return resolution{}
}

// canonical returns the clean URL of an HTML file: its directory for an
// index page, its name without the extension otherwise.
func (s *Site) canonical(name string) string {
	base := path.Base(name)
	stem := strings.TrimSuffix(name, path.Ext(name))
	if strings.TrimSuffix(base, path.Ext(base)) == "index" {
		dir := path.Dir(name)
		if dir == "." {
			return "/"
		}
		if s.rules.TrailingSlash == "never" {
			return "/" + dir
		}
		return "/" + dir + "/"
	}
	if s.rules.TrailingSlash == "always" {
		return "/" + stem + "/"
	}
	return "/" + stem
}

func (s *Site) redirect(w http.ResponseWriter, r *http.Request, target string, status int) {
	if r.URL.RawQuery != "" && !strings.Contains(target, "?") {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, status)
}

func (s *Site) applyRedirects(w http.ResponseWriter, r *http.Request, clean string) bool {
	for _, rule := range s.redirects {
		target, ok := rule.match(clean)
		if !ok {
			continue
		}
		if !rule.Force && s.resolve(clean).file != "" {
			continue // an existing file shadows the rule
		}
		if rule.Status == http.StatusOK {
			p, _, _ := strings.Cut(target, "?")
			p = path.Clean(p)
			// What a rule captured comes from the request, so that /pages/.env is not a way to reach what /.env is not.
			if res := s.resolve(p); res.file != "" && !s.hides(p) {
				s.serveFile(w, r, clean, res.file, http.StatusOK)
			} else {
				s.serveNotFound(w, r, clean)
			}
			return true
		}
		s.redirect(w, r, target, rule.Status)
		return true
	}
	return false
}

func (s *Site) serveNotFound(w http.ResponseWriter, r *http.Request, clean string) {
	if s.rules.SPA && wantsPage(r, clean) {
		if _, ok := s.idx.file("index.html"); ok {
			s.serveFile(w, r, clean, "index.html", http.StatusOK)
			return
		}
	}
	if s.rules.NotFound != "" {
		if _, ok := s.idx.file(s.rules.NotFound); ok {
			s.serveFile(w, r, clean, s.rules.NotFound, http.StatusNotFound)
			return
		}
	}
	s.notFound.ServeHTTP(w, r)
}

// wantsPage reports whether a request is for a page rather than an asset:
// it accepts HTML, or its path has no extension.
func wantsPage(r *http.Request, clean string) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html") || path.Ext(clean) == ""
}

func (s *Site) serveFile(w http.ResponseWriter, r *http.Request, reqPath, name string, status int) {
	f := s.idx.Files[name]
	h := w.Header()
	if isHashed(name) {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	for _, rule := range s.rules.Headers {
		if globMatch(rule.For, reqPath) {
			for k, v := range rule.Values {
				h.Set(k, v)
			}
		}
	}
	ctype := contentType(name)
	if ctype == "" {
		ctype = s.sniff(name)
	}
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")

	open, etag := name, f.Hash
	if len(etag) > 32 {
		etag = etag[:32]
	}
	if f.Br || f.Gz {
		h.Add("Vary", "Accept-Encoding")
		if enc, ext := chooseEncoding(r.Header.Get("Accept-Encoding"), f); enc != "" {
			open = name + ext
			etag += "-" + enc
			h.Set("Content-Encoding", enc)
		}
	}
	file, err := os.Open(filepath.Join(s.root, filepath.FromSlash(open)))
	if err != nil {
		http.Error(w, "The file could not be read.", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	if status != http.StatusOK {
		h.Set("Content-Length", strconv.FormatInt(s.idx.Files[open].Size, 10))
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, file)
		}
		return
	}
	h.Set("ETag", `"`+etag+`"`)
	http.ServeContent(w, r, "", time.Time{}, file)
}

// sniff detects the content type of a file whose extension is unknown.
func (s *Site) sniff(name string) string {
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(name)))
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	var buf [512]byte
	n, _ := io.ReadFull(f, buf[:])
	return http.DetectContentType(buf[:n])
}

// chooseEncoding picks a pre-compressed variant the client accepts:
// Brotli before gzip.
func chooseEncoding(accept string, f File) (encoding, ext string) {
	ok := map[string]bool{}
	for _, part := range strings.Split(accept, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		if q == "q=0" || q == "q=0.0" || q == "q=0.00" || q == "q=0.000" {
			continue
		}
		ok[strings.ToLower(strings.TrimSpace(token))] = true
	}
	switch {
	case f.Br && ok["br"]:
		return "br", ".br"
	case f.Gz && (ok["gzip"] || ok["x-gzip"]):
		return "gzip", ".gz"
	}
	return "", ""
}

// globMatch reports whether s matches pattern, where * matches any run of
// characters, including slashes.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// redirectRule is a compiled [[static.redirects]] entry.
type redirectRule struct {
	manifest.Redirect
	segments []string
	splat    bool
}

func compileRedirect(r manifest.Redirect) redirectRule {
	from := r.From
	rule := redirectRule{Redirect: r}
	if strings.HasSuffix(from, "*") {
		rule.splat = true
		from = strings.TrimSuffix(from, "*")
	}
	if trimmed := strings.Trim(from, "/"); trimmed != "" {
		rule.segments = strings.Split(trimmed, "/")
	}
	return rule
}

// match reports whether clean matches the rule and, if so, returns the
// target with placeholders filled in.
func (rule redirectRule) match(clean string) (string, bool) {
	var segs []string
	if trimmed := strings.Trim(clean, "/"); trimmed != "" {
		segs = strings.Split(trimmed, "/")
	}
	if len(segs) < len(rule.segments) || (!rule.splat && len(segs) != len(rule.segments)) {
		return "", false
	}
	captures := map[string]string{}
	for i, want := range rule.segments {
		if strings.HasPrefix(want, ":") && len(want) > 1 {
			captures[want[1:]] = segs[i]
		} else if want != segs[i] {
			return "", false
		}
	}
	target := rule.To
	if rule.splat {
		target = strings.ReplaceAll(target, ":splat", strings.Join(segs[len(rule.segments):], "/"))
	}
	// Longer names first, so that :id does not clobber part of :identifier.
	keys := make([]string, 0, len(captures))
	for k := range captures {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		target = strings.ReplaceAll(target, ":"+k, captures[k])
	}
	return target, true
}

type listingEntry struct {
	Name string
	Href string
	Size string
	Dir  bool
}

func (s *Site) serveListing(w http.ResponseWriter, r *http.Request, dir string) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]bool{}
	var entries []listingEntry
	for name, f := range s.idx.Files {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok || s.hides("/"+name) {
			continue
		}
		if sub, _, isDir := strings.Cut(rest, "/"); isDir {
			if !seen[sub] {
				seen[sub] = true
				entries = append(entries, listingEntry{Name: sub + "/", Href: sub + "/", Dir: true})
			}
			continue
		}
		entries = append(entries, listingEntry{Name: rest, Href: rest, Size: humanSize(f.Size)})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return entries[i].Name < entries[j].Name
	})
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	_ = listingTemplate.Execute(w, struct {
		Path    string
		Parent  bool
		Entries []listingEntry
	}{"/" + prefix, dir != "", entries})
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

var listingTemplate = template.Must(template.New("listing").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Index of {{.Path}}</title>
<style>
:root { color-scheme: light dark; font-family: system-ui, sans-serif; }
body { max-width: 56rem; margin: 2rem auto; padding: 0 1rem; }
table { width: 100%; border-collapse: collapse; }
td { padding: .35rem .5rem; border-bottom: 1px solid color-mix(in srgb, currentColor 15%, transparent); }
td.size { text-align: right; white-space: nowrap; opacity: .7; }
a { text-decoration: none; }
</style>
</head>
<body>
<h1>Index of {{.Path}}</h1>
<table>
{{- if .Parent}}
<tr><td><a href="../">../</a></td><td class="size"></td></tr>
{{- end}}
{{- range .Entries}}
<tr><td><a href="{{.Href}}">{{.Name}}</a></td><td class="size">{{.Size}}</td></tr>
{{- end}}
</table>
</body>
</html>
`))
