# Poll: Express and Vite

A live poll: an [Express](https://expressjs.com) API, and a page that
[Vite](https://vite.dev) builds when the app is deployed. The results update
every few seconds.

What it shows:

- **The whole Node.js build.** Moorage installs the Node.js that `engines`
  asks for (`>=22`: the newest LTS release), runs `npm ci`, the `build`
  script (`vite build`), then `npm prune --omit=dev`, and runs the `start`
  script (`node server.js`) directly, so that the stop signal reaches the
  app.
- **A front end built at deploy time.** Vite's output goes to `dist/`, which
  the server serves; files under `/assets` carry a hash of their content in
  their names and are cached for good. The same works for React, Vue or
  Svelte front ends.
- **Variables.** The question and the answers come from `[env]` in
  [`moorage.toml`](moorage.toml) and can be changed on the server without a
  new build.
- **Data that two instances can share.** Votes are appended to a file in
  `$MOORAGE_DATA_DIR` ([`votes.js`](votes.js)), so none is lost while the old
  and the new release both run during a deploy.
- **A graceful stop** on `SIGTERM` ([`server.js`](server.js)).

## Deploy it

From this directory, with a Moorage server
([installing Moorage](../../docs/install.md)):

```bash
moor deploy
```

Then change the question; the change is a new release, rolled out without
downtime:

```bash
moor env set POLL_QUESTION="Tabs or spaces?" POLL_OPTIONS="Tabs,Spaces"
```

## Run it locally

```bash
npm install
```

```bash
npm run dev:api     # the API, on http://127.0.0.1:3000
```

```bash
npm run dev:web     # the page, on http://127.0.0.1:5173, reloaded as you edit
```

Or build the page and serve both from the server, as Moorage does:

```bash
npm run build && npm start
```

## The API

| Request | What it does |
|---|---|
| `GET /api/poll` | The question, the results and the release |
| `POST /api/votes` `{"option": "Go"}` | Adds a vote and returns the results |
| `GET /healthz` | Answers `ok` |

It is a demonstration, not a ballot: nothing stops a second vote.
