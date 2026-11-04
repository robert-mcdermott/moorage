# Node.js release keys

The public keys of those who sign Node.js releases, past and present, as the project publishes them at
https://github.com/nodejs/release-keys (in `keys/`). Moorage checks the signature on a release's checksums
against these, so that a new releaser's key needs a new moor, unless `signatures = "warn"` is set under
`[toolchains]` in `server.toml`.

To bring them up to date, replace the `.asc` files with those from `keys/` in that repository.
