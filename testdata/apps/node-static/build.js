// Builds dist/index.html from message.txt, as a static site generator would.
const fs = require("node:fs");
const message = fs.readFileSync("message.txt", "utf8").trim();
fs.mkdirSync("dist", { recursive: true });
fs.writeFileSync("dist/index.html", `<!doctype html>\n<title>Sample</title>\n<p>${message} from a site built with node ${process.version}</p>\n`);
console.log("built dist/index.html");
