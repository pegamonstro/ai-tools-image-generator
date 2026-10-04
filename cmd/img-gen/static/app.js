'use strict';

let genres = null;
let activeJobId = null;

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
  $('generate').onclick = generate;
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

async function generate() {
  const body = {
    genre: $('genre').value,
    fields: collectFields(),
    size: $('size').value,
    enhance: $('enhance').checked,
  };
  $('generate').disabled = true;
  setStatus('Submitting…', 'pending');
  try {
    const { job_id } = await jsonFetch('/api/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    subscribe(job_id);
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
    if (ev.status === 'done') {
      setStatus('complete', 'ok');
      showImage(id);
      loadHistory();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status === 'failed') {
      setStatus('failed' + (ev.error ? ': ' + ev.error : ''), 'err');
      loadHistory();
      es.close();
      activeJobId = null;
      $('generate').disabled = false;
    } else if (ev.status) {
      setStatus(ev.status, 'pending');
    }
  };
  es.onerror = () => { /* generation can take a long time; keep the stream open */ };
}

function showImage(id) {
  $('result').hidden = false;
  $('image').src = '/api/images/' + id + '.png';
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
    const head = document.createElement('div');
    head.className = 'card-head';
    const title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = j.genre;
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

init();
