// SPDX-License-Identifier: Apache-2.0

package ingress

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestPlatformHeader(t *testing.T) {
	for name, want := range map[string]bool{
		"X-Moorage-User":     true,
		"x-moorage-user":     true,
		"X_Moorage_User":     true,
		"X.Moorage.User":     true,
		"X~Moorage~User":     true,
		"X_Moorage.User":     true,
		"X-Moorage-Anything": true,
		"X_Forwarded_For":    true,
		"X.Forwarded.For":    true,
		"X.Real.IP":          true,
		// These are looked after where they are read, and spelled like this they may be a trusted proxy's.
		"X-Forwarded-For": false,
		"X-Real-IP":       false,
		"X-Request-ID":    false,
		// What apps have of their own.
		"Content-Type": false,
		"Accept":       false,
		"X-My-Header":  false,
		"X.My.Header":  false,
		"X_Moorage":    false,
		"XMoorageUser": false,
	} {
		if got := platformHeader(name); got != want {
			t.Errorf("platformHeader(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestHSTSHeader(t *testing.T) {
	const name = "Strict-Transport-Security"
	for _, c := range []struct {
		what    string
		handler http.HandlerFunc
		want    []string
	}{
		{"a response with no body", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }, []string{hstsValue}},
		{"a response that only writes", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("hello")) }, []string{hstsValue}},
		{"a stream", func(w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush()
			w.Write([]byte("later"))
		}, []string{hstsValue}},
		{"an app with its own", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(name, "max-age=60; includeSubDomains")
			w.WriteHeader(http.StatusOK)
		}, []string{"max-age=60; includeSubDomains"}},
		{"an answer that comes after an early one, which a proxy clears the headers after", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Link", "</a.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			clear(w.Header())
			w.WriteHeader(http.StatusOK)
		}, []string{hstsValue}},
		{"nothing at all", func(http.ResponseWriter, *http.Request) {}, []string{hstsValue}},
	} {
		srv := httptest.NewServer(HSTS(c.handler))
		res, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		srv.Close()
		if got := res.Header.Values(name); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.what, got, c.want)
		}
	}
}

// What streams and what takes over a connection find out by asking the writer, so that it has to pass those on.
func TestHSTSLetsThroughWhatItWraps(t *testing.T) {
	w := http.ResponseWriter(&hstsWriter{ResponseWriter: httptest.NewRecorder()})
	if _, ok := w.(http.Flusher); !ok {
		t.Error("it cannot flush")
	}
	if _, ok := w.(http.Hijacker); !ok {
		t.Error("it cannot be hijacked")
	}
	if _, _, err := http.NewResponseController(w).Hijack(); err == nil {
		t.Error("a recorder cannot be hijacked, and it says that it can")
	}
}
