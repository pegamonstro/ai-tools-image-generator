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