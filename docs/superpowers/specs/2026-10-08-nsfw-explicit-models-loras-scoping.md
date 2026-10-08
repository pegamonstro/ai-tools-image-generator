# NSFW Explicit Models & LoRAs — Research & Integration Scoping

> A follow-up to `2026-10-05-additional-loras-research.md`, focused on the
> specific goal: maxing out img-gen's fidelity for explicit, erotic,
> pornographic generation through the lattice-sidecar stack.

## Executive summary

The base model, **Persephone 2.0**, is already uncensored. Web evidence
(independent tests of FLUX.1-dev-class uncensored models) shows what that
base actually struggles with, and it is not "refusal":

1. **Two-person scenes soften.** Even with the best uncensored LoRA, FLUX.1
   consistently tones down explicit *acts* between two people. This is the
   single biggest quality gap for the stated goal.
2. **Anatomy drift** — hands, proportions, body correctness degrade
   specifically in difficult positions.
3. **Composition** — getting two subjects into a deliberate arrangement at
   all, before any explicitness question.

All three of those are addressable with **LoRAs**, which we have proven
catalog-only integration for. A base-model swap to a different explicit
checkpoint is possible (a conversion recipe exists and has been proven
once), but it costs hours of work and loses Persephone's identity, for a
gain the LoRA stack overlaps with. **Recommendation: LoRA-first, checkpoint
swap as an optional second phase.**

## Constraint matrix (verified this session)

The sidecar host (Apple silicon, 32 GB, MLX/mflux) imposes the format rules:

| Candidate form | Loadable? | Note |
|---|---|---|
| HF-repo-id download at request time | ✅ | re-fetches per request; use only for ad-hoc |
| Pre-downloaded local `.safetensors` path | ✅ | the default-catalog convention |
| Diffusers-format LoRA | ✅ | verified live (~82 s first gen incl. download) |
| BFL-format LoRA | ✅ | |
| LoKR / LyCORIS LoRA (`lora_unet_*`, `lycoris_*` prefixes) | ✅ | mflux ≥0.18 |
| DoRA (`lora_A/lora_B` PEFT layout) | ❌ | excluded from shortlists |
| Single-file merged FLUX.1 `.safetensors` checkpoint | ❌ | open mflux issue #182; needs conversion |
| HF-layout / mflux-save full model dir | ✅ | how Persephone is deployed |
| Other architectures (FLUX.2, Z-Image, Qwen Image…) | ⚠️ | mflux supports them, but our sidecar endpoints (`/fill`, `/redux`, `/pose`) and presets are FLUX.1-bound — out of scope |

## Findings

### A. Explicit-capable full checkpoints (heavy path)

All of these ship as single ComfyUI-style files → **every one needs the
Persephone conversion recipe** (download → diffusers conversion → assemble
around dev encoders → bake 4-bit) before the sidecar can load it.

| Checkpoint | Source | Stats | Notes |
|---|---|---|---|
| Fluxed Up | Civitai 847101 | most-downloaded NSFW FLUX (~8.3 M) | FP16/FP8/GGUF variants; updated Feb 2026 |
| MS Flux NSFW V3 | Civitai 1333695 | | SFW/NSFW paired versions |
| DEMON CORE | Civitai 155977 | | de-distilled, guidance-free |
| Project Gaia | Civitai 720719 | | NF4/FP8 variants |
| NSFW UNLOCKED V2.0 | Civitai 2119037 | ~15 GB FP8 | euler, 20+ steps, CFG 1 |
| human v1.0 | Civitai 2456602 | | ComfyUI-only pipeline, hardest to port |

Verdict: **none required for phase 1.** They are the fallback if the LoRA
stack can't deliver two-person explicit fidelity.

### B. Uncensored / activation LoRAs (HF-hosted = frictionless)

| LoRA | Source | Format | Trigger | Weight |
|---|---|---|---|---|
| `enhanceaiteam/Flux-uncensored` (v1) | HF | Diffusers | `nsfw`, `nude` | 0.7–1.0 |
| `kenerateai/Flux-uncensored` | HF | Diffusers (687 MB) | similar family | ~0.8 |
| "Lustly Uncensored v1" | Civitai | verify | none | ~0.8 — the one independent testing rated best |

Note: Persephone mostly *is* uncensored already — these matter as **boosters
stacked for two-person scenes**, not as gate-openers. Test them stacked, at
moderate scale, rather than alone.

⚠️ **Excluded:** `Flux-Uncensored-V2` (EnhanceAI, HF mirrors). Its own model
card's example prompts sexualize minors — illegal content, disqualifying.
This is also why model *cards* must be vetted, not just download counts.

### C. Anatomy LoRAs (the correctness lever)

| LoRA | Source | Recommended settings |
|---|---|---|
| SCG-Anatomy-Flux1.d | Civitai 640156 (mirror-listed) | scale 0.65–1.2, guidance 3.0, DEIS/DDIM, 25 steps |
| "slider" anatomy control | CivArchive 2468993 | NSFW-labeled; non-commercial |
| FLUX Photo nuds DEV | Civitai 675247 (already shortlisted 2026-10-05) | 0.1–0.6 |

### D. Composition / act LoRAs (the two-person lever)

| LoRA | Source | Trigger | Weight |
|---|---|---|---|
| Multiple People Flux/SDXL | Civitai 540150 | `multipeoplelora` | ~0.8 |
| Multiple Human Poses | Civitai 1891239 | none | 0.7–1.0 |
| Dynamic Poses FLUX+SDXL | Civitai 332248 | none | 0.6–0.9 — 15.7 k dl, overwhelmingly positive |

Composition + booster + anatomy stacked is the evidence-backed approach to
two-person explicit scenes.

## Content-policy & acquisition gate (non-negotiable)

- Civitai login/age-gates explicit downloads; round-2 of the 2026-10-05
  shortlist is still blocked on a token (see open items). HF-hosted entries
  have no gate and are preferred for default-catalog additions.
- Mirrors for the gated ones: CivArchive, civitas-style mirrors,
  civitai.green — mirror availability is per-model and unverifiable from
  here, so the token or a manual download remains the reliable path.
- FLUX.1-dev's non-commercial license still binds everything derived from
  it; fine for this self-hosted tool, worth stating in the public-spec prose.
- **Never install a model/LoRA whose card or examples include sexualized
  minors**; treat an unvetted "uncensored" repo as guilty until inspected.

## Integration scoping through the lattice/gateway

**Traffic topology (verified in code):** img-gen talks to the image sidecars
directly (`IMAGE_URL` / fill / redux / upscale env routing); the lattice
gateway is a **chat-only** touchpoint used solely for prompt enhancement
(`lattice.Client.BaseURL`, `ENHANCE_SYSTEM` env). Image traffic never crosses
the gateway, and catalogs live in img-gen (`models.json`) while the sidecar
resolves values. Consequences:

1. **LoRA additions = zero gateway changes, zero code.** Add a file on the
   sidecar host (or use an HF repo id) + one `models.json` entry + restart.
   HF repo-id entries need no file placement; the sidecar fetches on first
   use (re-fetch each request — so pre-download for anything kept).
2. **Model additions (full checkpoint) = zero gateway changes** but the
   heavy conversion recipe side, on the sidecar host: download the
   FP8/GGUF → convert transformer to diffusers layout (proven once, venv
   exists there) → assemble around dev's text/vae encoders → bake 4-bit →
   new `models.json` `models[]` entry. Transient disk ~35–70 GB, hours of
   conversion; 32 GB RAM headroom is proven (Persephone shipped fine).
3. **Prompt-enhancement interplay:** `ENHANCE_SYSTEM` (default in
   `cmd/img-gen/main.go`) already instructs the gateway-side LLM that
   explicit content is permitted and expected, with refusal detection
   downstream. The enhancer does not currently receive the active
   model/LoRA context — an optional, small improvement would pass the
   active LoRA trigger words into the enhancement context so it preserves
   them while rewriting. Not required for phase 1.
4. **No new lattice endpoints, no img-gen API surface changes.** Gen modes,
   catalog endpoints, and the frontend picker all already handle whatever
   the catalogs declare.

## Recommended scope

### Phase 1 (recommended first): explicit LoRA stack, catalog-only

1. Pre-download on the sidecar host, verify format (safetensors keys, not
   XLabs/DoRA), then add to `models.json` `loras[]`:
   - one uncensored booster from section B (HF-first: `enhanceaiteam/Flux-uncensored`),
   - the two-person composition picks from section D,
   - one anatomy pick from section C (token-gated → manual download if needed).
2. Verify live: a stacked (composition × booster × anatomy) two-person
   generation on the sidecar; check per-LoRA scale behavior at ranges from
   each card; record winning scaling in the preset catalog (`presets.json`)
   as an "explicit act" preset.
3. Keep persona/kink-specific additions out of the default catalog
   (free-text covers them) — carried over from the 2026-10-05 ruling.

### Phase 2 (optional, only if phase 1 disappoints): alternate explicit base

Convert **Fluxed Up FP8** (or Project Gaia NF4) via the proven recipe and
install as a second selectable base model. Identity-LoRA compatibility
(fnc, etc.) must be re-tested against the new base before exposing it.

### Phase 3 (out of scope unless evidence demands): second sidecar

SDXL/Pony/Illustrious-class models need a non-mflux backend (different
VAE/scheduler stack) — a new sidecar, new model hosting, new endpoints. Not
recommended now; only revisit if FLUX-native outputs remain unsatisfactory
after phases 1–2.

## Tasks

- [ ] Phase 1: download + format-verify LoRA shortlist files on the sidecar
      host; inspect card/license for each pick first.
- [ ] Phase 1: add catalog entries (`models.json` local paths; HF ids only
      as documented ad-hoc examples) + sidecar restart + catalog reload.
- [ ] Phase 1: stacked two-person generation test; tune scales; capture the
      best combination in a `presets.json` preset.
- [ ] Phase 2 (gated on phase 1 results): conversion run for one checkpoint
      + models.json entry + base-compat re-test.
- [ ] Docs: update `models.json.example` / README when catalog entries land.

## References

- [SCG-Anatomy-Flux1.d (CivArchive mirror)](https://civarchive.com/models/640156?modelVersionId=715962)
- [Flux-Uncensored v1 README (HF)](https://huggingface.co/enhanceaiteam/Flux-uncensored)
- [kenerateai/Flux-uncensored (HF)](https://huggingface.co/kenerateai/Flux-uncensored)
- [shauray/flux.1-dev-uncensored-q4 (HF)](https://huggingface.co/shauray/flux.1-dev-uncensored-q4)
- [NSFW UNLOCKED V2.0 FP8 (CivArchive)](https://civarchive.com/models/2119037?modelVersionId=2421111)
- [Multiple People Flux (Civitai)](https://civitai.com/models/540150/multiple-people-fluxsdxl)
- [Multiple Human Poses (Civitai)](https://civitai.com/models/1891239/multiple-human-poses)
- [Dynamic Poses FLUX+SDXL (Civitai)](https://civitai.com/models/332248/dynamic-poses-flux-sdxl)
- [FLUX.1-dev base license/usage policy (HF)](https://huggingface.co/black-forest-labs/FLUX.1-dev)