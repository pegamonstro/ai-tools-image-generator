'use strict';

/* Refine loop — multi-pass generate→critique→localized-inpaint→re-critique.
   Uses only existing endpoints: /api/v1/jobs (generate/edit/inpaint), /api/v1/validate. */

(function () {
  let refining = false;

  function buildPanel() {
    const host = document.createElement('details');
    host.className = 'csec details';
    host.id = 'refine-panel';
    host.innerHTML = `
      <summary>Refine loop (multi-pass critique + localized retouch)</summary>
      <div class="details-body">
        <label class="toggle">
          <input type="checkbox" id="refine-on">
          <span class="toggle-track" aria-hidden="true"></span>
          Auto-refine: judge → fix region → inpaint → re-judge (until pass or max passes)
        </label>
        <div class="row">
          <label class="ctrl">Max passes
            <input type="number" id="refine-max" min="1" max="6" value="3">
          </label>
          <label class="ctrl">Refine model (passes ≥1)
            <select id="refine-model"></select>
          </label>
        </div>
        <label class="toggle">
          <input type="checkbox" id="refine-segment" checked>
          <span class="toggle-track" aria-hidden="true"></span>
          Segment-then-retouch (rembg cutout before inpainting)
        </label>
        <p class="hint">Pass 0 renders from the base prompt (enhanced by the LLM). Each later pass inpaints only
        the region the VLM flags, with the judge's correction text. Every pass is a normal job — all of them
        land in the Library. Scaffold from a vanilla model via the base Model select; retouch with the refine model + LoRAs.</p>
      </div>`;
    const genControls = document.getElementById('gen-controls');
    genControls.appendChild(host);
    const sel = document.getElementById('refine-model');
    for (const m of ((models || { models: [] }).models)) {
      const o = document.createElement('option');
      o.value = m.key; o.textContent = m.label;
      sel.appendChild(o);
    }
    sel.value = 'persephone';
  }

  function visPrompt(userPrompt) {
    // Step 1-2 in one shot: the uncensored LLM (server-side enhance leg) rewrites
    // the request into a visual prompt. We read it back from the finished job's prompt.
    return null;
  }

  function askJudge(imageB64, originalPrompt, passNo) {
    // Two vlm_checks: (a) the fix text, (b) a region bbox as JSON inside the fix field.
    return jsonFetch('/api/v1/validate', {
      method: 'POST',
      headers: POSTJSON,
      body: JSON.stringify({
        image: imageB64,
        checks: [
          { op: 'vlm_check', arg: 'Does this image match the request and look anatomically/visually consistent (no blur, overlaps, fused limbs, wrong proportions, impossible geometry)? Answer pass only if fully consistent. In the fix field put a one-sentence correction prompt for the worst defect.' },
          { op: 'vlm_check', arg: 'Ignore correctness. Locate the single worst artifact region. In the fix field put ONLY a JSON object {"bbox":[x,y,w,h]} with normalized 0..1 floats of that region. Nothing else.' },
        ],
      }),
    });
  }

  function parseBBox(checks) {
    const c = checks && checks[1];
    const t = (c && (c.fix || c.reason) || '');
    const m = t.match(/\{[^}]*\}/);
    if (!m) return null;
    try {
      const j = JSON.parse(m[0]);
      const b = j.bbox;
      if (!Array.isArray(b) || b.length !== 4) return null;
      let [x, y, w, h] = b.map(Number);
      x = Math.min(0.95, Math.max(0, x)); y = Math.min(0.95, Math.max(0, y));
      w = Math.min(1 - x, Math.max(0.02, w)); h = Math.min(1 - y, Math.max(0.02, h));
      return [x, y, w, h];
    } catch (_) { return null; }
  }

  function maskB64FromBBox(bbox, w, h) {
    // white rect on black, full-res PNG via canvas
    const cv = document.createElement('canvas');
    cv.width = w; cv.height = h;
    const ctx = cv.getContext('2d');
    ctx.fillStyle = '#000';
    ctx.fillRect(0, 0, w, h);
    ctx.fillStyle = '#fff';
    const [x, y, bw, bh] = bbox;
    // a little bleed around the bbox so edges merge
    const pad = 0.03;
    ctx.fillRect(
      Math.max(0, (x - pad) * w), Math.max(0, (y - pad) * h),
      Math.min(w, (bw + 2 * pad) * w), Math.min(h, (bh + 2 * pad) * h));
    return cv.toDataURL('image/png').split(',')[1];
  }

  async function fetchB64(jobId) {
    const r = await fetch(imgUrl(jobId));
    const b = await r.blob();
    return blobToBase64(b);
  }

  async function renderJob(body) {
    let res = null;
    for (let k = 0; k < 12 && !res; k++) {
      try {
        res = await jsonFetch('/api/jobs', { method: 'POST', headers: POSTJSON, body: JSON.stringify(body) });
      } catch (e) {
        if (/429/.test(e.message) && k < 11) {
          const cd = 40 + (k * 15);
          appendLog(new Date(), 'info', `429 — cooling ${cd}s before retry`);
          await new Promise(r => setTimeout(r, cd * 1000));
          continue;
        }
        throw e;
      }
    }
    const jid = res.job_id;
    appendLog(new Date(), 'queued', 'refine pass job ' + jid);
    // wait for terminal via SSE-lite polling (phases are short)
    for (let i = 0; i < 240; i++) {
      await new Promise(r => setTimeout(r, 5000));
      const d = await jsonFetch('/api/jobs/' + jid);
      if (['done', 'failed', 'cancelled'].includes(d.status)) return d;
    }
    return { status: 'timeout', id: jid };
  }

  async function refineLoop(firstJobId, body, opts) {
    let cur = firstJobId;
    let basePrompt = body.fields ? (body.fields.description || '') : (body.prompt || '');
    for (let pass = 1; pass <= opts.maxPasses; pass++) {
      const j0 = await jsonFetch('/api/jobs/' + cur);
      const b64 = await fetchB64(cur);
      const r = await askJudge(b64, basePrompt, pass);
      const verdict = r.verdict;
      const failChecks = (r.checks || []).filter(c => c.status === 'fail');
      const fixText = failChecks.length ? (failChecks[0].fix || failChecks[0].suggestion || failChecks[0].reason || '') : '';
      appendLog(new Date(), verdict === 'failed' ? 'failed' : 'info',
        `refine pass ${pass}: verdict=${verdict} fix="${String(fixText).slice(0, 90)}"`);
      if (verdict !== 'failed') {
        setStatus('Refine: consistent after ' + (pass - 1) + ' correction pass(es).', 'ok');
        lastImageId = cur;
        const im = document.getElementById('image');
        im.src = imgUrl(cur); im.hidden = false;
        document.getElementById('empty-state').hidden = true;
        break;
      }
      const bbox = parseBBox(r.checks);
      const segmentOn = document.getElementById('refine-segment') && document.getElementById('refine-segment').checked;
      if (segmentOn) {
        const cut = await rembgCutout(b64);
        if (cut && cut.set_id) appendLog(new Date(), 'info', 'rembg cutout set ' + cut.set_id + ' (figure masked for pass ' + pass + ')');
      }
      const w = 512, h = 768;
      const passBody = {
        mode: bbox ? 'inpaint' : 'edit',
        prompt: 'fix: ' + fixText,
        size: body.size || '512x768',
        image: b64,
        model: opts.refineModel || body.model || 'persephone',
        loras: body.loras || [],
        enhance: false,
        seed: body.seed,
      };
      if (bbox) passBody.mask = maskB64FromBBox(bbox, w, h);
      if (bbox) passBody.strength = 0.85;
      const d = await renderJob(passBody);
      if (d.status !== 'done') {
        showStatus('Refine pass ' + pass + ' failed: ' + humanizeError(new Error(d.error || d.status)), 'err', d.error);
        break;
      }
      cur = d.id;
      appendLog(new Date(), 'ok', 'refine pass ' + pass + ' produced ' + cur);
      lastImageId = cur;
      const im = document.getElementById('image');
      im.src = imgUrl(cur); im.hidden = false;
      document.getElementById('empty-state').hidden = true;
      showProgress(false);
      refreshLibrarySoon();
    }
    setStatus('Refine loop finished.', 'ok');
    refining = false;
    document.getElementById('generate').disabled = false;
  }

  window.__startRefine = async function () {
    const on = document.getElementById('refine-on');
    if (!on || !on.checked) return false;
    if (refining) return true;
    refining = true;
    const max = parseInt(document.getElementById('refine-max').value, 10) || 3;
    const refineModel = (document.getElementById('refine-model') || {}).value || 'persephone';
    const body = collectGenerateBody();
    if (!body) { refining = false; return true; }
    body.enhance = true; // steps 1-2: uncensored LLM turns the request into the visual prompt
    let first;
    try {
      first = await renderJob(body);
    } catch (e) {
      showStatus(humanizeError(e), 'err', e.message);
      refining = false;
      document.getElementById('generate').disabled = false;
      return true;
    }
    if (first.status !== 'done') {
      showStatus('Initial render failed: ' + humanizeError(new Error(first.error || first.status)), 'err', first.error);
      refining = false;
      document.getElementById('generate').disabled = false;
      return true;
    }
    setStatus('Rendered ' + first.id + ' — starting refine passes…', 'pending');
    await refineLoop(first.id, body, { maxPasses: max, refineModel });
    return true;
  };

  document.addEventListener('DOMContentLoaded', () => {
    setTimeout(buildPanel, 400);
    const orig = document.getElementById('generate').onclick;
    document.getElementById('generate').onclick = async () => {
      if (await window.__startRefine()) return; // refine loop took over
      if (orig) orig();
    };
  });
  if (document.readyState !== 'loading') {
    buildPanel();
    const gen = document.getElementById('generate');
    if (gen && !gen.dataset.refineHooked) {
      gen.dataset.refineHooked = '1';
      const orig = gen.onclick;
      gen.onclick = async () => {
        if (await window.__startRefine()) return;
        if (orig) orig();
      };
    }
  }
})();
