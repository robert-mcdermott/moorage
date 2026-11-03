package ingress

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Backend is one running instance that can receive requests.
type Backend struct {
	ID   string
	Addr string // host:port on the loopback interface

	inflight atomic.Int64
	healthy  atomic.Bool
	ejected  atomic.Int64 // Unix nanoseconds until which it gets no requests
}

// NewBackend returns a healthy backend.
func NewBackend(id, addr string) *Backend {
	b := &Backend{ID: id, Addr: addr}
	b.healthy.Store(true)
	return b
}

// SetHealthy marks the backend healthy or not (by its health checks).
func (b *Backend) SetHealthy(ok bool) { b.healthy.Store(ok) }

// Healthy reports the last health check result.
func (b *Backend) Healthy() bool { return b.healthy.Load() }

// InFlight is the number of requests the backend is serving.
func (b *Backend) InFlight() int64 { return b.inflight.Load() }

func (b *Backend) usable(now int64) bool {
	return b.healthy.Load() && b.ejected.Load() < now
}

// eject keeps requests away from a backend whose connection failed, until
// its health checks have had a chance to notice.
func (b *Backend) eject(d time.Duration) {
	b.ejected.Store(time.Now().Add(d).UnixNano())
}

// Pool is the set of backends serving an environment. A rollout replaces
// the set as a whole; requests in flight finish on the backends they
// started on.
type Pool struct {
	App, Env string

	backends atomic.Pointer[[]*Backend]
	proxy    *httputil.ReverseProxy
	trusted  []netip.Prefix
	log      *slog.Logger
}

// NewPool returns an empty pool. trusted lists the proxies whose
// X-Forwarded-* headers are honoured.
func NewPool(app, env string, trusted []netip.Prefix, log *slog.Logger) *Pool {
	p := &Pool{App: app, Env: env, trusted: trusted, log: log}
	empty := []*Backend{}
	p.backends.Store(&empty)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.MaxIdleConnsPerHost = 32
	transport.ResponseHeaderTimeout = 0
	p.proxy = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		Transport:    &retryTransport{pool: p, base: transport},
		ErrorHandler: p.proxyError,
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	return p
}

// Set replaces the backends.
func (p *Pool) Set(bs []*Backend) {
	cp := append([]*Backend(nil), bs...)
	p.backends.Store(&cp)
}

// Backends returns the current backends.
func (p *Pool) Backends() []*Backend { return *p.backends.Load() }

// pick chooses a usable backend other than skip: of two picked at random,
// the one with fewer requests in flight.
func (p *Pool) pick(skip *Backend) *Backend {
	now := time.Now().UnixNano()
	var usable []*Backend
	for _, b := range *p.backends.Load() {
		if b != skip && b.usable(now) {
			usable = append(usable, b)
		}
	}
	switch len(usable) {
	case 0:
		return nil
	case 1:
		return usable[0]
	}
	i := rand.IntN(len(usable))
	j := rand.IntN(len(usable) - 1)
	if j >= i {
		j++
	}
	if usable[j].inflight.Load() < usable[i].inflight.Load() {
		return usable[j]
	}
	return usable[i]
}

type backendKey struct{}

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := p.pick(nil)
	if b == nil {
		w.Header().Set("Retry-After", "5")
		unavailable(p.App).ServeHTTP(w, r)
		return
	}
	b.inflight.Add(1)
	defer b.inflight.Add(-1)
	setInstance(r, b.ID)
	p.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), backendKey{}, b)))
}

func (p *Pool) rewrite(pr *httputil.ProxyRequest) {
	b := pr.In.Context().Value(backendKey{}).(*Backend)
	pr.SetURL(&url.URL{Scheme: "http", Host: b.Addr})
	pr.Out.Host = pr.In.Host // apps see the hostname they are served at
	setForwarded(pr, p.trusted)
}

func (p *Pool) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if b, ok := r.Context().Value(backendKey{}).(*Backend); ok && isDialError(err) {
		b.eject(5 * time.Second)
	}
	if errors.Is(err, context.Canceled) {
		return // the client went away
	}
	p.log.Warn("proxying failed", "app", p.App, "env", p.Env, "err", err)
	unavailable(p.App).ServeHTTP(w, r)
}

// retryTransport retries an idempotent request without a body once on
// another backend when the connection to the first one is refused.
type retryTransport struct {
	pool *Pool
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err == nil || !isDialError(err) || !idempotent(req) {
		return res, err
	}
	first, _ := req.Context().Value(backendKey{}).(*Backend)
	if first != nil {
		first.eject(5 * time.Second)
	}
	alt := t.pool.pick(first)
	if alt == nil {
		return res, err
	}
	retry := req.Clone(req.Context())
	retry.URL.Host = alt.Addr
	return t.base.RoundTrip(retry)
}

func idempotent(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.Body == nil || r.Body == http.NoBody
	}
	return false
}

func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// setForwarded sets the X-Forwarded-* family on a proxied request. Headers
// a client sent are kept only when it is a trusted proxy; anything else
// could be forged. X-Moorage-* headers from clients are always removed.
func setForwarded(pr *httputil.ProxyRequest, trusted []netip.Prefix) {
	in, out := pr.In, pr.Out
	client := remoteIP(in.RemoteAddr)
	fromProxy := isTrusted(client, trusted)
	xff, proto, host, realIP := client, "http", in.Host, client
	if in.TLS != nil {
		proto = "https"
	}
	if fromProxy {
		if prior := strings.Join(in.Header.Values("X-Forwarded-For"), ", "); prior != "" {
			xff = prior + ", " + client
			realIP = strings.TrimSpace(strings.Split(prior, ",")[0])
		}
		if v := in.Header.Get("X-Forwarded-Proto"); v == "http" || v == "https" {
			proto = v
		}
		if v := in.Header.Get("X-Forwarded-Host"); v != "" {
			host = v
		}
	}
	for k := range out.Header {
		if strings.HasPrefix(k, "X-Moorage-") {
			out.Header.Del(k)
		}
	}
	out.Header.Del("Forwarded")
	out.Header.Set("X-Forwarded-For", xff)
	out.Header.Set("X-Forwarded-Proto", proto)
	out.Header.Set("X-Forwarded-Host", host)
	out.Header.Set("X-Real-IP", realIP)
	forwardedFor := realIP
	if strings.Contains(forwardedFor, ":") {
		forwardedFor = "[" + forwardedFor + "]"
	}
	out.Header.Set("Forwarded", `for="`+forwardedFor+`";host="`+host+`";proto=`+proto)
	out.Header.Set("X-Request-Id", requestID(in))
}

func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func isTrusted(ip string, trusted []netip.Prefix) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// newRequestID returns 16 random bytes in hex.
func newRequestID() string {
	var b [16]byte
	crand.Read(b[:])
	return hex.EncodeToString(b[:])
}
