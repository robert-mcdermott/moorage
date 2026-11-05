// SPDX-License-Identifier: Apache-2.0

package ingress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mholt/acmez/v3/acme"
)

func stickyPool(t *testing.T, backend http.Handler) (*Pool, string) {
	t.Helper()
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	p := NewPool("a", "production", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.Set([]*Backend{NewBackend("i1", addr)})
	p.SetSticky(true)
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return p, front.URL
}

// A backend that sends an early hint, as Rails and others do, before its answer.
func TestStickyCookieAfterEarlyHints(t *testing.T) {
	_, url := stickyPool(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</a.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		fmt.Fprint(w, "page")
	}))
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(res.Cookies()) == 0 {
		t.Error("no sticky cookie on an answer that came after a 103")
	}
}

// What an app hears of its own cookies, whatever they look like.
func TestStickyLeavesAnAppsCookiesAlone(t *testing.T) {
	for _, sticky := range []bool{false, true} {
		p, url := stickyPool(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Header.Get("Cookie")) }))
		p.SetSticky(sticky)
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		sent := `plain=1; json={"a":1}; foo[bar]=2; lang=日本語; path=a\b; spaced=a b`
		req.Header.Set("Cookie", sent)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(body) != sent {
			t.Errorf("sticky=%v: the app heard %q", sticky, body)
		}
	}
}

// A sticky cookie goes on the answer, and not on the 100 before it, which a browser would make nothing of.
func TestStickyCookieAfterAContinue(t *testing.T) {
	_, url := stickyPool(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // reading sends the 100
		fmt.Fprint(w, "ok")
	}))
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\nExpect: 100-continue\r\nConnection: close\r\n\r\nhi")
	out, _ := io.ReadAll(conn)
	before, after, ok := strings.Cut(string(out), "HTTP/1.1 200")
	if !ok || strings.Contains(before, "Set-Cookie") || !strings.Contains(after, "Set-Cookie") {
		t.Errorf("what came back:\n%s", out)
	}
}

// A hook that leaves something behind in the background, which keeps its output open.
func TestDNSHookLeavingAChildBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a shell script")
	}
	script := filepath.Join(t.TempDir(), "hook.sh")
	os.WriteFile(script, []byte("#!/bin/sh\n(sleep 6) &\nexit 0\n"), 0o755)
	s := &commandSolver{command: script}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err := s.run(ctx, "add", acme.Challenge{Identifier: acme.Identifier{Type: "dns", Value: "example.test"}, Token: "t", KeyAuthorization: "k"})
	if err != nil {
		t.Errorf("the hook did what it was to: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("the limit did not hold")
	}
}

// A first visit waits for its certificate, whatever the server's limit on a handshake.
func TestAFirstVisitWaitsForItsCertificate(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, DNSNames: []string{"x.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	slow := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		time.Sleep(500 * time.Millisecond)
		return &pair, nil
	}
	visit := func(get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) error {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{ReadHeaderTimeout: 100 * time.Millisecond, ErrorLog: slog.NewLogLogger(slog.New(slog.NewTextHandler(io.Discard, nil)).Handler(), slog.LevelError),
			TLSConfig: &tls.Config{GetCertificate: get},
			Handler:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "hello") })}
		go srv.ServeTLS(ln, "", "")
		defer srv.Close()
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "x.example.test"}}, Timeout: 10 * time.Second}
		res, err := client.Get("https://" + ln.Addr().String() + "/")
		if err != nil {
			return err
		}
		res.Body.Close()
		return nil
	}
	if err := visit(slow); err == nil {
		t.Error("that was within the limit, so that this says nothing")
	}
	if err := visit(func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return patiently(h, slow) }); err != nil {
		t.Errorf("the certificate came, and the visitor got: %v", err)
	}
}
