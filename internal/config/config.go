// Package config reads, validates and writes server.toml, the bootstrap
// configuration of a Moorage server: what the server needs before its
// database is open. Everything else is configured at runtime.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/robert-mcdermott/moorage/internal/names"
)

// Config is the content of server.toml.
type Config struct {
	DataDir     string     `toml:"data_dir"`
	Domain      string     `toml:"domain"`
	ConsoleHost string     `toml:"console_host"`
	Routing     string     `toml:"routing"`
	Listen      Listen     `toml:"listen"`
	TLS         TLS        `toml:"tls"`
	Proxy       Proxy      `toml:"proxy"`
	Toolchains  Toolchains `toml:"toolchains"`
	Containers  Containers `toml:"containers"`
	Log         Log        `toml:"log"`
	Access      Access     `toml:"access"`
	// Notifications says what they may reach.
	Notifications Notifications `toml:"notifications"`
	// Isolation says how apps are kept from one another.
	Isolation Isolation `toml:"isolation"`
	// Auth says how people sign in, beyond what the console can set.
	Auth Auth `toml:"auth"`

	// Path is the file the configuration was read from.
	Path string `toml:"-"`
}

// Access says who may reach each kind of environment, unless the manifest asks otherwise or a
// maintainer has set otherwise.
type Access struct {
	Production string `toml:"production"`
	Named      string `toml:"named"`
	Preview    string `toml:"preview"`
}

// Auth is how people sign in, in what has to be settled before anyone can.
type Auth struct {
	// TrustedHeader names the header in which a proxy that signs people in says who they are. It
	// counts only from the addresses in [proxy] trusted, which have to replace any that a client sends.
	TrustedHeader string `toml:"trusted_header"`
	// TrustedNameHeader names the header with their name, if there is one.
	TrustedNameHeader string `toml:"trusted_name_header"`
	// TrustedProvision makes an account for someone new, the first time the proxy says who they are.
	TrustedProvision bool `toml:"trusted_provision"`
}

// Isolation is how apps are kept from one another (docs/isolation.md).
type Isolation struct {
	// Level is "auto", "none", "user" or "sandbox".
	Level string `toml:"level"`
	// UIDBase is where the users of apps start.
	UIDBase uint32 `toml:"uid_base"`
}

// Notifications is how notifications are sent.
type Notifications struct {
	// Allow lists addresses and prefixes that webhooks and mail servers may be at, though they
	// would be refused as this machine or a link-local address, which is the cloud's metadata.
	Allow []string `toml:"allow"`
	// DiskPercent is how full the data volume may get before someone is told. Nothing means 90.
	DiskPercent float64 `toml:"disk_percent"`
}

// accessModes are the gates an environment can have.
var accessModes = []string{"public", "password", "login", "share"}

// DefaultAccess says what gate an environment of a kind has, if nobody has said.
func (c Config) DefaultAccess(kind string) string {
	switch kind {
	case "preview":
		return c.Access.Preview
	case "named":
		return c.Access.Named
	}
	return c.Access.Production
}

// GateFor says what gate an environment has, and where that comes from (setting, manifest or
// server): what has been set, or else what its manifest asks, or else the server's default for
// its kind. Setting a gate takes a maintainer. A manifest is a deployer's, so it may ask for a
// gate where the server would have none, and it may not open what the server has closed.
func (c Config) GateFor(set, asked, kind string) (mode, source string) {
	if set != "" {
		return set, "setting"
	}
	fallback := c.DefaultAccess(kind)
	if asked != "" && (asked != "public" || fallback == "public") {
		return asked, "manifest"
	}
	return fallback, "server"
}

// Listen holds the listening addresses; an empty address disables the
// listener (all but HTTP may be empty).
type Listen struct {
	HTTP  string `toml:"http"`
	HTTPS string `toml:"https"`
	SSH   string `toml:"ssh"`
	// DNS, if set, is a loopback address where the server answers DNS queries
	// for its domain, for moor server resolver.
	DNS string `toml:"dns"`
}

// TLS selects how Moorage terminates TLS, if at all.
type TLS struct {
	// Mode is "internal" (certificates from Moorage's CA), "files"
	// (certificate and key files) or "off" (plain HTTP, for example behind a
	// TLS-terminating proxy).
	Mode string `toml:"mode"`
	// CACert and CAKey optionally name an intermediate CA for "internal";
	// without them Moorage generates its own root.
	CACert string `toml:"ca_cert"`
	CAKey  string `toml:"ca_key"`
	// Certificates are the certificate and key pairs for "files".
	Certificates []Certificate `toml:"certificates"`
	// ACME says where certificates come from with "acme".
	ACME ACME `toml:"acme"`
	// HSTS tells browsers to use nothing but HTTPS here from now on, for each hostname that says so.
	HSTS bool `toml:"hsts"`
}

// LetsEncrypt is the directory that ACME certificates come from, unless one is named.
const LetsEncrypt = "https://acme-v02.api.letsencrypt.org/directory"

// ACME says where certificates come from, and how they are asked for.
type ACME struct {
	// Directory is the address of the ACME server's directory: a public CA's or an internal one's.
	Directory string `toml:"directory"`
	// Email is who the CA tells of what it has to say about the account.
	Email string `toml:"email"`
	// AgreeTerms says that the CA's terms have been read and accepted, which it asks for.
	AgreeTerms bool `toml:"agree_terms"`
	// CAFile holds the certificates to trust an internal ACME server's address by.
	CAFile string `toml:"ca_file"`
	// Challenges are how a hostname is shown to be this server's, of "http-01" and "tls-alpn-01". Nothing means both.
	Challenges []string `toml:"challenges"`
	// DNS, with a provider, gets a wildcard certificate for the whole domain.
	DNS ACMEDNS `toml:"dns"`
}

// ACMEDNS says how a TXT record is set, to show that a domain is this server's by DNS-01. Each secret is in a file of its
// own, so that server.toml can be read by those who should not have it.
type ACMEDNS struct {
	// Provider is "cloudflare", "rfc2136" or "command".
	Provider string `toml:"provider"`
	// TokenFile holds a Cloudflare API token.
	TokenFile string `toml:"token_file"`
	// Server is the DNS server that takes RFC 2136 updates, as host:port.
	Server string `toml:"server"`
	// KeyName, KeyAlgorithm and KeyFile are its TSIG key. The file holds the base64 secret.
	KeyName      string `toml:"key_name"`
	KeyAlgorithm string `toml:"key_algorithm"`
	KeyFile      string `toml:"key_file"`
	// Command is a program that is run as Command add|remove NAME VALUE, to set and take away a TXT record.
	Command string `toml:"command"`
	// Resolvers are the DNS servers to look for the record through, as host:port. Nothing means the system's.
	Resolvers []string `toml:"resolvers"`
	// Wait is how long to give a record to reach the CA, as a duration, when it is set by a command. Nothing means 30s.
	Wait string `toml:"wait"`
}

// Certificate is a certificate chain and its private key, in PEM files.
type Certificate struct {
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
}

// Proxy describes TLS-terminating proxies in front of Moorage.
type Proxy struct {
	// Trusted lists addresses or CIDR prefixes whose X-Forwarded-* headers
	// and PROXY protocol are honoured.
	Trusted []string `toml:"trusted"`
	// ExternalScheme is the scheme browsers see: "http" or "https".
	ExternalScheme string `toml:"external_scheme"`
}

// Containers says which engine runs apps in containers: "auto" for Podman if
// it is installed, or else Docker, or the path of a binary.
type Containers struct {
	Engine string `toml:"engine"`
}

// Toolchains says where build toolchains are downloaded from: mirrors of
// the official locations, for proxies and air-gapped networks. Empty
// values mean the official ones.
type Toolchains struct {
	// GoURL serves Go's download index and files (as https://go.dev/dl/).
	GoURL string `toml:"go_url"`
	// NodeURL serves Node.js releases (as https://nodejs.org/dist/).
	NodeURL string `toml:"node_url"`
	// UVURL serves uv's releases (as https://github.com/astral-sh/uv/releases).
	UVURL string `toml:"uv_url"`
	// PythonURL serves the Python builds uv installs.
	PythonURL string `toml:"python_url"`
	// Signatures says what to do with a release whose signature cannot be checked: "require" refuses it,
	// which is the default, and "warn" goes on and says so. A signature that is wrong is always refused.
	Signatures string `toml:"signatures"`
}

// Log configures the server's own log.
type Log struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

// Values accepted by the enumerated settings.
var (
	routings      = []string{"host", "path"}
	tlsModes      = []string{"internal", "files", "acme", "off"}
	schemes       = []string{"http", "https"}
	logLevels     = []string{"debug", "info", "warn", "error"}
	logFormats    = []string{"text", "json"}
	envOverriders = []struct {
		name string
		set  func(*Config, string)
	}{
		{"MOORAGE_DATA_DIR", func(c *Config, v string) { c.DataDir = v }},
		{"MOORAGE_LOG_LEVEL", func(c *Config, v string) { c.Log.Level = v }},
		{"MOORAGE_LOG_FORMAT", func(c *Config, v string) { c.Log.Format = v }},
	}
)

// Load reads the configuration at path, applies MOORAGE_* environment
// overrides (looked up with getenv) and defaults, and validates it.
func Load(path string, getenv func(string) string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := Parse(data, path)
	if err != nil {
		return Config{}, err
	}
	cfg.Path = path
	cfg.ApplyEnv(getenv)
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Parse decodes a configuration strictly: unknown keys are errors. name is
// used in error messages.
func Parse(data []byte, name string) (Config, error) {
	var cfg Config
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, decodeError(name, err)
	}
	return cfg, nil
}

func decodeError(name string, err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		var lines []string
		for _, e := range strict.Errors {
			row, col := e.Position()
			lines = append(lines, fmt.Sprintf("%s:%d:%d: unknown key %q", name, row, col, strings.Join(e.Key(), ".")))
		}
		return errors.New(strings.Join(lines, "\n"))
	}
	var decode *toml.DecodeError
	if errors.As(err, &decode) {
		row, col := decode.Position()
		return fmt.Errorf("%s:%d:%d: %s", name, row, col, decode.Error())
	}
	return fmt.Errorf("%s: %w", name, err)
}

// ApplyEnv overrides settings from MOORAGE_DATA_DIR, MOORAGE_LOG_LEVEL and
// MOORAGE_LOG_FORMAT when they are set.
func (c *Config) ApplyEnv(getenv func(string) string) {
	for _, o := range envOverriders {
		if v := getenv(o.name); v != "" {
			o.set(c, v)
		}
	}
}

// ApplyDefaults fills in settings left empty and normalises hostnames.
func (c *Config) ApplyDefaults() {
	c.Domain = strings.ToLower(strings.TrimSpace(c.Domain))
	c.ConsoleHost = strings.ToLower(strings.TrimSpace(c.ConsoleHost))
	if c.ConsoleHost == "" && c.Domain != "" {
		c.ConsoleHost = "moorage." + c.Domain
	}
	if c.Routing == "" {
		c.Routing = "host"
	}
	if c.Access.Production == "" {
		c.Access.Production = "public"
	}
	if c.Access.Named == "" {
		c.Access.Named = "public"
	}
	if c.Access.Preview == "" {
		c.Access.Preview = "login" // work in progress, with production's variables
	}
	if c.TLS.Mode == "" {
		if c.Listen.HTTPS != "" {
			c.TLS.Mode = "internal"
		} else {
			c.TLS.Mode = "off"
		}
	}
	if c.Proxy.ExternalScheme == "" {
		if c.TLS.Mode != "off" || len(c.Proxy.Trusted) > 0 {
			c.Proxy.ExternalScheme = "https"
		} else {
			c.Proxy.ExternalScheme = "http"
		}
	}
	if c.Containers.Engine == "" {
		c.Containers.Engine = "auto"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
}

// WildcardCertificate says whether this server gets one certificate for its whole domain, by DNS.
func (c Config) WildcardCertificate() bool {
	return c.TLS.Mode == "acme" && c.TLS.ACME.DNS.Provider != ""
}

// validateACME checks what [tls.acme] says.
func (c Config) validateACME(add func(string, ...any)) {
	a := c.TLS.ACME
	if c.TLS.Mode != "acme" {
		if a.Directory != "" || a.Email != "" || a.AgreeTerms || a.CAFile != "" || len(a.Challenges) > 0 || a.DNS.Provider != "" {
			add("[tls.acme] applies only to tls.mode \"acme\"")
		}
		return
	}
	if c.Domain == "localhost" || strings.HasSuffix(c.Domain, ".localhost") {
		add("tls.mode \"acme\" needs a domain that a CA can check: %q is only this computer", c.Domain)
	}
	if !a.AgreeTerms {
		add("tls.acme.agree_terms must be true: a CA asks you to accept its terms, so read them, and say so here")
	}
	if a.Directory != "" {
		if u, err := url.Parse(a.Directory); err != nil || u.Scheme != "https" || u.Host == "" {
			add("tls.acme.directory %q must be an https address", a.Directory)
		}
	}
	for _, challenge := range a.Challenges {
		if !oneOf(challenge, []string{"http-01", "tls-alpn-01"}) {
			add("tls.acme.challenges: %q must be http-01 or tls-alpn-01: DNS-01 is what tls.acme.dns does", challenge)
		}
	}
	d := a.DNS
	switch d.Provider {
	case "":
		if d.TokenFile != "" || d.Server != "" || d.KeyFile != "" || d.KeyName != "" || d.Command != "" || len(d.Resolvers) > 0 || d.Wait != "" {
			add("tls.acme.dns needs a provider: cloudflare, rfc2136 or command")
		}
	case "cloudflare":
		if d.TokenFile == "" {
			add("tls.acme.dns.token_file is required with the provider cloudflare: the file that holds its API token")
		}
	case "rfc2136":
		if d.Server == "" || d.KeyName == "" || d.KeyFile == "" {
			add("tls.acme.dns.server, key_name and key_file are required with the provider rfc2136")
		} else if _, _, err := net.SplitHostPort(d.Server); err != nil {
			add("tls.acme.dns.server %q must be a host and a port, such as ns1.example.com:53", d.Server)
		}
	case "command":
		if !filepath.IsAbs(d.Command) {
			add("tls.acme.dns.command is required with the provider command, and must be an absolute path")
		}
	default:
		add("tls.acme.dns.provider %q must be cloudflare, rfc2136 or command", d.Provider)
	}
	if d.Wait != "" {
		if wait, err := time.ParseDuration(d.Wait); err != nil || wait < 0 {
			add("tls.acme.dns.wait %q is not a duration, such as 30s", d.Wait)
		}
	}
	for _, r := range d.Resolvers {
		if _, _, err := net.SplitHostPort(r); err != nil {
			add("tls.acme.dns.resolvers: %q must be a host and a port, such as 1.1.1.1:53", r)
		}
	}
}

// ValidationError lists everything wrong with a configuration.
type ValidationError struct {
	Path     string
	Problems []string
}

func (e *ValidationError) Error() string {
	where := e.Path
	if where == "" {
		where = "configuration"
	}
	return "invalid " + where + ":\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Validate checks every setting and reports all problems at once.
func (c Config) Validate() error {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	switch {
	case c.DataDir == "":
		add("data_dir is required")
	case !filepath.IsAbs(c.DataDir):
		add("data_dir %q must be an absolute path", c.DataDir)
	}
	if !validHostname(c.Domain) {
		add("domain %q is not a valid DNS name (for example apps.example.com, or localhost)", c.Domain)
	}
	if !validHostname(c.ConsoleHost) {
		add("console_host %q is not a valid DNS name", c.ConsoleHost)
	}
	for kind, mode := range map[string]string{"production": c.Access.Production, "named": c.Access.Named, "preview": c.Access.Preview} {
		if mode != "" && !oneOf(mode, accessModes) {
			add("access.%s %q must be one of %s", kind, mode, list(accessModes))
		}
	}
	if c.Routing == "path" && c.ConsoleHost == c.Domain {
		add("with routing = \"path\", console_host must be another hostname than domain: pages of apps must not share an origin with the console, or a script in any of them could act as whoever is signed in")
	}
	if c.Auth.TrustedHeader != "" && len(c.Proxy.Trusted) == 0 {
		add("auth.trusted_header needs proxy.trusted: otherwise anyone could say who they are")
	}
	if !oneOf(c.Toolchains.Signatures, []string{"", "require", "warn"}) {
		add("toolchains.signatures %q must be require or warn", c.Toolchains.Signatures)
	}
	if !oneOf(c.Isolation.Level, []string{"", "auto", "none", "user", "sandbox"}) {
		add("isolation.level %q must be auto, none, user or sandbox", c.Isolation.Level)
	}
	if c.Isolation.UIDBase != 0 && (c.Isolation.UIDBase < 1000 || c.Isolation.UIDBase > 1<<30) {
		add("isolation.uid_base %d must be between 1000 and %d", c.Isolation.UIDBase, 1<<30)
	}
	if d := c.Notifications.DiskPercent; d != 0 && (d < 10 || d > 100) {
		add("notifications.disk_percent %v must be between 10 and 100", d)
	}
	for _, a := range c.Notifications.Allow {
		if _, err := ParseTrusted(a); err != nil {
			add("notifications.allow: %q is not an address or a prefix", a)
		}
	}
	if !oneOf(c.Routing, routings) {
		add("routing %q must be one of %s", c.Routing, list(routings))
	}

	if c.Listen.HTTP == "" {
		add("listen.http is required")
	} else if err := checkListen(c.Listen.HTTP); err != nil {
		add("listen.http: %v", err)
	}
	if c.Listen.DNS != "" {
		host, _, err := net.SplitHostPort(c.Listen.DNS)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			add("listen.dns: %q must be a loopback address and a port, such as 127.0.0.1:15353: it answers only for this computer", c.Listen.DNS)
		}
	}
	for _, l := range []struct{ name, addr string }{{"listen.https", c.Listen.HTTPS}, {"listen.ssh", c.Listen.SSH}} {
		if l.addr != "" {
			if err := checkListen(l.addr); err != nil {
				add("%s: %v", l.name, err)
			}
		}
	}

	switch {
	case !oneOf(c.TLS.Mode, tlsModes):
		add("tls.mode %q must be one of %s", c.TLS.Mode, list(tlsModes))
	case c.TLS.Mode == "off" && c.Listen.HTTPS != "":
		add("listen.https is set but tls.mode is \"off\"; remove listen.https or choose a TLS mode")
	case c.TLS.Mode != "off" && c.Listen.HTTPS == "":
		add("tls.mode %q needs listen.https", c.TLS.Mode)
	}
	if (c.TLS.CACert == "") != (c.TLS.CAKey == "") {
		add("tls.ca_cert and tls.ca_key must be given together")
	}
	if c.TLS.CACert != "" && c.TLS.Mode != "internal" {
		add("tls.ca_cert and tls.ca_key apply only to tls.mode \"internal\"")
	}
	c.validateACME(add)
	if c.TLS.HSTS && c.TLS.Mode == "off" {
		add("tls.hsts needs Moorage to terminate TLS: behind a proxy, the proxy sends it")
	}
	if c.TLS.Mode == "files" && len(c.TLS.Certificates) == 0 {
		add("tls.mode \"files\" needs at least one [[tls.certificates]] entry with cert and key")
	}
	if len(c.TLS.Certificates) > 0 && c.TLS.Mode != "files" {
		add("[[tls.certificates]] apply only to tls.mode \"files\"")
	}
	for i, crt := range c.TLS.Certificates {
		if crt.Cert == "" || crt.Key == "" {
			add("tls.certificates[%d] needs both cert and key", i)
		}
	}

	for _, t := range c.Proxy.Trusted {
		if _, err := ParseTrusted(t); err != nil {
			add("proxy.trusted: %v", err)
		}
	}
	if !oneOf(c.Proxy.ExternalScheme, schemes) {
		add("proxy.external_scheme %q must be one of %s", c.Proxy.ExternalScheme, list(schemes))
	}

	for key, u := range map[string]string{"go_url": c.Toolchains.GoURL, "node_url": c.Toolchains.NodeURL, "uv_url": c.Toolchains.UVURL, "python_url": c.Toolchains.PythonURL} {
		if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			add("toolchains.%s %q must be an http:// or https:// URL", key, u)
		}
	}

	if !oneOf(c.Log.Level, logLevels) {
		add("log.level %q must be one of %s", c.Log.Level, list(logLevels))
	}
	if !oneOf(c.Log.Format, logFormats) {
		add("log.format %q must be one of %s", c.Log.Format, list(logFormats))
	}

	if len(p) > 0 {
		return &ValidationError{Path: c.Path, Problems: p}
	}
	return nil
}

// ParseTrusted parses a proxy.trusted entry: a CIDR prefix, or a single
// address (which becomes a prefix of full length).
func ParseTrusted(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not an address or CIDR prefix", s)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an address or CIDR prefix", s)
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ReleaseURL is the address of one release of a static site. With path routing there is none: apps
// have no hostnames of their own. Nor is there with a certificate from a CA for each hostname: anyone could spend a week's limit on
// them by visiting them, so that those take a wildcard.
func (c Config) ReleaseURL(app, env string, version int) string {
	if c.Routing == "path" || c.TLS.Mode == "acme" && !c.WildcardCertificate() {
		return ""
	}
	return c.PublicURL(names.PermalinkLabel(app, env, version) + "." + c.Domain)
}

// NotificationsAllowed parses [notifications] allow.
func (c Config) NotificationsAllowed() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(c.Notifications.Allow))
	for _, a := range c.Notifications.Allow {
		p, err := ParseTrusted(a)
		if err != nil {
			return nil, fmt.Errorf("notifications.allow: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
}

// PublicURL is how a browser reaches host: through the proxy when Moorage is
// behind one, otherwise on Moorage's own listener, with the port when it is
// not the scheme's default.
func (c Config) PublicURL(host string) string {
	port := ""
	if len(c.Proxy.Trusted) == 0 {
		addr, def := c.Listen.HTTP, "80"
		if c.TLS.Mode != "off" {
			addr, def = c.Listen.HTTPS, "443"
		}
		if _, p, err := net.SplitHostPort(addr); err == nil && p != def {
			port = ":" + p
		}
	}
	return c.Proxy.ExternalScheme + "://" + host + port
}

// EnvHost is the hostname of an app's environment. With path routing they all share one.
func (c Config) EnvHost(app, env string) string {
	if c.Routing == "path" {
		return c.Domain
	}
	return names.Label(app, env) + "." + c.Domain
}

// EnvPrefix is the path an app's environment is served at, if any: with path routing, its own.
func (c Config) EnvPrefix(app, env string) string {
	if c.Routing == "path" {
		return "/" + names.Label(app, env)
	}
	return ""
}

// EnvURL is the address of an app's environment.
func (c Config) EnvURL(app, env string) string {
	return c.PublicURL(c.EnvHost(app, env)) + c.EnvPrefix(app, env)
}

func checkListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port (for example :80 or 127.0.0.1:8080)", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%q has an invalid port", addr)
	}
	if host != "" && net.ParseIP(host) == nil && !validHostname(strings.ToLower(host)) {
		return fmt.Errorf("%q has an invalid host", addr)
	}
	return nil
}

// validHostname reports whether s is a lowercase DNS name of letters, digits
// and hyphens, without a trailing dot.
func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func oneOf(s string, values []string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}

func list(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = strconv.Quote(v)
	}
	return strings.Join(q, ", ")
}
