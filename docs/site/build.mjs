// SPDX-License-Identifier: Apache-2.0
// Builds the Moorage website from the guides in docs/ (the same Markdown GitHub shows) and a home page.
//
//   node docs/site/build.mjs [FOLDER]      build into FOLDER (default: site/ in the repository, which git ignores)
//   node docs/site/build.mjs --publish     build into ../moorage-site, a checkout of the gh-pages branch
//
// Committing that checkout publishes the site, so the build writes into it only with --publish. Each page of the
// documentation is a whole guide, or some of a guide's sections (DOCS below), so that the guides and the site never
// say different things. A link between guides becomes a link to the page that holds its anchor, and a link to any other
// file in the repository goes to GitHub. A paragraph that is only a picture, ![alt](images/<name>-light.webp "caption"),
// becomes a figure with both themes' pictures, of which the page shows the one that matches its theme. The API
// reference is made from docs/openapi.json. The build then checks every local link, anchor and image, and fails if one
// is broken. It needs nothing beyond Node 22.

import { copyFileSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, posix, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, '../..');
const args = process.argv.slice(2);
const publish = args.includes('--publish');
const folder = args.find((a) => !a.startsWith('--'));
const out = resolve(folder ?? (publish ? join(repo, '..', 'moorage-site') : join(repo, 'site')));

// A checkout of gh-pages publishes what is committed in it, so nothing writes there by accident.
const isPagesCheckout = (() => {
  try {
    const dotGit = join(out, '.git');
    const pointer = statSync(dotGit).isDirectory() ? dotGit : readFileSync(dotGit, 'utf8').replace(/^gitdir:\s*/, '').trim();
    return /ref: refs\/heads\/gh-pages\s*$/.test(readFileSync(join(pointer, 'HEAD'), 'utf8'));
  } catch {
    return false;
  }
})();
if (isPagesCheckout && !publish) {
  console.error(`${out} is a checkout of gh-pages, which publishes the website: add --publish to build into it, or give another folder.`);
  process.exit(2);
}
if (publish && !isPagesCheckout) {
  console.error(`${out} is not a checkout of gh-pages: see docs/site/README.md for how to make one.`);
  process.exit(2);
}

const SITE = 'https://robert-mcdermott.github.io/moorage/';
const GITHUB = 'https://github.com/robert-mcdermott/moorage';
const RAW = 'https://raw.githubusercontent.com/robert-mcdermott/moorage/main';
const VERSION = /var Version = "([^"]+)"/.exec(readFileSync(join(repo, 'internal/buildinfo/buildinfo.go'), 'utf8'))[1];
const TAGLINE = 'Push code. Get a URL. Own the platform.';

// The documentation, in reading order: the menu. Each page is a file of the repository, or some of its ## sections
// (`intro` also takes the text before the first of them). A page of one section takes that section's heading as its own.
const DOCS = [
  { group: 'Start', pages: [
    { slug: 'index', title: 'Documentation', file: 'docs/README.md', lede: 'What Moorage is, where everything is, and the commands you will use most.' },
    { slug: 'getting-started', title: 'Getting started', file: 'docs/getting-started.md', lede: 'From nothing to an app with a URL, a preview and a rollback, on your own computer, in about ten minutes.' },
    { slug: 'concepts', title: 'Concepts', file: 'docs/concepts.md', lede: 'Apps, environments, builds, releases, instances, gates, projects and roles: the words everything else uses.' },
    { slug: 'examples', title: 'Examples', file: 'examples/README.md', lede: 'Complete apps, small but realistic, each ready to deploy.' },
  ] },
  { group: 'Deploy your apps', pages: [
    { slug: 'git', title: 'With git push', file: 'docs/git.md', lede: 'Push to a repository on the server: the push shows the build, and fails if the deploy does.' },
    { slug: 'forges', title: 'From GitHub, GitLab or Gitea', file: 'docs/forges.md', lede: 'Connect a repository: a push deploys, and each pull request gets a preview.' },
    { slug: 'go', title: 'Go', file: 'docs/go.md', lede: 'Built from go.mod, with the Go version it asks for.' },
    { slug: 'python', title: 'Python', file: 'docs/python.md', lede: 'Built with uv, from pyproject.toml and uv.lock.' },
    { slug: 'node', title: 'Node.js', file: 'docs/node.md', lede: 'Built with npm, with the Node.js version it asks for.' },
    { slug: 'static', title: 'Static sites', file: 'docs/static.md', lede: 'Served by Moorage itself, with clean URLs, headers and redirects, and no process of its own.' },
    { slug: 'containers', title: 'In containers', file: 'docs/containers.md', lede: 'A Containerfile, or an image from a registry, under Podman or Docker.' },
    { slug: 'other', title: 'Anything else', file: 'docs/other.md', lede: 'Your own build, a script, or an app that is already built.' },
  ] },
  { group: 'Run your apps', pages: [
    { slug: 'console', title: 'The web console', file: 'docs/console.md', lede: 'Everything about your apps, live, in a browser.' },
    { slug: 'releases', title: 'Releases, rollbacks and scaling', file: 'docs/releases.md', lede: 'How a release takes over without downtime, how to go back in a moment, and how many instances run.' },
    { slug: 'variables', title: 'Variables and secrets', file: 'docs/variables.md', lede: 'What an app runs with, and what is sealed.' },
    { slug: 'previews', title: 'Previews and environments', file: 'docs/previews.md', lede: 'A place of its own for every branch, staging, promoting, locks and sleeping.' },
    { slug: 'cron', title: 'Cron jobs and one-off commands', file: 'docs/cron.md', lede: 'What runs on a schedule, and what runs once, in the current release.' },
    { slug: 'interactive', title: 'Getting into an app', file: 'docs/interactive.md', lede: 'A terminal, a console in the browser, and the data directory.' },
    { slug: 'logs', title: 'Logs', file: 'docs/logs.md', lede: 'What apps say, what they were asked, and what happened, followed and searched.' },
    { slug: 'metrics', title: 'Metrics', file: 'docs/metrics.md', lede: 'Requests, response times, CPU and memory, in the console and for Prometheus.' },
    { slug: 'notifications', title: 'Notifications', file: 'docs/notifications.md', lede: 'Told when something goes wrong: in the console, by webhook, in chat and by email.' },
    { slug: 'access', title: 'Who can reach an app', file: 'docs/access.md', lede: 'Passwords, signing in, share links and lists of addresses.' },
    { slug: 'domains', title: 'Your own domains, and paths', file: 'docs/domains.md', lede: 'Hostnames of your own, several apps on one hostname, and servers with no wildcard DNS.' },
  ] },
  { group: 'Run a server', pages: [
    { slug: 'install', title: 'Installing Moorage', file: 'docs/install.md', intro: true, sections: ['1. Get `moor`', '2. Configure the server', '3. Run it as a service', '4. Claim it', '5. Deploy'], lede: 'Get moor, configure the server, run it as a service, and claim it.' },
    { slug: 'sizing', title: 'How big a machine', file: 'docs/install.md', sections: ['How big a machine'], lede: 'What a server needs, and how much it holds.' },
    { slug: 'upgrading', title: 'Upgrading and uninstalling', file: 'docs/install.md', sections: ['Back up', 'Upgrading', 'Uninstalling'], lede: 'Moving to a new release and back again, and taking Moorage away.' },
    { slug: 'configuration', title: 'Server configuration', file: 'docs/configuration.md', lede: 'Every setting of server.toml, with its default.' },
    { slug: 'administration', title: 'Running a server', file: 'docs/administration.md', lede: 'What to look at, what to set up in the first week, and what to do as it grows.' },
    { slug: 'tls', title: 'TLS and trusting the server', file: 'docs/tls.md', lede: 'The certificates, and moor trust.' },
    { slug: 'proxy', title: 'Behind a TLS proxy', file: 'docs/proxy.md', lede: 'nginx or anything else in front of Moorage.' },
    { slug: 'accounts', title: 'Accounts and access', file: 'docs/accounts.md', lede: 'Claiming a server, signing in, users, projects and roles, API tokens and SSH keys.' },
    { slug: 'sign-in', title: 'How people sign in', file: 'docs/sign-in.md', lede: 'Single sign-on with your identity provider, two-factor sign-in, and signing in through a proxy.' },
    { slug: 'isolation', title: 'How apps are kept apart', file: 'docs/isolation.md', lede: 'A user of its own for each app, limits and a sandbox.' },
    { slug: 'backups', title: 'Backups', file: 'docs/backups.md', lede: 'On demand and on a schedule, to a directory or a bucket, and restoring them.' },
    { slug: 'telemetry', title: 'Sending telemetry elsewhere', file: 'docs/telemetry.md', lede: 'Logs to syslog, OTLP or HTTP, metrics over OTLP, and thresholds.' },
    { slug: 'troubleshooting', title: 'When something is wrong', file: 'docs/troubleshooting.md', lede: 'moor server doctor, stopping everything, and where the logs are.' },
  ] },
  { group: 'Reference', pages: [
    { slug: 'manifest', title: 'moorage.toml', file: 'docs/manifest.md', lede: 'Every setting of the app manifest.' },
    { slug: 'cli', title: 'Command line', file: 'docs/cli.md', lede: 'Every moor command and its flags.' },
    { slug: 'api', title: 'The API', file: 'docs/api.md', lede: 'Authentication, lists, errors, and the OpenAPI document.' },
    { slug: 'api-reference', title: 'API reference', generated: 'api', lede: 'Every route of the REST API, with what it takes, what it gives and what it needs.' },
    { slug: 'changelog', title: 'Changelog', file: 'CHANGELOG.md', omit: ['Unreleased'], lede: 'What changed in each release.' },
  ] },
  { group: 'How it works', pages: [
    { slug: 'architecture', title: 'Architecture', file: 'docs/architecture.md', lede: 'One process and its small helpers: how a request, a push and a restart go, and where everything is kept.' },
    { slug: 'security', title: 'Security', file: 'docs/security.md', lede: 'What Moorage protects, what it does not, and how to report a problem.' },
  ] },
];
const PAGES = DOCS.flatMap((g) => g.pages);

const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const attr = esc;

// --- Markdown -------------------------------------------------------------------------------------------------------
// The subset the guides use: headings, paragraphs, lists (nested, with code inside), fenced code, tables, block quotes,
// and inline code, emphasis, links, images and autolinks.

// GitHub's anchor for a heading, with -1, -2... for repeats in one file.
function slugger() {
  const seen = new Map();
  return (text) => {
    const base = text.toLowerCase().replace(/<[^>]+>/g, '').replace(/[^\p{L}\p{N}\s_-]/gu, '').trim().replace(/\s/g, '-');
    const n = seen.get(base) ?? 0;
    seen.set(base, n + 1);
    return n ? `${base}-${n}` : base;
  };
}
const plain = (md) => md.replace(/`([^`]+)`/g, '$1').replace(/\*\*([^*]+)\*\*/g, '$1').replace(/\*([^*]+)\*/g, '$1')
  .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1');

// Inline Markdown. `link` turns a link's target into the site's. A link's text shares its page's code spans.
function inline(text, link, codes = []) {
  let s = text.replace(/(`+)([\s\S]*?[^`])\1(?!`)/g, (_, _ticks, code) => `\u0000${codes.push(esc(code.trim())) - 1}\u0000`);
  const keep = [];
  const stash = (html) => `\u0001${keep.push(html) - 1}\u0001`;
  s = s.replace(/!\[([^\]]*)\]\(([^)\s]+)(?:\s+"([^"]*)")?\)/g, (_, alt, src) => stash(`<img src="${attr(link(src))}" alt="${attr(alt)}" />`));
  s = s.replace(/\[([^\]]+)\]\(([^)\s]+)(?:\s+"([^"]*)")?\)/g, (_, label, href) => stash(`<a href="${attr(link(href))}">${inline(label, link, codes)}</a>`));
  s = s.replace(/<(https?:\/\/[^>\s]+)>/g, (_, url) => stash(`<a href="${attr(url)}">${esc(url)}</a>`));
  s = esc(s.replace(/\\([\\`*_[\]|<>#])/g, (_, c) => stash(esc(c))));
  s = s.replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, '<strong>$1</strong>').replace(/(^|[^\w*])\*(?=\S)([\s\S]*?\S)\*(?!\w)/g, '$1<em>$2</em>');
  s = s.replace(/\u0001(\d+)\u0001/g, (_, i) => keep[i]).replace(/\u0001(\d+)\u0001/g, (_, i) => keep[i]);
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${codes[i]}</code>`);
}

// A little colour for code: comments, TOML's sections, keys and strings, and the command a shell line runs.
function highlight(code, lang) {
  const span = (cls, text) => `<span class="t-${cls}">${esc(text)}</span>`;
  const values = (text) => text.split(/("(?:[^"\\]|\\.)*"|'[^']*')/).map((part, i) => (i % 2 ? span('s', part) : esc(part))).join('');
  if (lang === 'toml') {
    return code.split('\n').map((line) => {
      if (/^\s*#/.test(line)) return span('c', line);
      if (/^\s*\[/.test(line) && !line.includes('=')) return span('h', line);
      const kv = /^(\s*[\w.-]+|\s*"[^"]*")(\s*=\s*)(.*)$/.exec(line);
      if (!kv) return values(line);
      const comment = /^(.*?)(\s+#.*)$/.exec(kv[3]);
      return `${span('k', kv[1])}${esc(kv[2])}${comment ? `${values(comment[1])}${span('c', comment[2])}` : values(kv[3])}`;
    }).join('\n');
  }
  if (['bash', 'sh', 'powershell'].includes(lang)) {
    return code.split('\n').map((line) => {
      if (/^\s*#/.test(line)) return span('c', line);
      const command = /^(\s*(?:sudo\s+)?)(moor|git|curl|irm|node|go|uv|npm|nginx|systemctl|python3?)(\b.*)$/.exec(line);
      return command ? `${esc(command[1])}${span('k', command[2])}${values(command[3])}` : values(line);
    }).join('\n');
  }
  return esc(code);
}

const cells = (row) => row.trim().replace(/^\|/, '').replace(/(?<!\\)\|$/, '').split(/(?<!\\)\|/).map((c) => c.trim());
const indentOf = (line) => /^ */.exec(line)[0].length;
const LIST = /^( *)([-*]|\d+\.) +(.*)$/;

// Blocks of Markdown to HTML. `ctx` has link (targets), slug (anchors), shot (figures) and headings (collected).
function blocks(lines, ctx) {
  const html = [];
  let i = 0;
  const para = [];
  const flush = () => {
    if (!para.length) return;
    const text = para.join(' ').trim();
    para.length = 0;
    const figure = /^!\[([^\]]*)\]\(images\/([\w-]+)-light\.(webp|svg)(?:\s+"([^"]*)")?\)$/.exec(text);
    html.push(figure ? ctx.shot(figure[2], figure[1], figure[4] ? inline(figure[4], ctx.link) : '', { ext: figure[3] }) : `<p>${inline(text, ctx.link)}</p>`);
  };
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { flush(); i += 1; continue; }
    const fence = /^ *(```+|~~~+)\s*([\w+-]*)/.exec(line);
    if (fence) {
      flush();
      const pad = indentOf(line);
      const body = [];
      i += 1;
      while (i < lines.length && !lines[i].trimStart().startsWith(fence[1])) body.push(lines[i].slice(Math.min(pad, indentOf(lines[i])))), i += 1;
      i += 1;
      html.push(`<div class="code"><pre tabindex="0"><code${fence[2] ? ` class="language-${fence[2]}"` : ''}>${highlight(body.join('\n'), fence[2])}</code></pre></div>`);
      continue;
    }
    const heading = /^(#{1,6})\s+(.*?)\s*#*$/.exec(line);
    if (heading) {
      flush();
      const level = heading[1].length;
      const id = ctx.slug(plain(heading[2]));
      ctx.headings.push({ level, id, text: heading[2] });
      html.push(`<h${level} id="${id}">${inline(heading[2], ctx.link)}<a class="anchor" href="#${id}" aria-label="Link to this section">#</a></h${level}>`);
      i += 1;
      continue;
    }
    if (line.trimStart().startsWith('|') && i + 1 < lines.length && /^\s*\|?\s*:?-{3,}/.test(lines[i + 1])) {
      flush();
      const head = cells(line);
      const align = cells(lines[i + 1]).map((c) => (c.endsWith(':') ? (c.startsWith(':') ? 'center' : 'right') : ''));
      const rows = [];
      i += 2;
      while (i < lines.length && lines[i].trimStart().startsWith('|')) rows.push(cells(lines[i])), i += 1;
      const cell = (tag, c, j) => `<${tag}${align[j] ? ` style="text-align:${align[j]}"` : ''}>${inline(c.replace(/\\\|/g, '|'), ctx.link)}</${tag}>`;
      const blank = head.every((c) => !c);
      html.push(`<div class="table-wrap" tabindex="0"><table>${blank ? '' : `<thead><tr>${head.map((c, j) => cell('th', c, j)).join('')}</tr></thead>`}<tbody>${rows.map((r) => `<tr>${r.map((c, j) => cell('td', c, j)).join('')}</tr>`).join('')}</tbody></table></div>`);
      continue;
    }
    if (/^ *>/.test(line)) {
      flush();
      const quoted = [];
      while (i < lines.length && /^ *>/.test(lines[i])) quoted.push(lines[i].replace(/^ *> ?/, '')), i += 1;
      html.push(`<blockquote>${blocks(quoted, ctx)}</blockquote>`);
      continue;
    }
    const item = LIST.exec(line);
    if (item && (!para.length || (item[1].length === 0 && /^([-*]|1\.)$/.test(item[2])))) {
      flush();
      const base = item[1].length;
      const ordered = /\d/.test(item[2]);
      const items = [];
      let loose = false;
      while (i < lines.length) {
        let j = i; // items may be apart, a blank line between them (a loose list)
        while (j < lines.length && !lines[j].trim()) j += 1;
        const m = LIST.exec(lines[j] ?? '');
        if (!m || m[1].length !== base || /\d/.test(m[2]) !== ordered) break;
        if (j > i) loose = true;
        i = j;
        const contentIndent = base + m[2].length + 1;
        const body = [m[3]];
        i += 1;
        while (i < lines.length) {
          const next = lines[i];
          if (!next.trim()) {
            const after = lines.slice(i + 1).find((l) => l.trim());
            if (after === undefined || indentOf(after) < contentIndent) break;
            body.push('');
            i += 1;
            continue;
          }
          if (indentOf(next) < contentIndent && (LIST.test(next) || !body.at(-1)?.trim() || indentOf(next) <= base)) break;
          body.push(next.slice(Math.min(contentIndent, indentOf(next))));
          i += 1;
        }
        items.push(body);
      }
      const start = ordered ? Number(/^\s*(\d+)/.exec(line)[1]) : 1;
      const rendered = items.map((body) => {
        const inner = blocks(body, ctx);
        const tight = !loose && !body.includes('') && /^<p>[\s\S]*?<\/p>(?=<|$)/.test(inner);
        return `<li>${tight ? inner.replace(/^<p>([\s\S]*?)<\/p>/, '$1') : inner}</li>`;
      });
      html.push(ordered ? `<ol${start !== 1 ? ` start="${start}"` : ''}>${rendered.join('')}</ol>` : `<ul>${rendered.join('')}</ul>`);
      continue;
    }
    if (/^ *(-{3,}|\*{3,})\s*$/.test(line) && !para.length) { flush(); html.push('<hr />'); i += 1; continue; }
    para.push(line.trim());
    i += 1;
  }
  flush();
  return html.join('\n');
}

// --- The guides, split into pages -----------------------------------------------------------------------------------

// A guide's parts: its title, the text before its first section, and each section (## heading) with every anchor in
// it, as GitHub makes them for the whole file.
function readGuide(file) {
  const lines = readFileSync(join(repo, file), 'utf8').replace(/\r\n/g, '\n').split('\n');
  const slug = slugger();
  const parts = { title: '', intro: [], sections: [], introAnchors: [] };
  let current = null;
  let fenced = false;
  for (const line of lines) {
    if (/^ *(```|~~~)/.test(line)) fenced = !fenced;
    const h = fenced ? null : /^(#{1,6})\s+(.*?)\s*#*$/.exec(line);
    if (h && h[1].length === 1 && !parts.title) { parts.title = h[2]; slug(plain(h[2])); continue; }
    if (h && h[1].length === 2) {
      current = { title: h[2], lines: [line], anchors: [slug(plain(h[2]))] };
      parts.sections.push(current);
      continue;
    }
    if (h) {
      const anchor = slug(plain(h[2]));
      if (current) current.anchors.push(anchor);
      else parts.introAnchors.push(anchor);
    }
    (current ? current.lines : parts.intro).push(line);
  }
  return parts;
}

const guides = new Map();
const guide = (file) => {
  if (!guides.has(file)) guides.set(file, readGuide(file));
  return guides.get(file);
};

// Where each guide's anchors are on the site: `file#anchor` → page slug, and `file` → its first page.
const where = new Map();
for (const page of PAGES) {
  if (page.generated) continue;
  const g = guide(page.file);
  const sections = page.sections ? page.sections.map((t) => {
    const s = g.sections.find((x) => x.title === t);
    if (!s) throw new Error(`${page.file} has no section "${t}" (for ${page.slug})`);
    return s;
  }) : g.sections.filter((s) => !page.omit?.includes(s.title));
  const whole = page.intro || !page.sections;
  page.parts = { intro: whole ? g.intro : [], sections };
  if (!where.has(page.file) && whole) where.set(page.file, page.slug);
  for (const s of sections) for (const a of s.anchors) where.set(`${page.file}#${a}`, page.slug);
  for (const a of whole ? g.introAnchors : []) where.set(`${page.file}#${a}`, page.slug);
}
for (const page of PAGES) if (page.file && !where.has(page.file)) where.set(page.file, page.slug);
const omitted = (file) => PAGES.filter((p) => p.file === file).flatMap((p) => p.omit ?? []);
for (const [file, g] of guides) {
  const unplaced = g.sections.filter((s) => !omitted(file).includes(s.title) && !where.has(`${file}#${s.anchors[0]}`)).map((s) => s.title);
  if (unplaced.length) throw new Error(`${file}: sections on no page: ${unplaced.join(', ')} (add them to DOCS in build.mjs)`);
}

const pageFile = (slug) => (slug === 'index' ? './' : `${slug}.html`);

// A link in a guide, as the site has it: a page and anchor for another guide, GitHub for other files.
function linker(file, pageSlug) {
  return (href) => {
    if (/^(https?:|mailto:)/.test(href)) return href;
    const [path, anchor] = href.split('#');
    if (!path) {
      const target = where.get(`${file}#${anchor}`);
      return target && target !== pageSlug ? `${pageFile(target)}#${anchor}` : `#${anchor}`;
    }
    const resolved = posix.normalize(posix.join(posix.dirname(file), path));
    if (resolved.startsWith('docs/images/') || resolved.startsWith('docs/assets/')) return `../assets/img/${posix.basename(resolved)}`;
    if (resolved === 'docs/openapi.json') return '../assets/openapi.json';
    if (where.has(resolved)) {
      const target = anchor ? where.get(`${resolved}#${anchor}`) : where.get(resolved);
      if (!target) return `#missing-${resolved}-${anchor}`; // the link check reports it
      return `${target === pageSlug ? '' : pageFile(target)}${anchor ? `#${anchor}` : ''}` || './';
    }
    const full = join(repo, resolved);
    const kind = existsSync(full) && statSync(full).isDirectory() ? 'tree' : 'blob';
    return `${GITHUB}/${kind}/main/${resolved}${anchor ? `#${anchor}` : ''}`;
  };
}

// A picture in both themes.
const shotsUsed = new Map();
function dimensions(name, ext) {
  const file = join(repo, 'docs/images', `${name}-light.${ext}`);
  if (!existsSync(file)) return { width: 1700, height: 1062 };
  const data = readFileSync(file);
  if (ext === 'svg') {
    const box = /viewBox="0 0 (\d+) (\d+)"/.exec(data.toString('utf8'));
    return box ? { width: Number(box[1]), height: Number(box[2]) } : { width: 1200, height: 600 };
  }
  // WebP: the size is in the first chunk, whichever kind that is.
  const kind = data.toString('latin1', 12, 16);
  if (kind === 'VP8 ') return { width: data.readUInt16LE(26) & 0x3fff, height: data.readUInt16LE(28) & 0x3fff };
  if (kind === 'VP8L') {
    const bits = data.readUInt32LE(21);
    return { width: (bits & 0x3fff) + 1, height: ((bits >> 14) & 0x3fff) + 1 };
  }
  if (kind === 'VP8X') return { width: 1 + data.readUIntLE(24, 3), height: 1 + data.readUIntLE(27, 3) };
  return { width: 1700, height: 1062 };
}
function shot(root) {
  return (name, alt, caption, { window = false, eager = false, ext = 'webp', phone = false } = {}) => {
    shotsUsed.set(name, ext);
    const size = dimensions(name, ext);
    phone ||= ext === 'webp' && size.height > size.width;
    const loading = eager ? 'fetchpriority="high"' : 'loading="lazy"';
    const img = (theme) => `<img class="shot-${theme}" src="${root}assets/img/${name}-${theme}.${ext}" width="${size.width}" height="${size.height}" ${loading} decoding="async" alt="${attr(alt)}" />`;
    const classes = ['shot', window && 'shot-window', ext === 'svg' && 'shot-diagram', phone && 'shot-phone'].filter(Boolean).join(' ');
    const frame = window ? '<div class="shot-bar" aria-hidden="true"><span></span><span></span><span></span></div>' : '';
    return `<figure${window ? ' class="hero-shot"' : ''}><div class="${classes}">${frame}${img('light')}${img('dark')}</div>${caption ? `<figcaption>${caption}</figcaption>` : ''}</figure>`;
  };
}

// --- Layout ---------------------------------------------------------------------------------------------------------

const ICON = {
  github: '<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M12 .5a11.5 11.5 0 0 0-3.64 22.41c.58.1.79-.25.79-.56v-2c-3.2.7-3.88-1.37-3.88-1.37-.53-1.34-1.29-1.7-1.29-1.7-1.05-.72.08-.7.08-.7 1.16.08 1.77 1.19 1.77 1.19 1.03 1.77 2.71 1.26 3.37.96.1-.75.4-1.26.73-1.55-2.56-.29-5.25-1.28-5.25-5.69 0-1.26.45-2.29 1.19-3.1-.12-.29-.52-1.46.11-3.05 0 0 .97-.31 3.17 1.18a11 11 0 0 1 5.77 0c2.2-1.49 3.17-1.18 3.17-1.18.63 1.59.23 2.76.11 3.05.74.81 1.19 1.84 1.19 3.1 0 4.42-2.7 5.4-5.26 5.68.41.36.78 1.06.78 2.14v3.17c0 .31.21.67.8.56A11.5 11.5 0 0 0 12 .5Z"/></svg>',
  moon: '<svg class="icon-moon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z"/></svg>',
  sun: '<svg class="icon-sun" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>',
  menu: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M4 7h16M4 12h16M4 17h16"/></svg>',
  search: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.8-3.8"/></svg>',
};

const home = (root) => root || './';

function head({ title, description, path = '' }, root) {
  const full = title === 'Moorage' ? `Moorage: ${TAGLINE}` : `${title} · Moorage`;
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>${esc(full)}</title>
  <meta name="description" content="${attr(description)}" />
  <link rel="canonical" href="${SITE}${path}" />
  <meta property="og:type" content="website" />
  <meta property="og:site_name" content="Moorage" />
  <meta property="og:title" content="${attr(full)}" />
  <meta property="og:description" content="${attr(description)}" />
  <meta property="og:url" content="${SITE}${path}" />
  <meta property="og:image" content="${SITE}assets/img/og-image.jpg" />
  <meta property="og:image:width" content="1200" />
  <meta property="og:image:height" content="630" />
  <meta name="twitter:card" content="summary_large_image" />
  <meta name="theme-color" content="#08264c" />
  <link rel="icon" href="${root}assets/img/moorage-icon.svg" type="image/svg+xml" />
  <link rel="stylesheet" href="${root}assets/site.css" />
  <script>try { const t = localStorage.getItem('moorage-site-theme'); if (t) document.documentElement.dataset.theme = t; } catch (e) {}</script>
  <script src="${root}assets/site.js" defer></script>
</head>
<body data-root="${root}">
  <a class="skip-link" href="#main">Skip to content</a>
`;
}

function header(root, current) {
  const link = (href, label, key) => `<a href="${root}${href}"${current === key ? ' aria-current="page"' : ''}>${label}</a>`;
  const features = root ? `${root}index.html#features` : '#features';
  return `
  <header class="site-header">
    <div class="container">
      <a class="brand" href="${home(root)}" aria-label="Moorage home"><img class="logo-light" src="${root}assets/img/moorage-logo.svg" alt="" width="133" height="30" /><img class="logo-dark" src="${root}assets/img/moorage-logo-dark.svg" alt="" width="133" height="30" /></a>
      <nav class="site-nav" id="site-nav" aria-label="Main">
        <a href="${features}">Features</a>
        ${link('docs/getting-started.html', 'Get started', 'start')}
        ${link('docs/', 'Documentation', 'docs')}
        ${link('docs/install.html', 'Install', 'install')}
        ${link('docs/changelog.html', 'Changelog', 'changelog')}
      </nav>
      <div class="header-actions">
        <button class="search-button" type="button" data-search-open aria-label="Search the documentation">${ICON.search}<span>Search</span><kbd>/</kbd></button>
        <a class="icon-button" href="${GITHUB}" aria-label="Moorage on GitHub">${ICON.github}</a>
        <button class="icon-button theme-toggle" type="button" aria-label="Switch between light and dark">${ICON.moon}${ICON.sun}</button>
        <button class="icon-button nav-toggle" type="button" aria-label="Menu" aria-controls="site-nav" aria-expanded="false">${ICON.menu}</button>
      </div>
    </div>
  </header>
`;
}

function footer(root) {
  const docs = (slug, label) => `<li><a href="${root}docs/${slug}.html">${label}</a></li>`;
  return `
  <footer class="site-footer">
    <div class="container">
      <div class="footer-grid">
        <div class="footer-about">
          <a class="brand" href="${home(root)}" aria-label="Moorage home"><img class="logo-light" src="${root}assets/img/moorage-logo.svg" alt="" width="133" height="30" /><img class="logo-dark" src="${root}assets/img/moorage-logo-dark.svg" alt="" width="133" height="30" /></a>
          <p>${esc(TAGLINE)} Free and open source under the Apache License 2.0.</p>
        </div>
        <div>
          <h2>Use</h2>
          <ul>
            ${docs('getting-started', 'Getting started')}
            ${docs('git', 'Deploy with git push')}
            ${docs('previews', 'Previews')}
            ${docs('manifest', 'moorage.toml')}
          </ul>
        </div>
        <div>
          <h2>Run</h2>
          <ul>
            ${docs('install', 'Install')}
            ${docs('administration', 'Running a server')}
            ${docs('backups', 'Backups')}
            ${docs('troubleshooting', 'Troubleshooting')}
          </ul>
        </div>
        <div>
          <h2>Project</h2>
          <ul>
            <li><a href="${GITHUB}">GitHub</a></li>
            <li><a href="${GITHUB}/releases">Releases</a></li>
            <li><a href="${GITHUB}/issues">Issues</a></li>
            ${docs('architecture', 'Architecture')}
            ${docs('security', 'Security')}
          </ul>
        </div>
      </div>
      <p class="footer-note">Moorage ${esc(VERSION)} · <a href="${GITHUB}/blob/main/LICENSE">Apache License 2.0</a></p>
    </div>
  </footer>

  <dialog class="search-dialog" aria-label="Search the documentation">
    <div class="search-box">
      ${ICON.search}
      <input type="search" placeholder="Search the documentation" aria-label="Search the documentation" autocomplete="off" spellcheck="false" />
      <button type="button" class="search-close" aria-label="Close">Esc</button>
    </div>
    <ul class="search-results" role="list"></ul>
  </dialog>
</body>
</html>
`;
}

function sidebar(current) {
  const groups = DOCS.map((g) => `
        <div class="docs-group">
          <h2>${esc(g.group)}</h2>
          <ul>
            ${g.pages.map((p) => `<li><a href="${pageFile(p.slug)}"${p.slug === current ? ' aria-current="page"' : ''}>${esc(p.navTitle ?? p.title)}</a></li>`).join('\n            ')}
          </ul>
        </div>`).join('');
  return `
    <aside class="docs-sidebar">
      <button class="docs-menu-toggle" type="button" aria-expanded="false">Documentation menu</button>
      <nav class="docs-nav" aria-label="Documentation">${groups}
      </nav>
    </aside>`;
}

function pager(current) {
  const at = PAGES.findIndex((p) => p.slug === current);
  const prev = PAGES[at - 1];
  const next = PAGES[at + 1];
  const card = (p, rel, label) => (p ? `<a class="doc-pager-${rel}" rel="${rel}" href="${pageFile(p.slug)}"><span>${label}</span>${esc(p.title)}</a>` : '<span></span>');
  return `<nav class="doc-pager" aria-label="Previous and next pages">${card(prev, 'prev', 'Previous')}${card(next, 'next', 'Next')}</nav>`;
}

// "On this page": the page's sections, and the headings within them, or only its sections on a very long page.
function tocEntries(headings) {
  const list = headings.filter((h) => h.level === 2 || h.level === 3);
  return list.length > 40 ? list.filter((h) => h.level === 2) : list;
}
const tocList = (list, link) => `<ul>
        ${list.map((h) => `<li class="toc-${h.level}"><a href="#${h.id}">${inline(h.text, link)}</a></li>`).join('\n        ')}
      </ul>`;
function tocOf(headings, link) {
  const list = tocEntries(headings);
  if (list.length < 2) return '';
  return `
    <aside class="toc" aria-label="On this page">
      <h2>On this page</h2>
      ${tocList(list, link)}
    </aside>`;
}
// The same, for where there is no room for that column.
function tocInline(headings, link) {
  const list = tocEntries(headings);
  if (list.length < 4) return '';
  return `<details class="toc-inline"><summary>On this page</summary>${tocList(list, link)}</details>`;
}

// --- Writing the site -----------------------------------------------------------------------------------------------

rmIfNotCheckout();
function rmIfNotCheckout() {
  // Only what the build itself writes goes, so that a folder given by mistake loses nothing else.
  for (const name of ['docs', 'assets', 'index.html', '404.html', 'sitemap.xml', 'robots.txt', '.nojekyll']) {
    rmSync(join(out, name), { recursive: true, force: true });
  }
}
mkdirSync(out, { recursive: true });

const written = [];
const write = (path, text) => {
  const file = join(out, path);
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, text);
  if (path.endsWith('.html') && !path.endsWith('404.html')) written.push(path.replace(/index\.html$/, ''));
};

// What the search looks through: a page's title, its headings, and its words.
const searchIndex = [];
const words = (html) => html.replace(/<pre[^>]*>[\s\S]*?<\/pre>/g, ' ').replace(/<a class="anchor"[^>]*>#<\/a>/g, '')
  .replace(/<\/(p|li|td|th|h\d|div|tr|figcaption|blockquote)>/g, ' ').replace(/<[^>]+>/g, '')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&amp;/g, '&').replace(/\s+/g, ' ').trim();

function docPage(page, body, headings, { single = '', edit = page.file } = {}) {
  const root = '../';
  const path = `docs/${page.slug === 'index' ? '' : `${page.slug}.html`}`;
  const link = page.file ? linker(page.file, page.slug) : (x) => x;
  const section = page.slug === 'install' ? 'install' : page.slug === 'getting-started' ? 'start' : page.slug === 'changelog' ? 'changelog' : 'docs';
  const editLink = edit ? `<p class="doc-edit"><a href="${GITHUB}/edit/main/${edit}">Suggest a change to this page</a></p>` : '';
  write(`docs/${page.slug}.html`, head({ title: page.title, description: page.lede, path }, root) + header(root, section) + `
  <div class="docs-layout">${sidebar(page.slug)}
    <main id="main" class="doc">
      ${single ? `<span id="${single}" class="anchor-target"></span>` : ''}<h1>${esc(page.title)}</h1>
      <p class="doc-lede">${esc(page.lede)}</p>
${tocInline(headings, link)}
${body}
${editLink}
${pager(page.slug)}
    </main>
${tocOf(headings, link)}
  </div>
` + footer(root));
  searchIndex.push({
    t: page.title,
    u: `docs/${page.slug}.html`,
    g: DOCS.find((g) => g.pages.includes(page)).group,
    h: headings.filter((h) => h.level <= 3).map((h) => [h.id, plain(h.text)]),
    b: words(body).slice(0, 6000),
  });
}

for (const page of PAGES) {
  if (page.generated) continue;
  const slug = slugger();
  const g = guide(page.file);
  // Anchors as GitHub makes them for the whole guide, so that links from elsewhere find them on this page.
  const anchors = [...(page.parts.intro.length ? g.introAnchors : []), ...page.parts.sections.flatMap((s) => s.anchors)];
  let next = 0;
  const ctx = { link: linker(page.file, page.slug), shot: shot('../'), headings: [], slug: () => anchors[next++] ?? slug('heading') };
  let lines = [...page.parts.intro, ...page.parts.sections.flatMap((s) => s.lines)];
  // A page of one section, without the guide's introduction, takes the section's heading as its own: its h3s become
  // h2s, and its anchor is the page's top.
  let single = '';
  if (page.sections?.length === 1 && !page.intro) {
    single = page.parts.sections[0].anchors[0];
    next = 1;
    lines = lines.slice(1).map((l) => l.replace(/^###(?!#)/, '##').replace(/^####(?!#)/, '###'));
  }
  docPage(page, blocks(lines, ctx), ctx.headings, { single });
}

// The API reference, from the OpenAPI document that the server's own tests keep current.
{
  const page = PAGES.find((p) => p.generated === 'api');
  const api = JSON.parse(readFileSync(join(repo, 'docs/openapi.json'), 'utf8'));
  const METHODS = ['get', 'post', 'put', 'patch', 'delete'];
  // What a route's permission asks of a role: internal/api/routes.go names them.
  const PERMISSIONS = { public: 'Nothing: anyone may ask', 'signed-in': 'Anyone signed in', read: 'Viewer', logs: 'Viewer, if the project lets viewers read logs', deploy: 'Deployer', admin: 'Maintainer', 'server-admin': 'Administrator' };
  const refName = (ref) => ref.split('/').at(-1);
  // A type, as a link to its document if it has one.
  const typeOf = (s) => {
    if (!s) return '';
    if (s.$ref) return `<a href="#schema-${esc(refName(s.$ref))}"><code>${esc(refName(s.$ref))}</code></a>`;
    if (s.type === 'array') return `${typeOf(s.items)}<code>[]</code>`;
    if (s.additionalProperties && typeof s.additionalProperties === 'object') return `<code>map of</code> ${typeOf(s.additionalProperties)}`;
    return `<code>${esc(s.format ?? s.type ?? 'object')}</code>`;
  };
  const groups = new Map();
  for (const [path, item] of Object.entries(api.paths)) {
    const part = path.split('/')[1] || 'root';
    const name = path.startsWith('/apps/') && /\/envs\//.test(path) ? 'environments' : part;
    for (const method of METHODS) {
      if (!item[method]) continue;
      if (!groups.has(name)) groups.set(name, []);
      groups.get(name).push({ path, method, op: item[method] });
    }
  }
  const TITLES = { apps: 'Apps', environments: 'An app\'s environments', auth: 'Signing in', me: 'You', server: 'The server', projects: 'Projects', users: 'Users', events: 'What happened', deployments: 'Deployments', builds: 'Builds', notifications: 'Notifications', audit: 'The audit log', hooks: 'Webhooks from forges', previews: 'Previews', registries: 'Container registries', 'openapi.json': 'This document' };
  const ORDER = ['apps', 'environments', 'deployments', 'previews', 'events', 'projects', 'users', 'me', 'auth', 'notifications', 'registries', 'audit', 'server', 'hooks', 'openapi.json'];
  const rank = (name) => { const i = ORDER.indexOf(name); return i < 0 ? ORDER.length : i; };
  const titleOf = (name) => TITLES[name] ?? name[0].toUpperCase() + name.slice(1);
  const headings = [];
  const seen = slugger();
  let body = `<p>This page is made from <a href="../assets/openapi.json">the OpenAPI document</a>, which a test of the server keeps the same as the routes it serves. <a href="api.html">The API</a> says how to authenticate, page through lists and read errors. Paths are under <code>/api/v1</code>.</p>\n`;
  for (const [name, ops] of [...groups.entries()].sort((a, b) => rank(a[0]) - rank(b[0]))) {
    const id = seen(titleOf(name));
    headings.push({ level: 2, id, text: titleOf(name) });
    body += `<h2 id="${id}">${esc(titleOf(name))}<a class="anchor" href="#${id}" aria-label="Link to this section">#</a></h2>\n<div class="api-list">\n`;
    for (const { path, method, op } of ops) {
      const query = (op.parameters ?? []).filter((p) => p.in === 'query').map((p) => `<code>${esc(p.name)}</code>`).join(', ');
      const takes = typeOf(op.requestBody?.content?.['application/json']?.schema);
      const okKey = Object.keys(op.responses ?? {}).find((k) => k.startsWith('2'));
      const ok = okKey ? op.responses[okKey] : null;
      const gives = ok?.content?.['application/json'] ? typeOf(ok.content['application/json'].schema) : ok?.content ? `<code>${esc(Object.keys(ok.content)[0])}</code>` : '';
      const needs = PERMISSIONS[op['x-moorage-permission']];
      if (!needs) throw new Error(`${method} ${path}: no words for the permission ${op['x-moorage-permission']}`);
      const details = [
        needs && `<span><b>Needs</b> ${esc(needs)}</span>`,
        query && `<span><b>Query</b> ${query}</span>`,
        takes && `<span><b>Takes</b> ${takes}</span>`,
        gives && `<span><b>Gives</b> ${gives}</span>`,
      ].filter(Boolean).join('');
      body += `<div class="api-op"><div class="api-route"><span class="api-method api-${method}">${method.toUpperCase()}</span><code>${esc(path)}</code></div><p>${esc(op.summary ?? '')}</p>${details ? `<div class="api-details">${details}</div>` : ''}</div>\n`;
    }
    body += '</div>\n';
  }
  // The documents that routes take and give.
  headings.push({ level: 2, id: 'documents', text: 'Documents' });
  body += '<h2 id="documents">Documents<a class="anchor" href="#documents" aria-label="Link to this section">#</a></h2>\n<p>What the routes above take and give, as JSON.</p>\n';
  for (const name of Object.keys(api.components?.schemas ?? {}).sort()) {
    const schema = api.components.schemas[name];
    const required = new Set(schema.required ?? []);
    const fields = Object.entries(schema.properties ?? {});
    body += `<h3 id="schema-${esc(name)}">${esc(name)}</h3>\n`;
    if (!fields.length) { body += `<p>${typeOf(schema)}</p>\n`; continue; }
    body += `<div class="table-wrap" tabindex="0"><table><thead><tr><th>Field</th><th>Type</th><th>Required</th></tr></thead><tbody>${fields.map(([field, s]) => `<tr><td><code>${esc(field)}</code></td><td>${typeOf(s)}</td><td>${required.has(field) ? 'Yes' : ''}</td></tr>`).join('')}</tbody></table></div>\n`;
  }
  docPage(page, body, headings, { edit: null });
}

// The home page: pages/index.html, with {{shot name "alt" "caption" [window] [eager] [phone]}} for pictures, and
// {{version}}, {{github}} and {{raw}}.
{
  const text = readFileSync(join(here, 'pages/index.html'), 'utf8');
  const take = shot('');
  const body = text
    .replace(/\{\{shot\s+([\w-]+)\s+"([^"]*)"(?:\s+"([^"]*)")?((?:\s+\w+)*)\s*\}\}/g, (_, name, alt, caption, flags) =>
      take(name, alt, caption ?? '', { window: /\bwindow\b/.test(flags), eager: /\beager\b/.test(flags), phone: /\bphone\b/.test(flags) }))
    .replaceAll('{{version}}', esc(VERSION)).replaceAll('{{github}}', GITHUB).replaceAll('{{raw}}', RAW);
  write('index.html', head({ title: 'Moorage', description: 'Moorage is a self-hosted platform for deploying web apps: one program, with builds from source, a URL for every app and branch, zero-downtime releases, native runtimes or containers, and a web console. For Linux, macOS and Windows.' }, '') + header('', 'home') + `
  <main id="main">
${body.trimEnd()}
  </main>
` + footer(''));
}

// A page for an address that is not one. GitHub Pages serves it from anywhere, so its links are absolute.
{
  const root = '/moorage/';
  write('404.html', head({ title: 'Not found', description: 'This page is not here.', path: '404.html' }, root) + header(root, '') + `
  <main id="main" class="not-found">
    <div class="container">
      <h1>This page is not here</h1>
      <p>It may have moved. <a href="${root}docs/">The documentation</a> lists everything, and <a href="${root}">the home page</a> says what Moorage is.</p>
    </div>
  </main>
` + footer(root));
}

// Everything else.
for (const dir of ['assets/img']) mkdirSync(join(out, dir), { recursive: true });
copyFileSync(join(here, 'assets/site.css'), join(out, 'assets/site.css'));
copyFileSync(join(here, 'assets/site.js'), join(out, 'assets/site.js'));
copyFileSync(join(repo, 'docs/openapi.json'), join(out, 'assets/openapi.json'));
for (const name of ['moorage-icon.svg', 'moorage-logo.svg', 'moorage-logo-dark.svg']) copyFileSync(join(repo, 'docs/assets', name), join(out, 'assets/img', name));
for (const [name, ext] of shotsUsed) for (const theme of ['light', 'dark']) {
  const image = join(repo, 'docs/images', `${name}-${theme}.${ext}`);
  if (existsSync(image)) copyFileSync(image, join(out, 'assets/img', `${name}-${theme}.${ext}`));
}
if (existsSync(join(here, 'assets/og-image.jpg'))) copyFileSync(join(here, 'assets/og-image.jpg'), join(out, 'assets/img/og-image.jpg'));
writeFileSync(join(out, 'assets/search.json'), JSON.stringify(searchIndex));
write('.nojekyll', '');
write('robots.txt', `User-agent: *\nAllow: /\n\nSitemap: ${SITE}sitemap.xml\n`);
write('sitemap.xml', `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
${written.map((p) => `  <url><loc>${SITE}${p}</loc></url>`).join('\n')}
</urlset>
`);

// --- Checking it ----------------------------------------------------------------------------------------------------

const problems = [];
const ids = new Map();
const idsOf = (file) => {
  if (!ids.has(file)) ids.set(file, new Set([...readFileSync(file, 'utf8').matchAll(/\sid="([^"]+)"/g)].map((m) => m[1])));
  return ids.get(file);
};
const built = [];
const walk = (dir) => {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.name.startsWith('.')) continue;
    const full = join(dir, entry.name);
    if (entry.isDirectory()) walk(full);
    else if (entry.name.endsWith('.html')) built.push(full);
  }
};
walk(out);
for (const file of built) {
  if (file.endsWith('404.html')) continue;
  const html = readFileSync(file, 'utf8');
  for (const [, url] of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
    if (/^(https?:|mailto:|data:)/.test(url)) continue;
    const [path, anchor] = url.split('#');
    let target = path ? resolve(dirname(file), path) : file;
    if (path.endsWith('/') || path === '.' || (existsSync(target) && statSync(target).isDirectory())) target = join(target, 'index.html');
    const at = relative(out, file);
    if (!existsSync(target)) problems.push(`${at}: ${url} does not exist`);
    else if (anchor && target.endsWith('.html') && !idsOf(target).has(anchor)) problems.push(`${at}: ${url} has no #${anchor}`);
  }
}
for (const [name, ext] of shotsUsed) for (const theme of ['light', 'dark']) {
  if (!existsSync(join(repo, 'docs/images', `${name}-${theme}.${ext}`))) problems.push(`docs/images/${name}-${theme}.${ext} is missing`);
}
console.log(`Built ${built.length} pages into ${relative(process.cwd(), out) || '.'} (Moorage ${VERSION}, ${shotsUsed.size} pictures).`);
if (problems.length) {
  console.error(`${problems.length} problem${problems.length === 1 ? '' : 's'}:\n  ${[...new Set(problems)].join('\n  ')}`);
  process.exitCode = 1;
}
