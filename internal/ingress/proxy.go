package ingress

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/robert-mcdermott/moorage/internal/auth"
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
	h2c      atomic.Bool  // it speaks cleartext HTTP/2, and not HTTP/1.1
}

// UseH2C says that the backend speaks cleartext HTTP/2 on its port, as gRPC servers do.
func (b *Backend) UseH2C(on bool) { b.h2c.Store(on) }

// H2C says whether it does.
func (b *Backend) H2C() bool { return b.h2c.Load() }

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
	sticky   atomic.Bool
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
	cleartext := transport.Clone()
	cleartext.Protocols = new(http.Protocols)
	cleartext.Protocols.SetUnencryptedHTTP2(true)
	p.proxy = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		Transport:    &retryTransport{pool: p, base: transport, h2c: cleartext},
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

// SetSticky says whether a browser stays with the instance it first reached.
func (p *Pool) SetSticky(on bool) { p.sticky.Store(on) }

// stickyName is the cookie's name: this environment's own, as apps can share a hostname.
func (p *Pool) stickyName() string {
	sum := sha256.Sum256([]byte(p.App + "/" + p.Env))
	return "moorage_sticky_" + hex.EncodeToString(sum[:4])
}

// stickyValue says which instance a cookie means, and nothing else. A value that someone has made up can only choose among
// the instances that are there, as anyone choosing at random might.
func (p *Pool) stickyValue(b *Backend) string {
	sum := sha256.Sum256([]byte(p.App + "/" + p.Env + "/" + b.ID))
	return hex.EncodeToString(sum[:8])
}

// sticking chooses the instance that the cookie names, if it is there and can take a request, or else any.
func (p *Pool) sticking(w http.ResponseWriter, r *http.Request) *Backend {
	name := p.stickyName()
	if c, err := r.Cookie(name); err == nil {
		now := time.Now().UnixNano()
		for _, b := range *p.backends.Load() {
			if b.usable(now) && p.stickyValue(b) == c.Value {
				return b
			}
		}
	}
	b := p.pick(nil)
	if b != nil {
		http.SetCookie(w, &http.Cookie{Name: name, Value: p.stickyValue(b), Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	}
	return b
}

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b *Backend
	if p.sticky.Load() {
		b = p.sticking(w, r)
	} else {
		b = p.pick(nil)
	}
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
	identityHeaders(pr.Out.Header, pr.In)
	if p.sticky.Load() {
		withoutCookie(pr.Out, p.stickyName())
	}
}

// withoutCookie takes a cookie of Moorage's out of what an app hears.
func withoutCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
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
	h2c  http.RoundTripper
}

// to is how to reach a backend.
func (t *retryTransport) to(b *Backend) http.RoundTripper {
	if b != nil && b.H2C() {
		return t.h2c
	}
	return t.base
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	chosen, _ := req.Context().Value(backendKey{}).(*Backend)
	res, err := t.to(chosen).RoundTrip(req)
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
	return t.to(alt).RoundTrip(retry)
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
// platformHeader says whether a client's header is one that only Moorage may send, as an app would
// read it: some ways of serving apps take an underscore for a dash, and would hear X_Moorage_User
// as X-Moorage-User.
func platformHeader(name string) bool {
	norm := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	if strings.HasPrefix(norm, "x-moorage-") {
		return true
	}
	if !strings.Contains(name, "_") {
		return false
	}
	switch norm {
	case "x-forwarded-for", "x-forwarded-proto", "x-forwarded-host", "x-forwarded-prefix", "x-real-ip", "x-request-id", "forwarded":
		return true
	}
	return false
}

func setForwarded(pr *httputil.ProxyRequest, trusted []netip.Prefix) {
	in, out := pr.In, pr.Out
	client := remoteIP(in.RemoteAddr)
	fromProxy := isTrusted(client, trusted)
	xff, proto, host, realIP := client, "http", in.Host, auth.ClientIP(in, trusted)
	if in.TLS != nil {
		proto = "https"
	}
	if fromProxy {
		if prior := strings.Join(in.Header.Values("X-Forwarded-For"), ", "); prior != "" {
			xff = prior + ", " + client
		}
		if v := in.Header.Get("X-Forwarded-Proto"); v == "http" || v == "https" {
			proto = v
		}
		if v := in.Header.Get("X-Forwarded-Host"); v != "" {
			host = v
		}
	}
	for k := range out.Header {
		if platformHeader(k) {
			delete(out.Header, k)
		}
	}
	out.Header.Del("Forwarded")
	out.Header.Del("X-Forwarded-Prefix")
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
