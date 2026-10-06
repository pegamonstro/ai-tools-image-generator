# Queue restart-hardening

Date: 2026-10-06
Status: approved (scoped from the 2026-10-06 restart incident)

## Problem

During the fnc strength sweep, a routine `launchctl kickstart -k` of img-gen
destroyed three in-flight jobs:

- Queued jobs (never dispatched) vanished entirely — `GET /api/v1/jobs/<id>`
  returned 404.
- A mid-flight job stayed "generating" nowhere: the manager held it in memory
  only, and after restart the id was unknown.

Root cause: the job queue is memory-only. `history.jsonl` is appended **once**,
at terminal state (`Manager.finish`), and `finish` ignores the persist error.
Any img-gen restart strands live work with no record.

Secondary defect observed at the same time: `LoadHistory` returns records
without id-based dedup, so once a job can be persisted more than once, the
gallery would show duplicates after restart.

## Constraint: no sidecar re-attach

An interrupted M6 sidecar request is unrecoverable in the general case: the
sidecar's synchronous handler dies with its (killed) client, cleans its temp
output dir, and the mflux Popen's result becomes unreachable. Re-attaching to
a survive-the-restart generation would need a sidecar protocol change
(persistent output dir + fetch-by-id). That is out of scope. The honest
recovery for an interrupted job is regeneration: same params (+ same seed, if
the seed was locked) reproduce the image deterministically.

## Design

`history.jsonl` stays append-only. A job may now be appended multiple times
(once per persisted transition); readers dedup by id, last record wins.

Changes, by file:

1. `internal/storage` — `LoadHistory` dedups by job id, last-wins, and returns
   jobs ordered by first appearance (submit-time chronology) carrying the last
   record's content. Doc comment updated. `AppendHistory` unchanged.

2. `internal/queue.submitOne` — after the job is built and stored in memory
   (status `"queued"`), append it to history **before** enqueueing on the work
   channel. Order matters: persist first, then dispatch — a crash between the
   two leaves a record that boot reconciliation will resolve, not a ghost.

3. `internal/queue.New` — restore pass:
   - `LoadHistory`; on error (missing file or an unreadable line), start with
     an empty manager as today (do not fail startup over history).
   - Seed `m.jobs`/`m.order` from the returned records chronologically, so
     `Get`, `List`, `Cancel`, `Wait`, and `Subscribe` all see the same universe
     after a restart. `List`'s existing "history jobs not in memory" merge
     becomes a fallback (no-op in the normal case).
   - Any record whose status is non-terminal (`queued`, `enhancing`,
     `generating`) is marked terminal-failed: `Status = "failed"`, `FinishedAt`
     set, `Step/Total` zeroed (matching `finish`), `Error` set to a clear,
     hostname-free message pointing at regeneration, e.g.
     `"interrupted by a service restart before completion; submit the job again (same seed and params reproduce the image)"`.
     The corrected record is appended to history. Terminal records are seeded
     untouched.

4. `internal/queue.finish` — stop discarding the `AppendHistory` error:
   log a one-line server-side error (`log.Printf`) instead of `_ =`.
   The job's in-memory terminal state and event are unaffected.

## Behavior after restart

- Finished jobs: appear in the gallery as before; `GET` by id now also works
  (previously 404). Improvement, not a regression.
- In-flight jobs: an honest `failed` record with the restart message; the
  frontend pollers get a terminal status instead of a 404 or a hang.
- Cancel/re-queue of boot-failed jobs: terminal-guarded like any other failed
  job; the user resubmits.

## Out of scope

- Sidecar-side persistent outputs (fetch-by-id after restart).
- Idempotency-key mapping persistence (`/api/v1` replay cache is process-local).
- Rewriting history.jsonl (dedup is in readers; the file keeps every record).

## Tests

`internal/storage`:
- dedup last-wins: two records for one id → one record, content of the second,
  position of the first; distinct ids keep chronological interleaving.
- malformed line still returns an error (unchanged semantics, pinned by test).

Update `TestAppendAndLoadHistory`, which currently pins double-append = 2
records.

`internal/queue`:
- a submitted job exists in history with status `queued` before the worker
  picks it up (Generate blocked), and dedups to the terminal record after
  completion.
- restore: pre-seed a history file with one `generating` and one `done`
  record; after `New`, the generating one is `failed` with the restart error,
  the done one is untouched; both are retrievable via `Get`, and the failed
  one's corrected record is in the file.
- restore with a corrupt history line starts empty without failing.

## Verification

- `go test ./...`, `go vet ./...`.
- Live: restart the service at a slot-idle moment (no regression), then
  (optional, next quiet window) a deliberate restart mid-queue showing gallery
  entries flip to failed-with-restart-message instead of vanishing/404.