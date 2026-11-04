// The votes, kept in a file of JSON lines that only grows: one line per
// vote. For a moment during a deploy the old and the new release run at
// once; both append to the file, and each reads what the other appended, so
// no vote is lost.

import fs from "node:fs";
import path from "node:path";

export class Votes {
  constructor(file, options) {
    this.file = file;
    this.offset = 0; // how much of the file has been read
    this.counts = new Map(options.map((option) => [option, 0]));
    fs.mkdirSync(path.dirname(file), { recursive: true });
  }

  add(option) {
    fs.appendFileSync(this.file, JSON.stringify({ option, time: new Date().toISOString() }) + "\n");
  }

  results() {
    this.#refresh();
    return [...this.counts].map(([option, votes]) => ({ option, votes }));
  }

  // #refresh reads the lines appended since the last read. The file is
  // small and local, so it is read synchronously.
  #refresh() {
    let fd;
    try {
      fd = fs.openSync(this.file, "r");
    } catch (error) {
      if (error.code === "ENOENT") return;
      throw error;
    }
    try {
      const size = fs.fstatSync(fd).size;
      if (size <= this.offset) return;
      const chunk = Buffer.alloc(size - this.offset);
      fs.readSync(fd, chunk, 0, chunk.length, this.offset);
      const end = chunk.lastIndexOf(0x0a) + 1; // a line still being written is read next time
      this.offset += end;
      for (const line of chunk.subarray(0, end).toString("utf8").split("\n")) {
        if (!line) continue;
        const { option } = JSON.parse(line);
        if (this.counts.has(option)) this.counts.set(option, this.counts.get(option) + 1);
      }
    } finally {
      fs.closeSync(fd);
    }
  }
}
