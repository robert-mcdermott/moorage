package doctor

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/ingress"
)

// backend is the server's router, with what it needs for the proxy check.
func backend(t *testing.T, trusted ...string) *httptest.Server {
	t.Helper()
	cfg, err := config.Preset("lan", config.PresetOptions{Domain: "apps.test", DataDir: t.TempDir(), BehindProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Proxy.Trusted = trusted
	rt := ingress.NewRouter(cfg, nil, cfg.DataDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(rt.Close)
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return srv
}

// proxy is a proxy that behaves, changed by what's passed to it.
type proxyOptions struct {
	dropHost, buffer, noUpgrade, noForwarded bool
	bodyLimit                                int64
	timeout                                  time.Duration
}

func frontProxy(t *testing.T, target string, o proxyOptions) http.Handler {
	t.Helper()
	u, _ := url.Parse(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = o.timeout
	rp := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			if !o.dropHost {
				pr.Out.Host = pr.In.Host
			}
			if !o.noForwarded {
				pr.SetXForwarded()
			}
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.bodyLimit > 0 && r.ContentLength > o.bodyLimit {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		if o.noUpgrade {
			r.Header.Del("Upgrade")
			r.Header.Del("Connection")
		}
		if o.buffer {
			rec := httptest.NewRecorder()
			rp.ServeHTTP(rec, r)
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// through checks a server through a proxy, with hostnames that all lead to it.
func through(t *testing.T, o proxyOptions, opts Options, trusted ...string) []Check {
	t.Helper()
	if trusted == nil {
		trusted = []string{"127.0.0.1/32"}
	}
	front := httptest.NewServer(frontProxy(t, backend(t, trusted...).URL, o))
	t.Cleanup(front.Close)
	return checkThrough(t, front, opts, "http")
}

func checkThrough(t *testing.T, front *httptest.Server, opts Options, scheme string) []Check {
	t.Helper()
	cfg, _ := config.Preset("lan", config.PresetOptions{Domain: "apps.test", DataDir: t.TempDir(), BehindProxy: true})
	addr := front.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	opts.Config = cfg
	opts.PublicURL = scheme + "://moorage.apps.test:" + port
	opts.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	opts.defaults()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return checkPublic(ctx, opts)
}

func TestPublicPathAllWell(t *testing.T) {
	checks := through(t, proxyOptions{}, Options{Timeouts: true, Wait: 2 * time.Second})
	for _, name := range []string{"proxy (headers)", "proxy (apps)", "proxy (websockets)", "proxy (streaming)", "proxy (uploads)", "proxy (timeouts)"} {
		if c := mustFind(t, checks, name); c.Status != OK {
			t.Errorf("%+v", c)
		}
	}
}

func TestPublicPathProblems(t *testing.T) {
	for _, c := range []struct {
		what    string
		proxy   proxyOptions
		trusted []string
		check   string
		detail  string
		fix     string
	}{
		{"an untrusted proxy", proxyOptions{}, []string{"10.0.0.1/32"}, "proxy (headers)", "does not list", "under [proxy] trusted"},
		{"no Host", proxyOptions{dropHost: true}, nil, "proxy (headers)", "not \"moorage.apps.test", "proxy_set_header Host"},
		{"no X-Forwarded-For", proxyOptions{noForwarded: true}, nil, "proxy (headers)", "X-Forwarded-For did not arrive", "proxy-config"},
		{"no upgrades", proxyOptions{noUpgrade: true}, nil, "proxy (websockets)", "an upgrade got 426", "Upgrade and Connection"},
		{"buffering", proxyOptions{buffer: true}, nil, "proxy (streaming)", "buffered", "proxy_buffering off"},
		{"a limit on bodies", proxyOptions{bodyLimit: 1 << 20}, nil, "proxy (uploads)", "413", "client_max_body_size"},
	} {
		t.Run(c.what, func(t *testing.T) {
			got := mustFind(t, through(t, c.proxy, Options{}, c.trusted...), c.check)
			if got.Status != Fail || !strings.Contains(got.Detail, c.detail) || !strings.Contains(got.Fix, c.fix) {
				t.Errorf("%+v", got)
			}
		})
	}
}

func TestPublicPathTimeout(t *testing.T) {
	checks := through(t, proxyOptions{timeout: 500 * time.Millisecond}, Options{Timeouts: true, Wait: 2 * time.Second})
	c := mustFind(t, checks, "proxy (timeouts)")
	if c.Status != Fail || !strings.Contains(c.Fix, "proxy_read_timeout") {
		t.Errorf("%+v", c)
	}
	// Unless asked, it says it has not looked.
	checks = through(t, proxyOptions{}, Options{})
	if c := mustFind(t, checks, "proxy (timeouts)"); c.Status != Info || !strings.Contains(c.Fix, "--timeouts") {
		t.Errorf("%+v", c)
	}
}

func TestPublicPathTLS(t *testing.T) {
	front := httptest.NewUnstartedServer(frontProxy(t, backend(t, "127.0.0.1/32").URL, proxyOptions{}))
	dir := t.TempDir()
	certFile, keyFile, cert := certificate(t, dir, "proxy", time.Now().AddDate(1, 0, 0), true, "moorage.apps.test", "*.apps.test")
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	front.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	front.StartTLS()
	defer front.Close()

	// A certificate nothing here trusts.
	checks := checkThrough(t, front, Options{}, "https")
	c := mustFind(t, checks, "proxy (headers)")
	if c.Status != Fail || !strings.Contains(c.Fix, "--ca-cert") || len(checks) != 1 {
		t.Errorf("%+v", checks)
	}

	// With its issuer given, the whole path checks out, WebSockets over TLS too.
	file := filepath.Join(dir, "issuer.pem")
	os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644)
	checks = checkThrough(t, front, Options{CACert: file}, "https")
	for _, c := range checks {
		if c.Status == Fail {
			t.Errorf("%+v", c)
		}
	}
	want(t, mustFind(t, checks, "proxy (websockets)"), OK, "both directions")
}

func TestPublicURLNotUnderstood(t *testing.T) {
	cfg, _ := config.Preset("lan", config.PresetOptions{Domain: "apps.test", DataDir: t.TempDir(), BehindProxy: true})
	checks := checkPublic(context.Background(), Options{Config: cfg, PublicURL: "moorage.apps.test"})
	if checks[0].Status != Fail || !strings.Contains(checks[0].Detail, "not an address") {
		t.Errorf("%+v", checks)
	}
}
