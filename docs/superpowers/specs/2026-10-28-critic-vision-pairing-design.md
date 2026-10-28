# Critic / vision-pairing design (Phase D)

Date: 2026-10-28
Status: approved design (brainstormed and section-approved with the user, 2026-10-28)
Predecessors: `2026-10-14-capability-roadmap-round2-design.md` (Phases A/B/C — shipped), `.claude/research/2026-10-28-enrichment-research.md` (round-3 research, untracked)
Supersession: this design **reconsiders round-2's parked non-goal** "VLM auto-captioning (new local-VLM dependency; deferred, reconsider later)". The enabler decision below changes the cost calculus: the VLM is already hosted and the only missing piece is a translation in the lattice gateway.

## Goal

Give img-gen the validation limb: every opted-in render is checked — by a vision-language judge, by an objective danbooru tagger, and by deterministic in-process metrics — against the request, with bounded self-correction, and every verdict persisted. The critic's purpose, as ruled in brainstorming: **confirm the result matches the request and is acceptable.** Nothing changes for callers that do not opt in.

## Rulings from brainstorming (do not relitigate in the plan phase)

1. **Enabler:** the judge rides the lattice. `lattice-gateway` grows OpenAI-style image content parts → Ollama-native `images` translation. Not an img-gen→ollama client (bypasses the lattice, duplicates a client), not a critic sidecar (moves logic out of testable Go).
2. **Judge model:** `CRITIC_MODEL` env, default `huihui_ai/qwen2.5-vl-abliterated:latest` (full tag — the control plane passes literal names through verbatim). Uncensored judge is a requirement, not a preference: a censored judge would flinch on the tool's core output class.
3. **v1 op set:** `vlm_check`, `tag_roundtrip`, `palette_conform`, `tile_conform` (the last ships now, dormant until the Phase E `tile` kind exists).
4. **Attach points:** standalone sync endpoint + generate jobs with auto-retry + asset-set children + GIF frame sources. All opt-in.
5. **Retry policy:** attempt 2 mutates the prompt **mechanically** (failed checks' suggestions + missing tags appended; seed unchanged), attempt 3 resamples the seed (`seed+i`); default budget 2 (range 0–3); best-of-N by score; `on_fail` = `deliver` (default) or `fail`.
6. **Deferred ops:** NudeNet/anatomy gate (drop-in op later; WD14 roundtrip covers adult-content acceptability in v1), reward-model battery, two-tier fast+heavy judges.

## Non-goals

- NudeNet/anatomy gate; reward scorers (ImageReward/imscore — PyTorch hosting); two-tier judge; gateway capability-announcement/registry schema change (ollama refuses non-vision models loudly; documented, not encoded).
- Web UI surface for validation (Phase G).
- Auto-captioning for LoRA training datasets (a different feature).
- Any behavior change when `validate` is absent — byte-identical defaults, like every prior phase.
- LoRA-dataset captioning coupling, cross-model judging, batch judging outside the job lifecycle.

## Component map

- **`lattice-gateway` (LLM-router repo, cross-repo deliverable):** content-part translation on `/v1/chat/completions` + coarse image-payload token estimation. Its own tests; its own deploy.
- **`internal/critic` (new img-gen package):** op registry, checklist compiler, verdict aggregation. Pure dispatch and pure-image ops live here; remote ops call the clients.
- **`internal/lattice`:** `ChatVision` (content-part chat via the frontend) and `Tagger` client (`TAGGER_URL`).
- **Tagger sidecar (LLM-router repo `deploy/`):** WD14 onnxruntime, u2netp deployment pattern, port 8904.
- **`internal/queue`:** the `validating` stage, retry loop, attempt provenance.
- **API:** `POST /api/v1/validate` (sync) + the `validate` field on job / asset-spec / gif-source submits; `docs/openapi.yaml`; skills + README.
- **Docs:** third packaged skill `skills/img-gen-validation/SKILL.md`; per-asset skill additions.

## D1 — Gateway enabler (LLM-router)

**Translation.** On `POST /v1/chat/completions` (both streaming and not — translation happens once, on the message objects, before any send path), `ollamaMessages` additionally maps OpenAI-style content arrays to Ollama-native form: `content` becomes the concatenated text parts; each `{type:"image_url", image_url:{url:...}}` part's data URL (`data:image/<fmt>;base64,<b64>`) becomes an entry on the Ollama message's `images:[<b64>...]` array. Non-data URLs and non-array content are untouched; the client's original shape is never forwarded where ollama would choke. Tool-call argument rewriting (the existing normalization) composes unchanged.

**Token honesty.** `estimateTokens` counts base64 image payloads coarsely (today they contribute zero to `num_ctx` sizing, so a vision call can silently under-size context). Per-image estimate = **1000 + min(ceil(len(b64)/4), 2048)** — the payload term is CAPPED so a real PNG can never pin `num_ctx` at the maxContext ceiling (KV allocation is the memory-unsafe direction; D1 final-review ruling, recorded in the lattice SDD ledger), while the flat allowance errors upward for any sane image.

**Error path.** When the resolved model cannot take images, ollama's error surfaces through the existing `provider_error` propagation — no special-casing, no silent drop. When the gateway predates the change, the parts body passes through to ollama's native endpoint, ollama rejects it, and the gateway's `Execute` collapses upstream non-200s into `ollama returned status 400` written as **HTTP 500** — so img-gen's named-upgrade detection matches BOTH `500` with that body text and a direct `400` (the original 400-only detection never fires against the live pre-upgrade gateway; D1 final-review ruling).

**Out of scope in D1:** capability strings, registry schema changes, vision-aware slot sizing (the wired-budget resident gauge already tracks the judge — see Resources).

## The check library (`internal/critic`)

The op enum (submit-time `validation_error` on unknown op/arg; ops are an enum so manifests reproduce any checklist):

### `vlm_check{spec: string}` — the judge
- `spec` is a free-text binary criterion ("subject matches the request", "background is seamless wood texture", ...). A checklist may carry ≤ 4 VLM criteria per image; the compiler batches them into **one judge prompt per check call**: `vlm_check` ops are evaluated in a single `ChatVision` request per attempt (image + all criteria + strict JSON reply shape), never one call per criterion.
- Reply schema (strict): `[{"id": <criterion index>, "pass": <bool>, "reason": <≤1 sentence>, "fix": <≤1 sentence, optional>}]`. Malformed JSON → all criteria in the call become `undetermined` (with the raw head of the reply stored, truncated); never fail-open into pass.
- Judge model: `CRITIC_MODEL`. Timeout: `CRITIC_TIMEOUT_S` (default 120 s). Image capture: the attempt's PNG, ≤ 64 MB, single image per call.

### `tag_roundtrip{}` — the objective drift detector
- Image → tagger sidecar (`POST /tag`) → tags with scores. The prompt is tokenized to danbooru conventions (lowercase; comma-split; spaces→underscores for multi-word entries). A prompt token is **required** iff it exists in the tagger's vocabulary; a required token whose best score on the render is absent or < floor (default 0.35) is drift.
- Missing tags → feedback `"strengthen: <tag>"` (joined, comma-separated) and, on retry, appended to the mutated prompt.
- Threshold: tag score floor **0.35** (WD14 convention). Vocabulary: the sidecar serves its tag list once (`GET /meta`), img-gen caches it. Unset `TAGGER_URL` → `validation_error` naming the env at submit (honest, RIFE precedent).

### `palette_conform{}` — deterministic coherence (Go)
- Target resolution, in order: the set palette (asset inside a set with `palette_lock`) → the submit-time `validate.palette` array → else submit-time `validation_error` ("palette_conform requires a palette_lock set or an explicit palette") — honest gate, never a skipped check.
- With `pixel_fix` (render already quantized): every opaque pixel's color must ∈ the target palette — catches post-op drift (outline/resize) after quantization.
- Otherwise: ≥ 98% of opaque pixels within CIEDE2000 ΔE ≤ 8 of the target palette.
- Failure feedback: `"palette drift"`.

### `tile_conform{}` — the seam metric (Go)
- Wrap-edge error: compare each pixel against its wrapped counterpart across all four offsets; E = normalized wrap-edge error over the interior variation baseline. Pass iff E ≤ **1.3** (TexTile-lite, CVPR 2024 lineage — learned tileability scoring explicitly outperforms SSIM/LPIPS and vision-eyeball for this property).
- Valid on any square image; **dormant by design** until Phase E's `tile` kind adopts it as its gate. `arg` accepts an optional numeric override `{"op":"tile_conform","arg":{"max_ratio":1.2}}` (1.0..2.0).

**Threshold calibration is a ruling, never a test edit** — the Phase C DetectGridCell precedent: constants here are pinned by goldens; recalibration requires a recorded ruling with before/after smokes, and `internal/compose/testdata`-style fixtures for the deterministic ops.

**Aggregation.** Per check: `pass | fail | undetermined | error`. Only `fail` consumes retry budget and only `pass`/`fail` are verdict-relevant; `undetermined` (judge flinch / malformed) and `error` (tagger infra) are inert and recorded. A checklist is bounded: ≤ 8 check entries per job (submit-time `validation_error` above that).

## Clients (`internal/lattice`)

- **`ChatVision(criteria, images, model)`** — non-streaming `POST {LATTICE_FRONTEND_URL}/v1/chat/completions`, messages shaped as OpenAI content parts (`text` + `image_url` data URLs), per-call context deadline `CRITIC_TIMEOUT_S`, base URL and auth exactly like `Enhance`. Gateway-pre-image-era detection — HTTP 500 with `ollama returned status` in the body, or a direct 400 — → `validation_error`-style named failure ("lattice gateway needs the image-content upgrade; IMAGE_ROUTING=direct does not help — the judge is chat-side").
- **`Tagger.Tag(image)`** — `POST {TAGGER_URL}/tag`, `GET {TAGGER_URL}/health` at boot wiring (and `GET /meta` for the vocab, cached), 64 MB cap, the standard retryable trio (`sidecar_unavailable` / `busy` / `timeout` → a check carries `error` after a small bounded retry cap — never the render budget). `TAGGER_TIMEOUT_S` default 60 s.

## Tagger sidecar (LLM-router `deploy/`)

- **Tagger sidecar:** WD14 (`wd-v1-4-convnextv2-tagger-v2`, onnxruntime in-process like rembg 2.0.85; uv venv, weights (~0.5 GB) auto-download on first use — the `/meta` call at img-gen boot preloads them). LaunchAgent template `com.lattice.tagger.plist.in`, bind `0.0.0.0:8904` (tailnet reach), **unauthenticated** like every sibling image sidecar (esrgan/sdxl/rembg posture) — the earlier `token header X-Tagger-Token` line is superseded by that fleet-wide ruling (recorded in the lattice spec note).
- Contract: `GET /health {status, model, session_ready}`; `GET /meta {tags: [<vocabulary>, ...]}` (cached list); `POST /tag {"image": <b64 png>, "threshold"?: f} → {"tags": [{"name","score"}...], "seconds"}`. Single-flight lock + 64 MB body cap; errors as `{"error","error_type"}` with the sibling sidecars' taxonomy. Stdout/stderr logs under `~/Library/Logs/lattice/`.
- Deployment notes: same targeted-install path as rembg (it is not in `install-macos.sh`; that known gap is not fixed here). Smoke probe script stays a workspace artifact (rembg's `verify-segment.py` precedent — no test-probe convention in `deploy/`).

## API surface

### `POST /api/v1/validate` (sync — the `/api/v1/repair` precedent)

```jsonc
// request
{ "image": "<b64 png ≤1024²>",
  "checks": [ {"op":"vlm_check","arg":"subject matches the request"},
              {"op":"tag_roundtrip"}, {"op":"palette_conform"}, {"op":"tile_conform"} ],
  "palette"?: ["#rrggbb", ...] }   // explicit target for palette_conform when no set context
// response 200
{ "verdict": "pass | failed | indeterminate",  // sync path never retries — the verdict reflects checks only
  "checks": [ {"op": "...", "status": "pass|fail|undetermined|error",
               "reason": "...", "suggestion": "..."} ] }
// 400 validation_error (unknown op, >8 checks, undecodable image, palette_conform without target,
//     tag op with TAGGER_URL unset — naming the env, judge with pre-upgrade gateway — naming the upgrade)
```

Sync contract mirrors `/api/v1/repair`: bounded size, bounded time, no retry logic — the primitive agents loop over externally.

### The `validate` field (opt-in on submits)

```jsonc
"validate": {
  "checks": [ ...same op entries... ],
  "budget": 2,        // 0..3, default 2; 0 = evaluate-only, never retries
  "on_fail": "deliver", // "deliver" (default) | "fail"
  "palette"?: ["#rrggbb", ...] // explicit palette_conform target when no set context provides one
}
```

Attaches to: `/api/v1/generate` + `POST /api/v1/jobs` (generate-mode), every asset-spec in `/api/v1/asset-sets`, and gif-source specs in `/api/v1/gifs` (validates each resolved source frame pre-encoding). Absent → byte-identical existing behavior.

## Job lifecycle & retry

**Stage.** Opted-in jobs gain `validating` between `generating` and terminal:
`queued → (enhancing) → generating → validating → [generating → validating]×(budget) → done | failed`.
The stage runs on the worker slot (short against the render; a cold judge call costs the documented ~20–40 s), emits per-check SSE log events, and writes the summary into the job record:

```jsonc
"validate": { "verdict": "pass | best_of_n | failed | indeterminate", "attempts": 2, "checks": [...] },
"attempts": [ {"image": "<img id>", "prompt": "<the exact prompt rendered>", "seed": <n>,
               "checks": [{"op","status","reason","suggestion"}], "chosen": true} ]
```

`storage.Job` grows `validate` + `attempts` (additive; existing history readers ignore unknown fields). `ImagePath` points at the chosen attempt — History semantics unchanged; the non-chosen attempt images are ordinary persisted images (the dual-render-siblings pattern), listed in `attempts[]` so any future UI/agent can show them.

**Verdict taxonomy (precise).**
- `pass` — every verdict-relevant check on the delivered attempt passed (whether or not retries happened).
- `best_of_n` — delivered attempt still carries ≥1 `fail` after the chain resolved; delivered explicitly marked.
- `failed` — terminal failure under `on_fail: "fail"` (the full check list is in the error envelope).
- `indeterminate` — zero verdict-relevant checks (all `undetermined`/`error`); delivered, honestly not-a-verdict.

**Retry mechanics.**

1. **Attempt 1** — the render as requested; one judge call + remote/deterministic ops.
2. **Attempt 2** (budget ≥ 1 and ≥1 `fail`): re-render with a **mechanically mutated prompt** — failed `vlm_check` `fix` texts + missing tags joined and appended with `", "` (deduped, bounded), trigger words preserved, **seed unchanged** (pinned-seed scaffold, the npc mechanic). No LLM rewrite — the mutation is deterministic, auditable text.
3. **Attempt N (N ≥ 3)** (budget ≥ N−1 and attempt N−1 still failing): mutated prompt, **seed = original + (N−2)** — the first retry is the composition-fix leg, every further retry is a seed resample of the same mutated prompt.
4. **Best-of-N:** score = pass ratio over `pass|fail` checks (deterministic tiebreak: attempt index). Chosen attempt delivered as primary; every attempt recorded in the manifest/tEXt (`critic_attempt` = n, `critic_verdict`, `critic_<op>` keys — reasons truncated like `renderParamsKV`).
5. Under `on_fail: "deliver"` a fully failed chain still delivers the best attempt, marked — never a silent drop.

**Mutation provenance.** Each attempt's exact prompt is recorded (job record + manifest). A later reproduction is `submit_job` with the recorded values, same as the asset-set manifest contract.

**Compose children.** Each child job owns its own retry chain; the parent compose logic is unchanged (children terminal when their chain resolves). Set-level `palette_lock` remap runs **after** attempts settle, so coherence survives retries. Per-asset check failures follow the existing partial-success semantics (the set completes if any asset did; per-asset errors recorded). Kit children retry within their class derivation (mutated class prompt, class offset seed preserved; resample leg increments from it).

**GIF frames.** Each resolved source frame validated before encoding; a frame that exhausts budget is still assembled (marked in `manifest.json`) under `"deliver"`, or fails the GIF with the offending frame named under `"fail"` — the composite overhang-honesty rule.

## Requeue / restart recovery

- Killed **mid-`validating`**: checks are stateless and idempotent — on restore the stage re-evaluates from the persisted attempt images (RIFE-contract style; no sidecar state).
- Killed **mid-render (attempt N)**: the existing GenID attach flow applies unchanged — attempt 1's image is already persisted locally before attempt 2 starts a new sidecar run; `captureGenID` carries the *current* attempt's gen id, and per-attempt persistence in history makes the chain crash-safe.
- Non-terminal jobs with no surviving attempt renders fail honestly (existing interrupted-message pattern), with the resubmit hint.

## Resource plan

- **Gateway deploy:** the established M6 runbook (build darwin-arm64 → scp → codesign → bootout/bootstrap — never kickstart). **Live-infra touch → explicit user authorization at deploy time**; rollback = `lattice-gateway.bak`. Independently verifiable pre-integration with a one-line curl (image content part → non-zero-token, correct vision behavior).
- **Judge residency:** `qwen2.5-vl-abliterated:7b` ≈ 5.5 GB resident when loaded; the single-slot queue guarantees checks never run concurrently with a render; if a render is requested while the judge is resident, the wired-budget gate refuses the render honestly (resident gauge reads the judge from `/api/ps`) instead of OOMing — the round-2 admission design already covers the collision direction that matters. `OLLAMA_KEEP_ALIVE=1m` unloads the judge after idle; a cold first check costs ~20–40 s (model load), warm bursts are the norm — documented for operators.
- **Tagger sidecar:** port 8904; onnxruntime CPU inference ≈ sub-second per 512² — residency-free. `TAGGER_URL` joins `RIFE_URL`/`REMBG_URL` in `com.img-gen.plist.in` + `install-macos.sh` wiring at integration time.

## Error semantics

- **Submit-time** (`validation_error`, the existing envelope): unknown op/arg; >8 checks; >4 VLM criteria; `palette_conform` without a resolvable target; `tag_roundtrip` with `TAGGER_URL` unset; `validate` on a mode/engine that cannot render (honest gate, like conditioning); budget out of range.
- **Runtime:** malformed judge JSON → `undetermined` (raw head stored; never fail-open); VLM refusal → `undetermined` + reason, **does not consume budget** (a refusing judge must not burn budget forever; the abliterated default makes this rare); tagger infra failure after the client's bounded retry cap → `error` (inert), the render budget is never consumed by infra; deterministic ops cannot error (pure functions) — they pass or fail.
- **Verdict relevance:** only `pass`/`fail` feed verdicts and budget; `undetermined`/`error` are recorded and inert. Job verdicts: `pass | best_of_n | failed | indeterminate` (definitions in the lifecycle section).

## Testing

- **img-gen unit/integration:** httptest fakes for `ChatVision` (assert the exact content-part body + strict JSON parse paths incl. malformed) and the tagger (tags/drift math, vocab-cache); checklist compiler tables (op enum, bounds, batching); retry state machine (budget boundaries 0/1/2/3, mutation derivation incl. dedupe + cap + trigger-word preservation, best-of scoring + tiebreak, both `on_fail` branches); manifest + tEXt provenance round-trips; sync endpoint validation matrix; SSE events during `validating`; **restart recovery** (mid-`validating` kill → idempotent re-evaluation; mid-attempt-2 kill → attach lands attempt 2).
- **Gateway (LLM-router):** translation tests in the providers harness — content parts → `images` (single/multi), streaming path preserved, tool-call normalization composes, token estimation changes `num_ctx` sizing; the existing 20-file test style.
- **Tagger sidecar:** smoke probe (workspace artifact pattern) against real weights; contract tests are img-gen's client-side fakes.
- **Deterministic-op goldens:** synthetic fixtures for `tile_conform` (drifted vs seamless synthetic tiles around the 1.3 ratio) and `palette_conform` (quantized in/out of palette); threshold changes require ruling + golden updates.
- **OpenAPI:** CI grep extended to `POST /api/v1/validate` and the `validate` field on existing schemas (round-1's `check-openapi.py` pattern).

## Docs & skills

- `README.md`: the validate field + endpoint, judge/tagger env vars, latency note.
- `docs/openapi.yaml`: new path + schemas (hand-maintained, per its header note).
- **`skills/img-gen-validation/SKILL.md`** (new): checklist vocabulary, when to attach at submit vs call standalone, budget/on_fail guidance, undetermined/error semantics, judge model + refusal notes, mutation provenance reading.
- `skills/img-gen-assets/SKILL.md`: one paragraph — per-asset validate (palette_conform pairing with `palette_lock`).
- `skills/img-gen-animation/SKILL.md`: one paragraph — frame checks at submit.
- LLM-router repo: its own spec note for the translation change (the B3 cross-repo pattern), plus the tagger sidecar deploy doc.

## Phasing

1. **D1 — gateway translation** (LLM-router): translation + token estimate + tests + repo spec note. Independently shippable; curl-verifiable. Deploys to M6 under explicit authorization; img-gen must not rely on it until live (detection makes the dependency honest).
2. **D2 — critic core** (img-gen): `internal/critic` ops + `ChatVision`/`Tagger` clients + tagger sidecar deployment + **`POST /api/v1/validate`** + its OpenAPI path — usable standalone without the queue.
3. **D3 — integration** (img-gen): `validate` submit fields (jobs / asset specs / gif sources) + their OpenAPI schema extensions, the `validating` stage, retry + best-of-N, attempt provenance (job record + manifest + tEXt), restart recovery, env wiring in plists.
4. **D4 — docs**: skills + README + openapi field schema sync + a showcase validation sweep (one validated job per showcase site family as the acceptance bar, mirroring how rounds are judged).

Ordering: D1 ⊥ D2; D3 needs both; D4 trails D3. The tagger sidecar deploy sits alongside D1/D2 (either order; D2's sync endpoint fails honest on the tag op until it exists).

## Prior-art basis (from the round-3 research sweeps, 2025–26)

- **Loop pattern:** history-free judge + binary checklist + bounded retries + best-of-N retention — OpenAI Cookbook image evals; KitCut judge method; Google ADK Image Scoring; academic lineage: LLM-grounded Diffusion (EECS-2024-206), Self-correcting LLM-controlled Diffusion (+26.5% on DALL·E 3, CVPR 2024), test-time prompt refinement (ICCVW 2025), PromptLoop (2025). Open-source implementors: comfyui-cyclist ("generate until score passes"), Ouroboros sampler node, ComfyGI (hill-climb on ImageReward).
- **Judge:** Qwen2.5-VL 7B Q4 ≈ 6 GB for checklist verdicts; the two-pass local pattern (heavy judge + light captioner) noted and *declined* for v1 (ruling 6). `huihui_ai/qwen2.5-vl-abliterated:7b` benchmarks-documented on M6 (docs/benchmarks-m6.md: "vision default candidate"; qwen3-vl:8b validated e2e).
- **Tagger roundtrip:** WD14 (`SmilingWolf/wd-v1-4-convnextv2-tagger-v2`) standard interrogator; Camie-tagger-v2 (Aug 2025) noted as successor candidate; drift-diff vs prompt tags as prompt-repair signal.
- **Tileability metric:** TexTile (CVPR 2024, juglab/TilArtMetrics) — differentiable tileability classifier (0.982 acc) outperforming SSIM/LPIPS/FID and VLM eyeballing; our `tile_conform` is its metric-lite (wrap-edge error), pinned threshold + goldens.
- **Transparency of the enabler:** gateway multimodal gap verified in LLM-router code (no `image_url`→`images` translation exists; image parts contribute zero tokens) — see `.claude/research/2026-10-28-enrichment-research.md` §1.2 and §2.6.