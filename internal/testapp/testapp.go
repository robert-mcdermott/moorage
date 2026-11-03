// Package testapp is a small app for tests: test binaries run it, in place
// of a real app, when started with MOOR_TEST_ROLE=app. It serves HTTP on
// $PORT (or, without a port, runs as a worker) and behaves as its
// environment asks:
//
//	TESTAPP_BODY         body of GET / (default: "hello from $MOORAGE_RELEASE")
//	TESTAPP_FAIL         exit with status 1 at once
//	TESTAPP_START_DELAY  wait this long (a Go duration) before listening
//	TESTAPP_IGNORE_TERM  ignore the stop signal (to test forced stops)
//
// Paths: /health, /pid, /env?name=X, /headers, /slow?ms=N, /crash, /sick
// (from then on /health fails), and /upgrade, which switches to an echo
// protocol (to test proxied upgrades).
package testapp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

var sick atomic.Bool

// Main runs the app and returns its exit status.
func Main() int {
	fmt.Println("testapp starting, pid", os.Getpid())
	if os.Getenv("TESTAPP_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "testapp: failing as asked")
		return 1
	}
	if d, err := time.ParseDuration(os.Getenv("TESTAPP_START_DELAY")); err == nil {
		time.Sleep(d)
	}
	stop := make(chan os.Signal, 1)
	if os.Getenv("TESTAPP_IGNORE_TERM") == "" {
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	} else {
		// Handle the signal and do nothing: on Windows, CTRL_BREAK ends a
		// process that merely ignores interrupts.
		ignored := make(chan os.Signal, 1)
		signal.Notify(ignored, os.Interrupt, syscall.SIGTERM)
		go func() {
			for range ignored {
			}
		}()
	}

	port := os.Getenv("PORT")
	if port == "" {
		fmt.Println("worker running")
		<-stop
		fmt.Println("worker stopping")
		return 0
	}
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: handler()}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testapp:", err)
		return 1
	}
	fmt.Println("listening on", ln.Addr())
	go srv.Serve(ln)
	<-stop
	fmt.Println("stopping")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	return 0
}

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body := os.Getenv("TESTAPP_BODY")
		if body == "" {
			body = "hello from " + os.Getenv("MOORAGE_RELEASE")
		}
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if sick.Load() {
			http.Error(w, "sick", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/sick", func(w http.ResponseWriter, r *http.Request) { sick.Store(true); fmt.Fprint(w, "now sick") })
	mux.HandleFunc("/pid", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, os.Getpid()) })
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, os.Getenv(r.URL.Query().Get("name"))) })
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Request-Id", "X-Moorage-User"} {
			fmt.Fprintf(w, "%s: %s\n", h, r.Header.Get(h))
		}
		fmt.Fprintf(w, "Host: %s\n", r.Host)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Fprint(w, "slow done")
	})
	mux.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(os.Stderr, "testapp: crashing as asked")
		os.Exit(3)
	})
	mux.HandleFunc("/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "expected Upgrade: echo", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		sc := bufio.NewScanner(rw)
		for sc.Scan() {
			rw.WriteString("echo: " + sc.Text() + "\n")
			rw.Flush()
		}
	})
	return mux
}
