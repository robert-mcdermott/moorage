// What scripts add to the console's pages, all of which work without them: the
// theme, copying commands, times that stay true, the command palette, a deployment's
// log as it is written, and live updates over one stream of events.
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

  function addCopyButtons() {
    $$('.command').forEach(function (block) {
      var pre = $('pre', block);
      if (!pre || !navigator.clipboard || $('button', block)) { return; }
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
  }
  addCopyButtons();

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

  // Filtering the apps, as they are typed. The command palette finds one too; this
  // keeps the list in view.

  var appFilter = $('[data-filter-apps]');

  function filterApps() {
    if (!appFilter) { return; }
    var query = appFilter.value.trim().toLowerCase();
    $$('[data-app-row]').forEach(function (row) {
      row.hidden = query !== '' && row.getAttribute('data-app-row').indexOf(query) < 0;
    });
  }

  if (appFilter) {
    appFilter.hidden = false;
    appFilter.addEventListener('input', filterApps);
  }

  // Forms that take a while say so, and those that matter ask first. They are found
  // from the document: the parts of a page that are drawn again bring new ones.

  document.addEventListener('submit', function (e) {
    var form = e.target;
    var question = form.getAttribute('data-confirm');
    if (question && !window.confirm(question)) {
      e.preventDefault();
      return;
    }
    var busy = form.getAttribute('data-busy');
    var button = $('button[type="submit"]', form);
    if (busy && button) {
      button.textContent = busy;
      button.disabled = true;
      form.setAttribute('data-submitted', '');
    }
  });

  // A page the browser kept, on the way back, is as it was left: buttons that say
  // they are busy, and a stream it let go of.
  window.addEventListener('pageshow', function (e) {
    if (e.persisted) { location.reload(); }
  });

  // What was done is said once, not again on reloading.
  if (location.search.indexOf('done=') >= 0 && window.history.replaceState) {
    history.replaceState(null, '', location.pathname);
  }

  // The parts of a page that change: the server draws them again, so that there is one
  // place that knows how they look.

  var refreshTimer = null;
  var pollTimer = null;
  var refreshing = false;
  var refreshAgain = false;

  // A part someone is working in stays as it is.
  function inUse(part) {
    if ($('[data-submitted]', part)) { return true; }
    var active = document.activeElement;
    if (active && part.contains(active) && /^(INPUT|SELECT|TEXTAREA)$/.test(active.tagName)) { return true; }
    return $$('input', part).some(function (input) {
      if (input.type === 'hidden') { return false; }
      return input.type === 'checkbox' ? input.checked !== input.defaultChecked : input.value !== input.defaultValue;
    });
  }

  function refresh() {
    if (refreshing) {
      refreshAgain = true;
      return;
    }
    refreshing = true;
    fetch(location.href, { credentials: 'same-origin' }).then(function (res) {
      return res.ok ? res.text() : '';
    }).then(function (html) {
      var drawn = new DOMParser().parseFromString(html, 'text/html');
      $$('[data-refresh]').forEach(function (part) {
        var fresh = drawn.querySelector('[data-refresh="' + part.getAttribute('data-refresh') + '"]');
        if (fresh && !inUse(part)) { part.replaceWith(document.adoptNode(fresh)); }
      });
      addCopyButtons();
      showTimes();
      filterApps();
    }).catch(function () {
      // The stream or the next poll will bring it.
    }).then(function () {
      refreshing = false;
      if (refreshAgain) {
        refreshAgain = false;
        refreshSoon();
      }
      pollIfNeeded();
    });
  }

  function refreshSoon() {
    if (!window.fetch || !window.DOMParser || !$('[data-refresh]')) { return; }
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(refresh, 250);
  }

  // A deployment under way changes more often than it has events to tell.
  function pollIfNeeded() {
    clearTimeout(pollTimer);
    if ($('[data-poll]') && !document.hidden) { pollTimer = setTimeout(refreshSoon, 2500); }
  }
  pollIfNeeded();

  function refreshFor(event) {
    var main = $('[data-refresh-on]');
    if (!main || !event.app) { return; }
    var on = main.getAttribute('data-refresh-on');
    if (on === '*' || on === event.app) { refreshSoon(); }
  }

  // The log of a deployment under way, as it is written.

  function followLog() {
    var box = $('[data-log-follow]');
    if (!box || !window.fetch || !window.TextDecoder || !window.AbortController) { return; }
    var id = box.getAttribute('data-log-id');
    var seen = Number(box.getAttribute('data-log-count')) || 0;
    var status = $('[data-log-status]');
    var controller = null;
    var ended = false;
    var keepAtEnd = true;

    box.addEventListener('scroll', function () {
      keepAtEnd = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
    });
    box.scrollTop = box.scrollHeight;

    function show(text, kind) {
      if (!status) { return; }
      status.hidden = !text;
      status.textContent = text;
      status.className = 'badge live-status ' + kind;
    }

    function add(line) {
      seen++;
      var span = document.createElement('span');
      span.className = 'line s-' + line.stream;
      span.title = line.time;
      span.textContent = line.text.replace(/\x1b\[[0-9;]*[A-Za-z]/g, '') + '\n';
      box.appendChild(span);
      if (keepAtEnd) { box.scrollTop = box.scrollHeight; }
      // A note of Moorage's own may begin a stage.
      if (line.stream === 'M' && /^[▸✔✘]/.test(line.text)) { refreshSoon(); }
    }

    function open() {
      if (controller || ended) { return; }
      var mine = new AbortController();
      controller = mine;
      fetch('/api/v1/deployments/' + encodeURIComponent(id) + '/log?follow=true&skip=' + seen, {
        credentials: 'same-origin', signal: mine.signal
      }).then(function (res) {
        if (!res.ok || !res.body) { throw new Error('the log could not be read'); }
        show('Live', 'ok');
        var reader = res.body.getReader();
        var decoder = new TextDecoder();
        var rest = '';
        function pump() {
          return reader.read().then(function (part) {
            if (part.done) { return null; }
            rest += decoder.decode(part.value, { stream: true });
            var lines = rest.split('\n');
            rest = lines.pop();
            lines.forEach(function (text) {
              if (text) { add(JSON.parse(text)); }
            });
            return pump();
          });
        }
        return pump();
      }).then(function () {
        ended = true;
        controller = null;
        show('', '');
        refreshSoon();
      }).catch(function () {
        controller = null;
        if (!mine.signal.aborted) {
          show('Reconnecting', 'warn');
          setTimeout(open, 2000);
        }
      });
    }

    function close() {
      if (controller) { controller.abort(); }
    }

    document.addEventListener('visibilitychange', function () {
      if (document.hidden) { close(); } else { open(); }
    });
    window.addEventListener('pagehide', close);
    if (!document.hidden) { open(); }
  }
  followLog();

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

  // At the end of this script, once what it hears of has been set up.

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
        refreshFor(event);
        updateWaiting(event);
        followNextDeployment(event);
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
      if (document.hidden) {
        close();
      } else {
        open();
        refreshSoon();
        pollIfNeeded();
      }
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
    if (event.app && !only) {
      var where = document.createElement('a');
      where.className = 'where';
      where.href = '/apps/' + encodeURIComponent(event.app);
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

  // The page that says how to deploy an app goes to the deployment when one starts.

  var waitingFor = $('[data-wait-deployment]');
  if (waitingFor && window.EventSource) { waitingFor.hidden = false; }

  function followNextDeployment(event) {
    if (!waitingFor || event.type !== 'deployment.queued' || event.app !== waitingFor.getAttribute('data-wait-deployment')) { return; }
    var id = event.data && event.data.deployment;
    if (id) { location.href = '/deployments/' + encodeURIComponent(id); }
  }

  if (document.body.hasAttribute('data-live') && window.EventSource) {
    follow();
  }
})();
