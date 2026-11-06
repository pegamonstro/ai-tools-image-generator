'use strict';

/* img-gen — Bikini Club (by pegamonstro)
   Workspaces: Library | Compose | Studio. Zero build, zero deps. */

let genres = null;
let models = null;
let presets = null;

/* ---------------- helpers ---------------- */
function $(id) { return document.getElementById(id); }
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}
function envelopeMessage(text) {
  try {
    const j = JSON.parse(text);
    if (j && j.error) return j.error.message || j.error.code || text;
  } catch (_) { /* plain text */ }
  return text;
}
async function jsonFetch(url, opts) {
  const r = await fetch(url, opts);
  const text = await r.text().catch(() => '');
  if (!r.ok) throw new Error(envelopeMessage(text) || r.statusText);
  return text ? JSON.parse(text) : {};
}
const POSTJSON = { 'Content-Type': 'application/json' };
function humanizeError(e) {
  const raw = (e && e.message ? e.message : String(e)) || 'Something went wrong.';
  const line = raw.split('\n')[0].trim();
  const lower = line.toLowerCase();
  if (line.includes('409') && lower.includes('already in progress'))
    return 'Generator busy — this runs when the current job finishes.';
  if (/broken pipe|eof|connection refused|dial tcp|no such host/.test(lower))
    return 'Generator unreachable — check that the sidecar is running.';
  if (line.includes('429') && line.toLowerCase().includes('memory'))
    return 'Engine memory busy — the batch retries; wait a minute and resubmit.';
  if (line.includes('429')) return 'Inference queue full.';
  if (line.includes('404')) return 'Endpoint unavailable.';
  if (/50[23]/.test(line) || lower.includes('internal server error'))
    return 'The engine is busy or erroring — retry shortly.';
  const scrubbed = line
    .replace(/https?:\/\/\S+/g, '…')
    .replace(/\b\d{1,3}(\.\d{1,3}){3}\b/g, '…')
    .replace(/\/Users\/\S+/g, '…');
  return scrubbed.length > 120 ? scrubbed.slice(0, 117) + '…' : scrubbed;
}
function setStatus(msg, cls, raw) { showStatus(msg, cls, raw); }
function setConn(state, cls) {
  const elc = $('conn');
  elc.textContent = state;
  elc.className = 'conn ' + cls;
}

/* ---------------- log ---------------- */
function fmtTime(ts) {
  const d = ts instanceof Date ? ts : new Date(ts);
  return d.toLocaleTimeString('en-GB', { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0');
}
function appendLog(ts, status, msg) {
  const log = $('log');
  if (!log) return;
  const empty = log.querySelector('.log-empty');
  if (empty) empty.remove();
  const line = el('div', 'log-line ' + (status || 'info'));
  line.appendChild(el('span', 'log-ts', fmtTime(ts)));
  line.appendChild(el('span', 'log-msg', msg));
  log.appendChild(line);
  log.scrollTop = log.scrollHeight;
}
function clearLog() {
  $('log').innerHTML = '<div class="log-empty">No activity yet.</div>';
}
function showStatus(msg, cls, raw) {
  const elz = $('status');
  if (msg) {
    elz.hidden = false;
    elz.textContent = msg;
    elz.className = 'status ' + (cls || '');
  } else {
    elz.hidden = true;
    elz.textContent = '';
    elz.className = 'status';
  }
  if (raw) appendLog(new Date(), cls === 'err' ? 'failed' : 'info', raw);
}

/* ---------------- view routing ---------------- */
function setView(name) {
  for (const v of ['fast', 'library', 'compose', 'studio']) {
    const elv = $('view-' + v);
    if (elv) elv.hidden = v !== name;
  }
  document.querySelectorAll('.tab').forEach(t =>
    t.setAttribute('aria-selected', String(t.dataset.view === name)));
  location.hash = name;
  if (name === 'fast' && window.Fast && window.Fast.render) window.Fast.render();
  if (window.Fast && window.Fast.renderStrip) window.Fast.renderStrip();
}

/* ---------------- shared image helpers ---------------- */
function imgUrl(id) { return '/api/images/' + id + '.png'; }

let __libRefreshTimer = null;
function refreshLibrarySoon() {
  clearTimeout(__libRefreshTimer);
  __libRefreshTimer = setTimeout(() => {
    if (window.Library && window.Library.refresh) window.Library.refresh().catch(() => {});
  }, 1200);
}
function setFileUrl(setId, name) { return '/api/v1/sets/' + setId + '/files/' + encodeURIComponent(name); }
function readFileAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(r.result);
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}
function stripDataURL(d) { return d.split(',')[1]; }
async function blobToBase64(blob) {
  return stripDataURL(await readFileAsDataURL(blob));
}
function metaRow(k, v, cls) {
  const dt = el('span', 'k', k);
  const dv = el('span', 'v' + (cls ? ' ' + cls : ''), String(v));
  const row = document.createElement('div');
  row.style.display = 'contents';
  row.appendChild(dt); row.appendChild(dv);
  return row;
}
window.startLibrary = () => {};
window.onJobDone = () => {};

/* ---------------- the composer: state + catalog rendering ---------------- */
const EDIT_SIZES = ['512x512', '768x512', '1024x576', '1024x1024'];
let uploaded = { edit: null, inpaint: null, blend: [], upscale: null, pose: null };
let activeJobId = null;
let lockSeed = false;

function currentMode() {
  const active = document.querySelector('#mode-grid .chip[aria-pressed="true"]');
  return active ? active.dataset.mode : 'generate';
}
function setMode(m) {
  document.querySelectorAll('#mode-grid .chip').forEach(c =>
    c.setAttribute('aria-pressed', String(c.dataset.mode === m)));
  renderModeVisibility();
}
function renderModeVisibility() {
  const m = currentMode();
  $('gen-controls').hidden = m !== 'generate';
  $('edit-controls').hidden = m !== 'edit';
  $('inpaint-controls').hidden = m !== 'inpaint';
  $('outpaint-controls').hidden = m !== 'outpaint';
  $('blend-controls').hidden = m !== 'blend';
  $('upscale-controls').hidden = m !== 'upscale';
  $('pose-controls').hidden = m !== 'pose';
  $('enhance').closest('.toggle').hidden = m === 'upscale';
  const genLike = (m === 'generate' || m === 'edit' || m === 'pose');
  document.querySelectorAll('#lora-list select').forEach(x => { x.disabled = !genLike; });
  $('add-lora').disabled = !genLike;
  $('lora-raw').disabled = !genLike;
  $('sampling-controls').hidden = !genLike;
}
function populateSizes(sel, sizes) {
  sel.innerHTML = '';
  for (const s of sizes) {
    const o = el('option'); o.value = s; o.textContent = s;
    sel.appendChild(o);
  }
}
function renderGenreSelect() {
  const sel = $('genre');
  sel.innerHTML = '';
  for (const [key, g] of Object.entries(genres.genres)) {
    const o = el('option'); o.value = key; o.textContent = g.label;
    sel.appendChild(o);
  }
  sel.onchange = renderForm;
  renderForm();
}
function renderStyleSelect() {
  const sel = $('style');
  sel.innerHTML = '';
  sel.appendChild(Object.assign(el('option'), { value: '', textContent: 'None' }));
  for (const s of (genres.styles || [])) {
    const o = el('option'); o.value = s.key; o.textContent = s.label;
    sel.appendChild(o);
  }
}
function renderModelSelect() {
  const sel = $('model');
  sel.innerHTML = '';
  const none = Object.assign(el('option'), { value: '', textContent: 'Default (' + (window.__serverDefault || 'engine default') + ')' });
  sel.appendChild(none);
  for (const m of models.models) {
    const o = el('option'); o.value = m.key; o.textContent = m.label;
    sel.appendChild(o);
  }
  const rawM = document.createElement('optgroup');
  rawM.label = 'raw values';
  sel.appendChild(rawM);
  const asSel = $('as-model');
  if (asSel) {
    asSel.innerHTML = '';
    const nA = Object.assign(el('option'), { value: '', textContent: 'Default' });
    asSel.appendChild(nA);
    for (const m of models.models) {
      const o = el('option'); o.value = m.key; o.textContent = m.label;
      asSel.appendChild(o);
    }
  }
}
function renderPresetSelect() {
  const sel = $('preset');
  sel.innerHTML = '';
  sel.appendChild(Object.assign(el('option'), { value: '', textContent: 'None' }));
  for (const p of presets.presets) {
    const o = el('option'); o.value = p.key; o.textContent = p.label;
    sel.appendChild(o);
  }
  if (presets.presets.find(p => p.key === 'photon-noir')) {
    sel.value = 'photon-noir';
  }
}
function renderForm() {
  const m = currentMode();
  if (m !== 'generate') return;
  const key = $('genre').value;
  const g = (genres.genres || {})[key];
  if (!g) return;
  const sizeSel = $('size');
  sizeSel.innerHTML = '';
  for (const s of (g.sizes || [])) {
    const o = el('option'); o.value = s; o.textContent = s;
    sizeSel.appendChild(o);
  }
  const wrap = $('fields');
  wrap.innerHTML = '';
  for (const f of (g.fields || [])) {
    const lab = el('label', 'ctrl', f.label + (f.required ? '' : ''));
    if (f.type === 'text' || f.type === 'textarea') {
      const t = document.createElement(f.type === 'textarea' ? 'textarea' : 'input');
      if (f.type !== 'textarea') t.type = 'text';
      if (f.type === 'textarea') t.rows = 2;
      if (f.placeholder) t.placeholder = f.placeholder;
      t.id = 'field-' + f.key;
      if (f.type === 'textarea' && window.__seedValues && window.__seedValues[f.key]) t.value = window.__seedValues[f.key];
      lab.appendChild(t);
    } else if (f.type === 'select') {
      const t = document.createElement('select');
      t.id = 'field-' + f.key;
      for (const o of (f.options || [])) {
        const opt = el('option'); opt.value = o; opt.textContent = o;
        if (o === f.default) opt.selected = true;
        t.appendChild(opt);
      }
      lab.appendChild(t);
    } else if (f.type === 'number') {
      const t = document.createElement('input');
      t.type = 'number'; t.id = 'field-' + f.key;
      if (f.min != null) t.min = f.min;
      if (f.max != null) t.max = f.max;
      if (f.default != null) t.value = f.default;
      lab.appendChild(t);
    } else if (f.type === 'boolean') {
      const cb = document.createElement('input');
      cb.type = 'checkbox'; cb.id = 'field-' + f.key;
      lab.appendChild(cb);
    }
    wrap.appendChild(lab);
  }
  wrap.appendChild(el('div'));
  const promptLab = el('label', 'ctrl');
  promptLab.innerHTML = 'Free prompt add-on';
  const pt = document.createElement('input');
  pt.type = 'text'; pt.id = 'free-prompt'; pt.placeholder = 'anything to append to the composed prompt';
  promptLab.appendChild(pt);
  wrap.appendChild(promptLab);
}
window.__seedValues = {}; // preset/genre fill-in hooks for the compose handoff

/* ---------------- the composer: submission ---------------- */
function collectGenerateBody() {
  const m = currentMode();
  const body = { enhance: $('enhance').checked };
  if (m === 'generate') {
    body.mode = 'generate';
    body.genre = $('genre').value;
    const fields = {};
    document.querySelectorAll('#fields [id^="field-"]').forEach(inp => {
      const key = inp.id.slice('field-'.length);
      // The template macros match on the literal strings ('true'/'false'),
      // and the fields map is map[string]string — booleans serialize as words.
      if (inp.type === 'checkbox') fields[key] = inp.checked ? 'true' : 'false';
      else if (inp.value !== '') fields[key] = inp.value;
    });
    const free = $('free-prompt') ? $('free-prompt').value.trim() : '';
    if (free) fields.free_prompt = free;
    body.fields = fields;
    body.size = $('size').value;
  } else if (m === 'edit') {
    body.mode = 'edit';
    body.prompt = $('prompt').value;
    body.size = $('edit-size').value;
    body.image = uploaded.edit;
    body.strength = parseFloat($('strength').value);
    if (!body.image) { showStatus('Upload an image to edit first.', 'err'); return null; }
  } else if (m === 'inpaint') {
    body.mode = 'inpaint';
    body.prompt = $('inpaint-prompt').value;
    body.image = uploaded.inpaint;
    body.mask = maskB64;
    if (!body.image || !body.mask) { showStatus('Upload an image and paint a mask first.', 'err'); return null; }
  } else if (m === 'outpaint') {
    body.mode = 'outpaint';
    body.prompt = $('outpaint-prompt').value;
    body.image = uploaded.outpaint;
    body.direction = $('outpaint-dir').value;
    body.expand_px = parseInt($('outpaint-pad').value, 10);
    if (!body.image) { showStatus('Upload an image to outpaint first.', 'err'); return null; }
  } else if (m === 'blend') {
    body.mode = 'blend';
    body.prompt = $('blend-prompt').value;
    body.size = $('blend-size').value;
    body.images = uploaded.blend;
    body.strengths = Array.from(document.querySelectorAll('#refs input[type=range]')).map(w => parseFloat(w.value));
    if (!body.images.length) { showStatus('Upload at least one reference image.', 'err'); return null; }
  } else if (m === 'upscale') {
    body.mode = 'upscale';
    body.image = uploaded.upscale;
    if (!body.image) { showStatus('Upload an image to upscale first.', 'err'); return null; }
  } else if (m === 'pose') {
    body.mode = 'pose';
    body.prompt = $('pose-prompt').value;
    body.size = $('pose-size').value;
    body.image = uploaded.pose;
    body.strength = parseFloat($('pose-strength').value);
    if (!body.image) { showStatus('Upload a pose reference first.', 'err'); return null; }
  }
  if (body.mode === 'generate' || body.mode === 'edit' || body.mode === 'pose') {
    if ($('style').value) body.style = $('style').value;
    if ($('preset').value) body.preset = $('preset').value;
    if ($('model').value) body.model = $('model').value;
    const rawModel = $('model-raw').value.trim();
    if (rawModel) body.model = rawModel;
    const rawLora = $('lora-raw').value.trim();
    if (rawLora) {
      const scaleRow = document.querySelector('#lora-list input[type=number]');
      const sc = scaleRow ? parseFloat(scaleRow.value) : 0.8;
      body.loras = (body.loras || []).concat([{ name: rawLora, scale: sc }]);
    }
    const loraRows = document.querySelectorAll('#lora-list .lora-row');
    body.loras = (body.loras || []).concat(Array.from(loraRows).map(r => ({
      name: r.querySelector('select').value,
      scale: parseFloat(r.querySelector('input[type=number]').value),
    })).filter(l => l.name));
    const neg = $('negative-prompt').value.trim();
    if (neg) body.negative_prompt = neg;
    const seed = $('seed').value;
    if (seed !== '') body.seed = parseInt(seed, 10);
    else if (lockSeed && window.__lastSeed != null) body.seed = window.__lastSeed;
    const steps = $('steps').value;
    if (steps !== '') body.steps = parseInt(steps, 10);
    const guidance = $('guidance').value;
    if (guidance !== '') body.guidance = parseFloat(guidance);
  }
  const batch = parseInt(($('batch') || {}).value || '1', 10);
  if (body.mode === 'generate') {
    if (m === 'generate' && batch > 1) body.batch = batch;
  }
  return body;
}

let maskB64 = null;
async function setupMask(file) {
  const url = await readFileAsDataURL(file);
  uploaded.inpaint = url.split(',')[1];
  const pv = $('mask-canvas');
  const img = new Image();
  img.onload = () => {
    pv.width = img.width; pv.height = img.height;
    pv.style.maxWidth = '100%';
    const ctx = pv.getContext('2d');
    ctx.fillStyle = '#000';
    ctx.fillRect(0, 0, pv.width, pv.height);
    const uv = $('inpaint-preview') || el('img');
    uv.src = url; uv.hidden = false;
  };
  img.src = url;
  $('mask-wrap').hidden = false;
}

/* ---------------- SSE + progress + result handling ---------------- */
function subscribe(id) {
  activeJobId = id;
  showProgress(true, 'queued');
  appendLog(new Date(), 'queued', 'job ' + id + ' submitted');
  stream(id);
}
function stream(id) {
  const es = new EventSource('/api/jobs/' + id + '/events');
  es.onmessage = (e) => {
    let ev = null;
    try { ev = JSON.parse(e.data); } catch (_) { return; }
    if (ev.log) appendLog(new Date(), ev.status || 'info', ev.log);
    if (ev.step != null && ev.step >= 0 && ev.total > 0) {
      updateProgress(id, ev.step, ev.total);
    }
    if (ev.status === 'done') {
      if (ev.seed != null) {
        window.__lastSeed = ev.seed;
        if ($('lock-seed').checked) $('seed').value = String(ev.seed);
      }
      showProgress(false);
      setStatus('Complete.', 'ok');
      showImage(id);
      refreshLibrarySoon();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status === 'failed') {
      showProgress(false);
      showStatus(ev.error ? humanizeError(new Error(ev.error)) : 'Generation failed.', 'err', ev.error);
      refreshLibrarySoon();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status === 'cancelled') {
      showProgress(false);
      setStatus('Cancelled.', 'warn');
      refreshLibrarySoon();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status) {
      const label = ev.status === 'generating' ? 'Generating…' : ev.status + '…';
      if (activeJobId === id) setStatus(label, 'pending');
      showProgress(true, ev.status);
    }
  };
}
function showImage(id) {
  lastImageId = id;
  $('empty-state').hidden = true;
  const im = $('image');
  im.hidden = false;
  im.src = imgUrl(id);
  im.onclick = () => window.Library && window.Library.openLightbox(id, null);
}
function showProgress(show, label) {
  const wrap = $('progress-wrap');
  if (show) {
    $('empty-state').hidden = true;
    $('image').hidden = true;
    wrap.hidden = false;
    wrap.classList.toggle('indeterminate', label === 'queued' || label === 'enhancing');
    $('progress-text').textContent = label && label !== 'generating' ? label + '…' : 'generating…';
  } else {
    wrap.hidden = true;
    wrap.classList.remove('indeterminate');
    if (lastImageId) {
      $('image').hidden = false;
      $('image').src = imgUrl(lastImageId);
      $('empty-state').hidden = true;
    } else {
      $('image').hidden = true;
      $('empty-state').hidden = false;
    }
  }
}
function updateProgress(id, step, total) {
  const pct = total > 0 ? Math.round((step / total) * 100) : 0;
  if (id === activeJobId) {
    $('progress-text').textContent = 'step ' + step + ' of ' + total + ' (' + pct + '%)';
  }
  window.Library && window.Library.updateCardProgress && window.Library.updateCardProgress(id, pct);
}
async function cancelJob(id) {
  id = id || activeJobId;
  if (!id) return;
  setStatus('Cancelling…', 'pending');
  try {
    await jsonFetch('/api/jobs/' + id + '/cancel', { method: 'POST' });
    setStatus('Cancelling…', 'pending');
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
  }
}
async function deleteJob(id) {
  try {
    await jsonFetch('/api/jobs/' + id, { method: 'DELETE' });
    refreshLibrarySoon();
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
  }
}
async function exportImage(id) {
  try {
    const r = await jsonFetch('/api/export', {
      method: 'POST', headers: POSTJSON,
      body: JSON.stringify({ ids: [id] }),
    });
    const dest = (r.copied || [])[0] || 'skipped';
    setStatus('Saved: ' + dest, r.copied && r.copied.length ? 'ok' : 'err');
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
  }
}
async function generate() {
  const body = collectGenerateBody();
  if (!body) return;
  $('generate').disabled = true;
  setStatus('Submitting…', 'pending');
  try {
    const res = await jsonFetch('/api/jobs', {
      method: 'POST', headers: POSTJSON,
      body: JSON.stringify(body),
    });
    if (res.job_ids) {
      res.job_ids.forEach(subscribe);
      setStatus('Queued ' + res.job_ids.length + ' images.', 'pending');
      subscribe(res.job_ids[res.job_ids.length - 1]);
    } else subscribe(res.job_id);
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
    $('generate').disabled = false;
  }
}

/* ---------------- use-as-source handoff (cross-workspace) ---------------- */
async function useAsSource(id, mode) {
  const wantMode = mode || 'edit';
  try {
    const resp = await fetch(imgUrl(id));
    if (!resp.ok) throw new Error('image not found');
    uploaded[wantMode === 'upscale' ? 'upscale' : wantMode] = await blobToBase64(await resp.blob());
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
    return;
  }
  setView('compose');
  setMode(wantMode);
  const pv = $(wantMode + '-preview');
  if (pv) {
    pv.src = imgUrl(id);
    pv.hidden = false;
  }
  setStatus('Loaded image ' + id + ' as the ' + wantMode + ' source.', 'pending');
}

/* ---------------- mask painting (inpaint) ---------------- */
function initMaskCanvas() {
  const canvas = $('mask-canvas');
  let painting = false;
  let erase = false;
  const ctx = () => canvas.getContext('2d');
  function pos(e) {
    const r = canvas.getBoundingClientRect();
    return [(e.clientX - r.left) * (canvas.width / r.width),
            (e.clientY - r.top) * (canvas.height / r.height)];
  }
  function paint(e) {
    if (!painting) return;
    const [x, y] = pos(e);
    const c = ctx();
    const brush = parseInt($('brush').value, 10);
    c.globalCompositeOperation = erase ? 'destination-out' : 'source-over';
    c.fillStyle = '#fff';
    c.beginPath();
    c.arc(x, y, brush, 0, Math.PI * 2);
    c.fill();
  }
  canvas.onpointerdown = (e) => { painting = true; canvas.setPointerCapture(e.pointerId); paint(e); };
  canvas.onpointermove = paint;
  canvas.onpointerup = () => { painting = false; maskB64 = canvasToB64(canvas); };
  $('mask-eraser').onclick = () => { erase = !erase; $('mask-eraser').ariaPressed = String(erase); };
  $('mask-clear').onclick = () => {
    ctx().clearRect(0, 0, canvas.width, canvas.height);
    ctx().fillStyle = '#000';
    ctx().fillRect(0, 0, canvas.width, canvas.height);
    maskB64 = null;
  };
}
function canvasToB64(canvas) {
  const d = canvas.toDataURL('image/png');
  return d.split(',')[1];
}

/* ---------------- in-flight card hooks (the library's progress bars) ---------------- */
window.LibraryHooks = {
  activeId: () => activeJobId,
};

/* ---------------- boot ---------------- */
/* ---------------- Simple / Pro interface mode ---------------- */
function setUIMode(mode) {
  document.documentElement.dataset.ui = (mode === 'pro') ? 'pro' : 'simple';
  const seg = $('ui-mode-seg');
  if (seg) seg.querySelectorAll('.seg-btn').forEach(b =>
    b.setAttribute('aria-pressed', String(b.dataset.ui === mode)));
  try { localStorage.setItem('imggen-ui', mode); } catch (_) {}
}


async function init() {
  try {
    await loadCatalogs();
  } catch (e) {
    setConn('error', 'err');
    appendLog(new Date(), 'failed', 'catalogs: ' + e.message);
    return;
  }
  setConn('connected', 'ok');
  renderGenreSelect();
  renderStyleSelect();
  renderModelSelect();
  renderPresetSelect();
  const genLike = () => {};
  $('add-lora').onclick = addLoraRow;
  document.querySelectorAll('#mode-grid .chip').forEach(c => {
    c.onclick = () => setMode(c.dataset.mode);
  });
  populateSizes($('edit-size'), EDIT_SIZES);
  populateSizes($('blend-size'), EDIT_SIZES);
  populateSizes($('pose-size'), EDIT_SIZES);
  $('edit-image').onchange = async (e) => {
    if (e.target.files[0]) {
      const url = await readFileAsDataURL(e.target.files[0]);
      uploaded.edit = stripDataURL(url);
      const pv = $('edit-preview');
      pv.src = url; pv.hidden = false;
    }
  };
  $('inpaint-image').onchange = async (e) => {
    if (e.target.files[0]) await setupMask(e.target.files[0]);
  };
  $('blend-image').onchange = async (e) => {
    uploaded.blend = [];
    for (const f of e.target.files) uploaded.blend.push(stripDataURL(await readFileAsDataURL(f)));
    window.Compose && window.Compose.renderRefs && window.Compose.renderRefs();
  };
  $('outpaint-image').onchange = async (e) => {
    if (e.target.files[0]) {
      const url = await readFileAsDataURL(e.target.files[0]);
      uploaded.outpaint = stripDataURL(url);
      const pv = $('outpaint-preview');
      pv.src = url; pv.hidden = false;
    }
  };
  $('upscale-image').onchange = async (e) => {
    if (e.target.files[0]) {
      const url = await readFileAsDataURL(e.target.files[0]);
      uploaded.upscale = stripDataURL(url);
      const pv = $('upscale-preview');
      pv.src = url; pv.hidden = false;
      $('repair-run') && (window.Studio && (window.Studio.onUploadReady && window.Studio.onUploadReady()));
    }
  };
  $('pose-image').onchange = async (e) => {
    if (e.target.files[0]) {
      const url = await readFileAsDataURL(e.target.files[0]);
      uploaded.pose = stripDataURL(url);
      const pv = $('pose-preview');
      pv.src = url; pv.hidden = false;
    }
  };
  $('pose-strength').oninput = () => { $('pose-strength-val').textContent = $('pose-strength').value; };
  $('strength').oninput = () => { $('strength-val').textContent = $('strength').value; };
  $('composer').onsubmit = (e) => e.preventDefault();
  initMaskCanvas();
  renderModeVisibility();
  $('generate').onclick = generate;
  $('cancel').onclick = () => cancelJob();
  $('clear-log').onclick = clearLog;
  document.querySelectorAll('.tab').forEach(t => t.onclick = () => setView(t.dataset.view));
  const seg = $('ui-mode-seg');
  if (seg) seg.querySelectorAll('.seg-btn').forEach(b => b.onclick = () => setUIMode(b.dataset.ui));
  let uiMode = 'simple';
  try { uiMode = localStorage.getItem('imggen-ui') || 'simple'; } catch (_) {}
  setUIMode(uiMode);
  const startView = (location.hash || '').slice(1);
  setView(['fast', 'library', 'compose', 'studio'].includes(startView)
    ? startView : (document.documentElement.dataset.ui === 'pro' ? 'library' : 'fast'));
  window.Library.refresh && window.Library.refresh();
  window.addEventListener('hashchange', () => {
    const v = (location.hash || '').slice(1);
    if (['fast', 'library', 'compose', 'studio'].includes(v) && !document.getElementById('view-' + v).hidden) return;
    if (['fast', 'library', 'compose', 'studio'].includes(v)) setView(v);
  });
}
init();

/* ---------------- lora rows ---------------- */
function addLoraRow(prefName, prefScale) {
  const list = $('lora-list');
  const rowE = el('div', 'lora-row');
  const sel = document.createElement('select');
  const none = Object.assign(el('option'), { value: '', textContent: '— pick a LoRA —' });
  sel.appendChild(none);
  for (const l of (models.loras || [])) {
    const o = el('option'); o.value = l.key; o.textContent = l.label;
    sel.appendChild(o);
  }
  if (prefName) sel.value = prefName;
  const scale = document.createElement('input');
  scale.type = 'number';
  scale.min = '0'; scale.max = '2'; scale.step = '0.05';
  scale.value = String(prefScale != null ? prefScale : 0.8);
  const rm = el('button', 'ghost small', '×');
  rm.type = 'button';
  rm.onclick = () => rowE.remove();
  rowE.appendChild(sel);
  rowE.appendChild(scale);
  rowE.appendChild(rm);
  list.appendChild(rowE);
}

/* ---------------- catalogs ---------------- */
async function loadCatalogs() {
  genres = await jsonFetch('/api/genres');
  models = await jsonFetch('/api/models');
  presets = await jsonFetch('/api/presets');
  // the profile-aware server default for model-less submits
  try {
    const r = await fetch('/api/models');
    const j = await r.json();
    window.__defaultModel = (j.models || []).find(m => m.key === 'sdxl-illustrious-v2') ? 'sdxl-illustrious-v2' : '';
  } catch (_) {}
}
