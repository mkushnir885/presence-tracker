(function () {
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

  function show(idx) {
    if (idx < 0) idx = 0;
    if (idx >= pages.length) idx = pages.length - 1;
    current = idx;
    pages.forEach((p, i) => p.hidden = i !== idx);
    if (label) label.textContent = (idx + 1) + ' / ' + pages.length;
    if (prevBtn) prevBtn.disabled = idx === 0;
    if (nextBtn) nextBtn.disabled = idx === pages.length - 1;
    const expectedHash = '#' + idx;
    if (window.location.hash !== expectedHash) {
      history.replaceState(null, '', window.location.pathname + window.location.search + expectedHash);
    }
  }

  if (prevBtn) prevBtn.addEventListener('click', () => show(current - 1));
  if (nextBtn) nextBtn.addEventListener('click', () => show(current + 1));
  window.addEventListener('hashchange', () => show(readIndexFromHash()));

  // Search-as-you-type filtering, in-memory.
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

    search.addEventListener('input', () => {
      const q = search.value.trim().toLowerCase();
      if (!q) { clearResults(); return; }
      const matches = names.filter(n => n.name.toLowerCase().includes(q));
      renderResults(matches);
    });

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

  // Rename buttons: prompt for new name and PATCH each-file individually.
  const filesQuery = (document.querySelector('.stats-page') || {}).dataset?.files || '';
  document.querySelectorAll('button[data-action="rename"]').forEach(btn => {
    btn.addEventListener('click', () => {
      const oldName = btn.dataset.displayName;
      if (!oldName) return;
      const newName = window.prompt('New display name for ' + oldName + ':', oldName);
      if (!newName || newName === oldName) return;
      const url = '/participants/' + encodeURIComponent(oldName) + '/display-name?'
        + filesQuery + (filesQuery ? '&' : '') + 'new=' + encodeURIComponent(newName);
      fetch(url, { method: 'PATCH' }).then(r => {
        if (r.ok) {
          window.location.reload();
        } else {
          r.text().then(t => alert('rename failed: ' + t));
        }
      });
    });
  });

  show(current);
})();
