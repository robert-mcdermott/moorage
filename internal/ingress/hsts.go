// SPDX-License-Identifier: Apache-2.0

package ingress

import (
	"bufio"
	"net"
	"net/http"
)

const (
	hstsHeader = "Strict-Transport-Security"
	// A year, for the one hostname that answered: its subdomains may well be somebody else's, and nothing here can speak for them.
	hstsValue = "max-age=31536000"
)

// HSTS tells browsers to use nothing but HTTPS from now on for each hostname that next answers, unless what answered had
// something of its own to say about that.
func HSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hw := &hstsWriter{ResponseWriter: w}
		next.ServeHTTP(hw, r)
		// One that wrote nothing is answered once this is over.
		hw.decide()
	})
}

// hstsWriter decides at the last moment, when an app's own headers are there to be seen. A proxy clears the headers after a
// 1xx response, so that those do not count.
type hstsWriter struct {
	http.ResponseWriter
	decided bool
}

func (w *hstsWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	if w.Header().Get(hstsHeader) == "" {
		w.Header().Set(hstsHeader, hstsValue)
	}
}

func (w *hstsWriter) WriteHeader(code int) {
	if code >= 200 {
		w.decide()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *hstsWriter) Write(b []byte) (int, error) {
	w.decide()
	return w.ResponseWriter.Write(b)
}

func (w *hstsWriter) Flush() {
	w.decide()
	http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *hstsWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *hstsWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
