// Package ingress is Moorage's front door: it matches each request's
// hostname to an app and serves it, answers platform endpoints, terminates
// TLS, and hands console and API requests to the console handler
// (spec/06-ingress.md).
package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"sort"
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
	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Router routes requests by hostname. Its routing table is an immutable
// snapshot replaced atomically by Reload, so a request always sees one
// consistent table and deploys never interrupt requests in flight.
type Router struct {
	// active holds when each environment last had a request, and open how many it has going
	// on, by "app/env".
	active sync.Map
	open   sync.Map
	// metrics hears of requests.
	metrics atomic.Pointer[metrics.Store]
	// tokenOf is what shows an app that a request came through here.
	tokenOf func(envID string) string
	// sleeper says which environments are asleep, and gate lets people through their gates.
	sleeper atomic.Pointer[Sleeper]
	gate    atomic.Pointer[Gatekeeper]

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

// Sleeper says which environments are asleep, and wakes them for requests.
type Sleeper interface {
	Asleep(app, env string) bool
	WakeFor(app, env string) <-chan struct{}
}

// PoolSource provides the backend pool of an environment of a process app.
type PoolSource interface {
	Pool(app, env string) *Pool
}

type routeTable struct {
	generation uint64
	// routes maps a hostname to what serves it.
	routes map[string]route
	// prefixed is what serves paths of a hostname, the longest first.
	prefixed map[string][]route
	// apps holds every app name, deployed or not.
	apps map[string]bool
	// permalinks has what each served environment's static releases go through, by
	// names.PermalinkBase, and releases those that have been asked for, by hostname.
	permalinks map[string]route
	releases   sync.Map
}

type route struct {
	app, env string
	handler  http.Handler
	gate     gateRoute
	// prefix is the path it serves, if not the whole hostname, and strip says that the app
	// should not hear it. proxyToken shows the app that a request came through here.
	prefix     string
	strip      bool
	proxyToken string
	// release says that this is one release of an environment's at an address of its own: it
	// needs nothing awake, and keeps nothing awake.
	release bool
}

// lookup finds what serves a hostname and a path: the longest path that fits, or else the
// hostname as a whole. rest is the path after it.
func (t *routeTable) lookup(host, path string) (rte route, rest string, ok bool) {
	for _, r := range t.prefixed[host] {
		if path == r.prefix {
			return r, "", true
		}
		if tail, found := strings.CutPrefix(path, r.prefix+"/"); found {
			return r, "/" + tail, true
		}
	}
	rte, ok = t.routes[host]
	return rte, path, ok
}

// find is lookup, and also the addresses of static releases.
func (rt *Router) find(t *routeTable, host, path string) (rte route, rest string, ok bool) {
	if rte, rest, ok = t.lookup(host, path); ok {
		return rte, rest, true
	}
	rte, ok = rt.permalink(t, host)
	return rte, path, ok
}

// permalinkOf reads a hostname as the address of a release of an environment that is served.
func (rt *Router) permalinkOf(t *routeTable, host string) (env route, version int, ok bool) {
	label, ok := strings.CutSuffix(host, "."+rt.cfg.Domain)
	if !ok {
		return route{}, 0, false
	}
	base, version, ok := names.ParsePermalink(label)
	if !ok {
		return route{}, 0, false
	}
	env, ok = t.permalinks[base]
	return env, version, ok
}

// permalink finds what serves one release of a static site: a release that is kept and is a static
// site, behind the gate that its environment has now. It looks only when asked, so that a server
// with many old releases pays for none of them until someone wants one.
func (rt *Router) permalink(t *routeTable, host string) (route, bool) {
	env, version, ok := rt.permalinkOf(t, host)
	if !ok {
		return route{}, false
	}
	if found, ok := t.releases.Load(host); ok {
		return found.(route), true
	}
	_, b, err := rt.db.ReleaseVersion(context.Background(), env.app, env.env, version)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			rt.log.Error("cannot read a release", "app", env.app, "env", env.env, "version", version, "err", err)
		}
		return route{}, false
	}
	var plan manifest.Plan
	if b.Collected || json.Unmarshal(b.Plan, &plan) != nil || plan.Kind != manifest.KindStatic {
		return route{}, false
	}
	site, err := rt.loadSite(b, plan)
	if err != nil {
		rt.log.Error("cannot serve a release", "app", env.app, "build", b.ID, "err", err)
		return route{}, false
	}
	env.handler, env.release = noindex(site), true
	t.releases.Store(host, env)
	return env, true
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

// SetSleeper sets what says which environments are asleep. Until then, none are.
func (rt *Router) SetSleeper(s Sleeper) { rt.sleeper.Store(&s) }

// Trusted returns the trusted proxy prefixes.
func (rt *Router) Trusted() []netip.Prefix { return rt.trusted }

// SetConsole sets the handler for the console hostname (and for requests
// addressed to the server by IP address or as localhost).
func (rt *Router) SetConsole(h http.Handler) { rt.console = h }

// SetCACert sets the certificate served at /.moorage/ca.crt.
func (rt *Router) SetCACert(pem []byte) { rt.caPEM = pem }

// Generation counts routing table changes.
func (rt *Router) Generation() uint64 { return rt.table.Load().generation }

// envRoute is what serves an environment, for its domains to serve too.
type envRoute struct {
	route
	static bool
}

// SetMetrics says what hears of requests.
func (rt *Router) SetMetrics(m *metrics.Store) { rt.metrics.Store(m) }

// SetProxyToken says what shows an app that a request came through here.
func (rt *Router) SetProxyToken(token func(envID string) string) { rt.tokenOf = token }

func (rt *Router) proxyToken(envID string) string {
	if rt.tokenOf == nil {
		return ""
	}
	return rt.tokenOf(envID)
}

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
	next := &routeTable{generation: old.generation + 1, routes: map[string]route{}, prefixed: map[string][]route{}, permalinks: map[string]route{}, apps: map[string]bool{}}
	for _, n := range appNames {
		next.apps[n] = true
	}
	used := map[string]*static.Site{}
	byEnv := map[string]envRoute{}
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
		rte := route{app: r.App, env: r.Env, handler: h, gate: rt.gateOf(r, plan), proxyToken: rt.proxyToken(r.EnvID)}
		byEnv[r.EnvID] = envRoute{rte, plan.Kind != manifest.KindProcess}
		if rt.cfg.Routing == "path" {
			rte.prefix, rte.strip = rt.cfg.EnvPrefix(r.App, r.Env), plan.StripPrefix || plan.Kind != manifest.KindProcess
			next.prefixed[rt.cfg.Domain] = append(next.prefixed[rt.cfg.Domain], rte)
			continue
		}
		next.routes[rt.cfg.EnvHost(r.App, r.Env)] = rte
		next.permalinks[names.PermalinkBase(r.App, r.Env)] = rte
	}
	domains, err := rt.db.AllDomains(ctx)
	if err != nil {
		return err
	}
	for _, d := range domains {
		er, serving := byEnv[d.EnvID]
		if !d.Verified() || !serving {
			continue
		}
		rte := er.route
		if d.Prefix == "" {
			next.routes[d.Hostname] = rte
			continue
		}
		rte.prefix, rte.strip = d.Prefix, d.StripPrefix || er.static
		next.prefixed[d.Hostname] = append(next.prefixed[d.Hostname], rte)
	}
	for _, list := range next.prefixed {
		sort.Slice(list, func(i, j int) bool { return len(list[i].prefix) > len(list[j].prefix) })
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
	if rte, rest, ok := rt.find(t, host, r.URL.Path); ok {
		if rte.prefix != "" {
			switch {
			case rest == "":
				target := rte.prefix + "/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusMovedPermanently)
				return
			case isGatePath(rest):
				rt.gateEndpoint(w, r, rte, rest)
				return
			case strings.HasPrefix(rest, "/.moorage/"):
				notFound.ServeHTTP(w, r)
				return
			}
		}
		rt.serveApp(w, r, rte)
		return
	}
	if rt.cfg.Routing == "path" && host == rt.cfg.Domain {
		rt.noPathRoute(w, r, t)
		return
	}
	if env, version, ok := rt.permalinkOf(t, host); ok {
		releaseNotKept(env.app, version).ServeHTTP(w, r)
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

// noPathRoute says what there is at a path that no app serves, with path routing.
func (rt *Router) noPathRoute(w http.ResponseWriter, r *http.Request, t *routeTable) {
	label, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	app, _, _ := strings.Cut(label, "--")
	switch {
	case label == "":
		notFound.ServeHTTP(w, r)
	case t.apps[app]:
		nothingDeployed(app).ServeHTTP(w, r)
	case names.ValidateApp(app) == nil:
		noSuchApp(app).ServeHTTP(w, r)
	default:
		notFound.ServeHTTP(w, r)
	}
}

// isGatePath says whether a path is one the gates use.
func isGatePath(path string) bool {
	return strings.HasPrefix(path, "/.moorage/gate/") || strings.HasPrefix(path, "/.moorage/auth/") || strings.HasPrefix(path, "/.moorage/share/")
}

// RedirectToHTTPS wraps the router for the plain HTTP listener when Moorage
// terminates TLS: requests move to HTTPS, except platform endpoints and
// requests addressed to this machine (localhost or a loopback address).
func (rt *Router) RedirectToHTTPS(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostname(r.Host)
		// What the gates ask of a browser carries passwords and codes, and so goes over HTTPS.
		if strings.HasPrefix(r.URL.Path, "/.moorage/") && !isGatePath(r.URL.Path) || isLocal(host) {
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

// AllowCertificate reports whether a certificate may be issued for name: the console's hostname, and
// the hostnames under the base domain of apps, their environments and their releases. Anything else
// gets no certificate.
func (rt *Router) AllowCertificate(name string) bool {
	if name == rt.cfg.ConsoleHost || rt.cfg.Routing == "path" && name == rt.cfg.Domain {
		return true
	}
	label, ok := strings.CutSuffix(name, "."+rt.cfg.Domain)
	return ok && !strings.Contains(label, ".") && names.ValidHostLabel(label)
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
	case isGatePath(r.URL.Path):
		t := rt.table.Load()
		host := hostname(r.Host)
		rte, ok := t.routes[host]
		if !ok {
			rte, ok = rt.permalink(t, host)
		}
		if ok {
			rt.gateEndpoint(w, r, rte, r.URL.Path)
		} else {
			notFound.ServeHTTP(w, r)
		}
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
