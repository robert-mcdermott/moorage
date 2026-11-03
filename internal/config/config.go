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
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
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

	// Path is the file the configuration was read from.
	Path string `toml:"-"`
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
}

// Log configures the server's own log.
type Log struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

// Values accepted by the enumerated settings.
var (
	routings      = []string{"host", "path"}
	tlsModes      = []string{"internal", "files", "off"}
	schemes       = []string{"http", "https"}
	logLevels     = []string{"debug", "info", "warn", "error"}
	logFormats    = []string{"text", "json"}
	plannedModes  = map[string]string{"acme": "ACME certificates are planned for a later release"}
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
	case plannedModes[c.TLS.Mode] != "":
		add("tls.mode %q is not available yet (%s); use %s", c.TLS.Mode, plannedModes[c.TLS.Mode], list(tlsModes))
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
