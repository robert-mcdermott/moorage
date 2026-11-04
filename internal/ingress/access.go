package ingress

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ingress/static"
	"github.com/robert-mcdermott/moorage/internal/observe/logs"
)

// requestInfo travels with a request to an app: its ID and the instance
// that served it, for the access log.
type requestInfo struct {
	id       string
	instance string
}

type infoKey struct{}

// requestID returns the request's ID, set by the router.
func requestID(r *http.Request) string {
	if info, ok := r.Context().Value(infoKey{}).(*requestInfo); ok {
		return info.id
	}
	return newRequestID()
}

func setInstance(r *http.Request, id string) {
	if info, ok := r.Context().Value(infoKey{}).(*requestInfo); ok {
		info.instance = id
	}
}

func (rt *Router) openCount(key string) *atomic.Int64 {
	if n, ok := rt.open.Load(key); ok {
		return n.(*atomic.Int64)
	}
	n, _ := rt.open.LoadOrStore(key, new(atomic.Int64))
	return n.(*atomic.Int64)
}

// Busy says whether an environment has requests going on, such as a stream or a WebSocket,
// which it would lose by going to sleep.
func (rt *Router) Busy(app, env string) bool {
	return rt.openCount(app+"/"+env).Load() > 0
}

// How long a request waits for an environment to wake: a browser's moment before it is told
// what is happening, and for anything else, a client that may not know what to do with a page.
var (
	wakeHoldBrowser = 2 * time.Second
	wakeHoldClient  = 30 * time.Second
)

// awaitAwake holds a request for an environment that is asleep, and wakes it. It says whether
// to go on: if it took too long, it has answered with what the client should do.
func (rt *Router) awaitAwake(w http.ResponseWriter, r *http.Request, rte route) bool {
	sleeper := rt.sleeper.Load()
	if sleeper == nil || !(*sleeper).Asleep(rte.app, rte.env) {
		return true
	}
	done := (*sleeper).WakeFor(rte.app, rte.env)
	browser := wantsPage(r)
	hold := wakeHoldClient
	if browser {
		hold = wakeHoldBrowser
	}
	timer := time.NewTimer(hold)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-r.Context().Done():
		return false
	case <-timer.C:
	}
	w.Header().Set("Retry-After", "2")
	if browser {
		wakingUp(rte.app).ServeHTTP(w, r)
	} else {
		http.Error(w, rte.app+" is waking up: try again in a moment\n", http.StatusServiceUnavailable)
	}
	return false
}

// wantsPage says whether a request is a browser's going to a page, which can be shown one that
// says what is happening, and reloads itself.
func wantsPage(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Upgrade") == "" && strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveApp serves a request for an app and writes it to the app's access
// log. A request ID from a trusted proxy is kept; otherwise one is made.
func (rt *Router) serveApp(w http.ResponseWriter, r *http.Request, rte route) {
	start := time.Now()
	client := remoteIP(r.RemoteAddr)
	trusted := isTrusted(client, rt.trusted)
	info := &requestInfo{id: newRequestID()}
	if id := r.Header.Get("X-Request-Id"); trusted && validRequestID(id) {
		info.id = id
	}
	if trusted {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			client = strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	w.Header().Set("X-Request-Id", info.id)
	key := rte.app + "/" + rte.env
	rt.active.Store(key, start)
	open := rt.openCount(key)
	open.Add(1)
	defer open.Add(-1)
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	if rt.awaitAwake(rec, r, rte) {
		rte.handler.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
	}
	rt.access(rte, accessEntry{
		Time:     start.UTC(),
		ID:       info.id,
		Method:   r.Method,
		Host:     r.Host,
		Path:     r.URL.RequestURI(),
		Status:   rec.status,
		Bytes:    rec.bytes,
		Millis:   float64(time.Since(start).Microseconds()) / 1000,
		Client:   client,
		Instance: info.instance,
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

type accessEntry struct {
	Time     time.Time `json:"time"`
	ID       string    `json:"id"`
	Method   string    `json:"method"`
	Host     string    `json:"host"`
	Path     string    `json:"path"`
	Status   int       `json:"status"`
	Bytes    int64     `json:"bytes"`
	Millis   float64   `json:"ms"`
	Client   string    `json:"client"`
	Instance string    `json:"instance,omitempty"`
}

// access appends an entry to logs/router/<app>/<env>.log.
func (rt *Router) access(rte route, e accessEntry) {
	key := rte.app + "/" + rte.env
	rt.logsMu.Lock()
	f := rt.accessLogs[key]
	if f == nil {
		var err error
		f, err = logs.Open(filepath.Join(rt.dataDir, "logs", "router", rte.app, rte.env+".log"), logs.DefaultMaxSize, logs.DefaultKeep)
		if err != nil {
			rt.logsMu.Unlock()
			rt.log.Warn("cannot open the access log", "app", rte.app, "err", err)
			return
		}
		rt.accessLogs[key] = f
	}
	rt.logsMu.Unlock()
	line, _ := json.Marshal(e)
	f.Write(append(line, '\n'))
}

// LastActive says when an environment last had a request, since the server started.
func (rt *Router) LastActive(app, env string) time.Time {
	if at, ok := rt.active.Load(app + "/" + env); ok {
		return at.(time.Time)
	}
	return time.Time{}
}

// ForgetApp lets go of an app's files, which are moving or going: its access logs, and the
// sites read from its builds, which the next reload reads from wherever they are then.
func (rt *Router) ForgetApp(app string) {
	rt.mu.Lock()
	rt.sites = map[string]*static.Site{}
	rt.mu.Unlock()
	rt.logsMu.Lock()
	defer rt.logsMu.Unlock()
	for key, f := range rt.accessLogs {
		if strings.HasPrefix(key, app+"/") {
			f.Close()
			delete(rt.accessLogs, key)
		}
	}
}

// Close closes the access logs.
func (rt *Router) Close() {
	rt.logsMu.Lock()
	defer rt.logsMu.Unlock()
	for k, f := range rt.accessLogs {
		f.Close()
		delete(rt.accessLogs, k)
	}
}

// recorder notes a response's status and size. It passes flushing and
// hijacking (for upgraded connections) through to the real writer.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Flush() { http.NewResponseController(r.ResponseWriter).Flush() }

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	r.status, r.wrote = http.StatusSwitchingProtocols, true
	return http.NewResponseController(r.ResponseWriter).Hijack()
}
