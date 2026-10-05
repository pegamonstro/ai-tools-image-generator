# LoRA training

Train a LoRA adapter — a character, face, body, scene, room, lighting, or
style — on FLUX.1-dev from a set of captioned images, then register it in
img-gen and generate with the trigger word.

Two paths to a trained adapter:

| | Path A: local (`mflux-train`) | Path B: cloud (`ai-toolkit`) |
|---|---|---|
| Where | the M6 (Apple-silicon, MLX) | a rented NVIDIA GPU (RunPod et al.) |
| Cost | free | ~$2–6 for a typical character run |
| Speed | hours (700 steps ≈ 4–8 h at 1024px) | ~30–60 min |
| Output | mflux-format `.safetensors` | diffusers-format `.safetensors` (also loadable by mflux) |
| Use when | the M6 is idle and you don't mind waiting | you want it today, or the dataset is large |

Both produce a single `.safetensors` file that the img-gen pickers load the
same way, so the registration section below applies to either.

## Dataset conventions

`mflux-train` auto-discovers a dataset from a plain directory: every image
(`.jpg`, `.jpeg`, `.png`, `.webp`) must have a matching `<name>.txt` caption
sidecar next to it. Mixing normal images with mflux's `_in`/`_out` training
pairs is an error.

- 15–40 images is the sweet spot for a concept; fewer works for a strict scene.
- Min side 1024px; the trainer crops/resizes to `max_resolution`.
- Captions are natural-language sentences ("aria, a woman with short dark
  hair, standing on a rooftop at dusk") **starting with the trigger word** —
  a single lowercase token you will type to summon it in img-gen.
- Every caption begins with the trigger; remaining text describes what varies
  in the image. Keep them varied.

The `datasets/` and `out/` trees, `.safetensors` files, and per-dataset
`*.train.json` configs are gitignored: they are your data, not repo files.

## Step 1 — prepare the dataset

    python3 training/prepare_dataset.py --src ~/Downloads/aria-set \
        --dataset aria --trigger aria --captions ~/Downloads/aria-set/captions.txt \
        --write-config

- `--dataset` sets the slug: files land in `training/datasets/<slug>/` and
  the config is written to `training/<slug>.train.json`.
- `--captions` takes a manifest of `<stem><TAB><caption>` lines (one per
  image, stem = source filename without extension; `#` comments allowed).
  Without it, the script uses existing `<name>.txt` sidecars in the source
  or destination directories.
- Missing captions abort with the list of offending files — either fix the
  manifest, add sidecars, or pass `--bootstrap-captions` to fill in
  placeholders (`"<trigger>, a <slug>"`, worth hand-editing afterwards).
- `--check-only` validates and prints the manifest without copying.
- `--write-config` fills `training/mflux-train.json.example` — the generated
  config points `data` at the new dataset and needs one edit before training:
  `model_path`.

Edit the generated `training/<slug>.train.json` and set:

    "model_path": "~/mflux-models/persephone"   (the mflux-native base dir)

`"model"` stays `"dev"` (the family); `model_path` selects the local FLUX.1-dev
fine-tune the LoRA is trained against — train against the model you generate
with, so use the base your img-gen sidecar actually serves.

## Step 2 (Path A) — train on the M6

The datasets dir travels with the config (relative `data:` and
`checkpoint.output_path:` resolve against the config file's directory), so
copy the whole layout to the M6 and keep it there:

    rsync -rv training/datasets/<slug> <m6>:~/training/datasets/
    scp training/<slug>.train.json <m6>:~/training/

Validate — `--dry-run` checks the config schema, discovers
image+caption pairs, and exits without touching the GPU:

    ssh <m6> '~/.local/bin/mflux-train --dry-run --config ~/training/<slug>.train.json'

Train — a 700-step run at 1024px/4-bit/rank 16 takes hours; run it inside
`tmux` or with `nohup` so ssh drops don't kill it:

    ssh <m6> 'tmux new -d -s lora "~/.local/bin/mflux-train --config ~/training/<slug>.train.json 2>&1 | tee ~/training/<slug>.log"'

With `monitoring.generate_image_frequency` set, preview PNGs land in
`out/<slug>/preview/` and the loss plot in `out/<slug>/loss/` — check them
periodically (`ssh <m6> 'ls ~/training/out/<slug>/preview/'`). The previews
use the prompts in `data/preview*.txt`; mflux ships defaults if you don't.

If a run dies mid-way, resume from the newest checkpoint:

    ssh <m6> 'tmux new -d -s lora "~/.local/bin/mflux-train --config ~/training/<slug>.train.json --resume"'

### Memory pitfalls

- `quantize: 4` is required on ≤48GB machines; 8-bit and unquantized 1024px
  runs do not fit.
- `training_loop.batch_size: 1` — batch 2+ at 1024px on rank-16 attention
  targets outgrows 48GB.
- Tight on headroom? Set `low_ram: true` and/or
  `gradient_checkpointing: true` (slower, less memory), and cap
  `max_resolution` at 512 for long runs.
- Raise `low_ram`'s `--mlx-cache-limit-gb` if the loader complains about the
  weight cache.
- Don't train while the img-gen sidecar is serving generations: both
  processes contend for the same GPU and each slows to (near) a crawl.

## Step 2 (Path B) — cloud via ai-toolkit

[ostris/ai-toolkit](https://github.com/ostris/ai-toolkit) trains FLUX.1-dev
LoRAs on rented NVIDIA GPUs; its FLUX trainer is the best-maintained open
option and the mflux runtime loads its output directly.

Sketch (adjust `GPU_COUNT`, memory, and disk class when renting):

    git clone https://github.com/ostris/ai-toolkit && cd ai-toolkit
    python3 run.py config/<slug>.yaml

In the config: `base_model` = FLUX.1-dev (the checkpoint the sidecar serves),
`folder_path` = the same dataset layout from Step 1 (image + `.txt` pairs,
trigger-word-first captions), `trigger_word` = your token, `network_dim` 16,
`steps` ~700–1500. Output: a single `<name>.safetensors` in the repo's
`output/` dir — take it from there to Step 3. Don't train against fp8
checkpoints; use the bf16 base and let the runtime handle precision.

## Step 3 — register in img-gen

Copy the adapter from the checkpoint bundle to the models tree:

    unzip -p out/<slug>/checkpoints/0000700_checkpoint.zip 0000700_adapter.safetensors \
        > <models>/loras/<slug>.safetensors

(mflux checkpoints are `<output_path>/checkpoints/NNNNNNN_checkpoint.zip`
containing `NNNNNNN_adapter.safetensors`; the numbered file *is* the LoRA.
For ai-toolkit output, just copy the `.safetensors`.)

Add it to `models.json` (gitignored data file — `models.json.example` has the
shape) so it appears in the LoRA picker:

    { "key": "lora-aria", "label": "aria (trained)", "value": "~/mflux-models/loras/aria.safetensors" }

Optionally add a preset to `presets.json` so the trigger phrase is one click
away, and restart img-gen if it caches the catalog at startup.

## Step 4 — verify with the trigger word

Smoke a low-cost generation with the trigger (512px, 8 steps, scale 1.0):

    curl ... "/api/v1/generate" -d '{
      "genre": "portrait", "size": "512x512", "steps": 8,
      "loras": [{"name": "<slug>.safetensors", "scale": 1.0}],
      "prompt": "<trigger>, <a prompt from the training captions>"
    }'

Then the same prompt without the LoRA to compare. The trained concept should
appear only with the adapter on.

## Config reference

`training/mflux-train.json.example` documents every field the template uses;
defaults are sane for a character LoRA on an Apple-silicon Mac (700 steps,
AdamW 1e-4 cosine, rank 16 on attention q/k/v/out across all 19 joint + 38
single transformer blocks, checkpoints every 100 steps). Bump `rank` to 32
for complex scenes/styles; never reduce `steps` below ~500.