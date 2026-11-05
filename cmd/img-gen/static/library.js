'use strict';

/* the Library — every render as browsable thumbnails, sets as folders,
   filterable by type/tag/text, a lightbox slideshow over any filter's
   context, set detail with file browsing + manifest, export + delete. */

(function () {
  const state = {
    jobs: [],
    setsById: new Map(),
    type: 'all',
    tags: new Set(),
    sort: 'new',
    search: '',
    fold: true,
  };
  const tagUniverse = new Map(); // tag label -> count

  async function refresh() {
    let jobs = [];
    try {
      jobs = await jsonFetch('/api/jobs');
    } catch (e) {
      appendLog(new Date(), 'failed', 'library: ' + e.message);
      renderFilterChips();
      return;
    }
    state.jobs = jobs;
    state.setsById.clear();
    for (const j of jobs) {
      if (j.mode === 'compose' && j.set_id) {
        state.setsById.set(j.set_id, j);
      }
    }
    renderFilterChips();
    render();
    const inFlight = (j) =>
      ['queued', 'enhancing', 'generating', 'validating'].includes(j.status);
    for (const j of jobs) {
      if (inFlight(j) && window.LibraryHooks.activeId() !== j.id) {
        window.LibraryHooks.onInflight && window.LibraryHooks.onInflight(j);
      }
    }
  }

  function jobTags(j) {
    const tags = [];
    if (j.mode) tags.push(j.mode);
    if (j.model_key) tags.push(j.model_key);
    if (j.style) tags.push(j.style);
    if (j.preset) tags.push('preset:' + j.preset);
    if (j.genre) tags.push(j.genre);
    if (j.size) tags.push(j.size);
    const v = (j.critic && j.critic.verdict) || (j.validate && j.validate.verdict);
    if (v === 'pass') tags.push('validated');
    if (j.status === 'failed') tags.push('failed');
    if (j.compose_parent) tags.push('child');
    return tags;
  }

  function computeTagUniverse() {
    tagUniverse.clear();
    for (const j of state.jobs) {
      for (const t of jobTags(j)) {
        tagUniverse.set(t, (tagUniverse.get(t) || 0) + 1);
      }
      if (j.files && j.files.length) {
        for (const f of j.files) {
          const kind = fileKind(f.name);
          if (kind) tagUniverse.set('kind:' + kind, (tagUniverse.get('kind:' + kind) || 0) + 1);
        }
      }
    }
  }

  function fileKind(name) {
    if (/\.gif$/i.test(name)) return 'gif';
    if (/insets\.json|frame\.json$/.test(name)) return 'frame';
    if (/atlas\.json$/.test(name)) return 'atlas';
    if (/palette\.json$/.test(name)) return 'palette';
    if (/manifest\.json$/.test(name)) return 'manifest';
    return null;
  }

  function renderFilterChips() {
    computeTagUniverse();
    const wrap = $('tag-chips');
    wrap.innerHTML = '';
    const ranked = Array.from(tagUniverse.entries())
      .filter(([t, c]) => c >= 2 && c < state.jobs.length)
      .sort((a, b) => b[1] - a[1])
      .slice(0, 18);
    for (const [t, c] of ranked) {
      const chip = el('button', 'chip');
      chip.textContent = t + ' ' + c;
      chip.setAttribute('aria-pressed', String(state.tags.has(t)));
      chip.onclick = () => {
        if (state.tags.has(t)) state.tags.delete(t);
        else state.tags.add(t);
        renderFilterChips();
        render();
      };
      wrap.appendChild(chip);
    }
  }

  function matches(j) {
    if (state.type === 'set' && !(j.mode === 'compose' && j.set_id)) return false;
    if (state.type === 'image' && (j.mode === 'compose' || j.status === 'failed')) return false;
    if (state.type === 'gif') {
      const setJ = j;
      const hasGif = (setJ.files || []).some(f => /\.gif$/i.test(f.name));
      if (!hasGif) return false;
    }
    if (state.type === 'failed' && j.status !== 'failed') return false;
    if (state.fold && state.type !== 'set' && j.compose_parent) return false;
    if (state.tags.size) {
      const jt = jobTags(j);
      for (const want of state.tags) if (!jt.includes(want)) return false;
    }
    if (state.search) {
      const hay = ((j.prompt || '') + ' ' + (j.model_key || '') + ' ' + (j.model || '') + ' ' +
        (j.style || '') + ' ' + (j.preset || '') + ' ' + (j.genre || '') + '').toLowerCase();
      if (!hay.includes(state.search.toLowerCase())) return false;
    }
    return true;
  }

  function render() {
    const grid = $('lib-grid');
    grid.innerHTML = '';
    let items = state.jobs.filter(matches).slice();
    const sort = state.sort;
    items.sort((a, b) => {
      const ta = new Date(a.created_at).getTime() || 0;
      const tb = new Date(b.created_at).getTime() || 0;
      if (sort === 'new') return tb - ta;
      if (sort === 'old') return ta - tb;
      if (sort === 'model') return String(a.model_key || a.model || '').localeCompare(b.model_key || b.model || '');
      return 0;
    });
    const n = items.length;
    $('lib-count').textContent = n + ' item' + (n === 1 ? '' : 's');
    $('lib-empty').hidden = n > 0;
    state._visible = items;
    window.Library._visible = items;
    let lastDay = '';
    for (const j of items) {
      const day = (j.created_at || '').slice(0, 10);
      if (day && day !== lastDay) {
        grid.appendChild(el('div', 'ghdr', dayTitle(day) + '  ·  ' + items
          .filter(x => (x.created_at || '').slice(0, 10) === day).length + ' items'));
        lastDay = day;
      }
      if (j.mode === 'compose' && j.set_id && state.fold) {
        grid.appendChild(setCard(j));
      } else if (j.mode === 'compose' && !state.fold) {
        for (const f of (j.files || [])) grid.appendChild(setFileCard(j, f));
      } else {
        grid.appendChild(genCard(j));
      }
    }
  }

  function dayTitle(day) {
    const today = new Date().toISOString().slice(0, 10);
    const yest = new Date(Date.now() - 864e5).toISOString().slice(0, 10);
    if (day === today) return 'Today';
    if (day === yest) return 'Yesterday';
    return day;
  }

  function genCard(j) {
    const card = el('div', 'card');
    card.setAttribute('role', 'listitem');
    card.dataset.id = j.id;
    const wrap = el('div', 'thumb-wrap');
    if (j.image_path || j.status === 'done') {
      const img = el('img');
      img.loading = 'lazy';
      img.src = imgUrl(j.id);
      img.onerror = () => { img.remove(); wrap.appendChild(el('div', 'placeholder', 'image gone')); };
      wrap.appendChild(img);
    } else if (j.status === 'failed' || j.status === 'cancelled') {
      wrap.appendChild(el('div', 'placeholder', j.status === 'failed' ? 'Failed' : 'Cancelled'));
    } else {
      wrap.appendChild(el('div', 'placeholder', j.status + '…'));
      const fill = el('div', 'progress-fill');
      wrap.appendChild(fill);
      wrap.dataset.fill = fill.dataset.id = j.id;
    }
    card.appendChild(wrap);
    const meta = el('div', 'card-meta');
    const title = el('div', 'card-title', (j.prompt || j.mode || j.id).slice(0, 70));
    meta.appendChild(title);
    const m = [j.model_key || (j.model || '').split('/').pop(), j.style, j.preset,
      j.seed != null ? 'seed ' + j.seed : null].filter(Boolean).join(' · ');
    meta.appendChild(el('div', 'm', m || j.status));
    card.appendChild(meta);
    card.onclick = () => openLightbox(j.id, null);
    return card;
  }

  function setCard(j) {
    const card = el('div', 'card set-card');
    card.setAttribute('role', 'listitem');
    const wrap = el('div', 'thumb-wrap');
    const pics = (j.files || []).filter(f => /\.(png|gif|webp)$/i.test(f.name));
    const primary = pick(j);
    if (primary) {
      const img = el('img');
      img.loading = 'lazy';
      img.src = setFileUrl(j.set_id, primary);
      img.onerror = () => { img.remove(); wrap.appendChild(el('div', 'placeholder', 'files gone')); };
      wrap.appendChild(img);
      const count = el('span', 'count-badge', (j.files || []).length + ' files');
      wrap.appendChild(count);
    } else {
      wrap.appendChild(el('div', 'placeholder', j.status + '…'));
    }
    card.appendChild(wrap);
    const kind = setKind(j);
    if (kind) wrap.appendChild(el('span', 'kind-chip', kind));
    const meta = el('div', 'card-meta');
    meta.appendChild(el('div', 'card-title', setName(j)));
    meta.appendChild(el('div', 'm', (kind || 'set') + ' · ' + (j.files || []).length + ' files'));
    card.appendChild(meta);
    card.onclick = () => openSetModal(j.set_id);
    return card;
  }

  function setFileCard(j, f) {
    const card = el('div', 'card');
    const wrap = el('div', 'thumb-wrap');
    if (/\.(png|gif|webp)$/i.test(f.name)) {
      const img = el('img');
      img.loading = 'lazy';
      img.src = setFileUrl(j.set_id, f.name);
      wrap.appendChild(img);
    } else {
      wrap.appendChild(el('div', 'placeholder', 'json'));
    }
    const kind = fileKind(f.name);
    if (kind) wrap.appendChild(el('span', 'kind-chip', kind));
    const meta = el('div', 'card-meta');
    meta.appendChild(el('div', 'card-title', f.name));
    meta.appendChild(el('div', 'm', 'in ' + setName(j)));
    card.appendChild(meta);
    card.onclick = () => {
      if (/\.(png|gif|webp)$/i.test(f.name)) {
        openSetFileLightbox(j.set_id, f.name);
      } else {
        openSetModal(j.set_id);
      }
    };
    return card;
  }

  function pick(j) {
    let primary = null;
    for (const f of (j.files || [])) {
      if (f.primary && /\.(png|gif|webp)$/i.test(f.name)) primary = f.name;
    }
    if (!primary) {
      for (const f of (j.files || [])) {
        if (/\.(png|gif|webp)$/i.test(f.name)) primary = f.name;
      }
    }
    return primary;
  }

  function setKind(j) {
    try {
      const spec = JSON.parse(j.compose_spec || '{}');
      if (spec.kind === 'gif') return 'gif';
      if (spec.kind === 'asset-set') {
        const a = (spec.assets || [])[0];
        return a ? a.kind : 'set';
      }
      return spec.kind;
    } catch (_) { return 'set'; }
  }

  function setName(j) {
    try {
      const spec = JSON.parse(j.compose_spec || '{}');
      return spec.name || spec.gif && spec.gif.name || j.set_id;
    } catch (_) { return j.set_id || j.id; }
  }

  /* ---------------- the lightbox + slideshow ---------------- */
  let lbList = [];   // [{src, caption, meta}]
  let lbIndex = 0;
  let slideTimer = null;

  function buildList(ids) {
    lbList = ids.map((it) => typeof it === 'string' ? { src: imgUrl(it), caption: it } : it);
  }

  function openLightbox(id, caption, list) {
    if (list) buildList(list);
    else buildList(state._visible.filter(j => !j.mode || j.mode !== 'compose').map(j => j.id));
    // the clicked one drives the index
    const idxHere = lbList.findIndex(item => item.src && item.src.includes(id));
    if (idxHere >= 0) lbIndex = idxHere;
    else lbList.unshift({ src: imgUrl(id), caption: id });
    showLightbox();
  }

  function openSetFileLightbox(setId, fileName, j) {
    const setJ = state.setsById.get(setId) || j;
    const pics = ((setJ && setJ.files) || []).filter(f => /\.(png|gif|webp)$/i.test(f.name)).map(f => f.name);
    const selfName = fileName;
    let set = setName(setJ || {});
    let srcs = pics.map(n => ({
      src: setFileUrl(setId, n),
      caption: n + ' · ' + set,
    }));
    buildList(srcs);
    lbIndex = Math.max(0, pics.indexOf(selfName));
    showLightbox();
  }

  function showLightbox() {
    const item = lbList[lbIndex];
    $('lightbox').hidden = false;
    $('lb-img').src = item.src;
    $('lb-meta').innerHTML = '';
    if (item.caption) $('lb-meta').appendChild(el('div', null, item.caption));
    const j = state._visible && state._visible.find(x => item.src.includes(x.id));
    if (j && item.src.includes(j.id)) {
      const tbl = el('div', 'meta-table');
      const meta = (({ prompt, model_key, model, style, preset, seed, size, created_at, status }) =>
        ({ prompt, model_key, model, style, preset, seed, size, created_at, status }))(j);
      for (const [k, v] of Object.entries(meta)) {
        if (v == null || v === '') continue;
        tbl.appendChild(metaRow(k, k === 'created_at' ? new Date(v).toLocaleString() : v));
      }
      const cr = (j.validate || j.critic);
      if (cr && cr.verdict) tbl.appendChild(metaRow('verdict', cr.verdict, cr.verdict));
      $('lb-meta').appendChild(tbl);
    }
    $('lightbox').dataset.src = item.src;
  }

  function step(delta) {
    if (!lbList.length) return;
    lbIndex = (lbIndex + delta + lbList.length) % lbList.length;
    showLightbox();
  }

  function closeLightbox() {
    $('lightbox').hidden = true;
    $('lb-img').src = '';
    stopSlide();
  }

  function toggleSlide() {
    if (slideTimer) { stopSlide(); return; }
    slideTimer = setInterval(() => step(1), 2500);
    $('lb-slide').textContent = 'Slideshow ■';
  }
  function stopSlide() {
    if (slideTimer) { clearInterval(slideTimer); slideTimer = null; }
    const btn = $('lb-slide');
    if (btn) btn.textContent = 'Slideshow ▶';
  }

  $('lb-prev').onclick = () => step(-1);
  $('lb-next').onclick = () => step(1);
  $('lb-close').onclick = closeLightbox;
  $('lb-slide').onclick = toggleSlide;
  $('lightbox').onclick = (e) => { if (e.target === $('lightbox')) closeLightbox(); };
  document.addEventListener('keydown', (e) => {
    if ($('lightbox').hidden) return;
    if (e.key === 'ArrowLeft') step(-1);
    else if (e.key === 'ArrowRight') step(1);
    else if (e.key === 'Escape') closeLightbox();
    else if (e.key === ' ') { e.preventDefault(); toggleSlide(); }
  });

  /* ---------------- set detail modal ---------------- */
  let currentSet = null;

  async function openSetModal(setId) {
    currentSet = setId;
    $('setdetail').hidden = false;
    $('set-title').textContent = 'Set ' + setId;
    $('set-files').innerHTML = 'loading…';
    $('set-manifest').innerHTML = '';
    try {
      const s = await jsonFetch('/api/v1/sets/' + setId);
      const files = (s.files || []);
      $('set-title').textContent = setName(s.job) + '  (' + files.length + ' files)';
      const wrap = $('set-files');
      wrap.innerHTML = '';
      for (const f of files) {
        const card = el('div', 'set-file' + (/\.(png|gif|webp)$/i.test(f.name) ? '' : ' json-chip'));
        if (/\.(png|gif|webp)$/i.test(f.name)) {
          const img = el('img');
          img.loading = 'lazy';
          img.src = setFileUrl(setId, f.name);
          img.onclick = () => openSetFileLightbox(setId, f.name, s.job);
          card.appendChild(img);
          card.appendChild(el('div', 'fname', f.name + (f.primary ? ' ·primary' : '')));
        } else {
          const a = el('span', 'jname', '📄 ' + f.name);
          a.onclick = () => showJson(setId, f.name);
          card.appendChild(a);
        }
        wrap.appendChild(card);
      }
      const mresp = await fetch(setFileUrl(setId, 'manifest.json'));
      if (mresp.ok) {
        const mt = (await mresp.text());
        const detail = el('details');
        detail.appendChild(el('summary', null, 'Manifest'));
        const pre = el('pre', null, pretty(mt));
        detail.appendChild(pre);
        $('set-manifest').appendChild(detail);
      }
      $('set-export').onclick = () => exportSetFolder(setId, s);
    } catch (e) {
      $('set-files').innerHTML = '';
      $('set-files').appendChild(el('div', 'log-empty', humanizeError(e)));
    }
  }

  function pretty(t) {
    try { return JSON.stringify(JSON.parse(t), null, 2); }
    catch (_) { return t; }
  }

  async function showJson(setId, name) {
    const t = await (await fetch(setFileUrl(setId, name))).text();
    $('set-manifest').innerHTML = '';
    const d = el('details');
    d.open = true;
    d.appendChild(el('summary', null, name));
    d.appendChild(el('pre', null, pretty(t)));
    $('set-manifest').appendChild(d);
  }

  async function exportSetFolder(setId, s) {
    const btn = $('set-export');
    const files = (s.files || []);
    let n = 0;
    for (const f of files) {
      try {
        const raw = await (await fetch(setFileUrl(setId, f.name))).blob();
        const url = URL.createObjectURL(raw);
        const a = el('a');
        a.href = url;
        a.download = f.name;
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => window.URL.revokeObjectURL(url), 2000);
        n++;
      } catch (_) { /* a file that failed keeps going */ }
    }
    appendLog(new Date(), 'info', 'export set: triggered ' + n + ' downloads for ' + setName(s.job));
  }

  $('set-close').onclick = () => { $('setdetail').hidden = true; currentSet = null; };
  $('setdetail').onclick = (e) => { if (e.target === $('setdetail')) $('setdetail').hidden = true; };

  function updateCardProgress(id, pct) {
    const cell = document.querySelector('.card[data-id="' + id + '"] .progress-fill');
    if (cell) cell.style.width = pct + '%';
  }

  window.Library = { refresh, openLightbox, openSetModal, openSetFileLightbox, updateCardProgress, _visible: [], state };
})();
