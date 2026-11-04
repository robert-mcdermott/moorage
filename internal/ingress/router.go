// Package ingress is Moorage's front door: it matches each request's
// hostname to an app and serves it, answers platform endpoints, terminates
// TLS, and hands console and API requests to the console handler
// (spec/06-ingress.md).
package ingress

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
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
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Router routes requests by hostname. Its routing table is an immutable
// snapshot replaced atomically by Reload, so a request always sees one
// consistent table and deploys never interrupt requests in flight.
type Router struct {
	// active holds when each environment last had a request, by "app/env".
	active sync.Map

	cfg     config.Config
	db      *store.DB
	dataDir string
	log     *slog.Logger
	console http.Handler
	caPEM   []byte
	pools   PoolSource
	trusted []netip.Prefix

	table atomic.Pointer[routeTable]

	proxyChecks atomic.Int32 // the proxy checks under way

	mu    sync.Mutex              // serialises Reload
	sites map[string]*static.Site // by build ID, reused across reloads

	logsMu     sync.Mutex
	accessLogs map[string]*logs.File // by "app/env"
}

// PoolSource provides the backend pool of an environment of a process app.
type PoolSource interface {
	Pool(app, env string) *Pool
}

type routeTable struct {
	generation uint64
	// routes maps a hostname to what serves it.
	routes map[string]route
	// apps holds every app name, deployed or not.
	apps map[string]bool
}

type route struct {
	app, env string
	handler  http.Handler
}

// NewRouter returns a router with an empty table; call Reload to fill it.
func NewRouter(cfg config.Config, db *store.DB, dataDir string, log *slog.Logger) *Router {
	rt := &Router{cfg: cfg, db: db, dataDir: dataDir, log: log, console: running,
		sites: map[string]*static.Site{}, accessLogs: map[string]*logs.File{}}
	for _, t := range cfg.Proxy.Trusted {
		if p, err := config.ParseTrusted(t); err == nil {
			rt.trusted = append(rt.trusted, p)
		}
	}
	rt.table.Store(&routeTable{routes: map[string]route{}, apps: map[string]bool{}})
	return rt
}

// SetPools sets where the pools of process apps come from.
func (rt *Router) SetPools(ps PoolSource) { rt.pools = ps }

// Trusted returns the trusted proxy prefixes.
func (rt *Router) Trusted() []netip.Prefix { return rt.trusted }

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
	next := &routeTable{generation: old.generation + 1, routes: map[string]route{}, apps: map[string]bool{}}
	for _, n := range appNames {
		next.apps[n] = true
	}
	used := map[string]*static.Site{}
	for _, r := range routes {
		var plan manifest.Plan
		if err := json.Unmarshal(r.Build.Plan, &plan); err != nil {
			rt.log.Error("cannot read a build's plan", "app", r.App, "build", r.Build.ID, "err", err)
			continue
		}
		var h http.Handler
		switch plan.Kind {
		case manifest.KindStatic:
			site := rt.sites[r.Build.ID]
			if site == nil {
				if site, err = rt.loadSite(r.Build, plan); err != nil {
					rt.log.Error("cannot serve build", "app", r.App, "build", r.Build.ID, "err", err)
					continue
				}
			}
			used[r.Build.ID] = site
			h = site
		case manifest.KindProcess:
			web, ok := plan.Processes[manifest.Web]
			switch {
			case !ok || (web.Expose != nil && !*web.Expose):
				h = noWebProcess(r.App)
			case rt.pools == nil:
				h = unavailable(r.App)
			default:
				h = rt.pools.Pool(r.App, r.Env)
			}
		default:
			rt.log.Error("build plan kind not served by this version", "app", r.App, "kind", plan.Kind)
			continue
		}
		if r.Env != names.Production {
			h = noindex(h)
		}
		next.routes[rt.cfg.EnvHost(r.App, r.Env)] = route{app: r.App, env: r.Env, handler: h}
	}
	rt.sites = used
	rt.table.Store(next)
	return nil
}

func (rt *Router) loadSite(b store.Build, plan manifest.Plan) (*static.Site, error) {
	dir := filepath.Join(rt.dataDir, filepath.FromSlash(b.ArtifactRef))
	idx, err := static.ReadIndex(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	var rules manifest.Static
	if plan.Static != nil {
		rules = *plan.Static
	}
	root := "site" // an uploaded site; a built one names its directory
	if plan.Root != "" {
		root = plan.Root
	}
	return static.NewSite(filepath.Join(dir, filepath.FromSlash(root)), idx, rules, notFound), nil
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
	if rte, ok := t.routes[host]; ok {
		rt.serveApp(w, r, rte)
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
	case r.URL.Path == proxyCheckPrefix || strings.HasPrefix(r.URL.Path, proxyCheckPrefix+"/"):
		rt.proxyCheck(w, r)
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

// noindex keeps an environment that is not production out of search engines.
func noindex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}
