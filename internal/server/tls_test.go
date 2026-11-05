package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

func tlsConfigFor(t *testing.T, cfg config.Config, mode string) config.Config {
	t.Helper()
	cfg.Listen.HTTPS = "127.0.0.1:0"
	cfg.TLS.Mode = mode
	cfg.Proxy.ExternalScheme = "https"
	return cfg
}

// fetchTLS requests path over HTTPS from addr, with SNI and Host set to
// host, trusting only the roots in caPEM.
func fetchTLS(t *testing.T, addr net.Addr, caPEM []byte, host, path string) (*http.Response, string, error) {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("no CA certificate to trust")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: host}}}
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr.String()+path, nil)
	req.Host = host
	res, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body), nil
}

func TestInternalTLS(t *testing.T) {
	cfg := tlsConfigFor(t, testConfig(t), "internal")
	addrs, _ := start(t, cfg)
	caPEM, err := os.ReadFile(filepath.Join(cfg.DataDir, "certs", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}

	res, _, err := fetchTLS(t, addrs.HTTPS, caPEM, "moorage.localhost", "/.moorage/health")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("a server that did not ask for HSTS sent %q", got)
	}
	leaf := res.TLS.PeerCertificates[0]
	if res.StatusCode != 200 || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "moorage.localhost" {
		t.Errorf("console certificate: %d %v", res.StatusCode, leaf.DNSNames)
	}
	if life := time.Until(leaf.NotAfter); life < 29*24*time.Hour || life > 31*24*time.Hour {
		t.Errorf("certificate lifetime %v, want about 30 days", life)
	}

	c := apiClient(t, cfg, addrs)
	site := writeSite(t, map[string]string{"index.html": "secure"})
	d, _, err := c.DeployStatic(context.Background(), "docs", "production", site, manifest.Static{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.URL, "https://docs.localhost") {
		t.Errorf("URL = %q", d.URL)
	}
	if _, body, err := fetchTLS(t, addrs.HTTPS, caPEM, "docs.localhost", "/"); err != nil || body != "secure" {
		t.Errorf("app over TLS: %q, %v", body, err)
	}
	// Unknown apps under the domain get a certificate and the platform page;
	// other names get no certificate at all.
	if res, _, err := fetchTLS(t, addrs.HTTPS, caPEM, "unknown-app.localhost", "/"); err != nil || res.StatusCode != 404 {
		t.Errorf("unknown app over TLS: %v, %v", res, err)
	}
	if _, _, err := fetchTLS(t, addrs.HTTPS, caPEM, "evil.example.org", "/"); err == nil {
		t.Error("a certificate was issued for a name outside the domain")
	}

	// Plain HTTP moves to HTTPS, except for this machine and platform paths.
	_, httpsPort, _ := net.SplitHostPort(addrs.HTTPS.String())
	if res, _ := get(t, addrs.HTTP, "docs.localhost", "/a?b=c"); res.StatusCode != http.StatusPermanentRedirect ||
		res.Header.Get("Location") != "https://docs.localhost:"+httpsPort+"/a?b=c" {
		t.Errorf("HTTP redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := get(t, addrs.HTTP, "127.0.0.1", "/"); res.StatusCode != 200 {
		t.Errorf("HTTP on loopback: %d", res.StatusCode)
	}
	if res, body := get(t, addrs.HTTP, "docs.localhost", "/.moorage/ca.crt"); res.StatusCode != 200 || body != string(caPEM) {
		t.Errorf("CA download: %d", res.StatusCode)
	}
}

func TestInternalTLSRefusesForeignRoot(t *testing.T) {
	cfg := tlsConfigFor(t, testConfig(t), "internal")
	_, stop := start(t, cfg)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	cfg.Domain, cfg.ConsoleHost = "apps.example.com", "moorage.apps.example.com"
	err := Run(context.Background(), cfg, quiet, Options{})
	if err == nil || !strings.Contains(err.Error(), "may not sign") {
		t.Errorf("a root generated for another domain: %v", err)
	}
}

// ca is a test certificate authority.
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T, name string, parent *ca, permitted ...string) *ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		PermittedDNSDomains:   permitted,
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &ca{cert, key}
}

func (c *ca) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

func (c *ca) write(t *testing.T, dir, name string) (certFile, keyFile string) {
	t.Helper()
	keyDER, _ := x509.MarshalECPrivateKey(c.key)
	certFile, keyFile = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	os.WriteFile(certFile, c.pem(), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

func TestInternalTLSWithCorporateIntermediate(t *testing.T) {
	root := newCA(t, "Corporate Root", nil)
	intermediate := newCA(t, "Moorage Intermediate", root, "localhost")
	certFile, keyFile := intermediate.write(t, t.TempDir(), "intermediate")

	cfg := tlsConfigFor(t, testConfig(t), "internal")
	cfg.TLS.CACert, cfg.TLS.CAKey = certFile, keyFile
	addrs, _ := start(t, cfg)
	// Clients trust only the corporate root; the server sends the
	// intermediate after its certificate.
	res, _, err := fetchTLS(t, addrs.HTTPS, root.pem(), "moorage.localhost", "/.moorage/health")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || len(res.TLS.PeerCertificates) != 2 {
		t.Errorf("status %d, chain of %d certificates", res.StatusCode, len(res.TLS.PeerCertificates))
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "certs", "ca.key")); err == nil {
		t.Error("a root was generated although an intermediate was configured")
	}
}

func TestFilesTLS(t *testing.T) {
	// A self-signed wildcard certificate stands in for a corporate one.
	wildcard := newCA(t, "wildcard", nil)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "*.localhost"},
		DNSNames:     []string{"*.localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, wildcard.cert, &key.PublicKey, wildcard.key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "wildcard.crt"), filepath.Join(dir, "wildcard.key")
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)

	cfg := tlsConfigFor(t, testConfig(t), "files")
	cfg.TLS.Certificates = []config.Certificate{{Cert: certFile, Key: keyFile}}
	addrs, _ := start(t, cfg)
	res, _, err := fetchTLS(t, addrs.HTTPS, wildcard.pem(), "moorage.localhost", "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.TLS.PeerCertificates[0].Subject.CommonName != "*.localhost" {
		t.Errorf("files mode: %d %s", res.StatusCode, res.TLS.PeerCertificates[0].Subject.CommonName)
	}
}

// TestBehindTLSProxy stands in for nginx: a TLS-terminating reverse proxy
// that keeps the Host header forwards to Moorage's plain HTTP listener.
func TestBehindTLSProxy(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy = config.Proxy{Trusted: []string{"127.0.0.1"}, ExternalScheme: "https"}
	addrs, _ := start(t, cfg)

	backend := &url.URL{Scheme: "http", Host: addrs.HTTP.String()}
	proxy := httptest.NewTLSServer(&httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(backend)
			r.Out.Host = r.In.Host // keep the Host header, as nginx does with proxy_set_header Host $host
			r.SetXForwarded()
		},
	})
	defer proxy.Close()

	c := apiClient(t, cfg, addrs)
	site := writeSite(t, map[string]string{"index.html": "via proxy"})
	d, _, err := c.DeployStatic(context.Background(), "docs", "production", site, manifest.Static{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.URL != "https://docs.localhost" {
		t.Errorf("URL behind a TLS proxy = %q, want https without a port", d.URL)
	}
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Host = "docs.localhost"
	res, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "via proxy" {
		t.Errorf("through the proxy: %d %q", res.StatusCode, body)
	}
	// Moorage does not redirect to HTTPS itself behind a proxy.
	if res, _ := get(t, addrs.HTTP, "docs.localhost", "/"); res.StatusCode != 200 {
		t.Errorf("plain HTTP behind a proxy: %d", res.StatusCode)
	}
}

func TestHSTS(t *testing.T) {
	cfg := tlsConfigFor(t, testConfig(t), "internal")
	cfg.TLS.HSTS = true
	addrs, _ := start(t, cfg)
	caPEM, err := os.ReadFile(filepath.Join(cfg.DataDir, "certs", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	if _, _, err := c.DeployStatic(ctx, "docs", "production", writeSite(t, map[string]string{"index.html": "plain"}), manifest.Static{}, nil); err != nil {
		t.Fatal(err)
	}
	own := manifest.Static{Headers: []manifest.HeaderRule{{For: "*", Values: map[string]string{"Strict-Transport-Security": "max-age=60"}}}}
	if _, _, err := c.DeployStatic(ctx, "own", "production", writeSite(t, map[string]string{"index.html": "plain"}), own, nil); err != nil {
		t.Fatal(err)
	}
	for _, try := range []struct {
		host, path string
		want       []string
	}{
		{"moorage.localhost", "/.moorage/health", []string{"max-age=31536000"}},
		{"moorage.localhost", "/", []string{"max-age=31536000"}},
		{"docs.localhost", "/", []string{"max-age=31536000"}},
		{"nothing-here.localhost", "/", []string{"max-age=31536000"}},
		{"own.localhost", "/", []string{"max-age=60"}},
	} {
		res, _, err := fetchTLS(t, addrs.HTTPS, caPEM, try.host, try.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Header.Values("Strict-Transport-Security"); !slices.Equal(got, try.want) {
			t.Errorf("%s%s: %q, want %q", try.host, try.path, got, try.want)
		}
	}
	// Not over plain HTTP, where a browser would be right to ignore it.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/", nil)
	req.Host = "docs.localhost"
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Strict-Transport-Security") != "" {
		t.Errorf("over plain HTTP: %q", res.Header.Get("Strict-Transport-Security"))
	}
}
