// Package ingress is Moorage's front door: it matches each request's
// hostname to an app and serves it, answers platform endpoints, terminates
// TLS, and hands console and API requests to the console handler
// (docs/06-ingress.md).
package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/ingress/static"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/names"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Router routes requests by hostname. Its routing table is an immutable
// snapshot replaced atomically by Reload, so a request always sees one
// consistent table and deploys never interrupt requests in flight.
type Router struct {
	cfg     config.Config
	db      *store.DB
	dataDir string
	log     *slog.Logger
	console http.Handler
	caPEM   []byte

	table atomic.Pointer[routeTable]

	mu    sync.Mutex              // serialises Reload
	sites map[string]*static.Site // by build ID, reused across reloads
}

type routeTable struct {
	generation uint64
	// sites maps a hostname to the site it serves.
	sites map[string]*static.Site
	// apps holds every app name, deployed or not.
	apps map[string]bool
}

// NewRouter returns a router with an empty table; call Reload to fill it.
func NewRouter(cfg config.Config, db *store.DB, dataDir string, log *slog.Logger) *Router {
	rt := &Router{cfg: cfg, db: db, dataDir: dataDir, log: log, console: running, sites: map[string]*static.Site{}}
	rt.table.Store(&routeTable{sites: map[string]*static.Site{}, apps: map[string]bool{}})
	return rt
}

// SetConsole sets the handler for the console hostname (and for requests
// addressed to the server by IP address or as localhost).
func (rt *Router) SetConsole(h http.Handler) { rt.console = h }

// SetCACert sets the certificate served at /.moorage/ca.crt.
func (rt *Router) SetCACert(pem []byte) { rt.caPEM = pem }

// Generation counts routing table changes.
func (rt *Router) Generation() uint64 { return rt.table.Load().generation }

// Reload rebuilds the routing table from the database and swaps it in.
func (rt *Router) Reload(ctx context.Context) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	routes, err := rt.db.Routes(ctx)
	if err != nil {
		return err
	}
	appNames, err := rt.db.AppNames(ctx)
	if err != nil {
		return err
	}
	old := rt.table.Load()
	next := &routeTable{generation: old.generation + 1, sites: map[string]*static.Site{}, apps: map[string]bool{}}
	for _, n := range appNames {
		next.apps[n] = true
	}
	used := map[string]*static.Site{}
	for _, route := range routes {
		if route.Env != names.Production {
			continue // other environments arrive with previews
		}
		site := rt.sites[route.Build.ID]
		if site == nil {
			if site, err = rt.loadSite(route.Build); err != nil {
				rt.log.Error("cannot serve build", "app", route.App, "build", route.Build.ID, "err", err)
				continue
			}
		}
		used[route.Build.ID] = site
		next.sites[route.App+"."+rt.cfg.Domain] = site
	}
	rt.sites = used
	rt.table.Store(next)
	return nil
}

func (rt *Router) loadSite(b store.Build) (*static.Site, error) {
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	if plan.Kind != "static" {
		return nil, fmt.Errorf("plan kind %q is not served by this version", plan.Kind)
	}
	dir := filepath.Join(rt.dataDir, filepath.FromSlash(b.ArtifactRef))
	idx, err := static.ReadIndex(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	var rules manifest.Static
	if plan.Static != nil {
		rules = *plan.Static
	}
	return static.NewSite(filepath.Join(dir, "site"), idx, rules, notFound), nil
}

// ServeHTTP routes a request.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/.moorage/") {
		rt.platform(w, r)
		return
	}
	host := hostname(r.Host)
	if host == rt.cfg.ConsoleHost || isAddress(host) {
		rt.console.ServeHTTP(w, r)
		return
	}
	t := rt.table.Load()
	if s := t.sites[host]; s != nil {
		s.ServeHTTP(w, r)
		return
	}
	if app, ok := rt.appOf(host); ok {
		if t.apps[app] {
			nothingDeployed(app).ServeHTTP(w, r)
		} else {
			noSuchApp(app).ServeHTTP(w, r)
		}
		return
	}
	notFound.ServeHTTP(w, r)
}

// RedirectToHTTPS wraps the router for the plain HTTP listener when Moorage
// terminates TLS: requests move to HTTPS, except platform endpoints and
// requests addressed to this machine (localhost or a loopback address).
func (rt *Router) RedirectToHTTPS(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostname(r.Host)
		if strings.HasPrefix(r.URL.Path, "/.moorage/") || isLocal(host) {
			rt.ServeHTTP(w, r)
			return
		}
		target := "https://" + host
		if httpsPort != "443" {
			target += ":" + httpsPort
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// AllowCertificate reports whether a certificate may be issued for name:
// the console's hostname and the hostnames of apps under the base domain.
// Anything else gets no certificate.
func (rt *Router) AllowCertificate(name string) bool {
	if name == rt.cfg.ConsoleHost {
		return true
	}
	_, ok := rt.appOf(name)
	return ok
}

// appOf returns the app a hostname under the base domain names: the single
// label in front of the domain, when it is a valid app name.
func (rt *Router) appOf(host string) (string, bool) {
	label, ok := strings.CutSuffix(host, "."+rt.cfg.Domain)
	if !ok || strings.Contains(label, ".") || names.ValidateApp(label) != nil {
		return "", false
	}
	return label, true
}

func (rt *Router) platform(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.moorage/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		rt.health(w, r)
	case r.URL.Path == "/.moorage/ca.crt" && rt.caPEM != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h := w.Header()
		h.Set("Content-Type", "application/x-pem-file")
		h.Set("Content-Disposition", `attachment; filename="moorage-ca.crt"`)
		_, _ = w.Write(rt.caPEM)
	default:
		notFound.ServeHTTP(w, r)
	}
}

type health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

func (rt *Router) health(w http.ResponseWriter, r *http.Request) {
	h, code := health{Status: "ok", Version: buildinfo.Version}, http.StatusOK
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := rt.db.Reader.PingContext(ctx); err != nil {
		h.Status, code = "database unavailable", http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(h)
}

// hostname returns the lowercase host of a Host header, without the port or
// a trailing dot.
func hostname(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// isAddress reports whether host is an IP address or localhost: a request
// for the server itself rather than for an app.
func isAddress(host string) bool {
	return host == "localhost" || net.ParseIP(host) != nil
}

// isLocal reports whether host refers to this machine.
func isLocal(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
