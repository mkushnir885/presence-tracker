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

    search.addEventListener('blur', () => setTimeout(clearResults, 100));
  }

  show(current);
})();

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
  input.addEventListener('blur', () => setTimeout(clearResults, 100));
})();

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

  function open(trigger) {
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

  document.addEventListener('click', (ev) => {
    const trigger = ev.target.closest('[data-popover]');
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
