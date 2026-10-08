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
`UPSCALE_URL` — the Real-ESRGAN sidecar; `SDXL_URL` — the optional second
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
