# Asset Expansion Design: Palette Coherence, Frames, Bars, NPC Sheets, Composite GIFs

**Status:** approved design (2026-10-12)
**Predecessor:** `2026-10-12-agent-assets-gif-design.md` (asset sets, kits, atlases, GIFs — shipped)
**Research basis:** delivered-manifest fix shipped as 1f6d44c; NPC priors research (2026-10-12): RPG-Maker resource standards, Retro Diffusion seed-pinning docs, engine import guides, Kenney packs.

## 1. Goal

Extend the img-gen asset factory with five engine-feasible features requested by the user: coherent composite compositions (color unity), boundaries/frames (nine-patch), bars, NPC character sheets, and hybrid GIF compositions — all under the binding engine constraint.

**Binding constraint:** the generation engine takes ONLY text prompts + integer seed. No image conditioning, no img2img, no runtime LoRA. Every feature below is a prompt+seed strategy plus a Go post-process. Anything needing conditioning is a non-goal (walk-cycle animation), shipped instead as an honest variant-family mechanic that is documented as such.

## 2. What exists to build on

- `POST /api/v1/asset-sets` — 1..8 assets of kind `element|kit|atlas`; children render sequentially; compose writes files into `DATA_DIR/sets/{id}/` + `manifest.json`; delivery namespaces manifests (`<name>.manifest.json`).
- `POST /api/v1/gifs` — `source.type` `from_frames|from_spec` (keyframes rejected, not_implemented); frames quantize to a shared median-cut palette (`sharedPalette`, internal/compose/gif.go:89) and composite pre-quantization.
- Child seeds: atlas children derive `seed+i` per frame (queue/compose.go atlasChildren); kit children `seed+SeedOffset`.
- Alpha: element `transparent:true` = white+black double pass (internal/compose/alpha.go); GIF alpha is 1-bit, thresholded at α<128 pre-quantization (halo-free by construction).

## 3. Feature F1 — Palette lock + coherence remap

**API.** `AssetSetRequest` gains:

```go
PaletteLock bool `json:"palette_lock,omitempty"`
```

**Mechanics.** When true, after ALL children of the set finish:

1. Build the set palette: run the existing fixed-stride histogram + deterministic median-cut (`sharedPalette` shape, 255 entries) over every *composed* set image (all kinds).
2. Remap every pixel of every composed image to its nearest palette entry in CIELAB space using the CIEDE2000 distance (full 7-term formula, exact float64 implementation — no shortcuts). Pixels with α<128 are excluded from the histogram and left fully transparent (α=0 preserved exactly).
3. The remapped images flow through the normal pipeline: set files, atlas/kit/frame/bar post-processing, manifest, delivery.

Rationale: batch coherence is won in post, not generation (research: LAB ΔE2000 nearest-color remap lifts batch consistency ~70–85% → 95%+). Coherence becomes a property of the set; GIFs and composites built from locked sets inherit it.

**Deliverable.** The set additionally contains `<name>.palette.json`:

```json
{"colors": ["#0f766e", "#0d3b39", "..."]}
```

It is an ordinary set file (validSetFile passes; delivered flat like the other files). Max 255 colors — one slot stays reserved for the 1-bit transparent index, mirroring the GIF convention.

**Validation.** `palette_lock` is a bool; no extra validation. A set with it enabled that fails part-way still records per-asset errors in the manifest as today. Remap never fails the set: if the image decoding of one child fails remap, that child records its existing per-asset error unchanged (remap runs only on successfully composed children).

**Tests.** ΔE2000 nearest-match correctness on crafted palettes; α<128 preservation; determinism (two runs → byte-identical remapped PNG + palette file); palette file content; remap integration point ordering (applied before frame/bar/atlas post-processing so every deliverable inherits it).

**Integration point.** Remap runs in the compose executor after per-kind composition — element/kit/atlas/frame/bar/npc deliverables exist as PNG outputs — and before `SaveSetFile`: the set's PNG outputs are decoded once, the palette is built over all of them via the existing fixed-stride median-cut, and each PNG output is remapped to that palette before saving. Non-image outputs (atlas/frame/bar/palette JSON) pass through untouched. This is one uniform hook: the post-processing geometry is lossless slicing, so remapping outputs is equivalent to remapping inputs while never touching the alpha-extraction pass.

## 4. Feature F2 — Nine-patch `frame` kind

**API.** `AssetSpecIn` gains:

```go
FrameMargins int `json:"frame_margins,omitempty"`
```

A `frame` asset renders ONE child: the asset prompt, full asset size, prompt expectation: border art on all four sides, full-bleed. Post-process punches the interior transparent:

- Interior rect `[margins, size-margins)` gets α=0 on every pixel (the transparent hole).
- Deliverables: `<name>.png` (single border image with hole — the convention CSS `border-image`, Unity 9-slice, Godot nine-patch all consume) and `<name>.frame.json`:

```json
{"layout": "nine-slice", "margins": {"top": 32, "right": 32, "bottom": 32, "left": 32}}
```

All four margins equal `frame_margins`. Engines stretch corners/edges from the single PNG; we ship no separate corner/edge cells.

**Validation.** `frame_margins` is REQUIRED for kind `frame` (and `bar`, F3): 1 ≤ m ≤ size/2 − 1, error otherwise. Works with `transparent:true` (double pass first, punch second — punch preserves the extracted alpha outside the interior).

**Tests.** Hole geometry on synthetic images (interior α=0, border untouched); margins JSON content; validation bounds; frame+transparent double-pass ordering.

## 5. Feature F3 — `bar` kind

**API.** `AssetSpecIn` gains:

```go
BarFill     string `json:"bar_fill,omitempty"`
FillStates  []int  `json:"fill_states,omitempty"`
```

A `bar` asset renders TWO children (both at the set seed family, sequential as today):

1. **Track** — the asset `Prompt`, full size. Opaque render; post-process punches the interior hole (interior rect from `frame_margins`, same rule as frame) → `<name>-track.png`. If `transparent:true`, the track goes through double-pass first (rounded outer corners), then the punch.
2. **Fill** — prompt = `BarFill`, full width, opaque. One render; every fill state is a horizontal CLIP of this single render (never re-rendered per state): fraction `pct/100` of width, full height → `<name>-fill-<NNN>.png`, NNN zero-padded 3 digits (000, 025, 050, 075, 100 for the default states).

**Deliverables.** Track, fill, per-state fills, and `<name>.bar.json`:

```json
{"track": "health-track.png", "fill": "health-fill.png",
 "margins": {"top": 8, "right": 8, "bottom": 8, "left": 8}, "states": [25, 50, 75, 100]}
```

No separate cap files — the track PNG contains them (YAGNI).

**Validation.** `bar_fill` REQUIRED for kind bar (prompt for the fill art). `fill_states` optional; default `[25, 50, 75, 100]`; each value 1..100, ≤ 16 states, ascending after dedupe (error otherwise). `frame_margins` required (drives the track hole). Caps slicing from tracks with tiny sizes (16px) must degenerate gracefully — margins < size/2 already guarantees a non-empty interior.

**Tests.** Fill clip fractions (25% → 25% width); hole punch on track; bar.json contract; validation of states/margins; bar+transparent double-pass ordering; state determinism.

## 6. Feature F4 — NPC kind (`npc`)

Research verdict shapes what ships: under text+seed ONLY, consistent variant sets come from **pinned seed + one prompt per variant** (same seed, per-variant prompt suffix — composition scaffold repeats, content varies). `seed+i` with identical prompt yields variant FAMILIES, never animation. The npc kind adds one engine mechanic: children whose seed does NOT increment.

**API.** `AssetSpecIn` gains:

```go
Shape    string   `json:"shape,omitempty"`    // npc only: "variants" | "walk4"
Variants []string `json:"variants,omitempty"` // npc only: per-variant prompt suffixes
```

### Shape `variants` (covers expression sheets, emotes, turnarounds)

Each variant renders one child: prompt = asset `Prompt` + `", "` + variant suffix, and every child renders at the SAME seed — the set seed, with NO `+i` increment (the pinned-seed mechanic). Packed by existing atlas rules (`columns`, cell = asset size) → `<name>-sheet.png` + `<name>-atlas.json` (same naming and TexturePacker format the atlas kind ships; cell key `<name>-<NNNN>`). Manifest's asset entry records the variants list in sheet order.

Expressed as the RPG-Maker face grid: 96×96 cells, `columns: 4`, 8 variants → 384×192 — directly engine-compatible (VX/Ace: 96×96 faces, 4×2).

### Shape `walk4` (4-direction standing sheet — labeled honestly)

Three renders at the pinned seed with fixed direction suffixes:

- down: `", full front view, facing the viewer"`
- up: `", rear view, facing away from the viewer"`
- right: `", side profile view, facing right"`

Left = horizontal mirror of the right render (the 3-drawing rule). Sheet: 4 rows × 1 col in RPG-Maker order (down, left, right, up) → `<name>-walk.png`, plus the four individual cells `<name>-down.png`, `<name>-left.png` (flipped copy), `<name>-right.png`, `<name>-up.png`, and `<name>-atlas.json` documenting all four cells (RPG-Maker-compatible order).

**Honesty contract (must appear in docs/skills):** walk4 delivers four standing direction views whose palette/composition form a family — NOT a walk animation and not guaranteed single-identity across directions; direction-prompt drift is the researched failure mode. True walk cycles need an anchor-image mechanic img-gen does not have; the animation skill says why.

### `party` = documented pattern, no engine code

N distinct NPCs = N `element` assets in one set with `palette_lock: true` (Kenney-style separate-sprite delivery). Documented as the standard recipe in the assets skill; no new kind, no new fields.

**Validation.** kind npc requires `shape` ∈ {variants, walk4}; `variants` requires 2..64 suffixes and shape `variants`; walk4 ignores `variants`. Sizes 16-multiple per existing rule — 96 (RPG-Maker face cells) = 6×16 ✓.

**Tests.** Pinned-seed derivation (child seed == set seed for npc, unlike atlas seed+i); variant prompt composition (base + suffix with exact separator); walk4 mirror is a true horizontal flip (pixel-level); sheet packing order; manifest variants recording; validation errors.

## 7. Feature F5 — `composite` GIF source kind

**API.** `GifSource` gains type `composite`:

```go
type CompositeSource struct {
    Layers []CompositeLayer `json:"layers"`
}
type CompositeLayer struct {
    Static *StaticLayer `json:"static,omitempty"`
    Anim   *FileFrames  `json:"anim,omitempty"`   // same ref form as from_frames frames
}
type StaticLayer struct {
    Pos [2]int `json:"pos"` // x,y — static layer origin on the anim canvas
    // From: one frame ref in the form from_frames accepts (job id,
    // data-dir path, base64) PLUS a new {"set": "<set_id>", "file": "<name>"}
    // form resolved via the storage layer.
    From FrameRef `json:"from"`
}
```

Exactly ONE `anim` layer (the base — same per-frame refs `from_frames` accepts: job/path/b64, PLUS a new `{"set": "<set_id>", "file": "<name>"}` form resolved via the storage layer). Zero or more `static` layers, composited in list order after the base. Canvas = the anim layer's frame dimensions.

**Mechanics.** Per frame: decode anim frame as RGBA → blit each static layer at `pos` alpha-aware (over operator) → the composed RGBA enters the EXISTING pipeline (pre-quantization, so 1-bit GIF alpha stays halo-free). Static layers are constant across frames — the encoder's inter-frame difference optimization makes them nearly free in output size. Statics reused from already-rendered sets cost ZERO renders.

**Validation (submit time).** ≥ 1 anim layer, ≤ 8 static layers; `pos` present for statics. **Compose time:** every static source resolves; a static that does not fit the canvas (pos + image size beyond canvas) is a compose error with the offending file named — fail fast beats silent crop. Anim frame dimensions must be uniform (existing from_frames rule).

**Tests.** Static blit at pos over synthetic anim frames (alpha-correct over-compositing, pre-quantization order); `{"set","file"}` ref resolution incl. refusal of invalid set/file; overhang error naming the file; multi-static ordering; output GIF frame count/dimensions; WebP variant unchanged.

## 8. Docs and skills

- `skills/img-gen-assets/SKILL.md`: frame/bar/npc kinds, `palette_lock` recipe (incl. the party pattern), margins contracts.
- `skills/img-gen-animation/SKILL.md`: composite source, the variant-family vs walk-cycle distinction (the honesty contract), composite-from-locked-sets recipe.
- `README.md`: request field tables per endpoint, new kinds, palette file, NPC grids.
- MCP tools: field passthrough only (schema additions per kind/shape; no new tools).

## 9. Non-goals

- True walk-cycle / sequential limb animation (needs anchor-image conditioning — engine-constraint).
- img2img, reference conditioning, runtime LoRA, ControlNet.
- Phase 2/3 sidecars: mflux tileable, RIFE keyframes (`keyframes` stays not_implemented), rembg, LayerDiffuse.
- APNG / animated-WebP as NEW output formats (existing gif2webp best-effort stays).
- Separate nine-slice corner/edge cell files; separate bar cap files; per-state re-renders.
- An `npc` "party" kind (party ships as the elements+palette_lock pattern).

## 10. Build order

F1 palette lock → F2 frame → F3 bar → F4 npc → F5 composite. Each step is independently shippable; F5 last so composite demos can layer frames/bars/npc art. Docs land per-feature with each build, not as a separate trailing task.

## 11. Testing strategy

Unit tests at each layer per feature (as itemized above): pure-image feature tests in `internal/compose` (remap math, punch geometry, clip math, mirror, blit), validation + seed-derivation tests in `internal/queue` (compose_test.go patterns: genCapture fake generator, waitFor), API-schema tests at the v1 submit surface, MCP passthrough coverage where fields flow through. Every feature's delivery contract asserted through the existing compose_test.go delivery tests (namespaced manifests per 1f6d44c).