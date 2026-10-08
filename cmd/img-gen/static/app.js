'use strict';

let genres = null;
let models = { models: [], loras: [] };
let presets = { presets: [] };
let activeJobId = null;
let detailJob = null;
let lastImageId = null;

const EDIT_SIZES = ['512x512', '768x512', '1024x576', '1024x1024'];
let uploaded = { edit: null, inpaint: null, blend: [], upscale: null, pose: null }; // base64 strings
let outpaintImg = null; // decoded Image of the source to expand
let brushErase = false;

function currentMode() {
  const active = document.querySelector('#mode-grid .chip[aria-pressed="true"]');
  return active ? active.dataset.mode : 'generate';
}

function setMode(m) {
  document.querySelectorAll('#mode-grid .chip').forEach(c =>
    c.setAttribute('aria-pressed', String(c.dataset.mode === m)));
  renderMode();
}

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

// envelopeMessage digs the human message out of a {…,"error":{"message":…}}
// response so the later /api/v1 error contract lands without touching callers.
function envelopeMessage(text) {
  try {
    const j = JSON.parse(text);
    if (j && j.error) return j.error.message || j.error.code || text;
  } catch (_) { /* plain-text error */ }
  return text;
}

async function jsonFetch(url, opts) {
  const r = await fetch(url, opts);
  const text = await r.text().catch(() => '');
  if (!r.ok) throw new Error(envelopeMessage(text) || r.statusText);
  return text ? JSON.parse(text) : {};
}

// humanizeError turns backend errors into one calm line. Raw text still goes
// to the Activity log; cards and the status line only ever see scrubbed text.
function humanizeError(e) {
  const raw = (e && e.message ? e.message : String(e)) || 'Something went wrong.';
  const line = raw.split('\n')[0].trim();
  const lower = line.toLowerCase();
  if (line.includes('409') && lower.includes('already in progress')) {
    return 'Generator busy — this runs when the current job finishes.';
  }
  if (/broken pipe|eof|connection refused|dial tcp|no such host/.test(lower)) {
    return 'Generator unreachable — check that the sidecar is running.';
  }
  if (line.includes('429')) return 'Inference queue full.';
  if (line.includes('404')) return 'Endpoint unavailable.';
  if (line.includes('500') || lower.includes('internal server error')) {
    return 'The generator hit an internal error — try again (raw details in the Activity log).';
  }
  const scrubbed = line
    .replace(/https?:\/\/\S+/g, '…')
    .replace(/\b\d{1,3}(\.\d{1,3}){3}\b/g, '…')
    .replace(/\/Users\/\S+/g, '…');
  return scrubbed.length > 120 ? scrubbed.slice(0, 117) + '…' : scrubbed;
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

// showStatus surfaces a humanized one-liner in the result card and the raw
// message in the Activity log (status may be a plain string, not an Error).
function showStatus(msg, cls, raw) {
  if (msg) setStatus(msg, cls);
  if (raw) appendLog(new Date(), cls === 'err' ? 'failed' : 'info', raw);
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
    setConn('error', 'err');
    appendLog(new Date(), 'failed', 'could not load genres: ' + e.message);
    setStatus('Could not load the app: ' + humanizeError(e), 'err');
    return;
  }
  setConn('connected', 'ok');
  renderGenreSelect();
  renderStyleSelect();
  loadModels();
  loadPresets();
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
  $('strength').oninput = () => { $('strength-val').textContent = $('strength').value; };
  $('composer').onsubmit = (e) => e.preventDefault();
  renderMode();
  $('generate').onclick = generate;
  $('cancel').onclick = () => cancelJob();
  $('clear-log').onclick = () => { logEl().innerHTML = '<div class="log-empty">No activity yet.</div>'; };
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
  none.textContent = 'Default';
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
  rm.type = 'button'; rm.textContent = '×'; rm.className = 'ghost'; rm.setAttribute('aria-label', 'Remove LoRA');
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
  uploaded.blend.forEach((_, i) => {
    const row = document.createElement('div');
    row.className = 'ref';
    const label = document.createElement('span');
    label.textContent = 'Ref ' + (i + 1);
    const w = document.createElement('input');
    w.type = 'range'; w.min = '0'; w.max = '1'; w.step = '0.05'; w.value = '1';
    w.dataset.idx = i;
    const rm = document.createElement('button');
    rm.type = 'button'; rm.textContent = '×'; rm.className = 'ghost'; rm.setAttribute('aria-label', 'Remove reference');
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
    if (!body.image) { showStatus('Upload an image to edit first.', 'err'); return; }
  } else if (mode === 'inpaint') {
    body.prompt = $('inpaint-prompt').value;
    body.image = uploaded.inpaint;
    if (!body.image || !maskStroke) { showStatus('Upload an image and paint a mask first.', 'err'); return; }
    body.mask = maskAsBase64();
  } else if (mode === 'outpaint') {
    body.prompt = $('outpaint-prompt').value;
    if (!outpaintImg) { showStatus('Upload an image to expand first.', 'err'); return; }
    Object.assign(body, buildOutpaint());
  } else if (mode === 'pose') {
    body.prompt = $('pose-prompt').value;
    body.size = $('pose-size').value;
    body.image = uploaded.pose;
    body.strength = parseFloat($('pose-strength').value);
    if (!body.image) { showStatus('Upload a pose reference first.', 'err'); return; }
  } else if (mode === 'blend') {
    body.prompt = $('blend-prompt').value;
    body.size = $('blend-size').value;
    body.images = uploaded.blend;
    body.strengths = Array.from(document.querySelectorAll('#refs input[type=range]')).map(w => parseFloat(w.value));
    if (!body.images.length) { showStatus('Upload at least one reference image.', 'err'); return; }
  } else if (mode === 'upscale') {
    body.image = uploaded.upscale;
    if (!body.image) { showStatus('Upload an image to upscale first.', 'err'); return; }
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
      setStatus('Queued ' + res.job_ids.length + ' images.', 'pending');
    } else {
      subscribe(res.job_id);
    }
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
    $('generate').disabled = false;
  }
}

function subscribe(id) {
  activeJobId = id;
  setStatus('Queued.', 'pending');
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
      setStatus('Complete.', 'ok');
      showImage(id);
      loadHistory();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status === 'failed') {
      showProgress(false);
      showStatus(ev.error ? humanizeError(new Error(ev.error)) : 'Generation failed.', 'err', ev.error);
      loadHistory();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status === 'cancelled') {
      showProgress(false);
      setStatus('Cancelled.', 'warn');
      loadHistory();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status) {
      setStatus(ev.status === 'generating' ? 'Generating…' : ev.status + '…', 'pending');
      showProgress(true, ev.status);
    }
  };
  es.onerror = () => { /* generation can take a long time; keep the stream open */ };
}

function showImage(id) {
  lastImageId = id;
  $('empty-state').hidden = true;
  $('image').hidden = false;
  $('image').src = '/api/images/' + id + '.png';
}

// showProgress toggles the inline progress bar inside the Result card. The
// bar shimmers while queued/enhancing and fills by step count while
// generating; when the job ends the previous image (or the empty state)
// returns.
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
      $('image').src = '/api/images/' + lastImageId + '.png';
      $('empty-state').hidden = true;
    } else {
      $('image').hidden = true;
      $('empty-state').hidden = false;
    }
  }
}

// updateProgress fills the Result bar for the active job and any matching
// in-flight history card.
function updateProgress(id, step, total) {
  const pct = total > 0 ? Math.round((step / total) * 100) : 0;
  if (id === activeJobId) {
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
  setStatus('Cancelling…', 'pending');
  try {
    await jsonFetch('/api/jobs/' + id + '/cancel', { method: 'POST' });
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
  }
}

// deleteJob is the one destructive action: it removes the job's history
// lines and stored PNG server-side, then refreshes the gallery.
async function deleteJob(id) {
  if (!confirm('Delete this image and its history entry? This cannot be undone.')) return;
  try {
    await jsonFetch('/api/jobs/' + id, { method: 'DELETE' });
    setStatus('Deleted ' + id + '.', 'ok');
    await loadHistory();
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
  }
}

const CARD_ACTIONS = (j) => [
  { label: 'View', fn: () => openDetail(j) },
  { label: 'Use as edit source', fn: () => useHistoryAsEdit(j.id) },
  { label: 'Upscale 4×', fn: () => useHistoryAsUpscale(j.id) },
  { label: 'Save to folder', fn: () => exportImage(j.id) },
  { label: 'Download', href: '/api/images/' + j.id + '.png?download=1' },
  { label: 'Delete', fn: () => deleteJob(j.id), danger: true },
];

function historyCard(j) {
  const card = document.createElement('div');
  card.className = 'card ' + j.status;
  card.dataset.id = j.id;
  const inflight = j.status === 'queued' || j.status === 'enhancing' || j.status === 'generating';

  if (inflight) {
    const ph = document.createElement('div');
    ph.className = 'card-thumb placeholder';
    ph.textContent = j.status + '…';
    card.appendChild(ph);
    const wrapper = document.createElement('div');
    wrapper.className = 'card-inflight';
    const prog = document.createElement('div');
    prog.className = 'progress-bar';
    const fill = document.createElement('div');
    fill.className = 'progress-fill';
    prog.appendChild(fill);
    wrapper.appendChild(prog);
    const cancelBtn = document.createElement('button');
    cancelBtn.type = 'button';
    cancelBtn.className = 'danger';
    cancelBtn.textContent = 'Cancel';
    cancelBtn.onclick = () => cancelJob(j.id);
    wrapper.appendChild(cancelBtn);
    card.appendChild(wrapper);
    return card;
  }

  if (j.image_path) {
    const thumb = document.createElement('img');
    thumb.className = 'card-thumb';
    thumb.src = '/api/images/' + j.id + '.png';
    thumb.loading = 'lazy';
    thumb.alt = j.mode || j.genre || 'image';
    thumb.onclick = () => openDetail(j);
    card.appendChild(thumb);

    const overlay = document.createElement('div');
    overlay.className = 'card-overlay';
    const badge = document.createElement('span');
    badge.className = 'badge ' + j.status;
    badge.textContent = j.status;
    overlay.appendChild(badge);

    const menu = document.createElement('details');
    menu.className = 'card-menu';
    const sum = document.createElement('summary');
    sum.textContent = '⋯';
    sum.setAttribute('aria-label', 'More actions');
    menu.appendChild(sum);
    const pop = document.createElement('div');
    pop.className = 'menu-pop';
    for (const it of CARD_ACTIONS(j)) {
      if (it.href) {
        const a = document.createElement('a');
        a.href = it.href;
        a.download = '';
        a.textContent = it.label;
        a.onclick = () => { menu.open = false; };
        pop.appendChild(a);
      } else {
        const b = document.createElement('button');
        b.type = 'button';
        if (it.danger) b.className = 'danger';
        b.textContent = it.label;
        b.onclick = () => { menu.open = false; it.fn(); };
        pop.appendChild(b);
      }
    }
    menu.appendChild(pop);
    overlay.appendChild(menu);
    card.appendChild(overlay);
    return card;
  }

  // Terminal without an image: failed or cancelled — show a human one-liner.
  const ph = document.createElement('div');
  ph.className = 'card-thumb placeholder';
  ph.textContent = j.status === 'failed' ? 'Failed' : 'Cancelled';
  card.appendChild(ph);
  if (j.error) {
    const err = document.createElement('div');
    err.className = 'card-error';
    err.textContent = humanizeError(new Error(j.error));
    card.appendChild(err);
  }
  return card;
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
  for (const j of jobs) el.appendChild(historyCard(j));
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
  setStatus('Resumed ' + id + '.', 'pending');
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

function metaRow(key, val) {
  const k = document.createElement('span');
  k.className = 'meta-key';
  k.textContent = key;
  const v = document.createElement('span');
  v.className = 'meta-val';
  v.textContent = val;
  const row = document.createElement('div');
  row.style.display = 'contents';
  row.appendChild(k); row.appendChild(v);
  return row;
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
  const meta = $('detail-meta');
  meta.innerHTML = '';
  const rows = [];
  if (j.prompt) rows.push(['Prompt', j.prompt]);
  if (j.model) rows.push(['Model', j.model]);
  if (j.loras && j.loras.length) rows.push(['LoRAs', j.loras.map(l => l.name).join(', ')]);
  if (j.style) rows.push(['Style', j.style]);
  if (j.genre) rows.push(['Genre', j.genre]);
  if (j.size) rows.push(['Size', j.size]);
  if (j.seed != null) rows.push(['Seed', j.seed]);
  if (j.created_at) rows.push(['Created', new Date(j.created_at).toLocaleString()]);
  for (const [k, v] of rows) meta.appendChild(metaRow(k, String(v)));

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
    showStatus(humanizeError(e), 'err', e.message);
    return;
  }
  const pv = $('edit-preview');
  pv.src = '/api/images/' + id + '.png';
  pv.hidden = false;
  setMode('edit');
  setStatus('Loaded image ' + id + ' for editing.', 'pending');
}

async function useHistoryAsUpscale(id) {
  $('detail').hidden = true;
  try {
    const resp = await fetch('/api/images/' + id + '.png');
    if (!resp.ok) throw new Error('image not found');
    uploaded.upscale = await blobToBase64(await resp.blob());
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
    return;
  }
  const pv = $('upscale-preview');
  pv.src = '/api/images/' + id + '.png';
  pv.hidden = false;
  setMode('upscale');
  setStatus('Loaded image ' + id + ' for upscaling.', 'pending');
}

async function exportImage(id) {
  try {
    const r = await jsonFetch('/api/export', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids: [id] }),
    });
    const dest = r.copied[0] || 'skipped';
    setStatus('Saved: ' + dest, r.copied.length ? 'ok' : 'err');
  } catch (e) {
    showStatus(humanizeError(e), 'err', e.message);
  }
}

$('detail-close').onclick = () => { $('detail').hidden = true; };
$('detail-edit').onclick = () => useHistoryAsEdit(detailJob.id);
$('detail-save').onclick = () => { if (detailJob) exportImage(detailJob.id); };

init();