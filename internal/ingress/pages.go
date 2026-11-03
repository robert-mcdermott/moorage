package ingress

import (
	"html/template"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
)

// page is a small platform page (docs/06-ingress.md, "Platform pages").
type page struct {
	Status  int
	Title   string
	Heading string
	Message string
	Detail  string
}

func (p page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(p.Status)
	if r.Method == http.MethodHead {
		return
	}
	_ = pageTemplate.Execute(w, struct {
		page
		Version string
	}{p, buildinfo.Version})
}

func noSuchApp(name string) page {
	return page{
		Status:  http.StatusNotFound,
		Title:   "No app named " + name,
		Heading: "There is no app named “" + name + "”",
		Message: "Nothing is deployed at this address. Check the spelling of the link, or ask whoever shared it.",
	}
}

func nothingDeployed(name string) page {
	return page{
		Status:  http.StatusNotFound,
		Title:   name + " has not been deployed yet",
		Heading: "“" + name + "” has not been deployed yet",
		Message: "The app exists, but nothing has been deployed to it so far.",
	}
}

var notFound = page{
	Status:  http.StatusNotFound,
	Title:   "Not found",
	Heading: "Not found",
	Message: "There is nothing at this address.",
}

// Placeholder answers on the console's hostname until the console arrives.
func Placeholder() http.Handler { return running }

var running = page{
	Status:  http.StatusOK,
	Title:   "Moorage",
	Heading: "Moorage is running",
	Message: "The web console arrives in a later release. Deploy with the moor command line in the meantime.",
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --bg: #f7f7f5; --fg: #1d1f24; --muted: #5e6470; --accent: #2a6f97; }
@media (prefers-color-scheme: dark) { :root { --bg: #15171c; --fg: #e8e9ec; --muted: #9aa0ab; --accent: #6fb3d9; } }
html, body { height: 100%; margin: 0; }
body { background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
       display: flex; align-items: center; justify-content: center; padding: 0 1rem; }
main { max-width: 34rem; }
h1 { font-size: 1.5rem; margin: 0 0 .5rem; }
p { margin: 0 0 1rem; color: var(--muted); }
footer { font-size: .8rem; color: var(--muted); border-top: 1px solid color-mix(in srgb, var(--muted) 30%, transparent); padding-top: .75rem; }
footer span { color: var(--accent); font-weight: 600; }
</style>
</head>
<body>
<main>
<h1>{{.Heading}}</h1>
<p>{{.Message}}</p>
{{- if .Detail}}
<p>{{.Detail}}</p>
{{- end}}
<footer><span>Moorage</span> {{.Version}}</footer>
</main>
</body>
</html>
`))
