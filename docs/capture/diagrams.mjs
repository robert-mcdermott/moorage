// SPDX-License-Identifier: Apache-2.0
// Draws the documentation's diagrams, in the light and dark themes, as docs/images/<name>-<theme>.svg, and the picture that
// link previews show, as docs/site/assets/og-image.jpg. GitHub and the website both show them as they are.
//
//   node docs/capture/diagrams.mjs
//
// Each diagram is plain SVG with its colours from one table, so that the two themes cannot differ in anything else.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { launch, sleep } from './cdp.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const IMAGES = join(ROOT, 'docs/images');

const THEMES = {
  light: { bg: '#ffffff', panel: '#f5f6f8', box: '#ffffff', line: '#c6ccd5', text: '#171b22', muted: '#555f6e', accent: '#0b7372', accentSoft: '#e0f1f0', blue: '#2a6aa3', blueSoft: '#e3eef8', warn: '#8a5a00', warnSoft: '#faefd8', arrow: '#555f6e' },
  dark: { bg: '#111419', panel: '#191d24', box: '#20262f', line: '#3a434f', text: '#e8ebef', muted: '#a3adb9', accent: '#4cc4b8', accentSoft: '#12322f', blue: '#7ab3e6', blueSoft: '#17293b', warn: '#e0b050', warnSoft: '#33290f', arrow: '#a3adb9' },
};

const FONT = `font-family="ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif"`;
const MONO = `font-family="ui-monospace, 'SF Mono', Menlo, Consolas, monospace"`;
const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

// Helpers for one theme's drawing.
function kit(t) {
  const text = (x, y, s, { size = 15, weight = 400, fill = t.text, anchor = 'start', mono = false } = {}) =>
    `<text x="${x}" y="${y}" ${mono ? MONO : FONT} font-size="${size}" font-weight="${weight}" fill="${fill}" text-anchor="${anchor}">${esc(s)}</text>`;
  const rect = (x, y, w, h, { fill = t.box, stroke = t.line, r = 10, dash = '' } = {}) =>
    `<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="${r}" fill="${fill}" stroke="${stroke}" stroke-width="1.5"${dash ? ` stroke-dasharray="${dash}"` : ''}/>`;
  // A box with a title and lines under it.
  const box = (x, y, w, h, title, lines = [], o = {}) => rect(x, y, w, h, o)
    + text(x + 14, y + 27, title, { size: 16, weight: 650, fill: o.titleFill ?? t.text })
    + lines.map((l, i) => text(x + 14, y + 49 + i * 20, l, { size: 14.5, fill: t.muted })).join('');
  const arrow = (x1, y1, x2, y2, { label = '', dy = -7, color = t.arrow, anchor = 'middle', lx = null, ly = null } = {}) => {
    const a = Math.atan2(y2 - y1, x2 - x1);
    const head = (d) => `${x2 - 10 * Math.cos(a + d)},${y2 - 10 * Math.sin(a + d)}`;
    return `<line x1="${x1}" y1="${y1}" x2="${x2 - 4 * Math.cos(a)}" y2="${y2 - 4 * Math.sin(a)}" stroke="${color}" stroke-width="1.6"/>`
      + `<polygon points="${x2},${y2} ${head(0.45)} ${head(-0.45)}" fill="${color}"/>`
      + (label ? text(lx ?? (x1 + x2) / 2, ly ?? (y1 + y2) / 2 + dy, label, { size: 14, fill: t.muted, anchor }) : '');
  };
  return { text, rect, box, arrow };
}

const svg = (w, h, body, t, title) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${w} ${h}" width="${w}" height="${h}" role="img" aria-label="${esc(title)}"><title>${esc(title)}</title><rect width="${w}" height="${h}" fill="${t.bg}"/>${body}</svg>\n`;

const diagrams = {
  architecture(t) {
    const { text, rect, box, arrow } = kit(t);
    let o = '';
    // Who comes in.
    o += box(40, 16, 240, 74, 'Developers', ['git push, and moor']);
    o += box(310, 16, 240, 74, 'CI and scripts', ['the API, with a token']);
    o += box(580, 16, 240, 74, 'Visitors', ['a browser']);
    // The server.
    o += rect(20, 122, 820, 530, { fill: t.accentSoft, stroke: t.accent, r: 14 });
    o += text(40, 638, 'The moor server: one process', { size: 18, weight: 700, fill: t.accent });
    const cell = (x, y, title, lines) => box(x, y, 240, 96, title, lines);
    o += cell(40, 144, 'Git server', ['SSH and HTTPS', 'shows the build as it goes']);
    o += cell(310, 144, 'API and console', ['REST, streams, a WebSocket', 'pages rendered here']);
    o += cell(580, 144, 'Ingress', ['TLS, hostnames, gates', 'static files, proxy, waking']);
    o += cell(40, 268, 'Forges', ['webhooks, and looking', 'for changes; reports back']);
    o += cell(310, 268, 'Deployments', ['a queue per environment', 'build, release, rollout']);
    o += cell(580, 268, 'Toolchains', ['Go, Node.js, uv, Python', 'downloaded and checked']);
    o += cell(40, 392, 'Logs and metrics', ['health, notifications', 'telemetry to elsewhere']);
    o += cell(310, 392, 'Reconciler', ['what should run, and', 'what does: starts, stops']);
    o += cell(580, 392, 'Scheduler', ['cron, sleep, expiry', 'backups, tidying up']);
    o += box(40, 512, 780, 96, 'The store', ['One SQLite database for what is a record,', 'and plain files for everything else: repositories, builds, logs and apps\u2019 data.'], {});
    // In.
    o += arrow(120, 90, 120, 144, { label: 'git push', lx: 128, ly: 114, anchor: 'start' });
    o += arrow(250, 90, 350, 144, { label: 'moor', lx: 312, ly: 110, anchor: 'start' });
    o += arrow(470, 90, 470, 144);
    o += arrow(700, 90, 700, 144);
    // Within.
    o += arrow(280, 220, 310, 286);
    o += arrow(430, 240, 430, 268);
    o += arrow(280, 316, 310, 316);
    o += arrow(550, 316, 580, 316);
    o += arrow(430, 364, 430, 392);
    o += arrow(580, 440, 550, 440);
    // What it runs.
    o += box(40, 698, 370, 74, 'A small supervisor for each instance', ['outlives the server, and restarts what fails'], { fill: t.blueSoft, stroke: t.blue });
    o += box(40, 804, 370, 74, 'Your app', ['an ordinary process, on 127.0.0.1'], {});
    o += box(450, 698, 370, 74, 'Podman or Docker', ['only if you use containers'], { fill: t.blueSoft, stroke: t.blue });
    o += box(450, 804, 370, 74, 'Your app', ['in a container, on 127.0.0.1'], {});
    o += arrow(225, 652, 225, 698, { label: 'starts', lx: 233, ly: 680, anchor: 'start' });
    o += arrow(635, 652, 635, 698, { label: 'starts', lx: 643, ly: 680, anchor: 'start' });
    o += arrow(225, 772, 225, 804);
    o += arrow(635, 772, 635, 804);
    return svg(860, 896, o, t, 'Developers, scripts and visitors reach one server process. It starts apps through a small supervisor each, or through a container engine.');
  },

  rollout(t) {
    const { text, rect, arrow } = kit(t);
    let o = '';
    const x0 = 120;
    const bar = (y, x, w, label, fill, stroke, color = t.text) => rect(x, y, w, 50, { fill, stroke, r: 8 }) + text(x + w / 2, y + 32, label, { size: 16, weight: 600, anchor: 'middle', fill: color });
    o += text(20, 92, 'v7, old', { size: 17, weight: 650 });
    o += text(20, 172, 'v8, new', { size: 17, weight: 650 });
    o += bar(62, x0, 420, 'serving', t.accentSoft, t.accent, t.accent);
    o += bar(62, x0 + 420, 150, 'finishing', t.warnSoft, t.warn, t.warn);
    o += bar(142, x0 + 130, 290, 'starting: health checks', t.blueSoft, t.blue, t.blue);
    o += bar(142, x0 + 420, 300, 'serving', t.accentSoft, t.accent, t.accent);
    // The moment traffic moves.
    const xm = x0 + 420;
    o += `<line x1="${xm}" y1="38" x2="${xm}" y2="214" stroke="${t.text}" stroke-width="2" stroke-dasharray="5 4"/>`;
    o += text(xm, 26, 'traffic moves, all at once', { size: 16, weight: 650, anchor: 'middle' });
    // Time.
    o += arrow(x0, 236, x0 + 720, 236, { color: t.muted });
    const step = (x, label) => `<line x1="${x}" y1="230" x2="${x}" y2="242" stroke="${t.muted}" stroke-width="1.5"/>` + text(x, 262, label, { size: 14.5, fill: t.muted, anchor: 'middle' });
    o += step(x0 + 130, 'new instances start');
    o += step(xm, 'healthy');
    o += step(xm + 150, 'old ones stop');
    o += text(20, 304, 'If v8 never becomes healthy, traffic never moves: v7 goes on serving, and', { size: 16, fill: t.muted });
    o += text(20, 326, 'the deployment fails. Nobody using the app sees anything.', { size: 16, fill: t.muted });
    return svg(860, 344, o, t, 'The old release serves until the new one is healthy. Traffic then moves at once, and the old release finishes what it was doing and stops.');
  },

  concepts(t) {
    const { text, rect, box, arrow } = kit(t);
    let o = '';
    o += box(16, 104, 124, 96, 'An app', ['tasks', 'a repository'], {});
    // Environments.
    o += rect(176, 16, 300, 282, { fill: t.panel, stroke: t.line, r: 12 });
    o += text(192, 44, 'Its environments', { weight: 650, fill: t.muted, size: 15 });
    o += box(192, 58, 268, 72, 'production', ['tasks.apps.example.com'], { fill: t.accentSoft, stroke: t.accent, titleFill: t.accent });
    o += box(192, 138, 268, 68, 'staging', ['tasks--staging.apps.example.com']);
    o += box(192, 214, 268, 68, 'a preview of a branch', ['tasks--new-title.apps.example.com']);
    o += arrow(140, 152, 176, 152);
    // A release.
    o += rect(524, 16, 320, 196, { fill: t.panel, stroke: t.line, r: 12 });
    o += text(540, 44, 'Its current release: v8', { weight: 650, fill: t.muted, size: 15 });
    o += box(540, 58, 140, 138, 'A build', ['fixed once', 'it is made,', 'and kept for', 'rollbacks'], { fill: t.blueSoft, stroke: t.blue });
    o += box(688, 58, 140, 138, 'Variables', ['and secrets,', 'as they were', 'then'], { fill: t.blueSoft, stroke: t.blue });
    o += arrow(460, 94, 524, 94, { label: 'runs', lx: 492, ly: 86 });
    // Instances.
    o += box(524, 240, 152, 72, 'web.1', ['an instance']);
    o += box(692, 240, 152, 72, 'worker.1', ['an instance']);
    o += arrow(610, 212, 610, 240);
    o += arrow(758, 212, 758, 240);
    // What stays.
    o += box(192, 322, 268, 72, 'Its own data directory', ['survives deploys and rollbacks']);
    o += arrow(326, 298, 326, 322);
    o += text(524, 344, 'A deploy, a change of variables, a rollback', { size: 15, fill: t.muted });
    o += text(524, 366, 'or a promotion each makes a new release,', { size: 15, fill: t.muted });
    o += text(524, 388, 'so that the list is the history.', { size: 15, fill: t.muted });
    return svg(860, 410, o, t, 'An app has environments. Each has an address, variables and a data directory, and a current release. A release is a build and a set of variables, and instances run it.');
  },
};

for (const [name, draw] of Object.entries(diagrams)) {
  for (const [theme, colours] of Object.entries(THEMES)) writeFileSync(join(IMAGES, `${name}-${theme}.svg`), draw(colours));
}

// The picture link previews show: social sites want PNG or JPEG.
const logo = readFileSync(join(ROOT, 'docs/assets/moorage-logo-dark.svg')).toString('base64');
const shot = join(IMAGES, 'overview-dark.webp');
const page = `<!doctype html><meta charset="utf-8"><body style="margin:0;width:1200px;height:630px;overflow:hidden;position:relative;background:linear-gradient(160deg,#08264c,#0a3a5c 60%,#0b5a63);font-family:system-ui,-apple-system,'Segoe UI',Roboto,sans-serif;color:#fff">
<img src="data:image/svg+xml;base64,${logo}" style="position:absolute;left:64px;top:64px;width:300px">
<div style="position:absolute;left:64px;top:150px;width:490px;font-size:56px;line-height:1.08;font-weight:750;letter-spacing:-.02em">Push code.<br>Get a URL.<br><span style="color:#7fd8cf">Own the platform.</span></div>
<div style="position:absolute;left:64px;top:360px;width:470px;font-size:26px;line-height:1.35;color:#c9dbe6">One binary turns your laptop or a server into your own app platform.</div>
<div style="position:absolute;left:64px;bottom:58px;font-size:22px;color:#7fd8cf;font-weight:600">Linux, macOS and Windows · Free and open source</div>
<img src="data:image/webp;base64,${readFileSync(shot).toString('base64')}" style="position:absolute;left:590px;top:70px;width:900px;border-radius:12px;box-shadow:0 20px 60px rgba(0,0,0,.45)">
</body>`;
const html = join(IMAGES, '.og.html');
writeFileSync(html, page);
const b = await launch({ width: 1200, height: 630, scale: 1 });
await b.goto(`file://${html}`, 500);
await sleep(300);
await b.capture(join(ROOT, 'docs/site/assets/og-image.jpg'), { format: 'jpeg', quality: 86 });
await b.close();
const { rmSync } = await import('node:fs');
rmSync(html, { force: true });
console.log('Drew the diagrams and the link preview.');
process.exit(0);
