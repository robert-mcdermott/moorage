// SPDX-License-Identifier: Apache-2.0
// Takes the documentation's screenshots, in the light and dark themes, as docs/images/<scene>-<theme>.webp. The guides
// show the light ones; the website (docs/site/build.mjs copies them) shows the one matching its theme.
//
// The pictures are of a real server, and nothing in them is drawn: the script builds moor, starts a server of its own
// in a folder of its own, claims it, deploys the examples, makes some requests for some minutes so that there are
// charts and logs to show, and opens each scene in headless Chrome. All it changes is what would read oddly: the
// server's temporary folder and port, and the addresses of a little receiver for its webhooks and telemetry, are
// replaced in the page as it is captured by what a reader would see.
//
//   node docs/capture/capture.mjs [scene …] [--theme light|dark|both] [--reuse DIR] [--traffic MINUTES]
//
// --reuse DIR keeps the demo in DIR, and uses it again on later runs, which is quicker while working on scenes.
// Without it, the demo is made in a temporary folder and removed afterwards. The apps are stopped either way.
//
// Needs Go, Git, and Chrome, Chromium, Edge or Brave (CHROME=path). It downloads the toolchains the examples need, as
// any server does, and contacts nothing else.

import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { createServer, request as httpRequest } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { launch, sleep } from './cdp.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const IMAGES = join(ROOT, 'docs/images');

const args = process.argv.slice(2);
const option = (name) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 ? args[i + 1] : null;
};
const themes = { light: ['light'], dark: ['dark'], both: ['light', 'dark'] }[option('theme') ?? 'both'];
const reuse = option('reuse');
const trafficMinutes = Number(option('traffic') ?? 15);
const wanted = args.filter((a, i) => !a.startsWith('--') && !['--theme', '--reuse', '--traffic'].includes(args[i - 1]));

const say = (text) => console.log(`${new Date().toISOString().slice(11, 19)} ${text}`);

// --- A folder, ports and a server of its own ------------------------------------------------------------------------

const DIR = realpathSync(reuse ? (mkdirSync(reuse, { recursive: true }), reuse) : mkdtempSync(join(tmpdir(), 'moorage-demo-')));
const STATE = join(DIR, 'state.json');
const state = existsSync(STATE) ? JSON.parse(readFileSync(STATE, 'utf8')) : {};
const remember = () => writeFileSync(STATE, JSON.stringify(state, null, 1), { mode: 0o600 });

const freePort = () => new Promise((done) => {
  const probe = createNetServer().listen(0, '127.0.0.1', () => { const { port } = probe.address(); probe.close(() => done(port)); });
});
state.port ??= await freePort();
state.receiver ??= await freePort();
remember();

const PORT = state.port;
const CONSOLE = `http://moorage.localhost:${PORT}`;
const MOOR = join(DIR, 'moor');
const CONFIG = join(DIR, 'server.toml');
const DATA = join(DIR, 'data');
const environment = () => ({ ...process.env, MOORAGE_CONFIG: CONFIG, MOOR_CONFIG: join(DIR, 'client.toml'), MOOR_SERVER: CONSOLE,
  ...(state.token ? { MOOR_TOKEN: state.token } : {}) });

// Runs a command, and says what it said if it fails.
function run(command, argv, { cwd = DIR, input = null, may = false, env = environment() } = {}) {
  return new Promise((done, fail) => {
    const child = spawn(command, argv, { cwd, env, stdio: ['pipe', 'pipe', 'pipe'] });
    let out = '';
    child.stdout.on('data', (d) => { out += d; });
    child.stderr.on('data', (d) => { out += d; });
    if (input !== null) child.stdin.write(input);
    child.stdin.end();
    child.on('exit', (code) => {
      if (code && !may) fail(new Error(`${command} ${argv.join(' ')} failed (${code}):\n${out}`));
      else done(out);
    });
  });
}
const moor = (...argv) => run(MOOR, argv);

let server = null;
async function startServer() {
  server = spawn(MOOR, ['server', 'run'], { cwd: DIR, env: environment(), stdio: ['ignore', 'ignore', 'ignore'] });
  for (let i = 0; i < 300; i += 1) {
    try {
      if ((await fetch(`http://127.0.0.1:${PORT}/.moorage/health`, { signal: AbortSignal.timeout(1000) })).ok) return;
    } catch { /* not up yet */ }
    await sleep(200);
  }
  throw new Error('The server did not start.');
}

// Stops the server and the apps however the run ends: killing the server alone would leave them running.
let stopped = false;
async function stopEverything() {
  if (stopped) return;
  stopped = true;
  receiver.close();
  await run(MOOR, ['server', 'stop', '--all'], { may: true });
  if (server && server.exitCode === null) server.kill();
}

// What a server's webhooks and telemetry go to, so that they have somewhere to succeed.
const receiver = createServer((req, res) => {
  req.resume();
  req.on('end', () => { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end('{}'); });
}).listen(state.receiver, '127.0.0.1');

// --- A browser, and what it shows -----------------------------------------------------------------------------------

// Most pictures are of a page's content, without the console's menu, so that they can be read at the width of a column
// of text: 1000 CSS pixels beside the menu's 240. A few show the whole window.
const SIDEBAR = 240;
const WIDTH = 1240;
const HEIGHT = 900;
const WIDE = 1440;
const SCALE = 1.6;
const TALLEST = 800;
const b = await launch({ width: WIDTH, height: HEIGHT, scale: SCALE });

// What the page says that is only so because this is a temporary server.
const rewrites = [
  [DATA, '/home/alex/.moorage/data'],
  [CONFIG, '/home/alex/.moorage/server.toml'],
  [DIR, '/home/alex/.moorage'],
  ['+modified', ''],
  [`localhost:${PORT}`, 'localhost:8080'],
  [`127.0.0.1:${PORT}`, '127.0.0.1:8080'],
  [`http://127.0.0.1:${state.receiver}/hooks/deploys`, 'https://chat.example.com/hooks/deploys'],
  [`http://127.0.0.1:${state.receiver}`, 'https://otel.example.com:4318'],
  [`127.0.0.1:${state.receiver}`, 'chat.example.com'],
];
const REWRITE = `(() => {
  const swaps = ${JSON.stringify(rewrites)};
  const swap = (text) => swaps.reduce((s, [a, b]) => s.split(a).join(b), text);
  const walk = document.createTreeWalker(document.documentElement, NodeFilter.SHOW_TEXT);
  while (walk.nextNode()) {
    const next = swap(walk.currentNode.nodeValue);
    if (next !== walk.currentNode.nodeValue) walk.currentNode.nodeValue = next;
  }
  for (const input of document.querySelectorAll('input, textarea')) {
    const next = swap(input.value);
    if (next !== input.value) input.value = next;
  }
})()`;

async function shoot(name, theme, { clip = null } = {}) {
  await b.eval(REWRITE);
  await b.capture(join(IMAGES, `${name}-${theme}.webp`), { clip });
}

// Fills in a form on the page, and sends it. `fields` is by name.
async function submit(fields, formSelector = 'main form, form') {
  await b.eval(`(() => {
    const fields = ${JSON.stringify(fields)};
    const names = Object.keys(fields);
    const form = [...document.querySelectorAll(${JSON.stringify(formSelector)})].find((f) => names.every((n) => f.elements[n]));
    if (!form) throw new Error('No form with ' + names.join(', '));
    for (const [name, value] of Object.entries(fields)) {
      const el = form.elements[name];
      if (el.length !== undefined && !el.tagName) { for (const o of el) o.checked = o.value === value; }
      else el.value = value;
    }
    form.requestSubmit();
  })()`);
  await sleep(1200);
  for (let i = 0; i < 100; i += 1) {
    if (await b.eval(`document.readyState === 'complete'`).catch(() => false)) break;
    await sleep(100);
  }
  await sleep(300);
}

const signIn = async (email, password) => {
  await b.goto(`${CONSOLE}/login`);
  if (await b.eval(`location.pathname === '/login'`)) await submit({ email, password });
};

// A token for whoever is signed in, from the console, as a person would make one.
async function makeToken(name) {
  await b.goto(`${CONSOLE}/account/tokens`);
  await submit({ name, scope: 'admin' });
  const token = await b.eval(`[...document.querySelectorAll('.secret-once code')].map((c) => c.textContent.trim()).find((t) => t.startsWith('moor_')) ?? ''`);
  if (!token) throw new Error('No token was shown.');
  return token;
}

// A user with a password and a token of their own, for what they deploy.
async function person(email, name) {
  const out = await moor('server', 'users', 'create', email, '--name', name);
  const link = /https?:\/\/\S+/.exec(out)?.[0];
  if (!link) throw new Error(`No link for ${email}: ${out}`);
  const password = randomBytes(18).toString('base64url');
  await b.clearCookies();
  await b.goto(link);
  await submit({ password, confirm: password });
  const token = await makeToken('laptop');
  await b.clearCookies();
  return { email, password, token };
}

// --- The demo -------------------------------------------------------------------------------------------------------

// Asks an app for pages, by the hostname it answers to.
function hit(host, path) {
  return new Promise((done) => {
    const req = httpRequest({ host: '127.0.0.1', port: PORT, path, headers: { Host: `${host}.localhost:${PORT}` }, timeout: 5000 }, (res) => {
      res.resume();
      res.on('end', done);
    });
    req.on('error', done);
    req.on('timeout', () => { req.destroy(); done(); });
    req.end();
  });
}

const VISITS = [
  { host: 'tasks', rate: 70, paths: ['/', '/', '/', '/healthz', '/api/tasks', '/api/tasks', '/missing'] },
  { host: 'links', rate: 45, paths: ['/', '/', '/healthz', '/nope'] },
  { host: 'poll', rate: 110, paths: ['/', '/', '/api/results', '/healthz', '/assets/none.js'] },
  { host: 'handbook', rate: 25, paths: ['/', '/about', '/guide/', '/docs/old', '/missing'] },
  { host: 'tasks--staging', rate: 8, paths: ['/', '/healthz'] },
];

// Requests a minute, rising and falling, as real ones do.
async function traffic(minutes, marks = {}) {
  const start = Date.now();
  const end = start + minutes * 60000;
  const fired = new Set();
  while (Date.now() < end) {
    const t = (Date.now() - start) / 60000;
    for (const [at, action] of Object.entries(marks)) {
      if (t >= Number(at) && !fired.has(at)) {
        fired.add(at);
        action().catch((e) => say(`During traffic: ${e.message}`));
      }
    }
    const wave = 0.65 + 0.35 * Math.sin(t * 1.3);
    for (const { host, rate, paths } of VISITS) {
      const n = Math.round((rate * wave) / 12 + Math.random());
      for (let i = 0; i < n; i += 1) hit(host, paths[Math.floor(Math.random() * paths.length)]);
    }
    await sleep(5000);
  }
}

// A copy of an example to change without changing the example.
function source(example) {
  const to = join(DIR, 'src', example);
  rmSync(to, { recursive: true, force: true });
  cpSync(join(ROOT, 'examples', example), to, { recursive: true, filter: (from) => !from.includes('node_modules') });
  return to;
}
const change = (file, from, to) => {
  const text = readFileSync(file, 'utf8');
  if (!text.includes(from)) throw new Error(`${file} has no "${from}"`);
  writeFileSync(file, text.replace(from, to));
};
const git = (cwd, ...argv) => run('git', ['-c', 'user.name=Alex Morgan', '-c', 'user.email=alex@example.com', ...argv], { cwd });
// As someone else, for who it says deployed it.
const as = (who, cwd, ...argv) => run(MOOR, argv, { cwd, env: { ...environment(), MOOR_TOKEN: who.token } });

async function seed() {
  say('Building moor');
  await run('go', ['build', '-o', MOOR, './cmd/moor'], { cwd: ROOT });
  await moor('server', 'init', '--preset', 'local', '--config', CONFIG, '--data-dir', DATA);
  const config = readFileSync(CONFIG, 'utf8')
    .replace(/^http\s*=.*$/m, `http  = "127.0.0.1:${PORT}"`)
    .replace(/^ssh\s*=.*$/m, 'ssh   = ""')
    // This computer's disk may be nearly full, which would be news on every page.
    .replace(/^\[notifications\]$/m, '[notifications]\nallow = ["127.0.0.1"]\ndisk_percent = 99.9');
  if (!config.includes('allow = ["127.0.0.1"]')) throw new Error('server.toml has no [notifications] for the receiver.');
  writeFileSync(CONFIG, config);
  await startServer();

  say('Claiming the server');
  const code = readFileSync(join(DATA, 'keys/claim-code'), 'utf8').trim();
  await b.size(640, 680);
  await b.goto(`${CONSOLE}/setup?code=${code}`);
  for (const theme of themes) {
    await b.theme(theme);
    await sleep(200);
    await shoot('setup', theme);
  }
  await b.size(WIDTH, HEIGHT);
  state.alex = { email: 'alex@example.com', password: randomBytes(18).toString('base64url') };
  remember();
  await submit({ email: state.alex.email, name: 'Alex Morgan', password: state.alex.password, confirm: state.alex.password });
  for (const theme of themes) {
    await b.theme(theme);
    await b.goto(`${CONSOLE}/welcome`);
    await shoot('welcome', theme, { clip: await contentClip() });
  }
  state.token = await makeToken('laptop');
  remember();

  say('Adding people and a project');
  state.sam = await person('sam@example.com', 'Sam Okafor');
  state.priya = await person('priya@example.com', 'Priya Nair');
  remember();
  await signIn(state.alex.email, state.alex.password);
  await moor('projects', 'create', 'storefront');
  await moor('projects', 'add-member', 'storefront', state.sam.email, '--role', 'deployer');
  await moor('projects', 'add-member', 'storefront', state.priya.email, '--role', 'viewer');

  say('Deploying the examples');
  const tasks = source('python-fastapi');
  const cron = '\n[[cron]]\nname = "tidy"\nschedule = "*/10 * * * *"\ncommand = "python -m tasks.migrate"\n';
  writeFileSync(join(tasks, 'moorage.toml'), readFileSync(join(tasks, 'moorage.toml'), 'utf8') + cron);
  await git(tasks, 'init', '-q', '-b', 'main');
  await git(tasks, 'add', '.');
  await git(tasks, 'commit', '-qm', 'Task list with a web page');
  await moor('apps', 'create', 'tasks', '--project', 'storefront');
  await run(MOOR, ['git', 'remote', '--http'], { cwd: tasks });
  await git(tasks, 'push', '-q', 'moorage', 'main');

  const links = source('go-stdlib');
  await run(MOOR, ['deploy'], { cwd: links });
  const poll = source('node-express');
  await moor('apps', 'create', 'poll', '--project', 'storefront');
  await as(state.sam, poll, 'deploy');
  await moor('scale', '--app', 'poll', 'web=2');
  await run(MOOR, ['deploy'], { cwd: source('static-site') });

  say('Variables, environments, a preview, gates');
  await moor('env', 'set', '--app', 'tasks', 'TASKS_TITLE=Team tasks', 'LOG_LEVEL=info');
  await run(MOOR, ['env', 'set', '--app', 'tasks', '--secret', '--production-only', 'MAIL_API_KEY'], { input: `${randomBytes(16).toString('hex')}\n` });
  await run(MOOR, ['env', 'set', '--app', 'tasks', '--secret', 'SESSION_SECRET'], { input: `${randomBytes(24).toString('hex')}\n` });
  await moor('environments', 'create', 'staging', '--app', 'tasks', '--branch', 'staging');
  await git(tasks, 'checkout', '-qb', 'staging');
  await git(tasks, 'push', '-q', 'moorage', 'staging');
  await git(tasks, 'checkout', '-q', 'main');
  change(join(tasks, 'moorage.toml'), 'TASKS_TITLE = "Tasks"', 'TASKS_TITLE = "Tasks, with due dates"');
  await git(tasks, 'checkout', '-qb', 'feature/due-dates');
  await git(tasks, 'commit', '-qam', 'Say so in the title');
  await git(tasks, 'push', '-q', 'moorage', 'feature/due-dates');
  await git(tasks, 'checkout', '-q', 'main');
  await run(MOOR, ['access', 'set', 'password', '--app', 'tasks', '--env', 'staging'], { input: `${randomBytes(12).toString('hex')}\n` });
  await moor('share', '--app', 'tasks', '--env', 'staging', '--expires', '3d');
  await moor('domains', 'add', 'tasks.example.com', '--app', 'tasks');
  await run(MOOR, ['files', 'mkdir', '--app', 'tasks', 'exports'], { may: true });
  await moor('alerts', 'add', 'error-rate', '5', '--for', '5m', '--app', 'tasks');
  await moor('alerts', 'add', 'latency-p95', '800', '--for', '10m', '--app', 'tasks');

  say('Notifications, telemetry and a backup');
  await moor('notifications', 'channels', 'add', 'deploys', '--kind', 'webhook', '--url', `http://127.0.0.1:${state.receiver}/hooks/deploys`);
  await moor('notifications', 'rules', 'add', '--channel', 'deploys', '--project', 'storefront');
  await moor('notifications', 'rules', 'add', '--channel', 'deploys');
  await moor('server', 'telemetry', 'add', 'collector', '--kind', 'otlp', '--address', `http://127.0.0.1:${state.receiver}`, '--logs', '--metrics');
  await moor('server', 'backup', '--to', join(DATA, 'backups'));
  await b.goto(`${CONSOLE}/settings/backups`);
  await submit({ kind: 'path', path: join(DATA, 'backups'), schedule: '@daily', keep: '14' }).catch((e) => say(`Backups: ${e.message}`));

  say(`Making requests for ${trafficMinutes} minutes`);
  await traffic(trafficMinutes, {
    [trafficMinutes * 0.35]: () => run(MOOR, ['cron', 'run', 'tidy', '--app', 'tasks'], { may: true }),
    [trafficMinutes * 0.5]: async () => {
      change(join(tasks, 'moorage.toml'), 'TASKS_TITLE = "Tasks"', 'TASKS_TITLE = "Tasks, with owners"');
      await git(tasks, 'commit', '-qam', 'A title that says what it is');
      await git(tasks, 'push', '-q', 'moorage', 'main');
      await as(state.sam, poll, 'deploy');
    },
    [trafficMinutes * 0.7]: async () => {
      // One that fails: the release before it keeps serving.
      change(join(links, 'main.go'), 'package main', 'package main\n\nvar oops int = "a mistake in the code"');
      await run(MOOR, ['deploy'], { cwd: links, may: true });
    },
  });
  await moor('run', '--app', 'tasks', '--', 'python', '-m', 'tasks.migrate');
  await moor('run', '--app', 'tasks', '--', 'python', '--version');

  const deployments = async (app) => JSON.parse(await moor('deployments', '--app', app, '--json', '--limit', '5'));
  const taskList = await deployments('tasks');
  const linkList = await deployments('links');
  const list = (d) => d.deployments ?? d.items ?? d;
  state.deployment = list(taskList).find((d) => (d.status ?? d.state) === 'live' || (d.status ?? d.state) === 'succeeded')?.id;
  state.failed = list(linkList).find((d) => (d.status ?? d.state) === 'failed')?.id;
  state.seeded = true;
  remember();
  say(`Seeded: ${JSON.stringify({ deployment: state.deployment, failed: state.failed })}`);
}

// --- Scenes ---------------------------------------------------------------------------------------------------------
// Each opens a page, and may return what to capture instead of the window.

const open = (path, settle = 700) => b.goto(`${CONSOLE}${path}`, settle);

// The page's content, beside the menu, as far down as it goes or as far as a picture should.
async function contentClip(tallest = TALLEST) {
  const bottom = await b.eval(`Math.ceil(Math.max(...[...document.querySelector('main').children].map((c) => c.getBoundingClientRect().bottom)) + scrollY)`);
  return { x: SIDEBAR, y: 0, width: WIDTH - SIDEBAR, height: Math.min(bottom + 32, tallest) };
}

const scenes = {
  async overview() { await open('/apps/tasks', 1200); return { whole: true }; },
  async 'sign-in'() { return { signedOut: true, path: '/login' }; },
  async apps() { await open('/', 1500); },
  async 'new-app'() { await open('/new'); },
  async app() { await open('/apps/tasks', 1200); },
  async 'app-deploy'() { await open('/apps/tasks/deploy'); },
  async deployment() { await open(`/deployments/${state.deployment}`, 1200); },
  async 'deployment-failed'() { await open(`/deployments/${state.failed}`, 1200); },
  async 'app-failed'() { await open('/apps/links', 1000); },
  async logs() { await open('/apps/tasks/logs', 2000); },
  async metrics() { await open('/apps/poll/metrics?range=live', 1500); return { tallest: 900 }; },
  async environments() { await open('/apps/tasks/environments'); },
  async previews() { await open('/previews'); },
  async variables() { await open('/apps/tasks/variables'); },
  async 'variables-edit'() { await open('/apps/tasks/variables/edit'); },
  async cron() { await open('/apps/tasks/cron'); },
  async 'console-tab'() { await open('/apps/tasks/console'); },
  async files() { await open('/apps/tasks/files'); },
  async access() { await open('/apps/tasks/access?env=staging'); return { tallest: 900 }; },
  async domains() { await open('/apps/tasks/access'); return { tallest: 560 }; },
  async 'app-settings'() { await open('/apps/tasks/settings'); return { tallest: 900 }; },
  async projects() { await open('/projects'); },
  async project() { await open('/projects/storefront'); },
  async tokens() { await open('/account/tokens'); },
  async 'ssh-keys'() { await open('/account/keys'); },
  async 'two-factor'() { await open('/account/security'); },
  async inbox() { await open('/notifications'); },
  async 'settings-notifications'() { await open('/settings/notifications'); return { tallest: 900 }; },
  async telemetry() { await open('/settings/telemetry'); },
  async backups() { await open('/settings/backups'); },
  async users() { await open('/settings/users'); },
  async audit() { await open('/settings/audit'); },
  async system() { await open('/settings/system', 1500); },
  async settings() { await open('/settings'); },
  async activity() { await open('/activity', 1000); },
  async palette() {
    await open('/', 1200);
    await b.send('Input.dispatchKeyEvent', { type: 'keyDown', key: '/', text: '/', code: 'Slash', windowsVirtualKeyCode: 191 });
    await b.send('Input.dispatchKeyEvent', { type: 'keyUp', key: '/', code: 'Slash', windowsVirtualKeyCode: 191 });
    await sleep(300);
    await b.send('Input.insertText', { text: 'tasks' });
    await sleep(400);
    return { tallest: 520 };
  },
  async phone() { return { phone: true, path: '/apps/tasks' }; },
  async 'phone-logs'() { return { phone: true, path: '/apps/tasks/logs' }; },
};

async function capture() {
  const names = wanted.length ? wanted : Object.keys(scenes);
  for (const name of names) if (!scenes[name] && !['setup', 'welcome'].includes(name)) throw new Error(`No scene ${name}`);
  await signIn(state.alex.email, state.alex.password);
  for (const name of names) {
    if (!scenes[name]) continue;
    for (const theme of themes) {
      await b.theme(theme);
      const asked = (await scenes[name]()) ?? {};
      if (asked.signedOut) {
        const kept = await b.cookies();
        await b.clearCookies();
        await b.size(640, 680);
        await open(asked.path);
        await shoot(name, theme);
        await b.size(WIDTH, HEIGHT);
        await b.setCookies(kept.map(({ name: n, value, domain, path, secure, httpOnly, sameSite, expires }) => ({ name: n, value, domain, path, secure, httpOnly, sameSite, expires })));
      } else if (asked.phone) {
        await b.size(390, 780, true, 2);
        await open(asked.path, 1500);
        await shoot(name, theme);
        await b.size(WIDTH, HEIGHT);
      } else if (asked.whole) {
        await b.size(WIDE, HEIGHT, false, 1.2);
        await sleep(500);
        await shoot(name, theme);
        await b.size(WIDTH, HEIGHT);
      } else {
        await shoot(name, theme, { clip: await contentClip(asked.tallest) });
      }
    }
    say(`Captured ${name}`);
  }
}

// --- The run --------------------------------------------------------------------------------------------------------

let failure = null;
try {
  mkdirSync(IMAGES, { recursive: true });
  if (state.seeded) {
    await run('go', ['build', '-o', MOOR, './cmd/moor'], { cwd: ROOT });
    await startServer();
    // Some recent requests, for what the pages say of the last while.
    await traffic(Math.min(trafficMinutes, Number(option('traffic') ?? 1)));
  } else {
    await seed();
  }
  await capture();
} catch (e) {
  failure = e;
}
await b.close();
await stopEverything();
if (!reuse) {
  try { rmSync(DIR, { recursive: true, force: true, maxRetries: 5 }); } catch { /* a temporary folder */ }
}
if (failure) {
  console.error(failure);
  process.exit(1);
}
say('Done');
process.exit(0);
