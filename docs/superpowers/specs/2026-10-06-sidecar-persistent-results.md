# Sidecar persistent results + boot re-attach

Date: 2026-10-06
Status: approved
Follows: `2026-10-06-queue-restart-hardening.md` — this implements that spec's
"out of scope: sidecar-side persistent outputs" item.

## Problem

After the restart-hardening change, a mid-job restart leaves an **honest**
failed record — but the work is still lost whenever the sidecar had actually
received the job: the sidecar's `run_*` helpers write `out.png` into a
`tempfile.TemporaryDirectory("mflux-")` that is deleted the moment the handler
returns, and the PNG exists only as base64 in the HTTP response body. If
img-gen dies mid-wait, the completed (or near-completed) generation is
destroyed by temp-dir cleanup and the only recovery is resubmission.

Note the asymmetry being fixed: mflux itself finishes its work after img-gen
dies (the Popen is not img-gen's child); only the *result* is unreachable.
Persisting it makes recovery free.

## Design

### Sidecar: `LLM-router/deploy/mflux-sidecar.py`

New environment (`results` config, all optional):

- `MFLUX_RESULTS_DIR` — default `~/mflux-outputs`. Created at startup.
- `MFLUX_RESULTS_TTL_HOURS` — default `24`. Sweep deletes files older than this.
- `MFLUX_RESULTS_MAX` — default `300`. Sweep deletes oldest beyond this count.

Generation output path: instead of `tempdir/out.png`, every `run_*` writes its
`--output` to `<RESULTS_DIR>/<gen-id>.png` (gen id from the handler's
`state["id"]`, threaded through `state`; aux inputs — init/mask/image — stay
in the tempdir). The success response body is unchanged.

New endpoint `GET /result/<id>`:

- Auth: same gate as POST (`X-Mflux-Token` when `MFLUX_TOKEN` is set).
- 200 `image/png` (raw bytes, not base64) when the file exists.
- 404 otherwise. `/status` already exposes the running gen id — combined with
  `/result`, a client can distinguish "still running", "finished", and "lost".

Retention sweep (`_sweep_results()`): runs at startup and at the start of each
generation (inside the single-flight slot, so serialized). Age-based TTL
first, then a newest-N cap.

### img-gen: lattice client

- `Progress` returns `(step, total int, genID string, err error)` — the gen id
  piggybacked on the existing 500 ms `/status` poll (no second poll).
- New `Status(ctx, mode) (runningGenID string, err error)` — `""` when idle.
- New `Result(ctx, mode, genID) ([]byte, error)` — `GET /result/<genID>`;
  missing result is the exported sentinel `ErrResultNotFound` (404), distinct
  from transport errors.

### img-gen: queue

- `storage.Job` gains `GenID string json:"gen_id,omitempty"` (plumbing id;
  also added to the OpenAPI Job schema as an optional property). Not shown by
  the frontend.
- `dispatch`: on the first tick where `Progress` surfaces a non-empty gen id
  that differs from the job's current one, set `job.GenID` and append the job
  to history — so a restart from that moment on knows where to look.
- `ImageOps` gains the 1:1 wiring for `Status` and `Result`; `Progress`'s
  signature is extended (test fakes updated).

### img-gen: boot re-attach (replaces "fail everything")

In `restore`, non-terminal records split on `GenID`:

- `GenID == ""` (queued-but-never-dispatched, or interrupted before enhance/
  dispatch learned the id) → failed honestly, as today.
- `GenID != ""` → keep status `generating`, spawn an attach goroutine that:
  1. polls `Result(mode, genID)` every 3 s; 200 → `SaveImage` + `finish done`
     (real image, zero regeneration — the point of the item);
  2. on `ErrResultNotFound`/404, checks `Status`: sidecar running **our** gen
     id → still running, keep polling; else → one immediate retry of `Result`
     (write-then-respond race), then `finish failed` with the restart message;
  3. transport errors → keep retrying for up to 15 min (M6 may still be
     booting), then fail;
  4. while waiting on a still-running sidecar gen, no overall timeout is
     imposed beyond mflux's own `GEN_TIMEOUT` (1 h) — but a belt-and-braces
     cap of 2 h ends the attach with an honest failure;
  5. `Cancel(id)` works as for any generating job: registered cancelled flag +
     `Ops.Cancel`, and the attach loop stops when it sees the job terminal.

Attach needs no per-job context plumbing beyond the registered cancel: the
loop is a goroutine owned by the manager, reading `isCancelled(id)` each tick.

## Out of scope

- Persistent results from the ESRGAN upscale sidecar (seconds-fast op; if its
  `/status` exposes no id, upscale jobs simply keep the fail-honestly path).
- Any fetch-by-id for *live* clients — live jobs already receive bytes from
  the blocking POST; `/result` is a recovery path, not a second delivery.
- Frontend changes.

## Tests (img-gen)

- lattice: result 200 → bytes; 404 → `ErrResultNotFound`; transport error →
  other error (httptest).
- queue: dispatch persists `GenID` + history append once first learned;
  attach path (fake Ops with scripted Status/Result) recovers a finished
  image into the gallery with status done; the 404-after-idle path fails
  honestly with the restart message; cancel during attach terminates the job;
  jobs without gen id still fail at boot as before (existing test).

## Verification

- img-gen: `go test ./...`, `go vet ./...`.
- Sidecar: local smoke of the persistence + `/result` + sweep with a stub
  mflux binary (scratch, not committed), then deploy to M6 (scp + bootout/
  bootstrap per the established sidecar procedure, launchd logs unchanged)
  and live-verify: submit, kill img-gen mid-`generating`, restart img-gen →
  the job re-attaches and lands `done` with the image — the incident from
  2026-10-06, replayed, now recovering instead of failing.
- Note the lattice repo has unrelated in-progress edits (gateway/control) —
  only `deploy/mflux-sidecar.py` is touched there and committed alone.