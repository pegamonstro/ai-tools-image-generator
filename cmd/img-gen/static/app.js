'use strict';

let genres = null;
let models = { models: [], loras: [] };
let presets = { presets: [] };
let activeJobId = null;
let detailJob = null;

const EDIT_SIZES = ['512x512', '768x512', '1024x576', '1024x1024'];
let uploaded = { edit: null, inpaint: null, blend: [], upscale: null, pose: null }; // base64 strings
let outpaintImg = null; // decoded Image of the source to expand
let brushErase = false;

function currentMode() { return $('mode').value; }

function renderMode() {
  const m = currentMode();
  $('gen-controls').hidden = m !== 'generate';
  $('edit-controls').hidden = m !== 'edit';
  $('inpaint-controls').hidden = m !== 'inpaint';
  $('outpaint-controls').hidden = m !== 'outpaint';
  $('blend-controls').hidden = m !== 'blend';
  $('upscale-controls').hidden = m !== 'upscale';
  $('pose-controls').hidden = m !== 'pose';
  // Upscale is a promptless deterministic pass; there is nothing to enhance.
  $('enhance').closest('.toggle').hidden = m === 'upscale';
  // LoRA + sampling knobs apply to generate/edit/pose (fill/redux take none).
  const genLike = (m === 'generate' || m === 'edit' || m === 'pose');
  $('lora-list').querySelectorAll('select, input').forEach(el => { el.disabled = !genLike; });
  $('add-lora').disabled = !genLike;
  $('lora-raw').disabled = !genLike;
  $('sampling-controls').hidden = !genLike;
}

function populateSizes(sel, sizes) {
  sel.innerHTML = '';
  for (const s of sizes) {
    const o = document.createElement('option');
    o.value = s; o.textContent = s;
    sel.appendChild(o);
  }
}

function readFileAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(r.result);
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}
function stripDataURL(d) { return d.split(',')[1]; }

function $(id) { return document.getElementById(id); }
function logEl() { return $('log'); }

async function jsonFetch(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) throw new Error((await r.text().catch(() => '')) || r.statusText);
  return r.json();
}

function setConn(state, cls) {
  const el = $('conn');
  el.textContent = state;
  el.className = 'conn ' + cls;
}

function setStatus(msg, cls) {
  const el = $('status');
  if (!msg) { el.hidden = true; el.textContent = ''; el.className = 'status'; return; }
  el.hidden = false;
  el.textContent = msg;
  el.className = 'status ' + (cls || '');
}

function fmtTime(ts) {
  const d = ts instanceof Date ? ts : new Date(ts);
  return d.toLocaleTimeString('en-GB', { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0');
}

function appendLog(ts, status, msg) {
  const log = logEl();
  const empty = log.querySelector('.log-empty');
  if (empty) empty.remove();
  const line = document.createElement('div');
  line.className = 'log-line ' + (status || 'info');
  const t = document.createElement('span');
  t.className = 'log-ts';
  t.textContent = fmtTime(ts);
  line.appendChild(t);
  const m = document.createElement('span');
  m.className = 'log-msg';
  m.textContent = msg;
  line.appendChild(m);
  log.appendChild(line);
  log.scrollTop = log.scrollHeight;
}

async function init() {
  try {
    genres = await jsonFetch('/api/genres');
  } catch (e) {
    setConn('● error', 'err');
    appendLog(new Date(), 'failed', 'could not load genres: ' + e.message);
    setStatus('Failed to load the app: ' + e.message, 'err');
    return;
  }
  setConn('● connected', 'ok');
  renderGenreSelect();
  renderStyleSelect();
  loadModels();
  loadPresets();
  $('add-lora').onclick = addLoraRow;
  $('mode').onchange = () => { renderMode(); };
  populateSizes($('edit-size'), EDIT_SIZES);
  populateSizes($('blend-size'), EDIT_SIZES);
  populateSizes($('pose-size'), EDIT_SIZES);
  $('edit-image').onchange = async (e) => {
    if (e.target.files[0]) {
      const url = await readFileAsDataURL(e.target.files[0]);
      uploaded.edit = stripDataURL(url);
      const pv = $('edit-preview');
      pv.src = url;
      pv.hidden = false;
    }
  };
  $('inpaint-image').onchange = async (e) => {
    if (e.target.files[0]) {
      uploaded.inpaint = stripDataURL(await readFileAsDataURL(e.target.files[0]));
      await setupMask(e.target.files[0]);
    }
  };
  $('blend-image').onchange = async (e) => {
    uploaded.blend = [];
    for (const f of e.target.files) uploaded.blend.push(stripDataURL(await readFileAsDataURL(f)));
    renderRefs();
  };
  $('outpaint-image').onchange = async (e) => {
    if (e.target.files[0]) await setupOutpaint(e.target.files[0]);
  };
  $('upscale-image').onchange = async (e) => {
    if (e.target.files[0]) {
      const url = await readFileAsDataURL(e.target.files[0]);
      uploaded.upscale = stripDataURL(url);
      const pv = $('upscale-preview');
      pv.src = url;
      pv.hidden = false;
    }
  };
  $('pose-image').onchange = async (e) => {
    if (e.target.files[0]) {
      const url = await readFileAsDataURL(e.target.files[0]);
      uploaded.pose = stripDataURL(url);
      const pv = $('pose-preview');
      pv.src = url;
      pv.hidden = false;
    }
  };
  $('pose-strength').oninput = () => { $('pose-strength-val').textContent = $('pose-strength').value; };
  renderMode();
  $('generate').onclick = generate;
  $('cancel').onclick = () => cancelJob();
  $('clear-log').onclick = () => { logEl().innerHTML = ''; };
  loadHistory();
}

function renderGenreSelect() {
  const sel = $('genre');
  sel.innerHTML = '';
  for (const [key, g] of Object.entries(genres.genres)) {
    const opt = document.createElement('option');
    opt.value = key;
    opt.textContent = g.label;
    sel.appendChild(opt);
  }
  sel.onchange = renderForm;
  renderForm();
}

function renderStyleSelect() {
  const sel = $('style');
  sel.innerHTML = '';
  const none = document.createElement('option');
  none.value = '';
  none.textContent = 'None';
  sel.appendChild(none);
  for (const s of (genres.styles || [])) {
    const o = document.createElement('option');
    o.value = s.key;
    o.textContent = s.label;
    sel.appendChild(o);
  }
}

async function loadModels() {
  try {
    models = await jsonFetch('/api/models');
  } catch (e) {
    appendLog(new Date(), 'failed', 'could not load models: ' + e.message);
    return;
  }
  renderModelSelect();
  renderLoraSelect();
}

async function loadPresets() {
  try {
    presets = await jsonFetch('/api/presets');
  } catch (e) {
    appendLog(new Date(), 'failed', 'could not load presets: ' + e.message);
    return;
  }
  renderPresetSelect();
}

function renderPresetSelect() {
  const sel = $('preset');
  sel.innerHTML = '';
  const none = document.createElement('option');
  none.value = '';
  none.textContent = 'None';
  sel.appendChild(none);
  for (const p of (presets.presets || [])) {
    const o = document.createElement('option');
    o.value = p.key;
    o.textContent = p.label;
    sel.appendChild(o);
  }
}

function renderModelSelect() {
  const sel = $('model');
  sel.innerHTML = '';
  const none = document.createElement('option');
  none.value = '';
  none.textContent = 'Sidecar default';
  sel.appendChild(none);
  for (const m of (models.models || [])) {
    const o = document.createElement('option');
    o.value = m.value;
    o.textContent = m.label;
    sel.appendChild(o);
  }
}

function makeLoraSelect() {
  const sel = document.createElement('select');
  sel.className = 'lora-name';
  const none = document.createElement('option');
  none.value = '';
  none.textContent = 'None';
  sel.appendChild(none);
  for (const l of (models.loras || [])) {
    const o = document.createElement('option');
    o.value = l.value;
    o.textContent = l.label;
    sel.appendChild(o);
  }
  return sel;
}

function addLoraRow() {
  const row = document.createElement('div');
  row.className = 'lora-row';
  const sel = makeLoraSelect();
  const scale = document.createElement('input');
  scale.type = 'range'; scale.min = '0'; scale.max = '1'; scale.step = '0.05'; scale.value = '1';
  scale.className = 'lora-scale';
  const rm = document.createElement('button');
  rm.type = 'button'; rm.textContent = '×'; rm.className = 'ghost';
  rm.onclick = () => row.remove();
  row.appendChild(sel); row.appendChild(scale); row.appendChild(rm);
  $('lora-list').appendChild(row);
}

// renderLoraSelect resets the LoRA list to a single empty row after the catalog
// loads. Rows can be added or removed afterwards without reloading.
function renderLoraSelect() {
  $('lora-list').innerHTML = '';
  addLoraRow();
}

function collectLoras() {
  const out = [];
  for (const row of document.querySelectorAll('#lora-list .lora-row')) {
    const name = row.querySelector('.lora-name').value;
    if (name) out.push({ name, scale: parseFloat(row.querySelector('.lora-scale').value) });
  }
  // The custom free-text LoRA contributes one extra entry at full scale.
  const raw = $('lora-raw').value.trim();
  if (raw) out.push({ name: raw, scale: 1.0 });
  return out;
}

function collectSampling() {
  const sp = {};
  const seed = $('seed').value.trim();
  if (seed !== '') sp.seed = parseInt(seed, 10);
  const steps = $('steps').value.trim();
  if (steps !== '') sp.steps = parseInt(steps, 10);
  const guidance = $('guidance').value.trim();
  if (guidance !== '') sp.guidance = parseFloat(guidance);
  const neg = $('negative-prompt').value.trim();
  if (neg !== '') sp.negative_prompt = neg;
  return sp;
}

function collectModelSpec() {
  const model = $('model-raw').value.trim() || $('model').value;
  const spec = {};
  if (model) spec.model = model;
  const loras = collectLoras();
  if (loras.length) spec.loras = loras;
  return spec;
}

function renderForm() {
  const g = genres.genres[$('genre').value];
  const fieldsEl = $('fields');
  fieldsEl.innerHTML = '';
  for (const f of g.fields) fieldsEl.appendChild(renderField(f));

  const sizeEl = $('size');
  sizeEl.innerHTML = '';
  for (const s of g.sizes) {
    const opt = document.createElement('option');
    opt.value = s;
    opt.textContent = s;
    sizeEl.appendChild(opt);
  }
}

function renderField(f) {
  const wrap = document.createElement('label');
  wrap.className = 'field';
  const span = document.createElement('span');
  span.className = 'field-label';
  span.textContent = f.label + (f.required ? ' *' : '');
  wrap.appendChild(span);

  let input;
  switch (f.type) {
    case 'textarea':
      input = document.createElement('textarea');
      input.placeholder = f.placeholder || '';
      break;
    case 'select':
      input = document.createElement('select');
      for (const o of f.options) {
        const opt = document.createElement('option');
        opt.value = o;
        opt.textContent = o;
        if (o === f.default) opt.selected = true;
        input.appendChild(opt);
      }
      break;
    case 'number':
      input = document.createElement('input');
      input.type = 'number';
      if (f.min != null) input.min = f.min;
      if (f.max != null) input.max = f.max;
      if (f.default != null) input.value = f.default;
      break;
    case 'boolean':
      input = document.createElement('input');
      input.type = 'checkbox';
      break;
    default:
      input = document.createElement('input');
      input.type = 'text';
      input.placeholder = f.placeholder || '';
  }
  input.dataset.key = f.key;
  input.dataset.type = f.type;
  wrap.appendChild(input);

  if (f.hint) {
    const small = document.createElement('small');
    small.textContent = f.hint;
    wrap.appendChild(small);
  }
  return wrap;
}

function collectFields() {
  const out = {};
  for (const el of document.querySelectorAll('#fields [data-key]')) {
    out[el.dataset.key] = el.dataset.type === 'boolean' ? (el.checked ? 'true' : 'false') : el.value;
  }
  return out;
}

let maskStroke = null; // offscreen canvas holding only black bg + painted strokes
let guideImg = null;

async function setupMask(file) {
  const url = URL.createObjectURL(file);
  const img = new Image();
  img.onload = () => {
    const cv = $('mask-canvas');
    cv.width = img.naturalWidth;
    cv.height = img.naturalHeight;

    maskStroke = document.createElement('canvas');
    maskStroke.width = cv.width;
    maskStroke.height = cv.height;
    const sctx = maskStroke.getContext('2d');
    sctx.fillStyle = '#000';
    sctx.fillRect(0, 0, cv.width, cv.height);

    redrawMask(img);
    $('mask-wrap').hidden = false;
    URL.revokeObjectURL(url);
  };
  img.src = url;

  $('mask-eraser').onclick = () => { brushErase = !brushErase; $('mask-eraser').textContent = brushErase ? 'Brush' : 'Eraser'; };
  $('mask-clear').onclick = () => {
    const sctx = maskStroke.getContext('2d');
    sctx.fillStyle = '#000';
    sctx.fillRect(0, 0, maskStroke.width, maskStroke.height);
    redrawMask(null);
  };
  const cv = $('mask-canvas');
  cv.onmousedown = (e) => { cv._drawing = true; paintMask(e); };
  cv.onmousemove = (e) => { if (cv._drawing) paintMask(e); };
  cv.onmouseup = cv.onmouseleave = () => { cv._drawing = false; };
}

function redrawMask(img) {
  guideImg = img;
  const cv = $('mask-canvas');
  const ctx = cv.getContext('2d');
  ctx.clearRect(0, 0, cv.width, cv.height);
  if (img) { ctx.globalAlpha = 0.4; ctx.drawImage(img, 0, 0); ctx.globalAlpha = 1.0; }
  ctx.drawImage(maskStroke, 0, 0);
}

function paintMask(e) {
  const cv = $('mask-canvas');
  const r = cv.getBoundingClientRect();
  const x = (e.clientX - r.left) * (cv.width / r.width);
  const y = (e.clientY - r.top) * (cv.height / r.height);
  const sctx = maskStroke.getContext('2d');
  sctx.fillStyle = brushErase ? '#000' : '#fff';
  sctx.beginPath();
  sctx.arc(x, y, $('brush').value, 0, Math.PI * 2);
  sctx.fill();
  redrawMask(guideImg);
}

function maskAsBase64() {
  return maskStroke.toDataURL('image/png').split(',')[1];
}

async function setupOutpaint(file) {
  const url = URL.createObjectURL(file);
  const img = new Image();
  img.onload = () => {
    outpaintImg = img;
    const pv = $('outpaint-preview');
    pv.src = url;
    pv.hidden = false;
    URL.revokeObjectURL(url);
  };
  img.src = url;
}

// buildOutpaint places the source on a larger canvas and returns the padded
// image plus a border mask (white = regenerate, black = keep) for the /fill
// sidecar. New dimensions are rounded up to multiples of 64 for FLUX.
function buildOutpaint() {
  const dir = $('outpaint-dir').value;
  const pad = parseInt($('outpaint-pad').value, 10);
  const w = outpaintImg.naturalWidth;
  const h = outpaintImg.naturalHeight;
  let nw = w, nh = h, ox = 0, oy = 0;
  switch (dir) {
    case 'all': nw = w + 2 * pad; nh = h + 2 * pad; ox = pad; oy = pad; break;
    case 'left': nw = w + pad; ox = pad; break;
    case 'right': nw = w + pad; break;
    case 'top': nh = h + pad; oy = pad; break;
    case 'bottom': nh = h + pad; break;
    case 'left+right': nw = w + 2 * pad; ox = pad; break;
    case 'top+bottom': nh = h + 2 * pad; oy = pad; break;
  }
  nw = Math.ceil(nw / 64) * 64;
  nh = Math.ceil(nh / 64) * 64;

  const cv = document.createElement('canvas');
  cv.width = nw; cv.height = nh;
  const ctx = cv.getContext('2d');
  ctx.fillStyle = '#000';
  ctx.fillRect(0, 0, nw, nh);
  ctx.drawImage(outpaintImg, ox, oy, w, h);

  const mc = document.createElement('canvas');
  mc.width = nw; mc.height = nh;
  const mctx = mc.getContext('2d');
  mctx.fillStyle = '#fff';
  mctx.fillRect(0, 0, nw, nh);
  mctx.fillStyle = '#000';
  mctx.fillRect(ox, oy, w, h);

  return {
    image: cv.toDataURL('image/png').split(',')[1],
    mask: mc.toDataURL('image/png').split(',')[1],
  };
}

function renderRefs() {
  const el = $('refs');
  el.innerHTML = '';
  uploaded.blend.forEach((b64, i) => {
    const row = document.createElement('div');
    row.className = 'ref';
    const label = document.createElement('span');
    label.textContent = 'Ref ' + (i + 1);
    const w = document.createElement('input');
    w.type = 'range'; w.min = '0'; w.max = '1'; w.step = '0.05'; w.value = '1';
    w.dataset.idx = i;
    const rm = document.createElement('button');
    rm.type = 'button'; rm.textContent = '×';
    rm.onclick = () => { uploaded.blend.splice(i, 1); renderRefs(); };
    row.appendChild(label); row.appendChild(w); row.appendChild(rm);
    el.appendChild(row);
  });
}

async function generate() {
  const mode = currentMode();
  const body = { mode, enhance: $('enhance').checked, style: $('style').value, preset: $('preset').value };
  body.batch = Math.max(1, Math.min(8, parseInt($('batch').value, 10) || 1));
  if (mode === 'generate' || mode === 'edit' || mode === 'pose') {
    Object.assign(body, collectModelSpec());
    Object.assign(body, collectSampling());
  }
  if (mode === 'generate') {
    body.genre = $('genre').value;
    body.fields = collectFields();
    body.size = $('size').value;
  } else if (mode === 'edit') {
    body.prompt = $('prompt').value;
    body.size = $('edit-size').value;
    body.image = uploaded.edit;
    body.strength = parseFloat($('strength').value);
    if (!body.image) { setStatus('please upload an image', 'err'); return; }
  } else if (mode === 'inpaint') {
    body.prompt = $('inpaint-prompt').value;
    body.image = uploaded.inpaint;
    if (!body.image || !maskStroke) { setStatus('please upload an image and paint a mask', 'err'); return; }
    body.mask = maskAsBase64();
  } else if (mode === 'outpaint') {
    body.prompt = $('outpaint-prompt').value;
    if (!outpaintImg) { setStatus('please upload an image to expand', 'err'); return; }
    Object.assign(body, buildOutpaint());
  } else if (mode === 'pose') {
    body.prompt = $('pose-prompt').value;
    body.size = $('pose-size').value;
    body.image = uploaded.pose;
    body.strength = parseFloat($('pose-strength').value);
    if (!body.image) { setStatus('please upload a reference image', 'err'); return; }
  } else if (mode === 'blend') {
    body.prompt = $('blend-prompt').value;
    body.size = $('blend-size').value;
    body.images = uploaded.blend;
    body.strengths = Array.from(document.querySelectorAll('#refs input[type=range]')).map(w => parseFloat(w.value));
    if (!body.images.length) { setStatus('please upload at least one reference image', 'err'); return; }
  } else if (mode === 'upscale') {
    body.image = uploaded.upscale;
    if (!body.image) { setStatus('please upload an image to upscale', 'err'); return; }
  }
  $('generate').disabled = true;
  setStatus('Submitting…', 'pending');
  try {
    const res = await jsonFetch('/api/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (res.job_ids) {
      res.job_ids.forEach(subscribe);
      setStatus('queued ' + res.job_ids.length + ' images', 'pending');
    } else {
      subscribe(res.job_id);
    }
  } catch (e) {
    setStatus('error: ' + e.message, 'err');
    $('generate').disabled = false;
  }
}

function subscribe(id) {
  activeJobId = id;
  setStatus('queued', 'pending');
  appendLog(new Date(), 'queued', 'job ' + id + ' submitted');
  stream(id);
}

function stream(id) {
  const es = new EventSource('/api/jobs/' + id + '/events');
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (ev.log) appendLog(ev.ts || new Date(), ev.status || 'info', ev.log);
    if (ev.step != null && ev.total > 0) {
      updateProgress(id, ev.step, ev.total);
    }
    if (ev.status === 'done') {
      if (ev.seed != null && $('lock-seed').checked) {
        $('seed').value = String(ev.seed);
      }
      showProgress(false);
      setStatus('complete', 'ok');
      showImage(id);
      loadHistory();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status === 'failed') {
      showProgress(false);
      setStatus('failed' + (ev.error ? ': ' + ev.error : ''), 'err');
      loadHistory();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status === 'cancelled') {
      showProgress(false);
      setStatus('cancelled', 'warn');
      loadHistory();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status) {
      setStatus(ev.status, 'pending');
      showProgress(true, ev.status === 'generating' ? 'generating…' : ev.status + '…');
    }
  };
  es.onerror = () => { /* generation can take a long time; keep the stream open */ };
}

function showImage(id) {
  $('result').hidden = false;
  $('image').hidden = false;
  $('image').src = '/api/images/' + id + '.png';
}

// showProgress reveals the Result panel's progress bar (and hides any previous
// image) during a job, or restores the image when the job ends.
function showProgress(show, label) {
  const wrap = $('progress-wrap');
  if (show) {
    $('result').hidden = false;
    $('image').hidden = true;
    wrap.hidden = false;
    if (label) $('progress-text').textContent = label;
  } else {
    wrap.hidden = true;
    $('image').hidden = false;
  }
}

// updateProgress fills the Result bar for the active job and any matching
// in-flight history card.
function updateProgress(id, step, total) {
  const pct = total > 0 ? Math.round((step / total) * 100) : 0;
  if (id === activeJobId) {
    $('progress-fill').style.width = pct + '%';
    $('progress-text').textContent = `step ${step} of ${total} (${pct}%)`;
  }
  const card = document.querySelector('.card[data-id="' + id + '"]');
  if (card) {
    const fill = card.querySelector('.progress-fill');
    if (fill) fill.style.width = pct + '%';
  }
}

async function cancelJob(id) {
  id = id || activeJobId;
  if (!id) return;
  setStatus('cancelling…', 'pending');
  try {
    await jsonFetch('/api/jobs/' + id + '/cancel', { method: 'POST' });
  } catch (e) {
    setStatus('cancel error: ' + e.message, 'err');
  }
}

async function loadHistory() {
  let jobs;
  try {
    jobs = await jsonFetch('/api/jobs');
  } catch (e) {
    appendLog(new Date(), 'failed', 'could not load history: ' + e.message);
    return;
  }
  const el = $('history');
  el.innerHTML = '';
  if (!jobs.length) {
    const empty = document.createElement('div');
    empty.className = 'log-empty';
    empty.textContent = 'No generations yet.';
    el.appendChild(empty);
  }
  for (const j of jobs) {
    const card = document.createElement('div');
    card.className = 'card ' + j.status;
    card.dataset.id = j.id;
    const inflight = j.status === 'queued' || j.status === 'enhancing' || j.status === 'generating';
    const head = document.createElement('div');
    head.className = 'card-head';
    const title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = j.mode && j.mode !== 'generate' ? j.mode : j.genre;
    head.appendChild(title);
    const badge = document.createElement('span');
    badge.className = 'badge ' + j.status;
    badge.textContent = j.status;
    head.appendChild(badge);
    card.appendChild(head);
    if (j.image_path) {
      const img = document.createElement('img');
      img.src = '/api/images/' + j.id + '.png';
      img.loading = 'lazy';
      card.appendChild(img);
    } else if (j.error) {
      const err = document.createElement('div');
      err.className = 'card-error';
      err.textContent = j.error;
      card.appendChild(err);
    }

    const meta = document.createElement('div');
    meta.className = 'card-meta';
    meta.textContent = [j.model, (j.loras && j.loras.length ? j.loras.map(l => l.name).join(', ') : ''), j.style, (j.seed != null ? 'seed ' + j.seed : '')].filter(Boolean).join(' · ');
    if (meta.textContent) card.appendChild(meta);

    if (inflight) {
      const prog = document.createElement('div');
      prog.className = 'progress-bar';
      const fill = document.createElement('div');
      fill.className = 'progress-fill';
      prog.appendChild(fill);
      card.appendChild(prog);

      const cancelBtn = document.createElement('button');
      cancelBtn.type = 'button';
      cancelBtn.className = 'danger';
      cancelBtn.textContent = 'Cancel';
      cancelBtn.onclick = () => cancelJob(j.id);
      card.appendChild(cancelBtn);
    }

    const acts = document.createElement('div');
    acts.className = 'card-actions';
    const view = document.createElement('button');
    view.type = 'button';
    view.textContent = 'View';
    view.onclick = () => openDetail(j);
    acts.appendChild(view);
    if (j.image_path) {
      const editBtn = document.createElement('button');
      editBtn.type = 'button';
      editBtn.textContent = 'Edit';
      editBtn.onclick = () => useHistoryAsEdit(j.id);
      acts.appendChild(editBtn);
      const upBtn = document.createElement('button');
      upBtn.type = 'button';
      upBtn.textContent = 'Upscale';
      upBtn.onclick = () => useHistoryAsUpscale(j.id);
      acts.appendChild(upBtn);
    }
    card.appendChild(acts);
    el.appendChild(card);
  }
  // Re-attach to any job still in flight so its live log keeps streaming.
  for (const j of jobs) {
    const inflight = j.status === 'queued' || j.status === 'enhancing' || j.status === 'generating';
    if (inflight && j.id !== activeJobId) {
      resume(j.id);
    }
  }
}

function resume(id) {
  activeJobId = id;
  setStatus('resumed ' + id, 'pending');
  stream(id);
}

function blobToBase64(blob) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(r.result.split(',')[1]);
    r.onerror = () => reject(r.error);
    r.readAsDataURL(blob);
  });
}

function openDetail(j) {
  detailJob = j;
  $('detail').hidden = false;
  $('detail-img').hidden = !j.image_path;
  if (j.image_path) {
    $('detail-img').src = '/api/images/' + j.id + '.png';
    $('detail-download').href = '/api/images/' + j.id + '.png?download=1';
  } else {
    $('detail-img').removeAttribute('src');
    $('detail-download').removeAttribute('href');
  }
  $('detail-meta').textContent = [j.prompt, j.model, (j.loras || []).map(l => l.name).join(', '), j.style, j.size, (j.seed != null ? 'seed: ' + j.seed : ''), j.created_at].filter(Boolean).join('\n');
  $('detail-edit').hidden = !j.image_path;
  $('detail-save').hidden = !j.image_path;
  $('detail-download').hidden = !j.image_path;
}

async function useHistoryAsEdit(id) {
  $('detail').hidden = true;
  try {
    const resp = await fetch('/api/images/' + id + '.png');
    if (!resp.ok) throw new Error('image not found');
    uploaded.edit = await blobToBase64(await resp.blob());
  } catch (e) {
    setStatus('error: ' + e.message, 'err');
    return;
  }
  const pv = $('edit-preview');
  pv.src = '/api/images/' + id + '.png';
  pv.hidden = false;
  $('mode').value = 'edit';
  renderMode();
  setStatus('loaded image ' + id + ' for editing', 'pending');
}

async function useHistoryAsUpscale(id) {
  $('detail').hidden = true;
  try {
    const resp = await fetch('/api/images/' + id + '.png');
    if (!resp.ok) throw new Error('image not found');
    uploaded.upscale = await blobToBase64(await resp.blob());
  } catch (e) {
    setStatus('error: ' + e.message, 'err');
    return;
  }
  const pv = $('upscale-preview');
  pv.src = '/api/images/' + id + '.png';
  pv.hidden = false;
  $('mode').value = 'upscale';
  renderMode();
  setStatus('loaded image ' + id + ' for upscaling', 'pending');
}

async function saveToFolder() {
  if (!detailJob) return;
  try {
    const r = await jsonFetch('/api/export', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids: [detailJob.id] }),
    });
    setStatus('saved: ' + (r.copied[0] || 'skipped'), r.copied.length ? 'ok' : 'err');
  } catch (e) {
    setStatus('error: ' + e.message, 'err');
  }
}

$('detail-close').onclick = () => { $('detail').hidden = true; };
$('detail-edit').onclick = () => useHistoryAsEdit(detailJob.id);
$('detail-save').onclick = saveToFolder;

init();
