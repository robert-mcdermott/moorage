# Links: Go with only the standard library

A link shortener: a page to add links, and short addresses that redirect to
them and count the visits. It uses only Go's standard library, and its page
and stylesheet are built into the binary.

What it shows:

- **No manifest needed to build.** A `go.mod` is enough: Moorage installs the
  Go version it names, builds the binary and runs it.
  [`moorage.toml`](moorage.toml) only names the app and its health check.
- **`HOST`, `PORT` and `MOORAGE_DATA_DIR`.** The app listens where Moorage
  says, and keeps its data where it survives deploys
  ([`main.go`](main.go)).
- **A graceful stop.** On a deploy, the old instance gets `SIGTERM` once
  traffic has moved to the new one; it finishes the requests in flight, then
  exits.
- **Data that two instances can share.** For a moment during a deploy, the
  old and the new release run at once. The links live in a file of JSON
  lines that only grows ([`store.go`](store.go)): each instance appends, and
  reads what the other appended, so no visit is lost.

## Deploy it

From this directory, with a Moorage server
([installing Moorage](../../docs/install.md)):

```bash
moor deploy
```

The first deploy downloads Go (once, checked against its published
checksum); later builds reuse the module and build caches. Then:

```bash
moor logs -f
```

```bash
moor restart     # new instances, without dropping a request
```

## Run it locally

```bash
go run .
```

Then open <http://127.0.0.1:8080>. Locally, the links go to
`./data/links.jsonl`.

## The routes

| Request | What it does |
|---|---|
| `GET /` | The page: the form and the links |
| `POST /links` | Adds a link (`url`, and an optional `code`) |
| `GET /{code}` | Redirects to the link, and counts the visit |
| `GET /healthz` | Answers `ok` |
