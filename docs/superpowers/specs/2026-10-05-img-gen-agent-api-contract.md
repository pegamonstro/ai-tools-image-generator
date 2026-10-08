# img-gen agent API contract — /api/v1 + MCP server

**Spec author:** 2026-10-05. Consumers: Hermes agent, Claude Code, DeepSeek Harness, and in-house tools (inference-lattice, codex solbian, solace, bee terminal).

## Decisions (rulings)

1. **REST stays the core; MCP is a thin sidecar binary** (`cmd/img-gen-mcp`) that wraps REST over stdio. img-gen itself never learns MCP. The MCP binary adds zero third-party deps (hand-rolled JSON-RPC 2.0; the MCP stdio surface needed here is `initialize`, `tools/list`, `tools/call`, `ping`).
2. **Auth:** `Authorization: Bearer <IMG_GEN_TOKEN>` enforced on `/api/*` when `IMG_GEN_TOKEN` is set; unset → open (current localhost deployment default). Static UI (`/`, static assets) is never auth'd. 401 + error envelope.
3. **Versioning:** all new surface under `/api/v1/*`. Legacy unversioned `/api/*` keeps byte-identical behavior (plain-text errors) — existing frontend/tests untouched.
4. **Error envelope (v1 only):** `{"error":{"code":"<machine>","message":"<human>","job_id":"<opt>","retryable":<bool>}}` with HTTP status. Codes: `unauthorized`, `validation_error`, `unknown_genre`, `unknown_style`, `unknown_preset`, `unknown_mode`, `invalid_size`, `job_not_found`, `sidecar_unavailable` (connect/refused/EOF/broken pipe → 503, retryable), `sidecar_busy` (409 → 503, retryable), `sidecar_timeout` (504), `generation_failed` (500), `cancelled`, `bad_idempotency_key`.
5. **Idempotency:** `POST /api/v1/jobs` accepts `Idempotency-Key` header; an in-memory map key→response (job_id or job_ids) is keyed per-submit; replaying a key returns the first response (200). Concurrent duplicate key → 409 `bad_idempotency_key`. Map cleared on restart (documented).
6. **generate-and-wait:** `POST /api/v1/generate` with body = `SubmitRequest` fields plus `wait_seconds` (default 0 → returns `202 {job_id}` at once). When > 0: block until terminal status (poll internally ≤ `wait_seconds`) → final Job JSON (200) or `504` envelope `sidecar_timeout`.
7. **Progress for pollers:** drop `json:"-"` on `Step`/`Total` in `storage.Job` so `GET /api/jobs/{id}` (both versions) exposes `step`/`total` (`0`,0 when N/A).
8. **OpenAPI 3.1** at `docs/openapi.yaml` (hand-written, v1 surface; served later as a static file is fine to skip).

## V1 endpoint set

| Method/Path | Notes |
|---|---|
| `GET /api/v1/catalogs` | `{genres, models, presets}` in one call |
| `GET /api/v1/genres` / `/models` / `/presets` | individual catalogs (same JSON as legacy) |
| `POST /api/v1/jobs` | `SubmitRequest`; 201 `{job_id}` or `{job_ids:[...]}` (batch ≤ 8); honors `Idempotency-Key` |
| `GET /api/v1/jobs` | list (live + history) |
| `GET /api/v1/jobs/{id}` | Job incl. `step`/`total` and `gen_id`; resolves ids from previous sessions via history (2026-10-06: finished ids keep resolving after a restart; mid-flight generations recover through the sidecar's persisted result — see `2026-10-06-queue-restart-hardening.md` and `2026-10-06-sidecar-persistent-results.md`) |
| `GET /api/v1/jobs/{id}/events` | SSE unchanged semantics (auth applies) |
| `POST /api/v1/jobs/{id}/cancel` | 202 |
| `POST /api/v1/generate` | sync convenience (ruling 6) |
| `GET /api/v1/images/{id}.png` | `?download=1` |
| `POST /api/v1/export` | unchanged body |

`mode` ∈ {generate, edit, inpaint, outpaint, blend, upscale, pose}; statuses {queued, enhancing, generating, done, failed, cancelled}. The engine is catalog-derived (`models.json` model entries may carry `"sidecar": "sdxl"`); submitting a mode the selected engine doesn't support (sdxl: generate/edit only) fails at submit with `validation_error` (`mode <x> is not supported on engine sdxl`).

## MCP server: `cmd/img-gen-mcp`

- stdio JSON-RPC 2.0; reads `Content-Length`-less newline-delimited JSON (MCP stdio framing = **one JSON message per line**) — implement per current MCP stdio spec: JSON-Lines.
- `initialize` → `{protocolVersion:"2025-03-26", capabilities:{tools:{}}, serverInfo:{name:"img-gen-mcp", version:"1.0.0"}}`.
- Env: `IMG_GEN_URL` (default `http://127.0.0.1:8099`), `IMG_GEN_TOKEN` (optional; sent as Bearer).
- Tools (JSON-schema'd args, plain text or JSON result content):
  - `list_catalogs()` → catalogs JSON.
  - `submit_job(request)` → job_id(s). `request` mirrors SubmitRequest (validation passes through).
  - `get_job(job_id)` → Job.
  - `list_jobs(limit=20)`.
  - `generate_and_wait(request, wait_seconds=900)` → terminal Job.
  - `cancel_job(job_id)`.
  - `get_image(id, download=false)` → base64 PNG in tool result (image content type if client supports; else text b64).
- No v1 notifications/push; progress via `get_job`.

## Tasks

- [ ] storage: un-hide `step`/`total`; unit test.
- [ ] queue: `SubmitRequest` validation helpers (typed error strings reused by v1 mapping); `Wait(jobID, timeout)` used by generate-and-wait (poll `Get` ≤ timeout).
- [ ] server: v1 routes + auth middleware + envelope writer + idempotency + `/api/v1/generate`; unit/integration tests (auth on/off, envelope codes, idempotent replay, wait timeout path).
- [ ] `cmd/img-gen-mcp`: JSON-Lines stdio MCP; dispatch table; subprocesses hit img-gen over HTTP; tests for schema + dispatch (httptest backend).
- [ ] `docs/openapi.yaml`.
- [ ] README section: running `img-gen-mcp`, agent registration examples (Claude Code mcp config snippet; Hermes/DeepSeek = same stdio binary).

## Acceptance

- [ ] With `IMG_GEN_TOKEN=x`: unauth `/api/v1/jobs` → 401 envelope; `curl -H "Authorization: Bearer x"` → 200/201.
- [ ] Replay with same `Idempotency-Key` ⇒ same job_id.
- [ ] `POST /api/v1/generate {"wait_seconds":600, ...}` returns done Job.
- [ ] Handshake + `tools/list` + a real `generate_and_wait` round-trip against a running img-gen.
- [ ] Legacy `/api/*` responses byte-identical pre/post change (existing tests untouched).