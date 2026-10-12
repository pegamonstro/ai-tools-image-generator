---
name: img-gen-animation
description: Build animated GIFs with the img-gen service — assemble frames from existing generations, generate frame sequences, or reference future keyframe interpolation. Use when a project needs animated images, loading indicators, or sprite playback.
---

# img-gen animation

`make_gif` (MCP) or `POST /api/v1/gifs` turns frame sequences into GIFs.

## Frame sources

- `from_frames` — assemble given images. Each frame is
  `{job: <img-gen job id>}` or `{path: "<data-dir-relative path>"}` or
  `{b64: "<base64 png>"}` or `{set: "<set id>", file: "<set file>"}`.
  2-64 frames.
- `from_spec` — img-gen generates the frames: `{"type":"from_spec",
  "request":{"mode":"asset","prompt":"<subject>","size":"64x64","seed":7},
  "count":8}`. Frames get `seed+i` and a frame-index suffix; keep prompts
  style-stable (state the shared style once and describe only per-frame
  motion).
- `keyframes` — interpolation between keyframes needs the RIFE sidecar;
  rejected with `not_implemented` until phase 3.
- `composite` — layer statics under an animation: `{"type":"composite",
  "composite":{"anim":[<2-64 frame refs>],"statics":[{"from":<frame ref>,
  "pos":[x,y]}, ...]}}`. The overlay must fit inside the anim canvas or the
  GIF fails at compose (named error, no silent crop). Compositing happens
  pre-quantization, so 1-bit GIF alpha stays halo-free; statics stay crisp
  across frames. Combine with palette-locked sets for coherent title
  screens.

## Timing and formats

`delay_ms` 10-5000 (default 100; GIF delays are centiseconds, so 100ms =
10). Values under 20ms round UP to 20ms — GIF frame delays have a
2-centisecond browser-safe floor, so 10-19 render one step slower than
asked. `loop: 0` loops forever. `formats: ["gif","webp"]` also writes an
animated WebP when `gif2webp` is on the server's PATH — otherwise the
skip is recorded in `manifest.json` (`gif.webp`), not an error.

## Workflow

1. Submit and wait (the MCP tool blocks): text result is the parent job
   JSON; the GIF itself arrives as an inline preview.
2. `read_export` (MCP) returns the job view, the GIF, and `manifest.json`.
3. Deliver with `"dest"` or copy from the set dir
   (`DATA_DIR/sets/{set_id}/<name>.gif`). A delivered GIF set contains:
   `<name>.gif`, the optional `.webp` variant, `<name>.manifest.json`, and the
   source frames (`source-frame-NNNN.png` — from-frames and composite anim)
   plus composite static layers (`static-layer-NN.png`).