package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Presets are the starting points `moor server init` offers.
var Presets = []string{"lan", "local"}

// PresetOptions are the choices passed to a preset.
type PresetOptions struct {
	// Domain is the base domain; required for "lan", "localhost" by default
	// for "local".
	Domain string
	// DataDir is the absolute path of the data directory.
	DataDir string
	// BehindProxy selects plain HTTP on 127.0.0.1:8080 behind a
	// TLS-terminating proxy on the same machine ("lan" only).
	BehindProxy bool
	// Routing is how apps are told apart: host, the default, or path.
	Routing string
	// ConsoleHost, if set, is the console's hostname, instead of moorage.<domain>.
	ConsoleHost string
}

// Preset returns the configuration a preset starts from:
//   - "local": this computer only — *.localhost on 127.0.0.1:8080, no TLS;
//   - "lan": a server on an internal network — ports 80, 443 and 2222 with
//     Moorage's internal CA, or plain HTTP behind a TLS proxy.
func Preset(name string, o PresetOptions) (Config, error) {
	var c Config
	switch name {
	case "local":
		if o.BehindProxy {
			return Config{}, errors.New("--behind-proxy applies to the lan preset; the local preset serves this computer only")
		}
		domain := o.Domain
		if domain == "" {
			domain = "localhost"
		}
		c = Config{
			Domain: domain,
			Listen: Listen{HTTP: "127.0.0.1:8080", SSH: "127.0.0.1:2222"},
			TLS:    TLS{Mode: "off"},
			Proxy:  Proxy{ExternalScheme: "http"},
		}
	case "lan":
		if o.Domain == "" {
			return Config{}, errors.New("the lan preset needs --domain: the domain whose wildcard DNS record points at this server, such as apps.example.com")
		}
		c = Config{
			Domain: o.Domain,
			Listen: Listen{HTTP: ":80", HTTPS: ":443", SSH: ":2222"},
			TLS:    TLS{Mode: "internal"},
			Proxy:  Proxy{ExternalScheme: "https"},
		}
		if o.BehindProxy {
			c.Listen = Listen{HTTP: "127.0.0.1:8080", SSH: ":2222"}
			c.TLS = TLS{Mode: "off"}
			c.Proxy = Proxy{Trusted: []string{"127.0.0.1", "::1"}, ExternalScheme: "https"}
		}
	default:
		return Config{}, fmt.Errorf("unknown preset %q: choose %s", name, list(Presets))
	}
	c.DataDir = o.DataDir
	c.Routing = "host"
	if o.Routing != "" {
		c.Routing = o.Routing
	}
	if o.ConsoleHost != "" {
		c.ConsoleHost = o.ConsoleHost
	}
	c.Log = Log{Level: "info", Format: "text"}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Write renders c as a commented server.toml and writes it to path,
// replacing an existing file only if force is set. The file is written to a
// temporary name and renamed into place.
func Write(path string, c Config, preset string, force bool) error {
	data, err := Render(c, preset)
	if err != nil {
		return err
	}
	if _, err := Parse(data, path); err != nil {
		return fmt.Errorf("internal error: the configuration written would not parse: %w", err)
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; pass --force to replace it", path)
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".server.toml-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Render formats c as server.toml with comments explaining each setting.
func Render(c Config, preset string) ([]byte, error) {
	var b bytes.Buffer
	err := fileTemplate.Execute(&b, struct {
		Config
		Preset string
	}{c, preset})
	return b.Bytes(), err
}

var fileTemplate = template.Must(template.New("server.toml").Funcs(template.FuncMap{
	"q":    quote,
	"list": quoteList,
}).Parse(`# Moorage server configuration: what the server needs before its database
# is open. Written by ` + "`moor server init --preset {{.Preset}}`" + `; edit it and restart the
# server to apply. Everything else is configured in the console or the CLI.

# Where Moorage keeps its database, builds, logs and app data.
data_dir = {{q .DataDir}}

# Apps are served at <app>.<domain>, previews at <app>--<branch>.<domain>.
# Point a wildcard DNS record (*.<domain>) at this server; browsers resolve
# *.localhost to this computer by themselves.
domain = {{q .Domain}}

# Hostname of the console, the API and HTTPS git pushes.
console_host = {{q .ConsoleHost}}

# "host": every app on its own hostname (needs the wildcard record).
# "path": apps under /<app>/ on the domain, with no wildcard record: the console
# needs a hostname of its own, and apps share an origin with one another.
routing = {{q .Routing}}

# Who may reach each kind of environment, unless its manifest or a maintainer says
# otherwise: "public", "password", "login" or "share".
[access]
production = {{q .Access.Production}}
named      = {{q .Access.Named}}
preview    = {{q .Access.Preview}}

{{- if .Auth.TrustedHeader}}
[auth]
trusted_header = {{q .Auth.TrustedHeader}}
{{- if .Auth.TrustedNameHeader}}
trusted_name_header = {{q .Auth.TrustedNameHeader}}
{{- end}}
trusted_provision = {{.Auth.TrustedProvision}}

{{- end}}
[isolation]
# How apps are kept from one another: "auto" is "user" where the server runs as root on
# Linux and "none" elsewhere. "user" gives each app an operating system user of its own, and
# "sandbox" also confines it to its own files with Landlock.
{{- if .Isolation.Level}}
level = {{q .Isolation.Level}}
{{- else}}
# level = "auto"
{{- end}}
{{- if .Isolation.UIDBase}}
uid_base = {{.Isolation.UIDBase}}
{{- end}}

[notifications]
# What webhooks and mail servers may be at, though they would be refused as this machine,
# a link-local address or a cloud's metadata service.
{{- if .Notifications.Allow}}
allow = {{list .Notifications.Allow}}
{{- else}}
# allow = ["127.0.0.1/32"]
{{- end}}

[listen]
http  = {{q .Listen.HTTP}}
https = {{q .Listen.HTTPS}}   # empty when Moorage does not terminate TLS
ssh   = {{q .Listen.SSH}}   # git pushes over SSH; empty to disable
{{- if .Listen.DNS}}
dns   = {{q .Listen.DNS}}   # answers for the domain on loopback (moor server resolver)
{{- end}}

[tls]
# How Moorage terminates TLS:
#   "internal"  certificates from Moorage's own CA, generated on first start,
#               or from an intermediate CA issued by your corporate CA
#               (uncomment ca_cert and ca_key below);
#   "files"     certificate and key files you provide (uncomment the
#               [[tls.certificates]] entry below; repeat it for more);
#   "off"       plain HTTP, for example behind a TLS-terminating proxy such
#               as nginx (also set [proxy] below).
mode = {{q .TLS.Mode}}
{{- if .TLS.CACert}}
ca_cert = {{q .TLS.CACert}}
ca_key  = {{q .TLS.CAKey}}
{{- else}}
# ca_cert = "/etc/moorage/tls/intermediate.crt"
# ca_key  = "/etc/moorage/tls/intermediate.key"
{{- end}}
{{- range .TLS.Certificates}}

[[tls.certificates]]
cert = {{q .Cert}}
key  = {{q .Key}}
{{- else}}
#
# [[tls.certificates]]
# cert = "/etc/moorage/tls/wildcard.crt"
# key  = "/etc/moorage/tls/wildcard.key"
{{- end}}

[proxy]
# Proxies whose X-Forwarded-* headers are trusted: addresses or CIDR prefixes.
trusted = {{list .Proxy.Trusted}}
# The scheme browsers use to reach Moorage: "https" or "http".
external_scheme = {{q .Proxy.ExternalScheme}}

[toolchains]
# Builds download Go, Node.js, uv and Python on demand, and verify them.
# Mirrors of the official locations, for proxies and air-gapped networks:
# go_url     = "https://go.dev/dl/"
# node_url   = "https://nodejs.org/dist/"
# uv_url     = "https://github.com/astral-sh/uv/releases"
# python_url = ""   # handed to uv as its Python mirror
# A Node.js release has to be signed by a releaser this moor knows. "warn" goes on if that cannot be checked,
# for a mirror with no signatures, or a new releaser. A signature that is wrong is always refused.
# signatures = "warn"

[containers]
# What runs apps in containers: "auto" for Podman if it is installed, or else
# Docker, or the path of a binary.
engine = {{q .Containers.Engine}}

[log]
level  = {{q .Log.Level}}   # debug, info, warn or error
format = {{q .Log.Format}}   # text or json
`))

// quote returns s as a TOML basic string.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func quoteList(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = quote(v)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// SetListenDNS sets listen.dns in a server.toml, or removes it when addr is
// empty, and leaves the rest of the file as it is.
func SetListenDNS(path, addr string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	line := fmt.Sprintf("dns   = %q   # answers for the domain on loopback (moor server resolver)", addr)
	var out []string
	in, seen, added := false, false, false
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "[") {
			in = trimmed == "[listen]"
			seen = seen || in
		}
		if in {
			if key, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(key) == "dns" {
				if addr != "" {
					out = append(out, line)
				}
				added = true
				continue
			}
			// After the section's last setting.
			if !added && addr != "" && (i+1 == len(lines) || strings.TrimSpace(lines[i+1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[i+1]), "[")) {
				out = append(out, l, line)
				added = true
				continue
			}
		}
		out = append(out, l)
	}
	if !seen {
		return fmt.Errorf("%s has no [listen] section", path)
	}
	return os.WriteFile(path, []byte(strings.Join(out, eol)), fi.Mode().Perm())
}
