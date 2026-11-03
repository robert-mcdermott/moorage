// A guestbook: a page, a form, and a file in the data directory. It has no
// dependencies, and an image with nothing else in it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// entry is one line of the book.
type entry struct {
	Name    string    `json:"name"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// book is the entries file: one JSON document per line, so that a new entry is
// one append, and two instances of the app can share it during a deploy.
type book struct {
	path string
	mu   sync.Mutex
}

func (b *book) add(e entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.OpenFile(b.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func (b *book) entries() ([]entry, error) {
	data, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			out = append(out, e)
		}
	}
	slices.Reverse(out)
	return out, nil
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Guestbook</title>
<style>
  body { font: 16px/1.5 system-ui, sans-serif; max-width: 34rem; margin: 3rem auto; padding: 0 1rem; color: #1c2733; }
  input, textarea, button { font: inherit; width: 100%; box-sizing: border-box; margin-bottom: .5rem; padding: .5rem; }
  button { width: auto; padding: .5rem 1rem; }
  li { list-style: none; border-top: 1px solid #d5dce3; padding: .75rem 0; }
  ul { padding: 0; }
  small { color: #5b6b7a; }
</style>
<h1>Guestbook</h1>
<form method="post" action="/sign">
  <input name="name" placeholder="Your name" required maxlength="80">
  <textarea name="message" placeholder="Say something" required maxlength="500"></textarea>
  <button>Sign</button>
</form>
<ul>
{{range .Entries}}<li><strong>{{.Name}}</strong> <small>{{.At.Format "2 Jan 2006 15:04"}}</small><br>{{.Message}}</li>
{{else}}<li>Nobody has signed yet.</li>
{{end}}</ul>
<small>Running {{.Release}} in a container.</small>
</html>
`))

func server(b *book) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		entries, err := b.entries()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page.Execute(w, map[string]any{"Entries": entries, "Release": os.Getenv("MOORAGE_RELEASE")})
	})
	mux.HandleFunc("POST /sign", func(w http.ResponseWriter, r *http.Request) {
		name, message := strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("message"))
		if name == "" || message == "" {
			http.Error(w, "a name and a message, please", http.StatusBadRequest)
			return
		}
		if err := b.add(entry{Name: name, Message: message, At: time.Now().UTC()}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	// The health check reads the data directory: the app is no use without it.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(filepath.Dir(b.path)); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// migrate is the release command: it brings the data directory up to date.
// This app's first version has one thing to do.
func migrate(dir string) error {
	marker := filepath.Join(dir, "guestbook.version")
	if _, err := os.Stat(marker); err == nil {
		fmt.Println("the guestbook is up to date")
		return nil
	}
	if err := os.WriteFile(marker, []byte("1\n"), 0o644); err != nil {
		return err
	}
	fmt.Println("set up the guestbook in", dir)
	return nil
}

func main() {
	dir := os.Getenv("MOORAGE_DATA_DIR")
	if dir == "" {
		dir = "."
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := migrate(dir); err != nil {
			log.Fatal(err)
		}
		return
	}
	host, port := os.Getenv("HOST"), os.Getenv("PORT")
	if port == "" {
		host, port = "127.0.0.1", "8000"
	}
	srv := &http.Server{
		Addr:              net.JoinHostPort(host, port),
		Handler:           server(&book{path: filepath.Join(dir, "entries.jsonl")}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// On a deploy, the old instance is told to stop, and finishes its requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
