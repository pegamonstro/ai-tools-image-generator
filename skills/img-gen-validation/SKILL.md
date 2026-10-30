---
name: img-gen-validation
description: Validate and QA generated images through the img-gen critic checklist — VLM judge (vlm_check), tag round-trip (tag_roundtrip), palette conformance (palette_conform), and tile seam detection (tile_conform). Attach at submit for mutate-then-resample retries, or call /api/v1/validate standalone for one-off audits. Triggers on: validation, QA, critic, checklist, tag drift, palette conformance, tileability, prompt-match, mutate-then-resample.
---

# img-gen validation

img-gen's Phase D critic evaluates images against a checklist of up to 8 checks. Four operations are available:

| op | arg shape | purpose |
|---|---|---|
| `vlm_check` | JSON string criterion (e.g. `"\"a cat with green eyes\""`) | VLM judge via lattice chat (CRITIC_MODEL) — binary pass/fail with reason + fix suggestion |
| `tag_roundtrip` | (none) | WD14 tagger round-trip: prompt tags must appear in tagger output above TagFloor (0.35) — requires TAGGER_URL |
| `palette_conform` | (none; target palette on `validate.palette`) | ≥98% of opaque pixels within ΔE≤8 of target palette (CIEDE2000) — explicit target required |
| `tile_conform` | optional `{"max_ratio": 1.0..2.0}` (default 1.3) | wrap-edge ratio ≤ threshold (TileMetric) |

## When to attach `validate` at submit vs call `/api/v1/validate` standalone

- **Attach at submit** (`POST /api/v1/jobs`, `/api/v1/asset-sets`, `/api/v1/gifs`): enables mutate-then-resample retries. Attempt 1 renders base prompt + seed; attempt 2 renders mutated prompt, SAME seed; attempt N≥3 renders mutated prompt, seed+(N−2). Best-of-N by score. Verdicts: `pass`, `best_of_n`, `failed`, `indeterminate`. Provenance: `CriticPending`, `CriticNextPrompt`, `Attempts`, `critic_*` tEXt keys, manifest `validate` + `FrameChecks` records.
- **Call `/api/v1/validate` standalone**: single verdict. The budget is **inert on the sync path** — no retry loop; budget applies to job retries. Use for auditing existing images, CI gates, or when retries are handled externally.

## Budget & `on_fail` guidance

- `budget: 0` (explicit) → evaluate-only, never retries, never fails the job.
- omitted (default) → `budget: 2` (mutate-then-resample retries).
- `budget: 1..3`, `on_fail: "deliver"` (default) → up to N retries, delivers best attempt even if checks fail.
- `budget: 1..3`, `on_fail: "fail"` → up to N retries; job fails with `verdict: "failed"` if the chosen attempt still has failing checks. Use when pipelines must not ship imperfect frames.

## Verdict taxonomy

| verdict | meaning |
|---|---|
| `pass` | chosen attempt has 0 failing checks and ≥1 relevant (pass/fail) check |
| `best_of_n` | chosen attempt has ≥1 failing check, but `on_fail: "deliver"` (or default) |
| `failed` | `on_fail: "fail"` AND chosen attempt has ≥1 failing check |
| `indeterminate` | all checks inert (`undetermined` or `error`) — judge refusal, malformed criterion, timeout, missing tagger/vocab, image decode error. **Never fail-open, never budget-consuming.** |

Per-check `status`: `pass`, `fail`, `undetermined`, `error` (inert statuses; check-time infra failures return `error` within a 200 response — HTTP 400 is only for request-shape failures).

## Reading attempt provenance

- `attempts[].chosen` — boolean; true on the delivered attempt (not an index — the `critic_attempt` tEXt numbering is 1-based).
- `critic_verdict` tEXt key on the chosen PNG — one of `pass|best_of_n|failed|indeterminate`.
- `critic_attempt` tEXt key — 1-based attempt number.
- `critic_<op>` tEXt keys — per-check status (`pass|fail|undetermined|error`).
- Manifest `validate` object: `{verdict, attempts}` + `FrameChecks` for GIF frames.
- Reproduction: resubmit with recorded attempt prompts (`attempts[].prompt`) and seeds.

## Judge model & cold start

`CRITIC_MODEL` default: `huihui_ai/qwen2.5-vl-abliterated:latest` (served by the lattice gateway). Under `OLLAMA_KEEP_ALIVE=1m`, the first validate call after idle incurs ~20–40s cold-start latency while the model loads. Subsequent calls within the keep-alive window are fast. The queue-side `CRITIC_TIMEOUT_S` (default 120s) caps each check call so a hung judge releases the worker slot.

## Prerequisites

- Deployed lattice gateway with image-content translation (chat route for VLM judge).
- WD14 tagger sidecar on port 8904 (`TAGGER_URL`) for `tag_roundtrip`.
- `CRITIC_MODEL` / `CRITIC_TIMEOUT_S` / `TAGGER_URL` / `TAGGER_TIMEOUT_S` env vars wired (see README).
- `palette_conform` requires explicit `validate.palette` (array of `#rrggbb` hex) — no implicit target from `palette_lock` (which is POST-compose).

## Post-restart limitation

After a service restart, recovered jobs re-run checks on the stored attempt idempotently and **finalize without further retries** — the documented limitation: post-restart no further retries.
