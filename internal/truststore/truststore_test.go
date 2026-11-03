package truststore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func testCA(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Moorage CA for test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// fake is a machine that records commands instead of running them. No test here
// changes the real trust store: CI does that, on a runner it throws away.
type fake struct {
	System
	calls [][]string
	files map[string]string // what commands read, while they ran
	fail  string
	tools []string
}

func newFake(t *testing.T, goos string, tools ...string) *fake {
	f := &fake{files: map[string]string{}, tools: tools}
	f.System = System{GOOS: goos, Home: t.TempDir(), Root: t.TempDir(),
		LookPath: func(name string) (string, error) {
			if slices.Contains(f.tools, name) {
				return "/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		Run: func(name string, args ...string) error {
			f.calls = append(f.calls, append([]string{name}, args...))
			for _, a := range args {
				if strings.HasSuffix(a, ".crt") {
					if data, err := os.ReadFile(a); err == nil {
						f.files[a] = string(data)
					}
				}
			}
			if f.fail == name {
				return errors.New("refused")
			}
			return nil
		},
	}
	return f
}

func (f *fake) line(i int) string { return strings.Join(f.calls[i], " ") }

func TestMacOS(t *testing.T) {
	ca := testCA(t)
	sum := strings.ToUpper(sha1Hex(ca))

	// The user's own keychain.
	f := newFake(t, "darwin")
	res, err := f.Install(ca)
	if err != nil {
		t.Fatal(err)
	}
	login := filepath.Join(f.Home, "Library", "Keychains", "login.keychain-db")
	if !strings.HasPrefix(f.line(0), "security add-trusted-cert -r trustRoot -k "+login+" ") {
		t.Errorf("%s", f.line(0))
	}
	if len(f.files) != 1 {
		t.Errorf("the tool read no certificate: %v", f.calls)
	}
	for _, data := range f.files {
		if !strings.Contains(data, "BEGIN CERTIFICATE") {
			t.Errorf("%q", data)
		}
	}
	if res.Places[0] != "your login keychain" {
		t.Errorf("%+v", res)
	}
	f.calls = nil
	if _, err := f.Remove(ca); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.line(0), "security remove-trusted-cert ") || f.line(1) != "security delete-certificate -Z "+sum+" "+login {
		t.Errorf("%v", f.calls)
	}

	// The machine's, as root.
	f = newFake(t, "darwin")
	f.Privileged = true
	if _, err := f.Install(ca); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.line(0), "security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ") {
		t.Errorf("%s", f.line(0))
	}

	// An intermediate cannot be a root.
	inter := *ca
	inter.RawIssuer = []byte("someone else")
	f = newFake(t, "darwin")
	if _, err := f.Install(&inter); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.line(0), "-r trustAsRoot") {
		t.Errorf("%s", f.line(0))
	}

	f = newFake(t, "darwin")
	f.fail = "security"
	if _, err := f.Install(ca); err == nil || !strings.Contains(err.Error(), "sudo moor trust") {
		t.Errorf("%v", err)
	}
	f.Privileged = true
	if _, err := f.Install(ca); err == nil || !strings.Contains(err.Error(), "at the screen") {
		t.Errorf("%v", err)
	}
}

func TestWindows(t *testing.T) {
	ca := testCA(t)
	f := newFake(t, "windows")
	if _, err := f.Install(ca); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.line(0), "certutil -addstore -f -user Root ") {
		t.Errorf("%s", f.line(0))
	}
	f.calls = nil
	f.Privileged = true
	if _, err := f.Install(ca); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.line(0), "certutil -addstore -f Root ") {
		t.Errorf("%s", f.line(0))
	}
	f.calls = nil
	if _, err := f.Remove(ca); err != nil {
		t.Fatal(err)
	}
	if f.line(0) != "certutil -delstore Root "+sha1Hex(ca) {
		t.Errorf("%s", f.line(0))
	}
}

func TestLinux(t *testing.T) {
	ca := testCA(t)
	debian := func(t *testing.T, tools ...string) *fake {
		f := newFake(t, "linux", append(tools, "update-ca-certificates")...)
		os.MkdirAll(f.path("/usr/local/share/ca-certificates"), 0o755)
		return f
	}
	dest := func(f *fake) string { return f.path("/usr/local/share/ca-certificates/" + certName(ca)) }

	// As a user, with sudo for the system's list.
	f := debian(t, "sudo")
	res, err := f.Install(ca)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.line(0), "sudo install -m 0644 ") || !strings.HasSuffix(f.line(0), " "+dest(f)) || f.line(1) != "sudo update-ca-certificates" {
		t.Errorf("%v", f.calls)
	}
	if len(res.Places) != 1 || len(res.Notes) != 0 {
		t.Errorf("%+v", res)
	}
	f.calls = nil
	if _, err := f.Remove(ca); err != nil {
		t.Fatal(err)
	}
	if f.line(0) != "sudo rm -f "+dest(f) || f.line(1) != "sudo update-ca-certificates --fresh" {
		t.Errorf("%v", f.calls)
	}

	// As root, without; and without either, it says what to do.
	f = debian(t)
	f.Privileged = true
	if _, err := f.Install(ca); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.line(0), "install -m 0644 ") {
		t.Errorf("%v", f.calls)
	}
	f = debian(t)
	if _, err := f.Install(ca); err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Errorf("%v", err)
	}

	// Another distribution.
	f = newFake(t, "linux", "sudo", "update-ca-trust")
	os.MkdirAll(f.path("/etc/pki/ca-trust/source/anchors"), 0o755)
	if _, err := f.Install(ca); err != nil {
		t.Fatal(err)
	}
	if f.line(1) != "sudo update-ca-trust extract" {
		t.Errorf("%v", f.calls)
	}

	// One it does not know: the directory alone is not enough.
	f = newFake(t, "linux", "sudo")
	os.MkdirAll(f.path("/usr/local/share/ca-certificates"), 0o755)
	if _, err := f.Install(ca); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Errorf("%v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v", f.calls)
	}
}

func TestLinuxBrowsers(t *testing.T) {
	ca := testCA(t)
	newMachine := func(t *testing.T, tools ...string) (*fake, string, string) {
		f := newFake(t, "linux", append(tools, "sudo", "update-ca-certificates")...)
		os.MkdirAll(f.path("/usr/local/share/ca-certificates"), 0o755)
		chrome := filepath.Join(f.Home, ".pki", "nssdb")
		firefox := filepath.Join(f.Home, ".mozilla", "firefox", "abc.default")
		for _, d := range []string{chrome, firefox} {
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, "cert9.db"), nil, 0o644)
		}
		// A directory of Firefox's that is no profile.
		os.MkdirAll(filepath.Join(f.Home, ".mozilla", "firefox", "Crash Reports"), 0o755)
		return f, chrome, firefox
	}

	f, chrome, firefox := newMachine(t, "certutil")
	res, err := f.Install(ca)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 4 || !strings.HasPrefix(f.line(2), "certutil -A -d sql:"+chrome+" -t C,, -n Moorage CA "+id(ca)+" -i ") || !strings.Contains(f.line(3), "sql:"+firefox) {
		t.Errorf("%v", f.calls)
	}
	if len(res.Places) != 3 {
		t.Errorf("%+v", res)
	}
	f.calls = nil
	if _, err := f.Remove(ca); err != nil {
		t.Fatal(err)
	}
	if f.line(2) != "certutil -D -d sql:"+chrome+" -n Moorage CA "+id(ca) {
		t.Errorf("%v", f.calls)
	}

	// Without the tool, it says so; the system's list has still changed.
	f, _, _ = newMachine(t)
	res, err = f.Install(ca)
	if err != nil || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "certutil") {
		t.Errorf("%v %+v", err, res)
	}

	// Root's home is not the user's.
	f, _, _ = newMachine(t, "certutil")
	f.Privileged, f.UnderSudo = true, true
	res, err = f.Install(ca)
	if err != nil || len(f.calls) != 2 || !strings.Contains(res.Notes[0], "without sudo") {
		t.Errorf("%v %v %+v", err, f.calls, res)
	}
}

func TestFingerprint(t *testing.T) {
	ca := testCA(t)
	fp := Fingerprint(ca)
	if len(fp) != 95 || strings.ToUpper(fp) != fp {
		t.Errorf("%s", fp)
	}
	for _, same := range []string{fp, strings.ToLower(fp), strings.ReplaceAll(fp, ":", ""), "SHA256:" + fp, "sha-256:" + fp, " " + fp + "\n"} {
		if !SameFingerprint(same, ca) {
			t.Errorf("%q", same)
		}
	}
	for _, other := range []string{"", "AB:CD", Fingerprint(testCA(t))} {
		if SameFingerprint(other, ca) {
			t.Errorf("%q", other)
		}
	}
}

func TestUnknownSystem(t *testing.T) {
	f := newFake(t, "plan9")
	if _, err := f.Install(testCA(t)); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Errorf("%v", err)
	}
}
