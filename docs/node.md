# Deploying a Node.js app

Moorage builds Node.js apps with npm, with the Node.js version the project
asks for, and runs the `start` script — or serves the build's output as a
static site when there is none.

## The app

```json
{
  "name": "hello",
  "private": true,
  "scripts": {
    "start": "node server.js"
  },
  "engines": { "node": ">=22" }
}
```

```js
// server.js
const http = require("node:http");

http
  .createServer((req, res) => res.end(req.url === "/healthz" ? "ok" : "hello from Node.js"))
  .listen(Number(process.env.PORT), "127.0.0.1");
```

A `build` script, if there is one (`tsc`, `vite build`…), runs after the
install.

Commit `package-lock.json` (`npm install` makes it). A `moorage.toml` is
optional:

```toml
[app]
name = "hello"

[processes.web]
health = { path = "/healthz" }
```

## Deploy

```bash
moor manifest explain
```

```bash
moor deploy
```

## How it builds

- **Node.js version:** `.nvmrc`, or else `.node-version`, or else
  `engines.node`, or else the current LTS release; `[node].version` overrides
  them. Partial versions (`24`) and ranges (`>=22`, `^22.1`, `24.x`,
  `lts/*`) are resolved to the newest matching release (a `>=` range prefers
  LTS releases).
- **Dependencies:** `npm ci` (or `npm install`, with a warning, without a
  lockfile), then the `build` script if there is one (`[node].build_script`
  names another), then `npm prune --omit=dev` (`[node].prune = false` keeps
  development dependencies). pnpm, yarn and bun are not available yet; to use
  one installed on the server, set `[build].command` and
  `toolchains = ["pnpm@system"]` under `[app]`.
- `node_modules/.bin` comes first on `PATH`, and npm's cache is shared
  between builds.

## Running it

- The `start` script runs directly — not through `npm start` — so that the
  stop signal reaches the app. `NODE_ENV=production` is set.
- Without a `start` script, the build's `dist/`, `build/` or `out/` (the
  first with an `index.html`) is served as a static site, with the rules of
  `[static]` (see [static sites](static.md)): single-page apps set
  `spa = true`.

A complete app: the [Poll example](../examples/node-express), an Express API
with a Vite front end.
