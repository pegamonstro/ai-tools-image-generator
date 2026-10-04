# img-gen

Browser front-end for generating images through the inference-lattice's FLUX
model, using genre-keyed editable field sets and an async single-slot queue.

## Build

    go build ./cmd/img-gen

## Run

    LISTEN=:8081 go run ./cmd/img-gen

Env vars: `LATTICE_FRONTEND_URL` (default `http://127.0.0.1:8080`), `DATA_DIR`
(`./data`), `GENRES_FILE` (`./genres.json`), `IMAGE_MODEL` (`flux-dev`),
`ENHANCE_MODEL`, `ENHANCE_SYSTEM`, `IMAGE_TIMEOUT_S` (`7200`), `LISTEN`
(`:8081`).

## Adding a genre

Edit `genres.json`: each genre has a `label`, `fields` (typed: `text`,
`textarea`, `select`, `number`, `boolean`), a `prompt_template` with `{key}`
placeholders, and allowed `sizes`.
