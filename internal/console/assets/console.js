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

  // A form that asks different things of different kinds asks only what the kind chosen needs.
  $$('[data-kind-choice]').forEach(function (choice) {
    var parts = choice.form.querySelectorAll('[data-kinds]');
    function show() {
      parts.forEach(function (part) {
        var on = part.getAttribute('data-kinds').split(' ').indexOf(choice.value) >= 0;
        part.hidden = !on;
        part.querySelectorAll('input').forEach(function (input) { input.disabled = !on; });
      });
    }
    choice.addEventListener('change', show);
    show();
  });
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
    $$('time[datetime]:not([data-clock])').forEach(function (el) {
      var date = new Date(el.getAttribute('datetime'));
      if (isNaN(date.getTime())) { return; }
      el.textContent = ago(date);
      el.title = date.toLocaleString();
    });
  }

  // A line of a log says when in this browser's own time.
  var clockFormat = window.Intl ? new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }) : null;
  var dayFormat = window.Intl ? new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric' }) : null;

  function clockText(date) {
    if (!clockFormat) { return date.toISOString(); }
    var today = new Date();
    var sameDay = date.getFullYear() === today.getFullYear() && date.getMonth() === today.getMonth() && date.getDate() === today.getDate();
    return (sameDay ? '' : dayFormat.format(date) + ' ') + clockFormat.format(date);
  }

  function showClock(el) {
    var date = new Date(el.getAttribute('datetime'));
    if (isNaN(date.getTime())) { return; }
    el.textContent = clockText(date);
    el.title = date.toLocaleString();
  }
  showTimes();
  setInterval(showTimes, 30000);

  // What filters a page needs no button to press.

  document.addEventListener('change', function (e) {
    var el = e.target;
    if (!el.hasAttribute || !el.hasAttribute('data-autosubmit') || !el.form) { return; }
    if (el.hasAttribute('data-range') && el.value === 'custom') {
      var custom = $('[data-range-custom]', el.form);
      if (custom) { custom.hidden = false; }
      return; // it needs its moments first
    }
    if (el.form.requestSubmit) { el.form.requestSubmit(); } else { el.form.submit(); }
  });

  function hideAutosubmitButtons() {
    $$('[data-autosubmit]').forEach(function (el) {
      if (!el.form) { return; }
      var button = $('button[data-autosubmit-button]', el.form) || $('[data-log-submit]', el.form) || (el.form.classList.contains('filters') ? $('button', el.form) : null);
      if (button) { button.hidden = true; }
    });
  }
  hideAutosubmitButtons();

  // A moment typed into a form means this browser's time.
  $$('[data-tz]').forEach(function (el) { el.value = String(new Date().getTimezoneOffset()); });

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

  var confirmDialog = $('#confirm');

  function ask(question, yes) {
    if (!confirmDialog || !confirmDialog.showModal) {
      if (window.confirm(question)) { yes(); }
      return;
    }
    $('#confirm-text', confirmDialog).textContent = question;
    var button = $('[data-confirm-yes]', confirmDialog);
    var no = $('[data-confirm-no]', confirmDialog);
    var close = function () { confirmDialog.close(); };
    var go = function () {
      close();
      yes();
    };
    button.onclick = go;
    no.onclick = close;
    confirmDialog.showModal();
    no.focus(); // what is safe first
  }

  document.addEventListener('submit', function (e) {
    var form = e.target;
    var question = form.getAttribute('data-confirm');
    if (question && !form.hasAttribute('data-confirmed')) {
      e.preventDefault();
      ask(question, function () {
        form.setAttribute('data-confirmed', '');
        if (form.requestSubmit) { form.requestSubmit(); } else { form.submit(); }
      });
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
    var kept = new URLSearchParams(location.search);
    kept.delete('done');
    kept.delete('released');
    history.replaceState(null, '', location.pathname + (kept.toString() ? '?' + kept.toString() : ''));
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

  // The logs page: the lines that were drawn, then those written since.

  function setUpLogs() {
    var box = $('[data-logs]');
    if (!box) { return; }
    var MAX_LINES = 5000;
    var grep = (box.getAttribute('data-log-grep') || '').toLowerCase();
    var count = $('[data-log-lines]');
    var status = $('[data-log-status]');
    var pauseButton = $('[data-log-pause]');
    var wrapButton = $('[data-log-wrap]');
    var jump = $('[data-log-jump]');
    var stream = box.getAttribute('data-logs-stream');
    var after = box.getAttribute('data-logs-after');
    var keepAtEnd = true;
    var paused = false;
    var held = [];
    var controller = null;

    function mark(text) {
      var el = text.parentNode ? text : null;
      if (!grep || !el) { return; }
      var value = el.textContent;
      var lower = value.toLowerCase();
      var at = lower.indexOf(grep);
      if (at < 0) { return; }
      el.textContent = '';
      var from = 0;
      while (at >= 0) {
        el.appendChild(document.createTextNode(value.slice(from, at)));
        var hit = document.createElement('mark');
        hit.textContent = value.slice(at, at + grep.length);
        el.appendChild(hit);
        from = at + grep.length;
        at = lower.indexOf(grep, from);
      }
      el.appendChild(document.createTextNode(value.slice(from)));
    }

    $$('.log-line', box).forEach(function (line) {
      var time = $('time', line);
      if (time) { showClock(time); }
      mark($('.log-text', line));
    });
    box.scrollTop = box.scrollHeight;

    function atEnd() { return box.scrollHeight - box.scrollTop - box.clientHeight < 32; }

    box.addEventListener('scroll', function () {
      keepAtEnd = atEnd();
      if (jump) { jump.hidden = keepAtEnd; }
    });
    if (jump) {
      jump.addEventListener('click', function () {
        keepAtEnd = true;
        box.scrollTop = box.scrollHeight;
        jump.hidden = true;
      });
    }

    if (wrapButton) {
      var WRAP_KEY = 'moorage-log-wrap';
      var setWrap = function (on) {
        box.classList.toggle('no-wrap', !on);
        wrapButton.setAttribute('aria-pressed', String(on));
      };
      var saved = null;
      try { saved = localStorage.getItem(WRAP_KEY); } catch (e) { saved = null; }
      setWrap(saved !== 'off');
      wrapButton.hidden = false;
      wrapButton.addEventListener('click', function () {
        var on = wrapButton.getAttribute('aria-pressed') !== 'true';
        setWrap(on);
        try { localStorage.setItem(WRAP_KEY, on ? 'on' : 'off'); } catch (e) { /* It lasts for this page, then. */ }
      });
    }

    function lineOf(entry) {
      var line = document.createElement('div');
      var source = entry.source || 'stdout';
      line.className = 'log-line s-' + source + (entry.level ? ' level-' + entry.level : '') + ' new';
      if (entry.message) { line.title = entry.text; }
      var time = document.createElement('time');
      time.setAttribute('data-clock', '');
      time.setAttribute('datetime', entry.time);
      showClock(time);
      line.appendChild(time);
      var who = document.createElement('span');
      who.className = 'log-who';
      who.textContent = source === 'build' || source === 'access' || source === 'events' ? source : entry.process + '.' + entry.index;
      line.appendChild(who);
      var text = document.createElement('span');
      text.className = 'log-text';
      text.textContent = (entry.message || entry.text || '').replace(/\x1b\[[0-9;]*[A-Za-z]/g, '');
      line.appendChild(text);
      line.appendChild(document.createTextNode('\n'));
      mark(text);
      return line;
    }

    function add(lines) {
      if (!lines.length) { return; }
      var empty = $('[data-logs-empty]', box);
      if (empty) { empty.remove(); }
      var fragment = document.createDocumentFragment();
      lines.forEach(function (entry) { fragment.appendChild(lineOf(entry)); });
      box.appendChild(fragment);
      while (box.childElementCount > MAX_LINES) { box.removeChild(box.firstElementChild); }
      if (count) { count.textContent = String(box.childElementCount); }
      if (keepAtEnd) { box.scrollTop = box.scrollHeight; }
    }

    function receive(entry) {
      after = entry.time;
      if (paused) {
        held.push(entry);
        if (held.length > MAX_LINES) { held.shift(); }
        pauseButton.textContent = 'Resume (' + held.length + ' new)';
      } else {
        pending.push(entry);
        if (!flushing) {
          flushing = true;
          requestAnimationFrame(flush);
        }
      }
    }

    var pending = [];
    var flushing = false;
    function flush() {
      flushing = false;
      add(pending);
      pending = [];
    }

    if (!stream || !window.fetch || !window.TextDecoder || !window.AbortController) { return; }

    if (pauseButton) {
      pauseButton.hidden = false;
      pauseButton.addEventListener('click', function () {
        paused = !paused;
        pauseButton.setAttribute('aria-pressed', String(paused));
        pauseButton.textContent = paused ? 'Resume' : 'Pause';
        show(paused ? 'Paused' : 'Live', paused ? '' : 'ok');
        if (!paused) {
          add(held);
          held = [];
        }
      });
    }

    function show(text, kind) {
      if (!status) { return; }
      status.hidden = !text;
      status.textContent = text;
      status.className = 'badge live-status ' + kind;
    }

    function open() {
      if (controller) { return; }
      var mine = new AbortController();
      controller = mine;
      var address = stream + (after ? '&after=' + encodeURIComponent(after) : '');
      fetch(address, { credentials: 'same-origin', signal: mine.signal }).then(function (res) {
        if (!res.ok || !res.body) { throw new Error('the logs could not be read'); }
        if (!paused) { show('Live', 'ok'); }
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
              if (text) { receive(JSON.parse(text)); }
            });
            return pump();
          });
        }
        return pump();
      }).catch(function () {
        // A stream may end for any reason: the next one starts from where this one got to.
      }).then(function () {
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
  setUpLogs();

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
    var onlyEnv = timeline.getAttribute('data-env') || (only ? 'production' : '');
    if (onlyEnv && event.env && event.env !== onlyEnv) { return; }
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
      var otherEnv = event.env && event.env !== 'production';
      where.href = '/apps/' + encodeURIComponent(event.app) + (otherEnv ? '?env=' + encodeURIComponent(event.env) : '');
      where.textContent = otherEnv ? event.app + ' · ' + event.env : event.app;
      what.appendChild(where);
      what.appendChild(document.createTextNode(' '));
    }
    var deployment = event.data && event.data.deployment;
    if (deployment) {
      var link = document.createElement('a');
      link.href = '/deployments/' + encodeURIComponent(deployment);
      link.textContent = event.summary;
      what.appendChild(link);
    } else {
      what.appendChild(document.createTextNode(event.summary));
    }
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

  // Charts are all there without this: it says what is under the pointer, or the arrow keys.

  function chartText(unit, v) {
    if (unit === 'rate') { return (v >= 100 ? Math.round(v) : v >= 10 ? v.toFixed(1) : v.toFixed(2).replace(/\.00$/, '')) + '/min'; }
    if (unit === 'ms') { return v >= 1000 ? (v / 1000).toFixed(2) + ' s' : v >= 10 ? Math.round(v) + ' ms' : v.toFixed(1) + ' ms'; }
    if (unit === 'percent') { return v >= 10 ? Math.round(v) + '%' : v.toFixed(1) + '%'; }
    if (unit === 'bytes') {
      var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
      var i = 0;
      while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
      return (v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)) + ' ' + units[i];
    }
    return String(v);
  }

  function chartAt(chart, data, index) {
    var svg = $('svg', chart);
    var cross = $('.chart-cross', chart);
    if (!cross) {
      cross = document.createElementNS('http://www.w3.org/2000/svg', 'line');
      cross.setAttribute('class', 'chart-cross');
      cross.setAttribute('y1', data.top);
      cross.setAttribute('y2', data.bottom);
      svg.appendChild(cross);
    }
    var at = data.times[index];
    var x = data.left + (data.right - data.left) * (at - data.from) / (data.to - data.from);
    cross.setAttribute('x1', x);
    cross.setAttribute('x2', x);
    var tip = $('.chart-tip', chart);
    if (!tip) {
      tip = document.createElement('div');
      tip.className = 'chart-tip';
      tip.setAttribute('role', 'status');
      chart.appendChild(tip);
    }
    tip.textContent = '';
    var when = document.createElement('time');
    when.textContent = new Date(at * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
    tip.appendChild(when);
    data.lines.forEach(function (line) {
      var value = line.values[index];
      if (value === null || value === undefined) { return; }
      var row = document.createElement('div');
      var key = document.createElement('span');
      key.className = 'chart-key s' + line.slot;
      var strong = document.createElement('strong');
      strong.textContent = chartText(data.unit, value);
      var name = document.createElement('span');
      name.className = 'name';
      name.textContent = line.name;
      row.appendChild(key);
      row.appendChild(strong);
      row.appendChild(name);
      tip.appendChild(row);
    });
    var box = chart.getBoundingClientRect();
    var left = x / data.width * box.width;
    var wide = tip.offsetWidth;
    tip.style.left = Math.max(0, left + 12 + wide > box.width ? left - 12 - wide : left + 12) + 'px';
    tip.style.top = '4px';
  }

  function chartLeave(chart) {
    var tip = $('.chart-tip', chart);
    if (tip) { tip.remove(); }
    var cross = $('.chart-cross', chart);
    if (cross) { cross.remove(); }
  }

  function chartData(chart) {
    try { return JSON.parse(chart.getAttribute('data-chart')); } catch (e) { return null; }
  }

  document.addEventListener('pointermove', function (e) {
    var chart = e.target.closest ? e.target.closest('.chart[data-chart]') : null;
    $$('.chart-tip').forEach(function (tip) {
      if (tip.parentNode !== chart) { chartLeave(tip.parentNode); }
    });
    if (!chart) { return; }
    var data = chartData(chart);
    if (!data || data.times.length === 0) { return; }
    var box = chart.getBoundingClientRect();
    var x = (e.clientX - box.left) / box.width * data.width;
    var at = data.from + (x - data.left) / (data.right - data.left) * (data.to - data.from);
    var best = 0;
    data.times.forEach(function (t, i) {
      if (Math.abs(t - at) < Math.abs(data.times[best] - at)) { best = i; }
    });
    chart.setAttribute('data-index', best);
    chartAt(chart, data, best);
  });

  document.addEventListener('keydown', function (e) {
    var chart = e.target.matches && e.target.matches('.chart[data-chart]') ? e.target : null;
    if (!chart) { return; }
    if (e.key === 'Escape') { chartLeave(chart); return; }
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') { return; }
    var data = chartData(chart);
    if (!data || data.times.length === 0) { return; }
    e.preventDefault();
    var index = parseInt(chart.getAttribute('data-index'), 10);
    if (isNaN(index)) { index = data.times.length; }
    index = Math.min(data.times.length - 1, Math.max(0, index + (e.key === 'ArrowLeft' ? -1 : 1)));
    chart.setAttribute('data-index', index);
    chartAt(chart, data, index);
  });

  document.addEventListener('focusout', function (e) {
    if (e.target.matches && e.target.matches('.chart[data-chart]')) { chartLeave(e.target); }
  });

  // The page that says how to deploy an app goes to the deployment when one starts.

  var waitingFor = $('[data-wait-deployment]');
  if (waitingFor && window.EventSource) { waitingFor.hidden = false; }

  function followNextDeployment(event) {
    if (!waitingFor || event.type !== 'deployment.queued' || event.app !== waitingFor.getAttribute('data-wait-deployment')) { return; }
    if (event.env && event.env !== (waitingFor.getAttribute('data-env') || 'production')) { return; }
    var id = event.data && event.data.deployment;
    if (id) { location.href = '/deployments/' + encodeURIComponent(id); }
  }

  if (document.body.hasAttribute('data-live') && window.EventSource) {
    follow();
  }
})();
