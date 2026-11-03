package ingress

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
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
)

// selfSigned writes a self-signed certificate for name to dir.
func selfSigned(t *testing.T, dir, name string) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "site.crt"), filepath.Join(dir, "site.key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestFileCertificatesReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := selfSigned(t, dir, "one.example.com")
	fc, err := NewFileCertificates([]config.Certificate{{Cert: certFile, Key: keyFile}}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	cn := func() string {
		c, err := fc.GetCertificate(&tls.ClientHelloInfo{ServerName: "one.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		return c.Leaf.Subject.CommonName
	}
	if cn() != "one.example.com" {
		t.Fatalf("serving %q", cn())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fc.Watch(ctx, 10*time.Millisecond)

	// A broken file is ignored; the old certificate keeps being served.
	os.WriteFile(certFile, []byte("garbage"), 0o644)
	later := time.Now().Add(time.Minute)
	os.Chtimes(certFile, later, later)
	time.Sleep(50 * time.Millisecond)
	if cn() != "one.example.com" {
		t.Errorf("a broken file replaced the certificate")
	}

	selfSigned(t, dir, "two.example.com")
	later = later.Add(time.Minute)
	os.Chtimes(certFile, later, later)
	os.Chtimes(keyFile, later, later)
	deadline := time.Now().Add(5 * time.Second)
	for cn() != "two.example.com" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cn() != "two.example.com" {
		t.Errorf("changed files were not reloaded; serving %q", cn())
	}
}

func TestInternalCAIssuesAndRenews(t *testing.T) {
	cfg := config.Config{Domain: "apps.example.com", ConsoleHost: "deploy.example.org"}
	allowed := map[string]bool{"docs.apps.example.com": true, "deploy.example.org": true}
	ca, created, err := LoadInternalCA(t.TempDir(), cfg, func(n string) bool { return allowed[n] })
	if err != nil || !created {
		t.Fatalf("LoadInternalCA: created %v, %v", created, err)
	}
	// The generated root is limited to the domain and the console host.
	if len(ca.cert.PermittedDNSDomains) != 2 || !ca.cert.PermittedDNSDomainsCritical {
		t.Errorf("name constraints: %v", ca.cert.PermittedDNSDomains)
	}

	hello := &tls.ClientHelloInfo{ServerName: "Docs.Apps.Example.com."}
	first, err := ca.GetCertificate(hello)
	if err != nil {
		t.Fatal(err)
	}
	if first.Leaf.DNSNames[0] != "docs.apps.example.com" {
		t.Errorf("issued for %v", first.Leaf.DNSNames)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := first.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "docs.apps.example.com"}); err != nil {
		t.Errorf("issued certificate does not verify: %v", err)
	}
	if again, _ := ca.GetCertificate(hello); again != first {
		t.Error("a valid certificate was issued again instead of reused")
	}
	ca.now = func() time.Time { return time.Now().Add(25 * 24 * time.Hour) }
	if renewed, _ := ca.GetCertificate(hello); renewed == first {
		t.Error("a certificate close to expiry was not renewed")
	}
	if _, err := ca.GetCertificate(&tls.ClientHelloInfo{ServerName: "other.example.net"}); err == nil {
		t.Error("issued a certificate for a name that is not allowed")
	}
	if _, err := ca.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Error("issued a certificate without a server name")
	}
}
