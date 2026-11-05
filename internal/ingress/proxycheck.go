package ingress

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

// The proxy check answers what moor server doctor --public-url asks of whatever
// stands in front of the server: what arrives from it, and whether it passes on
// upgrades, streams, long silences and large requests. It tells a caller nothing
// but what it sent itself, and holds few resources for few callers.
const (
	proxyCheckPrefix = "/.moorage/proxy-check"
	// maxProxyChecks bounds the requests of the kinds that take time or memory.
	maxProxyChecks = 8
	maxCheckWait   = 2 * time.Minute
	maxCheckBody   = 32 << 20
)

func (rt *Router) proxyCheck(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, proxyCheckPrefix) {
	case "":
		rt.checkHeaders(w, r)
		return
	case "/stream":
		checkStream(w, r)
		return
	case "/wait", "/body", "/upgrade":
	default:
		notFound.ServeHTTP(w, r)
		return
	}
	if rt.proxyChecks.Add(1) > maxProxyChecks {
		rt.proxyChecks.Add(-1)
		http.Error(w, "too many checks at once", http.StatusServiceUnavailable)
		return
	}
	defer rt.proxyChecks.Add(-1)
	switch strings.TrimPrefix(r.URL.Path, proxyCheckPrefix) {
	case "/wait":
		checkWait(w, r)
	case "/body":
		checkBody(w, r)
	case "/upgrade":
		checkUpgrade(w, r)
	}
}

// checkHeaders says what arrived.
func (rt *Router) checkHeaders(w http.ResponseWriter, r *http.Request) {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(apitypes.ProxyCheck{
		Peer: peer, Trusted: isTrusted(peer, rt.trusted), Host: r.Host,
		ForwardedFor:   r.Header.Get("X-Forwarded-For"),
		ForwardedProto: r.Header.Get("X-Forwarded-Proto"),
		ForwardedHost:  r.Header.Get("X-Forwarded-Host"),
		TLS:            r.TLS != nil,
	})
}

// checkStream writes three lines over most of a second: one that buffers
// responses delivers them all at once.
func checkStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	for i := 1; i <= 3; i++ {
		if i > 1 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(400 * time.Millisecond):
			}
		}
		io.WriteString(w, strconv.Itoa(i)+"\n")
		flusher.Flush()
	}
}

// checkWait says nothing for as long as it is asked to, as a request does that
// is waiting on a deployment.
func checkWait(w http.ResponseWriter, r *http.Request) {
	seconds, err := strconv.Atoi(r.URL.Query().Get("seconds"))
	wait := time.Duration(seconds) * time.Second
	if err != nil || wait <= 0 || wait > maxCheckWait {
		http.Error(w, "seconds must be between 1 and 120", http.StatusBadRequest)
		return
	}
	select {
	case <-r.Context().Done():
	case <-time.After(wait):
		io.WriteString(w, "ok\n")
	}
}

// checkBody reads a request's body, as an upload.
func checkBody(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST a body", http.StatusMethodNotAllowed)
		return
	}
	n, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxCheckBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	io.WriteString(w, strconv.FormatInt(n, 10)+"\n")
}

// webSocketGUID is what the WebSocket handshake hashes a client's key with.
const webSocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// checkUpgrade is a small WebSocket server: it says hello, and repeats what it
// is told once. Real frames, for a proxy that looks inside them.
func checkUpgrade(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHas(r.Header, "Connection", "upgrade") {
		http.Error(w, "the Upgrade and Connection headers did not arrive", http.StatusUpgradeRequired)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "the Sec-WebSocket-Key header did not arrive", http.StatusBadRequest)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "this connection cannot be upgraded", http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	sum := sha1.Sum([]byte(key + webSocketGUID))
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	writeFrame(rw.Writer, "moorage")
	if rw.Flush() != nil {
		return
	}
	text, err := readFrame(rw.Reader)
	if err != nil {
		return
	}
	writeFrame(rw.Writer, text)
	rw.Flush()
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// writeFrame writes an unmasked text frame of less than 126 bytes.
func writeFrame(w *bufio.Writer, text string) {
	w.WriteByte(0x81)
	w.WriteByte(byte(len(text)))
	w.WriteString(text)
}

// readFrame reads a masked text frame of less than 126 bytes.
func readFrame(r *bufio.Reader) (string, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return "", err
	}
	if head[0] != 0x81 || head[1]&0x80 == 0 || head[1]&0x7f > 125 {
		return "", errors.New("not a short masked text frame")
	}
	var mask [4]byte
	if err := binary.Read(r, binary.BigEndian, &mask); err != nil {
		return "", err
	}
	data := make([]byte, head[1]&0x7f)
	if _, err := io.ReadFull(r, data); err != nil {
		return "", err
	}
	for i := range data {
		data[i] ^= mask[i%4]
	}
	return string(data), nil
}
