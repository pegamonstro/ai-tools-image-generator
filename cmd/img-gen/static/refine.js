'use strict';

/* Refine loop — multi-pass generate→critique→localized-inpaint→re-critique.
   Uses only existing endpoints: /api/v1/jobs (generate/edit/inpaint), /api/v1/validate. */

(function () {
  let refining = false;

  function buildPanel() {
    const host = document.createElement('details');
    host.className = 'csec details';
    host.dataset.adv = '1';
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
    // ONE concise combined check: concise wording keeps the critic in the
    // parseable single-object reply shape (long compound criteria drift into
    // array replies the parser can't read), and the server salvages corrupted
    // replies + propagates a dedicated bbox field into the fix text.
    return jsonFetch('/api/v1/validate', {
      method: 'POST',
      headers: POSTJSON,
      body: JSON.stringify({
        image: imageB64,
        checks: [
          { op: 'vlm_check', arg: 'Are the figures anatomically and visually consistent (no blur, no overlaps, no fused or extra limbs, natural proportions)? Answer pass if fully consistent. In the fix field put a one-sentence correction and the region {"bbox":[x,y,w,h]}.' },
        ],
      }),
    });
  }

  function parseBBox(checks) {
    const c = checks && (checks[1] || checks[0]); // combined single-check carries the bbox in fix
    const t = (c && (c.fix || c.reason) || '');
    const m = t.match(/\{[^}]*\}/);
    if (!m) return null;
    try {
      let j;
      try { j = JSON.parse(m[0]); } catch (_) {
        j = JSON.parse(m[0].replace(/'/g, '"')); // critic may emit single-quoted dicts
      }
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

  async function rembgCutout(b64) {
    // Segment-then-retouch: the cutout's alpha mask narrows the inpaint region
    // from "whole bbox" to "the actual figure inside the bbox".
    const r = await jsonFetch('/api/v1/segment', {
      method: 'POST', headers: POSTJSON, body: JSON.stringify({ image: b64 }),
    });
    return (r.image || null);
  }

  function maskFromCutout(cutB64, bbox, w, h) {
    // White where the cutout is opaque (the subject); intersected with bbox when given.
    return new Promise((resolve) => {
      const im = new Image();
      im.onload = () => {
        const s = document.createElement('canvas');
        s.width = w; s.height = h;
        const sc = s.getContext('2d');
        sc.drawImage(im, 0, 0, w, h);
        sc.globalCompositeOperation = 'source-in';
        sc.fillStyle = '#fff';
        sc.fillRect(0, 0, w, h);
        const out = document.createElement('canvas');
        out.width = w; out.height = h;
        const oc = out.getContext('2d');
        oc.fillStyle = '#fff';
        if (bbox) { const [x, y, bw, bh] = bbox; oc.fillRect(x * w, y * h, bw * w, bh * h); }
        else oc.fillRect(0, 0, w, h);
        oc.globalCompositeOperation = 'source-in';
        oc.drawImage(s, 0, 0, w, h);
        resolve(out.toDataURL('image/png').split(',')[1]);
      };
      im.onerror = () => resolve(null);
      im.src = 'data:image/png;base64,' + cutB64;
    });
  }

  async function fetchB64(jobId) {
    const r = await fetch(imgUrl(jobId));
    const b = await r.blob();
    return blobToBase64(b);
  }

  async function renderJob(body, onJob) {
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
    if (onJob) onJob(jid);
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
    const silent = !!(opts && opts.silent);
    let cur = firstJobId;
    let finalId = firstJobId;
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
        if (!silent) setStatus('Refine: consistent after ' + (pass - 1) + ' correction pass(es).', 'ok');
        finalId = cur;
        if (!silent) {
          lastImageId = cur;
          const im = document.getElementById('image');
          im.src = imgUrl(cur); im.hidden = false;
          document.getElementById('empty-state').hidden = true;
        }
        break;
      }
      const bbox = parseBBox(r.checks);
      const segmentOn = opts.segment != null ? opts.segment
        : !!(document.getElementById('refine-segment') && document.getElementById('refine-segment').checked);
      const w = 512, h = 768;
      let passMask = maskB64FromBBox(bbox, w, h); // bbox rect, or null when no bbox
      if (segmentOn) {
        try {
          const cut = await rembgCutout(b64);
          if (cut) {
            const narrow = await maskFromCutout(cut, bbox, w, h);
            if (narrow) {
              passMask = narrow; // subject alpha instead of the whole bbox
              appendLog(new Date(), 'info', 'rembg cutout masks pass ' + pass + ' (segment-then-retouch)');
            }
          }
        } catch (e) {
          appendLog(new Date(), 'info', 'segment step skipped: ' + (e.message || e));
        }
      }
      const passBody = {
        mode: passMask ? 'inpaint' : 'edit',
        prompt: 'fix: ' + fixText,
        size: body.size || '512x768',
        image: b64,
        model: opts.refineModel || body.model || 'persephone',
        loras: body.loras || [],
        enhance: false,
        seed: body.seed,
      };
      if (passMask) {
        passBody.mask = passMask;
        passBody.strength = 0.85;
      }
      const d = await renderJob(passBody);
      if (d.status !== 'done') {
        showStatus('Refine pass ' + pass + ' failed: ' + humanizeError(new Error(d.error || d.status)), 'err', d.error);
        break;
      }
      cur = d.id;
      finalId = cur;
      appendLog(new Date(), 'ok', 'refine pass ' + pass + ' produced ' + cur);
      if (!silent) {
        lastImageId = cur;
        const im = document.getElementById('image');
        im.src = imgUrl(cur); im.hidden = false;
        document.getElementById('empty-state').hidden = true;
      }
      showProgress(false);
      refreshLibrarySoon();
    }
    if (!silent) setStatus('Refine loop finished.', 'ok');
    refining = false;
    document.getElementById('generate').disabled = false;
    return { finalId };
  }

  /* One-button workflows (Quick) drive the SAME pipeline through this API:
     generate -> judge -> segment-then-retouch inpaint -> re-judge. */
  window.RefineAPI = {
    renderJob,
    refineLoop,
    askJudge,
    parseBBox,
    maskB64FromBBox,
    maskFromCutout,
    fetchB64,
    rembgCutout,
  };

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
