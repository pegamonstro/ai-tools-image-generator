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
(`./genres.json`), `ENHANCE_MODEL` (`local-brain`), `ENHANCE_SYSTEM`,
`IMAGE_TIMEOUT_S` (`7200`), `LISTEN` (`:8099`).

## Adding a genre

Edit `genres.json`: each genre has a `label`, `fields` (typed: `text`,
`textarea`, `select`, `number`, `boolean`), a `prompt_template` with `{key}`
placeholders, and allowed `sizes`.
