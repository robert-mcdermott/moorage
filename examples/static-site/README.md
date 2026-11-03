# Handbook: a static site

A small team handbook: a directory of HTML pages and a stylesheet, served by
Moorage itself. No process runs for it, and nothing is built.

What it shows ([`moorage.toml`](moorage.toml)):

- **Clean URLs.** `public/about.html` is served at `/about`, and
  `/about.html` redirects there; `public/guide/index.html` is `/guide/`.
- **A 404 page of the site's own**, for addresses that are not pages.
- **Headers** for every file, and a cache lifetime for the stylesheet.
- **Redirects** that keep old links working: `/docs/…` to `/guide/…`, and
  `/team` to `/about`.
- **Atomic releases.** Every deploy is a new release, swapped in at once;
  `moor rollback` brings the previous one back in a moment.

## Deploy it

From this directory, with a Moorage server
([installing Moorage](../../docs/install.md)):

```bash
moor deploy
```

The site is at `https://handbook.apps.example.com` (or
`http://handbook.localhost:8080` on your own computer). Edit a page and
deploy again: the new release replaces the old one at once.

## Run it locally

Any static file server will do, without the rules of `moorage.toml`:

```bash
python3 -m http.server --directory public
```

[Static sites](../../docs/static.md) and the
[`[static]` reference](../../docs/manifest.md#static) describe every rule.
