# Additional LoRAs — Research & Implementation Scope

> Research into uncensored/adult LoRAs that enhance the img-gen NSFW tool,
> their performance against the M6 host, and how to wire them in.

## Executive summary

The base model, **Persephone 2.0**, is already uncensored (a dedicated NSFW
FLUX.1-dev transformer fine-tune baked in place of dev's weights). So "more
uncensored LoRAs" is partly a misnomer: there is no uncensoring left to do.
What actually *enriches* the tool are **style, quality, and anatomy LoRAs**
stacked on top of the uncensored base. The existing `shauray/flux-uncensored-lora`
entry in `models.json` is a pre-Persephone leftover (it uncensored plain
`dev`); harmless to keep, but no longer load-bearing.

The good news: **the entire plumbing already exists.** Tier 1 shipped
multi-LoRA (per-request `loras: [{name, scale}]` → mflux `--lora <name> <scale>`),
the frontend has a LoRA dropdown + per-LoRA scale slider + a free-text field for
arbitrary HF ids, and mflux auto-downloads HuggingFace LoRA repos on first use.
Adding a LoRA is therefore **catalog-only** — a new entry in `models.json`
`loras[]`. No Go or JS changes are required.

## Hardware & compatibility assessment (M6: Apple M6, 32 GB RAM, MLX)

Empirically verified this session against the running mflux sidecar:

- **A Diffusers-format FLUX.1-dev LoRA generates correctly on M6**: a 512×512,
  20-step generation with `alvdansen/illustration-1.0-flux-dev` at scale 0.9
  completed in **~82 s including the one-time HF download** (base model already
  resident, so this is representative of cold-LoRA first use).
- **Memory**: FLUX LoRAs are 35–150 MB runtime adapters. The sidecar runs
  `--no-bake-lora` (adapters applied at inference, never merged into fp16 —
  the fp16 merge is what OOMs). The 4-bit Persephone base already fits; stacking
  2–3 LoRAs adds negligible memory. **32 GB is ample.**
- **Format compatibility (the one real constraint)** — mflux accepts:
  - ✅ BFL format
  - ✅ Diffusers format (all `alvdansen/*` repos, and anything HF-hosted as a
    `--lora org/model` repo)
  - ✅ LoKR / LyCORIS (FLUX.1 and FLUX.2)
  - ❌ **XLabs-AI format** (e.g. `flux-RealismLora`) — *not* supported.
- **Base compat**: LoRAs must be FLUX.1-dev (Persephone is a dev fine-tune).
  FLUX.1-schnell / SDXL / SD1.5 LoRAs will not apply.
- **Multi-LoRA**: supported end-to-end (sidecar iterates the `loras` list into
  repeated `--lora` flags; the frontend already stacks rows).

### Open item — repeat-download latency

The LoRA did not appear under a `models--*` dir in `~/.cache/huggingface/hub`,
so it is unconfirmed whether mflux caches LoRA weights persistently or re-fetches
them each request. If re-fetching proves true, mitigate by pre-downloading each
curated LoRA to a local dir and referencing the **local path** in `models.json`
(instead of the HF repo id). Verify before committing to repo-id references for
the default catalog.

## Curated LoRA shortlist

Recommended weights are from each model's own card.

### A. Style / aesthetic variety (the main enrichment lever)

| LoRA | Source | Format | Trigger | Weight |
|------|--------|--------|---------|--------|
| `alvdansen/illustration-1.0-flux-dev` | HF | Diffusers | none | 0.8–1.0 |
| `alvdansen/anime-style-flux-lora` | HF | Diffusers | none | 0.7–1.0 |
| Madbear's Best Anime Style | Civitai (35 MB) | verify | none | 0.75–1 |
| flux.1 dev modern anime v2 | Civitai (146 MB) | verify | `modern anime style` | — |

`alvdansen/illustration-1.0-flux-dev` is the strongest first pick: broad range
(anime/manga/watercolor/risograph), no trigger word, HF-hosted (safe Diffusers
format), and **already verified working on M6**.

### B. Quality / realism / detail

| LoRA | Source | Weight |
|------|--------|--------|
| Flux.1 Turbo Detailer | Civitai (18.9k dl) | 0.2–0.9 |
| Nelux_Realism | Civitai | 0.6–0.65 |

### C. NSFW anatomical correction

| LoRA | Source | Weight |
|------|--------|--------|
| FLUX Photo nuds DEV | Civitai (4.2k dl) | 0.1–0.6 |

### D. Flagged — do not pre-bake into the default catalog

- **Character / celebrity persona LoRAs** — persona-rights and likeness concerns;
  surface them via the existing free-text LoRA field, not the default dropdown.
- **Kink / fetish-specific LoRAs** — highly subjective; leave to user curation
  through the free-text field.

## Implementation scope

1. **Catalog-only (now, zero code)** — add curated entries to `models.json`
   `loras[]` in the form `{ "key", "label", "value" }` where `value` is an HF
   repo id (`org/model`) or a pre-downloaded local path. The frontend dropdown,
   scale slider, and free-text field already consume these.
2. **Format gate** — before adding any Civitai LoRA, confirm it is *not* XLabs
   (a quick `/generate` with the LoRA errors clearly if unsupported, or inspect
   the safetensors keys for `lora_unet_*` prefixes).
3. **Caching** — resolve the repeat-download question; prefer pre-downloaded
   local paths for the default catalog if mflux does not persist HF LoRA downloads.
4. **Tier 3 tie-in** — the "character preset" feature naturally bundles a base
   model + style LoRA + scale + trigger word + negative prompt into one named,
   one-click preset. This is where these LoRAs get productized; carry this list
   into the Tier 3 plan rather than exposing bare LoRAs only.

## References

- mflux LoRA docs: `mflux-generate --help` (`--lora`, `--no-bake-lora`),
  [mflux GitHub](https://github.com/filipstrand/mflux)
- LoRA sources: [illustration-1.0-flux-dev](https://huggingface.co/alvdansen/illustration-1.0-flux-dev),
  [anime-style-flux-lora](https://huggingface.co/alvdansen/anime-style-flux-lora),
  [flux.1 dev modern anime](https://civitai.com/models/640405/flux1-dev-modern-anime),
  [Madbear's Best Anime Style](https://civitai.com/models/680645/madbears-best-anime-style-on-flux1-dev),
  [Flux.1 Turbo Detailer](https://civitai.com/models/930386/flux1-turbo-detailer),
  [Nelux_Realism](https://civitai.com/models/1656505/neluxrealism),
  [FLUX Photo nuds DEV](https://civitai.com/models/675247/flux-photo-nuds-dev)
