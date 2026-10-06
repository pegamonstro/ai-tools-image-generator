# Custom LoRA training — local mflux-train + cloud ai-toolkit runbook & tooling

**Spec author:** 2026-10-05. Goal: let the user train character/face/scene/room/ambient-style LoRAs from uploaded image sets, and load the result in img-gen.

Constraint (settled): mflux consumes **diffusers-format** LoRAs; output of either path below must land as a single `.safetensors` loadable by `mflux --lora <path-or-repo>` (mflux-train emits its own format that mflux itself loads — that is fine). XLabs format is rejected.

## Path A — local on the M6 (proven recipe: patched mflux 0.15.5 venv)

**Amendment (2026-10-06, after the first real run):** the uv-tool `mflux-train`
(0.20.x) cannot train FLUX.1 — it rejects the flux1 dreambooth trainer. The
working local trainer is a dedicated venv pinned to `mflux==0.15.5` plus four
patch families committed at `training/patches/` (restore `model_path`, force
per-step `mx.eval` — 0.15.5 runs a lazy graph that otherwise thrashes into
swap — clear the Metal buffer cache per step, cap T5 `max_sequence_length` at
128). Proven end-to-end on a 40-image / 600-step character run: ~1.5 h at
320×448, stable memory, resumable checkpoints, adapter loads in the unpatched
generate-side runtime. Full runbook: `training/README.md`.

- MLX-native; Dreambooth-LoRA style (strong for faces/characters/
  single-subjects; usable for scenes/ambient with a wider caption set).
- Runs on M6 (32 GB class machine): `quantize: "4"` training config, `batch_size: 1`, modest rank.
- Loop length is `num_epochs × dataset_size` (config `steps` is sampler-only); checkpoint resume via `--train-checkpoint <zip>`.

## Path B — cloud ai-toolkit (higher ceiling; broad style/scene/environment sets)

ostris/ai-toolkit (repo, community standard), FLUX.1-dev LoRA config, emits **diffusers directly**. Costs ~$2-6 on a RunPod 4090/A100 for a typical 20-40 image run (~3-6 h). MPS (Apple) is non-convergent — never run ai-toolkit locally.

## Dataset conventions (shared by both paths)

```
training/datasets/<slug>/
  00.png 00.txt 01.png 01.txt ...   # caption files describe the whole image
```

- Trigger word: pick one (e.g. `aria`); **prepend it to every caption**; later generate prompts start with it.
- 15–40 images, varied framing/pose/lighting; 1024px min side; no text overlays.
- Caption style: FLUX prefers natural-language sentences (`"aria, a woman with long silver hair sitting on a velvet chair, warm lamp light"`).

## Tooling delivered (this repo)

- `training/README.md` — the runbook: both paths, exact commands, memory pitfalls, registration steps.
- `training/mflux-train.json.example` — config template for Path A.
- `training/prepare_dataset.py` — stdlib-Python helper: given a folder + trigger + optional caption file, sizes images (Pillow-free: only reorganizes/copy), writes/validates `.txt` sidecars, prints a manifest and a ready-to-run mflux-train command.
- Non-goal (YAGNI): in-app training UI/endpoints. Registration is a one-line `models.json`/`presets.json` edit.

## Registration in img-gen (both paths identical)

1. Copy the produced `.safetensors` to the M6: `~/mflux-models/loras/<slug>.safetensors` (scp).
2. `models.json` → `loras` entry `{key: <slug>, label: …, value: "/Users/<you>/mflux-models/loras/<slug>.safetensors"}` (data file, gitignored; real paths never committed).
3. Optional preset wiring: add `{"name": "<slug>", "scale": 0.8–1.0}` to that character's `loras` in `presets.json`.
4. Smoke: 512×512, 8 steps, trigger word in prompt, upscale later if wanted.

## Acceptance

- [ ] `training/prepare_dataset.py --help` works; validates missing captions; dry-run prints manifest + mflux-train command.
- [ ] `mflux-train --dry-run --config …` passes on M6 with the repo template (verifies schema/paths against the real installed mflux).
- [ ] README documents registering + generating with the new trigger word.
- [ ] (Deferred to first real use) an actual trained LoRA — requires the user's image set; runbook makes that a one-command step.