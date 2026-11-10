'use strict';

/* Quick — one-field one-button workflows. The complex pipeline runs under the
   hood through window.RefineAPI (the same refine loop the Pro panel exposes):
   enhance → generate → judge → segment-then-retouch → re-judge.
   Simple mode: one prompt box + Generate. Pro: the tab strip + composer. */

window.Fast = (function () {
  const WORKFLOWS = [
    { key: 'glamour', label: 'Glamour portrait', desc: 'photon-noir grade, one-button',
      genre: 'character', preset: 'photon-noir', model: 'persephone', size: '1024x1536',
      loras: [{ name: 'fncb', scale: 0.9 }, { name: 'uncensored', scale: 0.7 }],
      refine: 2, fields: { subject: '@', outfit: 'evening wear', pose: 'standing, weight even', expression: 'calm confident smile', setting: 'studio backdrop, a single light shaft' } },
    { key: 'beach', label: '90s beach', desc: 'palms + surf at dusk neon',
      genre: 'scene', preset: '', model: 'persephone', size: '1024x1024',
      loras: [{ name: 'fncb', scale: 0.9 }, { name: 'uncensored', scale: 0.7 }],
      refine: 2, fields: { description: '@. 90s beach scene, palm trees, rolling surf, dusk neon, vice-city mood', camera: 'wide' } },
    { key: 'pulp', label: 'Pulp cel', desc: 'pulp90s trained look, sharp detail',
      genre: 'scene', preset: '', model: 'persephone', size: '1024x1024',
      loras: [{ name: 'pulp90s', scale: 0.9 }, { name: 'uncensored', scale: 0.7 }],
      refine: 2, fields: { description: '@. pulp90s 1990s cel style, high detail, vice-city palette' } },
    { key: 'noir', label: 'Photon Noir', desc: 'the locked favorite, 1940s B&W',
      genre: 'scene', preset: 'photon-noir', model: 'persephone', size: '1024x1024',
      loras: [{ name: 'fncb', scale: 0.85 }],
      refine: 2, fields: { description: '@. monochrome 1940s film noir, one red accent, hard venetian-blind shadow slices, smoke in a single light shaft', mood: 'rain-soaked menace', camera: 'low angle, 35mm' } },
    { key: 'kit', label: 'Asset kit', desc: '9-slice panel + rembg badge, palette-locked',
      kind: 'kit-mini' },
  ];

  let wf = WORKFLOWS[0];
  let running = false;
  let styleFamily = 'comic';
  try { styleFamily = localStorage.getItem('imggen-style') || 'comic'; } catch (_) {} // comic | photoreal — swaps the noir style skin over the same workflows

  function view() { return document.getElementById('view-fast'); }

  function render() {
    const v = view();
    if (!v) return;
    v.innerHTML = `
      <div class="fast-wrap">
        <div class="fast-cards">
          ${WORKFLOWS.map(w => `
            <button type="button" class="fast-card ${w.key === wf.key ? 'sel' : ''}" data-k="${w.key}">
              <span class="fast-card-label">${w.label}</span>
              <span class="fast-card-desc">${w.desc}</span>
            </button>`).join('')}
        </div>
        <div class="fast-main panel">
          <h2 class="sec-label">${wf.label}</h2>
          <label class="ctrl">What do you want to see?
            <textarea id="fast-prompt" rows="3" placeholder="describe it in your own words — the pipeline handles the rest"></textarea>
          </label>
          <div class="seg-row" id="fast-style-row">
            <div class="seg" id="fast-style-seg" role="group" aria-label="Rendering style">
              <button class="seg-btn" data-style="comic" aria-pressed="${styleFamily === 'comic'}">Comic noir</button>
              <button class="seg-btn" data-style="photoreal" aria-pressed="${styleFamily === 'photoreal'}">Photoreal</button>
            </div>
          </div>
          <div class="fast-meta" id="fast-meta">${wf.kind === 'kit-mini'
            ? 'Two small assets, palette lock, cutout badge included, one set in the Library.'
            : 'Identity LoRA · uncensored · enhance · auto-refine (judge → segment-then-retouch) · photon-grade preset'}</div>
          <button id="fast-go" class="primary block big" type="button">Generate</button>
          <div id="fast-status" class="status" hidden></div>
          <div class="fast-hint">Everything lands in the Library with full metadata.
            <a href="#" id="fast-audit">Pro view →</a></div>
        </div>
        <div class="fast-out" id="fast-out"></div>
      </div>`;
    v.querySelectorAll('.fast-card').forEach(c => {
      c.onclick = () => {
        wf = WORKFLOWS.find(w => w.key === c.dataset.k);
        if (window.location.hash === '#fast') render(); else setView('fast');
      };
    });
    $('fast-go').onclick = run;
    const seg = $('fast-style-seg');
    if (seg) seg.querySelectorAll('.seg-btn').forEach(b => b.onclick = () => {
      styleFamily = b.dataset.style;
      try { localStorage.setItem('imggen-style', styleFamily); } catch (_) {}
      render();
    });
    $('fast-audit').onclick = (e) => { e.preventDefault(); setView('compose'); };
    const saved = (window.__fastPrompt || '').trim();
    if (saved) $('fast-prompt').value = saved;
    window.__fastPrompt = '';
  }

  /* The composer's workflow strip: same workflows, one click away in Pro. */
  function renderStrip() {
    const strip = document.getElementById('workflow-strip');
    if (!strip || running) return;
    strip.innerHTML = '';
    for (const w of WORKFLOWS) {
      const b = el('button', 'chip', w.label);
      b.type = 'button';
      b.style.textTransform = 'none';
      b.onclick = () => {
        wf = w;
        setView('fast');
      };
      strip.appendChild(b);
    }
  }

  function fastStatus(msg, cls) {
    const s = document.getElementById('fast-status');
    if (!s) return;
    s.hidden = !msg;
    s.textContent = msg || '';
    s.className = 'status ' + (cls || 'pending');
  }

  /* asset-set polling, same contract compose.js uses */
  async function pollSet(sid) {
    for (let i = 0; i < 200; i++) {
      await new Promise(r => setTimeout(r, 4000));
      const d = await jsonFetch('/api/v1/sets/' + sid);
      const st = (d.job || {}).status;
      if (st === 'done') return d;
      if (['failed', 'cancelled'].includes(st)) throw new Error((d.error || st));
    }
    throw new Error('set timed out');
  }

  async function runKit(text) {
    fastStatus('Composing asset kit…', 'pending');
    const body = {
      name: 'quick-kit-' + (Date.now() % 100000),
      seed: 73000 + (Date.now() % 1000),
      model: 'sdxl-illustrious-v2',
      palette_lock: true,
      assets: [
        { kind: 'frame', name: 'panel',
          prompt: text + ' — 90s beach HUD panel frame, chrome corners, neon edge light',
          size: '512x512', frame_margins: 96 },
        { kind: 'element', name: 'badge',
          prompt: text + ' — badge emblem, bold line art, plain background',
          size: '256x256', transparent: 'rembg' },
      ],
    };
    let r;
    try {
      r = await jsonFetch('/api/v1/asset-sets', { method: 'POST', headers: POSTJSON, body: JSON.stringify(body) });
    } catch (e) {
      fastStatus(humanizeError(e), 'err');
      throw e;
    }
    const d = await pollSet(r.set_id);
    refreshLibrarySoon();
    if (window.Library && Library.openSetModal) Library.openSetModal(r.set_id);
    return ((d.files || []).length + ' files');
  }

  async function runImage(text) {
    const fields = {};
    for (const [k, v0] of Object.entries(wf.fields)) {
      fields[k] = v0.replace(/@/, text).trim();
    }
    const body = {
      genre: wf.genre,
      fields,
      size: wf.size,
      mode: 'generate',
      model: wf.model,
      loras: wf.loras,
      enhance: true, // steps 1–2: the uncensored LLM turns the request into the visual prompt
      seed: 74000 + (Date.now() % 900),
      negative_prompt: 'worst quality, low quality, bad anatomy, blurry, watermark, text',
      batch: 1,
    };
    if (wf.preset) body.preset = wf.preset;
    if (styleFamily === 'photoreal') {
      body.fields.description += ' Photorealistic photographic rendering, 35mm film grain, natural skin texture, real light discipline.';
      const negs = (body.negative_prompt || '').split(',');
      negs.push('cartoon', 'anime', 'comic art', 'illustration', 'cel shading', 'ink outlines');
      body.negative_prompt = negs.join(',').replace(/\s*,\s*/g, ',').trim();
    } else {
      body.negative_prompt += ', photorealistic photograph, 3d render';
    }
    body.negative_prompt = Array.from(new Set(body.negative_prompt.split(',').map(t => t.trim()).filter(Boolean))).join(',');
    let first;
    let poller = null;
    try {
      first = await RefineAPI.renderJob(body, (jid) => {
        poller = setInterval(async () => {
          try {
            const d = await jsonFetch('/api/jobs/' + jid);
            const extra = d.step != null ? ' ' + Math.round(d.step * 100) + '%' : '';
            if (d.status && !['done', 'failed', 'cancelled'].includes(d.status))
              fastStatus('Rendering — ' + d.status + extra + ' (this takes a few minutes)', 'pending');
            else clearInterval(poller), (poller = null);
          } catch (_) { /* transient */ }
        }, 5000);
      }); // 429 cool-downs handled inside
    } catch (e) {
      if (poller) clearInterval(poller);
      fastStatus(humanizeError(e), 'err');
      throw e;
    }
    if (poller) { clearInterval(poller); poller = null; }
    if (first.status !== 'done') throw new Error(first.error || first.status);
    fastStatus('Rendered ' + first.id + ' — judging…', 'pending');
    appendLog(new Date(), 'ok', 'quick render ' + first.id);
    const out = await RefineAPI.refineLoop(first.id, body, {
      maxPasses: wf.refine,
      refineModel: wf.model,
      segment: true, // segment-then-retouch via the rembg sidecar
      silent: true,
    });
    const finalId = (out && out.finalId) || first.id;
    appendLog(new Date(), 'ok', 'quick workflow done → ' + finalId);
    const card = el('div', 'card quick-result');
    card.onclick = () => { if (window.Library && Library.openSetModal) setView('library'); };
    const tw = card.appendChild(el('div', 'thumb-wrap'));
    const im = tw.appendChild(el('img'));
    im.src = imgUrl(finalId);
    im.alt = wf.label;
    document.getElementById('fast-out').prepend(card);
    refreshLibrarySoon();
    return 'refined → ' + finalId;
  }

  async function run() {
    if (running) return;
    const box = document.getElementById('fast-prompt');
    const text = (box && box.value || window.__fastPrompt || '').trim();
    if (!text) { fastStatus('Describe it first, one line is enough.', 'err'); return; }
    window.__fastPrompt = text;
    running = true;
    $('fast-go').disabled = true;
    $('fast-go').textContent = 'Working…';
    try {
      const note = wf.kind === 'kit-mini' ? await runKit(text) : await runImage(text);
      fastStatus('Done. ' + (wf.kind === 'kit-mini' ? note : note + ' — view it in the Library.'), 'ok');
    } catch (e) {
      if (e.message) fastStatus(humanizeError(e), 'err');
    }
    $('fast-go').disabled = false;
    $('fast-go').textContent = 'Generate';
    running = false;
  }

  function boot() {
    renderStrip();
    if (view() && (!window.location.hash || window.location.hash === '#fast')) render();
  }
  if (document.readyState !== 'loading') boot();
  else document.addEventListener('DOMContentLoaded', boot);

  return { render, renderStrip, boot, selectWF: (k) => { wf = WORKFLOWS.find(w => w.key === k) || wf; } };
})();
