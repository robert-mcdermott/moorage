# Deploying a Go app

Moorage builds a Go app from its source with the Go version its `go.mod`
asks for, and runs the binary.

## The app

The app listens on `127.0.0.1` and the port in `$PORT`:

```go
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hello from Go")
	})
	http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil)
}
```

No manifest is needed: a `go.mod` is enough. A `moorage.toml` names the app
and a health check:

```toml
[app]
name = "hello"

[processes.web]
health = { path = "/healthz" }
```

## Deploy

See what Moorage will do, then do it:

```bash
moor manifest explain
```

```bash
moor deploy
```

The build is shown as it happens: the Go toolchain is downloaded (once,
and verified against its published checksum), the app is built with
`go build -trimpath -ldflags='-s -w'`, and the new release takes over once
it answers its health check.

## How it builds

- **Go version:** the `toolchain` line of `go.mod`, or else the newest patch
  release of the `go` line's version (`go 1.26` builds with the newest
  1.26.x). `[go].version` overrides both. Builds run with `GOTOOLCHAIN=local`.
- **Main package:** the top directory when it is `package main`, or else the
  single `./cmd/*` with one; set `[go].main` otherwise. The binary is named
  after the module (or the main package) and placed in `$MOORAGE_BIN`;
  `[go].binary` renames it.
- **cgo** is off; `[go].cgo = true` turns it on (the server then needs a C
  compiler). `[go].tags` and `[go].ldflags` pass build tags and linker flags.
- Modules and the build cache are shared between builds, so later builds are
  quick.

`[build].command` replaces the build entirely, and `[build].pre` and
`[build].post` run before and after it.

## Running it

- A web process gets `PORT` and `HOST=127.0.0.1`. Moorage sends it a TERM
  signal to stop, then kills it after 30 seconds (`stop_timeout`).
- The app runs in its build directory, whose files are read-only. Write data
  to `$MOORAGE_DATA_DIR`, which survives deploys.
- More processes, such as a worker run by the same binary, go under
  `[processes]` (a process that does not listen on `PORT` counts as healthy
  once it has run for five seconds):

```toml
[processes]
worker = '"$MOORAGE_BIN/hello" -worker'
```

Logs, restarts and rollbacks work the same for every app:

```bash
moor logs -f
```

```bash
moor rollback
```
