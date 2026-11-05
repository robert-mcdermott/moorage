package ingress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mholt/acmez/v3/acme"
	"go.uber.org/zap"

	"github.com/robert-mcdermott/moorage/internal/config"
)

func acmeConfig() config.Config {
	return config.Config{DataDir: "/data", Domain: "apps.example.com", ConsoleHost: "moorage.apps.example.com", Routing: "host",
		Listen: config.Listen{HTTP: ":80", HTTPS: ":443"}, TLS: config.TLS{Mode: "acme", ACME: config.ACME{AgreeTerms: true}}}
}

// A certificate is not asked for because someone connected and made up a hostname: a CA limits how many a domain can have.
func TestAllowIssuing(t *testing.T) {
	rt := &Router{cfg: acmeConfig()}
	rt.table.Store(&routeTable{
		routes:   map[string]route{"shop.apps.example.com": {}, "shop--staging.apps.example.com": {}, "portal.example.org": {}},
		prefixed: map[string][]route{"www.example.org": {{prefix: "/docs"}}},
		apps:     map[string]bool{"shop": true, "empty": true},
	})
	for name, want := range map[string]bool{
		"moorage.apps.example.com":              true,
		"shop.apps.example.com":                 true,
		"shop--staging.apps.example.com":        true,
		"portal.example.org":                    true,
		"www.example.org":                       true,
		"empty.apps.example.com":                true, // an app with nothing deployed yet has a page to say so
		"made-up.apps.example.com":              false,
		"shop--nothing.apps.example.com":        false,
		"shop--production--v3.apps.example.com": false,
		"apps.example.com":                      false,
		"a.b.apps.example.com":                  false,
		"example.org":                           false,
		"evil.example.net":                      false,
	} {
		if got := rt.AllowIssuing(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
	rt.cfg.Routing = "path"
	if !rt.AllowIssuing("apps.example.com") {
		t.Error("with path routing, the domain is what is served")
	}
}

func newTestACME(t *testing.T, change func(*config.Config)) *ACME {
	t.Helper()
	cfg := acmeConfig()
	if change != nil {
		change(&cfg)
	}
	m, err := NewACME(ACMEOptions{Dir: t.TempDir(), Config: cfg, Allow: func(string) bool { return true }, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.cache.Stop)
	return m
}

func TestACMEChallengesAndCoverage(t *testing.T) {
	both := newTestACME(t, nil)
	if !strings.Contains(strings.Join(both.TLSConfig().NextProtos, " "), "acme-tls/1") {
		t.Error("TLS-ALPN-01 needs its protocol offered")
	}
	httpOnly := newTestACME(t, func(c *config.Config) { c.TLS.ACME.Challenges = []string{"http-01"} })
	if strings.Contains(strings.Join(httpOnly.TLSConfig().NextProtos, " "), "acme-tls/1") {
		t.Error("TLS-ALPN-01 is offered with only HTTP-01 asked for")
	}
	if !httpOnly.issuer.DisableTLSALPNChallenge || httpOnly.issuer.DisableHTTPChallenge {
		t.Errorf("%+v", httpOnly.issuer)
	}
	for name, want := range map[string]bool{
		"shop.apps.example.com":         true,
		"SHOP.apps.example.com.":        true,
		"apps.example.com":              false,
		"a.shop.apps.example.com":       false,
		"shop.apps.example.com.evil.io": false,
		"portal.example.org":            false,
	} {
		if got := both.covered(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
	// Listening elsewhere than the ports a CA looks at, what answers is still this server.
	moved := newTestACME(t, func(c *config.Config) { c.Listen = config.Listen{HTTP: "10.0.0.1:8080", HTTPS: "10.0.0.1:8443"} })
	if moved.issuer.AltHTTPPort != 8080 || moved.issuer.AltTLSALPNPort != 8443 || moved.issuer.ListenHost != "10.0.0.1" {
		t.Errorf("%+v", moved.issuer)
	}
}

func TestACMENeedsItsFiles(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	for name, change := range map[string]func(*config.Config){
		"ca file":    func(c *config.Config) { c.TLS.ACME.CAFile = missing },
		"token file": func(c *config.Config) { c.TLS.ACME.DNS = config.ACMEDNS{Provider: "cloudflare", TokenFile: missing} },
		"key file": func(c *config.Config) {
			c.TLS.ACME.DNS = config.ACMEDNS{Provider: "rfc2136", Server: "ns:53", KeyName: "k", KeyFile: missing}
		},
	} {
		cfg := acmeConfig()
		change(&cfg)
		if _, err := NewACME(ACMEOptions{Dir: t.TempDir(), Config: cfg, Allow: func(string) bool { return true }, Log: slog.New(slog.DiscardHandler)}); err == nil {
			t.Errorf("%s: a file that is not there is not refused", name)
		}
	}
}

func TestACMEToldOnce(t *testing.T) {
	type said struct {
		failed bool
		name   string
	}
	var heard []said
	cfg := acmeConfig()
	m, err := NewACME(ACMEOptions{Dir: t.TempDir(), Config: cfg, Allow: func(string) bool { return true }, Log: slog.New(slog.DiscardHandler),
		Told: func(failed bool, name, _ string) { heard = append(heard, said{failed, name}) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.cache.Stop)
	ctx := context.Background()
	failed := map[string]any{"identifier": "a.example.com", "error": "no"}
	m.heard(ctx, "cert_failed", failed)
	m.heard(ctx, "cert_failed", failed)
	m.heard(ctx, "cert_failed", failed)
	if len(heard) != 1 || !heard[0].failed {
		t.Errorf("three failures in a row: %+v", heard)
	}
	// Nobody needs telling that what never failed has worked.
	m.heard(ctx, "cert_obtained", map[string]any{"identifier": "b.example.com"})
	if len(heard) != 1 {
		t.Errorf("%+v", heard)
	}
	m.heard(ctx, "cert_obtained", map[string]any{"identifier": "a.example.com"})
	if len(heard) != 2 || heard[1].failed || heard[1].name != "a.example.com" {
		t.Errorf("%+v", heard)
	}
	// It can fail again, and then it is news again.
	m.heard(ctx, "cert_failed", failed)
	if len(heard) != 3 {
		t.Errorf("%+v", heard)
	}
}

func TestCommandSolver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is a shell script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := filepath.Join(dir, "set-dns")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1 $2 $3\" >> "+log+"\n[ \"$2\" != _acme-challenge.fails.example.com ] || { echo it would not; exit 3; }\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &commandSolver{command: script, wait: time.Millisecond}
	ctx := context.Background()
	challenge := acme.Challenge{Token: "abc", KeyAuthorization: "abc.def", Identifier: acme.Identifier{Type: "dns", Value: "example.com"}}
	if err := s.Present(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanUp(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "add _acme-challenge.example.com " + challenge.DNS01KeyAuthorization() + "\nremove _acme-challenge.example.com " + challenge.DNS01KeyAuthorization() + "\n"
	if string(data) != want {
		t.Errorf("%q, not %q", data, want)
	}
	challenge.Identifier.Value = "fails.example.com"
	if err := s.Present(ctx, challenge); err == nil || !strings.Contains(err.Error(), "it would not") {
		t.Errorf("what the program said is what is wanted: %v", err)
	}
	waiting, cancel := context.WithCancel(ctx)
	cancel()
	if err := (&commandSolver{command: script, wait: time.Hour}).Wait(waiting, challenge); err == nil {
		t.Error("waiting did not end with its context")
	}
}

func TestACMECertificates(t *testing.T) {
	dir := t.TempDir()
	if found, err := ACMECertificates(dir); err != nil || len(found) != 0 {
		t.Fatalf("%v %v", found, err)
	}
	write := func(name string, after time.Duration) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now(), NotAfter: time.Now().Add(after)}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		at := filepath.Join(dir, "certificates", "acme-example", name)
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(at, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			t.Fatal(err)
		}
		// Beside it, what is not a certificate.
		if err := os.WriteFile(filepath.Join(at, name+".key"), []byte("not for here"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("later.example.com", 60*24*time.Hour)
	write("sooner.example.com", 5*24*time.Hour)
	found, err := ACMECertificates(dir)
	if err != nil || len(found) != 2 || found[0].Names[0] != "sooner.example.com" {
		t.Fatalf("%+v %v", found, err)
	}
}

func TestSlogCore(t *testing.T) {
	var b strings.Builder
	logger := zap.New(slogCore{log: slog.New(slog.NewTextHandler(&b, nil))}).With(zap.String("kept", "yes"))
	logger.Info("each step")
	if b.Len() != 0 {
		t.Errorf("each step should not reach the server's log: %s", b.String())
	}
	logger.Warn("something to know", zap.String("name", "a.example.com"))
	for _, want := range []string{"level=WARN", "acme: something to know", "name=a.example.com", "kept=yes"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("%s: no %q", b.String(), want)
		}
	}
}
