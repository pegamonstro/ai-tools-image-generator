# img-gen

Browser front-end for generating images through the inference-lattice's FLUX
model (via the local mflux sidecar) and optional LLM prompt enhancement, using
genre-keyed editable field sets and an async single-slot queue.

## Build

    go build ./cmd/img-gen

## Run

    go run ./cmd/img-gen

Image generation talks to the mflux sidecar (`IMAGE_URL`), prompt enhancement
talks to the lattice frontend's chat endpoint (`LATTICE_FRONTEND_URL`).

Env vars: `LATTICE_FRONTEND_URL` (default `http://127.0.0.1:8080`), `IMAGE_URL`
(default `http://127.0.0.1:8899`), `DATA_DIR` (`./data`), `GENRES_FILE`
(`./genres.json`), `MODELS_FILE` (`./models.json`), `EXPORT_DIR` (defaults to
`~/Downloads/img-gen`), `ENHANCE_MODEL` (`local-brain`), `ENHANCE_SYSTEM`,
`IMAGE_TIMEOUT_S` (`7200`), `LISTEN` (`:8099`).

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
a local path or a HuggingFace id. LoRA applies to generate/edit only (the
fill/redux CLIs hardcode their own model config).

## History gallery

Completed generations appear in the History gallery. Each card shows its
model/LoRA/style metadata and offers **View** (a detail modal with full prompt
metadata) plus, for images that produced a file, **Edit** (load the image into
img2img), **Download** (`GET /api/images/{id}.png?download=1`), and **Save to
folder** (`POST /api/export`, which copies the PNG to `EXPORT_DIR`).
