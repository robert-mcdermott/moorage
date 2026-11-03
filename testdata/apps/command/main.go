// A sample app built with its own command: GET / answers with message.txt.
package main

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
)

func main() {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		msg, _ := os.ReadFile("message.txt")
		fmt.Fprintf(w, "%s from a command build (%s)\n", strings.TrimSpace(string(msg)), runtime.Version())
	})
	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
