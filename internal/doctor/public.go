package doctor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

// The size of the request body that checks a proxy's limit: nginx's default is
// a megabyte, which pushes and uploads go far past.
const checkBodySize = 8 << 20

// checkPublic checks the path from outside to the server, through whatever
// stands in front of it.
func checkPublic(ctx context.Context, o Options) []Check {
	const name = "public url"
	u, err := url.Parse(o.PublicURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return []Check{{Name: name, Status: Fail, Detail: fmt.Sprintf("%q is not an address such as https://%s", o.PublicURL, o.Config.ConsoleHost)}}
	}
	p := publicChecker{o: o, base: u}
	if err := p.setup(); err != nil {
		return []Check{{Name: name, Status: Fail, Detail: err.Error(), Fix: "see --ca-cert"}}
	}
	checks := []Check{p.headers(ctx)}
	if checks[0].Status == Fail && p.unreachable {
		return checks
	}
	checks = append(checks, p.wildcard(ctx), p.upgrade(ctx), p.stream(ctx), p.body(ctx))
	if o.Timeouts {
		checks = append(checks, p.silence(ctx))
	} else {
		checks = append(checks, Check{Name: "proxy (timeouts)", Status: Info, Detail: "not checked: it takes over a minute", Fix: "add --timeouts: a push over HTTPS stays open for its whole deployment"})
	}
	return checks
}

type publicChecker struct {
	o           Options
	base        *url.URL
	tlsConfig   *tls.Config
	client      *http.Client
	unreachable bool
}

func (p *publicChecker) setup() error {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if p.o.CACert != "" {
		data, err := os.ReadFile(p.o.CACert)
		if err != nil {
			return err
		}
		if !roots.AppendCertsFromPEM(data) {
			return fmt.Errorf("%s holds no certificates", p.o.CACert)
		}
	}
	p.tlsConfig = &tls.Config{RootCAs: roots}
	p.client = &http.Client{
		Transport:     &http.Transport{TLSClientConfig: p.tlsConfig, ForceAttemptHTTP2: true, DialContext: p.o.Dial},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return nil
}

func (p *publicChecker) url(path string) string {
	return strings.TrimRight(p.base.String(), "/") + path
}

func (p *publicChecker) get(ctx context.Context, path string, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.url(path), nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	res, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return res, cancel, nil
}

// headers checks that the request reaches this server, with its headers.
func (p *publicChecker) headers(ctx context.Context) Check {
	c := Check{Name: "proxy (headers)"}
	res, cancel, err := p.get(ctx, "/.moorage/proxy-check", 15*time.Second)
	if err != nil {
		p.unreachable = true
		c.Status, c.Detail = Fail, fmt.Sprintf("cannot reach %s: %v", p.o.PublicURL, err)
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &unknown) {
			c.Fix = "moor trust --file, with the proxy's issuer; or pass it here with --ca-cert"
		} else {
			c.Fix = "see that its DNS, its firewall and the proxy are in place"
		}
		return c
	}
	defer cancel()
	defer res.Body.Close()
	var got apitypes.ProxyCheck
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&got) != nil {
		p.unreachable = true
		c.Status, c.Detail = Fail, fmt.Sprintf("%s answered %s, not as Moorage does", p.o.PublicURL, res.Status)
		c.Fix = "the proxy does not send this address to Moorage: see moor server proxy-config nginx"
		return c
	}
	switch {
	case !got.Trusted:
		c.Status, c.Detail = Fail, fmt.Sprintf("the connection comes from %s, which [proxy] trusted does not list: Moorage ignores its X-Forwarded headers", got.Peer)
		c.Fix = "add " + got.Peer + " under [proxy] trusted in " + p.o.Config.Path + ", if that is your proxy"
	case hostOnly(got.Host) != p.base.Hostname():
		c.Status, c.Detail = Fail, fmt.Sprintf("Moorage sees the hostname %q, not %q: it routes by hostname", got.Host, p.base.Host)
		c.Fix = "the proxy must keep the Host header: proxy_set_header Host $host in nginx"
	case got.ForwardedFor == "":
		c.Status, c.Detail = Fail, "X-Forwarded-For did not arrive: apps and logs would see the proxy as every client"
		c.Fix = "set it in the proxy: see moor server proxy-config nginx"
	case p.base.Scheme == "https" && got.ForwardedProto != "https":
		c.Status, c.Detail = Fail, fmt.Sprintf("X-Forwarded-Proto is %q, not https: apps would think their visitors use plain HTTP", got.ForwardedProto)
		c.Fix = "set it in the proxy: see moor server proxy-config nginx"
	default:
		c.Status, c.Detail = OK, fmt.Sprintf("from %s, a trusted proxy, with the hostname and X-Forwarded headers", got.Peer)
	}
	return c
}

func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// wildcard checks that an app's hostname reaches the server the same way.
func (p *publicChecker) wildcard(ctx context.Context) Check {
	c := Check{Name: "proxy (apps)"}
	app := randomLabel() + "." + p.o.Config.Domain
	if p.o.Config.Routing == "path" {
		app = p.o.Config.Domain
	}
	u := *p.base
	u.Host = app
	if port := p.base.Port(); port != "" {
		u.Host = net.JoinHostPort(app, port)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(u.String(), "/")+"/.moorage/proxy-check", nil)
	if err != nil {
		c.Status, c.Detail = Fail, err.Error()
		return c
	}
	res, err := p.client.Do(req)
	if err != nil {
		c.Status, c.Detail = Fail, fmt.Sprintf("cannot reach %s: %v", app, err)
		c.Fix = "the wildcard DNS record, the proxy's server_name and its certificate must all cover *." + p.o.Config.Domain
		return c
	}
	defer res.Body.Close()
	var got apitypes.ProxyCheck
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&got) != nil || hostOnly(got.Host) != app {
		c.Status, c.Detail = Fail, fmt.Sprintf("%s answered %s, not as Moorage does", app, res.Status)
		c.Fix = "the proxy must send *." + p.o.Config.Domain + " to Moorage, keeping the Host header"
		return c
	}
	c.Status, c.Detail = OK, "*."+p.o.Config.Domain+" reaches the server too"
	return c
}

// upgrade checks that a WebSocket gets through, in both directions.
func (p *publicChecker) upgrade(ctx context.Context) Check {
	c := Check{Name: "proxy (websockets)"}
	fail := func(detail string) Check {
		c.Status, c.Detail = Fail, detail
		c.Fix = "pass upgrades on: proxy_http_version 1.1 and the Upgrade and Connection headers in nginx"
		return c
	}
	host := p.base.Host
	if p.base.Port() == "" {
		port := "80"
		if p.base.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(p.base.Hostname(), port)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dial := p.o.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(dialCtx, "tcp", host)
	if err != nil {
		return fail(err.Error())
	}
	if p.base.Scheme == "https" {
		cfg := p.tlsConfig.Clone()
		cfg.ServerName = p.base.Hostname()
		cfg.NextProtos = []string{"http/1.1"}
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(dialCtx); err != nil {
			conn.Close()
			return fail(err.Error())
		}
		conn = tlsConn
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	var nonce [16]byte
	rand.Read(nonce[:])
	key := base64.StdEncoding.EncodeToString(nonce[:])
	fmt.Fprintf(conn, "GET /.moorage/proxy-check/upgrade HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", p.base.Host, key)
	br := bufio.NewReader(conn)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		return fail("no answer to an upgrade: " + err.Error())
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		return fail("an upgrade got " + res.Status)
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if res.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return fail("the upgrade's answer does not match its request")
	}
	first, err := readServerFrame(br)
	if err != nil || first != "moorage" {
		return fail(fmt.Sprintf("the server's message did not get through: %q %v", first, err))
	}
	if err := writeClientFrame(conn, "ping"); err != nil {
		return fail(err.Error())
	}
	if back, err := readServerFrame(br); err != nil || back != "ping" {
		return fail(fmt.Sprintf("a message to the server did not get through: %q %v", back, err))
	}
	c.Status, c.Detail = OK, "pass through in both directions"
	return c
}

func readServerFrame(r *bufio.Reader) (string, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return "", err
	}
	if head[0] != 0x81 || head[1] > 125 {
		return "", errors.New("not a short text frame")
	}
	data := make([]byte, head[1])
	_, err := io.ReadFull(r, data)
	return string(data), err
}

func writeClientFrame(w io.Writer, text string) error {
	var mask [4]byte
	rand.Read(mask[:])
	frame := append([]byte{0x81, 0x80 | byte(len(text))}, mask[:]...)
	for i := range len(text) {
		frame = append(frame, text[i]^mask[i%4])
	}
	_, err := w.Write(frame)
	return err
}

// stream checks that a response arrives as it is written.
func (p *publicChecker) stream(ctx context.Context) Check {
	c := Check{Name: "proxy (streaming)"}
	res, cancel, err := p.get(ctx, "/.moorage/proxy-check/stream", 15*time.Second)
	if err != nil {
		c.Status, c.Detail = Fail, err.Error()
		return c
	}
	defer cancel()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		c.Status, c.Detail = Fail, "the stream got "+res.Status
		return c
	}
	br := bufio.NewReader(res.Body)
	var first, last time.Time
	lines := 0
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			lines++
			last = time.Now()
			if first.IsZero() {
				first = last
			}
		}
		if err != nil {
			break
		}
	}
	if lines != 3 {
		c.Status, c.Detail = Fail, fmt.Sprintf("the stream ended after %d lines of 3", lines)
		return c
	}
	if last.Sub(first) < 500*time.Millisecond {
		c.Status, c.Detail = Fail, "responses are buffered: lines written over most of a second arrived all at once, so logs and deploys would not show as they happen"
		c.Fix = "proxy_buffering off in nginx"
		return c
	}
	c.Status, c.Detail = OK, "responses arrive as they are written"
	return c
}

// body checks that a large request gets through.
func (p *publicChecker) body(ctx context.Context) Check {
	c := Check{Name: "proxy (uploads)"}
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.url("/.moorage/proxy-check/body"), bytes.NewReader(make([]byte, checkBodySize)))
	if err != nil {
		c.Status, c.Detail = Fail, err.Error()
		return c
	}
	res, err := p.client.Do(req)
	if err != nil {
		c.Status, c.Detail = Fail, err.Error()
		return c
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		c.Status, c.Detail = Fail, fmt.Sprintf("a request of %d MiB got %s: uploads and pushes are larger", checkBodySize>>20, res.Status)
		c.Fix = "client_max_body_size 0 in nginx: Moorage limits them itself"
		return c
	}
	c.Status, c.Detail = OK, fmt.Sprintf("a request of %d MiB gets through", checkBodySize>>20)
	return c
}

// silence checks that a request may stay silent for over a minute.
func (p *publicChecker) silence(ctx context.Context) Check {
	c := Check{Name: "proxy (timeouts)"}
	seconds := int(p.o.Wait / time.Second)
	res, cancel, err := p.get(ctx, fmt.Sprintf("/.moorage/proxy-check/wait?seconds=%d", seconds), p.o.Wait+30*time.Second)
	fix := "raise the proxy's limit: proxy_read_timeout 1h in nginx. A push over HTTPS stays open for its whole deployment"
	if err != nil {
		c.Status, c.Detail, c.Fix = Fail, fmt.Sprintf("a request silent for %d seconds failed: %v", seconds, err), fix
		return c
	}
	defer cancel()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		c.Status, c.Detail, c.Fix = Fail, fmt.Sprintf("a request silent for %d seconds got %s", seconds, res.Status), fix
		return c
	}
	c.Status, c.Detail = OK, fmt.Sprintf("a request may stay silent for %d seconds", seconds)
	return c
}
