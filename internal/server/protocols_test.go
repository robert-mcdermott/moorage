package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/testroles"
)

func protocolManifest(more string) map[string]string {
	return map[string]string{"moorage.toml": `[app]
runtime = "command"

[processes.web]
command = ` + tomlString(testroles.AppCommand()) + `
health = { path = "/health", interval = "100ms", startup = "30s", threshold = 2 }
` + more + `

[deploy]
drain = "1s"
`}
}

// cleartextClient speaks HTTP/2 from the first byte, as a gRPC client does with no TLS.
func cleartextClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{Protocols: new(http.Protocols)}
	transport.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func TestCleartextHTTP2Backend(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	files := protocolManifest("protocol = \"h2c\"\n")
	files["moorage.toml"] += "\n[env]\nTESTAPP_H2C = \"1\"\n"
	// It is healthy only if the health check speaks what it speaks.
	deployedSource(t, c, "grpcish", files)
	host := "grpcish.localhost"

	// Someone with HTTP/1.1 reaches it too, as the server speaks HTTP/2 to it for them.
	if res, body := get(t, addrs.HTTP, host, "/proto"); res.StatusCode != http.StatusOK || body != "HTTP/2.0" {
		t.Errorf("over HTTP/1.1: %d %q", res.StatusCode, body)
	}

	// A stream, both ways at once, with HTTP/2 all along, and a trailer at its end.
	reader, writer := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addrs.HTTP.String()+"/stream", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	res, err := cleartextClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 || res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/grpc" {
		t.Fatalf("%s %d %v", res.Proto, res.StatusCode, res.Header)
	}
	lines := bufio.NewReader(res.Body)
	for _, word := range []string{"one", "two"} {
		// Each is answered before the next is sent, so that nothing here is waiting for the whole of anything.
		if _, err := fmt.Fprintln(writer, word); err != nil {
			t.Fatal(err)
		}
		line, err := lines.ReadString('\n')
		if err != nil || line != "heard "+word+"\n" {
			t.Fatalf("%q %v", line, err)
		}
	}
	writer.Close()
	if rest, err := io.ReadAll(lines); err != nil || len(rest) != 0 {
		t.Fatalf("%q %v", rest, err)
	}
	if got := res.Trailer.Get("Grpc-Status"); got != "0" {
		t.Errorf("the trailer: %q in %v", got, res.Trailer)
	}
}

func TestPlainListenerSpeaksBoth(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	deployedSource(t, c, "plain", protocolManifest(""))
	req, err := http.NewRequest(http.MethodGet, "http://"+addrs.HTTP.String()+"/proto", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "plain.localhost"
	res, err := cleartextClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	// HTTP/2 from the client, and the app goes on hearing what it speaks.
	if res.ProtoMajor != 2 || string(body) != "HTTP/1.1" {
		t.Errorf("%s %q", res.Proto, body)
	}
}

func TestStickySessions(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	deployedSource(t, c, "sticky", protocolManifest("sticky = true\ninstances = 3\n"))
	host := "sticky.localhost"
	base := "http://" + addrs.HTTP.String()
	client := &http.Client{Timeout: 10 * time.Second}
	ask := func(path string, cookies ...*http.Cookie) (string, *http.Response) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return string(body), res
	}
	cookieOf := func(res *http.Response) *http.Cookie {
		for _, cookie := range res.Cookies() {
			if strings.HasPrefix(cookie.Name, "moorage_sticky_") {
				return cookie
			}
		}
		return nil
	}

	// Strangers are spread over the instances.
	seen := map[string]bool{}
	for range 60 {
		instance, _ := ask("/instance")
		seen[instance] = true
	}
	if len(seen) != 3 {
		t.Errorf("60 requests with no cookie reached %d instances", len(seen))
	}

	instance, res := ask("/instance")
	cookie := cookieOf(res)
	if cookie == nil || !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("the cookie: %+v", cookie)
	}
	if strings.Contains(cookie.Value, instance) {
		t.Errorf("the cookie names the instance: %s", cookie.Value)
	}
	for range 30 {
		again, res := ask("/instance", cookie)
		if again != instance {
			t.Fatalf("it moved from %s to %s", instance, again)
		}
		if cookieOf(res) != nil {
			t.Fatal("a cookie that is right was set again")
		}
	}

	// The app hears its own cookies, and not this one.
	heard, _ := ask("/cookies", cookie, &http.Cookie{Name: "theirs", Value: "kept"})
	if heard != "theirs=kept" {
		t.Errorf("the app heard %q", heard)
	}

	// A value that means nothing gets an instance, and a cookie that does.
	_, res = ask("/instance", &http.Cookie{Name: cookie.Name, Value: "made-up"})
	if got := cookieOf(res); got == nil || got.Value == "made-up" {
		t.Errorf("%+v", got)
	}

	// An instance that is unhealthy gets no requests, cookie or not, and whoever was with it moves on.
	if body, _ := ask("/sick", cookie); !strings.Contains(body, "sick") {
		t.Fatal(body)
	}
	waitUntil(t, "the sick instance to lose its visitor", func() bool {
		now, res := ask("/instance", cookie)
		return now != instance && cookieOf(res) != nil
	})
}
