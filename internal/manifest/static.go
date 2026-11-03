package manifest

import (
	"fmt"
	"net/http"
	"path"
	"strings"
)

// Static is the [static] table: how a directory of files is served.
type Static struct {
	// Dir is the directory to serve, relative to the app.
	Dir string `toml:"dir" json:"dir,omitempty"`
	// SPA serves index.html for unknown paths (client-side routing).
	SPA bool `toml:"spa" json:"spa,omitempty"`
	// CleanURLs serves /about from about.html and redirects /about.html
	// to /about. Defaults to true.
	CleanURLs *bool `toml:"clean_urls" json:"clean_urls,omitempty"`
	// NotFound is a page served with status 404; defaults to 404.html when
	// the site has one.
	NotFound string `toml:"not_found" json:"not_found,omitempty"`
	// TrailingSlash is "auto" (directories end in a slash, pages do not),
	// "always" or "never".
	TrailingSlash string `toml:"trailing_slash" json:"trailing_slash,omitempty"`
	// Listing generates directory listings.
	Listing   bool         `toml:"listing" json:"listing,omitempty"`
	Headers   []HeaderRule `toml:"headers" json:"headers,omitempty"`
	Redirects []Redirect   `toml:"redirects" json:"redirects,omitempty"`
}

// HeaderRule sets response headers on paths matching For, where * matches
// any run of characters.
type HeaderRule struct {
	For    string            `toml:"for" json:"for"`
	Values map[string]string `toml:"values" json:"values"`
}

// Redirect redirects or rewrites requests matching From. A :name segment
// matches one path segment and a trailing * the rest of the path; To may
// use :name and :splat. Rules are tried in order and the first match wins;
// an existing file shadows a rule unless Force is set.
type Redirect struct {
	From   string `toml:"from" json:"from"`
	To     string `toml:"to" json:"to"`
	Status int    `toml:"status" json:"status,omitempty"`
	Force  bool   `toml:"force" json:"force,omitempty"`
}

// Defaults fills in the defaults of unset fields.
func (s *Static) Defaults() {
	if s.CleanURLs == nil {
		yes := true
		s.CleanURLs = &yes
	}
	if s.TrailingSlash == "" {
		s.TrailingSlash = "auto"
	}
	for i := range s.Redirects {
		if s.Redirects[i].Status == 0 {
			s.Redirects[i].Status = http.StatusMovedPermanently
		}
	}
}

// Validate checks the rules, reporting every problem.
func (s *Static) Validate() error {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	if s.Dir != "" && (path.IsAbs(s.Dir) || strings.HasPrefix(path.Clean(strings.ReplaceAll(s.Dir, `\`, "/")), "..")) {
		add("static.dir %q must be a directory inside the app", s.Dir)
	}
	switch s.TrailingSlash {
	case "", "auto", "always", "never":
	default:
		add("static.trailing_slash %q must be \"auto\", \"always\" or \"never\"", s.TrailingSlash)
	}
	if s.NotFound != "" && (strings.HasPrefix(s.NotFound, "/") || strings.Contains(s.NotFound, "..")) {
		add("static.not_found %q must be a file path relative to the site, such as 404.html", s.NotFound)
	}
	for i, h := range s.Headers {
		if !strings.HasPrefix(h.For, "/") && h.For != "*" {
			add("static.headers[%d].for %q must start with / (or be *)", i, h.For)
		}
		if len(h.Values) == 0 {
			add("static.headers[%d] sets no values", i)
		}
		for name := range h.Values {
			if name == "" || strings.ContainsAny(name, " :\r\n") {
				add("static.headers[%d] has an invalid header name %q", i, name)
			}
		}
	}
	for i, r := range s.Redirects {
		if !strings.HasPrefix(r.From, "/") {
			add("static.redirects[%d].from %q must start with /", i, r.From)
		} else if star := strings.Index(r.From, "*"); star >= 0 && star != len(r.From)-1 {
			add("static.redirects[%d].from %q may use * only at the end", i, r.From)
		}
		if r.To == "" {
			add("static.redirects[%d].to is required", i)
		}
		switch r.Status {
		case 0, 200, 301, 302, 303, 307, 308:
		default:
			add("static.redirects[%d].status %d must be 301, 302, 303, 307, 308, or 200 for a rewrite", i, r.Status)
		}
		if r.Status == 200 && !strings.HasPrefix(r.To, "/") {
			add("static.redirects[%d]: a rewrite (status 200) must point at a path of the site, not %q", i, r.To)
		}
	}
	if len(p) > 0 {
		return fmt.Errorf("invalid [static] settings:\n  - %s", strings.Join(p, "\n  - "))
	}
	return nil
}
