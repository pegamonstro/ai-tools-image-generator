'use strict';

/* the Studio — Validate (critic checklist: vlm_check / tag_roundtrip /
   palette_conform / tile_conform) and Repair (pixel-art quantizer), against
   /api/v1/validate and /api/v1/repair. Honest limits stated inline. */

window.addEventListener('DOMContentLoaded', () => {
  /* ---------------- validate ---------------- */
  let valImage = null;
  const CHECK_OPS = ['vlm_check', 'tag_roundtrip', 'palette_conform', 'tile_conform'];
  window.__valChecks = [{ op: 'vlm_check', arg: '' }];

  const checksWrap = window.__valChecksRoot = $('val-checks');

  function renderChecks() {
    checksWrap.innerHTML = '';
    for (const [i, c] of window.__valChecks.entries()) {
      const row = document.createElement('div');
      row.className = 'check-row';
      const sel = document.createElement('select');
      for (const op of CHECK_OPS) {
        const o = el('option'); o.value = op; o.textContent = op;
        if (op === c.op) o.selected = true;
        sel.appendChild(o);
      }
      sel.onchange = () => { c.op = sel.value; };
      row.appendChild(sel);
      const arg = document.createElement('input');
      arg.placeholder = c.op === 'vlm_check' ? 'criterion…' :
        c.op === 'tile_conform' ? '{"max_ratio":1.3} (object, optional)' :
        c.op === 'palette_conform' ? 'palette below' : 'no argument';
      arg.value = c.arg || '';
      arg.dataset.i = i;
      arg.oninput = (e) => { window.__valChecks[+e.target.dataset.i].arg = e.target.value; };
      row.appendChild(arg);
      const rm = document.createElement('button');
      rm.className = 'rm'; rm.type = 'button'; rm.textContent = '×';
      rm.onclick = () => {
        window.__valChecks.splice(i, 1);
        if (!window.__valChecks.length) window.__valChecks.push({ op: 'vlm_check', arg: '' });
        renderChecks();
      };
      row.appendChild(rm);
      checksWrap.appendChild(row);
    }
    $  ('val-image') && ($('validate-run').hidden = !valImage);
  }
  window.addEventListener('DOMContentLoaded', () => {});

  $('add-check').onclick = () => {
    window.__valChecks.push({ op: 'vlm_check', arg: '' });
    renderChecks();
  };
  $('val-image').onchange = async (e) => {
    if (!e.target.files[0]) return;
    valImage = stripDataURL(await readFileAsDataURL(e.target.files[0]));
    const pv = $('val-preview');
    pv.src = await readFileAsDataURL(e.target.files[0]);
    pv.hidden = false;
    $('validate-run').hidden = false;
  };
  $('validate-run').onclick = async () => {
    const btn = $('validate-run');
    btn.disabled = true;
    btn.textContent = 'Judging…';
    const checks = window.__valChecks.filter(c => c.op).map(c => {
      if (!c.arg) return { op: c.op };
      return c.op === 'tile_conform' && c.arg.trim().startsWith('{')
        ? { op: c.op, arg: JSON.parse(c.arg) }
        : { op: c.op, arg: c.arg };
    });
    const palette = (window.__valPalette || []).filter(Boolean);
    const body = { image: valImage, checks };
    if (checks.some(c => c.op === 'palette_conform') && palette.length) body.palette = palette;
    try {
      const r = await jsonFetch('/api/v1/validate', { method: 'POST', headers: POSTJSON, body: JSON.stringify(body) });
      const box = $('val-result');
      box.hidden = false;
      box.innerHTML = '';
      box.appendChild(el('div', 'vrow', 'verdict: ' + (r.verdict || '—')));
      for (const c of (r.checks || [])) {
        const rowE = el('div', 'vrow');
        rowE.appendChild(el('span', 'vop', c.op));
        rowE.appendChild(el('span', 'vstatus ' + c.status, c.status));
        rowE.appendChild(el('span', 'vreason', (c.reason || '') + (c.suggestion ? ' → ' + c.suggestion : '')));
        box.appendChild(rowE);
      }
      appendLog(new Date(), r.verdict === 'failed' ? 'failed' : 'info', 'validate: ' + r.verdict);
    } catch (e) {
      showStatus(humanizeError(e), 'err', e.message);
    }
    btn.disabled = false;
    btn.textContent = 'Run validation';
  };
  window.__valChecksRoot = checksWrap;
  renderChecks();

  /* ---------------- repair ---------------- */
  let repImage = null;
  $('rep-image').onchange = async (e) => {
    if (!e.target.files[0]) return;
    repImage = stripDataURL(await readFileAsDataURL(e.target.files[0]));
    const pv = $('rep-preview');
    pv.src = await readFileAsDataURL(e.target.files[0]);
    pv.hidden = false;
    $('repair-run').hidden = false;
  };
  $('repair-run').onclick = async () => {
    const btn = $('repair-run');
    btn.disabled = true;
    btn.textContent = 'Quantizing…';
    const body = { image: repImage };
    if ($('rep-cell').value) body.cell = parseInt($('rep-cell').value, 10);
    if ($('rep-colors').value) body.colors = parseInt($('rep-colors').value, 10);
    try {
      const r = await fetch('/api/v1/repair', { method: 'POST', headers: POSTJSON, body: JSON.stringify(body) });
      if (!r.ok) {
        const t = await r.text();
        throw new Error(envelopeMessage(t) || ('http ' + r.status));
      }
      const blob = await r.blob();
      const out = $('rep-out');
      const url = window.URL.createObjectURL(blob);
      out.src = url;
      $('rep-download').href = url;
      $('rep-result').hidden = false;
      setStatus('Repaired.', 'ok');
    } catch (e) {
      showStatus(humanizeError(e), 'err', e.message);
    }
    btn.disabled = false;
    btn.textContent = 'Repair';
  };

  window.Studio = { onUploadReady: () => {} };
});
