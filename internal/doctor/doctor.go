// Package doctor checks a server and what it depends on, and says how to fix
// what it finds (spec/07-observability.md, "Server health and diagnostics").
package doctor

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/driver/container"
	"github.com/robert-mcdermott/moorage/internal/ingress"
	"github.com/robert-mcdermott/moorage/internal/isolate"
	"github.com/robert-mcdermott/moorage/internal/layout"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/truststore"
)

// How a check came out.
const (
	OK   = "ok"
	Info = "info" // worth knowing; nothing is wrong
	Warn = "warn"
	Fail = "fail"
)

// Check is one thing checked.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Fix says what to do about it.
	Fix string `json:"fix,omitempty"`
}

// Options say what to check.
type Options struct {
	Config config.Config
	// PublicURL, if set, is the server's address through whatever stands in
	// front of it: the whole path is checked.
	PublicURL string
	// CACert is a PEM file of a CA to trust for PublicURL, besides the system's.
	CACert string
	// Timeouts adds the check of long silences to PublicURL's, which takes a while.
	Timeouts bool
	// WantsEngine, if set, says whether anything here runs in a container, which decides how much an engine that is missing matters.
	WantsEngine func(context.Context) bool
	// InstancePorts lists the instances' ports on this machine, for a running server.
	InstancePorts func(context.Context) ([]int, error)

	// GOOS, Root and these are for tests, in place of this machine's.
	GOOS     string
	Root     string
	Lookup   func(ctx context.Context, host string) ([]string, error)
	Command  func(ctx context.Context, name string, args ...string) ([]byte, error)
	Now      func() time.Time
	Wait     time.Duration // how long the check of silences waits
	AddrsFor func() ([]net.Addr, error)
	// Dial, if set, makes the connections of the checks of PublicURL.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (o *Options) defaults() {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Lookup == nil {
		o.Lookup = net.DefaultResolver.LookupHost
	}
	if o.Command == nil {
		o.Command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Wait == 0 {
		o.Wait = 70 * time.Second // beyond a common limit of a minute
	}
	if o.AddrsFor == nil {
		o.AddrsFor = net.InterfaceAddrs
	}
}

// Run makes the checks.
func Run(ctx context.Context, o Options) []Check {
	o.defaults()
	running, checks := checkServer(ctx, o)
	checks = append(checks, checkListeners(o, running)...)
	checks = append(checks, checkDNS(ctx, o)...)
	checks = append(checks, checkTLS(ctx, o, running)...)
	checks = append(checks, checkContainers(ctx, o))
	checks = append(checks, checkData(o)...)
	checks = append(checks, checkDisk(o))
	if c, ok := checkClock(ctx, o); ok {
		checks = append(checks, c)
	}
	if o.GOOS == "linux" {
		checks = append(checks, checkKernel(o))
	}
	checks = append(checks, checkIsolation(o))
	if running && o.InstancePorts != nil {
		if c, ok := checkApps(ctx, o); ok {
			checks = append(checks, c)
		}
	}
	if o.PublicURL != "" {
		checks = append(checks, checkPublic(ctx, o)...)
	}
	return checks
}

// Setup makes the checks of a server that is running, on what someone setting it
// up decides: its DNS and its certificates.
func Setup(ctx context.Context, o Options) []Check {
	o.defaults()
	return append(checkDNS(ctx, o), checkTLS(ctx, o, true)...)
}

// local turns a listening address into one to connect to.
func local(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return net.JoinHostPort(host, port)
}

func checkServer(ctx context.Context, o Options) (running bool, checks []Check) {
	name := "server"
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+local(o.Config.Listen.HTTP)+"/.moorage/health", nil)
	res, err := client.Do(req)
	if err != nil {
		return false, []Check{{Name: name, Status: Warn, Detail: "not running: nothing answers on " + o.Config.Listen.HTTP,
			Fix: "moor server start, or moor server run"}}
	}
	defer res.Body.Close()
	var h struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(res.Body).Decode(&h); err != nil || h.Version == "" {
		return false, []Check{{Name: name, Status: Fail, Detail: "something other than Moorage answers on " + o.Config.Listen.HTTP,
			Fix: "stop it, or change listen.http in " + o.Config.Path}}
	}
	if res.StatusCode != http.StatusOK || h.Status != "ok" {
		return true, []Check{{Name: name, Status: Fail, Detail: fmt.Sprintf("running %s, and not healthy: %s", h.Version, h.Status),
			Fix: "see the server's log, in " + filepath.Join(o.Config.DataDir, "logs", "server")}}
	}
	c := Check{Name: name, Status: OK, Detail: "running " + h.Version + " on " + o.Config.Listen.HTTP}
	if h.Version != buildinfo.Version {
		c.Status, c.Detail = Warn, fmt.Sprintf("running %s, and this moor is %s", h.Version, buildinfo.Version)
		c.Fix = "restart the server, to run the version installed"
	}
	return true, []Check{c}
}

// checkListeners checks each listening address: that the server answers there,
// or, while it is not running, that it could.
func checkListeners(o Options, running bool) []Check {
	var checks []Check
	for _, l := range []struct{ name, addr string }{
		{"listen.http", o.Config.Listen.HTTP}, {"listen.https", o.Config.Listen.HTTPS}, {"listen.ssh", o.Config.Listen.SSH},
	} {
		if l.addr == "" {
			continue
		}
		if running {
			conn, err := net.DialTimeout("tcp", local(l.addr), 2*time.Second)
			if err != nil {
				checks = append(checks, Check{Name: l.name, Status: Fail, Detail: fmt.Sprintf("the server does not answer on %s: %v", l.addr, err),
					Fix: "see the server's log, and restart it"})
				continue
			}
			conn.Close()
			checks = append(checks, Check{Name: l.name, Status: OK, Detail: "answering on " + l.addr})
			continue
		}
		ln, err := net.Listen("tcp", l.addr)
		switch {
		case err == nil:
			ln.Close()
			checks = append(checks, Check{Name: l.name, Status: OK, Detail: "free: the server can listen on " + l.addr})
		case platform.AddressInUse(err):
			checks = append(checks, Check{Name: l.name, Status: Fail, Detail: "something else is listening on " + l.addr,
				Fix: "stop it, or change " + l.name + " in " + o.Config.Path})
		case platform.AccessDenied(err):
			checks = append(checks, Check{Name: l.name, Status: Fail, Detail: "this user may not listen on " + l.addr,
				Fix: "run the server as root, as moor server install does (or give it CAP_NET_BIND_SERVICE)"})
		default:
			checks = append(checks, Check{Name: l.name, Status: Fail, Detail: fmt.Sprintf("cannot listen on %s: %v", l.addr, err),
				Fix: "change " + l.name + " in " + o.Config.Path})
		}
	}
	return checks
}

func randomLabel() string {
	var b [4]byte
	rand.Read(b[:])
	return "doctor-" + hex.EncodeToString(b[:])
}

func checkDNS(ctx context.Context, o Options) []Check {
	cfg := o.Config
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe := randomLabel() + "." + cfg.Domain
	if cfg.Routing == "path" {
		probe = cfg.Domain // every app is at a path of this one name, with no wildcard to look for
	}
	if cfg.Domain == "localhost" {
		addrs, err := o.Lookup(lookupCtx, probe)
		if err == nil && len(addrs) > 0 {
			return []Check{{Name: "dns", Status: OK, Detail: "*.localhost resolves on this machine"}}
		}
		c := Check{Name: "dns", Status: Info, Detail: "*.localhost does not resolve for every program here: Chromium- and Firefox-based browsers manage it on their own, others may not"}
		switch o.GOOS {
		case "darwin":
			c.Fix = "moor server resolver install, for Safari and command-line tools"
		case "linux":
			c.Fix = "systemd-resolved, or nss-myhostname, resolves these names"
		}
		return []Check{c}
	}
	var checks []Check
	lookup := func(name, host, fix string) []string {
		addrs, err := o.Lookup(lookupCtx, host)
		if err != nil || len(addrs) == 0 {
			checks = append(checks, Check{Name: name, Status: Fail, Detail: host + " does not resolve", Fix: fix})
			return nil
		}
		return addrs
	}
	console := lookup("dns (console)", cfg.ConsoleHost, "add a DNS record for "+cfg.ConsoleHost)
	appsFix := "add a wildcard DNS record, *." + cfg.Domain + ", for this server or its proxy"
	if cfg.Routing == "path" {
		appsFix = "add a DNS record for " + cfg.Domain + ", for this server or its proxy"
	}
	apps := lookup("dns (apps)", probe, appsFix)
	if console == nil || apps == nil {
		if console != nil {
			checks = append(checks, Check{Name: "dns (console)", Status: OK, Detail: cfg.ConsoleHost + " → " + strings.Join(console, ", ")})
		}
		if apps != nil {
			checks = append(checks, Check{Name: "dns (apps)", Status: OK, Detail: "*." + cfg.Domain + " → " + strings.Join(apps, ", ")})
		}
		return checks
	}
	c := Check{Name: "dns", Status: OK, Detail: fmt.Sprintf("%s → %s; *.%s → %s", cfg.ConsoleHost, strings.Join(console, ", "), cfg.Domain, strings.Join(apps, ", "))}
	if len(cfg.Proxy.Trusted) == 0 && !o.anyLocal(apps) {
		c.Status = Warn
		c.Detail += ": not an address of this machine"
		c.Fix = "fine if a load balancer or NAT forwards to it; otherwise, point the record at this server"
	}
	return []Check{c}
}

func (o Options) anyLocal(addrs []string) bool {
	localAddrs, err := o.AddrsFor()
	if err != nil {
		return true // it cannot say
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() {
			return true
		}
		for _, l := range localAddrs {
			if n, ok := l.(*net.IPNet); ok && n.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// checkACME says how a server's certificates from a CA stand. A wildcard is renewed when it has a third of its life left, so that one
// with less than two weeks to go has been failing for a while. Those for each hostname are renewed when next wanted, and stay on disk once they
// are not: an old one says nothing, and the event that a certificate could not be got is what says that something is wrong.
func checkACME(o Options) []Check {
	cfg := o.Config
	directory := cfg.TLS.ACME.Directory
	if directory == "" {
		directory = config.LetsEncrypt
	}
	var checks []Check
	found, err := ingress.ACMECertificates(filepath.Join(layout.New(cfg.DataDir).Certs, ingress.ACMEDir))
	switch {
	case err != nil:
		checks = append(checks, Check{Name: "tls", Status: Info, Detail: "this user cannot read the certificates", Fix: "run moor server doctor as the server's user"})
	case len(found) == 0:
		checks = append(checks, Check{Name: "tls", Status: Info, Detail: "no certificate yet from " + directory + ": each is asked for when it is first wanted"})
	default:
		c := Check{Name: "tls", Status: OK, Detail: fmt.Sprintf("%d certificates from %s", len(found), directory)}
		now := time.Now()
		if i := slices.IndexFunc(found, func(f ingress.CertificateOnDisk) bool { return f.NotAfter.After(now) }); i >= 0 {
			c.Detail += fmt.Sprintf("; the next to expire is for %s, on %s", strings.Join(found[i].Names, ", "), found[i].NotAfter.Format("2 Jan 2006"))
		}
		if cfg.WildcardCertificate() {
			wildcard := "*." + cfg.Domain
			var newest time.Time
			for _, f := range found {
				if slices.Contains(f.Names, wildcard) && f.NotAfter.After(newest) {
					newest = f.NotAfter
				}
			}
			switch {
			case newest.IsZero():
				c.Status, c.Fix = Warn, "there is no certificate for "+wildcard+" yet: the server's log says why"
			case time.Until(newest) < 14*24*time.Hour:
				c.Status, c.Fix = Warn, "the certificate for "+wildcard+" expires on "+newest.Format("2 Jan 2006")+", and should have been renewed by now: the server's log says why it has not"
			}
		}
		checks = append(checks, c)
	}
	if !cfg.WildcardCertificate() {
		checks = append(checks, Check{Name: "tls (limits)", Status: Info, Detail: "each hostname has a certificate of its own, and a public CA limits how many a domain can have in a week",
			Fix: "with many previews, set [tls.acme.dns] in " + cfg.Path + " for one certificate for the whole domain"})
	}
	for _, listen := range []struct{ address, port, what string }{{cfg.Listen.HTTP, "80", "http-01"}, {cfg.Listen.HTTPS, "443", "tls-alpn-01"}} {
		if _, port, err := net.SplitHostPort(listen.address); err == nil && port != listen.port {
			checks = append(checks, Check{Name: "tls (ports)", Status: Info, Detail: fmt.Sprintf("a CA looks at port %s for %s, and this server listens on %s", listen.port, listen.what, listen.address), Fix: "forward that port here, or see [tls.acme]"})
		}
	}
	return checks
}

func checkTLS(ctx context.Context, o Options, running bool) []Check {
	cfg := o.Config
	switch cfg.TLS.Mode {
	case "off":
		c := Check{Name: "tls", Status: OK, Detail: "plain HTTP behind a TLS proxy"}
		switch {
		case len(cfg.Proxy.Trusted) > 0:
			if !isLoopbackListen(cfg.Listen.HTTP) {
				c.Status = Info
				c.Detail += ", with " + cfg.Listen.HTTP + " open to more than this machine"
				c.Fix = "keep it to a private network: anyone who reaches it directly skips the proxy"
			}
			for _, p := range cfg.ProxyProblems() {
				c.Status, c.Detail, c.Fix = Warn, p, "see moor server proxy-config nginx"
			}
		case cfg.Domain == "localhost":
			c.Detail = "plain HTTP, on this computer only"
		default:
			c.Status, c.Detail = Warn, "plain HTTP with no proxy: passwords and tokens cross the network unencrypted"
			c.Fix = `set tls.mode = "internal" in ` + cfg.Path + ", or put a TLS proxy in front"
		}
		return []Check{c}
	case "files":
		return checkCertFiles(o)
	case "acme":
		return checkACME(o)
	}
	return checkInternalCA(ctx, o, running)
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func readFirstCert(file string) (*x509.Certificate, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM certificate", file)
	}
	return x509.ParseCertificate(block.Bytes)
}

// expiry judges how long a certificate has left.
func expiry(o Options, what string, cert *x509.Certificate, warnWithin time.Duration) Check {
	left := cert.NotAfter.Sub(o.Now())
	c := Check{Name: "tls", Status: OK, Detail: fmt.Sprintf("%s expires on %s", what, cert.NotAfter.Format("2 Jan 2006"))}
	switch {
	case left <= 0:
		c.Status, c.Detail = Fail, fmt.Sprintf("%s expired on %s", what, cert.NotAfter.Format("2 Jan 2006"))
		c.Fix = "replace it"
	case left < warnWithin:
		c.Status, c.Detail = Warn, fmt.Sprintf("%s expires in %d days, on %s", what, int(left.Hours()/24), cert.NotAfter.Format("2 Jan 2006"))
		c.Fix = "replace it before then"
	}
	return c
}

func checkInternalCA(ctx context.Context, o Options, running bool) []Check {
	cfg := o.Config
	file := cfg.TLS.CACert
	if file == "" {
		file = filepath.Join(layout.New(cfg.DataDir).Certs, "ca.crt")
	}
	cert, err := readFirstCert(file)
	switch {
	case errors.Is(err, os.ErrNotExist) && cfg.TLS.CACert == "":
		return []Check{{Name: "tls", Status: Info, Detail: "the server makes its CA the first time it starts"}}
	case errors.Is(err, os.ErrPermission):
		return []Check{{Name: "tls", Status: Info, Detail: "this user cannot read the CA in " + file, Fix: "run moor server doctor as the server's user, or with sudo"}}
	case err != nil:
		return []Check{{Name: "tls", Status: Fail, Detail: err.Error(), Fix: "see tls.ca_cert in " + cfg.Path}}
	}
	c := expiry(o, "the internal CA", cert, 180*24*time.Hour)
	c.Name = "tls (ca)"
	if c.Status == OK {
		c.Detail += ", SHA-256 " + truststore.Fingerprint(cert)
	}
	checks := []Check{c}
	if truststore.Trusted(cert) {
		checks = append(checks, Check{Name: "tls (trust)", Status: OK, Detail: "this machine trusts the internal CA"})
	} else {
		checks = append(checks, Check{Name: "tls (trust)", Status: Info, Detail: "this machine does not trust the internal CA, and browsers will warn about its certificates",
			Fix: "moor trust, on each machine that uses the server"})
	}
	if !running || cfg.Listen.HTTPS == "" {
		return checks
	}
	// What it serves comes from that CA.
	conn, err := (&tls.Dialer{Config: &tls.Config{ServerName: cfg.ConsoleHost, InsecureSkipVerify: true}}).DialContext(ctx, "tcp", local(cfg.Listen.HTTPS))
	if err != nil {
		return append(checks, Check{Name: "tls (serving)", Status: Fail, Detail: fmt.Sprintf("no certificate for %s: %v", cfg.ConsoleHost, err),
			Fix: "see the server's log"})
	}
	defer conn.Close()
	peers := conn.(*tls.Conn).ConnectionState().PeerCertificates
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	inter := x509.NewCertPool()
	for _, p := range peers[1:] {
		inter.AddCert(p)
	}
	if _, err := peers[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: cfg.ConsoleHost}); err != nil {
		return append(checks, Check{Name: "tls (serving)", Status: Fail, Detail: "what it serves does not check out against its CA: " + err.Error(),
			Fix: "restart the server, and see its log"})
	}
	return append(checks, Check{Name: "tls (serving)", Status: OK, Detail: "serves " + cfg.ConsoleHost + " with a certificate from it, until " + peers[0].NotAfter.Format("2 Jan 2006")})
}

func checkCertFiles(o Options) []Check {
	cfg := o.Config
	var checks []Check
	var loaded []*x509.Certificate
	for _, pair := range cfg.TLS.Certificates {
		if _, err := tls.LoadX509KeyPair(pair.Cert, pair.Key); err != nil {
			checks = append(checks, Check{Name: "tls", Status: Fail, Detail: fmt.Sprintf("%s and its key: %v", pair.Cert, err), Fix: "see [[tls.certificates]] in " + cfg.Path})
			continue
		}
		cert, err := readFirstCert(pair.Cert)
		if err != nil {
			checks = append(checks, Check{Name: "tls", Status: Fail, Detail: err.Error()})
			continue
		}
		loaded = append(loaded, cert)
		checks = append(checks, expiry(o, filepath.Base(pair.Cert), cert, 30*24*time.Hour))
	}
	covers := func(host string) bool {
		return slices.ContainsFunc(loaded, func(c *x509.Certificate) bool { return c.VerifyHostname(host) == nil })
	}
	appsHost := "any-app." + cfg.Domain
	if cfg.Routing == "path" {
		appsHost = cfg.Domain
	}
	for _, host := range []string{cfg.ConsoleHost, appsHost} {
		if !covers(host) && len(loaded) > 0 {
			checks = append(checks, Check{Name: "tls", Status: Fail, Detail: "no certificate covers " + host, Fix: "use a certificate for " + cfg.ConsoleHost + " and *." + cfg.Domain})
		}
	}
	return checks
}

func checkContainers(ctx context.Context, o Options) Check {
	name := "containers"
	st := container.Probe(ctx, o.Config.Containers.Engine)
	wanted := o.WantsEngine != nil && o.WantsEngine(ctx)
	named := o.Config.Containers.Engine != "" && o.Config.Containers.Engine != "auto"
	switch st.State {
	case container.Ready:
		return Check{Name: name, Status: OK, Detail: st.Version + ", answering"}
	case container.NotAnswering:
		// One found by chance matters only to apps in containers, if there are any.
		status := Info
		switch {
		case named:
			status = Fail
		case wanted || o.WantsEngine == nil:
			status = Warn
		}
		return Check{Name: name, Status: status, Detail: fmt.Sprintf("%s is not answering, so apps in containers cannot run: %s", st.Kind, st.Reason), Fix: st.Advice()}
	}
	switch {
	case named:
		return Check{Name: name, Status: Fail, Detail: st.Reason, Fix: "see [containers] in " + o.Config.Path}
	case wanted:
		return Check{Name: name, Status: Warn, Detail: "no engine, and apps here run in containers", Fix: st.Advice()}
	}
	return Check{Name: name, Status: Info, Detail: "no engine: apps run natively, not in containers", Fix: "install Podman or Docker if you want them. Nothing else needs one"}
}

func checkData(o Options) []Check {
	cfg := o.Config
	dirs := layout.New(cfg.DataDir)
	if _, err := os.Stat(dirs.Database); errors.Is(err, os.ErrNotExist) {
		return []Check{{Name: "data", Status: Info, Detail: cfg.DataDir + " has no database yet: the server makes it when it first starts"}}
	}
	var checks []Check
	fi, err := os.Stat(dirs.Database)
	if err != nil {
		return []Check{{Name: "data", Status: Fail, Detail: err.Error(), Fix: "run moor server doctor as the server's user, or with sudo"}}
	}
	checks = append(checks, Check{Name: "data", Status: OK, Detail: fmt.Sprintf("%s, with a database of %s", cfg.DataDir, human(uint64(fi.Size())))})
	if o.GOOS != "windows" {
		if fi, err := os.Stat(dirs.Keys); err == nil && fi.Mode().Perm()&0o077 != 0 {
			checks = append(checks, Check{Name: "keys", Status: Fail, Detail: fmt.Sprintf("%s can be read by others (mode %o): it holds the master key", dirs.Keys, fi.Mode().Perm()),
				Fix: "chmod 700 " + dirs.Keys})
		}
	}
	if _, err := os.Stat(dirs.MasterKey); err == nil {
		checks = append(checks, Check{Name: "keys", Status: Info, Detail: "secrets are encrypted with " + dirs.MasterKey, Fix: "back it up apart from the data directory: without it they cannot be read"})
	}
	return checks
}

func human(n uint64) string {
	const gib, mib, kib = 1 << 30, 1 << 20, 1 << 10
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/kib)
}

func checkDisk(o Options) Check {
	dir := o.Config.DataDir
	for dir != filepath.Dir(dir) { // the nearest that exists
		if _, err := os.Stat(dir); err == nil {
			break
		}
		dir = filepath.Dir(dir)
	}
	free, total, err := platform.DiskSpace(dir)
	if err != nil {
		return Check{Name: "disk", Status: Info, Detail: "cannot tell: " + err.Error()}
	}
	return judgeDisk(dir, free, total)
}

func judgeDisk(dir string, free, total uint64) Check {
	c := Check{Name: "disk", Status: OK, Detail: fmt.Sprintf("%s free of %s, for %s", human(free), human(total), dir)}
	switch {
	case free < 1<<30:
		c.Status, c.Fix = Fail, "free some space: builds, logs and data need it"
	case free < 5<<30:
		c.Status, c.Fix = Warn, "free some space before builds and logs run out of it"
	}
	return c
}

// checkClock asks the system whether its clock is synchronised, where it can say.
func checkClock(ctx context.Context, o Options) (Check, bool) {
	if o.GOOS != "linux" {
		return Check{}, false
	}
	clockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := o.Command(clockCtx, "timedatectl", "show", "--property=NTPSynchronized", "--value")
	if err != nil {
		return Check{}, false
	}
	if strings.TrimSpace(string(out)) == "yes" {
		return Check{Name: "clock", Status: OK, Detail: "synchronised"}, true
	}
	return Check{Name: "clock", Status: Warn, Detail: "not synchronised: certificates and tokens depend on the time",
		Fix: "turn on time synchronisation: timedatectl set-ntp true"}, true
}

// checkKernel reports what isolation will use, in a later release.
// checkIsolation says how apps would be kept from one another, by a server run as this user.
func checkIsolation(o Options) Check {
	exe, _ := os.Executable()
	st, err := isolate.Probe(isolate.Config{Level: o.Config.Isolation.Level, DataDir: o.Config.DataDir, Binary: exe})
	if err != nil {
		return Check{Name: "isolation", Status: Fail, Detail: sentence(err.Error())}
	}
	c := Check{Name: "isolation", Status: OK, Detail: st.Describe()}
	if st.Level == isolate.None {
		c.Status = Info
		c.Fix = "Apps are kept apart on Linux. On a server that others use, run it as root there, as the system service does: each app then gets a user of its own."
	}
	if len(st.Notes) > 0 {
		c.Detail += " (" + strings.Join(st.Notes, "; ") + ")"
	}
	return c
}

func checkKernel(o Options) Check {
	path := func(p string) string { return filepath.Join(o.Root, filepath.FromSlash(p)) }
	var parts []string
	status := OK
	if data, err := os.ReadFile(path("/sys/fs/cgroup/cgroup.controllers")); err == nil {
		parts = append(parts, "cgroup v2 ("+strings.Join(strings.Fields(string(data)), ", ")+")")
	} else {
		parts = append(parts, "no cgroup v2")
		status = Info
	}
	if data, err := os.ReadFile(path("/sys/kernel/security/lsm")); err == nil && strings.Contains(string(data), "landlock") {
		parts = append(parts, "Landlock")
	} else {
		parts = append(parts, "no Landlock")
		status = Info
	}
	return Check{Name: "kernel", Status: status, Detail: strings.Join(parts, ", ") + ": for isolating native apps, in a later release"}
}

// checkApps looks for instances that listen beyond the loopback interface, where
// they can be reached without the router.
func checkApps(ctx context.Context, o Options) (Check, bool) {
	portsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ports, err := o.InstancePorts(portsCtx)
	if err != nil || len(ports) == 0 {
		return Check{}, false
	}
	addrs, err := o.AddrsFor()
	if err != nil {
		return Check{}, false
	}
	var open []string
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
			continue
		}
		for _, port := range ports {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(n.IP.String(), fmt.Sprint(port)), 300*time.Millisecond)
			if err == nil {
				conn.Close()
				open = append(open, fmt.Sprintf("%s:%d", n.IP, port))
			}
		}
	}
	if len(open) == 0 {
		return Check{Name: "apps", Status: OK, Detail: fmt.Sprintf("%d instances, reachable only through the router", len(ports))}, true
	}
	return Check{Name: "apps", Status: Warn, Detail: "an app listens on more than the loopback interface, and can be reached without the router: " + strings.Join(open, ", "),
		Fix: "make it listen on $HOST and $PORT"}, true
}

// Worst returns the worst status of some checks.
func Worst(checks []Check) string {
	worst := OK
	for _, c := range checks {
		switch c.Status {
		case Fail:
			return Fail
		case Warn:
			worst = Warn
		}
	}
	return worst
}

func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
