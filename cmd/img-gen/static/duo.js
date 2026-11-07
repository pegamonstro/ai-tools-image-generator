'use strict';

/* Regional duo — two figures, one canvas, per-figure prompt regions.
   mode "regional" on /api/v1/jobs: region 0 renders the base (figure A),
   regions 1..N inpaint their rect with figure B's own prompt. The rect layout
   presets + occupancy-safe masking keep figure A outside the repaint. */

(function () {
  const LAYOUTS = {
    'right': { label: 'A left — B right', rect: (w, h) => [Math.round(w * 0.56), Math.round(h * 0.06), Math.round(w * 0.42), Math.round(h * 0.94)], exclude: false },
    'left':  { label: 'B left — A right', rect: (w, h) => [Math.round(w * 0.02), Math.round(h * 0.06), Math.round(w * 0.42), Math.round(h * 0.94)], exclude: false },
    'behind': { label: 'B behind A (occupancy-safe)', rect: (w, h) => [Math.round(w * 0.5), Math.round(h * 0.08), Math.round(w * 0.48), Math.round(h * 0.92)], exclude: true },
    'bandright': { label: 'B beside A (occupancy-safe, right band)', rect: (w, h) => [Math.round(w * 0.6), Math.round(h * 0.1), Math.round(w * 0.36), Math.round(h * 0.9)], exclude: true },
  };

  function buildPanel() {
    const host = document.createElement('details');
    host.className = 'csec details';
    host.dataset.adv = '0';
    host.id = 'duo-panel';
    host.innerHTML = `
      <summary>Compose two figures (regional prompts)</summary>
      <div class="details-body">
        <div class="row">
          <label class="ctrl" style="flex:1">Figure A (base) prompt
            <textarea id="duo-base" rows="3" placeholder="the woman kneeling upright on the chaise, compact in the left third, full body visible"></textarea>
          </label>
          <label class="ctrl" style="flex:1">Figure B prompt
            <textarea id="duo-b" rows="3" placeholder="the man lying on his back beneath and to the right of her, muscular build, full figure"></textarea>
          </label>
        </div>
        <div class="row">
          <label class="ctrl">B region
            <select id="duo-layout"></select>
          </label>
          <label class="ctrl">Model
            <select id="duo-model"></select>
          </label>
        </div>
        <div class="row">
          <label class="ctrl">Size
            <select id="duo-size">
              <option value="512x768" selected>512x768</option>
              <option value="768x512">768x512</option>
              <option value="640x960">640x960</option>
            </select>
          </label>
          <label class="ctrl">Seed <input type="number" id="duo-seed" placeholder="random"></label>
          <label class="ctrl">B fill strength <input type="number" id="duo-fill" min="0.1" max="1" step="0.05" value="1"></label>
          <label class="toggle" style="align-self:center">
            <input type="checkbox" id="duo-judge" checked>
            <span class="toggle-track" aria-hidden="true"></span>
            Judge anatomy
          </label>
        </div>
        <div class="row">
          <canvas id="duo-map" width="128" height="192" style="border:1px solid var(--line,#444); border-radius:6px; image-rendering:auto"></canvas>
          <p class="hint" style="flex:1">Region 0 renders figure A; region 1 repaints only the marked rect with figure B's
          prompt (masked inpaint — the kept latent clamps to the base). Occupancy-safe layouts carve the base figure's
          silhouette (rembg) out of the repaint region so B cannot cut A. Needs an SDXL-sidecar model.</p>
        </div>
        <div class="row">
          <button class="btn" id="duo-submit" type="button">Render duo</button>
        </div>
      </div>`;
    const genControls = document.getElementById('gen-controls');
    genControls.appendChild(host);
    const sel = document.getElementById('duo-layout');
    for (const [key, l] of Object.entries(LAYOUTS)) {
      const o = document.createElement('option');
      o.value = key; o.textContent = l.label;
      sel.appendChild(o);
    }
    // app.js loads the model catalog asynchronously; fill on arrival.
    const fillModels = () => {
      const modelSel = document.getElementById('duo-model');
      if (!modelSel || !models || !models.models) return;
      const keep = modelSel.value;
      modelSel.innerHTML = '';
      for (const m of models.models.filter(m => (m.sidecar || '') === 'sdxl')) {
        const o = document.createElement('option');
        o.value = m.key; o.textContent = m.label;
        modelSel.appendChild(o);
      }
      if (keep) modelSel.value = keep;
    };
    const catalogTimer = setInterval(() => {
      fillModels();
      if (models && models.models && document.getElementById('duo-model').options.length) clearInterval(catalogTimer);
    }, 250);
    setTimeout(() => clearInterval(catalogTimer), 30000);
    fillModels();
    drawMap();
    document.getElementById('duo-layout').onchange = drawMap;
    document.getElementById('duo-size').onchange = drawMap;
    document.getElementById('duo-submit').onclick = submitDuo;
  }

  function drawMap() {
    const cv = document.getElementById('duo-map');
    const sizeV = document.getElementById('duo-size').value.split('x');
    const w = parseInt(sizeV[0], 10), h = parseInt(sizeV[1], 10);
    const [rx, ry, rw, rh] = rectFor(w, h).rect;
    const ctx = cv.getContext('2d');
    ctx.clearRect(0, 0, cv.width, cv.height);
    ctx.fillStyle = '#333';
    ctx.fillRect(0, 0, cv.width, cv.height);
    ctx.fillStyle = '#777';
    ctx.fillRect(4, 4, cv.width * 0.35, cv.height - 8); // figure A zone (approx)
    ctx.strokeStyle = '#e33';
    ctx.lineWidth = 2;
    const sx = cv.width / w, sy = cv.height / h;
    ctx.strokeRect(rx * sx, ry * sy, rw * sx, rh * sy);
    ctx.fillStyle = '#e33';
    ctx.font = '10px monospace';
    ctx.fillText('B region', rx * sx + 4, (ry + 12) * sy);
  }

  function rectFor(w, h) {
    const lay = LAYOUTS[document.getElementById('duo-layout').value] || LAYOUTS.right;
    return { rect: lay.rect(w, h), exclude: lay.exclude };
  }

  const CHECK = 'Are the figures anatomically and visually consistent (no blur, no overlaps, no fused ' +
    'or extra limbs, natural proportions)? Answer pass if fully consistent. In the fix field ' +
    'put a one-sentence correction and the region {"bbox":[x,y,w,h]}.';

  function submitDuo() {
    const base = document.getElementById('duo-base').value.trim();
    const bPrompt = document.getElementById('duo-b').value.trim();
    if (!base || !bPrompt) { setStatus('Both figure prompts are required.', 'err'); return; }
    const model = document.getElementById('duo-model').value;
    if (!model) { setStatus('Pick an SDXL-sidecar model.', 'err'); return; }
    const size = document.getElementById('duo-size').value;
    const { rect, exclude } = rectFor(parseInt(size.split('x')[0], 10), parseInt(size.split('x')[1], 10));
    const seedV = document.getElementById('duo-seed').value;
    const fill = parseFloat(document.getElementById('duo-fill').value) || 1;
    const body = {
      mode: 'regional', model, size, batch: 1, enhance: false,
      regions: [
        { prompt: base },
        { prompt: bPrompt, rect: rect.slice(), strength: fill, exclude_occupancy: exclude },
      ],
    };
    if (seedV !== '') body.seed = parseInt(seedV, 10);
    const btn = document.getElementById('duo-submit');
    btn.disabled = true;
    btn.textContent = 'Rendering…';
    (async () => {
      try {
        const r = await jsonFetch('/api/v1/jobs', { method: 'POST', headers: POSTJSON, body: JSON.stringify(body) });
        await pollDuo(r.job_id, body);
      } catch (e) {
        showStatus(humanizeError(e), 'err', e.message);
      }
      btn.disabled = false;
      btn.textContent = 'Render duo';
    })();
  }

  async function pollDuo(jid, body) {
    setStatus('Rendering duo…', 'pending');
    showProgress(true);
    for (let i = 0; i < 900; i++) {
      let j;
      try {
        j = await jsonFetch('/api/v1/jobs/' + jid);
      } catch (e) { await new Promise(r => setTimeout(r, 2000)); continue; }
      if (j.step && j.total) setStatus(`Rendering duo… (step ${j.step}/${j.total})`, 'pending');
      else if (j.status === 'generating') setStatus('Rendering duo… (region fill stage)', 'pending');
      if (j.status === 'done') {
        lastImageId = jid; // implicit global the app.js progress tail reads; see refine.js
        showProgress(false);
        const img = document.getElementById('image');
        const wrap = document.getElementById('empty-state');
        if (wrap) wrap.hidden = true;
        if (img) { img.hidden = false; img.src = imgUrl(jid); }
        refreshLibrarySoon();
        if (document.getElementById('duo-judge').checked) {
          setStatus('Duo rendered — judging anatomy…', 'pending');
          judgeDuo(jid);
        } else {
          setStatus('Duo rendered.', 'ok');
        }
        appendLog(new Date(), 'ok', 'regional duo job ' + jid);
        return;
      }
      if (j.status === 'failed' || j.status === 'cancelled') {
        showProgress(false);
        showStatus(humanizeError(new Error(j.error || 'duo failed')), 'err', j.error);
        return;
      }
      await new Promise(r => setTimeout(r, 2000));
    }
    showProgress(false);
    setStatus('Duo job timed out — check the Library.', 'warn');
  }

  async function judgeDuo(jid) {
    try {
      const r = await fetch(imgUrl(jid));
      const blob = await r.blob();
      const b64 = await blobToBase64(blob);
      const v = await jsonFetch('/api/v1/validate', {
        method: 'POST', headers: POSTJSON,
        body: JSON.stringify({ image: b64, checks: [{ op: 'vlm_check', arg: CHECK }] }),
      });
      const c = (v.checks || [])[0] || {};
      if (v.verdict === 'pass') setStatus('Duo judged: pass — anatomy consistent.', 'ok');
      else setStatus('Duo judge: ' + (v.verdict || 'indeterminate') + ' — ' + ((c.fix || c.reason || '').slice(0, 120)), v.verdict === 'failed' ? 'err' : 'warn');
    } catch (e) {
      appendLog(new Date(), 'warn', 'duo judge failed: ' + e.message);
    }
  }

  if (document.readyState !== 'loading') { buildPanel(); }
  else document.addEventListener('DOMContentLoaded', buildPanel);
})();
