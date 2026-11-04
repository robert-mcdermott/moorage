package main

import (
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

type app struct {
	store   *store
	logger  *slog.Logger
	page    *template.Template
	static  http.Handler
	release string
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("POST /links", a.add)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /static/", http.StripPrefix("/static", a.static))
	mux.HandleFunc("GET /{code}", a.visit)
	return mux
}

// pageData is what the page shows.
type pageData struct {
	Base    string // the short addresses' prefix, such as https://links.apps.example.com
	Links   []link
	Created string
	Error   string
	URL     string // the form's values, kept when they are refused
	Code    string
	Release string
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, pageData{Created: r.URL.Query().Get("created")})
}

func (a *app) add(w http.ResponseWriter, r *http.Request) {
	target, code := strings.TrimSpace(r.FormValue("url")), strings.TrimSpace(r.FormValue("code"))
	l, err := a.store.Add(target, code)
	switch {
	case errors.Is(err, errInvalid), errors.Is(err, errBadCode), errors.Is(err, errTaken):
		a.render(w, r, http.StatusBadRequest, pageData{Error: err.Error(), URL: target, Code: code})
		return
	case err != nil:
		a.logger.Error("adding a link", "err", err)
		http.Error(w, "the link could not be saved", http.StatusInternalServerError)
		return
	}
	a.logger.Info("link added", "code", l.Code, "url", l.URL)
	// Redirect after the POST, so that reloading the page does not add the
	// link again.
	http.Redirect(w, r, "/?created="+l.Code, http.StatusSeeOther)
}

func (a *app) visit(w http.ResponseWriter, r *http.Request) {
	target, ok, err := a.store.Visit(r.PathValue("code"))
	if err != nil {
		a.logger.Error("counting a visit", "err", err)
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (a *app) render(w http.ResponseWriter, r *http.Request, status int, d pageData) {
	links, err := a.store.List()
	if err != nil {
		a.logger.Error("reading the links", "err", err)
	}
	d.Links, d.Release = links, a.release
	d.Base = os.Getenv("MOORAGE_URL")
	if d.Base == "" {
		d.Base = "http://" + r.Host
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.page.Execute(w, d); err != nil {
		a.logger.Error("rendering the page", "err", err)
	}
}
