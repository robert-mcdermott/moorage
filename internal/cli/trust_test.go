package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/truststore"
)

// testPKI is a CA and a certificate of a server's it issued.
type testPKI struct {
	ca     *x509.Certificate
	pem    []byte
	server tls.Certificate
}

func newPKI(t *testing.T, cn string, permitted ...string) testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		PermittedDNSDomains: permitted, PermittedDNSDomainsCritical: len(permitted) > 0,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return testPKI{ca: ca, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		server: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}
}

// caServer is a server that presents one PKI's certificate and offers another's CA.
func caServer(t *testing.T, presents, offers testPKI) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.moorage/ca.crt" {
			http.NotFound(w, r)
			return
		}
		w.Write(offers.pem)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{presents.server}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// fakeTrust records what moor trust does to the machine, which stays untouched.
func fakeTrust(t *testing.T) *[][]string {
	t.Helper()
	calls := &[][]string{}
	old := trustSystem
	trustSystem = func() truststore.System {
		return truststore.System{GOOS: "darwin", Home: t.TempDir(),
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			Run: func(name string, args ...string) error {
				*calls = append(*calls, append([]string{name}, args...))
				return nil
			}}
	}
	t.Cleanup(func() { trustSystem = old })
	t.Setenv("MOOR_CONFIG", filepath.Join(t.TempDir(), "client.toml"))
	t.Setenv("MOORAGE_CONFIG", filepath.Join(t.TempDir(), "none.toml"))
	return calls
}

func TestTrustFromAServer(t *testing.T) {
	calls := fakeTrust(t)
	pki := newPKI(t, "Moorage CA for test", "localhost")
	srv := caServer(t, pki, pki)
	fp := truststore.Fingerprint(pki.ca)

	// Nothing vouches for it, and nobody here can be asked.
	code, _, errOut := run(t, "trust", "--server", srv.URL, "--token", "x")
	if code == 0 || !strings.Contains(errOut, "--fingerprint") || len(*calls) != 0 {
		t.Errorf("unconfirmed: %d %q %v", code, errOut, *calls)
	}
	if !strings.Contains(errOut, fp) || !strings.Contains(errOut, "May sign for localhost, and nothing else") {
		t.Errorf("it does not describe the CA: %q", errOut)
	}

	// The wrong fingerprint.
	other := truststore.Fingerprint(newPKI(t, "another").ca)
	code, _, errOut = run(t, "trust", "--server", srv.URL, "--token", "x", "--fingerprint", other)
	if code == 0 || !strings.Contains(errOut, "not the one you gave") || len(*calls) != 0 {
		t.Errorf("wrong fingerprint: %d %q %v", code, errOut, *calls)
	}

	// The right one.
	code, out, errOut := run(t, "trust", "--server", srv.URL, "--token", "x", "--fingerprint", strings.ToLower(fp))
	if code != 0 || !strings.Contains(out, "Added it to your login keychain") {
		t.Errorf("right fingerprint: %d %q %q", code, out, errOut)
	}
	if len(*calls) != 1 || (*calls)[0][1] != "add-trusted-cert" {
		t.Errorf("%v", *calls)
	}

	// Print changes nothing; remove undoes.
	*calls = nil
	code, out, _ = run(t, "trust", "--server", srv.URL, "--token", "x", "--print")
	if code != 0 || !strings.Contains(out, "BEGIN CERTIFICATE") || len(*calls) != 0 {
		t.Errorf("print: %d %q %v", code, out, *calls)
	}
	code, out, _ = run(t, "trust", "--server", srv.URL, "--token", "x", "--remove", "--fingerprint", fp)
	if code != 0 || !strings.Contains(out, "Removed it") || len(*calls) != 2 {
		t.Errorf("remove: %d %q %v", code, out, *calls)
	}
}

func TestTrustRefusesAServerThatIsNotItsCA(t *testing.T) {
	calls := fakeTrust(t)
	// It presents a certificate from a CA other than the one it offers.
	srv := caServer(t, newPKI(t, "the real one"), newPKI(t, "the offered one"))
	code, _, errOut := run(t, "trust", "--server", srv.URL, "--token", "x")
	if code == 0 || !strings.Contains(errOut, "does not come from the CA it offers") || len(*calls) != 0 {
		t.Errorf("%d %q %v", code, errOut, *calls)
	}
}

func TestTrustAFile(t *testing.T) {
	calls := fakeTrust(t)
	pki := newPKI(t, "A corporate root")
	file := filepath.Join(t.TempDir(), "root.pem")
	os.WriteFile(file, pki.pem, 0o644)
	code, out, errOut := run(t, "trust", "--file", file)
	if code != 0 || !strings.Contains(out, "Added it") || len(*calls) != 1 {
		t.Errorf("%d %q %q %v", code, out, errOut, *calls)
	}
	if !strings.Contains(errOut, "May sign for any site") {
		t.Errorf("it should warn of a CA with no limits: %q", errOut)
	}
	os.WriteFile(file, []byte("not a certificate"), 0o644)
	if code, _, errOut := run(t, "trust", "--file", file); code == 0 || !strings.Contains(errOut, "no PEM certificate") {
		t.Errorf("%d %q", code, errOut)
	}
}

func TestTrustTheServerOnThisMachine(t *testing.T) {
	calls := fakeTrust(t)
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	cfgFile := filepath.Join(dir, "server.toml")
	t.Setenv("MOORAGE_CONFIG", cfgFile)
	if code, _, errOut := run(t, "server", "init", "--domain", "example.test", "--data-dir", dataDir, "--config", cfgFile); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := run(t, "trust"); code == 0 || !strings.Contains(errOut, "first time it starts") {
		t.Errorf("before it has started: %d %q", code, errOut)
	}
	pki := newPKI(t, "Moorage CA for example.test", "example.test")
	certs := filepath.Join(dataDir, "certs")
	os.MkdirAll(certs, 0o700)
	os.WriteFile(filepath.Join(certs, "ca.crt"), pki.pem, 0o644)

	// Its own file on its own machine needs no confirming.
	code, out, errOut := run(t, "trust")
	if code != 0 || !strings.Contains(out, "Added it") || len(*calls) != 1 {
		t.Errorf("%d %q %q %v", code, out, errOut, *calls)
	}

	// With a server of plain HTTP, there is nothing to trust.
	if code, _, errOut := run(t, "server", "init", "--preset", "local", "--data-dir", dataDir, "--config", cfgFile, "--force"); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := run(t, "trust"); code == 0 || !strings.Contains(errOut, "plain HTTP") {
		t.Errorf("plain HTTP: %d %q", code, errOut)
	}
}

func TestTrustAServerWithNoCA(t *testing.T) {
	fakeTrust(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	code, _, errOut := run(t, "trust", "--server", srv.URL, "--token", "x")
	if code == 0 || !strings.Contains(errOut, "no internal CA") {
		t.Errorf("%d %q", code, errOut)
	}
}
