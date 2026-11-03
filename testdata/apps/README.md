# Sample apps

One small app per runtime, built and deployed from source by the sample-app
test (`MOORAGE_TEST_BUILDS=1 go test ./internal/server -run TestSampleApps`),
which downloads the toolchains they need. Each app answers `GET /` with the
text of its `message.txt` and the runtime it runs on, and `GET /health` with
`ok`.

| App | Runtime | What it shows |
|---|---|---|
| `go` | `go` | A partial version from `go.mod` (`go 1.26`), the preset build and web command, a health path |
| `python` | `python` | uv with a lockfile, a partial version (`.python-version`), the project's script as the web command, a release command |
| `node` | `node` | npm with a lockfile, a version from `.nvmrc`, the build script, the start script run directly |
| `node-static` | `node` | A version range from `engines.node`; no start script, so the build's `dist/` is served |
| `command` | `command` | An extra toolchain (`go@1.26`) and a build command of its own |
| `binary` | `binary` | A script run as it is, with the interpreter on the host |
| `static` | `static` | A directory served as it is |
