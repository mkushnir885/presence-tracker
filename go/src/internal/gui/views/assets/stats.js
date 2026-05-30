// Cross-meeting view: page through one participant at a time — prev/next, a
// URL hash that keeps the position bookmarkable, and a search box that jumps
// to a name with keyboard navigation.
(function setupPager() {
  const pagesEl = document.getElementById('stats-participant-pages');
  if (!pagesEl) return;

  const pages = Array.from(pagesEl.querySelectorAll('.participant-card'));
  if (pages.length === 0) return;

  const prevBtn = document.getElementById('stats-prev');
  const nextBtn = document.getElementById('stats-next');
  const label = document.getElementById('stats-pager-label');
  const search = document.getElementById('stats-search');
  const results = document.getElementById('stats-search-results');

  function readIndexFromHash() {
    const m = /^#(\d+)$/.exec(window.location.hash);
    if (m) {
      const n = parseInt(m[1], 10);
      if (!isNaN(n) && n >= 0 && n < pages.length) return n;
    }
    return 0;
  }

  let current = readIndexFromHash();

  const nameEl = document.getElementById('stats-current-name');
  const renameBtn = document.getElementById('stats-rename-btn');

  function show(idx) {
    if (idx < 0) idx = 0;
    if (idx >= pages.length) idx = pages.length - 1;
    current = idx;
    pages.forEach((p, i) => p.hidden = i !== idx);
    if (label) label.textContent = (idx + 1) + ' / ' + pages.length;
    if (prevBtn) prevBtn.disabled = idx === 0;
    if (nextBtn) nextBtn.disabled = idx === pages.length - 1;
    const name = pages[idx]?.dataset.displayName || '';
    if (nameEl) nameEl.textContent = name;
    if (renameBtn) renameBtn.dataset.displayName = name;
    const expectedHash = '#' + idx;
    if (window.location.hash !== expectedHash) {
      history.replaceState(null, '', window.location.pathname + window.location.search + expectedHash);
    }
  }

  if (prevBtn) prevBtn.addEventListener('click', () => show(current - 1));
  if (nextBtn) nextBtn.addEventListener('click', () => show(current + 1));
  window.addEventListener('hashchange', () => show(readIndexFromHash()));

  if (search && results) {
    const names = pages.map((p, i) => ({ name: p.dataset.displayName || '', index: i }));
    let activeRow = -1;

    function clearResults() {
      results.innerHTML = '';
      activeRow = -1;
    }

    function renderResults(matches) {
      results.innerHTML = '';
      matches.slice(0, 12).forEach((m, i) => {
        const li = document.createElement('li');
        li.textContent = m.name;
        li.dataset.index = m.index;
        if (i === 0) {
          li.classList.add('active');
          activeRow = 0;
        }
        li.addEventListener('mousedown', (ev) => {
          ev.preventDefault();
          show(m.index);
          search.value = '';
          clearResults();
        });
        results.appendChild(li);
      });
    }

    function update() {
      const q = search.value.trim().toLowerCase();
      const matches = q ? names.filter(n => n.name.toLowerCase().includes(q)) : names;
      renderResults(matches);
    }

    search.addEventListener('input', update);
    search.addEventListener('focus', update);

    search.addEventListener('keydown', (ev) => {
      const items = Array.from(results.children);
      if (ev.key === 'ArrowDown' && items.length) {
        ev.preventDefault();
        activeRow = Math.min(items.length - 1, activeRow + 1);
        items.forEach((it, i) => it.classList.toggle('active', i === activeRow));
      } else if (ev.key === 'ArrowUp' && items.length) {
        ev.preventDefault();
        activeRow = Math.max(0, activeRow - 1);
        items.forEach((it, i) => it.classList.toggle('active', i === activeRow));
      } else if (ev.key === 'Enter' && items.length) {
        ev.preventDefault();
        const chosen = items[activeRow] || items[0];
        const idx = parseInt(chosen.dataset.index, 10);
        if (!isNaN(idx)) show(idx);
        search.value = '';
        clearResults();
      } else if (ev.key === 'Escape') {
        search.value = '';
        clearResults();
      }
    });

    // Delay so a click (mousedown) on a result lands before blur clears it.
    search.addEventListener('blur', () => setTimeout(clearResults, 100));
  }

  show(current);
})();

// Rename a participant: PATCH the display name across exactly the loaded
// files (carried in data-files), then reload to show the result.
(function setupRename() {
  const filesQuery = (document.querySelector('.stats-page') || {}).dataset?.files || '';
  document.querySelectorAll('button[data-action="rename"]').forEach(btn => {
    btn.addEventListener('click', () => {
      const oldName = btn.dataset.displayName;
      if (!oldName) return;
      const newName = window.prompt('New display name for ' + oldName + ':', oldName);
      if (newName === null) return;
      const trimmed = newName.trim();
      if (!trimmed || trimmed === oldName) return;
      const url = '/participants/' + encodeURIComponent(oldName) + '/display-name?'
        + filesQuery + (filesQuery ? '&' : '') + 'new=' + encodeURIComponent(trimmed);
      fetch(url, { method: 'PATCH' }).then(r => {
        if (r.ok) {
          window.location.reload();
        } else {
          r.text().then(t => alert('rename failed: ' + t));
        }
      });
    });
  });
})();

// Single-meeting view: filter the participant rows by name, with a search
// dropdown that scrolls/selects a matching row.
(function setupMeetingFilter() {
  const input = document.getElementById('stats-filter-input');
  if (!input) return;
  const results = document.getElementById('stats-filter-results');
  const emptyEl = document.getElementById('stats-filter-empty');
  const rows = Array.from(document.querySelectorAll('.participant-list .participant-row'));
  if (rows.length === 0) return;

  const names = rows.map(r => (r.querySelector('.participant-name')?.textContent || ''));
  let activeRow = -1;

  function clearResults() {
    if (results) results.innerHTML = '';
    activeRow = -1;
  }

  function applyFilter() {
    const q = input.value.trim().toLowerCase();
    let lastVisible = null;
    let visibleCount = 0;
    rows.forEach((r, i) => {
      const visible = !q || names[i].toLowerCase().includes(q);
      r.hidden = !visible;
      r.style.borderBottom = '';
      if (visible) {
        lastVisible = r;
        visibleCount++;
      }
    });
    if (lastVisible) lastVisible.style.borderBottom = 'none';
    if (emptyEl) emptyEl.hidden = visibleCount > 0;
  }

  function renderResults() {
    if (!results) return;
    const q = input.value.trim().toLowerCase();
    results.innerHTML = '';
    activeRow = -1;
    if (!q) return;
    const matches = names
      .map((name, idx) => ({ name, idx }))
      .filter(m => m.name.toLowerCase().includes(q))
      .slice(0, 12);
    matches.forEach((m, i) => {
      const li = document.createElement('li');
      li.textContent = m.name;
      li.dataset.index = m.idx;
      if (i === 0) {
        li.classList.add('active');
        activeRow = 0;
      }
      li.addEventListener('mousedown', (ev) => {
        ev.preventDefault();
        input.value = m.name;
        applyFilter();
        clearResults();
      });
      results.appendChild(li);
    });
  }

  input.addEventListener('input', () => { applyFilter(); renderResults(); });
  input.addEventListener('keydown', (ev) => {
    const items = results ? Array.from(results.children) : [];
    if (ev.key === 'ArrowDown' && items.length) {
      ev.preventDefault();
      activeRow = Math.min(items.length - 1, activeRow + 1);
      items.forEach((it, i) => it.classList.toggle('active', i === activeRow));
    } else if (ev.key === 'ArrowUp' && items.length) {
      ev.preventDefault();
      activeRow = Math.max(0, activeRow - 1);
      items.forEach((it, i) => it.classList.toggle('active', i === activeRow));
    } else if (ev.key === 'Enter' && items.length) {
      ev.preventDefault();
      const chosen = items[activeRow] || items[0];
      input.value = chosen.textContent;
      applyFilter();
      clearResults();
    } else if (ev.key === 'Escape') {
      input.value = '';
      applyFilter();
      clearResults();
    }
  });
  // Delay so a click (mousedown) on a result lands before blur clears it.
  input.addEventListener('blur', () => setTimeout(clearResults, 100));
})();

// Click-to-open detail popovers for the timeline: markers (challenges),
// meetings, and grouped-challenge dots. Only one is open at a time.
(function setupPopovers() {
  let popoverEl = null;
  let triggerEl = null;

  function close() {
    if (popoverEl) {
      popoverEl.remove();
      popoverEl = null;
    }
    if (triggerEl) {
      triggerEl.removeAttribute('aria-expanded');
      triggerEl = null;
    }
  }

  // Center the popover under the trigger, clamp it to the viewport, and flip
  // it above the trigger when it would overflow the bottom edge.
  function position(trigger, pop) {
    const r = trigger.getBoundingClientRect();
    const ph = pop.offsetHeight;
    const pw = pop.offsetWidth;
    const margin = 8;
    const vw = document.documentElement.clientWidth;
    const vh = document.documentElement.clientHeight;

    let left = r.left + r.width / 2 - pw / 2;
    if (left < margin) left = margin;
    if (left + pw > vw - margin) left = vw - margin - pw;

    let top = r.bottom + 8;
    if (top + ph > vh - margin && r.top - ph - 8 > margin) {
      top = r.top - ph - 8;
    }
    pop.style.left = (left + window.scrollX) + 'px';
    pop.style.top = (top + window.scrollY) + 'px';
  }

  // Pick a renderer from whichever data-popover-* attribute the trigger
  // carries; plain data-popover is the text-only fallback.
  function open(trigger) {
    const markerJSON = trigger.getAttribute('data-popover-marker');
    if (markerJSON) {
      openMarker(trigger, markerJSON);
      return;
    }
    const meetingJSON = trigger.getAttribute('data-popover-meeting');
    if (meetingJSON) {
      openMeeting(trigger, meetingJSON);
      return;
    }
    const challengeJSON = trigger.getAttribute('data-popover-challenge');
    if (challengeJSON) {
      openChallenge(trigger, challengeJSON);
      return;
    }
    const text = trigger.getAttribute('data-popover');
    if (!text) return;
    close();
    const pop = document.createElement('div');
    pop.className = 'stat-popover';
    pop.setAttribute('role', 'dialog');
    pop.textContent = text;
    document.body.appendChild(pop);
    position(trigger, pop);
    popoverEl = pop;
    triggerEl = trigger;
    trigger.setAttribute('aria-expanded', 'true');
  }

  function openChallenge(trigger, json) {
    let data;
    try { data = JSON.parse(json); } catch (_) { return; }
    close();
    const pop = document.createElement('div');
    pop.className = 'stat-popover stat-popover-challenge';
    pop.setAttribute('role', 'dialog');
    const rows = Array.isArray(data.rows) ? data.rows : [];
    for (const row of rows) {
      if (!row) continue;
      const line = document.createElement('div');
      line.className = 'challenge-row';
      if (row.text) line.appendChild(document.createTextNode(row.text));
      if (row.tipLabel) {
        const tip = document.createElement('span');
        tip.className = 'challenge-tip';
        tip.setAttribute('data-tooltip', row.tipText || '');
        tip.setAttribute('tabindex', '0');
        tip.textContent = row.tipLabel;
        line.appendChild(tip);
      }
      pop.appendChild(line);
    }
    document.body.appendChild(pop);
    position(trigger, pop);
    popoverEl = pop;
    triggerEl = trigger;
    trigger.setAttribute('aria-expanded', 'true');
  }

  function openMeeting(trigger, json) {
    let data;
    try { data = JSON.parse(json); } catch (_) { return; }
    close();
    const pop = document.createElement('div');
    pop.className = 'stat-popover stat-popover-meeting';
    pop.setAttribute('role', 'dialog');
    const rows = Array.isArray(data.rows) ? data.rows : [];
    for (const row of rows) {
      if (!row) continue;
      if (row.type === 'hint') {
        const hint = document.createElement('div');
        hint.className = 'meeting-hint';
        hint.textContent = row.value || '';
        pop.appendChild(hint);
        continue;
      }
      const line = document.createElement('div');
      line.className = 'meeting-row';
      const k = document.createElement('strong');
      k.textContent = (row.label || '') + ': ';
      line.appendChild(k);
      line.appendChild(document.createTextNode(row.value || ''));
      pop.appendChild(line);
    }
    document.body.appendChild(pop);
    position(trigger, pop);
    popoverEl = pop;
    triggerEl = trigger;
    trigger.setAttribute('aria-expanded', 'true');
  }

  function openMarker(trigger, json) {
    let data;
    try {
      data = JSON.parse(json);
    } catch (_) {
      return;
    }
    close();
    const pop = document.createElement('div');
    pop.className = 'stat-popover stat-popover-marker';
    pop.setAttribute('role', 'dialog');

    const title = document.createElement('div');
    title.className = 'marker-title';
    if (data.chip) {
      const chip = document.createElement('code');
      chip.className = 'marker-chip marker-chip-' + data.chip.shape;
      const glyph = document.createElement('span');
      glyph.className = 'marker-chip-glyph';
      chip.appendChild(glyph);
      chip.appendChild(document.createTextNode(data.chip.label || ''));
      title.appendChild(chip);
    }
    const when = document.createElement('span');
    when.className = 'marker-when';
    when.textContent = ' ' + (data.whenPrefix || '') + ' ' + (data.when || '');
    title.appendChild(when);
    pop.appendChild(title);

    if (data.state && data.state.label) {
      const stateLine = document.createElement('div');
      stateLine.className = 'marker-state-line';
      const stateLabel = document.createElement('span');
      stateLabel.className = 'marker-state marker-state-' + (data.state.color || '');
      stateLabel.textContent = data.state.label;
      stateLine.appendChild(stateLabel);
      if (data.state.hint) {
        const hint = document.createElement('span');
        hint.className = 'marker-hint';
        hint.textContent = data.state.hint;
        stateLine.appendChild(hint);
      }
      pop.appendChild(stateLine);
    }

    if (data.reason && data.reason.value) {
      const reasonLine = document.createElement('div');
      reasonLine.className = 'marker-reason-line';
      reasonLine.appendChild(makeKV(data.reason.label, data.reason.value));
      pop.appendChild(reasonLine);
    }

    const body = document.createElement('div');
    body.className = 'marker-body';
    let bodyHasContent = false;

    if (data.prompt && data.prompt.value) {
      body.appendChild(makeKV(data.prompt.label, data.prompt.value));
      bodyHasContent = true;
    }
    if (Array.isArray(data.extras)) {
      for (const e of data.extras) {
        if (e && e.value) {
          body.appendChild(makeKV(e.label, e.value));
          bodyHasContent = true;
        }
      }
    }
    if (data.answer && data.answer.value) {
      body.appendChild(makeKV(data.answer.label, data.answer.value));
      bodyHasContent = true;
    }
    if (!bodyHasContent && data.missing && data.missing.value) {
      const notice = document.createElement('div');
      notice.className = 'marker-missing';
      notice.textContent = data.missing.value;
      body.appendChild(notice);
      bodyHasContent = true;
    }
    if (bodyHasContent) {
      pop.appendChild(document.createElement('hr'));
      pop.appendChild(body);
    }

    if (data.submitted && data.submitted.value) {
      pop.appendChild(document.createElement('hr'));
      const submitted = document.createElement('div');
      submitted.className = 'marker-submitted';
      submitted.appendChild(makeKV(data.submitted.label, data.submitted.value));
      pop.appendChild(submitted);
    }

    document.body.appendChild(pop);
    position(trigger, pop);
    popoverEl = pop;
    triggerEl = trigger;
    trigger.setAttribute('aria-expanded', 'true');
  }

  function makeKV(label, value) {
    const line = document.createElement('div');
    const k = document.createElement('strong');
    k.textContent = (label || '') + ': ';
    line.appendChild(k);
    line.appendChild(document.createTextNode(value));
    return line;
  }

  document.addEventListener('click', (ev) => {
    const trigger = ev.target.closest('[data-popover], [data-popover-marker], [data-popover-meeting], [data-popover-challenge]');
    if (trigger) {
      ev.preventDefault();
      ev.stopPropagation();
      if (triggerEl === trigger) close();
      else open(trigger);
      return;
    }
    if (popoverEl && !popoverEl.contains(ev.target)) close();
  });

  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') close();
  });

  window.addEventListener('resize', close);
})();
