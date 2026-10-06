# LoRA training

Train a LoRA adapter — a character, face, body, scene, room, lighting, or
style — on FLUX.1-dev from a set of captioned images, then register it in
img-gen and generate with the trigger word.

Two paths to a trained adapter:

| | Path A: local (`mflux-train`) | Path B: cloud (`ai-toolkit`) |
|---|---|---|
| Where | the M6-class Mac (Apple-silicon, MLX) | a rented NVIDIA GPU (RunPod et al.) |
| Cost | free | ~$2–6 for a typical character run |
| Speed | 600 steps in ~1.5 h at 320×448; many hours at 512×704+ | ~30–60 min |
| Output | mflux-format `.safetensors` | diffusers-format `.safetensors` (also loadable by mflux) |
| Use when | the machine is idle and you don't mind waiting | you want it today, or the dataset is large |

Both produce a single `.safetensors` file that the img-gen pickers load the
same way, so the registration section below applies to either.

## Path A is NOT the stock `mflux-train`

The mflux uv tool (`~/.local/bin/mflux-train`, currently 0.20.x) **cannot
train FLUX.1** — it errors that the flux1 dreambooth trainer is "no longer
supported" and steers you to flux2. The working local trainer is a
**dedicated venv pinned to `mflux==0.15.5`** (the last mflux that still ships
the flux1 dreambooth loop), plus four patches committed in this repo at
`training/patches/` (memory hygiene, local-model support, T5 length). The
shared uv tool is untouched and is what *loads* the trained adapters for
generation.

The patches were proven end-to-end on a 40-image / 600-step character run
(stable memory, no leak, working checkpoints + resume):

| patch | file(s) touched | why |
|---|---|---|
| 01 | `dreambooth/state/training_spec.py`, `dreambooth_initializer.py` | restore `model_path` so a local fine-tune dir can be the base model |
| 02 | `dreambooth_initializer.py`, `dreambooth.py` | clear the MLX Metal buffer cache after load |
| 03 | `dreambooth.py` | force `mx.eval` of loss/params/optimizer state every step — 0.15.5 runs a lazy graph that accumulates ~18 steps then thrashes 44 GB into swap |
| 04 | `dreambooth.py` | clear the buffer cache every step (plot-tick cadence still let GB accumulate) |
| 05 | `models/common/config/model_config.py` | `"dev"` T5 `max_sequence_length` 512→128 — captions pad to max_length, halving it halves attention time per step |

## One-time setup (training host)

```bash
uv venv ~/flux-train-venv --python 3.11
uv pip install --python ~/flux-train-venv/bin/python "mflux==0.15.5"
git clone <this repo> img-gen
img-gen/training/patches/apply.sh ~/flux-train-venv/lib/python3.11/site-packages
```

`apply.sh` applies `01`–`05` in order, fails fast if any anchor text doesn't
match (never re-run mid-way — recreate the venv instead), and drops stale
`__pycache__` afterwards. The generate-side mflux stays whatever the uv tool
ships; only this venv's train code changes.

## Dataset conventions

The 0.15.5 trainer resolves `examples.path` **relative to the process working
directory**, and the config's `examples.images` list must name every image +
prompt pair (see `training/mflux-train.json.example`). `prepare_dataset.py`
builds both the directory and that list for you:

- 15–40 images is the sweet spot for a concept; fewer works for a strict
  scene. Min side 1024px.
- Captions are natural-language sentences ("aria, a woman with short dark
  hair, standing on a rooftop at dusk") **starting with the trigger word** —
  a single lowercase token you will type to summon it in img-gen.
- Snap the training resolution to the dataset's mean aspect ratio (both
  dimensions multiples of 16); resolution drives step time far more than any
  other dial (see the planning table below).

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
- `--write-config` fills `training/mflux-train.json.example` for the 0.15.5
  schema (explicit `examples.images` built from the dataset's sidecars).
  One edit before training: set `model_path`.

`"model"` stays `"dev"` (the family); `model_path` selects the local
FLUX.1-dev fine-tune the LoRA is trained against — train against the model
you generate with, so use the base your img-gen sidecar actually serves.

## Step 2 (Path A) — train

Run on the training host, from a directory containing `datasets/` (the
trainer resolves `examples.path` and `save.output_path` against the CWD, not
the config file):

    rsync -rv training/datasets/<slug> training/<slug>.train.json <host>:~/training/
    ssh <host> 'cd ~/training && nohup ~/flux-train-venv/bin/mflux-train \
        --train-config ~/training/<slug>.train.json > <slug>.log 2>&1 &'

There is no `--dry-run` in 0.15.5 — the first successful steps in the log are
the validation (a syntax/paths error shows within seconds). Monitor:

    ssh <host> 'top -pid $(pgrep -f mflux-train) -l 1 -stats pid,cpu,mem'
    ssh <host> 'tail -5 ~/training/<slug>.log'          # loss postfix on the tqdm line
    ssh <host> 'ls ~/training/out/<slug>/_validation/images/'   # preview PNGs per generate_image_frequency

Memory on a 32 GB host with the patches applied: steady 8–11 G, per-step
peak 18–27 G, swap flat. `quantize: 4` and `batch_size: 1` are required at
this class of machine. Don't run training while the img-gen sidecar is
generating — both contend for the same GPU and each slows to a crawl.

Resume a dead run from the newest checkpoint:

    ssh <host> 'cd ~/training && nohup ~/flux-train-venv/bin/mflux-train \
        --train-config ~/training/<slug>.train.json \
        --train-checkpoint ~/training/out/<slug>/_checkpoints/0000400_checkpoint.zip > <slug>-resume.log 2>&1 &'

Re-running against a non-empty `out/<slug>` makes 0.15.5 timestamp the
directory instead of reusing it — `rm -rf out/<slug>` first if the run is
restart-from-zero and the dir is only old checkpoints.

Planning numbers (measured, 4-bit, rank 16, batch 1, T5 patch applied):

| resolution | step time | notes |
|---|---|---|
| 320×448 | 5–9 s | first ~70 steps 50–80 s (Metal kernel warmup); ~600 steps ≈ 1.5 h |
| 512×704 | 155–257 s | 27 G per-step peaks; a 600-step run ≈ 1.5–2 days |

Dataset encoding is a one-time up front pass (~1.3 img/s), and each
`generate_image_frequency` tick pauses ~7 min to render the validation image.

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
(MPS/Apple-silicon is non-convergent for ai-toolkit — never train it locally.)

## Step 3 — register in img-gen

Extract the adapter from the newest checkpoint bundle and copy it to the
models tree:

    unzip -p ~/training/out/<slug>/_checkpoints/0000600_checkpoint.zip \
        0000600_adapter.safetensors > ~/mflux-models/loras/<slug>.safetensors

(checkpoints are `out/<slug>/_checkpoints/NNNNNNN_checkpoint.zip` containing
`NNNNNNN_adapter.safetensors`; the numbered file *is* the LoRA. For
ai-toolkit output, just copy its `.safetensors`.)

Add it to `models.json` (gitignored data file — `models.json.example` has the
shape) so it appears in the LoRA picker:

    { "key": "lora-aria", "label": "aria (trained)", "value": "<abs path>/mflux-models/loras/aria.safetensors" }

`value` must be the absolute `.safetensors` path as the generating host sees
it (or an HF `repo-id` the loader can pull). Restart img-gen if it caches the
catalog at startup. Optionally add a preset to `presets.json` so the
character is one click away.

## Step 4 — verify with the trigger word

Smoke a low-cost generation with the trigger (512px, 8 steps, scale 1.0).
The `/api/v1` contract passes mflux argument values through verbatim, so
`loras[].name` here is the `.safetensors` path/hf-id itself:

    curl ... "/api/v1/generate" -d '{
      "genre": "portrait", "size": "512x512", "steps": 8,
      "loras": [{"name": "<abs path>/mflux-models/loras/<slug>.safetensors", "scale": 1.0}],
      "prompt": "<trigger>, <a prompt from the training captions>"
    }'

Then the same prompt without the LoRA to compare — fixed seed, same
resolution. The trained concept should appear only with the adapter on.
(Training-resolution validation previews look soft/blurry — that's the
low-res render, not the adapter; judge at 1024px.)

## Config reference

`training/mflux-train.json.example` is the 0.15.5 `TrainingSpec` schema;
defaults are sane for a character LoRA (rank 16 on attention/ff across all 19
joint + 38 single transformer blocks, AdamW 1e-4, checkpoints every 100).
The one field everyone trips on: **loop length is
`training_loop.num_epochs × number_of_images`, not `steps`** — `steps` only
sizes the sampler's sigma schedule. Compute `num_epochs = ceil(target_steps
/ N_images)` before launching, or your "700 steps" turns into 1600.