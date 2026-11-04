# Deploying a static site

A directory of files — documentation, a report, a single-page app — is served
as it is, from Moorage itself: no process runs for it.

## Upload a directory

```bash
moor deploy --static ./public --app docs
```

Every deploy is a new release, swapped in at once; `moor rollback` brings
the previous one back in a moment.

## Or build it

From source, Moorage detects a static site when the directory has an
`index.html` (at the top or in `dist`, `build`, `out`, `public`, `_site` or
`site`), and runs a build command first if you give one:

```toml
[app]
name = "docs"
runtime = "static"
toolchains = ["hugo@system"]   # the server's own hugo

[build]
command = "hugo --minify"

[static]
dir = "public"
```

```bash
moor deploy
```

A Node.js project without a `start` script is served the same way, from
the output of its build (see [Node.js](node.md)).

## Serving rules

`[static]` says how files are served:

```toml
[static]
dir = "dist"
spa = true                 # serve index.html for unknown paths
clean_urls = true          # /about serves about.html
not_found = "404.html"
trailing_slash = "auto"    # or "always", "never"

[[static.headers]]
for = "/assets/*"
values = { "Cache-Control" = "public, max-age=31536000, immutable" }

[[static.redirects]]
from = "/blog/:year/*"
to = "/posts/:year/:splat"
status = 301
```

Files whose names carry a content hash (`app.3f2a9c1b.js`) are cached as
immutable, HTML is revalidated, and pre-compressed `.br` and `.gz` files are
served to browsers that accept them. `listing = true` lists directories, for
file shares.

A complete site: the [Handbook example](../examples/static-site), with clean
URLs, a 404 page, headers and redirects.
