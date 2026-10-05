# img-gen — Image Editing & Multi-Reference Design

**Status:** draft, pending user review.
**Date:** 2026-10-05
**Supersedes:** the §13 non-goal in `2026-10-04-img-gen-design.md` ("Image edit UI — wire it later").

Extends the img-gen web tool with three new image operations — **edit**
(image-to-image restyle), **inpaint** (masked region edit), and **blend**
(multi-reference generation) — on top of the existing **generate** mode.

---

## 1. Purpose

Let the user, from the browser:

1. **Edit** an existing image (uploaded or previously generated) from a prompt —
   restyle it while keeping its content (image-to-image).
2. **Inpaint** — repaint a chosen region of an image (mask a face, add an object,
   change the background) while the rest stays untouched.
3. **Blend** — generate a *new* image from a prompt plus **multiple** reference
   images (style, subject, composition), with per-reference weights.

All three run on the local lattice's FLUX stack, which already supports every one
of these techniques at the model layer — the work is exposing them, not building
them.

## 2. Background — what the backend already does

The model library in use is [mflux](https://github.com/filipstrand/mflux)
(FLUX on Apple Silicon via MLX). It ships distinct CLI entry points for each
technique, each backed by its own checkpoint:

| Technique | CLI | Checkpoint | Download |
|---|---|---|---|
| text→image | `mflux-generate` | FLUX.1-dev (+ uncensored LoRA) | already deployed |
| img2img | `mflux-generate --image --image-strength` | FLUX.1-dev (same) | already deployed |
| inpainting | `mflux-generate-fill --image-path --masked-image-path` | FLUX.1-Fill-dev | ~34 GB |
| multi-reference | `mflux-generate-redux --redux-image-paths --redux-image-strengths` | FLUX.1-Redux-dev | ~1.1 GB |

The current [mflux-sidecar.py](deploy/mflux-sidecar.py) wraps only the first two:
`POST /generate` (text→image) and `POST /edit` (img2img via `init_image` +
`strength`). It is a single-flight HTTP server that spawns `mflux-generate` as a
fresh subprocess per request (writing base64 inputs to a temp dir, invoking the
CLI, reading back the PNG). Mask and multi-reference are **not** exposed — the
gateway even rejects a `Mask` field today ("the mflux sidecar has no mask
support").

**Decisions locked in this spec:**

- **Stay on FLUX.1-dev** and add the *Fill* and *Redux* checkpoints as adapters —
  do **not** migrate to FLUX.2 (which would lose the uncensored LoRA and rewrite
  the whole image stack).
- **Full edit = img2img + inpainting**, both in scope.

## 3. Anti-drift rules (inherited from `2026-10-04-img-gen-design.md` §3)

1. Go stdlib (`net/http` + `encoding/json`); no framework, no ORM.
2. One static binary; UI embedded via `//go:embed`.
3. **Minimise SSD writes** — hot state in RAM; durable artifacts written once,
   at completion.
4. No speculative infrastructure.
5. Complexity must earn its existence.

## 4. Architecture (updated)

```
Browser (vanilla JS SPA, embedded)
   │  HTTP + SSE (progress)
   ▼
Go backend (img-gen)  ──►  mflux sidecar (:8899)  ──►  mflux-generate[-fill|-redux]
   │  data/ (durable: images/ + history.jsonl)              │
   └──►  lattice chat (:8080) for prompt enhancement          └─ temp files (RAM-bounded)
```

The img-gen backend already calls the sidecar **directly** (`IMAGE_URL`), not
through the lattice gateway. This spec keeps that path: the new operations are
new sidecar endpoints, and new `internal/lattice` client methods. The gateway's
parallel `/v1/images/*` endpoints are left as-is (used by the control plane, not
this tool). Routing image traffic through the gateway for its priority-queue and
memory-budgeting is a **future** option, not part of this change.

### Components changed

| Package | Change |
|---|---|
| `deploy/mflux-sidecar.py` | add `POST /fill` and `POST /redux` |
| `internal/lattice` | add `Edit`, `Inpaint`, `Blend` client methods |
| `internal/queue` | accept `mode` + image/mask/strength inputs; branch the worker |
| `internal/storage` | no change to write discipline (input images stay in RAM) |
| `cmd/img-gen` | pass new client methods into the queue `Options` |
| `static/` | mode selector, image upload, mask painter, strength slider, multi-upload |

## 5. The four modes

### 5.1 Generate (unchanged)

Genre → field set → prompt (direct or enhanced) → `mflux-generate`. No image
input. This is today's behaviour, untouched.

### 5.2 Edit (image-to-image)

Source image + prompt (+ optional **strength**) → a new image that keeps the
source's content but applies the prompt. Strength is the denoise strength in
`[0,1]`: **low** (≈0.3) keeps the source almost unchanged; **high** (≈0.8)
departs far from it. Default 0.4 (mflux's own default).

Map: `mflux-generate --image <src> <strength>` — the existing `/edit` endpoint
already does this; the *only* gap is exposing `strength` through the img-gen
client and UI (the sidecar already accepts it).

### 5.3 Inpaint

Source image + a **binary mask** (white = regenerate, black = keep) + prompt →
only the masked region changes; everything else is preserved pixel-for-pixel.
This is the "edit *this* part" feature.

Map: `mflux-generate-fill --image-path <src> --masked-image-path <mask>`.
Guidance is higher than text→image (~30 vs ~3.5); steps 20–30. The mask is a
separate same-sized image the user paints in the UI.

### 5.4 Blend (multi-reference)

Prompt + **N reference images** (1..K) with optional per-reference **weights** →
a new image steered by the references (style, subject, composition). Weight 1.0
= full influence; 0.0 = none.

Map: `mflux-generate-redux --redux-image-paths <img…> --redux-image-strengths
<w…>`. Note: the reference images tend to dominate the prompt, so the weight
sliders are the primary control.

### Prompt source for non-generate modes

Generate derives its prompt from `genre` + `fields`. Edit/Inpaint/Blend take a
**free-text prompt** entered directly (and optionally run through the existing
LLM-enhance toggle). The `genre`/`fields` machinery is bypassed in these modes.

## 6. Request & data model

### `SubmitRequest` (extended)

```go
type SubmitRequest struct {
    Genre     string            `json:"genre"`             // generate mode only
    Fields    map[string]string `json:"fields"`            // generate mode only
    Size      string            `json:"size"`
    Enhance   bool              `json:"enhance"`

    Mode      string            `json:"mode"`              // "generate"(default)|"edit"|"inpaint"|"blend"
    Prompt    string            `json:"prompt"`            // free-text for non-generate modes
    Image     string            `json:"image"`             // base64: edit/inpaint source
    Mask      string            `json:"mask"`              // base64: inpaint mask
    Strength  float64           `json:"strength"`          // edit denoise strength
    Images    []string          `json:"images"`            // base64: blend references
    Strengths []float64         `json:"strengths"`         // blend per-reference weights
}
```

`Mode` selects the operation; the other new fields are required per mode:

| Mode | Required inputs | Optional |
|---|---|---|
| `generate` | `genre`, `fields`, `size` | `enhance` |
| `edit` | `prompt`, `image`, `size` | `strength`, `enhance` |
| `inpaint` | `prompt`, `image`, `mask`, `size` | `enhance` |
| `blend` | `prompt`, `images` (≥1), `size` | `strengths`, `enhance` |

Images travel as base64 in the JSON body (the Mac-side already establishes this
pattern — bytes ride the request, not a shared filesystem). Sizes are validated
against the same genre-agnostic allow-list; in non-generate modes the size
defaults to the source image's dimensions when the source is a raster image.

### Storage discipline (unchanged principle)

Input images (base64) are held in **RAM only** for the life of the job and are
**not** persisted. The two durable writes per completed job remain:

1. output PNG → `data/images/{job_id}.png`;
2. one history line → `data/history.jsonl`, recording `mode`, prompt, and input
   metadata (e.g. `"refs": 2`) but **not** the base64 bytes — history stays small.

A consequence: an edit/blend job is not reproducible from history alone (its
source images are gone after completion). This matches the existing
"completed generations survive; queued/running don't" stance and keeps SSD
writes bounded.

## 7. Sidecar changes (`mflux-sidecar.py`)

Add two endpoints alongside `/generate` and `/edit`, each following the existing
shape (write base64 to a temp dir, spawn the CLI, read back the PNG):

| Endpoint | Body | CLI |
|---|---|---|
| `POST /fill` | `{prompt, width, height, image: b64, mask: b64, steps?, guidance?, seed?}` | `mflux-generate-fill --image-path --masked-image-path` |
| `POST /redux` | `{prompt, width, height, images: [b64…], strengths: [f…], steps?, seed?}` | `mflux-generate-redux --redux-image-paths --redux-image-strengths` |

New env config:

- `MFLUX_FILL_BIN` / `MFLUX_REDUX_BIN` — CLI paths (default `mflux-generate-fill`
  / `mflux-generate-redux`), so the two new commands are independently
  configurable without touching `BIN`.
- Model flags for fill/redux (quantize, base-model, low-ram) mirror the existing
  `QUANTIZE` / `EXTRA` handling, but must be verified against the installed mflux
  version at implementation time — `mflux-generate-fill` and
  `mflux-generate-redux` do **not** accept the same flag set as `mflux-generate`.

The sidecar stays single-flight (the `threading.Lock` already serialises all four
operations). Because each request spawns a fresh `mflux-*` subprocess that loads
and then releases its checkpoint, at most one checkpoint is loaded at any instant
and **none** is held resident between requests.

## 8. img-gen backend changes

### `internal/lattice`

Three new methods mirroring `Generate`:

```go
func (c *Client) Edit(ctx context.Context, prompt, size string, image []byte, strength float64) ([]byte, error)
func (c *Client) Inpaint(ctx context.Context, prompt, size string, image, mask []byte) ([]byte, error)
func (c *Client) Blend(ctx context.Context, prompt, size string, images [][]byte, strengths []float64) ([]byte, error)
```

Each POSTs to `ImageURL + "/edit|fill|redux"` with base64-encoded inputs and
decodes the returned PNG — the same shape as `Generate` today.

### `internal/queue`

- `Options` gains a single **image op** function (or three), so the worker can
  branch on mode without growing a God-object. Concretely, replace the single
  `Generate func(ctx, prompt, size) ([]byte, error)` with a small
  `ImageOps` struct:

  ```go
  type ImageOps struct {
      Generate func(context.Context, string, string) ([]byte, error)
      Edit     func(context.Context, string, string, []byte, float64) ([]byte, error)
      Inpaint  func(context.Context, string, string, []byte, []byte) ([]byte, error)
      Blend    func(context.Context, string, string, [][]byte, []float64) ([]byte, error)
  }
  ```

- `Submit` validates the mode-specific required inputs (mirroring §6) and stores
  the base64 inputs on the in-RAM job record (they are **not** written to
  `storage.Job`, which is the durable record — a separate in-RAM field holds
  them).
- `run` resolves the prompt (enhance or direct, for the free-text prompt in
  non-generate modes) and dispatches to the matching op.

### `cmd/img-gen`

Wire the new `internal/lattice` methods into the `ImageOps` passed to the queue.
No route changes — `POST /api/jobs` already carries the (now-extended) body.

## 9. Frontend changes (`static/`)

- **Mode selector** — Generate / Edit / Inpaint / Blend, at the top of the form.
- **Generate** — unchanged (genre + fields).
- **Edit** — an image drop-zone/picker (upload, or pick a previously generated
  image), a free-text prompt box, and a **strength slider** (0–1, default 0.4).
- **Inpaint** — the same source picker plus a **mask painter**: a `<canvas>`
  overlay on the source image where the user paints the region to change
  (white = regenerate, black = keep). A brush-size control, eraser, and
  clear/undo. On submit, the canvas is rasterised to a same-sized PNG mask.
- **Blend** — a **multi-image** picker (add/remove/weight each reference), plus
  the free-text prompt. Per-reference weight sliders (0–1, default 1.0).

All modes keep the existing job-progress panel (SSE) and result display. The
uploaded images are read via `FileReader.readAsDataURL`, base64-stripped, and
sent in the JSON body; the mask is produced by `canvas.toDataURL`.

The mask painter is the single largest new UI surface; everything else reuses the
existing form/SSE scaffolding.

## 10. API surface (unchanged routes, extended body)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/jobs` | extended `SubmitRequest` (now carries `mode`, images, mask, strength) |
| (all other routes) | — | unchanged |

No new backend routes are required: the mode is a field on the existing job
submission, and results flow through the existing SSE + image routes.

## 11. Model, disk & memory implications

| Checkpoint | Download | RAM when loaded | Host |
|---|---|---|---|
| FLUX.1-dev (+LoRA, baked 4-bit) | already present | ~12 GB | current 16 GB host |
| FLUX.1-Fill-dev | ~34 GB | comparable to dev (~12 GB 4-bit) | **may exceed the 16 GB host** |
| FLUX.1-Redux-dev | ~1.1 GB | small adapter over dev | fine |

Two risks to plan for explicitly:

1. **Disk** — Fill-dev is a ~34 GB download; Redux is ~1.1 GB. Both are
   one-time.
2. **Memory** — loading Fill-dev alongside the base dev model (if the sidecar
   ever keeps more than one resident) would OOM the 16 GB host. The single-flight
   sidecar loads **one** checkpoint per request (and releases it when the
   subprocess exits), so this is fine today; the risk only materialises if the
   sidecar is later changed to hold models resident. If Fill-dev cannot fit the
   16 GB host even alone, the fill operation routes to the 32 GB M6 the same way
   the image host is already selectable via `IMAGE_URL`.

These are flagged at implementation time (checkpoint sizes are model-repo
values, not this spec's contract), but the single-flight-per-request design means
no new model-resident/swap logic is needed in v1.

## 12. Error handling

- Unknown `mode` → `400`.
- Missing a mode-required input (e.g. `edit` without `image`, `blend` with empty
  `images`) → `400` with a specific message.
- Sidecar/CLI failure (non-zero exit, timeout) → job `failed` with the trimmed
  stderr surfaced, exactly as `Generate` does today.
- `strength`/`strengths` out of `[0,1]` → `400` (or clamp client-side).

## 13. Testing

- **Unit** — `internal/lattice` methods encode the right body and decode the PNG
  (against an `httptest` fake sidecar asserting on `init_image`/`mask`/`images`
  and the `strength` values); `internal/queue` mode validation (missing inputs,
  unknown mode) and dispatch.
- **Integration** — a fake sidecar returning a tiny PNG for `/generate`, `/edit`,
  `/fill`, `/redux` exercises each full `submit → queue → op → save → SSE →
  fetch` path.
- **Sidecar** — a manual smoke run of `mflux-generate-fill` and
  `mflux-generate-redux` against the real checkpoints (small size, e.g. 512²)
  before wiring the endpoints, to confirm the flag set matches the installed
  mflux version.
- **Manual** — one real run per new mode at a small size, including a mask
  painted in the UI.

## 14. Non-goals (this change)

- FLUX.2 / Kontext migration (staying on FLUX.1-dev + adapters).
- ControlNet (structure/depth conditioning) — a later, separate change.
- Routing image traffic through the lattice gateway for priority/memory
  budgeting.
- Persisting uploaded input images (they are ephemeral, per §6).
- Multi-user auth, accounts, sharing (unchanged non-goal).

## 15. Suggested phasing

1. **Edit (img2img) with strength** — smallest: sidecar already supports it;
   only the img-gen client + a strength slider are new. Ships the "edit" ask
   immediately.
2. **Blend (multi-reference)** — add `/redux` + `Blend` client + multi-upload UI
   (~1.1 GB checkpoint).
3. **Inpaint** — add `/fill` + mask painter UI (~34 GB checkpoint), the largest
   UI and disk piece.
