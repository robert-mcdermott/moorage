// Package truststore adds a certificate authority to the lists a machine trusts:
// the operating system's, which most programs use, and on Linux the browsers'
// own. Everything is done with the system's own tools, which ask for whatever
// authorisation they need (spec/06-ingress.md, "TLS modes").
package truststore

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/platform"
)

// System is the machine whose trust stores are changed.
type System struct {
	GOOS string
	// Privileged says the process runs as root, or an elevated Administrator.
	Privileged bool
	// UnderSudo says it does so for another user, whose browsers it leaves alone.
	UnderSudo bool
	Home      string
	// Root is put before the paths of system directories; tests point it at a
	// directory of their own.
	Root string
	// Run runs a command with the terminal attached: it may ask for a password.
	Run      func(name string, args ...string) error
	LookPath func(string) (string, error)
}

// Local returns this machine.
func Local() System {
	home, _ := os.UserHomeDir()
	return System{
		GOOS: runtime.GOOS, Privileged: platform.IsPrivileged(), UnderSudo: os.Getenv("SUDO_USER") != "", Home: home,
		LookPath: exec.LookPath,
		Run: func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return cmd.Run()
		},
	}
}

// Result says what was changed, and what was not.
type Result struct {
	// Places are the lists that changed.
	Places []string
	// Notes are for the user to act on.
	Notes []string
}

// Fingerprint is a certificate's SHA-256, written as browsers and openssl do.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(parts, ":")
}

// SameFingerprint compares a fingerprint given by a person with a certificate's:
// with or without colons, a SHA256: prefix, in either case.
func SameFingerprint(given string, cert *x509.Certificate) bool {
	clean := func(s string) string {
		s = strings.ToUpper(strings.TrimSpace(s))
		s = strings.TrimPrefix(strings.TrimPrefix(s, "SHA256:"), "SHA-256:")
		return strings.ReplaceAll(s, ":", "")
	}
	return given != "" && clean(given) == clean(Fingerprint(cert))
}

// Trusted reports whether the machine's list already trusts a certificate.
func Trusted(cert *x509.Certificate) bool {
	_, err := cert.Verify(x509.VerifyOptions{KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}

func id(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:8])
}

func sha1Hex(cert *x509.Certificate) string {
	sum := sha1.Sum(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func (s System) path(p string) string { return filepath.Join(s.Root, filepath.FromSlash(p)) }

// tempPEM writes a certificate to a temporary file for a tool to read. It is
// public, and the file goes when done is called.
func tempPEM(cert *x509.Certificate) (file string, done func(), err error) {
	f, err := os.CreateTemp("", "moorage-ca-*.crt")
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	// sudo's tools may run as another user.
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// Install adds a CA to the machine's trust stores.
func (s System) Install(cert *x509.Certificate) (Result, error) {
	file, done, err := tempPEM(cert)
	if err != nil {
		return Result{}, err
	}
	defer done()
	switch s.GOOS {
	case "darwin":
		return s.installDarwin(cert, file)
	case "windows":
		return s.installWindows(file)
	case "linux":
		return s.installLinux(cert, file)
	}
	return Result{}, fmt.Errorf("Moorage cannot change the trust store on %s: add the CA certificate (moor trust --print) by hand", s.GOOS)
}

// Remove takes a CA out of the machine's trust stores.
func (s System) Remove(cert *x509.Certificate) (Result, error) {
	file, done, err := tempPEM(cert)
	if err != nil {
		return Result{}, err
	}
	defer done()
	switch s.GOOS {
	case "darwin":
		return s.removeDarwin(cert, file)
	case "windows":
		return s.removeWindows(cert)
	case "linux":
		return s.removeLinux(cert)
	}
	return Result{}, fmt.Errorf("Moorage cannot change the trust store on %s", s.GOOS)
}

// macOS: the System keychain as root, or the user's own, for which it asks for
// the user's password in a dialog.

func (s System) keychain() (file, name string) {
	if s.Privileged {
		return "/Library/Keychains/System.keychain", "the System keychain"
	}
	return filepath.Join(s.Home, "Library", "Keychains", "login.keychain-db"), "your login keychain"
}

func (s System) installDarwin(cert *x509.Certificate, file string) (Result, error) {
	keychain, name := s.keychain()
	result := "trustRoot"
	if !bytes.Equal(cert.RawIssuer, cert.RawSubject) {
		result = "trustAsRoot" // an intermediate
	}
	args := []string{"add-trusted-cert"}
	if s.Privileged {
		args = append(args, "-d")
	}
	args = append(args, "-r", result, "-k", keychain, file)
	if err := s.Run("security", args...); err != nil {
		return Result{}, fmt.Errorf("add it to %s: %w (without a window to ask for your password in, use sudo moor trust)", name, err)
	}
	return Result{Places: []string{name}, Notes: []string{"Firefox may keep its own list: if it does not trust the server, import the CA certificate (moor trust --print) in its settings."}}, nil
}

func (s System) removeDarwin(cert *x509.Certificate, file string) (Result, error) {
	keychain, name := s.keychain()
	args := []string{"remove-trusted-cert"}
	if s.Privileged {
		args = append(args, "-d")
	}
	if err := s.Run("security", append(args, file)...); err != nil {
		return Result{}, fmt.Errorf("remove it from %s: %w", name, err)
	}
	if err := s.Run("security", "delete-certificate", "-Z", strings.ToUpper(sha1Hex(cert)), keychain); err != nil {
		return Result{}, fmt.Errorf("remove it from %s: %w", name, err)
	}
	return Result{Places: []string{name}}, nil
}

// Windows: the machine's list as an Administrator, or the user's own, for which
// it asks in a dialog.

func (s System) windowsStore() (args []string, name string) {
	if s.Privileged {
		return nil, "the machine's Trusted Root Certification Authorities"
	}
	return []string{"-user"}, "your Trusted Root Certification Authorities"
}

func (s System) installWindows(file string) (Result, error) {
	scope, name := s.windowsStore()
	args := append([]string{"-addstore", "-f"}, scope...)
	if err := s.Run("certutil", append(args, "Root", file)...); err != nil {
		return Result{}, fmt.Errorf("add it to %s: %w", name, err)
	}
	return Result{Places: []string{name}, Notes: []string{"Firefox may keep its own list: if it does not trust the server, import the CA certificate (moor trust --print) in its settings."}}, nil
}

func (s System) removeWindows(cert *x509.Certificate) (Result, error) {
	scope, name := s.windowsStore()
	args := append([]string{"-delstore"}, scope...)
	if err := s.Run("certutil", append(args, "Root", sha1Hex(cert))...); err != nil {
		return Result{}, fmt.Errorf("remove it from %s: %w", name, err)
	}
	return Result{Places: []string{name}}, nil
}

// Linux: a file in the distribution's directory of anchors and its tool to take
// it up, which need root; and the browsers' lists, which belong to the user.

var linuxStores = []struct {
	dir    string
	update []string
}{
	{"/usr/local/share/ca-certificates", []string{"update-ca-certificates"}},           // Debian, Ubuntu, Alpine
	{"/etc/pki/ca-trust/source/anchors", []string{"update-ca-trust", "extract"}},       // Fedora, RHEL
	{"/usr/share/pki/trust/anchors", []string{"update-ca-certificates"}},               // openSUSE
	{"/etc/ca-certificates/trust-source/anchors", []string{"trust", "extract-compat"}}, // Arch
}

// admin runs a command as root: as it is, or through sudo, which asks for a
// password if it needs one.
func (s System) admin(name string, args ...string) error {
	if s.Privileged {
		return s.Run(name, args...)
	}
	if _, err := s.LookPath("sudo"); err != nil {
		return errors.New("changing the system's list needs root: run moor trust as root")
	}
	return s.Run("sudo", append([]string{name}, args...)...)
}

// linuxStore finds this distribution's way of adding a CA.
func (s System) linuxStore() (dir string, update []string, err error) {
	for _, st := range linuxStores {
		if fi, err := os.Stat(s.path(st.dir)); err != nil || !fi.IsDir() {
			continue
		}
		if _, err := s.LookPath(st.update[0]); err != nil {
			continue
		}
		return st.dir, st.update, nil
	}
	return "", nil, errors.New("Moorage does not know how this Linux distribution keeps its CAs: add the CA certificate (moor trust --print) by hand")
}

func certName(cert *x509.Certificate) string { return "moorage-" + id(cert) + ".crt" }

func (s System) installLinux(cert *x509.Certificate, file string) (Result, error) {
	dir, update, err := s.linuxStore()
	if err != nil {
		return Result{}, err
	}
	dest := s.path(dir + "/" + certName(cert))
	if err := s.admin("install", "-m", "0644", file, dest); err != nil {
		return Result{}, fmt.Errorf("add it to %s: %w", dir, err)
	}
	if err := s.admin(update[0], update[1:]...); err != nil {
		return Result{}, fmt.Errorf("%s: %w", update[0], err)
	}
	res := Result{Places: []string{"the system's list (" + dir + ")"}}
	return s.browsers(res, cert, file, true)
}

func (s System) removeLinux(cert *x509.Certificate) (Result, error) {
	dir, update, err := s.linuxStore()
	if err != nil {
		return Result{}, err
	}
	if err := s.admin("rm", "-f", s.path(dir+"/"+certName(cert))); err != nil {
		return Result{}, fmt.Errorf("remove it from %s: %w", dir, err)
	}
	args := update[1:]
	if update[0] == "update-ca-certificates" {
		args = []string{"--fresh"} // or the link it made stays
	}
	if err := s.admin(update[0], args...); err != nil {
		return Result{}, fmt.Errorf("%s: %w", update[0], err)
	}
	res := Result{Places: []string{"the system's list (" + dir + ")"}}
	return s.browsers(res, cert, "", false)
}

// browserLists finds the lists Linux browsers keep for themselves.
func (s System) browserLists() []string {
	var lists []string
	for _, pattern := range []string{
		".pki/nssdb",
		"snap/chromium/current/.pki/nssdb",
		".mozilla/firefox/*",
		"snap/firefox/common/.mozilla/firefox/*",
		".var/app/org.mozilla.firefox/.mozilla/firefox/*",
	} {
		found, _ := filepath.Glob(filepath.Join(s.Home, filepath.FromSlash(pattern), "cert9.db"))
		for _, db := range found {
			lists = append(lists, filepath.Dir(db))
		}
	}
	return lists
}

// browsers changes the user's browsers' lists, where it can. As root for another
// user, it leaves them alone: they are not root's.
func (s System) browsers(res Result, cert *x509.Certificate, file string, add bool) (Result, error) {
	if s.Privileged && s.UnderSudo {
		res.Notes = append(res.Notes, "Chromium and Firefox keep their own lists on Linux: run moor trust without sudo to change yours.")
		return res, nil
	}
	lists := s.browserLists()
	if len(lists) == 0 {
		return res, nil
	}
	if _, err := s.LookPath("certutil"); err != nil {
		res.Notes = append(res.Notes, "Chromium and Firefox keep their own lists on Linux: install certutil (libnss3-tools or nss-tools) and run moor trust again to change them.")
		return res, nil
	}
	nick := "Moorage CA " + id(cert)
	for _, list := range lists {
		args := []string{"-D", "-d", "sql:" + list, "-n", nick}
		if add {
			args = []string{"-A", "-d", "sql:" + list, "-t", "C,,", "-n", nick, "-i", file}
		}
		if err := s.Run("certutil", args...); err != nil {
			if add { // removing what was never added is no failure
				res.Notes = append(res.Notes, fmt.Sprintf("Could not change the browser list in %s: %v", list, err))
			}
			continue
		}
		res.Places = append(res.Places, "the browser list in "+list)
	}
	return res, nil
}
