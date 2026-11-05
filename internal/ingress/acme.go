package ingress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/rfc2136"
	"github.com/mholt/acmez/v3/acme"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/robert-mcdermott/moorage/internal/config"
)

// ACMEDir is where a server keeps its ACME account and what it has been issued, under its certificates.
const ACMEDir = "acme"

// ACMEOptions are what certificates from an ACME server need.
type ACMEOptions struct {
	// Dir is where the account and the certificates are kept.
	Dir    string
	Config config.Config
	// Allow says which hostnames may have a certificate. Each one asked for from a public CA counts against a limit, so
	// that a hostname that is made up by whoever is connecting must not get one.
	Allow func(string) bool
	Log   *slog.Logger
	// Told hears that a certificate could not be had, or that it now has been.
	Told func(failed bool, name, problem string)
}

// ACME gets certificates from an ACME server: one for the whole domain by DNS, if it has been told how to set a record, and
// otherwise one for each hostname, the first time that it is asked for.
type ACME struct {
	options  ACMEOptions
	cache    *certmagic.Cache
	perHost  *certmagic.Config
	wildcard *certmagic.Config
	issuer   *certmagic.ACMEIssuer

	mu      sync.Mutex
	failing map[string]time.Time
}

// NewACME prepares it. Nothing is asked of the CA until Start, or a handshake.
func NewACME(o ACMEOptions) (*ACME, error) {
	a := o.Config.TLS.ACME
	directory := a.Directory
	if directory == "" {
		directory = config.LetsEncrypt
	}
	var roots *x509.CertPool
	if a.CAFile != "" {
		data, err := os.ReadFile(a.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tls.acme.ca_file: %w", err)
		}
		if roots, _ = x509.SystemCertPool(); roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("tls.acme.ca_file: %s has no certificates in it", a.CAFile)
		}
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	m := &ACME{options: o, failing: map[string]time.Time{}}
	logger := zap.New(slogCore{log: o.Log})
	storage := &certmagic.FileStorage{Path: o.Dir}
	m.cache = certmagic.NewCache(certmagic.CacheOptions{Logger: logger, GetConfigForCert: func(c certmagic.Certificate) (*certmagic.Config, error) {
		if m.wildcard != nil && slices.Contains(c.Names, "*."+o.Config.Domain) {
			return m.wildcard, nil
		}
		return m.perHost, nil
	}})
	issuer := func() certmagic.ACMEIssuer {
		return certmagic.ACMEIssuer{CA: directory, Email: a.Email, Agreed: true, TrustedRoots: roots, Logger: logger}
	}
	m.perHost = certmagic.New(m.cache, certmagic.Config{Storage: storage, Logger: logger, OnEvent: m.heard, OnDemand: &certmagic.OnDemandConfig{
		DecisionFunc: func(_ context.Context, name string) error {
			if !o.Allow(name) {
				return fmt.Errorf("this server does not serve %s", name)
			}
			return nil
		},
	}})
	each := issuer()
	each.DisableHTTPChallenge = len(a.Challenges) > 0 && !slices.Contains(a.Challenges, "http-01")
	each.DisableTLSALPNChallenge = len(a.Challenges) > 0 && !slices.Contains(a.Challenges, "tls-alpn-01")
	// A CA looks at the ports that are its own to look at. Where this server listens on others, as behind a port
	// forward or in a test, it is still this server that has to answer, and not something that the library would start.
	host, port, err := net.SplitHostPort(o.Config.Listen.HTTP)
	if err != nil {
		return nil, err
	}
	each.ListenHost = host
	each.AltHTTPPort = atoiOr(port, 80)
	if _, port, err := net.SplitHostPort(o.Config.Listen.HTTPS); err == nil {
		each.AltTLSALPNPort = atoiOr(port, 443)
	}
	m.issuer = certmagic.NewACMEIssuer(m.perHost, each)
	m.perHost.Issuers = []certmagic.Issuer{m.issuer}

	if d := a.DNS; d.Provider != "" {
		solver, err := dnsSolver(d)
		if err != nil {
			return nil, err
		}
		m.wildcard = certmagic.New(m.cache, certmagic.Config{Storage: storage, Logger: logger, OnEvent: m.heard})
		byDNS := issuer()
		byDNS.DNS01Solver = solver
		m.wildcard.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(m.wildcard, byDNS)}
	}
	return m, nil
}

func atoiOr(s string, fallback int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return fallback
	}
	return n
}

// dnsSolver is how a record is set, to show a CA that the domain is this server's.
func dnsSolver(d config.ACMEDNS) (certmagicSolver, error) {
	if d.Provider == "command" {
		wait := 30 * time.Second
		if d.Wait != "" {
			var err error
			if wait, err = time.ParseDuration(d.Wait); err != nil {
				return nil, err
			}
		}
		return &commandSolver{command: d.Command, wait: wait}, nil
	}
	secret := func(what, file string) (string, error) {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("tls.acme.dns.%s: %w", what, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	solver := &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{Resolvers: d.Resolvers, PropagationTimeout: 5 * time.Minute}}
	switch d.Provider {
	case "cloudflare":
		token, err := secret("token_file", d.TokenFile)
		if err != nil {
			return nil, err
		}
		solver.DNSProvider = &cloudflare.Provider{APIToken: token}
	case "rfc2136":
		key, err := secret("key_file", d.KeyFile)
		if err != nil {
			return nil, err
		}
		algorithm := d.KeyAlgorithm
		if algorithm == "" {
			algorithm = "hmac-sha256"
		}
		solver.DNSProvider = &rfc2136.Provider{Server: d.Server, KeyName: d.KeyName, KeyAlg: algorithm, Key: key}
	default:
		return nil, fmt.Errorf("tls.acme.dns.provider %q is not one this server knows", d.Provider)
	}
	return solver, nil
}

// certmagicSolver is what an issuer takes for DNS-01.
type certmagicSolver interface {
	Present(context.Context, acme.Challenge) error
	CleanUp(context.Context, acme.Challenge) error
}

// commandSolver sets a record with a program of the server's owner's, for any DNS service at all.
type commandSolver struct {
	command string
	wait    time.Duration
}

func (s *commandSolver) Present(ctx context.Context, c acme.Challenge) error {
	return s.run(ctx, "add", c)
}

func (s *commandSolver) CleanUp(ctx context.Context, c acme.Challenge) error {
	return s.run(ctx, "remove", c)
}

// Wait gives the record time to reach the servers the CA asks.
func (s *commandSolver) Wait(ctx context.Context, _ acme.Challenge) error {
	select {
	case <-time.After(s.wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *commandSolver) run(ctx context.Context, action string, c acme.Challenge) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.command, action, c.DNS01TXTRecordName(), c.DNS01KeyAuthorization())
	cmd.WaitDelay = 2 * time.Second // what it left running holds its output, and issuing is not to wait for that
	if out, err := cmd.CombinedOutput(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		if len(out) > 400 {
			out = out[:400]
		}
		return fmt.Errorf("%s %s: %w: %s", s.command, action, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start asks for what is known to be wanted, and goes on renewing it until ctx ends.
func (m *ACME) Start(ctx context.Context) error {
	context.AfterFunc(ctx, m.cache.Stop)
	if m.wildcard != nil {
		return m.wildcard.ManageAsync(ctx, []string{"*." + m.options.Config.Domain})
	}
	return nil
}

// While a certificate is issued for a first visit, a handshake waits for it. The server's limit on a handshake is meant for what a client
// does, so that it applies again once the certificate has come.
const (
	waitForIssuing = 3 * time.Minute
	afterIssuing   = 10 * time.Second
)

// patiently lets a handshake wait for what get does, and leaves it no longer than usual after that.
func patiently(hello *tls.ClientHelloInfo, get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (*tls.Certificate, error) {
	if hello.Conn != nil {
		hello.Conn.SetDeadline(time.Now().Add(waitForIssuing))
		defer func() { hello.Conn.SetDeadline(time.Now().Add(afterIssuing)) }()
	}
	return get(hello)
}

// GetCertificate chooses the certificate for a handshake, and gets one if it is allowed and there is none.
func (m *ACME) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if m.wildcard != nil && m.covered(hello.ServerName) && !slices.Contains(hello.SupportedProtos, "acme-tls/1") {
		return m.wildcard.GetCertificate(hello)
	}
	return patiently(hello, m.perHost.GetCertificate)
}

// covered says whether the domain's wildcard is good for a hostname: one label under it, and no more.
func (m *ACME) covered(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	label, ok := strings.CutSuffix(name, "."+m.options.Config.Domain)
	return ok && label != "" && !strings.Contains(label, ".")
}

// TLSConfig is what the HTTPS listener serves with.
func (m *ACME) TLSConfig() *tls.Config {
	c := TLSConfig(m)
	if !m.issuer.DisableTLSALPNChallenge {
		c.NextProtos = append(c.NextProtos, "acme-tls/1")
	}
	return c
}

// HTTPChallenges answers a CA's HTTP-01 requests on the plain listener, and passes on anything else.
func (m *ACME) HTTPChallenges(next http.Handler) http.Handler {
	return m.issuer.HTTPChallengeHandler(next)
}

// heard hears what the library did, and says what matters.
func (m *ACME) heard(_ context.Context, event string, data map[string]any) error {
	name, _ := data["identifier"].(string)
	switch event {
	case "cert_obtained":
		renewal, _ := data["renewal"].(bool)
		m.options.Log.Info("got a certificate", "name", name, "renewal", renewal)
		m.mu.Lock()
		_, was := m.failing[name]
		delete(m.failing, name)
		m.mu.Unlock()
		if was && m.options.Told != nil {
			m.options.Told(false, name, "")
		}
	case "cert_failed":
		problem := fmt.Sprint(data["error"])
		m.options.Log.Warn("could not get a certificate", "name", name, "err", problem)
		m.mu.Lock()
		last, was := m.failing[name]
		tell := !was || time.Since(last) > 6*time.Hour
		if tell {
			m.failing[name] = time.Now()
		}
		m.mu.Unlock()
		if tell && m.options.Told != nil {
			m.options.Told(true, name, problem)
		}
	}
	return nil
}

// A CertificateOnDisk is one that a server has been issued.
type CertificateOnDisk struct {
	Names    []string
	NotAfter time.Time
}

// ACMECertificates lists what is kept in dir. It reads files, and so works whether or not the server is running.
func ACMECertificates(dir string) ([]CertificateOnDisk, error) {
	var found []CertificateOnDisk
	root := filepath.Join(dir, "certificates")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".crt") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return nil
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil
		}
		found = append(found, CertificateOnDisk{Names: cert.DNSNames, NotAfter: cert.NotAfter})
		return nil
	})
	slices.SortFunc(found, func(a, b CertificateOnDisk) int { return a.NotAfter.Compare(b.NotAfter) })
	return found, err
}

// slogCore sends what the library logs to the server's log: what went wrong, and not each step.
type slogCore struct {
	log    *slog.Logger
	fields []zapcore.Field
}

func (c slogCore) Enabled(l zapcore.Level) bool { return l >= zapcore.WarnLevel }

func (c slogCore) With(fields []zapcore.Field) zapcore.Core {
	return slogCore{log: c.log, fields: append(slices.Clip(c.fields), fields...)}
}

func (c slogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c slogCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	encoder := zapcore.NewMapObjectEncoder()
	for _, f := range append(slices.Clip(c.fields), fields...) {
		f.AddTo(encoder)
	}
	args := make([]any, 0, 2*len(encoder.Fields))
	for k, v := range encoder.Fields {
		args = append(args, k, v)
	}
	level := slog.LevelWarn
	if e.Level >= zapcore.ErrorLevel {
		level = slog.LevelError
	}
	c.log.Log(context.Background(), level, "acme: "+e.Message, args...)
	return nil
}

func (c slogCore) Sync() error { return nil }
