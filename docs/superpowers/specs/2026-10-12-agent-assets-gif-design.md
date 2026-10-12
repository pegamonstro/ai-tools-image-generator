# Agent-facing asset generation & animated GIF design

Date: 2026-10-12
Status: draft for review (revised after verification pass — repo-reality,
completeness, and consistency findings folded in)

## Goal

Two features that turn img-gen from a browser composer into an asset service
for coding agents, while keeping every capability usable from the existing web
UI:

1. **Asset generation bespoken to a project** — agents developing a frontend
   (Claude Code, OpenCode, Hermes, DeepSeek Harness) request image assets
   through img-gen's MCP server: seamless tileable patterns, modular
   line/corner/junction kits, standalone UI elements, and sprite-sheet
   atlases, delivered as files into the agent's project directory.
2. **Animated GIF (and WebP)** — assemble a GIF from provided frames,
   generate a frame set internally, or interpolate between keyframes.

Prior-art basis: verified research sweep (2026-10-12) — see "Prior art" at the
bottom. Decision record: Approach A (composite pipeline in img-gen core) was
chosen over skills-only (B) and a separate asset-engine sidecar (C), because
it keeps the asset intelligence server-side — every harness gets identical,
verified output — while reusing the existing queue, catalog, and lattice
routing.

## Non-goals (v1)

- Web UI for authoring asset sets or GIFs (composite results appear in
  History and are viewable, but no authoring UI yet).
- Alpha-capable generation models (LayerDiffuse for SDXL is a later opt-in;
  v1 alpha uses dual-render unmixing, see below).
- Vector/SVG assets, icon retrieval fallbacks, and 9-patch (.9.png) emission.
- A RIFE deployment; v1 defines the contract and honors `RIFE_URL` when set.
- Idempotent sync generation of composites: `Idempotency-Key` replay stays
  scoped to the async submit endpoints (as it is today on `/api/v1/jobs`);
  the required seed provides reproducibility for the sync variants. Composites
  are expensive enough that agents should prefer async submit + poll.

## Component map

- **`internal/compose`** (new package): deterministic pure-Go post-processing.
  Alpha dual-render unmixing, grid/atlas packing, GIF encoding (`image/gif`)
  with inter-frame transparency optimization, shared palette quantization.
  No new third-party dependencies.
- **Compose coordinator in `internal/queue`** (new; see "Queue coordination"
  below): a non-slotted watcher job type that enqueues child generation jobs
  and finalizes when they all land. It never occupies the single worker slot.
- **v1 endpoints** in `cmd/img-gen/v1.go`: `POST /api/v1/asset-set`,
  `POST /api/v1/gif`, plus `GET /api/v1/sets/{id}` and
  `GET /api/v1/sets/{id}/files/{name}` for multi-file composite retrieval
  (see Storage).
- **docs/openapi.yaml** gain the two new endpoints' paths and schemas, the
  `compose_error` code in the error-code enum, the set-retrieval endpoints,
  and the expanded job record shape (the file is hand-maintained and must
  stay in sync with v1.go per its own header note).
- **MCP tools** in `cmd/img-gen-mcp/main.go`: `request_asset_set`,
  `make_gif`, `read_export` (see MCP section).
- **Packaged skills**: `skills/img-gen-assets/SKILL.md` and
  `skills/img-gen-animation/SKILL.md`, installed by the deploy scripts into
  target projects (see Skills section).

## Queue coordination (compose jobs)

The queue is a single worker goroutine draining a FIFO work channel; a compose
parent must not occupy that slot while its children wait behind it — read
literally, that deadlocks. The compose parent is therefore a **coordinator,
not a work item**:

- Submitting an asset-set/gif-from-spec request persists the parent job
  record (with child job ids) **without pushing it into the work channel**,
  then enqueues each child generation as a normal job.
- The coordinator follows each child's terminal transition (the queue already
  emits them); when all children are terminal it claims a worker slot like any
  other job needs it only briefly — actually simpler and race-free: the **last
  child to reach a terminal state re-enqueues the parent as a compose
  work item**. The compose stage then runs like any other queue stage (short,
  CPU-only compose work), so the parent genuinely goes through the FIFO order
  with everyone else.
- If a child fails (after its normal retries), it too triggers the
  re-enqueue-at-terminal-count rule: the parent compose stage runs, skips the
  failed inputs, and reports partial success (see failure semantics).
- **Cancel**: cancelling the parent sets a cancellation flag that cancels all
  still-pending children through the normal `Ops.Cancel` path, then marks the
  parent cancelled. Cancelling a child only marks that child failed; the
  parent compose then completes with that asset failed.
- **Restore**: the parent's persisted record lists child job ids. On restore,
  a non-terminal parent does not attach-loop against a sidecar (unlike
  generation jobs — it has no GenID): it checks the restored children's
  states. Children restored as terminal re-count; children restored as
  generating will re-trigger the parent via the terminal-transition rule; if
  no children survive restart in non-terminal state but the parent was
  incomplete, the parent is failed with the surviving child ids recorded (the
  same honest-fail pattern the restore code uses elsewhere). Compose work is
  then re-derivable from the children's persisted outputs alone.
- **Latency note**: an 8-asset set with transparent elements fans out to up to
  16 sequential generations (`transparent` doubles per element), blocking all
  single-slot submitters for the duration; set-level progress lands in the
  compose job's events so agents can render progress instead of guessing.

## Storage (composite outputs)

Storage today is exactly one 16-hex-id PNG per job (`Job.ImagePath`; serving
and export hard-code `DATA_DIR/images/{id}.png`). Composites produce multiple
mixed-type files (PNG sheet, GIF, JSON sidecars, named pieces), so the spec
adds a real layout rather than pretending the existing one covers it:

- Composite files live in **`DATA_DIR/sets/{setID}/`** with their real
  sanitized names (`accent.png`, `border-line-h.png`, `spinner-sheet.png`,
  `atlas.json`, `loader.gif`, `manifest.json`).
- `storage.Job` gains `Files []FileRef` (`{name, kind, bytes, primary}`) and
  the child job ids. `ImagePath` keeps pointing at a **primary artifact**
  chosen per kind (tile/element PNG, atlas sheet PNG, composed GIF) so History
  galleries need no change to render the composed PNG — atlas frames and
  alpha side-jobs are ordinary 16-hex-id generation jobs first, so
  `GET /api/v1/images/{id}.png` already serves them.
- New **`GET /api/v1/sets/{id}`** returns the manifest + file list;
  **`GET /api/v1/sets/{id}/files/{name}`** streams any file in that set
  directory (name sanitized, id validated, path pinned to
  `DATA_DIR/sets/{id}/` after cleaning — no traversal). This is the
  generalization `/api/v1/export` cannot provide: `CopyImage` remains
  untouched and PNG-only.
- History cards for composite jobs show the primary artifact; the detail
  panel can list the remaining files via the set endpoint later (UI work is
  out of scope in v1; the endpoints exist from day one).

## Asset-set pipeline

`POST /api/v1/asset-set`

Request:

```jsonc
{
  "assets": [
    // kind is required and must match the enumerated kinds below.
    {
      "kind": "tile",        // seamless tileable pattern (Phase 2; rejected with validation_error in Phase 1)
      "name": "accent",      // filename stem; sanitized
      "prompt": "...",
      "size": "512x512"
    },
    {
      "kind": "kit",         // one asset spec expands into the full wang set
      "name": "border",
      "prompt": "...",       // shared style prompt for ALL pieces
      "size": "256x256"
    },
    {
      "kind": "element",     // standalone UI asset
      "name": "hero-badge",
      "prompt": "...",
      "size": "512x512",
      "transparent": true    // only the element kind honors transparent in v1
    },
    {
      "kind": "atlas",       // MxN grid packed into one PNG + atlas.json
      "name": "spinner",
      "prompt": "...",       // per-frame prompt template with {frame} var
      "frames": 8,
      "columns": 4,
      "size": "256x256"      // per-cell size
    }
  ],
  "generation": {            // defaults shared across the set (v1 SubmitRequest subset)
    "model": "persephone",
    "loras": [{"name": "...", "scale": 1.0}],
    "style": "...",
    "seed": 12345,           // required; per-piece derivation below
    "steps": 30,
    "guidance": 3.5,
    "negative_prompt": "...",
    "enhance": false
  },
  "output": {
    "dest": "/abs/path/project/assets",  // optional; see Delivery
    "name": "acme-brand"                 // optional set name for dirs/manifest
  }
}
```

- `generation` is the SubmitRequest subset above — **plus one new request
  field, `tileable` (Phase 2)**. It is threaded explicitly through
  `SubmitRequest` → `storage.Job` → the sidecar request body as a new field on
  each (the JSON round-trip in place today silently drops unknown fields);
  it is not routed through the lattice transport (see Phase 2).
- **`kind: "kit"` is Phase 1** — it needs only the child-submit path and
  composition, no sidecar work. Any kind not yet active in the shipping
  phase is rejected with `validation_error` naming its phase (the same
  pre-phase contract as `tile`).

Kinds:

- **`tile`** — a single N×N seamless pattern. Requires `generation.tileable`.
- **`kit`** — a baked server-side wang-set spec: straight lines (h, v), inner
  and outer corners, junctions, caps — a fixed enum of tile classes defined in
  `internal/compose`. Each class is a separate generation with per-class seed
  `seed + stable class offset` (the offset table lives in `internal/compose`
  beside the class enum, so manifests reproduce), sharing the style prompt
  plus a per-class compositional hint — the "style-lock": a consistent
  `generation.style` string and identical `generation` params across every
  piece; the skills explain this contract.
- **`element`** — a standalone asset; `transparent: true` triggers the alpha
  matte pipeline (element-kind only in v1).
- **`atlas`** — `${frames}` generations with the `{frame}`-substituted prompt
  template and per-frame seed `seed + frame index` (same derivation style as
  kit), packed into one M×N uniform-grid PNG plus an `atlas.json` sidecar
  (TexturePacker JSON-Hash shape: `frames["name"] = {frame: {x, y, w, h},
  ...}`). Individual frames are retained as their own jobs.

### Child submission (the free-prompt problem)

In mode `generate`, the existing submit path requires a genre whose fields
supply the template and rejects sizes outside the genre's `sizes` list;
free-text prompts only flow through non-generate modes. Asset children are
raw prompt + size, so a new **`asset` internal generation path** is added:
`submitAssetChild(prompt, size, generation…)` reuses `resolveModelValues`,
the dispatch switch, and the lattice/direct transport, but validates `size`
against the **engine's allowed-size list** instead of the genre catalog. The
child jobs are genuine jobs (persisted, restartable, cancel-able, listed in
History alongside normal work).

Validation (`validation_error` envelope):

- Unknown kind; kind not yet active (phase-gated); missing per-kind fields;
  frames in 2..64; columns >= 1 (and columns × height bounded so the sheet
  stays sane); size present and within the engine's allowed sizes.
- **tileable + ControlNet/pose conditioning is rejected** (verified: circular
  padding creates a seam where ControlNet conditioning is active).
- seed required (the kit/atlas/frame derivations all derive from it).
- `dest` (if given) must be an absolute path (see Delivery).

### Alpha matte (`transparent: true`)

Dual-render un-mixing: the child generation **with a solid-background
directive appended to the prompt** is rendered twice on a solid white and a
solid black background (two automatic child jobs, same seed); per-pixel,
C = αW + (1-α)B is solved channel-wise, yielding an RGBA PNG.
Deterministic, zero sidecar changes, both engines.

Honest caveats, documented in the compose package: prompt text alone cannot
guarantee a *pure* #fff/#000 fill — the directive improves it, and any
off-solid background degrades the matte; W/B-similar pixels are
ill-conditioned (flagged α≈indeterminate, fallback to B's value). Quality is
the acceptance bar; the rembg path (U^2-Net auto-matte, opt-in if the tool is
available) and LayerDiffuse-for-SDXL are the later alternatives. The matte
applies to the element's final artifact before any packing.

### Composition and failure semantics

Children go through normal lattice routing and the single-slot queue
(coordinator per "Queue coordination"). The compose stage then:

- Reports partial success, not fatal: per-asset failures mark
  `{index, name, error}`; the set completes if **any** asset succeeded;
  `all_failed` is the only compose-stage failure.
- Writes composite outputs into `DATA_DIR/sets/{setID}/`, marks the parent
  job complete with the `Files` list, and appears in History via the primary
  artifact.
- **Manifest**: `manifest.json` in the set directory — per-asset catalog key,
  resolved seeds, prompts, params, source job ids; GIF jobs additionally
  record frame metadata (frame count, delay, loop, per-frame source refs)
  and the webp outcome (encoded | skipped-with-reason) — so an agent can
  regenerate any variant later via `submit_job` with the recorded values.

## GIF pipeline

`POST /api/v1/gif`

Request:

```jsonc
{
  "source": {
    "type": "from_frames",     // | "from_spec" | "keyframes"
    "frames": [                // tagged refs — unambiguous by construction
      {"job": "<job id>"}, {"path": "<DATA_DIR/sets/-relative path>"}, {"b64": "<payload>"}
    ],                         // frame count bounded 2..64
    // from_spec: {"request": <v1 SubmitRequest subset>, "count": 8, "seed": 123}
    //   -> N generations sharing the SAME prompt (there is no template
    //      substitution in from_spec; identical prompt, VARIED seeds:
    //      seed+i) then composed as frames. count bounded 2..64.
    //      Fixed-seed img2img chaining is an established anti-pattern
    //      (progressive degradation) and is NOT used.
    // keyframes: {"frames": [<same tagged refs>], "interval": 2}
    //   -> interval = interpolated frames generated between each adjacent
    //      keyframe pair (1..8); refs use the same tagged shape as from_frames
  },
  "delay_ms": 100,             // per-frame delay (all frames share it in v1)
  "loop": 0,                   // 0 = infinite; NETSCAPE2.0
  "output": {"dest": "/abs/path", "name": "loader"},
  "formats": ["gif", "webp"]   // default ["gif"]; "webp" encodes via cwebp if installed
}
```

- **Encoding**: `image/gif` — per-frame delay in 1/100s units, disposal
  methods, transparency, NETSCAPE2.0 loop block. **Palette strategy (v1
  contract, not implementation-left): a shared median-cut palette across all
  frames**, built once from the frame set; inter-frame transparency
  optimization (gifx-style) in-process in `internal/compose`. When frames
  quantize poorly (photographic FLUX/SDXL output), the compose stage shells to
  `ffmpeg` two-pass `palettegen`/`paletteuse` when ffmpeg is present on PATH;
  the manifest records which encoder produced the file.
- **Interpolation**: the `keyframes` source requires `RIFE_URL` env (a RIFE
  4.24+ sidecar). Unset → `validation_error` naming the env var; no silent
  fallthrough.
- **WebP**: present in `formats`, via `cwebp` if installed; absent tool → the
  GIF stands and the manifest/response records the webp skip with reason.
- GIFs are composite jobs: stored in the set directory, first-class History
  artifact via `ImagePath`, frame metadata in the manifest.

## MCP layer

Three new tools in `cmd/img-gen-mcp`, same thin-wrapper pattern:

- **`request_asset_set`** → `POST /api/v1/asset-set` (+ `wait_seconds`
  blocking). Returns text (set id, absolute delivered paths, per-asset
  manifest summary) **plus one inline base64 preview image** of the set's
  primary artifact — the verified dominant convention among agent-facing MCP
  servers (inspect inline, deliver as files).
- **`make_gif`** → `POST /api/v1/gif` (+ `wait_seconds`), returns text (path,
  frame count, delay, formats) plus an inline preview of the GIF's first
  frame (MCP image blocks are static).
- **`read_export`** — returns base64 bytes for one file of a **completed
  composite set** (set id + file name, validated inside
  `DATA_DIR/sets/{id}/`) or of an ordinary job image (job id). Read roots are
  only ever `DATA_DIR` trees — the earlier "previously-supplied dest" idea is
  dropped: the server keeps no per-caller dest memory, and agents wanting
  bytes from an already-delivered project file can read that file themselves.
  This keeps the trusted read scope unchanged.

Tool schemas align to the de-facto agent vocabulary (`kind`, `prompt`,
`size`, `seed`, `count`), keeping flat parameters minimal — anything beyond
rides the inner request object exactly as `submit_job`/`generate_and_wait`
do today.

## Skills

Two packaged skills, installed by the deploy scripts:

- **`skills/img-gen-assets/SKILL.md`** — when to ask img-gen for assets
  (frontends, mockups, game/UI kits), the kinds vocabulary and constraints,
  the style-lock contract (consistent `generation.style` + identical params
  across a set's pieces), the file-handoff rule ("return paths, reference
  relative asset paths in code; do not base64 into context"), reading
  `manifest.json` to regenerate variants, and the tileable/ControlNet seam
  constraint.
- **`skills/img-gen-animation/SKILL.md`** — when to use `from_frames` /
  `from_spec` / `keyframes`, delay/loop guidance, GIF vs WebP for web
  deliverables, and the fixed-seed-chain anti-pattern.

OpenCode gets the same content via its commands mechanism
(`.opencode/commands/` markdown) documented in the README. Deploy scripts
gain an `install-skills` step copying the skill dirs into a target project.

## Delivery

Both composite endpoints accept `output.dest` (absolute directory). A new
compose-provided writer — not the existing `CopyImage`, which is PNG-only and
opaque-id-named — copies the finished named files from
`DATA_DIR/sets/{setID}/` into `dest`, preserving their real names
(`<name>.gif`, `<name>-sheet.png`, `atlas.json`, `manifest.json`):

- Path-traversal guard: every write target must resolve inside the supplied
  `dest` (cleaned, no `..`, symlinks resolved and re-pinned).
- **Unset dest**: composites inherit the existing export fallback chain
  (`dest` → `EXPORT_DIR` → `~/Downloads/img-gen`), matching
  `/api/v1/export`'s behavior. **Collisions with existing files are
  rejected** (`compose_error`); agents re-asking should pick a new `name`.
- Auth stays uniform with the rest of `/api/*` (all calls require the token
  when `IMG_GEN_TOKEN` is set); the traversal guard applies to every caller.
- Response text returns absolute delivered paths per asset.

## Error handling

Typed v1 envelopes throughout, consistent with the existing surface:

- `validation_error` — unknown or phase-gated kind, missing fields,
  tileable+ControlNet, RIFE unset with `keyframes`, bad/relative/traversal
  `dest`, frames out of range, columns < 1, size outside engine limits.
- `generation_error` — propagated per-asset; partial-success semantics above.
- `compose_error` — post-process failure (encode failed, packing
  inconsistent, delivery collision); job marked failed, inputs retained and
  listed in the manifest for manual composition.
- MCP: REST failures surface as `isError` tool results (existing pattern) —
  never protocol errors.

## Testing

- `internal/compose`: unit tests for GIF round-trips (delay, loop,
  transparency, shared palette), atlas packing (geometry + JSON shape),
  wang-class seed-derivation stability, alpha unmix golden files (synthetic
  fixture PNGs, not generated images), delivery writer (collision, traversal,
  fallback chain).
- Queue coordinator: stubbed generation (existing test fake pattern),
  covering partial-failure, all-failed, cancel propagation, and **restart
  restore** of a mid-compose parent (children terminal / mixed / none).
- v1 endpoints: `cmd/img-gen/v1_test.go` style — validation matrices, sync
  vs async, delivery guards, set-file serving bounds.
- MCP: `cmd/img-gen-mcp/main_test.go` fake-server pattern for the three new
  tools including preview content blocks and `read_export` bounds.
- Skills: deploy-script test that skill dirs land and SKILL.md frontmatter
  parses; CI grep that openapi.yaml mentions every shipped endpoint.

## Phasing

1. **Phase 1** — `internal/compose` + queue coordinator + child-submit
   (`asset` path) + storage sets/Files/retrieval endpoints + `/api/v1/asset-set`
   (kind: element (with alpha), kit, atlas) + `/api/v1/gif`
   (from_frames, from_spec) + MCP tools + skills + delivery + openapi.yaml +
   README. `kind: tile` rejected with a phase notice; `keyframes` rejected
   with the RIFE notice.
2. **Phase 2** — mflux sidecar `tileable` patch (latent-rolling + circular
   VAE decode); `kind: tile` flips active. **Transport**: `tileable` is
   direct-sidecar-only (`IMAGE_ROUTING=direct` semantics for tile-kind jobs),
   documented in the routing-transport table; lattice pass-through of a
   sidecar extension field is explicitly deferred until the lattice
   transport gains an extension channel. The SDXL circular-conv variant is
   **not** phase-two scope; tile-kind jobs submitted on an SDXL-pinned
   model get `validation_error` until the later patch.
3. **Phase 3** — RIFE sidecar contract + `keyframes`; optional `rembg` alpha
   path; LayerDiffuse RGBA for SDXL (opt-in flag). Note: when Phase 3 lands,
   `RIFE_URL` must be added to `com.img-gen.plist.in` + `install-macos.sh`
   exactly as `UPSCALE_URL` is wired, and to the pi systemd deploy notes.

## Prior art (verified research)

Verification: 34 web claims adversarially verified (78 sub-claims clean, 20
corrections applied). Corrections that shaped this design:

- **anthropics/skills has NO SVG/asset-generation skill** (canvas-design,
  web-artifacts-builder, algorithmic-art only) — an image-asset skill for
  coding agents remains a real gap even at the official skill level, and
  frontend-design (styling) does not generate raster assets.
- **Fixed-seed img2img chaining is an anti-pattern** (progressive
  degradation); consistent frame sets instead use varied seeds + ControlNet +
  sprite-sheet LoRAs (sd-anim-utils and established community recipes).
  This spec's `from_spec` and atlas use varied seeds.
- **Seamless tiling**: architecture-independent technique is latent-rolling +
  circular VAE decode (OliverCrosby/Universal-Seamless-Tiles); SD-class conv
  padding (asymmetric-tiling family) covers SDXL. **ControlNet breaks
  circular padding with a seam** — hence the rejection rule above.
- **LayerDiffuse yields real RGBA for SDXL-class only, never FLUX** — hence
  dual-render unmixing as the portable v1 and LayerDiffuse as a Phase 3
  opt-in (correct upstream: huchenlei/ComfyUI-layerdiffuse).
- **Go stdlib `image/gif` fully encodes animated GIFs** (per-frame delay,
  disposal, transparency, NETSCAPE2.0 loop); the inter-frame transparency
  optimization is the gifx technique, implementable in-process; ffmpeg
  palettegen/paletteuse stays the quality fallback. `cwebp` covers WebP.
- **RIFE 4.24+ is the recommended interpolator for diffusion output**
  (Practical-RIFE; 4.22.lite noted for diffusion post-processing in the ECCV
  README), served as an ncnn-sidecar analogous to the existing UPSCALE
  sidecar.
- **MCP delivery convention**: dominant agent-facing servers return file
  paths as text + one inline base64 preview (Figma MCP reference; MCP SDK
  Image helpers standardize the block). img-gen already persists
  `image_path` per job, so file-path delivery fits the storage model.
- Established commercial anchors for the kit/tileset use case: Retro
  Diffusion (wang tilesets, REST + MCP), Tilewise/SpriteShip/Ludo; atlas
  conventions: TexturePacker JSON-Hash, Aseprite CLI grid flags.
- Differentiators: several sprite/tile MCP servers exist in the wild, but no
  established server does structured Wang-style pattern-set generation for UI
  work, and none does animated GIF assembly.