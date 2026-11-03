package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
)

func testCfg(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Preset("local", config.PresetOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen.HTTP = "127.0.0.1:0"
	cfg.Path = "/etc/moorage/server.toml"
	return cfg
}

func find(checks []Check, name string) (Check, bool) {
	i := slices.IndexFunc(checks, func(c Check) bool { return c.Name == name })
	if i < 0 {
		return Check{}, false
	}
	return checks[i], true
}

func mustFind(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	c, ok := find(checks, name)
	if !ok {
		t.Fatalf("no check %q in %+v", name, checks)
	}
	return c
}

func want(t *testing.T, c Check, status, detail string) {
	t.Helper()
	if c.Status != status || !strings.Contains(c.Detail, detail) {
		t.Errorf("%s: %s %q, want %s with %q", c.Name, c.Status, c.Detail, status, detail)
	}
}

func TestServerNotRunning(t *testing.T) {
	cfg := testCfg(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg.Listen.HTTP = addr
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg.Listen.SSH = busy.Addr().String()

	checks := Run(context.Background(), Options{Config: cfg, Lookup: lookupNothing})
	want(t, mustFind(t, checks, "server"), Warn, "not running")
	want(t, mustFind(t, checks, "listen.http"), OK, "free")
	want(t, mustFind(t, checks, "listen.ssh"), Fail, "something else is listening")
	if Worst(checks) != Fail {
		t.Errorf("worst: %s", Worst(checks))
	}
}

func lookupNothing(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }

func healthServer(t *testing.T, body string, code int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestServerAnswers(t *testing.T) {
	for _, c := range []struct {
		body         string
		code         int
		status, says string
		running      bool
	}{
		{`{"status":"ok","version":"` + buildinfo.Version + `"}`, 200, OK, "running " + buildinfo.Version, true},
		{`{"status":"ok","version":"0.0.1-other"}`, 200, Warn, "and this moor is", true},
		{`{"status":"database unavailable","version":"` + buildinfo.Version + `"}`, 503, Fail, "not healthy", true},
		{`<html>a different server</html>`, 200, Fail, "something other than Moorage", false},
	} {
		cfg := testCfg(t)
		cfg.Listen.HTTP = healthServer(t, c.body, c.code)
		running, checks := checkServer(context.Background(), Options{Config: cfg})
		if running != c.running {
			t.Errorf("%s: running = %v", c.body, running)
		}
		want(t, checks[0], c.status, c.says)
	}
}

func TestDNS(t *testing.T) {
	lookup := func(answers map[string][]string) func(context.Context, string) ([]string, error) {
		return func(_ context.Context, host string) ([]string, error) {
			for suffix, addrs := range answers {
				if strings.HasSuffix(host, suffix) {
					return addrs, nil
				}
			}
			return nil, errors.New("no such host")
		}
	}
	addrs := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.10"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	cfg := testCfg(t)
	cfg.Domain, cfg.ConsoleHost = "apps.test", "moorage.apps.test"

	// Everything resolves, to this machine.
	checks := checkDNS(context.Background(), Options{Config: cfg, GOOS: "linux", AddrsFor: addrs, Lookup: lookup(map[string][]string{"apps.test": {"192.0.2.10"}})})
	want(t, checks[0], OK, "*.apps.test → 192.0.2.10")

	// To somewhere else.
	checks = checkDNS(context.Background(), Options{Config: cfg, GOOS: "linux", AddrsFor: addrs, Lookup: lookup(map[string][]string{"apps.test": {"198.51.100.1"}})})
	want(t, checks[0], Warn, "not an address of this machine")

	// The console has a record; the wildcard does not.
	checks = checkDNS(context.Background(), Options{Config: cfg, GOOS: "linux", AddrsFor: addrs, Lookup: lookup(map[string][]string{"moorage.apps.test": {"192.0.2.10"}})})
	c := mustFind(t, checks, "dns (apps)")
	if c.Status != Fail || !strings.Contains(c.Fix, "wildcard") {
		t.Errorf("%+v", c)
	}
	want(t, mustFind(t, checks, "dns (console)"), OK, "moorage.apps.test")

	// Behind a proxy, the address is the proxy's.
	proxied := cfg
	proxied.Proxy.Trusted = []string{"127.0.0.1/32"}
	checks = checkDNS(context.Background(), Options{Config: proxied, GOOS: "linux", AddrsFor: addrs, Lookup: lookup(map[string][]string{"apps.test": {"198.51.100.1"}})})
	want(t, checks[0], OK, "198.51.100.1")

	// *.localhost.
	cfg = testCfg(t)
	checks = checkDNS(context.Background(), Options{Config: cfg, GOOS: "darwin", Lookup: lookupNothing})
	if checks[0].Status != Info || !strings.Contains(checks[0].Fix, "moor server resolver install") {
		t.Errorf("%+v", checks[0])
	}
	checks = checkDNS(context.Background(), Options{Config: cfg, GOOS: "darwin", Lookup: lookup(map[string][]string{"localhost": {"127.0.0.1"}})})
	want(t, checks[0], OK, "resolves")
}

// certificate writes a certificate and its key; signer is nil for a CA of its own.
func certificate(t *testing.T, dir, name string, notAfter time.Time, isCA bool, names ...string) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: names,
		NotBefore: notAfter.Add(-24 * 365 * time.Hour), NotAfter: notAfter,
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ = x509.ParseCertificate(der)
	certFile, keyFile = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile, cert
}

func TestCertificateFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	base := testCfg(t)
	base.Domain, base.ConsoleHost = "apps.test", "moorage.apps.test"
	base.TLS.Mode = "files"
	opts := func(pairs ...config.Certificate) Options {
		cfg := base
		cfg.TLS.Certificates = pairs
		return Options{Config: cfg, Now: time.Now}
	}
	pair := func(name string, notAfter time.Time, names ...string) config.Certificate {
		c, k, _ := certificate(t, dir, name, notAfter, false, names...)
		return config.Certificate{Cert: c, Key: k}
	}

	good := pair("good", now.AddDate(1, 0, 0), "*.apps.test")
	checks := checkCertFiles(opts(good))
	if len(checks) != 1 {
		t.Fatalf("%+v", checks)
	}
	want(t, checks[0], OK, "expires on")

	want(t, checkCertFiles(opts(pair("soon", now.Add(10*24*time.Hour), "*.apps.test")))[0], Warn, "expires in")
	want(t, checkCertFiles(opts(pair("old", now.Add(-time.Hour), "*.apps.test")))[0], Fail, "expired on")

	checks = checkCertFiles(opts(pair("other", now.AddDate(1, 0, 0), "*.other.test")))
	want(t, checks[1], Fail, "no certificate covers moorage.apps.test")
	want(t, checks[2], Fail, "no certificate covers any-app.apps.test")

	checks = checkCertFiles(opts(config.Certificate{Cert: filepath.Join(dir, "missing.crt"), Key: good.Key}))
	want(t, checks[0], Fail, "missing.crt")
	wrong := pair("wrong", now.AddDate(1, 0, 0), "*.apps.test")
	checks = checkCertFiles(opts(config.Certificate{Cert: good.Cert, Key: wrong.Key}))
	want(t, checks[0], Fail, "good.crt")
}

func TestInternalCA(t *testing.T) {
	cfg := testCfg(t)
	cfg.TLS.Mode, cfg.Domain, cfg.ConsoleHost = "internal", "apps.test", "moorage.apps.test"
	checks := checkInternalCA(context.Background(), Options{Config: cfg, Now: time.Now}, false)
	want(t, checks[0], Info, "first time it starts")

	certs := filepath.Join(cfg.DataDir, "certs")
	os.MkdirAll(certs, 0o700)
	_, _, ca := certificate(t, certs, "ca", time.Now().AddDate(5, 0, 0), true)
	checks = checkInternalCA(context.Background(), Options{Config: cfg, Now: time.Now}, false)
	want(t, mustFind(t, checks, "tls (ca)"), OK, "SHA-256")
	if c := mustFind(t, checks, "tls (trust)"); c.Status != Info || !strings.Contains(c.Fix, "moor trust") {
		t.Errorf("%+v", c)
	}
	// One close to its end.
	soon := Options{Config: cfg, Now: func() time.Time { return ca.NotAfter.Add(-30 * 24 * time.Hour) }}
	want(t, mustFind(t, checkInternalCA(context.Background(), soon, false), "tls (ca)"), Warn, "expires in")
}

func TestPlainHTTP(t *testing.T) {
	cfg := testCfg(t)
	want(t, checkTLS(context.Background(), Options{Config: cfg}, false)[0], OK, "this computer only")

	cfg.Domain, cfg.ConsoleHost = "apps.test", "moorage.apps.test"
	want(t, checkTLS(context.Background(), Options{Config: cfg}, false)[0], Warn, "unencrypted")

	cfg.Proxy.Trusted, cfg.Proxy.ExternalScheme = []string{"127.0.0.1/32"}, "https"
	want(t, checkTLS(context.Background(), Options{Config: cfg}, false)[0], OK, "behind a TLS proxy")

	cfg.Listen.HTTP = "0.0.0.0:8080"
	want(t, checkTLS(context.Background(), Options{Config: cfg}, false)[0], Info, "open to more than this machine")

	cfg.Listen.HTTP = "127.0.0.1:8080"
	cfg.Proxy.ExternalScheme = "http"
	want(t, checkTLS(context.Background(), Options{Config: cfg}, false)[0], Warn, "external_scheme")
}

func TestData(t *testing.T) {
	cfg := testCfg(t)
	want(t, checkData(Options{Config: cfg, GOOS: runtime.GOOS})[0], Info, "no database yet")

	os.WriteFile(filepath.Join(cfg.DataDir, "moorage.db"), make([]byte, 2<<20), 0o600)
	keys := filepath.Join(cfg.DataDir, "keys")
	os.MkdirAll(keys, 0o755)
	os.WriteFile(filepath.Join(keys, "master.key"), []byte("x"), 0o600)
	checks := checkData(Options{Config: cfg, GOOS: runtime.GOOS})
	want(t, checks[0], OK, "2.0 MiB")
	if runtime.GOOS != "windows" {
		want(t, mustFind(t, checks, "keys"), Fail, "can be read by others")
		os.Chmod(keys, 0o700)
		checks = checkData(Options{Config: cfg, GOOS: runtime.GOOS})
	}
	c := mustFind(t, checks, "keys")
	if c.Status != Info || !strings.Contains(c.Fix, "back it up") {
		t.Errorf("%+v", c)
	}
}

func TestDisk(t *testing.T) {
	const gib = 1 << 30
	for _, c := range []struct {
		free, total uint64
		status      string
	}{
		{100 * gib, 200 * gib, OK},
		{4 * gib, 200 * gib, Warn},
		{60 * gib, 900 * gib, OK},
		{gib / 2, 100 * gib, Fail},
	} {
		if got := judgeDisk("/data", c.free, c.total); got.Status != c.status {
			t.Errorf("%+v: %+v", c, got)
		}
	}
	cfg := testCfg(t)
	cfg.DataDir = filepath.Join(cfg.DataDir, "not", "there", "yet")
	if got := checkDisk(Options{Config: cfg}); !strings.Contains(got.Detail, "free of") {
		t.Errorf("%+v", got)
	}
}

func TestClock(t *testing.T) {
	command := func(out string, err error) func(context.Context, string, ...string) ([]byte, error) {
		return func(context.Context, string, ...string) ([]byte, error) { return []byte(out), err }
	}
	if c, ok := checkClock(context.Background(), Options{GOOS: "linux", Command: command("yes\n", nil)}); !ok || c.Status != OK {
		t.Errorf("%+v", c)
	}
	if c, ok := checkClock(context.Background(), Options{GOOS: "linux", Command: command("no\n", nil)}); !ok || c.Status != Warn {
		t.Errorf("%+v", c)
	}
	if _, ok := checkClock(context.Background(), Options{GOOS: "linux", Command: command("", errors.New("no such tool"))}); ok {
		t.Error("a check it cannot make")
	}
	if _, ok := checkClock(context.Background(), Options{GOOS: "darwin"}); ok {
		t.Error("a check it cannot make")
	}
}

func TestKernel(t *testing.T) {
	root := t.TempDir()
	want(t, checkKernel(Options{Root: root}), Info, "no cgroup v2, no Landlock")
	os.MkdirAll(filepath.Join(root, "sys", "fs", "cgroup"), 0o755)
	os.MkdirAll(filepath.Join(root, "sys", "kernel", "security"), 0o755)
	os.WriteFile(filepath.Join(root, "sys", "fs", "cgroup", "cgroup.controllers"), []byte("cpu memory pids\n"), 0o644)
	os.WriteFile(filepath.Join(root, "sys", "kernel", "security", "lsm"), []byte("lockdown,landlock,yama\n"), 0o644)
	want(t, checkKernel(Options{Root: root}), OK, "cgroup v2 (cpu, memory, pids), Landlock")
}

func TestContainers(t *testing.T) {
	cfg := testCfg(t)
	cfg.Containers.Engine = "no-such-engine"
	if c := checkContainers(context.Background(), Options{Config: cfg, GOOS: "linux"}); c.Status != Fail {
		t.Errorf("%+v", c)
	}
	t.Setenv("PATH", t.TempDir())
	cfg.Containers.Engine = "auto"
	if c := checkContainers(context.Background(), Options{Config: cfg, GOOS: "linux"}); c.Status != Info || !strings.Contains(c.Fix, "install Podman or Docker") {
		t.Errorf("%+v", c)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// One that does not answer.
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in --version) echo podman version 5 ;; *) echo cannot connect >&2; exit 125 ;; esac\n"), 0o755)
	cfg.Containers.Engine = bin
	c := checkContainers(context.Background(), Options{Config: cfg, GOOS: "darwin"})
	if c.Status != Fail || !strings.Contains(c.Detail, "cannot connect") || c.Fix != "podman machine start" {
		t.Errorf("%+v", c)
	}
	os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in --version) echo podman version 5 ;; esac\n"), 0o755)
	if c := checkContainers(context.Background(), Options{Config: cfg, GOOS: "darwin"}); c.Status != OK || !strings.Contains(c.Detail, "podman version 5") {
		t.Errorf("%+v", c)
	}
}

func TestAppsBeyondLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	reachable := func() ([]net.Addr, error) {
		// 127.0.0.2 stands in for an address on the network; on Linux it reaches
		// only what listens on all addresses, and elsewhere it may not exist.
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.99"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	ports := func(context.Context) ([]int, error) { return []int{port}, nil }
	c, ok := checkApps(context.Background(), Options{InstancePorts: ports, AddrsFor: reachable})
	if !ok || c.Status != OK {
		t.Errorf("%v %+v", ok, c)
	}
	// One that listens everywhere is found on this machine's own address.
	all, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer all.Close()
	addrs, _ := net.InterfaceAddrs()
	var own net.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() && n.IP.To4() != nil {
			own = a
			break
		}
	}
	if own == nil {
		t.Skip("this machine has no address but loopback")
	}
	openPort := all.Addr().(*net.TCPAddr).Port
	c, ok = checkApps(context.Background(), Options{
		InstancePorts: func(context.Context) ([]int, error) { return []int{openPort}, nil },
		AddrsFor:      func() ([]net.Addr, error) { return []net.Addr{own}, nil },
	})
	if !ok || c.Status != Warn || !strings.Contains(c.Fix, "$HOST") {
		t.Errorf("%v %+v", ok, c)
	}
}
