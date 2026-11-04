package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/smtp"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Kinds of channel.
const (
	KindWebhook = "webhook"
	KindSlack   = "slack"
	KindTeams   = "teams"
	KindDiscord = "discord"
	KindEmail   = "email"
)

// Kinds lists them.
var Kinds = []string{KindWebhook, KindSlack, KindTeams, KindDiscord, KindEmail}

// Config is what a channel needs, which is kept sealed.
type Config struct {
	// URL is where a webhook goes, and Secret what signs it.
	URL    string `json:"url,omitempty"`
	Secret string `json:"secret,omitempty"`
	// For email: the server to send through, how to sign in to it, and who is told.
	Host     string   `json:"host,omitempty"`
	Port     int      `json:"port,omitempty"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	From     string   `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
}

// InvalidError says what is wrong with a channel or a rule.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Check says whether a channel's configuration can work, and for a webhook makes its secret.
func (c *Config) Check(kind string) error {
	switch kind {
	case KindWebhook, KindSlack, KindTeams, KindDiscord:
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return &InvalidError{"A webhook needs an address such as https://example.com/hook."}
		}
		if kind == KindWebhook && c.Secret == "" {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			c.Secret = hex.EncodeToString(raw)
		}
	case KindEmail:
		if c.Host == "" || c.From == "" || len(c.To) == 0 {
			return &InvalidError{"Email needs a mail server, who it is from and who it goes to."}
		}
		if c.Port == 0 {
			c.Port = 587
		}
		for _, addr := range append([]string{c.From}, c.To...) {
			if !strings.Contains(addr, "@") || strings.ContainsAny(addr, " \r\n<>,") {
				return &InvalidError{fmt.Sprintf("%q is not an email address.", addr)}
			}
		}
	default:
		return &InvalidError{fmt.Sprintf("%q is not a kind of channel: webhook, slack, teams, discord or email.", kind)}
	}
	return nil
}

// Message is what goes to a channel.
type Message struct {
	Kind, App, Env, Summary, Link string
	At                            time.Time
	// Suppressed is how many like it were held back since the last that went.
	Suppressed int
	// Data is the event's own, for a webhook.
	Data map[string]any
}

func (m Message) title() string {
	where := "Moorage"
	switch {
	case m.App != "" && m.Env != "" && m.Env != "production":
		where = m.App + " · " + m.Env
	case m.App != "":
		where = m.App
	}
	return where + ": " + m.Summary
}

func (m Message) text() string {
	text := m.title()
	if m.Suppressed > 0 {
		text += fmt.Sprintf(" (and %d more like it)", m.Suppressed)
	}
	if m.Link != "" {
		text += "\n" + m.Link
	}
	return text
}

// metadataAddresses are where clouds keep what a machine may know about itself, where they are not
// link-local addresses.
var metadataAddresses = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),
	netip.MustParseAddr("100.100.100.200"),
	netip.MustParseAddr("168.63.129.16"),
}

// guardedDialer refuses what a channel's address should not reach: this machine by any of its
// addresses, its link-local neighbours and the cloud's metadata service. It looks when connecting, so that a name that
// resolves to one thing and then another makes no difference. Addresses on the local network are
// allowed: a team's own services are what many webhooks are for.
func guardedDialer(allow []netip.Prefix) *net.Dialer {
	return &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			addr = addr.Unmap()
			for _, p := range allow {
				if p.Contains(addr) {
					return nil
				}
			}
			if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsInterfaceLocalMulticast() || slices.Contains(metadataAddresses, addr) || isOwnAddress(addr) {
				return fmt.Errorf("notifications may not reach %s, unless [notifications] allow in server.toml says so", addr)
			}
			return nil
		},
	}
}

// isOwnAddress says whether an address is this machine's, on any of its interfaces.
func isOwnAddress(addr netip.Addr) bool {
	own, err := net.InterfaceAddrs()
	if err != nil {
		return true // better no notification than one to this machine
	}
	for _, a := range own {
		if network, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(network.IP); ok && ip.Unmap() == addr {
				return true
			}
		}
	}
	return false
}

func (s *Service) httpClient() *http.Client {
	dialer := guardedDialer(s.Allow)
	return &http.Client{
		Timeout: 15 * time.Second,
		// A redirect could go anywhere, and an answer has nothing more to say to us.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, DisableKeepAlives: true},
	}
}

// Sign makes a webhook's signature: of when it was sent and what was sent, so that a copy cannot be
// sent again later. A receiver works out the same, and compares.
func Sign(secret string, at time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(at.Unix(), 10) + "."))
	mac.Write(body)
	return "t=" + strconv.FormatInt(at.Unix(), 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (s *Service) deliver(ctx context.Context, kind string, cfg Config, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if kind == KindEmail {
		return s.sendEmail(ctx, cfg, m)
	}
	var payload any
	switch kind {
	case KindSlack:
		// What an app's build said can be in a message, and is not to call out to a channel.
		payload = map[string]string{"text": slackEscape.Replace(m.text())}
	case KindTeams:
		payload = map[string]string{"text": m.text()}
	case KindDiscord:
		text := m.text()
		if len(text) > 1900 {
			text = strings.ToValidUTF8(text[:1900], "")
		}
		payload = map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}}
	default:
		payload = map[string]any{"type": m.Kind, "app": m.App, "env": m.Env, "summary": m.Summary, "link": m.Link, "at": m.At, "suppressed": m.Suppressed, "data": m.Data}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Moorage")
	if kind == KindWebhook {
		req.Header.Set("X-Moorage-Event", m.Kind)
		req.Header.Set("X-Moorage-Signature", Sign(cfg.Secret, time.Now(), body))
	}
	res, err := s.httpClient().Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return urlErr.Err // the address has what may be a secret in it, as chat webhooks' do
		}
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("it answered %s", res.Status)
	}
	return nil
}

func (s *Service) sendEmail(ctx context.Context, cfg Config, m Message) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := guardedDialer(s.Allow).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if cfg.Port == 465 { // this port has TLS from the start
		conn = tls.Client(conn, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return err
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	for _, to := range cfg.To {
		if err := client.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	clean := strings.NewReplacer("\r", " ", "\n", " ")
	var mail bytes.Buffer
	fmt.Fprintf(&mail, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n", clean.Replace(cfg.From), clean.Replace(strings.Join(cfg.To, ", ")), mime.QEncoding.Encode("utf-8", clean.Replace(m.title())), time.Now().Format(time.RFC1123Z))
	// What this writes to takes care of a line that starts with a dot.
	mail.WriteString(strings.ReplaceAll(m.text(), "\n", "\r\n") + "\r\n")
	if _, err := w.Write(mail.Bytes()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}
