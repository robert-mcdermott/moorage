// Poll: a JSON API, and the page Vite built into dist/.
//
//   node server.js

import path from "node:path";
import { fileURLToPath } from "node:url";
import express from "express";
import { Votes } from "./votes.js";

const here = path.dirname(fileURLToPath(import.meta.url));

// The question and its answers come from [env] in moorage.toml, and can be
// changed on the server with moor env set.
const question = process.env.POLL_QUESTION || "What do you deploy most?";
const options = (process.env.POLL_OPTIONS || "Go,Python,Node.js,Static sites")
  .split(",")
  .map((option) => option.trim())
  .filter(Boolean);

// MOORAGE_DATA_DIR survives deploys; the app's own directory is read-only
// once it is built. Locally, use ./data.
const votes = new Votes(path.join(process.env.MOORAGE_DATA_DIR || "data", "votes.jsonl"), options);

const app = express();
app.disable("x-powered-by");
// Requests come through Moorage's proxy on 127.0.0.1, which says in
// X-Forwarded-* headers how the browser reached the app.
app.set("trust proxy", "loopback");
app.use(express.json());

app.get("/healthz", (req, res) => res.type("text").send("ok\n"));

app.get("/api/poll", (req, res) => {
  res.json({ question, results: votes.results(), release: process.env.MOORAGE_RELEASE || "local" });
});

app.post("/api/votes", (req, res) => {
  const option = req.body?.option;
  if (!options.includes(option)) {
    return res.status(400).json({ error: `vote for one of: ${options.join(", ")}` });
  }
  votes.add(option);
  res.status(201).json({ results: votes.results() });
});

// The page. Vite names the files under /assets after a hash of their
// content, so they can be cached for good; index.html is checked each time.
const dist = path.join(here, "dist");
app.use("/assets", express.static(path.join(dist, "assets"), { immutable: true, maxAge: "1y" }));
app.use(express.static(dist, { maxAge: 0 }));

// Errors, such as a request whose JSON does not parse: answered in JSON,
// without the stack trace.
app.use((error, req, res, next) => {
  const status = error.status || 500;
  if (status >= 500) console.error(error);
  res.status(status).json({ error: status < 500 ? error.message : "something went wrong" });
});

// Moorage sets HOST and PORT for the web process.
const host = process.env.HOST || "127.0.0.1";
const port = Number(process.env.PORT || 3000);
const server = app.listen(port, host, (error) => {
  if (error) throw error;
  console.log(`listening on http://${host}:${port}`);
});

// Moorage stops an instance with SIGTERM once traffic has moved to the new
// release: finish the requests in flight, then exit.
process.on("SIGTERM", () => {
  console.log("stopping");
  server.close(() => process.exit(0));
});
