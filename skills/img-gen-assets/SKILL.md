---
name: img-gen-assets
description: Generate reusable image assets for the project being developed — UI elements, wang tile kits, sprite atlases — through the img-gen service. Use when a frontend or design task needs images, icons, tiles, or pattern sets, and when wiring img-gen into an agent workflow.
---

# img-gen asset generation

img-gen composes image assets from parallel generations. Two transports:
the MCP tools `request_asset_set` / `read_export`, or REST against
`$IMG_GEN_URL` (default http://127.0.0.1:8099).

## Asset kinds

| kind | inputs | output |
|---|---|---|
| `element` | prompt, size, `transparent: true` for cutouts | `<name>.png` |
| `kit` | prompt (subject/style), size | 20 wang tiles `<name>-<class>.png` |
| `atlas` | prompt, size, frames 2-64, columns | `<name>-sheet.png` + `<name>-atlas.json` |
| `frame` | prompt, size, `frame_margins` 1..size/2-1 | `<name>.png` (interior punched transparent) + `<name>.frame.json` |
| `bar` | prompt (track), `bar_fill` (fill), `frame_margins`, `fill_states` | `<name>-track.png`, `<name>-fill.png`, `<name>-fill-<NNN>.png`, `<name>.bar.json` |
| `npc` | prompt, `shape` ("variants"|"walk4"), `variants` (suffixes) | variants: `<name>-sheet.png` + `<name>-atlas.json`; walk4: `<name>-walk.png`, 4 cells, `<name>-atlas.json` |

Sizes must be `WxH` with both multiples of 16. `seed` is required — a set
must be reproducible while a design iterates.

## Workflow

1. Design the set: which standalone elements, whether a wang kit covers the
   tile work, whether animation frames belong in an atlas.
2. Submit (`request_asset_set` MCP tool, or POST /api/v1/asset-sets on the
   REST surface). One tool call returns the composed job plus an inline
   preview of the primary file.
3. Deliver into the project: pass `"dest": "/abs/path/to/project/assets"` —
   delivery copies the files there, ships the manifest as
   `<name>.manifest.json` so two sets can share one destination, and
   records the destination in it. Delivery never overwrites: clear the
   destination file and retry if you need to replace one.
4. Inspect or re-fetch anything later with `read_export` (MCP) or
   `GET /api/v1/sets/{set_id}`.

## Example

request `{"name":"ui-kit","seed":1234,"dest":"/work/site/assets",
"assets":[{"kind":"element","name":"logo","prompt":"flat logo, deep teal, minimal",
"size":"256x256","transparent":true},
{"kind":"kit","name":"panels","prompt":"parchment UI panels","size":"128x128"}]}`

Rest of the set arrives with the job; per-asset errors (one failed
generation among many) are recorded in manifest.json — the set still
succeeds when at least one asset composed.

## Constraints

- ≤ 8 assets per set, ≤ 64 total child generations.
- One asset failure does not fail the set; a kit missing a class fails as
  an asset (all 20 classes are required for the kit to compose).

## Palette lock

Set `"palette_lock": true` on the asset set request: every composed image is
remapped onto ONE shared palette (CIEDE2000 nearest color, ≤255 colors,
transparent pixels untouched) and the set ships `<name>.palette.json`. Use it
whenever separate deliverables must look like one art pass — UI bundles,
element families, sprite groups.

**Party pattern** (a group of distinct NPCs with coherent color): N `element`
assets in one set, each with its own prompt, `palette_lock: true`. Delivery
is Kenney-style separate sprites; no special kind.

## NPC sheets

Two shapes, both with every child seeded at the SET seed (pinned — the
researched mechanic for consistent characters without image conditioning):

- `shape: "variants"`, `variants: ["idle", "wave", ...]` — one render per
  suffix (`prompt + ", " + suffix`), packed by the atlas rules (`columns`
  sizes the grid). RPG-Maker face grids: 96x96 cells, `columns: 4`.
- `shape: "walk4"` — three renders with fixed direction suffixes (down/up/
  right); left = horizontal mirror of right. Delivers `<name>-walk.png`
  (rows in RPG-Maker order: down, left, right, up), the four cells, and the
  atlas JSON.

**Honesty contract:** walk4 delivers four standing direction views whose
palette and composition form a family — NOT a walk animation, and not
guaranteed single-identity across directions (direction-prompt drift is the
researched failure mode). True walk cycles need anchor-image conditioning,
which the text+seed engine does not have. An NPC party (N distinct NPCs) is
the palette-lock party pattern above: N elements, `palette_lock: true`.

## Engine metadata (`meta_formats`)

Engines expect their own metadata formats; per-asset
`"meta_formats": [...]` (subset of `phaser`, `unity`, `sparrow`, `tiled`,
default `[]`) emits them as extra set files:

| format | file(s) | applies to |
|---|---|---|
| phaser | `<name>.phaser.json` | atlas, npc |
| unity | `<name>.frames.json` | atlas, npc |
| sparrow | `<name>.sparrow.xml` | atlas, npc |
| tiled | `<name>.tsx` + `<name>-wangset.png` | kit |

Unknown names and duplicates fail the submit; a format that does not
serve the asset's kind fails that asset at compose time (recorded in the
manifest, set partial-success unchanged). Mind the one-letter difference:
unity metadata is `<name>.frames.json`, while the frame-kind nine-slice
sidecar is `<name>.frame.json`. The tiled `.tsx` is a Tiled 1.10 tileset
with an edge-color wangset mapping all 20 kit classes (autotile-ready,
with classes individually selectable in the terrain brush).