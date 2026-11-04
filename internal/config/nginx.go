package config

import (
	"bytes"
	"fmt"
	"net"
	"text/template"
)

// NginxOptions adjust the configuration Nginx writes.
type NginxOptions struct {
	// Cert and Key are the proxy's certificate for the console's and the apps'
	// hostnames: a wildcard for *.<domain>, usually.
	Cert, Key string
	// HTTPPort and HTTPSPort are where nginx listens: 80 and 443 by default.
	HTTPPort, HTTPSPort int
}

var nginxTemplate = template.Must(template.New("nginx").Parse(`# Moorage behind nginx, for {{.ConsoleHost}} and *.{{.Domain}}.
# Written by moor server proxy-config nginx. Put it in /etc/nginx/conf.d/moorage.conf
# and run: nginx -t && nginx -s reload
# Then check the whole path: moor server doctor --public-url {{.PublicURL}}
#
# Git over SSH does not pass through here: it goes straight to Moorage{{if .SSH}} ({{.SSH}}){{end}}.

# A WebSocket upgrades its connection; anything else keeps it open for the next
# request.
map $http_upgrade $moorage_connection {
    default upgrade;
    ''      '';
}

upstream moorage {
    server {{.Upstream}};
    keepalive 64;
}

server {
    listen {{.HTTPPort}};
    listen [::]:{{.HTTPPort}};
    server_name {{.ConsoleHost}} *.{{.Domain}};
    return 308 https://$host{{.RedirectPort}}$request_uri;
}

server {
    listen {{.HTTPSPort}} ssl;
    listen [::]:{{.HTTPSPort}} ssl;
    server_name {{.ConsoleHost}} *.{{.Domain}};

    # A certificate for {{.ConsoleHost}} and *.{{.Domain}}.
    ssl_certificate     {{.Cert}};
    ssl_certificate_key {{.Key}};

    location / {
        proxy_pass http://moorage;
        proxy_http_version 1.1;

        # Moorage routes by the hostname, and believes these headers because
        # server.toml lists this proxy under [proxy] trusted.
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host  $http_host;
        proxy_set_header X-Real-IP         $remote_addr;

        # WebSockets.
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection $moorage_connection;

        # Logs and deploys stream as they happen, in both directions.
        proxy_buffering         off;
        proxy_request_buffering off;

        # A push over HTTPS stays open for its whole deployment, and a log that is
        # followed can be silent for long. Moorage limits uploads itself.
        proxy_read_timeout   1h;
        proxy_send_timeout   1h;
        client_max_body_size 0;
    }
}
`))

// Nginx writes a configuration for nginx in front of the server.
func Nginx(c Config, o NginxOptions) string {
	if o.HTTPPort == 0 {
		o.HTTPPort = 80
	}
	if o.HTTPSPort == 0 {
		o.HTTPSPort = 443
	}
	if o.Cert == "" {
		o.Cert = "/etc/nginx/tls/" + c.Domain + ".crt"
	}
	if o.Key == "" {
		o.Key = "/etc/nginx/tls/" + c.Domain + ".key"
	}
	host, port, _ := net.SplitHostPort(c.Listen.HTTP)
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	public, redirect := "https://"+c.ConsoleHost, ""
	if o.HTTPSPort != 443 {
		redirect = fmt.Sprintf(":%d", o.HTTPSPort)
		public += redirect
	}
	var out bytes.Buffer
	err := nginxTemplate.Execute(&out, map[string]any{
		"ConsoleHost": c.ConsoleHost, "Domain": c.Domain, "PublicURL": public, "SSH": c.Listen.SSH,
		"Upstream": net.JoinHostPort(host, port), "HTTPPort": o.HTTPPort, "HTTPSPort": o.HTTPSPort, "RedirectPort": redirect,
		"Cert": o.Cert, "Key": o.Key,
	})
	if err != nil {
		panic(err) // the template and its data are ours
	}
	return out.String()
}

// ProxyProblems says what stops this configuration from working behind a TLS
// proxy.
func (c Config) ProxyProblems() []string {
	var p []string
	if c.TLS.Mode != "off" {
		p = append(p, fmt.Sprintf(`tls.mode is %q: Moorage terminates TLS itself. Behind a proxy, set it to "off", and listen.https to ""`, c.TLS.Mode))
	}
	if len(c.Proxy.Trusted) == 0 {
		p = append(p, "[proxy] trusted lists no proxy: Moorage will not believe its headers. List its address")
	}
	if c.Proxy.ExternalScheme != "https" {
		p = append(p, `proxy.external_scheme is not "https": apps' addresses will say http`)
	}
	return p
}
