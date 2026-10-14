# img-gen capability roadmap, round 2

## Overview

Round 1 (asset + GIF expansion) gave the compose layer mature deliverables
(element, frame, bar, atlas, kit, npc, composite, gif) with palette locking,
wang tiles and restart recovery, proven by three mock sites. This round
completes the remaining value in three sequential phases:

- **Phase A — Finish & consumability**: activate parked seams (RIFE
  keyframes), make existing output consumable by real game engines
  (metadata sidecars), embed generation params in PNGs, persist idempotency
  keys. Mostly pure Go, low risk.
- **Phase B — Control & consistency**: ControlNet + IP-Adapter conditioning
  via the SDXL sidecar, rembg alpha as an alternative to dual-render unmix,
  lattice mask passthrough, character-sheet generation that feeds LoRA
  training. Sidecar-deployment work that reuses Phase A's rife deployment
  template.
- **Phase C — Crisp pixel post-process**: pixel-art repair, an
  outline post-op that quantizes before outlining, and palette-coherent
  atlas frames. Pure Go post-processing.

Phases ship and are planned independently (one implementation plan per
phase); this spec is the umbrella that fixes their boundaries, contracts and
ordering.

**Goals**

1. `source.type: keyframes` works end-to-end and is no longer the codebase's
   only live `not_implemented` path.
2. Every compose deliverable can be dropped directly into Phaser, Unity,
   Sparrow or Tiled without hand-authoring JSON/XML.
3. Generated PNGs carry their generation params in tEXt chunks for
   restorability.
4. Idempotency keys survive a restart.
5. Character-consistent generation is possible: ControlNet pose/depth/canny
   and IP-Adapter identity via the SDXL sidecar.
6. Alpha extraction is possible without the white/black dual render (opt-in
   rembg sidecar).
7. A pixel-art repair post-op fixes grid drift, palette noise and scaling
   artifacts; an outline post-op quantizes before outlining.

**Non-goals** (unchanged from prior rounds)

- True walk-cycle/sequential limb animation beyond mirrored walk4.
- APNG / animated-WebP as *new* delivery formats (webp statics already ship).
- Separate nine-slice corner/edge files; separate bar caps; per-state re-renders.
- npc "party" kind.
- FLUX.2 / Z-Image / Qwen migration.
- VLM auto-captioning (new local-VLM dependency; deferred, reconsider later).
- ESRGAN persistent sidecar results (fail-honest path stays).
- history.jsonl rewrite.

## Scope correction discovered during research

img2img (edit + strength), inpainting (masked edit) and blend (multi-reference
Redux) were specified in 2026-10-05-img-edit-and-multi-reference-design.md and
are already implemented (`internal/queue/queue.go:31-36` — `mode`, `image`,
`mask`, `strength`, `strengths`). This round does **not** rebuild them.
The residual Phase B work is what they do not cover: ControlNet/IP-Adapter,
the rembg alpha alternative, mask transport through lattice, character sheets.

---

# Phase A — Finish & consumability

## A1. RIFE keyframes interpolation

The `keyframes` gif source is rejected at `internal/queue/compose.go:973`
with `not_implemented` ("requires the RIFE sidecar (RIFE_URL); planned for
phase 3"). The prior spec already defined the contract: `RIFE_URL` env with
the same wiring as `UPSCALE_URL` (`cmd/img-gen/main.go:55`,
`com.img-gen.plist.in`, `install-macos.sh`, pi systemd deploy notes).

**Sidecar contract** (Practical-RIFE 4.26, ~4-5 GB VRAM, MIT):

```
POST /interpolate
  { "images": ["<b64 png>", ...], "multiplier": <int> }   // 2..16 anchors
→ { "frames": ["<b64 png>", ...] }                        // (n*k - k + n) frames
```

The sidecar is stateless; one request yields all in-between frames. Frame
count may exceed memory for large requests — anchors are capped so the
interpolated total stays ≤ 64 frames (the existing gif cap), which bounds
both the request and the response.

**img-gen side:**

- `GifSource` gains `Keyframes { Refs []FrameRef; Multiplier int }`.
  - `refs`: 2..16 (existing `FrameRef` resolution, same as `from_frames`).
  - `multiplier`: 2..10, default 3.
  - Validation: `ceil(len(refs)*multiplier)` must be ≤ 64, else
    `validation_error` with a suggestion to raise multiplier ≤ 64/n.
- `RIFE_URL` unset → same validation_error shape as SDXL-pinned tile jobs
  today ("requires the RIFE sidecar (RIFE_URL)"). This replaces the
  `not_implemented` branch entirely; a `not_implemented` code path disappears
  from the codebase.
- Sidecar unreachable/busy/timeout → existing `sidecar_unavailable`,
  `busy`, `timeout` retryable errors (same client semantics as ESRGAN).
- GIF assembly stays unchanged: interpolated frames flow into the existing
  median-cut encoder, `formats: ["gif","webp"]`, delay/loop unchanged.
- Restart recovery: keyframes job's ComposeSpec carries resolved anchor
  images; rebuild re-calls the sidecar (stateless → idempotent).

**Anchors may come from anywhere a frame ref resolves:** delivered sets,
export dir, absolute paths — `FrameRef` is unchanged.

## A2. Engine-consumable metadata sidecars

Today compose emits its own contract files (`atlas.json`, `frame.json`,
`bar.json`, kit deliverables). Engines other than img-gen need their own
manifests. We emit them as *additional* files in the set directory, never
replacing img-gen's contract.

**API:** per-asset opt-in field on compose asset requests:

```json
{ "kind": "atlas", "name": "chips", ..., "meta_formats": ["phaser", "unity", "sparrow"] }
```

- Subset of `{phaser, unity, sparrow, tiled}`; default `[]` (no new files →
  zero behavior change for existing callers).
- Unknown element → `validation_error`.

**File formats** (emitted next to the existing deliverables, included in the
set endpoint's `files` list, downloadable individually):

| format | file | applies to | shape |
|---|---|---|---|
| phaser | `<name>.phaser.json` | atlas, npc | TexturePacker/Phaser 3 JSON-hash: `frames{file:{frame, rotated, trimmed, spriteSourceSize, sourceSize}}`, `meta{app:"img-gen compose", image}` pointing at the single sheet deliverable (`-sheet.png` / `-walk.png`) |
| unity | `<name>.frames.json` | atlas, npc | Unity Sprite Editor grid: array of sprites with frame rect + pivot |
| sparrow | `<name>.sparrow.xml` | atlas, npc | Sparrow/Starling XML: `<SubTexture name x y width height>`; npc walk4 uses the 4 direction frame names |
| tiled | `<name>.tsx` + `<name>-wangset.png` (the assembled sheet it references) | kit | Tiled tileset whose `<wangset>` maps the 20 fixed wang classes to Tiled-native edge-color wangids so terrains/autotiling work out of the box |

Single-texture formats target assets that ship one sheet (atlas, npc): phaser/
unity/sparrow each reference exactly one texture image, while kit renders 20
separate class PNGs with no sheet — only tiled, which assembles and ships its
own wangset sheet, applies to kit. A valid format on a kind it does not serve
is rejected at compose time as that asset's error.

Wang class mapping in the tsx is mechanical: img-gen's 20 fixed classes
already name the corner/edge configuration; the wangset `wangid` values are
derived from class names by the documented table in `internal/compose/wang.go`.

**Kenney-style pack layout** is a packaging concern on top: nothing in the
API changes; the docs page gains a section describing how to lay a finished
set's files into Kenney-convention folders (`images/`, `atlases/`,
`spritesheets/`, `tilemaps/`) for sharing. No code.

## A3. PNG tEXt param embedding

Go's stdlib `image/png` cannot write tEXt chunks, but a PNG chunk is a
simple framing format — img-gen grows a ~60-line pure-stdlib chunk writer.

- Every rendered PNG (generate + compose render path) gets tEXt chunks
  inserted between the last IDAT and IEND:
  - `software: img-gen`
  - `parameters: <single-line A1111-style dump>` — prompt, negative, seed,
    model, size, loras, routing target, and for compose assets: kind, name,
    set id. Long values are truncated at 4096 bytes (spec limit is larger,
    but truncation keeps history.jsonl and PNGs from diverging wildly).
- CRC32 per chunk (PNG framing requires it), validated by re-reading with
  the decoder fixture in tests.
- History (history.jsonl) is untouched — both sources carry the same data
  and the 2026-10-05 design note's "history meanwhile" deferral closes.
- Failure to embed never fails a render: embedding is best-effort, appended
  after the image is delivered. (A malformed chunk can only be added by a
  test bug; insertion is pure byte-splicing on a validated PNG.)

## A4. Idempotency-key persistence

Idempotency mapping is currently process-local (`cmd/img-gen/v1.go:516`).

- Map persists as append-only JSONL at `DATA_DIR/idempotency.jsonl`:
  `{"key": "...", "scope": "jobs"|"asset-sets"|"gifs", "id": "...", "ts": ...}`.
- On boot, replayed (last 10 000 lines scanned) into the in-memory map so a
  retried `Idempotency-Key` after restart returns the same job id.
- Append is synchronous with the map insert (same mutex), fsync-free
  (crash may lose the tail entry — acceptable: same crash lost the job).
- Existing semantics unchanged: in-memory fast path on every request,
  persistence only widens the survivable window.

## Phase A testing

- Unit: keyframes validation matrix (anchor count, multiplier, 64-frame
  cap, RIFE_URL unset); a fake RIFE sidecar (httptest server) drives the
  full keyframes→gif pipeline.
- Golden tests: Phaser JSON, Unity JSON, Sparrow XML, Tiled tsx generated
  from a fixed fake atlas/kit — byte-exact fixtures for stability.
- PNG tEXt: round-trip decode (stdlib) confirms chunks parse and
  `parameters` is recoverable; hand-mangled chunk is rejected by decoder
  (proving CRC correctness).
- Idempotency: restart simulation in test (construct queue, submit, rebuild
  queue with same DATA_DIR, replay returns same id).

---

# Phase B — Control & consistency

Phase A deployed RIFE as a sidecar; B's rembg deployment follows the same
template (env var → health gate → retryable errors). All Phase B engine
features are SDXL-sidecar-family features: stable-diffusion.cpp at
`SDXL_URL` ships ControlNet (`--control-net`, `--control-image`, `--canny`,
`--control-strength`) and IP-Adapter (`--ip-adapter`, `--ip-adapter-image`,
`--ip-adapter-image-strength`) for SD-family models. FLUX-routed requests
get honest `validation_error` ("engine unsupported for control"), never
silently ignored fields.

## B1. ControlNet and IP-Adapter via the SDXL sidecar

**API:** new optional fields on `/api/v1/generate` and job submission
(`JobSpec` grows two structs):

```json
{
  "prompt": "...",
  "control": { "net": "pose" | "depth" | "canny", "image": "<b64>", "strength": 0.7 },
  "ip_adapter": { "image": "<b64>", "strength": 0.6 }
}
```

- `control.image` is the conditioning reference (OpenPose skeleton / depth
  map / photo for canny). `control.strength` 0.1..1.0, default 0.7
  (sd.cpp `--control-strength`).
- `ip_adapter.image` is an identity reference. `ip_adapter.strength`
  0.1..1.0, default 0.6. IP-Adapter-Plus for SDXL is supported upstream.
- Validation: mutually exclusive with `mode: edit|inpaint|blend` (those
  already own init-image semantics); present + non-SDXL routing →
  `validation_error`.
- `control_net` preprocessor flag (`--canny`) is honored for `net: "canny"`;
  pose/depth pass the sidecar ready-made maps (documented in the howto —
  users supply processed maps, img-gen does not run OpenPose).
- Out of scope in this phase: compose kinds are not wired to control or
  ip_adapter (pose-locked npc variants stay a possible follow-on once pose
  maps are routinely available).

**Sidecar path:** SDXL sidecar request payload gains optional
`control_net` / `ip_adapter` objects; the sd.cpp invocation appends the four
flags accordingly (no sd.cpp code changes).

## B2. rembg alpha path

**Sidecar:** rembg with u2netp weights (~170 MB CPU-fast), contract:

```
POST /segment { "image": "<b64 png>" } → { "png": "<b64 png rgba>" }
```

`REMBG_URL` env, wired exactly like `RIFE_URL`.

**img-gen:** compose asset field `transparent: true` gains semantics — it
stays dual-render unmix (white/black siblings, existing
`internal/compose/alpha.go:18`), and a new value `"rembg"` opts into the
U²-Net path. Dual-render stays default because it is deterministic and
exact for generated art; rembg is opt-in for photographic-ish or complex
alpha that unmix mangles.

- `transparent: "rembg"` + `REMBG_URL` unset → `validation_error`.
- Sidecar errors → retryable `sidecar_unavailable`/`busy`/`timeout`.
- LayerDiffuse remains deferred (SDXL-only flag upstream, weaker alpha than
  rembg for sprite work; revisit if rembg proves insufficient).

## B3. Lattice mask/extension passthrough

Render-level compose today requires direct routing (`IMAGE_ROUTING=direct`
or the compose render instance). The lattice transport has no extension
channel for the sidecar fields (`Mask`, `Strength`, `control`, and the
compose extension field — deferred in
agent-assets-gif-design.md:420-421).

**This phase defines the field and ships img-gen side; the lattice-gateway
transport change is a separate cross-repo deliverable.** img-gen:

- Serializes an `extension` object into the lattice generate request
  containing every engine-optional field (mask, strength extras, control,
  ip_adapter) when lattice-routing is active.
- Detects lattice rejection of the field (status 400 with unknown-field
  code) and surfaces `validation_error` explaining that lattice routing
  needs the gateway upgrade — honest failure, no silent drop.
- Until the gateway accepts the field, catalog-routed calls with masks or
  control keep failing with the same message they fail with today;
  direct-routed renders and the compose instance are unaffected.

The gateway-side change (accept + relay `extension` to the sidecar) is out
of this repo; its acceptance test is: with an updated gateway, the
catalog-routed edit/inpaint path produces the same results as direct
routing.

## B4. Character sheets for LoRA training

New compose kind `sheet`: a multi-view character turnaround on one grid.

```json
{ "kind": "sheet", "name": "emberfall-hero",
  "prompt": "...", "views": ["front", "side", "back", "casting"],
  "size": "512x512", "columns": 2 }
```

- `views`: 2..8, `columns` 1..`len(views)` (default 1 row grid like
  npc variants), sizes follow the 16-multiple rule.
- Identity consistency: every cell shares the render's fixed seed (same
  pattern npc variants use — pinned-seed variants), plus whatever control
  tools Phase B provides when the caller supplies pose maps.
- Deliverable: single grid PNG + `sheet.json` (view name per cell rect) —
  the same atlas.json contract, renamed for its different consumers, plus
  `meta_formats` from A2 applies if requested.
- Downstream: sheets are the input for LoRA captioning on M6 (dolphin3
  caption step is manual today; the sheet gives the dataset's canonical
  views). No img-gen LoRA-training coupling in this repo.

## Phase B testing

- Unit: control/ip_adapter/transparent/rembg validation matrices; mutual
  exclusion with edit modes; lattice unknown-field detection.
- Fake sidecars (httptest) for rembg and SDXL-control payloads: the sd.cpp
  flag-string builder is a pure function with table tests.
- Smoke: real renders via the SDXL sidecar at :8902 (a pose-locked element
  and an identity-pinned pair), rembg alpha on an existing emberfall asset
  compared against its dual-render counterpart.
- Cross-repo B3: doc-level contract test of the serialized extension field.

---

# Phase C — Crisp pixel post-process

All Go compose/post-processing; no sidecars.

## C1. Pixel-art repair

`internal/compose/pixelart.go`.

**Pipeline** (following the unfake.js / ArcBrush proven ordering — repair
*before* any outline step):

1. **Grid detection** — autocorrelation over the horizontal/vertical
   gradient to estimate the source cell size (2..64 px). Found cell bounds
   or returns `not a pixel-art grid` to the caller when signal is weak.
2. **Dominant-palette extraction** — top-N colors by occurrence (N ≤ 64),
   merging near-duplicates with the existing CIEDE2000 machinery
   (`internal/compose/remap.go`) — one palette implementation, not two.
3. **Quantize** — snap every pixel to the palette.
4. **Downscale** — integer-ratio downscale (image ÷ cell) with center
   sampling, producing the clean small source.

**Exposure:**

- Standalone: `POST /api/v1/repair {image: b64, cell?: int (2..64, omit to
  auto-detect), colors?: int (1..64, default 32)}` → repaired PNG (sync,
  small images only, size ≤ 1024×1024).
- Compose post-op: `"pixel_fix": true` on `element` and `atlas` frames —
  repair runs *before* alpha unmix, palette lock, or delivery, since
  quantization stabilizes every downstream step.

## C2. Outline post-op (quantize-before-outline)

New compose post-op: `"outline": { "width": 1..2, "color": "auto" | "#hex" }`
on element-like assets. Ordering is the point of the feature: quantize →
then outline, so outline strokes follow palette-cleaned edges (the ArcBrush
rule) — outline is computed on the quantized image, never the raw render.
`auto` picks the darkest palette color; explicit hex overrides.

## C3. Palette-coherent atlas frames

When `palette_lock` is set on a set containing an atlas, every frame is
remapped against the set's locked palette *before* assembly (today remap
runs per-render). The assembled strip then shares one exact palette —
relevant for median-cut GIFs and sprite flicker. Mechanism: the set-level
palette computed at render 0 of the set is applied to each frame's pre-
assembly buffer; no new palette math.

## Phase C testing

- Unit: grid-detection on synthetic drifted sprites (known cell size →
  recovered within ±1px), palette N-cap and CIEDE2000 merge behavior,
  quantize→outline ordering (outline pixels must equal a quantized-input
  reference), atlas coherent-palette test (all frames share the set
  palette).
- Golden: before/after pairs for one real artifact per repair stage.
- Smoke: repair a genuinely noisy render from the mock sites and compare.

---

# Ordering and dependency rationale

1. **A before B:** RIFE is the sidecar-deployment template (env wiring,
   health gate, retryable semantics, compose integration, plist + systemd
   changes); B2's rembg reuses it verbatim. A's metadata work and PNG tEXt
   are unrelated to B, so they land first without risk.
2. **B before C:** C's post-ops compose with B's outputs (pose-locked
   elements, rembg alphas) — but nothing in C *depends* on B; the ordering
   is preferential, not hard.
3. Each phase is independently shippable and gets its own implementation
   plan; the umbrella spec keeps contracts consistent.

**Compatibility guarantees:** default behavior of every existing endpoint is
unchanged (new fields are opt-in, defaults empty). Prior round's rejected
non-goals stay rejected. The live :8099 instance is untouched by this work;
renders for verification use the :9106 render instance as established.

**Deployment notes:** `RIFE_URL`, `REMBG_URL` join `UPSCALE_URL` wiring in
`com.img-gen.plist.in`, `install-macos.sh`, and the pi systemd deploy notes
(the pattern agent-assets-gif-design.md:427-428 documents).