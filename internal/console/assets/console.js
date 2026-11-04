// What scripts add to the console's pages, all of which work without them: the
// theme, copying commands, times that stay true, the command palette, and live
// updates over one stream of events.
(function () {
  'use strict';

  var $ = function (selector, root) { return (root || document).querySelector(selector); };
  var $$ = function (selector, root) { return Array.prototype.slice.call((root || document).querySelectorAll(selector)); };

  // The theme

  var THEME_KEY = 'moorage-theme';

  function chosenTheme() {
    try { return localStorage.getItem(THEME_KEY) || 'system'; } catch (e) { return 'system'; }
  }

  function setTheme(theme) {
    try {
      if (theme === 'system') { localStorage.removeItem(THEME_KEY); } else { localStorage.setItem(THEME_KEY, theme); }
    } catch (e) { /* It lasts for this page, then. */ }
    if (theme === 'system') {
      document.documentElement.removeAttribute('data-theme');
    } else {
      document.documentElement.setAttribute('data-theme', theme);
    }
    showTheme();
  }

  function showTheme() {
    var theme = chosenTheme();
    $$('[data-theme-set]').forEach(function (button) {
      button.setAttribute('aria-pressed', String(button.getAttribute('data-theme-set') === theme));
    });
  }

  $$('.theme-choice').forEach(function (group) { group.hidden = false; });
  $$('[data-theme-set]').forEach(function (button) {
    button.addEventListener('click', function () { setTheme(button.getAttribute('data-theme-set')); });
  });
  showTheme();

  // Copying commands

  $$('.command').forEach(function (block) {
    var pre = $('pre', block);
    if (!pre || !navigator.clipboard) { return; }
    var button = document.createElement('button');
    button.type = 'button';
    button.textContent = 'Copy';
    button.setAttribute('aria-label', 'Copy this command');
    button.addEventListener('click', function () {
      navigator.clipboard.writeText(pre.textContent).then(function () {
        button.textContent = 'Copied';
        setTimeout(function () { button.textContent = 'Copy'; }, 1500);
      });
    });
    block.appendChild(button);
  });

  // Times

  function ago(date) {
    var seconds = (Date.now() - date.getTime()) / 1000;
    if (seconds < 60) { return 'just now'; }
    if (seconds < 3600) { return Math.floor(seconds / 60) + ' min ago'; }
    if (seconds < 86400) { return Math.floor(seconds / 3600) + ' h ago'; }
    var days = Math.floor(seconds / 86400);
    if (days < 30) { return days === 1 ? 'yesterday' : days + ' days ago'; }
    return date.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  }

  function showTimes() {
    $$('time[datetime]').forEach(function (el) {
      var date = new Date(el.getAttribute('datetime'));
      if (isNaN(date.getTime())) { return; }
      el.textContent = ago(date);
      el.title = date.toLocaleString();
    });
  }
  showTimes();
  setInterval(showTimes, 30000);

  // The select that filters the timeline needs no button to press.

  var filter = $('[data-autosubmit]');
  if (filter) {
    filter.addEventListener('change', function () { filter.form.submit(); });
    var submit = $('button', filter.form);
    if (submit) { submit.hidden = true; }
  }

  // The command palette

  var palette = $('#palette');
  var data = $('#palette-data');
  if (palette && data && palette.showModal) {
    setUpPalette(palette, JSON.parse(data.textContent || '[]') || []);
  }

  function setUpPalette(dialog, items) {
    var input = $('input', dialog);
    var list = $('ul', dialog);
    var none = $('.palette-none', dialog);
    var shown = [];
    var selected = 0;

    function matches(item, query) {
      var text = (item.label + ' ' + (item.hint || '')).toLowerCase();
      var at = -1;
      for (var i = 0; i < query.length; i++) {
        at = text.indexOf(query.charAt(i), at + 1);
        if (at < 0) { return false; }
      }
      return true;
    }

    function score(item, query) {
      var label = item.label.toLowerCase();
      if (label.indexOf(query) === 0) { return 0; }
      if (label.indexOf(query) >= 0) { return 1; }
      return 2;
    }

    function draw() {
      var query = input.value.trim().toLowerCase();
      shown = items.filter(function (item) { return matches(item, query); });
      if (query) {
        shown = shown.map(function (item, i) { return { item: item, i: i, s: score(item, query) }; })
          .sort(function (a, b) { return a.s - b.s || a.i - b.i; })
          .map(function (entry) { return entry.item; });
      }
      shown = shown.slice(0, 50);
      selected = Math.min(selected, Math.max(shown.length - 1, 0));
      list.textContent = '';
      shown.forEach(function (item, i) {
        var li = document.createElement('li');
        li.id = 'palette-item-' + i;
        li.setAttribute('role', 'option');
        li.setAttribute('aria-selected', String(i === selected));
        var label = document.createElement('span');
        label.textContent = item.label;
        li.appendChild(label);
        if (item.hint) {
          var hint = document.createElement('span');
          hint.className = 'palette-hint';
          hint.textContent = item.hint;
          li.appendChild(hint);
        }
        li.addEventListener('mousemove', function () { select(i); });
        li.addEventListener('click', function () { choose(item); });
        list.appendChild(li);
      });
      none.hidden = shown.length > 0;
      input.setAttribute('aria-activedescendant', shown.length ? 'palette-item-' + selected : '');
    }

    function select(i) {
      if (i === selected) { return; }
      selected = i;
      $$('li', list).forEach(function (li, n) { li.setAttribute('aria-selected', String(n === i)); });
      input.setAttribute('aria-activedescendant', 'palette-item-' + i);
      var li = list.children[i];
      if (li) { li.scrollIntoView({ block: 'nearest' }); }
    }

    function choose(item) {
      if (!item) { return; }
      if (item.action === 'signout') {
        var form = $('#signout');
        if (form) { form.submit(); }
      } else if (item.action && item.action.indexOf('theme:') === 0) {
        setTheme(item.action.slice(6));
        dialog.close();
      } else if (item.href) {
        dialog.close();
        if (item.external) { window.open(item.href, '_blank', 'noopener'); } else { location.href = item.href; }
      }
    }

    function open() {
      input.value = '';
      selected = 0;
      draw();
      dialog.showModal();
      input.focus();
    }

    input.addEventListener('input', function () { selected = 0; draw(); });
    input.addEventListener('keydown', function (e) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        if (shown.length) { select((selected + 1) % shown.length); }
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        if (shown.length) { select((selected - 1 + shown.length) % shown.length); }
      } else if (e.key === 'Enter') {
        e.preventDefault();
        choose(shown[selected]);
      }
    });
    dialog.addEventListener('click', function (e) {
      if (e.target === dialog) { dialog.close(); } // the backdrop
    });

    $$('[data-palette-open]').forEach(function (button) {
      button.hidden = false;
      button.addEventListener('click', open);
    });
    var mac = /mac/i.test(navigator.platform || '');
    $$('[data-palette-key]').forEach(function (el) { el.textContent = mac ? '⌘K' : 'Ctrl+K'; });

    document.addEventListener('keydown', function (e) {
      if (e.key.toLowerCase() === 'k' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        if (dialog.open) { dialog.close(); } else { open(); }
      } else if (e.key === '/' && !dialog.open && !/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) {
        e.preventDefault();
        open();
      }
    });
  }

  // Live updates: one stream of events for the whole page.

  if (document.body.hasAttribute('data-live') && window.EventSource) {
    follow();
  }

  function follow() {
    var stream = document.body.getAttribute('data-stream') || '/api/v1/events/stream';
    var status = $('[data-live-status]');
    var source = null;
    var last = '';

    var showStatus = function (live) {
      if (!status) { return; }
      status.hidden = false;
      status.textContent = live ? 'Live' : 'Reconnecting';
      status.className = 'badge live-status ' + (live ? 'ok' : 'warn');
    };

    var open = function () {
      if (source) { return; }
      var from = last ? '/api/v1/events/stream?after=' + encodeURIComponent(last) : stream;
      source = new EventSource(from);
      source.addEventListener('open', function () { showStatus(true); });
      source.addEventListener('error', function () { showStatus(false); });
      source.addEventListener('moorage', function (message) {
        var event;
        try { event = JSON.parse(message.data); } catch (e) { return; }
        last = event.id || last;
        addToTimeline(event);
        updateAppRow(event);
        updateWaiting(event);
      });
    };

    var close = function () {
      if (source) {
        source.close();
        source = null;
      }
    };

    // Over plain HTTP a browser allows six connections to a server, and each page
    // following it holds one: a page out of sight lets its go, and catches up on
    // what it missed when it returns.
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) { close(); } else { open(); }
    });
    window.addEventListener('pagehide', close);
    if (!document.hidden) { open(); }
  }

  function addToTimeline(event) {
    var timeline = $('[data-timeline]');
    if (!timeline || event.transient) { return; }
    var only = timeline.getAttribute('data-app');
    if (only && event.app !== only) { return; }
    if (timeline.querySelector('[data-id="' + event.id + '"]')) { return; }

    var li = document.createElement('li');
    li.className = 'new';
    li.setAttribute('data-id', event.id);
    var dot = document.createElement('span');
    dot.className = 'dot ' + (event.level || '');
    dot.setAttribute('aria-hidden', 'true');
    li.appendChild(dot);
    var what = document.createElement('span');
    what.className = 'what';
    if (event.app) {
      var where = document.createElement('a');
      where.className = 'where';
      where.href = '/activity?app=' + encodeURIComponent(event.app);
      where.textContent = event.app;
      what.appendChild(where);
      what.appendChild(document.createTextNode(' '));
    }
    what.appendChild(document.createTextNode(event.summary));
    li.appendChild(what);
    var time = document.createElement('time');
    time.setAttribute('datetime', event.at);
    time.textContent = ago(new Date(event.at));
    time.title = new Date(event.at).toLocaleString();
    li.appendChild(time);
    timeline.insertBefore(li, timeline.firstChild);

    var empty = $('[data-timeline-empty]');
    if (empty) { empty.hidden = true; }
    timeline.hidden = false;
  }

  function updateAppRow(event) {
    if (!event.app) { return; }
    var apps = $('[data-apps]');
    if (!apps) { return; }
    var row = $('[data-app-row="' + event.app.replace(/[^a-z0-9-]/g, '') + '"]', apps);
    if (!row) {
      // A new app: the server knows how to show it.
      if (event.type === 'deployment.live' || event.type === 'release.created') { location.reload(); }
      return;
    }
    var badge = $('[data-app-status]', row);
    var version = event.data && event.data.version;
    var show = function (text, kind) {
      badge.textContent = text;
      badge.className = 'badge ' + kind;
    };
    switch (event.type) {
      case 'deployment.queued':
      case 'deployment.started':
        show('Deploying', 'info pulse');
        break;
      case 'deployment.live':
      case 'release.created':
        show('v' + version, 'ok');
        break;
      case 'deployment.failed':
        show('Deploy failed', 'fail');
        break;
    }
  }

  function updateWaiting(event) {
    var waiting = $('[data-waiting]');
    if (!waiting || !event.app) { return; }
    var badge = $('[data-waiting-badge]', waiting);
    var text = $('[data-waiting-text]', waiting);
    var set = function (kind, label, message) {
      badge.className = 'badge ' + kind;
      badge.textContent = label;
      text.textContent = message;
    };
    switch (event.type) {
      case 'deployment.queued':
      case 'deployment.started':
        set('info pulse', 'Deploying', event.app + ' is on its way.');
        break;
      case 'deployment.failed':
        set('fail', 'Failed', event.summary);
        break;
      case 'deployment.live':
        set('ok', 'Live', event.app + ' is live at ');
        var url = event.data && event.data.url;
        if (url) {
          var link = document.createElement('a');
          link.href = url;
          link.textContent = url;
          text.appendChild(link);
        }
        break;
    }
  }
})();
