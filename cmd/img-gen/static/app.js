let genres = null;

async function jsonFetch(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

async function init() {
  genres = await jsonFetch('/api/genres');
  renderGenreSelect();
  document.getElementById('generate').onclick = generate;
  loadHistory();
}

function renderGenreSelect() {
  const sel = document.getElementById('genre');
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
  const g = genres.genres[document.getElementById('genre').value];

  const fieldsEl = document.getElementById('fields');
  fieldsEl.innerHTML = '';
  for (const f of g.fields) fieldsEl.appendChild(renderField(f));

  const sizeEl = document.getElementById('size');
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

function setStatus(msg) {
  const el = document.getElementById('status');
  el.hidden = !msg;
  el.textContent = msg;
}

async function generate() {
  const genre = document.getElementById('genre').value;
  const body = {
    genre,
    fields: collectFields(),
    size: document.getElementById('size').value,
    enhance: document.getElementById('enhance').checked,
  };
  try {
    const { job_id } = await jsonFetch('/api/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    subscribe(job_id);
  } catch (e) {
    setStatus('error: ' + e.message);
  }
}

function subscribe(id) {
  setStatus('queued');
  const es = new EventSource('/api/jobs/' + id + '/events');
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    setStatus(ev.status + (ev.error ? ': ' + ev.error : ''));
    if (ev.status === 'done') {
      showImage(id);
      loadHistory();
      es.close();
    } else if (ev.status === 'failed') {
      loadHistory();
      es.close();
    }
  };
  es.onerror = () => { /* keep waiting; generation can take a long time */ };
}

function showImage(id) {
  document.getElementById('result').hidden = false;
  document.getElementById('image').src = '/api/images/' + id + '.png';
}

async function loadHistory() {
  const jobs = await jsonFetch('/api/jobs');
  const el = document.getElementById('history');
  el.innerHTML = '';
  for (const j of jobs) {
    const card = document.createElement('div');
    card.className = 'card ' + j.status;
    const title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = j.genre + ' · ' + j.status;
    card.appendChild(title);
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
}

init();
