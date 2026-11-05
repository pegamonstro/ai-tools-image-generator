'use strict';

/* the Assets & Animation panel — asset sets (element/kit/atlas/frame/bar/npc/
   sheet/character) and animations (from_frames / from_spec / composite /
   keyframes RIFE), wired to /api/v1/asset-sets and /api/v1/gifs. */

window.Compose = (function () {
  let assetMode = 'set';
  const KIND_FIELDS = {
    element: [],
    kit: [],
    atlas: [{ key: 'frames', label: 'Frames (2..64)', type: 'number', min: 2, max: 64, default: 4 }],
    frame: [{ key: 'frame_margins', label: 'Frame margins (1..size/2-1)', type: 'number', min: 1, default: 64 }],
    bar: [{ key: 'frame_margins', label: 'Bar margins', type: 'number', min: 1, default: 12 },
          { key: 'bar_fill', label: 'Bar fill', type: 'select', options: ['fill', 'stripes', 'gradient'] },
          { key: 'fill_states', label: 'Fill states (comma list, ≤16)', type: 'text', placeholder: '25,50,75,100' }],
    npc: [{ key: 'shape', label: 'Shape', type: 'select', options: ['variants', 'walk4'] },
          { key: 'variants', label: 'Variants (2..64 prompt suffixes, comma list)', type: 'text', placeholder: 'idle, walking' }],
    sheet: [{ key: 'views', label: 'Views (2..8, comma list)', type: 'text', placeholder: 'front, side, back' },
            { key: 'columns', label: 'Columns (0 = one row)', type: 'number', min: 0 }],
    character: [{ key: 'poses', label: 'Poses (comma list; or base64 via "pose photo:")', type: 'text', placeholder: 'idle, walk 1, walk 2' },
                { key: 'pose_image', label: 'Pose photo (base64 — needs DWPOSE_URL)', type: 'hidden' }],
  };

  function kindFields(kind) {
    const wrap = $('as-kind-fields');
    wrap.innerHTML = '';
    for (const f of (KIND_FIELDS[kind] || [])) {
      if (f.type === 'hidden') continue;
      const lab = el('label', 'ctrl', f.label);
      let inp;
      if (f.type === 'select') {
        inp = document.createElement('select');
        for (const o of (f.options || [])) {
          const oE = el('option'); oE.value = o; oE.textContent = o;
          inp.appendChild(oE);
        }
      } else {
        inp = document.createElement('input');
        inp.type = f.type === 'number' ? 'number' : 'text';
        if (f.min != null) inp.min = f.min;
        if (f.max != null) inp.max = f.max;
        if (f.default != null) inp.value = f.default;
        if (f.placeholder) inp.placeholder = f.placeholder;
      }
      inp.id = 'asf-' + f.key;
      inp.dataset.kind = kind;
      lab.appendChild(inp);
      wrap.appendChild(lab);
    }
    const kindOnly = kind;
    $('as-identity-wrap') && ($('as-identity-wrap').hidden = kind !== 'character');
  }

  function bindKindUI() {
    const idWrap = el('div', 'ctrl');
    idWrap.id = 'as-identity-wrap';
    idWrap.hidden = true;
    idWrap.innerHTML = '<span>Identity (character kind)</span>' +
      '<label class="upload-btn sm" for="as-identity">identity image…</label>' +
      '<input type="file" id="as-identity" accept="image/*" hidden><img id="as-identity-pv" class="preview" hidden>';
    $('as-kind-fields').after(idWrap);
    $('as-identity').onchange = async (e) => {
      if (e.target.files[0]) {
        window.__identityB64 = stripDataURL(await readFileAsDataURL(e.target.files[0]));
        const pv = $('as-identity-pv');
        pv.src = await readFileAsDataURL(e.target.files[0]);
        pv.hidden = false;
      }
    };
  }

  function collectSetBody() {
    const kind = $('as-kind').value;
    const name = $('as-name').value.trim();
    const seed = parseInt($('as-seed').value, 10);
    if (!name || isNaN(seed)) { showStatus('Set name + seed required.', 'err'); return null; }
    const asset = { kind, name: name, prompt: $('as-prompt').value.trim(), size: $('as-size').value.trim() };
    if (!asset.prompt) { showStatus('A prompt is required.', 'err'); return null; }
    for (const f of (KIND_FIELDS[kind] || [])) {
      if (f.type === 'hidden') continue;
      const inp = $('asf-' + f.key);
      if (!inp || inp.value === '') continue;
      if (f.key === 'variants' || f.key === 'views' || f.key === 'fill_states') {
        asset[f.key] = inp.value.split(',').map(s => s.trim()).filter(Boolean);
      } else if (f.type === 'number') {
        asset[f.key] = parseInt(inp.value, 10);
      } else {
        asset[f.key] = inp.value;
      }
    }
    if (kind === 'character') {
      asset.character_identity = window.__identityB64 || '';
      if (!asset.character_identity) {
        showStatus('character kind needs an identity image.', 'err');
        return null;
      }
    }
    const model = $('as-model').value;
    if (kind === 'character' && !model) {
      showStatus('character kind requires an SDXL-sidecar model.', 'err');
      return null;
    }
    const body = { name, seed, assets: [asset] };
    if (model) body.model = model;
    if ($('as-palette').checked) body.palette_lock = true;
    if ($('as-pixelfix').checked && ['element', 'atlas'].includes(kind)) asset.pixel_fix = true;
    const checks = [];
    document.querySelectorAll('#as-validate-checks .check-row').forEach(r => {
      const op = r.querySelector('select').value;
      const arg = r.querySelector('input').value.trim();
      if (arg) checks.push({ op, arg });
      else checks.push({ op });
    });
    if (checks.length) {
      body.validate = { checks, budget: 2, on_fail: 'deliver' };
      asset.validate = body.validate;
    }
    return body;
  }

  function submitSet() {
    const body = collectSetBody();
    if (!body) return;
    const btn = $('as-submit');
    btn.disabled = true;
    btn.textContent = 'Composing…';
    appendLog(new Date(), 'queued', 'asset set ' + body.name + ' submitted');
    (async () => {
      try {
        // The v1 wrapper blocks until composed (wait_seconds default), so the
        // panel polls the parent job by set_id instead and the UI stays live.
        const bodyAsync = Object.assign({}, body);
        delete bodyAsync.wait_seconds;
        const r = await jsonFetch('/api/v1/asset-sets', {
          method: 'POST', headers: POSTJSON, body: JSON.stringify(bodyAsync),
        });
        const sid = r.set_id;
        await pollSet(sid, body.name);
      } catch (e) {
        showStatus(humanizeError(e), 'err', e.message);
      }
      btn.disabled = false;
      btn.textContent = 'Request asset set';
    })();
  }

  async function pollSet(sid, name) {
    setStatus('Composing set…', 'pending');
    for (let i = 0; i < 480; i++) {
      const d = await jsonFetch('/api/v1/sets/' + sid);
      const st = d.job.status;
      setStatus('Composing set… (' + st + ')', 'pending');
      if (st === 'done') {
        setStatus('Set composed: ' + name, 'ok');
        showProgress(false);
        refreshLibrarySoon();
        const pri = (d.files || []).find(f => f.primary);
        if (pri) {
          const wrap = $('empty-state');
          wrap.hidden = true;
          const im = $('image');
          im.hidden = false;
          im.src = setFileUrl(sid, pri.name);
          im.onclick = () => window.Library.openSetModal(sid);
        }
        setTimeout(openSetModalSafe, 300, sid);
        return;
      }
      if (st === 'failed') {
        showStatus(humanizeError(new Error(d.job.error || 'compose failed')), 'err', d.job.error);
        return;
      }
    }
    showStatus('Set composition timed out — check the Library; it may land late.', 'warn');
  }
  function openSetModalSafe(sid) {
    window.Library && window.Library.openSetModal(sid);
  }

  /* ---------------- animation builder ---------------- */
  const GIF_SOURCES = ['from_frames', 'from_spec', 'composite', 'keyframes'];
  let gifMode = 'from_frames';

  function renderGifBody() {
    const wrap = $('gif-body');
    wrap.innerHTML = '';
    if (gifMode === 'from_frames') {
      const lab = el('label', 'ctrl', 'Frames (comma list of job ids)');
      const inp = document.createElement('input');
      inp.type = 'text'; inp.id = 'gf-frames';
      inp.placeholder = 'job id, job id, … (2..64; from the Library)';
      lab.appendChild(inp);
      inp.onclick = () => {};
      wrap.appendChild(lab);
      wrap.appendChild(el('div', 'hint', 'Tip: copy job ids from the Library\'s cards (the detail drawer).'));
    } else if (gifMode === 'from_spec') {
      const spec = el('label', 'ctrl', 'Base prompt (per-frame indices are appended automatically)');
      const ta = document.createElement('textarea');
      ta.rows = 3; ta.id = 'gf-spec';
      ta.placeholder = 'the character mid-turn, cel shading, consistent lighting';
      spec.appendChild(ta);
      wrap.appendChild(spec);
      const rowS = el('div', 'row');
      const countLab = el('label', 'ctrl', 'Frame count (2..64)');
      const countInp = document.createElement('input');
      countInp.type = 'number'; countInp.id = 'gf-count'; countInp.min = 2; countInp.max = 64; countInp.value = 4;
      countLab.appendChild(countInp);
      rowS.appendChild(countLab);
      const seedLab = el('label', 'ctrl', 'Seed (frames ladder seed+i)');
      const seedInp = document.createElement('input');
      seedInp.type = 'number'; seedInp.id = 'gf-seed'; seedInp.placeholder = 'random';
      seedLab.appendChild(seedInp);
      rowS.appendChild(seedLab);
      wrap.appendChild(rowS);
      const modelLab = el('label', 'ctrl');
      modelLab.append('Model');
      const sel = document.createElement('select');
      sel.id = 'gf-model';
      const nA = Object.assign(el('option'), { value: '', textContent: 'Default' });
      sel.appendChild(nA);
      for (const m of models.models) {
        const o = el('option'); o.value = m.key; o.textContent = m.label;
        sel.appendChild(o);
      }
      modelLab.appendChild(sel);
      wrap.appendChild(modelLab);
    } else if (gifMode === 'composite') {
      const animLab = el('label', 'ctrl', 'Animation frames (comma list of job ids)');
      const animInp = document.createElement('input');
      animInp.type = 'text'; animInp.id = 'gf-anim';
      animLab.appendChild(animInp);
      wrap.appendChild(animLab);
      const statLab = el('label', 'ctrl', 'Statics (jobId@x,y per layer, semicolon-separated)');
      const statInp = document.createElement('input');
      statInp.type = 'text'; statInp.id = 'gf-statics';
      statInp.placeholder = 'jobId@10,20; otherJobId@40,80';
      statLab.appendChild(statInp);
      wrap.appendChild(statLab);
    } else if (gifMode === 'keyframes') {
      wrap.appendChild(el('div', 'hint', 'Interpolates 2-16 anchor frames on the RIFE sidecar; (n-1)×multiplier+1 ≤ 64 total. Server submits honestly without a sidecar.'));
      const anchors = el('label', 'ctrl', 'Anchor job ids (2..16, comma list)');
      const anchorsInp = document.createElement('input');
      anchorsInp.type = 'text'; anchorsInp.id = 'gf-keyframes';
      anchorsLabFix(anchors);
      anchors.appendChild(anchorsInp);
      wrap.appendChild(anchors);
      const mult = el('label', 'ctrl', 'Multiplier (2..10)');
      const multInp = document.createElement('input');
      multInp.type = 'number'; multInp.id = 'gf-multiplier';
      multInp.min = 2; multInp.max = 10; multInp.value = 3;
      mult.appendChild(multInp);
      wrap.appendChild(mult);
    }
  }
  function anchorsLabFix(anchors) {
    anchors.classList.add('anchors-lab');
  }

  function buildGif() {
    const name = $('gif-name').value.trim();
    if (!name) { showStatus('Animation name required.', 'err'); return; }
    const src = { type: gifMode };
    if (gifMode === 'from_frames') {
      const ids = ($('gf-frames').value || '').split(',').map(s => s.trim()).filter(Boolean);
      if (ids.length < 2) { showStatus('Two or more frames required.', 'err'); return; }
      src.frames = ids.map(id => ({ job: id }));
    } else if (gifMode === 'from_spec') {
      const prompt = ($('gf-spec').value || '').trim();
      const count = parseInt(($('gf-count') || {}).value || '0', 10);
      const seedV = ($('gf-seed') || {}).value;
      if (!prompt) { showStatus('A base prompt is required.', 'err'); return; }
      if (!(count >= 2 && count <= 64)) { showStatus('Frame count: 2..64.', 'err'); return; }
      src.request = { prompt, size: '512x512', mode: 'asset' };
      if (seedV === '') {
        showStatus('A seed is required for generated frames (frames ladder seed+i).', 'err');
        return;
      }
      src.request.seed = parseInt(seedV, 10);
      src.count = count;
      if ($('gf-model').value) src.model = $('gf-model').value;
    } else if (gifMode === 'composite') {
      const anim = ($('gf-anim').value || '').split(',').map(s => s.trim()).filter(Boolean);
      const staticsText = ($('gf-statics').value || '').split(';').map(s => s.trim()).filter(Boolean);
      if (anim.length < 2) { showStatus('Two or more animation frames required.', 'err'); return; }
      src.composite = {
        anim: anim.map(id => ({ job: id })),
        statics: staticsText.map(s => {
          const [job, pos] = s.split('@');
          return { from: { job }, pos: (pos || '0,0').split(',').map(Number) };
        }),
      };
    } else if (gifMode === 'keyframes') {
      const anchors = ($('gf-keyframes').value || '').split(',').map(s => s.trim()).filter(Boolean);
      if (anchors.length < 2 || anchors.length > 16) { showStatus('2..16 anchors required.', 'err'); return; }
      src.keyframes = {
        anchors: anchors.map(id => ({ job: id })),
        multiplier: parseInt(($('gf-multiplier') || {}).value || '3', 10) || 3,
      };
    }
    const body = { name, delay_ms: parseInt($('gif-delay').value, 10) || 100, source: src };
    const btn = $('gif-build');
    btn.disabled = true;
    btn.textContent = 'Building…';
    (async () => {
      try {
        const r = await jsonFetch('/api/v1/gifs', { method: 'POST', headers: POSTJSON, body: JSON.stringify(body) });
        await pollSet(r.set_id, name);
      } catch (e) {
        showStatus(humanizeError(e), 'err', e.message);
      }
      btn.disabled = false;
      btn.textContent = 'Build animation';
    })();
  }

  /* ---------------- panel wiring ---------------- */
  function setAssetMode(m) {
    assetMode = m;
    document.querySelectorAll('#asset-tabs .chip').forEach(c =>
      c.setAttribute('aria-pressed', String(c.dataset.asset === m)));
    $('asset-set-form').hidden = m !== 'set';
    $('asset-gif-form').hidden = m !== 'gif';
    $('asset-studio-form').hidden = m !== 'studio';
    $('as-submit-row').hidden = m !== 'set';
    $('gif-build').hidden = m !== 'gif';
    if (m === 'gif') renderGifBody();
  }

  function initAssets() {
    bindKindUI();
    document.querySelectorAll('#asset-tabs .chip').forEach(c => {
      c.onclick = () => setAssetMode(c.dataset.asset);
    });
    $('as-kind').onchange = () => kindFields($('as-kind').value);
    kindFields('element');
    $('as-add-check').onclick = () => {
      window.__valChecks.push({ op: 'vlm_check', arg: '' });
      const wrap = $('as-validate-checks');
      wrap.innerHTML = '';
      for (const c of window.__valChecks.slice(-3)) {
        const rowE = el('div', 'check-row');
        const selE = document.createElement('select');
        for (const op of ['vlm_check', 'tag_roundtrip']) {
          const o = el('option'); o.value = op; o.textContent = op;
          if (op === c.op) o.selected = true;
          selE.appendChild(o);
        }
        rowE.appendChild(selE);
        const argE = document.createElement('input');
        argE.placeholder = 'criterion…';
        argE.value = c.arg || '';
        rowE.appendChild(argE);
        wrap.appendChild(rowE);
      }
    };
    $('as-submit').onclick = submitSet;
    $('gif-source').onchange = () => { gifMode = $('gif-source').value; renderGifBody(); };
    $('gif-build').onclick = buildGif;
    renderGifBody();
  }

  function renderRefs() {
    const wrap = $('refs');
    wrap.innerHTML = '';
    uploaded.blend.forEach((b64, i) => {
      const rowE = el('div', 'check-row');
      const img = el('img');
      img.className = 'preview';
      img.src = 'data:image/png;base64,' + b64;
      img.style.maxHeight = '48px';
      const lab = el('label');
      lab.append(' strength ');
      const inp = document.createElement('input');
      inp.type = 'range'; inp.min = '0'; inp.max = '1'; inp.step = '0.05'; inp.value = '0.4';
      lab.appendChild(inp);
      rowE.appendChild(img);
      rowE.appendChild(lab);
      wrap.appendChild(rowE);
    });
  }

  if (document.readyState !== 'loading') initAssets();
  else document.addEventListener('DOMContentLoaded', initAssets);

  return { initAssets, renderRefs, setAssetMode, kindFields };
})();
