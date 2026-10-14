# img-gen

Browser front-end for generating images through the inference-lattice's FLUX
model (via the local mflux sidecar) and optional LLM prompt enhancement, using
genre-keyed editable field sets and an async single-slot queue.

## Build

    go build ./cmd/img-gen

## Run

    go run ./cmd/img-gen

Image generation goes through the lattice like every other inference: generate
and edit are submitted to the lattice frontend's OpenAI Images routes, and the
lattice routes each request by the model's registry name (the catalog `key`,
e.g. `persephone` or `sdxl-base`) to the host pinned to serve it. Prompt
enhancement talks to the lattice frontend's chat endpoint. The direct sidecar
URLs remain the fallback route and the transport for the remaining modes.

Env vars: `IMAGE_ROUTING` (`lattice`, default) — generate/edit go through the
lattice (`LATTICE_FRONTEND_URL`); set `direct` to restore legacy direct-sidecar
calls, in which case the lattice is only used for chat enhancement;
`LATTICE_FRONTEND_URL` (default `http://127.0.0.1:8080`) — the lattice frontend
the lattice-routed generate/edit and chat enhancement go through; `IMAGE_URL`
(default `http://127.0.0.1:8899`) — the mflux sidecar (direct mode, plus
pose/blend/upscale which stay direct); `FILL_URL` and
`REDUX_URL` — sidecar endpoints for inpaint/outpaint when served separately;
`UPSCALE_URL` — the Real-ESRGAN sidecar; `RIFE_URL` — the optional
Practical-RIFE sidecar used for keyframes GIF interpolation (see
"Keyframes interpolation" below); `SDXL_URL` — the optional second
image engine (a
[stable-diffusion.cpp](https://github.com/leejet/stable-diffusion.cpp) sidecar
that speaks the same body contract, used only by models whose catalog entry
carries `"sidecar": "sdxl"`); `IMG_GEN_TOKEN` — require a bearer
token on `/api/*` (see "Agent API" below); `DATA_DIR` (`./data`), `GENRES_FILE`
(`./genres.json`), `MODELS_FILE` (`./models.json`), `PRESETS_FILE`
(`./presets.json`), `EXPORT_DIR`, `ENHANCE_MODEL`
(`huihui_ai/dolphin3-abliterated:latest`), `ENHANCE_SYSTEM`, `IMAGE_TIMEOUT_S`
(`7200`), `LISTEN` (`:8099`).

On the lattice path the submitted catalog key is the lattice model name, so the
serving host's lattice registry must pin each catalog key to a checkpoint
(`persephone`, `fluxedup`, `dev`, plus `sdxl-base`/`sdxl-pony`/
`sdxl-illustrious`); LoRA values and sampling knobs ride the request through to
the engine. A raw model value (advanced override) is sent as-is and only
generates if the serving host pins that exact name.

## Modes

The composer drives the mflux sidecar in several modes:

- **Generate** — text-to-image from the genre template.
- **Edit** — img2img on an existing generation (from the History gallery).
- **Inpaint** — mask a region and regenerate what's inside (`FILL_URL`;
  FLUX.1-Fill-dev).
- **Outpaint** — extend an image beyond its frame with an FLUX Redux encoder
  (`REDUX_URL`).
- **Blend** — compose from multiple reference images.
- **Upscale** — enhance a finished image with Real-ESRGAN (`UPSCALE_URL`).
- **Pose** — ControlNet-guided generation: submit a pose/edge reference and
  the composition is honored in the output.

## Adding a genre

Edit `genres.json`: each genre has a `label`, `fields` (typed: `text`,
`textarea`, `select`, `number`, `boolean`), a `prompt_template` with `{key}`
placeholders, and allowed `sizes`.

## Model & LoRA selection

Per-image model and LoRA selection is a pass-through: the UI lets you pick a
model and a LoRA (plus an optional "advanced" raw override for a local path or
HuggingFace id), and img-gen forwards them on the `/generate`/`/edit` request to
the mflux sidecar, which passes them to `mflux-generate`. Selecting nothing uses
the sidecar's configured defaults.

The pickers are populated from `models.json` (served at `GET /api/models`):

    cp models.json.example models.json

Each entry has a `key` (unused by the runtime, kept for clarity), a `label`
(shown in the picker), and a `value` — an mflux `--model`/`--lora` value, either
a local path or a HuggingFace id. Model entries may also carry an optional
`"sidecar"` engine selection: `"sdxl"` routes that model to the
stable-diffusion.cpp sidecar (`SDXL_URL`) instead of mflux, falling back to the
default sidecar when `SDXL_URL` is unset. The engine is derived from the
catalog at submit time — requests never supply it directly — and only the
`generate` and `edit` modes are accepted on the sdxl engine (other modes get a
400 `validation_error`). LoRA applies to generate/edit only (the
fill/redux CLIs hardcode their own model config).

## History gallery

Completed generations appear in the History gallery. Each card shows its
model/LoRA/style metadata and offers **View** (a detail modal with full prompt
metadata) plus, for images that produced a file, **Edit** (load the image into
img2img), **Download** (`GET /api/images/{id}.png?download=1`), and **Save to
folder** (`POST /api/export`, which copies the PNG to `EXPORT_DIR`).

## Agent API (v1) & MCP

Agents and tools should build against `/api/v1/*` — the versioned contract with
typed error envelopes, `Idempotency-Key` support, and a sync generate-and-wait
endpoint. The full surface is specified in `docs/openapi.yaml`; the legacy
unversioned `/api/*` endpoints remain byte-identical for the bundled web UI.

Idempotency: sending an `Idempotency-Key` header on the v1 job submit
(`POST /api/v1/jobs`) returns the original response for every repeat of that
key. The asset-set and GIF submit endpoints (`POST /api/v1/asset-sets`,
`POST /api/v1/gifs`) are not keyed — a repeat there creates new work. The
mapping persists to `DATA_DIR/idempotency.jsonl` — each completed submit
appends `{"key","status","body"}` with the response body base64-encoded — and
on boot the server replays the most recent ≤10k records, so a retry with a key
whose submit completed before a restart returns the same response it would
have returned before. Past 20k lines the file is compacted to that 10k replay
tail at boot. In-flight keys are deliberately not persisted (a restart killed
their jobs, so their reservations are dead); a torn final line from a crash is
tolerated and skipped.

Auth: set `IMG_GEN_TOKEN` on the img-gen process to require
`Authorization: Bearer <IMG_GEN_TOKEN>` on every `/api/*` call (the web UI's
static assets are never authenticated). Unset → the API is open, which is the
right default for a loopback-only deployment.

The MCP server (`img-gen-mcp`) exposes the v1 surface as tools over stdio
(JSON-RPC 2.0, one message per line), so agents that speak MCP — Claude Code,
Hermes, DeepSeek Harness, and our own tools — can drive img-gen directly.

Build it:

    go build ./cmd/img-gen-mcp

It connects to img-gen through two env vars: `IMG_GEN_URL` (default
`http://127.0.0.1:8099`) and `IMG_GEN_TOKEN` (must match the server's), then
speaks plain JSON-Lines on stdin/stdout.

Register it in Claude Code's `mcpServers` config (e.g.
`~/.claude.json`), or point any MCP-stdio client at the binary the same way:

    {
      "mcpServers": {
        "img-gen": {
          "command": "/abs/path/to/bin/img-gen-mcp",
          "env": {
            "IMG_GEN_URL": "http://127.0.0.1:8099",
            "IMG_GEN_TOKEN": "<same value as the server's IMG_GEN_TOKEN>"
          }
        }
      }
    }

Tools: `list_catalogs`, `submit_job(request)`, `get_job(job_id)`,
`list_jobs(limit=20)`, `generate_and_wait(request, wait_seconds=900)`,
`cancel_job(job_id)`, `get_image(id)` (returns base64 PNG image content).
`generate_and_wait` is the workhorse for one-shot agent usage; use
`submit_job` + `get_job` when your harness prefers polling. REST-level failures
surface as `isError` tool results containing the v1 error envelope.

### Agent assets & animated GIFs

img-gen doubles as an asset factory for coding agents. Two workflows:

- **Asset sets** — `POST /api/v1/asset-sets` with 1–8 assets of kind
  `element` (one image, optional `transparent` double-pass), `kit`
  (a fixed 20-class wang tileset: h/v lines, outer/inner corners, T's,
  cross, caps, single), or `atlas` (2–64 frame sheet + TexturePacker-style
  JSON), or `frame` (a nine-slice border PNG: `frame_margins`
  1..size/2−1 punches the interior transparent, one sidecar
  `<name>.frame.json`), or **`bar`** (track + fill pair: `bar_fill`
  prompt renders the fill; `fill_states` 1..100, ≤16 ascending, default
  25/50/75/100 render as horizontal clips `<name>-fill-<NNN>.png`; the
  track interior punches transparent via `frame_margins`; sidecar
  `<name>.bar.json`), or **`npc`** (`shape: "variants"` packs per-variant
  sprites — every variant renders with the SAME set seed and a `", "` +
  suffix prompt — into `<name>-sheet.png` + atlas JSON; `shape: "walk4"`
  renders three direction views (down/up/right suffixes fixed, left
  mirrored from right) into a 4-row RPG-Maker-order sheet
  `<name>-walk.png` with individual cells. Standing views, not walk
  animation). Every asset needs a prompt and a 16-multiple `WxH` size. A
  `manifest.json` lands in the set with per-asset files and errors (per-asset
  seeds ride the child jobs). Setting `"palette_lock": true` builds one
  shared palette (up to 255 colors) over every composed image of the set and
  remaps each pixel to its CIEDE2000-nearest palette color — batch coherence
  is won in post, not generation. The set ships `<name>.palette.json` with
  the colors (`{"colors":["#0f766e",...]}`), an ordinary set file delivered
  flat like the rest. GIFs composed from a locked set's deliverable files (any source with `{set, file}` refs, whether composite sources or `from_frames` refs) inherit the coherence;
  `from_frames` refs by job id read the raw pre-remap renders instead.
- **GIFs** — `POST /api/v1/gifs` with `source.type` `from_frames`
  (assemble existing job ids, data-dir paths, or inline base64),
  `from_spec` (img-gen generates the 2–64 frames at `seed+i`),
  `composite` (one animated base of 2–64 frames plus up to 8 static
  layers, each `{from: <frame ref>, pos: [x,y]}`, alpha-composited
  pre-quantization — statics from already-rendered sets cost zero renders;
  frame refs accept `{job}`, `{path}`, `{b64}`, or `{set, file}` for set
  files), and `keyframes` (interpolates 2–16 anchor frames on the RIFE
  sidecar — see "Keyframes interpolation" below). `delay_ms` 10–5000
  (default 100),
  `loop` >= 0, optional `formats: ["gif","webp"]` (`webp` is a
  best-effort extra — gif2webp on PATH or the skip is recorded).

Both endpoints accept `wait_seconds`; with `0` (default) they return
`202 {"job_id","set_id"}` immediately. Fetch results via
`GET /api/v1/sets/{set_id}` (job + file list) and
`GET /api/v1/sets/{set_id}/files/{name}` (bytes, content-typed; `?download=1`
for a download header). `files[].primary` marks the asset the job points
at. Sets live under `DATA_DIR/sets/{set_id}/`; `dest` delivers copies
into an agent's project directory — the manifest ships as
`<name>.manifest.json`, so two sets can share one destination (absolute
path required — delivery refuses to overwrite, so retry after clearing
the destination).

#### Keyframes interpolation (RIFE sidecar)

`source.type: "keyframes"` animates smooth motion between 2–16 anchor
frames (`source.keyframes.anchors`, each a frame ref — `{job}`, `{path}`,
`{b64}`, or `{set, file}`) on a Practical-RIFE sidecar pointed at by
`RIFE_URL`. The sidecar returns `(anchors−1) × multiplier + 1` frames —
anchors included, in order — with `multiplier` taking 2..10 (default 3).
The projected frame count is capped at 64: a submit that would exceed the
cap is rejected with a `validation_error` naming the largest multiplier
that fits. Submitting valid anchors when the server has no sidecar
configured (`RIFE_URL` unset) is likewise rejected with a
`validation_error` mentioning `RIFE_URL` — no silent degradation. The
resolved anchors persist in the set as `keyframe-NNNN.png` and a restart
recomposes by re-calling the sidecar (interpolation is stateless, so the
re-call is idempotent).

The sidecar itself is a plain HTTP service speaking one route:

    POST {RIFE_URL}/interpolate
    {"images": ["<base64 png>", ...], "multiplier": <2..10>}

and img-gen decodes the reply `{"frames": ["<base64 png>", ...]}` as the
interpolated frame list. Any service answering that contract works as the
sidecar.

#### Engine metadata sidecars (`meta_formats`)

Game engines read their own metadata formats, not img-gen's contract
files, so every asset in a set can opt into engine-native sidecars with a
per-asset `"meta_formats"` field — a subset of `{phaser, unity, sparrow,
tiled}` (default `[]`: no new files). Unknown format names and duplicates
fail the submit as `validation_error`; kind applicability is checked at
compose time, where a format that does not serve that kind fails the
asset like any compose error (recorded in `manifest.json`, set
partial-success unchanged):

| format | file(s) | applies to |
|---|---|---|
| phaser | `<name>.phaser.json` | atlas, npc |
| unity | `<name>.frames.json` | atlas, npc |
| sparrow | `<name>.sparrow.xml` | atlas, npc |
| tiled | `<name>.tsx` + `<name>-wangset.png` | kit |

Single-texture formats target the kinds that ship one sheet — atlas and
npc (`-sheet.png`, or `-walk.png` for walk4). Kit renders 20 separate
class files with no sheet, so only `tiled` — which assembles and ships
its own wangset sheet — applies to it. The tiled `.tsx` is a Tiled 1.10
tileset whose wangset maps the 20 fixed wang classes onto Tiled-native
8-element edge-color wangids (edge-color only: inner and outer corner
classes share connectivity, so Tiled's autotile may pick either — every
class stays individually selectable in the terrain brush).

Mind the one-letter difference: unity metadata is `<name>.frames.json`,
while the frame-kind nine-slice sidecar is `<name>.frame.json`.

Emitted files are ordinary set deliverables: they appear in
`GET /api/v1/sets/{set_id}`'s `files` list, download individually, and
are delivered to `dest` alongside everything else.

#### PNG tEXt parameters

Rendered PNGs carry their parameters in ASCII `tEXt` chunks — one key per
chunk, sorted by key, spliced in just before the IEND trailer (any
standard tEXt reader shows them, e.g. `exiftool` or `pngcheck -v`).
Render-path images always embed `software` (`img-gen`), `prompt`,
`model`, `size`, `mode`, and `job`, plus `seed`, `negative`, `loras`, and
`routing` when they apply. `routing` is the models.json catalog key the
submitted model resolved by — it appears whenever a catalog key matched,
even under `IMAGE_ROUTING=direct`, so it identifies the model rather than
how the render travelled. Composed asset-set deliverables instead carry
the set identity: `software`, `kind`, `name`, and `set`. Values longer
than 4096 bytes are truncated. Embedding is best-effort: a failure leaves
the PNG unmodified and never fails the render, and GIF/WebP outputs are
not PNG files, so they have no embed path.

#### Kenney-style pack layout

img-gen ships set files flat; a finished set can be laid out into the
Kenney-asset-pack convention for sharing — purely a packaging step, no
API involvement:

- `images/` — individual sprites: element deliverables, kit class tiles,
  bar track/fill/clip states, npc walk4 cells.
- `atlases/` — atlas descriptors: `<name>-atlas.json`, plus engine
  metadata JSON/XML (`<name>.phaser.json`, `<name>.frames.json`,
  `<name>.sparrow.xml`) and the img-gen contract sidecars
  (`<name>.frame.json`, `<name>.bar.json`, `<name>.palette.json`).
- `spritesheets/` — the packed sheet images: `<name>-sheet.png`,
  `<name>-walk.png`, and the tiled `<name>-wangset.png`.
- `tilemaps/` — Tiled data: `<name>.tsx`.

Keep `<name>.manifest.json` alongside — it names the original files, so
the reshuffle is lossless and re-fetchable.

MCP tools: `request_asset_set`, `make_gif`, `read_export`.

Install the packaged agent skills:

```bash
deploy/install-skills.sh
```
