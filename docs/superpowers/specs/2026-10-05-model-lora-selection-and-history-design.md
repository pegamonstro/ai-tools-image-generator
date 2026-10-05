# img-gen — Model & LoRA Selection + History Gallery Design

**Status:** draft, pending user review.
**Date:** 2026-10-05
**Supersedes:** nothing — extends `2026-10-04-img-gen-design.md` and
`2026-10-05-img-edit-and-multi-reference-design.md` with two new capabilities:
(1) per-job **model + LoRA selection**, and (2) a **history gallery** with
select / edit / download / save-to-dir.

---

## 1. Purpose

Let the user, from the browser:

1. **Choose which model and which LoRA(s)** each generation uses, rather than
   the sidecar's single baked-at-startup model. This is the difference between
   "the tool generates FLUX images" and "the tool lets me drive whichever
   checkpoint/LoRA I want, per image."
2. **Browse and reuse past images** — select any history image to view it, feed
   it into an edit/inpaint/blend, download it (single or batch), or copy it to a
   folder on the host.

Both are surfaces over machinery that already exists or is a thin extension: the
mflux CLI already accepts arbitrary `--model` and `--lora`, and history images are
already persisted to `data/images/` + `data/history.jsonl`.

---

## 2. Background — what the backend already does

- **Model/LoRA is currently fixed.** `mflux-sidecar.py` reads `MFLUX_MODEL` and
  `MFLUX_LORA` from its LaunchAgent environment once at startup and uses them for
  every request. `lattice.Client.Generate`/`Edit` build a body with only
  `prompt`/`width`/`height` (+ `init_image`/`strength`). There is no per-request
  model knob anywhere in the chain.
- **History is already persisted.** `storage.Store` writes each PNG to
  `data/images/{id}.png` and one JSON line to `data/history.jsonl`; `main.go`
  serves them at `/api/images/{name}` and lists jobs at `/api/jobs` (in-RAM jobs
  merged with `LoadHistory()`). The gap is *user-facing*: no gallery, no
  "edit this", no download, no save-to-dir.
- **mflux CLI capabilities** (already available, just not exposed):
  - `mflux-generate --model <path|hf-id> --lora <hf-id> <scale>` — text→image and
    img2img; `--model` takes a local baked path *or* a HuggingFace id (which mflux
    downloads on first use), and `--lora` takes an HF id or local path with a
    `--lora-scale`.
  - `mflux-generate-fill` / `mflux-generate-redux` **hardcode** their
    `model_config` (`dev-fill` / `dev-redux`), so their `--model` selects *which
    baked fill/redux weights*, not an arbitrary dev fine-tune, and they take no
    `--lora`.

**Decisions locked in this spec:**

- **Model/LoRA selection is a pass-through.** The img-gen backend resolves a
  curated catalog to concrete `--model` / `--lora` values (or accepts a raw
  value from the "advanced" field) and hands them to the sidecar; the sidecar
  forwards them to the CLI. "Unknown model" is mflux's own error, surfaced as a
  failed job — no model registry logic on the backend.
- **The catalog is local, uncommitted state** (it contains `/Users/<you>/…`
  paths), not a committed file like `genres.json`. See §7.
- **LoRA stacking is designed in from day one** (schema is a list), even though
  the v1 UI offers a single LoRA — so no breaking schema change is needed later.
- **Model/LoRA selection is full for generate/edit; constrained for inpaint/blend**
  (the fill/redux CLIs hardcode their checkpoint family). See §6.5.

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
   │  data/ (images/ + history.jsonl)          ▲
   │  models.json (local catalog)             └─ --model <path|id> --lora <id> <s>
   └──►  lattice chat (:8080) for enhance
```

The only new edges are (a) `models.json` feeding a `/api/models` catalog to the
frontend, and (b) `model`/`loras` riding the existing `/generate`/`/edit` request
bodies through the sidecar to the CLI. No new host, no new routing.

### Components changed

| Package | Change |
|---|---|
| `internal/models` | **new** — load/validate `models.json` catalog (`Models()`, `Loras()`) |
| `internal/storage` | `Job` gains `Model` + `Loras`; add `LoraRef` type; add `CopyImage`/export helper |
| `internal/lattice` | `Generate`/`Edit` gain a `ModelSpec`; bodies carry `model`/`loras` |
| `internal/queue` | `SubmitRequest` + `ImageOps` gain `ModelSpec`; thread into `run` |
| `cmd/img-gen` | load catalog; add `/api/models`, `/api/images/{id}?download=1`, `/api/export` |
| `deploy/mflux-sidecar.py` | `/generate`+`/edit` accept `model`/`loras` overrides |
| `static/` | model + LoRA pickers (curated + advanced); history gallery; reuse/download/save |

## 5. Data model

### `LoraRef` (new, in `internal/storage`)

```go
// LoraRef is one LoRA to apply, in mflux terms: a --lora value (HF id or local
// path) plus its scale.
type LoraRef struct {
    Name  string  `json:"name"`  // HF id or local path
    Scale float64 `json:"scale"` // 0..1 (mflux --lora-scale); default 1.0
}
```

`LoraRef` lives in `storage` so `queue` (which imports `storage`) and `lattice`
(which will import `storage`) share one definition without a cycle.

### `SubmitRequest` (extended, `internal/queue`)

Add two fields to the existing struct (currently at `queue.go:18`):

```go
type SubmitRequest struct {
    // ...existing fields unchanged...

    Model string    `json:"model"` // mflux --model value (local path or HF id); "" = sidecar default
    Loras []storage.LoraRef `json:"loras"` // mflux --lora list; nil/empty = none
}
```

`Model` and `Loras` carry **resolved values** (paths / HF ids), not catalog keys.
The frontend resolves curated keys → values via `/api/models`; the "advanced"
field just puts a raw path/id in directly. The backend does **not** validate
against the catalog (a bad value surfaces as mflux's own non-zero exit → failed
job), which keeps the backend a dumb pass-through and matches §2's decision.

### `storage.Job` (extended)

```go
type Job struct {
    // ...existing fields unchanged...
    Model string          `json:"model,omitempty"`
    Loras []storage.LoraRef `json:"loras,omitempty"`
}
```

Both are persisted to `history.jsonl` so a completed job records *which* model +
LoRAs produced it (reproducibility + the gallery's metadata display). They are
copied into the Job at `Submit` time alongside the existing fields.

## 6. Model & LoRA selection (Phase A)

### 6.1 Catalog shape (`models.json`)

A local JSON file (git-ignored; default `./models.json`, configurable via
`MODELS_FILE` env — mirroring `GENRES_FILE`). It holds curated, human-friendly
entries whose `value` is the concrete `--model`/`--lora` string:

```json
{
  "models": [
    { "key": "persephone", "label": "Persephone 2.0 (NSFW)", "value": "/Users/<you>/mflux-models/persephone-4bit" },
    { "key": "dev",        "label": "FLUX.1-dev",          "value": "/Users/<you>/mflux-models/flux-dev-4bit" },
    { "key": "dev-schnell","label": "FLUX.1-schnell",      "value": "black-forest-labs/FLUX.1-schnell" }
  ],
  "loras": [
    { "key": "uncensored", "label": "Uncensored (dev-trained)", "value": "shauray/flux-uncensored-lora" }
  ]
}
```

- `value` may be a **local path** (a baked 4-bit dir) or an **HF id** (auto-downloaded
  by mflux on first use — no extra work; the "advanced field accepts HF ids"
  requirement is satisfied by this, with disk + first-run latency as the only cost).
- The catalog is **uncommitted** because local paths embed `/Users/<you>/`. A
  `models.json.example` is committed instead; `.gitignore` covers `models.json`.

### 6.2 `internal/models` (new)

Mirrors `internal/genres`:

```go
type Entry struct {
    Key   string `json:"key"`
    Label string `json:"label"`
    Value string `json:"value"`
}

type Catalog struct {
    Models []Entry `json:"models"`
    Loras  []Entry `json:"loras"`
}

func Load(path string) (*Catalog, error)          // missing file → empty catalog (all generation still works on the sidecar default)
func (c *Catalog) Model(key string) (Entry, bool)  // lookup helper (unused for validation; useful for labels)
```

### 6.3 `internal/lattice`

`Generate` and `Edit` gain a `ModelSpec`; `Inpaint`/`Blend` are **unchanged** in v1
(fill/redux take no model/lora — §6.5):

```go
// ModelSpec carries the per-request model + LoRAs to forward to the sidecar.
type ModelSpec struct {
    Model string          // "" → omit (sidecar default)
    Loras []storage.LoraRef
}

func (c *Client) Generate(ctx context.Context, prompt, size string, spec ModelSpec) ([]byte, error)
func (c *Client) Edit(ctx context.Context, prompt, size, imageB64 string, strength float64, spec ModelSpec) ([]byte, error)
```

The bodies become:

```go
// generate
body := map[string]any{"prompt": prompt, "width": w, "height": h}
if spec.Model != "" { body["model"] = spec.Model }
if len(spec.Loras) > 0 { body["loras"] = spec.Loras }  // []{name,scale}

// edit — same plus init_image + strength
```

### 6.4 `internal/queue`

- `ImageOps` gains `ModelSpec` on the two functions that support it:

```go
type ImageOps struct {
    Generate func(ctx context.Context, prompt, size string, spec ModelSpec) ([]byte, error)
    Edit     func(ctx context.Context, prompt, size, imageB64 string, strength float64, spec ModelSpec) ([]byte, error)
    Inpaint  func(ctx context.Context, prompt, imageB64, maskB64 string) ([]byte, error)  // unchanged
    Blend    func(ctx context.Context, prompt, size string, imagesB64 []string, strengths []float64) ([]byte, error) // unchanged
}
```

- `Submit` copies `req.Model` / `req.Loras` onto the `storage.Job` (with a default
  `Scale=1.0` for any ref that omits it, mirroring `normalizeStrengths`).
- `run` builds `spec := ModelSpec{Model: job.Model, Loras: job.Loras}` and passes it
  to `Ops.Generate` / `Ops.Edit`. `Inpaint`/`Blend` calls are unchanged.

### 6.5 fill/redux constraint ("all modes", honestly)

`mflux-generate-fill` / `-redux` hardcode their `model_config` (`dev-fill` /
`dev-redux`), so they accept **only baked fill/redux weights** via `--model` and
take **no `--lora`**. The UI shows a model selector in all four modes for
uniformity, but:

| Mode | Model selector | LoRA selector |
|---|---|---|
| generate | full (any catalog/advanced value) | full |
| edit | full | full |
| inpaint (fill) | baked fill checkpoints only (one today) | none (disabled) |
| blend (redux) | baked redux checkpoints only (one today) | none (disabled) |

This is documented in the UI ("LoRA applies to generate/edit only"). If a second
fill/redux checkpoint is ever baked, it drops into the same selector with no code
change.

### 6.6 Frontend — pickers

- A **Model** dropdown at the top of the form (all modes), populated from
  `/api/models` (`label`, sorted; default = empty "sidecar default").
- A **LoRA** dropdown + **scale** slider (0–1, default 1.0), shown for
  generate/edit, disabled for inpaint/blend. v1 ships one LoRA; the schema already
  supports a list so a future "add another LoRA" row is additive.
- An **"advanced"** toggle/field next to each: a free-text box accepting a local
  path *or* an HF id, which overrides the dropdown (used directly as `model` /
  `loras[0].name`).

## 7. History gallery (Phase B)

### 7.1 Gallery + metadata

- The frontend renders `GET /api/jobs` as a **thumbnail grid** (already returns
  every job with `image_path`, `prompt`, `mode`, `model`, `loras`, `style`, `size`,
  `created_at`). Thumbnails are `<img src="/api/images/{id}.png">`; a "no image"
  tile for failed jobs.
- Clicking a tile opens a **detail view**: full image, prompt, model/LoRA/style,
  size, timestamp, and actions (below).

### 7.2 Reuse as source ("select → edit")

- "**Edit**" / "**Inpaint**" / "**Blend**" on a history tile preloads that image as
  the source (`init_image` / inpaint source / a blend reference). Because the image
  is served same-origin, the frontend fetches `/api/images/{id}.png` → re-encodes
  base64 → sets it as the mode's source, exactly as an upload does today. The
  history `Job.Size` supplies the default size for edit/blend.

### 7.3 Download

- Single-image download is a browser `<a download>` on the same-origin URL (no
  server change required).
- **Server-side download** for robustness/scripting: `/api/images/{id}.png` accepts
  `?download=1`, which sets `Content-Disposition: attachment; filename="{id}.png"`.
- **Batch download**: `POST /api/export` (below) with a `zip` option, or a
  client-side multi-fetch of each `/api/images/{id}.png`.

### 7.4 Save-to-dir (`POST /api/export`)

A new endpoint copies selected history images to a directory on the host (img-gen
runs on the M6, next to the images):

```
POST /api/export   { "ids": ["<16-hex>", …], "dest": "/Users/<you>/Downloads/img-gen" }
→ { "copied": ["<path>", …], "skipped": ["<id>", …] }
```

- `dest` is optional; default `~/Downloads/img-gen` (configurable via
  `EXPORT_DIR` env). `dest` is validated to be an absolute path; it is created if
  missing.
- Implementation is `storage.Store.CopyImage(id, dest)` — a file copy guarded by the
  existing `idRe` (already used by `/api/images/`) so only our own `images/*.png`
  are ever touched (no path traversal). Unknown ids → `skipped`.
- The `zip` variant (if included) streams a zip of the selected PNGs with
  `Content-Disposition: attachment`.

### 7.5 PNG metadata (stretch — deferred)

A1111's "drag the image back to restore its params" depends on embedding the
prompt + params in the PNG `tEXt` chunk. Go stdlib does not write `tEXt`; it needs
manual PNG-chunk assembly. **Deferred** to a later change — `history.jsonl` already
carries the params, so nothing is lost; this only buys portability of the params
*inside* the file. Noted as a non-goal (§12).

## 8. API surface (net changes)

| Method | Path | Purpose | New? |
|---|---|---|---|
| `GET` | `/api/models` | return the catalog (`{models:[], loras:[]}`) | **new** |
| `POST` | `/api/jobs` | extended `SubmitRequest` (now carries `model`, `loras`) | extended |
| `GET` | `/api/images/{id}.png` | serve image; `?download=1` → attachment | extended |
| `POST` | `/api/export` | copy selected images to a host dir (or zip) | **new** |

`/api/jobs/{id}`, `/api/jobs/{id}/events`, `/api/genres`, `/` are unchanged.

## 9. Sidecar changes (`mflux-sidecar.py`)

`run_generation` (serving both `/generate` and `/edit`) reads optional `model`
(string) and `loras` (list of `{name, scale}`) from the body:

```python
cmd = [BIN, "--model", body.get("model") or MODEL, "--output", out]
for ref in body.get("loras") or []:
    cmd += ["--lora", ref["name"], str(ref.get("scale", 1.0))]
# then the existing --prompt / --negative-prompt / --width / --height / --steps / …
```

- `model` defaults to the agent-level `MODEL` env when absent, so the sidecar's
  current behaviour is unchanged for callers that don't send it.
- `run_fill` / `run_redux` are **unchanged** in v1 (they already take `--model
  <baked>` with no lora).
- **Implementation-time verification:** confirm whether the installed mflux
  `mflux-generate` accepts a *repeatable* `--lora` flag for stacking. If it accepts
  only one, v1 passes at most the first ref and stacking is deferred to an mflux
  upgrade (the schema still reserves the list). Flagged, not assumed.

## 10. Error handling

- `/api/models` with a missing `models.json` → `{models:[], loras:[]}` (empty, not
  error) — generation still works on the sidecar default.
- Bad model/lora value → **not** validated by the backend; mflux exits non-zero and
  the job fails with its trimmed stderr (existing path in `run`).
- `/api/export` unknown/non-image ids → listed under `skipped`, not a 500.
- `/api/export` non-absolute or unsafe `dest` → `400` with a message.
- `Scale` outside `[0,1]` → clamp client-side (slider bounds) and server-side
  (`normalize` to 1.0 default; out-of-range clamped in `Submit`).

## 11. Testing

- **Unit**
  - `internal/models`: load a sample `models.json`, missing-file → empty catalog.
  - `internal/lattice`: `Generate`/`Edit` encode `model`/`loras` in the body (against
    an `httptest` fake sidecar asserting the fields); empty spec omits them.
  - `internal/queue`: `Submit` copies `model`/`loras` onto the Job; default
    `Scale=1.0`; `run` passes `ModelSpec` to the op.
  - `internal/storage`: `CopyImage` copies only under `images/` (path-traversal
    attempt → skipped/error).
- **Integration** — a fake sidecar returning a tiny PNG for `/generate` and `/edit`
  exercising the `model`/`loras` fields end-to-end; `/api/models` and `/api/export`
  against a temp `DATA_DIR`.
- **Manual** — one real generation per catalog model (Persephone + dev) at 512²,
  one with a LoRA, one advanced HF id (confirming auto-download), one
  edit-from-history, one export-to-dir.

## 12. Non-goals (this change)

- PNG `tEXt` metadata embedding (§7.5, deferred).
- Multi-LoRA **stacking beyond what mflux supports** (schema reserves it; the
  repeatable-`--lora` question is deferred if mflux doesn't support it).
- A full unified canvas / mask-painter overhaul (the mask painter already exists;
  history-reuse feeds it, nothing new).
- Model **merging**, checkpoint download management UI, or a model registry on the
  backend.
- Multi-user auth / accounts / sharing (unchanged non-goal).

## 13. Suggested phasing

1. **Model/LoRA selection for generate/edit** — sidecar override + `ModelSpec`
   through lattice/queue + catalog + pickers. Highest value, smallest surface.
2. **History gallery + reuse** — grid + detail + "edit/inpaint/blend from history".
   Pure frontend over existing `/api/jobs` + `/api/images`.
3. **Download + save-to-dir** — `?download=1` + `/api/export` + the frontend
   actions.
4. **(stretch)** PNG `tEXt` metadata; multi-LoRA stacking if/when mflux allows it.
