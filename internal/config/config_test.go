package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func noEnv(string) string { return "" }

// normalize makes nil and empty slices compare equal.
func normalize(c Config) Config {
	if c.Proxy.Trusted == nil {
		c.Proxy.Trusted = []string{}
	}
	if c.TLS.Certificates == nil {
		c.TLS.Certificates = []Certificate{}
	}
	c.Path = ""
	return c
}

func roundTrip(t *testing.T, c Config, preset string) Config {
	t.Helper()
	data, err := Render(c, preset)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(data, "server.toml")
	if err != nil {
		t.Fatalf("rendered configuration does not parse: %v\n%s", err, data)
	}
	back.ApplyDefaults()
	if err := back.Validate(); err != nil {
		t.Fatalf("rendered configuration is invalid: %v\n%s", err, data)
	}
	return back
}

func TestPresetLocal(t *testing.T) {
	dir := t.TempDir()
	c, err := Preset("local", PresetOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if c.Domain != "localhost" || c.ConsoleHost != "moorage.localhost" {
		t.Errorf("domain %q, console_host %q", c.Domain, c.ConsoleHost)
	}
	if c.Listen.HTTP != "127.0.0.1:8080" || c.Listen.HTTPS != "" || c.TLS.Mode != "off" || c.Proxy.ExternalScheme != "http" {
		t.Errorf("unexpected local preset: %+v", c)
	}
	if got := roundTrip(t, c, "local"); !reflect.DeepEqual(normalize(got), normalize(c)) {
		t.Errorf("round trip changed the configuration:\n got %+v\nwant %+v", got, c)
	}
}

func TestPresetLan(t *testing.T) {
	dir := t.TempDir()
	if _, err := Preset("lan", PresetOptions{DataDir: dir}); err == nil || !strings.Contains(err.Error(), "--domain") {
		t.Errorf("lan without a domain: err = %v, want a hint about --domain", err)
	}
	c, err := Preset("lan", PresetOptions{DataDir: dir, Domain: "Apps.Example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Domain != "apps.example.com" || c.ConsoleHost != "moorage.apps.example.com" {
		t.Errorf("domain %q, console_host %q", c.Domain, c.ConsoleHost)
	}
	if c.Listen.HTTPS != ":443" || c.TLS.Mode != "internal" || c.Proxy.ExternalScheme != "https" {
		t.Errorf("unexpected lan preset: %+v", c)
	}
	if got := roundTrip(t, c, "lan"); !reflect.DeepEqual(normalize(got), normalize(c)) {
		t.Errorf("round trip changed the configuration:\n got %+v\nwant %+v", got, c)
	}

	p, err := Preset("lan", PresetOptions{DataDir: dir, Domain: "apps.example.com", BehindProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Listen.HTTP != "127.0.0.1:8080" || p.Listen.HTTPS != "" || p.TLS.Mode != "off" {
		t.Errorf("behind-proxy listeners: %+v, tls %q", p.Listen, p.TLS.Mode)
	}
	if p.Proxy.ExternalScheme != "https" || len(p.Proxy.Trusted) != 2 {
		t.Errorf("behind-proxy proxy settings: %+v", p.Proxy)
	}
	if got := roundTrip(t, p, "lan"); !reflect.DeepEqual(normalize(got), normalize(p)) {
		t.Errorf("round trip changed the configuration:\n got %+v\nwant %+v", got, p)
	}
}

func TestPresetErrors(t *testing.T) {
	if _, err := Preset("cloud", PresetOptions{DataDir: t.TempDir()}); err == nil {
		t.Error("unknown preset accepted")
	}
	if _, err := Preset("local", PresetOptions{DataDir: t.TempDir(), BehindProxy: true}); err == nil {
		t.Error("local preset accepted --behind-proxy")
	}
	if _, err := Preset("local", PresetOptions{DataDir: "relative/dir"}); err == nil {
		t.Error("relative data directory accepted")
	}
}

func TestUnknownKeyNamesLine(t *testing.T) {
	data := []byte("data_dir = \"/srv/moorage\"\ndomain = \"apps.example.com\"\n\n[listen]\nhttp = \":80\"\nhtps = \":443\"\n")
	_, err := Parse(data, "server.toml")
	if err == nil {
		t.Fatal("unknown key accepted")
	}
	if !strings.Contains(err.Error(), "server.toml:6:") || !strings.Contains(err.Error(), `"listen.htps"`) {
		t.Errorf("error does not name the line and key: %v", err)
	}
}

func TestSyntaxErrorNamesLine(t *testing.T) {
	_, err := Parse([]byte("domain = \"apps.example.com\"\nrouting = host\n"), "server.toml")
	if err == nil || !strings.Contains(err.Error(), "server.toml:2:") {
		t.Errorf("syntax error does not name the line: %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	c := Config{
		DataDir:     "relative",
		Domain:      "-bad-.example.com",
		ConsoleHost: "moorage.example.com",
		Routing:     "hostname",
		Listen:      Listen{HTTP: "80", HTTPS: ":443"},
		TLS:         TLS{Mode: "acme", CACert: "/ca.crt"},
		Proxy:       Proxy{Trusted: []string{"10.0.0.0/8", "office"}, ExternalScheme: "ftp"},
		Log:         Log{Level: "verbose", Format: "text"},
	}
	err := c.Validate()
	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("Validate() = %v, want a ValidationError", err)
	}
	for _, want := range []string{
		`data_dir "relative"`,
		`domain "-bad-.example.com"`,
		`routing "hostname"`,
		`listen.http: "80"`,
		`tls.mode "acme" is not available yet`,
		"tls.ca_cert and tls.ca_key must be given together",
		`proxy.trusted: "office"`,
		`proxy.external_scheme "ftp"`,
		`log.level "verbose"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("problems do not mention %q:\n%v", want, err)
		}
	}
}

func TestValidateTLSCombinations(t *testing.T) {
	base := func() Config {
		c := Config{DataDir: t.TempDir(), Domain: "apps.example.com", Listen: Listen{HTTP: ":80"}}
		c.ApplyDefaults()
		return c
	}
	c := base()
	c.Listen.HTTPS = ":443"
	c.TLS.Mode = "off"
	if err := c.Validate(); err == nil {
		t.Error("listen.https with tls.mode off accepted")
	}
	c = base()
	c.TLS.Mode = "files"
	c.Listen.HTTPS = ":443"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "[[tls.certificates]]") {
		t.Errorf("files mode without certificates: %v", err)
	}
	c.TLS.Certificates = []Certificate{{Cert: "/tls/wildcard.crt", Key: "/tls/wildcard.key"}}
	if err := c.Validate(); err != nil {
		t.Errorf("files mode with a certificate: %v", err)
	}
	if got := roundTrip(t, c, "lan"); !reflect.DeepEqual(normalize(got), normalize(c)) {
		t.Errorf("round trip changed a files configuration:\n got %+v\nwant %+v", got, c)
	}
}

func TestDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.toml")
	dataDir := filepath.Join(t.TempDir(), "data")
	content := "data_dir = " + quote(dataDir) + "\ndomain = \"apps.example.com\"\n[listen]\nhttp = \":80\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Path != path || c.ConsoleHost != "moorage.apps.example.com" || c.Routing != "host" ||
		c.TLS.Mode != "off" || c.Proxy.ExternalScheme != "http" || c.Log.Level != "info" || c.Log.Format != "text" {
		t.Errorf("defaults not applied: %+v", c)
	}

	withHTTPS := Config{DataDir: dataDir, Domain: "apps.example.com", Listen: Listen{HTTP: ":80", HTTPS: ":443"}}
	withHTTPS.ApplyDefaults()
	if withHTTPS.TLS.Mode != "internal" || withHTTPS.Proxy.ExternalScheme != "https" {
		t.Errorf("defaults with listen.https: tls %q, scheme %q", withHTTPS.TLS.Mode, withHTTPS.Proxy.ExternalScheme)
	}
	behindProxy := Config{DataDir: dataDir, Domain: "apps.example.com", Listen: Listen{HTTP: ":8080"}, Proxy: Proxy{Trusted: []string{"10.1.2.3"}}}
	behindProxy.ApplyDefaults()
	if behindProxy.TLS.Mode != "off" || behindProxy.Proxy.ExternalScheme != "https" {
		t.Errorf("defaults behind a proxy: tls %q, scheme %q", behindProxy.TLS.Mode, behindProxy.Proxy.ExternalScheme)
	}
}

func TestApplyEnv(t *testing.T) {
	dataDir := t.TempDir()
	env := map[string]string{"MOORAGE_DATA_DIR": dataDir, "MOORAGE_LOG_LEVEL": "debug"}
	c := Config{DataDir: "/somewhere/else", Log: Log{Level: "info"}}
	c.ApplyEnv(func(k string) string { return env[k] })
	if c.DataDir != dataDir || c.Log.Level != "debug" {
		t.Errorf("environment not applied: data_dir %q, log.level %q", c.DataDir, c.Log.Level)
	}
}

func TestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "server.toml")
	c, err := Preset("local", PresetOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(path, c, "local", false); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, c, "local", false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("second Write without force: %v", err)
	}
	if err := Write(path, c, "local", true); err != nil {
		t.Errorf("Write with force: %v", err)
	}
	loaded, err := Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalize(loaded), normalize(c)) {
		t.Errorf("loaded configuration differs:\n got %+v\nwant %+v", loaded, c)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml"), noEnv); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load of a missing file: %v", err)
	}
}

func TestQuote(t *testing.T) {
	for _, s := range []string{
		`C:\ProgramData\Moorage`,
		`/srv/with "quotes"`,
		"tab\there",
		"bell\a",
		"ünïcødé",
	} {
		var out struct {
			V string `toml:"v"`
		}
		if err := toml.Unmarshal([]byte("v = "+quote(s)), &out); err != nil {
			t.Errorf("quote(%q) = %s does not parse: %v", s, quote(s), err)
			continue
		}
		if out.V != s {
			t.Errorf("quote(%q) round trip gave %q", s, out.V)
		}
	}
}

func TestParseTrusted(t *testing.T) {
	cases := map[string]string{
		"10.0.0.0/8": "10.0.0.0/8",
		"10.1.2.3/8": "10.0.0.0/8",
		"127.0.0.1":  "127.0.0.1/32",
		"::1":        "::1/128",
		"fd00::/8":   "fd00::/8",
	}
	for in, want := range cases {
		p, err := ParseTrusted(in)
		if err != nil {
			t.Errorf("ParseTrusted(%q): %v", in, err)
			continue
		}
		if p.String() != want {
			t.Errorf("ParseTrusted(%q) = %s, want %s", in, p, want)
		}
	}
	for _, bad := range []string{"", "office", "10.0.0.0/33", "1.2.3"} {
		if _, err := ParseTrusted(bad); err == nil {
			t.Errorf("ParseTrusted(%q) accepted", bad)
		}
	}
}
