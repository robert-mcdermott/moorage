// SPDX-License-Identifier: Apache-2.0
// A small driver for headless Chrome, for the documentation's screenshots: it starts the browser (or Chromium, Edge,
// or Brave; CHROME chooses), evaluates scripts in the page, and captures it. It uses nothing beyond Node 22.

import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const CANDIDATES = [
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
  '/Applications/Brave Browser.app/Contents/MacOS/Brave Browser',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
];

function findChrome() {
  const found = process.env.CHROME ?? CANDIDATES.find((path) => existsSync(path));
  if (!found) throw new Error('No Chrome, Chromium, Edge or Brave found: set CHROME to its executable.');
  return found;
}

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// A browser of its own, with a window of width × height CSS pixels captured at `scale` device pixels for each. Chrome
// picks a free debugging port, which its profile's DevToolsActivePort names, so that a browser left running by an
// earlier run is never the one used.
export async function launch({ width = 1360, height = 850, scale = 1.25 } = {}) {
  const profile = mkdtempSync(join(tmpdir(), 'moorage-capture-'));
  const chrome = spawn(findChrome(), ['--headless=new', '--remote-debugging-port=0', `--user-data-dir=${profile}`,
    '--no-first-run', '--no-default-browser-check', '--hide-scrollbars', '--force-color-profile=srgb',
    `--window-size=${width},${height}`, 'about:blank'], { stdio: 'ignore' });
  let page = null;
  const deadline = Date.now() + 60000;
  while (!page && Date.now() < deadline) {
    try {
      const port = Number(readFileSync(join(profile, 'DevToolsActivePort'), 'utf8').split('\n')[0]);
      if (port) page = (await (await fetch(`http://127.0.0.1:${port}/json`, { signal: AbortSignal.timeout(5000) })).json()).find((t) => t.type === 'page');
    } catch { /* not up yet */ }
    if (!page) await sleep(200);
  }
  if (!page) {
    chrome.kill();
    throw new Error('Chrome did not start.');
  }
  const socket = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('Chrome did not open its debugging connection.')), 15000);
    socket.addEventListener('open', () => { clearTimeout(timer); resolve(); }, { once: true });
  });
  let next = 1;
  const pending = new Map();
  socket.addEventListener('message', (event) => {
    const message = JSON.parse(event.data);
    if (!message.id || !pending.has(message.id)) return;
    const { resolve, reject } = pending.get(message.id);
    pending.delete(message.id);
    if (message.error) reject(new Error(message.error.message));
    else resolve(message.result);
  });
  const send = (method, params = {}) => new Promise((resolve, reject) => {
    const id = next++;
    pending.set(id, { resolve, reject });
    socket.send(JSON.stringify({ id, method, params }));
  });
  const size = (w, h, mobile = false, s = scale) => send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: s, mobile });
  await size(width, height);
  await send('Page.enable');
  await send('Runtime.enable');
  await send('Network.enable');
  const evaluate = async (expression) => {
    const result = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description ?? result.exceptionDetails.text);
    return result.result.value;
  };
  return {
    send,
    size,
    eval: evaluate,
    // Opens a page, and waits until it has loaded.
    async goto(url, settle = 500) {
      await send('Page.navigate', { url });
      await sleep(100);
      for (let i = 0; i < 200; i += 1) {
        try {
          if (await evaluate(`document.readyState === 'complete' && location.href !== 'about:blank'`)) break;
        } catch { /* navigating */ }
        await sleep(100);
      }
      await sleep(settle);
    },
    theme: (scheme) => send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: scheme }] }),
    cookies: async () => (await send('Network.getCookies')).cookies,
    clearCookies: () => send('Network.clearBrowserCookies'),
    setCookies: (cookies) => send('Network.setCookies', { cookies }),
    // The window, or `clip` ({x, y, width, height} in CSS pixels of the page, which may reach below the window).
    async capture(file, { clip = null, quality = 82, format = 'webp' } = {}) {
      const params = { format, quality, captureBeyondViewport: Boolean(clip) };
      if (clip) params.clip = { ...clip, scale: 1 };
      const { data } = await send('Page.captureScreenshot', params);
      writeFileSync(file, Buffer.from(data, 'base64'));
    },
    async close() {
      const exited = new Promise((resolve) => chrome.once('exit', resolve));
      send('Browser.close').catch(() => { /* closing */ });
      await Promise.race([exited, sleep(5000)]);
      if (chrome.exitCode === null && chrome.signalCode === null) chrome.kill('SIGKILL');
      try { rmSync(profile, { recursive: true, force: true, maxRetries: 5 }); } catch { /* a temporary folder */ }
    },
  };
}
