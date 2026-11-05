// SPDX-License-Identifier: Apache-2.0
// The Moorage website: the theme, the menus, copy buttons, the install tabs, the "On this page" list, a larger view of
// screenshots, and the search. The site reads and works without any of it.

const storage = {
  get(key) { try { return localStorage.getItem(key); } catch { return null; } },
  set(key, value) { try { localStorage.setItem(key, value); } catch { /* private mode */ } },
};

const ICON_COPY = '<svg class="icon-copy" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
const ICON_CHECK = '<svg class="icon-check" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 6 9 17l-5-5"/></svg>';

// The theme: the saved choice, else the system's. The button flips between light and dark.
function currentTheme() {
  const saved = document.documentElement.dataset.theme;
  if (saved) return saved;
  return matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}
document.querySelectorAll('.theme-toggle').forEach((button) => {
  button.addEventListener('click', () => {
    const next = currentTheme() === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    storage.set('moorage-site-theme', next);
  });
});

// The menus.
document.querySelectorAll('.nav-toggle').forEach((button) => {
  const nav = document.getElementById(button.getAttribute('aria-controls'));
  button.addEventListener('click', () => button.setAttribute('aria-expanded', String(nav.classList.toggle('is-open'))));
  // A link to a place on this page leaves the menu in the way otherwise.
  nav.addEventListener('click', (event) => {
    if (!event.target.closest('a')) return;
    nav.classList.remove('is-open');
    button.setAttribute('aria-expanded', 'false');
  });
});
document.querySelectorAll('.docs-menu-toggle').forEach((button) => {
  const sidebar = button.closest('.docs-sidebar');
  button.addEventListener('click', () => button.setAttribute('aria-expanded', String(sidebar.classList.toggle('is-open'))));
});
// The menu of pages opens where you are.
document.querySelector('.docs-group a[aria-current="page"]')?.scrollIntoView({ block: 'center', behavior: 'instant' });
window.scrollTo({ top: location.hash ? window.scrollY : 0, behavior: 'instant' });

// Copy buttons on code.
document.querySelectorAll('.code').forEach((block) => {
  const pre = block.querySelector('pre');
  if (!pre) return;
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'copy-button';
  button.setAttribute('aria-label', 'Copy to clipboard');
  button.innerHTML = ICON_COPY + ICON_CHECK;
  button.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(pre.innerText.trim());
      button.classList.add('copied');
      button.setAttribute('aria-label', 'Copied');
      setTimeout(() => { button.classList.remove('copied'); button.setAttribute('aria-label', 'Copy to clipboard'); }, 1600);
    } catch { /* no clipboard here */ }
  });
  block.append(button);
});

// The install tabs.
document.querySelectorAll('[data-tabs]').forEach((group) => {
  const buttons = [...group.querySelectorAll('[role="tab"]')];
  const choose = (button) => {
    for (const b of buttons) {
      const on = b === button;
      b.setAttribute('aria-selected', String(on));
      b.tabIndex = on ? 0 : -1;
      document.getElementById(b.getAttribute('aria-controls')).hidden = !on;
    }
  };
  buttons.forEach((button, i) => {
    button.addEventListener('click', () => choose(button));
    button.addEventListener('keydown', (event) => {
      const to = event.key === 'ArrowRight' ? i + 1 : event.key === 'ArrowLeft' ? i - 1 : null;
      if (to === null) return;
      const next = buttons[(to + buttons.length) % buttons.length];
      choose(next);
      next.focus();
    });
  });
  if (/Win/.test(navigator.platform)) choose(buttons.find((b) => b.dataset.os === 'windows') ?? buttons[0]);
});

// "On this page": where you are.
const tocLinks = [...document.querySelectorAll('.toc a')];
const toc = document.querySelector('.toc');
// A long list follows too, moving only itself and not the page.
function keepInView(link) {
  const list = toc.getBoundingClientRect();
  const item = link.getBoundingClientRect();
  if (item.top < list.top + 40) toc.scrollTop -= list.top + 40 - item.top;
  else if (item.bottom > list.bottom - 20) toc.scrollTop += item.bottom - list.bottom + 20;
}
if (tocLinks.length) {
  const targets = tocLinks.map((a) => document.getElementById(decodeURIComponent(a.hash.slice(1)))).filter(Boolean);
  const mark = () => {
    let current = targets[0];
    for (const t of targets) if (t.getBoundingClientRect().top < 120) current = t;
    for (const a of tocLinks) {
      const on = a.hash.slice(1) === current?.id;
      if (on && !a.classList.contains('is-current')) keepInView(a);
      a.classList.toggle('is-current', on);
    }
  };
  mark();
  document.addEventListener('scroll', mark, { passive: true });
}

// A screenshot, larger.
const lightbox = document.createElement('dialog');
lightbox.className = 'lightbox';
lightbox.setAttribute('aria-label', 'A larger view of the picture');
const large = document.createElement('img');
const closeBox = document.createElement('button');
closeBox.type = 'button';
closeBox.className = 'lightbox-close';
closeBox.textContent = 'Close';
lightbox.append(closeBox, large);
document.body.append(lightbox);
lightbox.addEventListener('click', () => lightbox.close());
document.querySelectorAll('.shot').forEach((shot) => {
  const enlarge = () => {
    const shown = [...shot.querySelectorAll('img')].find((img) => getComputedStyle(img).display !== 'none');
    if (!shown) return;
    large.src = shown.src;
    large.alt = shown.alt;
    lightbox.showModal();
  };
  shot.tabIndex = 0;
  shot.setAttribute('role', 'button');
  shot.setAttribute('aria-label', 'See this picture larger');
  shot.addEventListener('click', enlarge);
  shot.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      enlarge();
    }
  });
});

// The search: a small index of every page, fetched the first time it is wanted.
const dialog = document.querySelector('.search-dialog');
if (dialog) {
  const input = dialog.querySelector('input');
  const results = dialog.querySelector('.search-results');
  const root = document.body.dataset.root ?? '';
  let index = null;
  let selected = -1;
  const escape = (s) => s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  // Marks every word at once on the raw text, and escapes the pieces, so that one mark can never land inside another.
  const marked = (text, words) => {
    const pattern = new RegExp(`(${words.map((w) => w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})`, 'ig');
    return text.split(pattern).map((part, i) => (i % 2 ? `<mark>${escape(part)}</mark>` : escape(part))).join('');
  };

  function search(query) {
    const words = query.toLowerCase().split(/\s+/).filter((w) => w.length > 1);
    if (!words.length || !index) return [];
    const hits = [];
    for (const page of index) {
      const title = page.t.toLowerCase();
      const body = page.b.toLowerCase();
      let score = 0;
      let heading = null;
      let all = true;
      for (const w of words) {
        const inTitle = title.includes(w);
        const h = page.h.find(([, text]) => text.toLowerCase().includes(w));
        const inBody = body.includes(w);
        if (!inTitle && !h && !inBody) { all = false; break; }
        if (inTitle) score += title === w ? 40 : 20;
        if (h) { score += 8; heading ??= h; }
        if (inBody) score += Math.min(4, body.split(w).length - 1);
      }
      if (!all) continue;
      const first = body.indexOf(words[0]);
      const start = Math.max(0, first - 50);
      const snippet = first >= 0 ? `${start ? '…' : ''}${page.b.slice(start, first + 90)}…` : '';
      hits.push({ page, score, heading, snippet });
    }
    return hits.sort((a, b) => b.score - a.score).slice(0, 12).map((hit) => ({ ...hit, words }));
  }

  function show() {
    const query = input.value.trim();
    const hits = search(query);
    selected = hits.length ? 0 : -1;
    input.setAttribute('aria-expanded', String(hits.length > 0));
    input.removeAttribute('aria-activedescendant');
    if (!hits.length) {
      results.innerHTML = query.length > 1 && index ? '<li class="search-none" role="presentation">Nothing matches that.</li>' : '';
      return;
    }
    results.innerHTML = hits.map(({ page, heading, snippet, words }, i) => {
      const href = `${root}${page.u}${heading ? `#${heading[0]}` : ''}`;
      return `<li role="presentation"><a id="search-hit-${i}" role="option" aria-selected="${i === 0}" href="${escape(href)}"${i === 0 ? ' class="is-selected"' : ''}><b>${marked(page.t, words)}</b> <span>${escape(page.g)}${heading ? ` · ${marked(heading[1], words)}` : ''}</span>${snippet ? `<small>${marked(snippet, words)}</small>` : ''}</a></li>`;
    }).join('');
    input.setAttribute('aria-activedescendant', 'search-hit-0');
  }

  function select(step) {
    const links = [...results.querySelectorAll('a')];
    if (!links.length) return;
    selected = (selected + step + links.length) % links.length;
    links.forEach((a, i) => {
      a.classList.toggle('is-selected', i === selected);
      a.setAttribute('aria-selected', String(i === selected));
    });
    input.setAttribute('aria-activedescendant', links[selected].id);
    links[selected].scrollIntoView({ block: 'nearest' });
  }

  async function open() {
    dialog.showModal();
    input.select();
    if (!index) {
      try {
        index = await (await fetch(`${root}assets/search.json`)).json();
        show();
      } catch {
        results.innerHTML = '<li class="search-none">The search could not load.</li>';
      }
    }
  }

  document.querySelectorAll('[data-search-open]').forEach((b) => b.addEventListener('click', open));
  dialog.querySelector('.search-close').addEventListener('click', () => dialog.close());
  dialog.addEventListener('click', (event) => { if (event.target === dialog) dialog.close(); });
  input.addEventListener('input', show);
  input.addEventListener('keydown', (event) => {
    // A search box clears itself on Escape, but the button beside it says that Escape closes.
    if (event.key === 'Escape') { event.preventDefault(); dialog.close(); }
    if (event.key === 'ArrowDown') { event.preventDefault(); select(1); }
    if (event.key === 'ArrowUp') { event.preventDefault(); select(-1); }
    if (event.key === 'Enter') {
      event.preventDefault();
      const link = results.querySelector('a.is-selected');
      if (link) { dialog.close(); location.href = link.href; }
    }
  });
  document.addEventListener('keydown', (event) => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(event.target.tagName) || event.target.isContentEditable;
    if ((event.key === '/' && !typing) || ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k')) {
      event.preventDefault();
      open();
    }
  });
}
