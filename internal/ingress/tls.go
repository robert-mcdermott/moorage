package ingress

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robert-mcdermott/moorage/internal/config"
)

// Certificates chooses the certificate for each TLS handshake.
type Certificates interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

// TLSConfig returns the server TLS configuration for certs.
func TLSConfig(certs Certificates) *tls.Config {
	return &tls.Config{
		GetCertificate: certs.GetCertificate,
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

// FileCertificates serves certificate and key files (tls.mode "files"),
// choosing among them by server name, and reloads them when they change.
type FileCertificates struct {
	pairs  []config.Certificate
	log    *slog.Logger
	certs  atomic.Pointer[[]*tls.Certificate]
	mtimes []time.Time
}

// NewFileCertificates loads every pair; any failure is an error.
func NewFileCertificates(pairs []config.Certificate, log *slog.Logger) (*FileCertificates, error) {
	f := &FileCertificates{pairs: pairs, log: log}
	certs, mtimes, err := loadPairs(pairs)
	if err != nil {
		return nil, err
	}
	f.certs.Store(&certs)
	f.mtimes = mtimes
	return f, nil
}

func loadPairs(pairs []config.Certificate) ([]*tls.Certificate, []time.Time, error) {
	var certs []*tls.Certificate
	var mtimes []time.Time
	for _, p := range pairs {
		c, err := tls.LoadX509KeyPair(p.Cert, p.Key)
		if err != nil {
			return nil, nil, fmt.Errorf("load certificate %s: %w", p.Cert, err)
		}
		certs = append(certs, &c)
		for _, name := range []string{p.Cert, p.Key} {
			fi, err := os.Stat(name)
			if err != nil {
				return nil, nil, err
			}
			mtimes = append(mtimes, fi.ModTime())
		}
	}
	return certs, mtimes, nil
}

// GetCertificate returns the first certificate valid for the requested
// name, or the first certificate when none is.
func (f *FileCertificates) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	certs := *f.certs.Load()
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	for _, c := range certs {
		if c.Leaf != nil && c.Leaf.VerifyHostname(name) == nil {
			return c, nil
		}
	}
	return certs[0], nil
}

// Watch reloads the files when any of them changes, checking every interval
// until ctx ends. A reload that fails keeps the certificates in use.
func (f *FileCertificates) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		changed := false
		i := 0
		for _, p := range f.pairs {
			for _, name := range []string{p.Cert, p.Key} {
				if fi, err := os.Stat(name); err == nil && !fi.ModTime().Equal(f.mtimes[i]) {
					changed = true
				}
				i++
			}
		}
		if !changed {
			continue
		}
		certs, mtimes, err := loadPairs(f.pairs)
		if err != nil {
			f.log.Error("certificate files changed but could not be loaded; keeping the previous ones", "err", err)
			continue
		}
		f.certs.Store(&certs)
		f.mtimes = mtimes
		f.log.Info("certificate files reloaded")
	}
}

// leafLifetime is how long issued certificates are valid; they are renewed
// when less than renewBefore remains.
const (
	leafLifetime = 30 * 24 * time.Hour
	renewBefore  = 7 * 24 * time.Hour
	maxCached    = 10000
)

// InternalCA issues certificates on demand (tls.mode "internal"), from a root
// it generated itself or from an intermediate CA the operator provided.
type InternalCA struct {
	cert  *x509.Certificate
	key   crypto.Signer
	chain [][]byte // certificates sent after the leaf
	pem   []byte
	allow func(name string) bool
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// LoadInternalCA loads the CA named by tls.ca_cert and tls.ca_key or, without
// them, the root in certsDir, generating that root on first use. The
// generated root is name-constrained to the base domain and the console's
// hostname. allow decides which names get certificates. created reports a
// newly generated root.
func LoadInternalCA(certsDir string, c config.Config, allow func(string) bool) (ca *InternalCA, created bool, err error) {
	ca = &InternalCA{allow: allow, now: time.Now, cache: map[string]*tls.Certificate{}}
	if c.TLS.CACert != "" {
		certs, err := readCertificates(c.TLS.CACert)
		if err != nil {
			return nil, false, err
		}
		key, err := readKey(c.TLS.CAKey)
		if err != nil {
			return nil, false, err
		}
		ca.cert = certs[0]
		ca.key = key
		for _, crt := range certs {
			ca.chain = append(ca.chain, crt.Raw)
		}
	} else {
		certFile, keyFile := filepath.Join(certsDir, "ca.crt"), filepath.Join(certsDir, "ca.key")
		if _, err := os.Stat(certFile); errors.Is(err, os.ErrNotExist) {
			if err := generateRoot(certFile, keyFile, c); err != nil {
				return nil, false, fmt.Errorf("generate the internal CA: %w", err)
			}
			created = true
		}
		certs, err := readCertificates(certFile)
		if err != nil {
			return nil, false, err
		}
		key, err := readKey(keyFile)
		if err != nil {
			return nil, false, err
		}
		ca.cert, ca.key = certs[0], key
	}
	if !ca.cert.IsCA || ca.cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, false, fmt.Errorf("the certificate %s is not a CA that can sign certificates", ca.cert.Subject)
	}
	if !publicKeysEqual(ca.cert.PublicKey, ca.key.Public()) {
		return nil, false, errors.New("the internal CA's key does not match its certificate")
	}
	for _, name := range []string{"probe." + c.Domain, c.ConsoleHost} {
		if !permits(ca.cert, name) {
			return nil, false, fmt.Errorf("the internal CA %q may not sign certificates for %s (its name constraints are %v); "+
				"if Moorage generated it for another domain, delete ca.crt and ca.key in %s to generate a new one",
				ca.cert.Subject.CommonName, name, ca.cert.PermittedDNSDomains, certsDir)
		}
	}
	ca.pem = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
	return ca, created, nil
}

// PEM returns the CA certificate, for users to trust.
func (ca *InternalCA) PEM() []byte { return ca.pem }

// GetCertificate returns a certificate for the requested name, issuing one
// when there is none or it is due for renewal.
func (ca *InternalCA) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if name == "" {
		return nil, errors.New("no server name in the TLS handshake; use a hostname, not an IP address")
	}
	if !ca.allow(name) {
		return nil, fmt.Errorf("no certificate for %q", name)
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	now := ca.now()
	if c := ca.cache[name]; c != nil && now.Before(c.Leaf.NotAfter.Add(-renewBefore)) {
		return c, nil
	}
	c, err := ca.issue(name, now)
	if err != nil {
		return nil, err
	}
	if len(ca.cache) >= maxCached {
		clear(ca.cache)
	}
	ca.cache[name] = c
	return c, nil
}

func (ca *InternalCA) issue(name string, now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(leafLifetime)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: append([][]byte{der}, ca.chain...), PrivateKey: key, Leaf: leaf}, nil
}

func generateRoot(certFile, keyFile string, c config.Config) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	permitted := []string{c.Domain}
	if c.ConsoleHost != c.Domain && !strings.HasSuffix(c.ConsoleHost, "."+c.Domain) {
		permitted = append(permitted, c.ConsoleHost)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "Moorage CA for " + c.Domain, Organization: []string{"Moorage"}},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         permitted,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// permits reports whether a CA's name constraints allow name.
func permits(ca *x509.Certificate, name string) bool {
	if len(ca.PermittedDNSDomains) == 0 {
		return true
	}
	for _, d := range ca.PermittedDNSDomains {
		d = strings.TrimPrefix(d, ".")
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

func readCertificates(file string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("%s contains no PEM certificate", file)
	}
	return certs, nil
}

func readKey(file string) (crypto.Signer, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("%s contains no PEM private key", file)
		}
		var key any
		switch block.Type {
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("%s: unsupported key type %T", file, key)
		}
		return signer, nil
	}
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	e, ok := a.(equaler)
	return ok && e.Equal(b)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n.Add(n, big.NewInt(1))
}
