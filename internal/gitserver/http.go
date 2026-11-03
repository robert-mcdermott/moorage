package gitserver

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/names"
)

// HTTP serves git's Smart HTTP protocol at /git/<app>.git/…, with HTTP
// Basic authentication.
type HTTP struct {
	Git *Server
	// Authorize returns who a request's Basic credentials belong to, if they may
	// read app's repository, or, with push, push to it. It fails with
	// ErrBadCredentials or ErrForbidden.
	Authorize func(r *http.Request, password, app string, push bool) (who string, err error)
}

// Errors of HTTP.Authorize.
var (
	ErrBadCredentials = errors.New("bad credentials")
	ErrForbidden      = errors.New("not allowed")
)

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/git/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	repo, action, ok := strings.Cut(rest, "/")
	app := strings.TrimSuffix(repo, ".git")
	if !ok || names.ValidateApp(app) != nil {
		http.Error(w, "not a repository: use /git/<app>.git", http.StatusNotFound)
		return
	}
	push := action == "git-receive-pack" || r.URL.Query().Get("service") == "git-receive-pack"
	_, password, hasAuth := r.BasicAuth()
	var who string
	err := ErrBadCredentials
	if hasAuth {
		who, err = h.Authorize(r, password, app, push)
	}
	switch {
	case errors.Is(err, ErrForbidden):
		http.Error(w, "That token may not do this: pushing needs the deploy scope, and a token for one app reaches only that app.", http.StatusForbidden)
		return
	case err != nil:
		w.Header().Set("WWW-Authenticate", `Basic realm="Moorage", charset="UTF-8"`)
		http.Error(w, "Moorage needs a token: the password is an API token (moor git credential supplies it)", http.StatusUnauthorized)
		return
	}

	switch {
	case action == "info/refs" && r.Method == http.MethodGet:
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			http.Error(w, "Moorage serves the smart protocol only", http.StatusForbidden)
			return
		}
		h.serve(w, r, app, who, service, true)
	case (action == "git-upload-pack" || action == "git-receive-pack") && r.Method == http.MethodPost:
		if r.Header.Get("Content-Type") != "application/x-"+action+"-request" {
			http.Error(w, "unexpected content type", http.StatusUnsupportedMediaType)
			return
		}
		h.serve(w, r, app, who, action, false)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *HTTP) serve(w http.ResponseWriter, r *http.Request, app, who, service string, advertise bool) {
	if service == "git-upload-pack" && !h.Git.Repos.Exists(app) {
		http.Error(w, "there is no app named "+app, http.StatusNotFound)
		return
	}
	var body io.ReadCloser = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "the request is not gzip", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	kind := "result"
	if advertise {
		kind = "advertisement"
		w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-"+kind)
	w.Header().Set("X-Accel-Buffering", "no") // a push's progress goes as it happens
	w.Header().Set("X-Content-Type-Options", "nosniff")
	out := &flushWriter{w: w}
	protocol := r.Header.Get("Git-Protocol")
	var err error
	if service == "git-receive-pack" {
		err = h.Git.receivePack(r.Context(), app, who, body, out, advertise, true, protocol)
	} else {
		err = h.Git.uploadPack(r.Context(), app, body, out, advertise, true, protocol)
	}
	if err != nil && !out.started {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrNoRepository) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	if err != nil {
		h.Git.logger().Info("git over HTTP", "service", service, "app", app, "by", who, "err", err)
	}
}

// flushWriter sends what is written at once, so that a push's progress
// reaches the client as it happens.
type flushWriter struct {
	w       http.ResponseWriter
	started bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.started = true
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func (f *flushWriter) Close() error { return nil }
