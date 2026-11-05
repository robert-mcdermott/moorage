// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/layout"
)

func certificateOnDisk(t *testing.T, dir, name string, notAfter time.Time) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-90 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "certificates", "ca-directory", name)
	os.MkdirAll(site, 0o700)
	os.WriteFile(filepath.Join(site, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
}

// A certificate for each hostname is renewed when next wanted, and stays on disk once it is not, so that an old one says nothing.
func TestDoctorOnOldCertificates(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{DataDir: dataDir, Domain: "example.test"}
	cfg.TLS.Mode = "acme"
	cfg.Listen.HTTP, cfg.Listen.HTTPS = ":80", ":443"
	dir := filepath.Join(layout.New(dataDir).Certs, ingress.ACMEDir)
	certificateOnDisk(t, dir, "pr-7--shop.example.test", time.Now().Add(-48*time.Hour))
	certificateOnDisk(t, dir, "shop.example.test", time.Now().Add(60*24*time.Hour))
	tls := func() Check {
		for _, c := range checkACME(Options{Config: cfg}) {
			if c.Name == "tls" {
				return c
			}
		}
		t.Fatal("no check of certificates")
		return Check{}
	}
	if c := tls(); c.Status != OK || !strings.Contains(c.Detail, "shop.example.test") || strings.Contains(c.Detail, "pr-7") {
		t.Errorf("for each hostname: %+v", c)
	}

	// A wildcard is kept up by the server, so that an old one is a problem, and it is the newest that counts.
	cfg.TLS.ACME.DNS.Provider = "command"
	if c := tls(); c.Status != Warn || !strings.Contains(c.Fix, "*.example.test") {
		t.Errorf("no wildcard: %+v", c)
	}
	certificateOnDisk(t, dir, "*.example.test", time.Now().Add(5*24*time.Hour))
	if c := tls(); c.Status != Warn {
		t.Errorf("a wildcard about to expire: %+v", c)
	}
	os.RemoveAll(filepath.Join(dir, "certificates"))
	certificateOnDisk(t, dir, "*.example.test", time.Now().Add(60*24*time.Hour))
	if c := tls(); c.Status != OK {
		t.Errorf("a wildcard in good time: %+v", c)
	}
}
