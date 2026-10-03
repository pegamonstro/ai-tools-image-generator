# img-gen — Design

**Status:** approved-in-principle, pending user review.
**Date:** 2026-10-04

A web front-end for generating images through the local **inference-lattice**'s
FLUX model, using structured, editable placeholder field sets keyed by scene
genre.

---

## 1. Purpose

Give the homelab a "simple but sophisticated" browser tool for image
generation. The user picks a **scene genre** (landscape, portrait, still life,
…), gets a set of labeled, editable fields (background, setting, object, …)
that are pre-filled with placeholders, edits them, and generates an image via
the lattice's FLUX model. A per-run toggle lets the user choose between a
deterministic prompt template and an LLM-assisted ("enhanced") prompt.

## 2. Context — the lattice API

The lattice (already built, in `/Users/<you>/Projects/LLM-router`) exposes an
OpenAI-compatible HTTP API. Relevant endpoints:

| Endpoint | Body | Response |
|---|---|---|
| `POST /v1/images/generations` | `{model:"flux-dev", prompt, size, n, response_format:"b64_json"}` | `{"data":[{"b64_json":...}]}` |
| `POST /v1/images/edits` | same + `image` (b64) | `{"data":[{"b64_json":...}]}` |
| `POST /v1/chat/completions` | OpenAI chat envelope | text |

Constraints that shape this design:

- **Slow.** A 1024² generation can take ~1 hour; smaller sizes are faster.
- **Single-slot.** The FLUX gateway serves one generation at a time.
- **`LATTICE_FRONTEND_URL`** default `http://127.0.0.1:8080`.

Consequence: the tool must be an **async job queue**, never a synchronous
request/response.

## 3. Anti-drift rules (inherited from lattice §6, adapted)

1. Go stdlib (`net/http` + `encoding/json`); no framework, no ORM.
2. One static binary; the UI is embedded via `//go:embed`.
3. **Minimise SSD writes** — hot state lives in RAM; durable artifacts are
   written once, at completion. (Lattice rule #6: protect SSD health.)
4. No speculative infrastructure (no Redis/Postgres/Kafka for this tool).
5. Complexity must earn its existence.

## 4. Architecture

```
Browser (vanilla JS SPA, embedded)
   │  HTTP + SSE (progress)
   ▼
Go backend  ──►  Lattice (http://127.0.0.1:8080)
   │                 ├── /v1/images/generations   (FLUX)
   │                 └── /v1/chat/completions    (prompt-enhance LLM)
   └── data/          (durable: images/ + history.jsonl)
```

### Components

| Package | Responsibility |
|---|---|
| `main` (`cmd/img-gen`) | HTTP server, routes, SSE; serves embedded `static/` |
| `internal/lattice` | thin HTTP client: `Generate`, `Edit`, `Chat` |
| `internal/genres` | load + validate `genres.json` |
| `internal/prompting` | direct template fill + LLM enhance |
| `internal/queue` | single-slot job worker (goroutine + channel) |
| `internal/storage` | write PNG once; append one history line |
| `static/` | `index.html`, `app.js`, `style.css` |
| `genres.json` | genre → field-set → templates (editable config) |

## 5. Data model — `genres.json` (rich field types)

```json
{
  "version": 1,
  "genres": {
    "landscape": {
      "label": "Landscape",
      "description": "A natural or scenic vista",
      "fields": [
        {"key": "setting", "label": "Setting", "type": "text",
         "placeholder": "alpine valley at dusk", "required": true,
         "hint": "Where the scene happens"},
        {"key": "time_of_day", "label": "Time of day", "type": "select",
         "options": ["dawn", "midday", "golden hour", "dusk", "night"],
         "default": "golden hour"},
        {"key": "weather", "label": "Weather", "type": "select",
         "options": ["clear", "misty", "overcast", "rain", "snow"], "default": "clear"},
        {"key": "season", "label": "Season", "type": "select",
         "options": ["spring", "summer", "autumn", "winter"], "default": "autumn"},
        {"key": "object", "label": "Focal point", "type": "text",
         "placeholder": "a lone pine", "required": true},
        {"key": "mood", "label": "Mood", "type": "textarea",
         "placeholder": "serene, untouched wilderness"},
        {"key": "include_wildlife", "label": "Wildlife", "type": "boolean",
         "true_text": ", with a few birds in the distance", "false_text": ""}
      ],
      "prompt_template": "A landscape image: {setting}, {time_of_day}, {weather} weather, {season}, focal point {object}, mood {mood}{include_wildlife}.",
      "sizes": ["1024x576", "768x512", "512x512"]
    },
    "portrait": {
      "label": "Portrait",
      "fields": [
        {"key": "subject", "label": "Subject", "type": "text",
         "placeholder": "an elderly fisherman", "required": true},
        {"key": "age", "label": "Age", "type": "number",
         "min": 1, "max": 110, "default": 60, "hint": "Approximate age in years"},
        {"key": "expression", "label": "Expression", "type": "textarea",
         "placeholder": "calm, weathered"},
        {"key": "lighting", "label": "Lighting", "type": "select",
         "options": ["soft window light", "golden hour", "harsh overhead", "candlelit"],
         "default": "soft window light"},
        {"key": "setting", "label": "Setting", "type": "text",
         "placeholder": "wooden boat interior"}
      ],
      "prompt_template": "A portrait of {subject}, age {age}, {expression} expression, {lighting} lighting, in {setting}.",
      "sizes": ["512x768", "1024x1536", "512x512"]
    },
    "product": {
      "label": "Product shot",
      "fields": [
        {"key": "product", "label": "Product", "type": "text",
         "placeholder": "a ceramic coffee mug", "required": true},
        {"key": "material", "label": "Material", "type": "select",
         "options": ["matte ceramic", "glossy metal", "wood", "glass", "fabric"],
         "default": "matte ceramic"},
        {"key": "backdrop", "label": "Backdrop", "type": "select",
         "options": ["plain white", "dark gradient", "warm wood table", "concrete"],
         "default": "plain white"},
        {"key": "lighting", "label": "Lighting", "type": "select",
         "options": ["soft studio", "dramatic rim", "natural window", "ring light"],
         "default": "soft studio"},
        {"key": "camera_angle", "label": "Camera angle (deg)", "type": "number",
         "min": 0, "max": 90, "default": 30, "hint": "0 = eye level, 90 = top-down"},
        {"key": "include_shadow", "label": "Soft shadow", "type": "boolean",
         "true_text": " with a soft contact shadow", "false_text": ""},
        {"key": "notes", "label": "Extra details", "type": "textarea",
         "placeholder": "steam rising from the mug"}
      ],
      "prompt_template": "A product shot of {product}, {material}, on a {backdrop} backdrop, {lighting} lighting, {camera_angle}° angle{include_shadow}. {notes}",
      "sizes": ["512x512", "1024x1024"]
    }
  }
}
```

### Field types (v1)

| `type` | Config | Render | Empty handling |
|---|---|---|---|
| `text` | `placeholder`, `hint` | single-line input | dropped if empty (unless `required`) |
| `textarea` | `placeholder`, `hint` | multi-line input | dropped if empty |
| `select` | `options`, `default` | dropdown | always has a value (default) |
| `number` | `min`, `max`, `default`, `hint` | numeric input | dropped if empty |
| `boolean` | `true_text`, `false_text` (optional) | checkbox/toggle | injects `true_text`/`false_text`, else "" |

All fields may carry `required` (bool) and `hint` (string). A `required` field
left empty is a client-side validation error; non-required empties are dropped
from the template rather than injected as noise.

## 6. Prompt assembly (both paths)

- **Direct (default).** Substitute field values into `prompt_template` via
  `{key}` placeholders. Empty optional fields (and their surrounding
  punctuation) are dropped; a missing required field is a validation error.
- **Enhance (per-run toggle).** POST the structured field set as JSON to
  `/v1/chat/completions` with a configurable system prompt ("You write concise,
  high-quality FLUX image prompts…"); the returned text becomes the final
  prompt. Configurable `ENHANCE_MODEL` (default a local/cloud model per
  lattice policy).

## 7. Job lifecycle (single-slot queue)

```
queued ─► enhancing (optional) ─► generating ─► done ─► image saved + shown
                └──────────────────► failed (retryable)
```

- `POST /api/jobs` returns `{job_id}` immediately; the job is queued, never
  blocking.
- One worker goroutine owns the single FLUX slot; a channel serializes jobs in
  submission order.
- The lattice `generations` call blocks (up to ~1 h), so the worker runs it
  with a long timeout; the frontend receives live status over **SSE**
  (`GET /api/jobs/{id}/events`).

### Statuses & progress

The lattice blocks without granular progress, so stages are coarse:
`queued` → `enhancing` (only on the enhance path) → `generating` → `done` /
`failed`. The UI shows the stage plus elapsed time (and, if the lattice ever
exposes it, an estimate).

## 8. Storage & SSD-wear minimisation

**Goal:** impose no harmful write wear on the host SSD. Every write that need
not survive a restart stays in RAM; durable writes are bounded to **two per
completed job** — and since one generation takes minutes to an hour, the write
rate is negligible (far below consumer-SSD endurance). No swap, no rewrite
loops, no temp-file churn.

- **Hot state (RAM only).** Job records live in an in-process, mutex-protected
  store. Status transitions write nothing to disk.
- **Durable artifacts (two writes per completed job, at completion):**
  1. PNG → `data/images/{job_id}.png` (single sequential write).
  2. One JSON line → `data/history.jsonl` (single append) with the full job
     record: `job_id`, genre, prompt, params, size, timestamps, status, image
     path.

No scratch files are needed — the base64 payload is held in memory and written
once. (A literal RAM disk is not required on macOS; if large intermediate
buffers ever demand one, it's a drop-in `hdiutil`/tmpfs mount behind
`storage`.)

## 9. API surface

| Method | Path | Purpose |
|---|---|---|
| `GET`  | `/` | embedded SPA |
| `GET`  | `/api/genres` | `genres.json` (field sets, so the UI renders forms) |
| `POST` | `/api/jobs` | `{genre, fields:{key:value}, size, enhance:bool}` → `{job_id}` |
| `GET`  | `/api/jobs` | jobs — merged view of active (in-memory) + completed (`history.jsonl`) |
| `GET`  | `/api/jobs/{id}` | job status |
| `GET`  | `/api/jobs/{id}/events` | SSE stream of status changes |
| `GET`  | `/api/images/{job_id}.png` | served result image |

## 10. Configuration (env)

| Var | Default | Purpose |
|---|---|---|
| `LATTICE_FRONTEND_URL` | `http://127.0.0.1:8080` | lattice base URL |
| `DATA_DIR` | `./data` | images + history |
| `GENRES_FILE` | `./genres.json` | field-set config |
| `ENHANCE_MODEL` | (lattice default) | LLM for prompt enhancement |
| `IMAGE_TIMEOUT_S` | `7200` | HTTP timeout for a generation |
| `LISTEN` | `:8081` | web server bind (avoid clashing with lattice on 8080) |

## 11. Error handling

- Lattice unreachable / timeout / non-200 → job `failed` with the message
  surfaced to the user; the job is retryable.
- Invalid `genres.json` → fail fast at startup with a clear error (never serve
  a half-broken UI).
- Unknown genre / missing required field / bad size → `400` with a message.
- The queue is in-memory: a process restart abandons queued/running jobs but
  `history.jsonl` remains intact, so completed generations survive.

## 12. Testing

- **Unit** — `prompting` direct fill (incl. empty-field dropping) and enhance
  (with a mocked `Chat`); `genres` load + validation; `storage` write-once
  behavior.
- **Integration** — a mock lattice (`httptest` server faking
  `/v1/images/generations` returning a tiny PNG, and `/v1/chat/completions`)
  exercises the full `submit → queue → generate → save → SSE → fetch` flow.
- **Manual** — a real run against the lattice at a small size (e.g. 512×512).

## 13. Non-goals (v1)

- UI-driven editing of genres/fields (config file only).
- Image edit (`/v1/images/edits`) UI — the lattice supports it; wire it later.
- Multi-user auth, accounts, sharing.
- Batch/parallel generation (the single slot is a hard constraint).
