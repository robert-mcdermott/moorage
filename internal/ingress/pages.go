package ingress

import (
	"html/template"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/brand"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
)

// page is a small platform page (spec/06-ingress.md, "Platform pages").
type page struct {
	Status  int
	Title   string
	Heading string
	Message string
	Detail  string
	// Refresh, if set, is how many seconds until the page reloads itself.
	Refresh int
	// Password asks for a password, sent to PasswordTo, to go on to Return.
	Password   bool
	PasswordTo string
	Return     string
	// Error says what was wrong with what was given.
	Error string
	// Confirm is a form with a button: what the button says, and where it goes.
	Confirm, ConfirmTo string
}

func (p page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(p.Status)
	if r.Method == http.MethodHead {
		return
	}
	_ = pageTemplate.Execute(w, struct {
		page
		Version string
		Icon    template.HTML
		Favicon template.URL
	}{p, buildinfo.Version, brand.Icon(), brand.FaviconURL()})
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

func accessDenied(name string) page {
	return page{
		Status:  http.StatusForbidden,
		Title:   "Access denied",
		Heading: "You cannot reach “" + name + "” from here",
		Message: "Whoever looks after it can let you in.",
	}
}

func passwordPage(rte route, back, failure string) page {
	p := page{
		Status:     http.StatusUnauthorized,
		Title:      rte.app + " asks for a password",
		Heading:    "“" + rte.app + "” asks for a password",
		Message:    "Whoever shared it with you will have it.",
		Password:   true,
		PasswordTo: rte.prefix + "/.moorage/gate/password",
		Return:     safeReturn(rte.prefix, back),
		Error:      failure,
	}
	if rte.gate.password == "" {
		p.Status, p.Password = http.StatusServiceUnavailable, false
		p.Message = "It has no password yet, so nobody can reach it until a maintainer sets one."
	}
	return p
}

func shareNeeded(name string) page {
	return page{
		Status:  http.StatusForbidden,
		Title:   name + " needs a link",
		Heading: "“" + name + "” needs a link",
		Message: "It can only be reached with a share link, which someone working on it can make.",
	}
}

func shareEnded(name string) page {
	return page{
		Status:  http.StatusGone,
		Title:   "This link has ended",
		Heading: "This link to “" + name + "” has ended",
		Message: "It may have expired, been used up or been revoked. Whoever gave it to you can make another.",
	}
}

func shareConfirm(name, to string) page {
	return page{
		Status:    http.StatusOK,
		Title:     name,
		Heading:   "Someone has shared “" + name + "” with you",
		Message:   "This link can only be used a limited number of times.",
		Confirm:   "Open it",
		ConfirmTo: to,
	}
}

func wakingUp(name string) page {
	return page{
		Status:  http.StatusServiceUnavailable,
		Title:   name + " is waking up",
		Heading: "“" + name + "” is waking up",
		Message: "Nothing has asked for it in a while, so it went to sleep. It is starting now, and this page will load by itself when it is ready.",
		Refresh: 2,
	}
}

func unavailable(name string) page {
	return page{
		Status:  http.StatusServiceUnavailable,
		Title:   name + " is unavailable",
		Heading: "“" + name + "” is not available right now",
		Message: "No instance of the app is ready to answer. It may be starting, restarting or failing; try again in a moment.",
	}
}

func noWebProcess(name string) page {
	return page{
		Status:  http.StatusNotFound,
		Title:   name + " has no web process",
		Heading: "“" + name + "” does not serve web pages",
		Message: "The app runs, but it has no web process to answer requests.",
	}
}

var notFound = page{
	Status:  http.StatusNotFound,
	Title:   "Not found",
	Heading: "Not found",
	Message: "There is nothing at this address.",
}

// running answers on the console's hostname until the server sets its console.
var running = page{
	Status:  http.StatusOK,
	Title:   "Moorage",
	Heading: "Moorage is running",
	Message: "The server is starting.",
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
{{- if .Refresh}}
<meta http-equiv="refresh" content="{{.Refresh}}">
{{- end}}
<title>{{.Title}}</title>
<link rel="icon" href="{{.Favicon}}">
<style>
:root { color-scheme: light dark; --bg: #f7f7f5; --fg: #1d1f24; --muted: #5e6470; --brand: #08264c; }
@media (prefers-color-scheme: dark) { :root { --bg: #15171c; --fg: #e8e9ec; --muted: #9aa0ab; --brand: #e6ebf2; } }
html, body { height: 100%; margin: 0; }
body { background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
       display: flex; align-items: center; justify-content: center; padding: 0 1rem; }
main { max-width: 34rem; }
h1 { font-size: 1.5rem; margin: 0 0 .5rem; }
p { margin: 0 0 1rem; color: var(--muted); }
form { margin: 0 0 1.5rem; }
label { display: block; font-weight: 600; margin-bottom: .25rem; }
input[type=password] { display: block; width: 100%; box-sizing: border-box; font: inherit; padding: .5rem .6rem; margin-bottom: .75rem;
  color: inherit; background: transparent; border: 1px solid color-mix(in srgb, var(--muted) 60%, transparent); border-radius: 6px; }
button { font: inherit; font-weight: 600; padding: .5rem 1rem; color: var(--bg); background: var(--brand); border: 0; border-radius: 6px; cursor: pointer; }
.error { color: #b3261e; font-weight: 600; }
@media (prefers-color-scheme: dark) { .error { color: #f2b8b5; } }
footer { display: flex; align-items: center; gap: .4rem; font-size: .8rem; color: var(--muted);
         border-top: 1px solid color-mix(in srgb, var(--muted) 30%, transparent); padding-top: .75rem; }
footer span, footer svg { display: block; width: 1.25rem; height: 1.25rem; }
footer strong { color: var(--brand); }
</style>
</head>
<body>
<main>
<h1>{{.Heading}}</h1>
<p>{{.Message}}</p>
{{- if .Detail}}
<p>{{.Detail}}</p>
{{- end}}
{{- if .Error}}
<p class="error" role="alert">{{.Error}}</p>
{{- end}}
{{- if .Password}}
<form method="post" action="{{.PasswordTo}}">
<input type="hidden" name="return" value="{{.Return}}">
<label for="password">Password</label>
<input id="password" type="password" name="password" required autofocus autocomplete="current-password">
<button type="submit">Go on</button>
</form>
{{- end}}
{{- if .Confirm}}
<form method="post" action="{{.ConfirmTo}}">
<button type="submit">{{.Confirm}}</button>
</form>
{{- end}}
<footer><span aria-hidden="true">{{.Icon}}</span><strong>Moorage</strong> {{.Version}}</footer>
</main>
</body>
</html>
`))
