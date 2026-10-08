# NSFW Explicit — Phases 1–3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to
> implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
> Choose inline sequential execution: every task touches the sidecar host over SSH
> and the single-slot GPU queue — parallel workers would fight over both.

**Goal:** Raise explicit, erotic, pornographic output fidelity in img-gen across the
lattice-sidecar stack: LoRA stack (Phase 1), an alternate explicit base model (Phase 2),
and a second SDXL-class image sidecar (Phase 3).

**Architecture:** Phase 1 is catalog-only (files on the sidecar host + `models.json`/
`presets.json` entries). Phase 2 reruns the proven single-file→diffusers conversion
recipe already on the sidecar host, then adds a second `models[]` entry. Phase 3 adds
a new SDXL sidecar speaking the same HTTP contract as the mflux sidecar, plus a
small `sidecar` engine field threaded from catalog → storage → queue → lattice client.

**Tech Stack:** Go stdlib (img-gen), Python stdlib http server (sidecars), mflux (MLX /
FLUX.1), stable-diffusion.cpp w/ Metal (SDXL engine), diffusers (conversion), HF + Civitai +
mirror downloads, launchd agents on the sidecar host.

**Spec:** `docs/superpowers/specs/2026-10-08-nsfw-explicit-models-loras-scoping.md`

## Global Constraints

- **This repo and LLM-router are scrubbed:** no real usernames, hostnames, IPs, or
  tokens in ANY tracked file. Remote commands address the sidecar host via
  `$M6_SSH` (real value lives in memory `m6-gateway-deploy.md`, set once per shell)
  and use `~`-relative paths so usernames never appear. Gitignored local catalogs
  (`models.json`, `presets.json`) already carry real paths by design.
- Real env values (`IMAGE_URL`, `M6_SSH`, tokens) are passed on command lines only.
- Commits end with `Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>`.
- img-gen build+restart: `go build -o ./bin/img-gen ./cmd/img-gen && launchctl
  kickstart -k gui/501/com.img-gen`; catalog-only changes need restart only.
- launchd agents on the sidecar host restart via `bootout` + `bootstrap`
  (cdhash/LWCR gotcha — `kickstart -k` fails for changed binaries).
- mflux LoRA format gate: keep safetensors whose keys are `transformer.*` /
  diffusers-style; reject `lora_unet_*` (XLabs) and PEFT-DoRA layouts.
- **Card-vetting rule:** never install a model/LoRA whose model card or example
  prompts sexualize minors. `Flux-Uncensored-V2` is disqualified on those grounds.
- Civitai anonymous downloads are refused; use mirrors first, token only if the
  user supplies it on the command line mid-run. Blocked picks are reported, not
  silently skipped.
- FLUX.1-dev-derived checkpoints/LoRAs are non-commercial (fine for this
  self-hosted tool; note in docs).
- New SDXL sidecar port: `:8902` (8899/8900/8901 taken).
- Do not push branches without an explicit user request.

---

### Task 1: Download + format-verify the HF uncensored booster LoRA

**Files:** none in-repo (remote sidecar host + local gitignored catalog in Task 4).

- [ ] Resolve the booster's asset URL:
      `curl -s "https://huggingface.co/api/models/enhanceaiteam/Flux-uncensored"`
      → pick the main `*.safetensors` (record its size from the API JSON).
      If the card needs re-check: fetch its README and confirm clean (no V2-style
      content); if v1's card is also compromised, fall back to
      `kenerateai/Flux-uncensored` (`lora.safetensors`, diffusers) and vet likewise.
- [ ] Download on the sidecar host:
      `ssh $M6 'curl -L -o ~/mflux-models/loras/nsfw-booster.safetensors "https://huggingface.co/<org>/<repo>/resolve/main/<file>"'`
- [ ] Format-verify (venv python from proven path):
      ```bash
      ssh $M6 '~/flux-train-venv/bin/python - <<PY
      from safetensors import safe_open
      with safe_open("/Users/igor/mflux-models/loras/nsfw-booster.safetensors", framework="numpy") as f:
          ks = list(f.keys())
      import collections
      heads = collections.Counter(k.split(".")[0] for k in ks)
      print(heads, len(ks))
      PY'
      ```
      Accept when heads are transformer-style blocks; reject on `lora_unet_*`
      (XLabs) or DoRA (`lora_A`/`lora_B`-PEFT + magnitude) signatures.
- [ ] `scp` the verified file to the sidecar host's loras dir if downloaded
      locally instead; record final size via `ssh $M6 'ls -la ~/mflux-models/loras/'`.

### Task 2: Composition + anatomy picks — mirror/attempt downloads

- [ ] For each Civitai pick from the spec §C/§D — Composition [Multiple People
      (civitai 540150, trigger `multipeoplelora`), Dynamic Poses (civitai 332248)];
      Anatomy [SCG-Anatomy-Flux1.d (civitai 640156), FLUX Photo nuds DEV (civitai
      675247)] — try in order:
      1. CivArchive mirror page: `curl -s https://civarchive.com/models/<id>` →
         extract a direct file link if present → download + format-verify as Task 1.
      2. Civitai direct: `curl -s "https://civitai.com/api/download/models/<ver>"`
         → expect the documented `Unauthorized` refusal (record it, not fatal).
- [ ] If Civitai paths are all blocked, run the HF fallback search:
      `curl -s "https://huggingface.co/api/models?search=flux+nsfw+pose+lora&filter=text-to-image&limit=40"`
      and vet top hits (diffusers/BFL format at least; clean card required) for one
      composition pick; download + verify the winner as Task 1.
- [ ] Produce a ledger line per pick: **installed-as / blocked-needs-token**.

### Task 3: Catalog entries for the new LoRAs

**Files (gitignored, local-only):** `models.json` (loras[])

- [ ] Add one entry per installed file, e.g.:
      ```json
      { "key": "nsfw-booster", "label": "NSFW booster (uncensored v1)",
        "value": "<mflux-models>/loras/nsfw-booster.safetensors" },
      { "key": "dyn-poses", "label": "Dynamic Poses",
        "value": "<mflux-models>/loras/<file>.safetensors" }
      ```
      (keys mirror each verified Task 1/2 file; values are sidecar-host local paths
      using the same convention as existing entries).
- [ ] Restart img-gen: `launchctl kickstart -k gui/501/com.img-gen`.
- [ ] Verify: `curl -s localhost:8099/api/models` lists the new keys in `loras[]`.

### Task 4: Live stacked two-person test + explicit preset

- [ ] Stacked generation (booster + composition at spec §D weights), sync API:
      ```bash
      curl -s -X POST localhost:8099/api/v1/generate \
        -H 'Content-Type: application/json' \
        -d '{"model":"persephone","size":"1024x576",
             "prompt":"explicit two-adult scene per catalog field prompts",
             "loras":[{"name":"nsfw-booster","scale":0.8}],
             "wait_seconds":600}'
      ```
      Confirm `done` + non-trivial image bytes.
- [ ] Scale sweep: 2–3 further gens varying booster scale (0.6/0.9) and adding
      composition LoRA; keep the strongest two-person composition.
- [ ] Add the winning combo to `presets.json` (local, gitignored):
      ```json
      { "key": "act", "label": "Explicit act (stacked)",
        "trigger": "",
        "loras": [ { "name": "<winner-booster-value>", "scale": <s1> },
                   { "name": "<winner-composition-value>", "scale": <s2> } ],
        "negative_prompt": "blurry, deformed hands, extra fingers, low quality, watermark",
        "steps": 24, "guidance": 3.5 }
      ```
- [ ] Restart, verify `GET /api/presets` lists `act`;
      commit example-file updates: `models.json.example` + `presets.json.example`
      gain a same-shape example using public HF/Civitai ids only (values never real
      hosts/paths). Commit img-gen docs.

### Task 5: Phase 2 checkpoint acquisition (Fluxed Up FP8)

- [ ] Disk check: `ssh $M6 'df -h / | tail -1'` (need ≥30 Gi; recon showed 165 Gi).
- [ ] Sources, in order (per spec §A): CivArchive mirror of civitai 847101
      (Fluxed Up) FP8/GGUF variant; CivArchive of 2119037 (NSFW UNLOCKED V2.0 FP8);
      HF mirror search: `curl -s "https://huggingface.co/api/models?search=flux+up+nsfw&limit=40"`
      / `...search=flux+nsfw+unlocked...`. Download the winner (~11–15 GB) to
      `~/fluxedup-download/` on the sidecar host. Record source URL + sha/size.
- [ ] If every mirror is dead: record blocked, defer Phase 2, proceed to Phase 3.

### Task 6: Convert + assemble + bake (rerun the proven recipe)

**Files:** remote sidecar host only (reuses `~/persephone-download/convert.py`,
`~/flux-dev-encoders/`, `~/persephone-venv/`).

- [ ] Parameterize the recipe for the new checkpoint:
      ```bash
      ssh $M6 'mkdir -p ~/fluxedup-download && cp ~/persephone-download/convert.py ~/fluxedup-download/ && sed -i -e "s|^SRC = .*|SRC = \"/Users/igor/fluxedup-download/<downloaded>.safetensors\",|" 2>/dev/null true'
      ```
      (concrete sed verified against convert.py's actual SRC/OUT lines at run
      time; OUT_DIR becomes `~/mflux-models/fluxedup/transformer`).
- [ ] Convert: `ssh $M6 'cd ~/fluxedup-download && ~/persephone-venv/bin/python convert.py > convert.log 2>&1'`.
      Expect fp16 ~12 GB streamed write; log ends `DONE …`.
- [ ] Assemble the model dir:
      ```bash
      ssh $M6 'cd ~/mflux-models && mkdir -p fluxedup && for d in text_encoder text_encoder_2 tokenizer tokenizer_2 vae; do cp -R ~/flux-dev-encoders/$d fluxedup/; done && mv /tmp/<transformer-out> fluxedup/transformer'
      ```
      Final layout must equal `persephone/`: transformer, vae, encoders, tokenizers.
- [ ] Bake 4-bit: derive the exact historical command first
      (`ssh $M6 'grep -a -m5 "bake" ~/.zsh_history; head -5 ~/mflux-models/persephone-bake.log'`),
      else use the current mflux CLI: check `mflux-generate --help | grep -iE "save|bake|quantize"`,
      then bake `fluxedup-4bit` from the assembled dir; smoke-log to
      `~/mflux-models/fluxedup-bake.log`. Expect ~8 GB dir.
- [ ] Smoke-generate directly against the sidecar with a model override:
      `curl -s -X POST http://<IMAGE_URL>/generate -d '{"prompt":"test portrait","width":512,"height":512,"model":"<MFLUX-MODELS>/fluxedup-4bit"}'`
      (sidecar accepts per-request `model` override — already shipped). PNG returns.

### Task 7: Register the alternate base + compat test + end-to-end

- [ ] LoRA-compat check on the new base: one generate (via img-gen API,
      `model":"fluxedup"` value) with `loras:[{"name":"illustration","scale":0.8}]`
      → done + sane image; note any degenerate combos.
- [ ] Local `models.json` `models[]` entry:
      ```json
      { "key": "fluxedup", "label": "Fluxed Up (NSFW, alt base)",
        "value": "<mflux-models>/fluxedup-4bit" }
      ```
      Restart img-gen; verify `/api/models` lists it.
- [ ] End-to-end via `/api/v1/generate` with the new model; verify done + PNG.
- [ ] Commit spec docs note (Phase 2 outcome) to img-gen repo; leave LLM-router
      untouched in this phase.

### Task 8: SDXL engine build on the sidecar host (stable-diffusion.cpp)

**Files:** none in-repo (sidecar host).

- [ ] Clone + build with Metal + server:
      ```bash
      ssh $M6 'git clone --depth 1 https://github.com/leejet/stable-diffusion.cpp ~/sd.cpp && cd ~/sd.cpp && cmake -B build -DSD_METAL=ON -DSD_BUILD_SERVER=ON -DCMAKE_BUILD_TYPE=Release && cmake --build build -j'
      ```
      (exact target/flag names verified against that README at run time; if the
      CMake flags differ — e.g. `GGML_METAL` — use the README's Metal build.)
- [ ] Smoke: `./build/bin/sd --help` (or `sd_server` variant) runs; then one CLI
      txt2img with a small SDXL checkpoint (Task 9's first download) at 512×512,
      20 steps → PNG written.

### Task 9: SDXL-class model downloads

- [ ] Grab, vet, and verify (each loaded once via the sd CLI from Task 8):
      1. **SDXL base** — ungated single-file mirror
         (`curl -s "https://huggingface.co/api/models?search=sd_xl_base_1.0.safetensors"`);
         stabilityai's own entry requires license acceptance → prefer mirror repo.
      2. **Pony Diffusion V6 XL** — Civitai 257749 is gated; search HF mirrors
         (`https://huggingface.co/api/models?search=pony-diffusion-v6-xl`), vet for
         unmodified weights, download single-file safetensors.
      3. **Illustrious-XL** — search HF (`...search=illustrious-xl...`), same vet.
- [ ] Store under `~/mflux-models/sdxl/` (keep separate from FLUX models); record
      each file's size + which CLI smoke passed. Gated-only files → report
      blocked-needs-token, continue with whatever installed.

### Task 10: sd-sidecar.py — new Python sidecar (LLM-router repo)

**Files (LLM-router / `feat/esrgan-sidecar` branch):**
- Create: `deploy/sd-sidecar.py`
- Create: `deploy/com.lattice.sdxl.plist.in` (template: `__REPO_ROOT__`-style
  placeholders per the existing plist templates)
- Modify or mirror the existing install script that provisions plists
  (name discovered at task time from `ls deploy/`).

**Interfaces:**
- Consumes: sd.cpp binary path (env `SD_BIN`), ckpts dir (env `SD_CKPT_DIR`),
  outputs dir (env `SD_RESULTS_DIR`, default `~/sd-outputs`), port `8902`.
- Produces: same HTTP contract as `deploy/mflux-sidecar.py` (read that file for
  exact shapes): `POST /generate` (body keys per mflux sidecar: `prompt`,
  `width`, `height`, `steps`, `seed`, `guidance`, `negative_prompt`, `model`,
  `loras:[{name,scale}]`) → `{gen_id}`; `GET /progress?<gen_id>`; `GET
  /result/<id>` (404 when unknown); `POST /cancel`; `GET /status`; `GET /health`.

- [ ] Port the mflux-sidecar runtime skeleton: uuid4 gen-ids, Popen child mflux→sd
      binary, single-slot serialization, per-request body override of `model`,
      stdout progress parsing ("step n/N"), cancel via SIGTERM, write-then-respond
      persistence to `$SD_RESULTS_DIR/<gen-id>.png` (TTL sweep + newest-300 keep,
      `/result` ungated with the 32-hex uuid guard, identical to mflux sidecar),
      `/health`. Diff points vs mflux sidecar:
      - sd CLI invocation: `sd -M i -m <ckpt> -W <w> -H <h> -p <prompt> -n <neg>
        -s <steps> -S <seed> -c <cfg> [-l <lora:scale> ...]` — flag names verified
        against `sd --help` (Task 8) before shipping.
      - LoRA support: pass-through when the CLI supports it; if not, return 400
        `{"error":"loras not supported by this engine"}` (img-gen will surface it).
      - Output: sd writes PNG itself → `--output`/rename to the results dir, then
        respond (write-then-respond invariant preserved).
- [ ] Syntactic check: `python3 -m py_compile deploy/sd-sidecar.py`.
- [ ] Commit in LLM-router repo (placeholders only, no real IPs).

### Task 11: img-gen engine routing (TDD — the only sizable Go change)

**Files:**
- Modify: `internal/storage` (ModelSpec gains `Sidecar string json:"sidecar,omitempty"`)
- Modify: `internal/models` (catalog entries parse `sidecar` field)
- Modify: `internal/queue` (submit-time engine/mode gate; engine persisted with job)
- Modify: `internal/lattice/lattice.go` (Client gains `SDXLImageURL`; routing by spec)
- Modify: `cmd/img-gen/main.go` (`SDXL_URL` env → client)
- Test: existing `_test.go` files per package (patterns already in place)

- [ ] Write failing tests first (mirroring existing package test style):
      1. catalog: an entry with `"sidecar":"sdxl"` round-trips into `ModelSpec.Sidecar`.
      2. queue: submit with an sdxl-sidecar model and mode `inpaint` → validation
         error `mode inpaint is not supported on engine sdxl` (allowed: generate,
         edit); mode gate absent for default engine (all modes).
      3. lattice: router function maps (mode, spec.Sidecar) → URL: sdxl+generate →
         SDXLImageURL; sdxl+edit → SDXLImageURL `/img2img`; default keeps the
         existing per-mode behavior untouched.
      4. storage: Job JSON round-trip preserves `sidecar` (restart-recovery reads
         it at boot attach).
- [ ] Run tests → fail; implement minimal code → pass.
- [ ] Engine validation constant lives beside the mode validation.
- [ ] Recovery: nothing new needed if `ModelSpec.Sidecar` rides the persisted Job —
      verify the boot attach path resolves the sidecar via the job's stored spec.
- [ ] Build + full test suite: `go build ./... && go test ./...`; commit img-gen.

### Task 12: Deploy the SDXL sidecar + wire img-gen

- [ ] Provision plist + bootstrap on the sidecar host (port 8902, agent label
      `com.lattice.sdxl`, same runbook style as the mflux sidecar agent — copy its
      plist shape); `bootout`+`bootstrap`, verify:
      `curl -s localhost:8902/health` and one direct `/generate` smoke.
- [ ] Reinstall img-gen carrying the new env on the command line:
      `IMAGE_URL=... LATTICE_FRONTEND_URL=... UPSCALE_URL=... SDXL_URL=http://<M6>:8902 ./deploy/install-macos.sh`
      (values from memory files, never from tracked files). Restart; startup log
      shows SDXL_URL line.
- [ ] Local catalogs: `models[]` entries (SDXL/Pony/Illustrious) each with
      `"sidecar":"sdxl"`; restart; verify `/api/models` lists them.
- [ ] Unsupported-mode envelope check: submit `inpaint` with an sdxl model →
      400 envelope `validation_error` with the mode message.
- [ ] Upstream-mode spot checks: generate with pony via `/api/v1/generate`
      (wait_seconds 600) → done PNG; upscale that PNG via mode `upscale`
      (engine-agnostic ESRGAN) → done.

### Task 13: Docs, specs, memory

**Files:**
- Modify: `docs/superpowers/specs/2026-10-08-nsfw-explicit-models-loras-scoping.md`
  (mark phase outcomes + final source URLs, public only)
- Modify: `README.md` (SDXL_URL env + engine note; model/LoRA install convention)
- Modify: `models.json.example` (document `sidecar` field with a placeholder value
  like `"sdxl"`)
- Modify: `docs/superpowers/specs/2026-10-05-img-gen-agent-api-contract.md` (one
  line: engines gate modes — validation message content)
- Memory: update `img-gen-routing.md` (+ new phase-3 sidecar memory if needed)
  and `MEMORY.md` index.

- [ ] Repo scrub check: `grep -rn "192\\.168\\|100\\.[0-9]\\|archcore\\|igor" --include=*`
      over changed files → empty; then commit img-gen.
- [ ] LLM-router commits (sidecar + plist template) — same scrub check.
- [ ] Final report to user: what was installed / blocked-needs-token (Civitai
      picks), how presets/models expose it, verification evidence.

## Self-review notes (done at plan time)

- Spec coverage: Phase 1 → Tasks 1–4; Phase 2 → Tasks 5–7; Phase 3 → Tasks 8–12;
  scope/docs → Task 13. The spec's Phase-3 gate ("only revisit if FLUX outputs
  disappoint") is being overridden by the user's implement-all-3 instruction —
  recorded here deliberately.
- Type consistency: engine field is named `sidecar` end-to-end (catalog JSON key,
  `ModelSpec.Sidecar`, `SDXL_URL` env → `SDXLImageURL` client field).
- Known verification risks: sd.cpp CLI flags, Pony/Illustrious mirror availability,
  and Civitai gating — each has a documented fallback/verify step at the task level.