// The build: copies the server into dist/, as a bundler would.
const fs = require("node:fs");
fs.mkdirSync("dist", { recursive: true });
fs.copyFileSync("server.js", "dist/server.js");
console.log("built dist/server.js");
