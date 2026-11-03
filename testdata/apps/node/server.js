const fs = require("node:fs");
const http = require("node:http");

http
  .createServer((req, res) => {
    res.setHeader("Content-Type", "text/plain");
    if (req.url === "/health") {
      res.end("ok\n");
      return;
    }
    const message = fs.readFileSync("message.txt", "utf8").trim();
    res.end(`${message} from node ${process.version} (${process.env.NODE_ENV})\n`);
  })
  .listen(Number(process.env.PORT), "127.0.0.1", () => console.log(`listening on 127.0.0.1:${process.env.PORT}`));
