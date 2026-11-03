package static

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/manifest"
)

var platform404 = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "platform 404", http.StatusNotFound)
})

func site(t *testing.T, files map[string]string, rules manifest.Static) *Site {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	// The index survives a round trip through its file.
	path := filepath.Join(t.TempDir(), "index.json")
	if err := idx.WriteIndex(path); err != nil {
		t.Fatal(err)
	}
	if idx, err = ReadIndex(path); err != nil {
		t.Fatal(err)
	}
	return NewSite(root, idx, rules, platform404)
}

type response struct {
	code   int
	header http.Header
	body   string
}

func get(s *Site, target string, headers ...string) response {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return response{rec.Code, rec.Result().Header, string(body)}
}

var basic = map[string]string{
	"index.html":             "home",
	"about.html":             "about",
	"docs/index.html":        "docs home",
	"docs/guide.html":        "guide",
	"assets/app.3f2a9c1d.js": "console.log('hashed')",
	"assets/plain.js":        "console.log('plain')",
	"style.css":              "body{}",
	"style.css.br":           "BROTLI",
	"style.css.gz":           "GZIP",
	"data.bin":               "\x00\x01binary",
	"files/report.pdf":       "%PDF-1.7",
	"files/sub/notes.txt":    "notes",
}

func TestCleanURLs(t *testing.T) {
	s := site(t, basic, manifest.Static{})
	cases := []struct {
		path, wantBody, wantLocation string
		wantCode                     int
	}{
		{"/", "home", "", 200},
		{"/about", "about", "", 200},
		{"/about.html", "", "/about", 301},
		{"/index.html", "", "/", 301},
		{"/docs", "", "/docs/", 301},
		{"/docs/", "docs home", "", 200},
		{"/docs/index.html", "", "/docs/", 301},
		{"/docs/guide", "guide", "", 200},
		{"/about/", "", "/about", 301},
		{"/style.css/", "", "/style.css", 301},
		{"/a//b/../about", "", "/a/about", 301},
	}
	for _, c := range cases {
		res := get(s, c.path)
		if res.code != c.wantCode {
			t.Errorf("%s: status %d, want %d", c.path, res.code, c.wantCode)
			continue
		}
		if c.wantBody != "" && res.body != c.wantBody {
			t.Errorf("%s: body %q, want %q", c.path, res.body, c.wantBody)
		}
		if c.wantLocation != "" && res.header.Get("Location") != c.wantLocation {
			t.Errorf("%s: Location %q, want %q", c.path, res.header.Get("Location"), c.wantLocation)
		}
	}
	if res := get(s, "/about.html?x=1"); res.header.Get("Location") != "/about?x=1" {
		t.Errorf("query string not kept: %q", res.header.Get("Location"))
	}
}

func TestCleanURLsOff(t *testing.T) {
	no := false
	s := site(t, basic, manifest.Static{CleanURLs: &no})
	if res := get(s, "/about.html"); res.code != 200 || res.body != "about" {
		t.Errorf("/about.html with clean URLs off: %d %q", res.code, res.body)
	}
	if res := get(s, "/about"); res.code != 404 {
		t.Errorf("/about with clean URLs off: %d", res.code)
	}
}

func TestTrailingSlash(t *testing.T) {
	never := site(t, basic, manifest.Static{TrailingSlash: "never"})
	if res := get(never, "/docs"); res.code != 200 || res.body != "docs home" {
		t.Errorf("never: /docs = %d %q", res.code, res.body)
	}
	if res := get(never, "/docs/"); res.code != 301 || res.header.Get("Location") != "/docs" {
		t.Errorf("never: /docs/ = %d %q", res.code, res.header.Get("Location"))
	}
	always := site(t, basic, manifest.Static{TrailingSlash: "always"})
	if res := get(always, "/about"); res.code != 301 || res.header.Get("Location") != "/about/" {
		t.Errorf("always: /about = %d %q", res.code, res.header.Get("Location"))
	}
	if res := get(always, "/about/"); res.code != 200 || res.body != "about" {
		t.Errorf("always: /about/ = %d %q", res.code, res.body)
	}
	if res := get(always, "/about.html"); res.header.Get("Location") != "/about/" {
		t.Errorf("always: /about.html redirects to %q", res.header.Get("Location"))
	}
}

func TestCachingAndTypes(t *testing.T) {
	s := site(t, basic, manifest.Static{})
	res := get(s, "/assets/app.3f2a9c1d.js")
	if res.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("hashed asset Cache-Control = %q", res.header.Get("Cache-Control"))
	}
	if res.header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", res.header.Get("Content-Type"))
	}
	if res := get(s, "/assets/plain.js"); res.header.Get("Cache-Control") != "no-cache" {
		t.Errorf("plain asset Cache-Control = %q", res.header.Get("Cache-Control"))
	}
	if res := get(s, "/data.bin"); res.header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("sniffed Content-Type = %q", res.header.Get("Content-Type"))
	}
	if res := get(s, "/files/report.pdf"); res.header.Get("Content-Type") != "application/pdf" {
		t.Errorf("pdf Content-Type = %q", res.header.Get("Content-Type"))
	}
	if res.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
}

func TestConditionalAndRange(t *testing.T) {
	s := site(t, basic, manifest.Static{})
	first := get(s, "/about")
	etag := first.header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if res := get(s, "/about", "If-None-Match", etag); res.code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", res.code)
	}
	if res := get(s, "/about", "Range", "bytes=1-2"); res.code != http.StatusPartialContent || res.body != "bo" {
		t.Errorf("Range: %d %q", res.code, res.body)
	}
}

func TestPrecompressed(t *testing.T) {
	s := site(t, basic, manifest.Static{})
	br := get(s, "/style.css", "Accept-Encoding", "gzip, deflate, br")
	if br.body != "BROTLI" || br.header.Get("Content-Encoding") != "br" || br.header.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("br: %q %q %q", br.body, br.header.Get("Content-Encoding"), br.header.Get("Content-Type"))
	}
	if !strings.Contains(br.header.Get("Vary"), "Accept-Encoding") {
		t.Error("Vary missing")
	}
	gz := get(s, "/style.css", "Accept-Encoding", "gzip, br;q=0")
	if gz.body != "GZIP" || gz.header.Get("Content-Encoding") != "gzip" {
		t.Errorf("gzip: %q %q", gz.body, gz.header.Get("Content-Encoding"))
	}
	if gz.header.Get("ETag") == br.header.Get("ETag") {
		t.Error("encodings share an ETag")
	}
	plain := get(s, "/style.css")
	if plain.body != "body{}" || plain.header.Get("Content-Encoding") != "" {
		t.Errorf("identity: %q %q", plain.body, plain.header.Get("Content-Encoding"))
	}
}

func TestNotFound(t *testing.T) {
	s := site(t, basic, manifest.Static{})
	if res := get(s, "/missing"); res.code != 404 || !strings.Contains(res.body, "platform 404") {
		t.Errorf("without a 404 page: %d %q", res.code, res.body)
	}
	with404 := map[string]string{"index.html": "home", "404.html": "custom not found"}
	s = site(t, with404, manifest.Static{})
	if res := get(s, "/missing"); res.code != 404 || res.body != "custom not found" {
		t.Errorf("with 404.html: %d %q", res.code, res.body)
	}
}

func TestSPA(t *testing.T) {
	s := site(t, basic, manifest.Static{SPA: true})
	if res := get(s, "/app/settings/profile"); res.code != 200 || res.body != "home" {
		t.Errorf("SPA route: %d %q", res.code, res.body)
	}
	if res := get(s, "/missing.png"); res.code != 404 {
		t.Errorf("SPA asset miss: %d", res.code)
	}
	if res := get(s, "/missing.png", "Accept", "text/html"); res.code != 200 {
		t.Errorf("SPA page request with an extension: %d", res.code)
	}
	if res := get(s, "/about"); res.body != "about" {
		t.Errorf("SPA must still serve real pages: %q", res.body)
	}
}

func TestRedirectRules(t *testing.T) {
	rules := manifest.Static{Redirects: []manifest.Redirect{
		{From: "/blog/:year/*", To: "/posts/:year/:splat"},
		{From: "/old", To: "/about", Status: 302},
		{From: "/about", To: "/elsewhere"},
		{From: "/guide/*", To: "/docs/:splat", Status: 200},
		{From: "/external", To: "https://example.com/x", Status: 308},
		{From: "/style.css", To: "/forced", Force: true},
	}}
	s := site(t, basic, rules)
	cases := []struct {
		path, location string
		code           int
		body           string
	}{
		{"/blog/2024/hello/world", "/posts/2024/hello/world", 301, ""},
		{"/blog/2024", "/posts/2024/", 301, ""},
		{"/old?ref=mail", "/about?ref=mail", 302, ""},
		{"/about", "", 200, "about"}, // the file shadows the rule
		{"/guide/guide", "", 200, "guide"},
		{"/guide/nothing", "", 404, ""},
		{"/external", "https://example.com/x", 308, ""},
		{"/style.css", "/forced", 301, ""},
	}
	for _, c := range cases {
		res := get(s, c.path)
		if res.code != c.code || (c.location != "" && res.header.Get("Location") != c.location) || (c.body != "" && res.body != c.body) {
			t.Errorf("%s: %d Location %q body %q; want %d %q %q", c.path, res.code, res.header.Get("Location"), res.body, c.code, c.location, c.body)
		}
	}
}

func TestHeaderRules(t *testing.T) {
	rules := manifest.Static{Headers: []manifest.HeaderRule{
		{For: "/assets/*", Values: map[string]string{"X-Asset": "yes", "Cache-Control": "max-age=60"}},
		{For: "/*", Values: map[string]string{"X-Frame-Options": "DENY"}},
	}}
	s := site(t, basic, rules)
	res := get(s, "/assets/plain.js")
	if res.header.Get("X-Asset") != "yes" || res.header.Get("Cache-Control") != "max-age=60" || res.header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("asset headers: %v", res.header)
	}
	if res := get(s, "/about"); res.header.Get("X-Asset") != "" || res.header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("page headers: %v", res.header)
	}
}

func TestListing(t *testing.T) {
	s := site(t, basic, manifest.Static{Listing: true})
	res := get(s, "/files/")
	if res.code != 200 || !strings.Contains(res.body, `href="report.pdf"`) || !strings.Contains(res.body, `href="sub/"`) || !strings.Contains(res.body, `href="../"`) {
		t.Errorf("listing: %d\n%s", res.code, res.body)
	}
	if res := get(site(t, basic, manifest.Static{}), "/files/"); res.code != 404 {
		t.Errorf("listing off: %d", res.code)
	}
}

func TestMethods(t *testing.T) {
	s := site(t, basic, manifest.Static{})
	req := httptest.NewRequest(http.MethodPost, "/about", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: %d %q", rec.Code, rec.Header().Get("Allow"))
	}
	head := httptest.NewRequest(http.MethodHead, "/about", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, head)
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestIsHashed(t *testing.T) {
	for name, want := range map[string]bool{
		"app.3f2a9c1d.js":             true,
		"index-BdU2kXq9.js":           true,
		"main.4e1a2b3c4d5e6f.css":     true,
		"chunk-vendors.a1b2c3d4e5.js": true,
		"jquery-standard.min.js":      false,
		"bootstrap5.min.css":          false,
		"lib-abcdefgh.js":             false,
		"photo-20240101.jpg":          false,
		"index.html":                  false,
	} {
		if got := isHashed(name); got != want {
			t.Errorf("isHashed(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"/assets/*", "/assets/a/b.js", true},
		{"/assets/*", "/other/a.js", false},
		{"/*.js", "/deep/x.js", true},
		{"/*.js", "/x.css", false},
		{"*", "/anything", true},
		{"/exact", "/exact", true},
		{"/exact", "/exact/", false},
		{"/a/*/c", "/a/b/c", true},
	} {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.pattern, c.s, got)
		}
	}
}
