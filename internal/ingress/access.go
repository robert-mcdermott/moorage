package ingress

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
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
	rt.active.Store(rte.app+"/"+rte.env, start)
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	rte.handler.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
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
