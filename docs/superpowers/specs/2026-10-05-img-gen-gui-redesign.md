# img-gen GUI redesign — elegant, minimalist, professional

**Spec author:** 2026-10-05, from a full audit of the three `//go:embed` frontend files.
**Scope:** complete visual + interaction redesign of `cmd/img-gen/static/{index.html,style.css,app.js}`. No framework, no build step, no backend behavior change. All existing features stay: 7 modes, presets, batch, model/LoRA pickers, sampling controls, SSE log, progress/cancel, history actions, export.

## Goals

1. One calm, refined dark surface — single accent, token-driven design system, no gradients/glows.
2. Creative path first: the subject (genre + fields) leads; knobs and overrides recede into collapsed sections.
3. Image-first gallery; errors humanized instead of raw dumps (no internal hosts/paths in cards).

## Non-goals

- No new backend endpoints (consumes the existing API; error parsing is envelope-tolerant so the later `/api/v1` contract can land without re-touching this code).
- No framework migration, no bundler.

## Design system (style.css)

Tokens in `:root` (replace every ad-hoc value; delete the purple gradient + both radial glows, one faint top accent glow may remain):

```css
--bg:#0b0d12; --surface:#13161f; --surface-2:#1a1e2a; --border:#232838;
--text:#e6e9f2; --muted:#8b91a5; --accent:#8b7bff; --danger:#ff6b7a;
--ok:#4ade80; --warn:#fbbf24;
--font: system-ui,-apple-system,"SF Pro Text",sans-serif;
--mono: ui-monospace,"SF Mono",Menlo,monospace;
```

- Type scale: 12 / 13 / 14 / 15 / 20 / 28. Labels: 13px, weight 500, `--muted`, **sentence case** (drop the `0.78rem` uppercase + letter-spacing). Numerals (`seed`, `steps`, sizes): `font-variant-numeric: tabular-nums`.
- Spacing scale: 4/8/12/16/24/32 (replace the scattered 14/18/20/22 values and the `-10px` negative-margin hack).
- Radii: 8 (controls), 10 (cards/sections), 14 (panels/modal). Border `1px solid var(--border)`.
- Unify `button`/`a.button` into one `.btn` (variants: `.btn-primary`, `.btn-ghost`, `.btn-danger-ghost`); pull all hardcoded log colors (`#c4b5fd`, `#93c5fd`) into tokens.
- Focus: visible `outline: 2px solid var(--accent)` on `:focus-visible`; honor `prefers-reduced-motion`.

## Layout (index.html)

```
topbar  : brand · (status pill: dot + "connected")
main    : grid
  [left, wider]  #result (image, empty state, progress + cancel) / #log (terminal card, collapsed-looking)
  [right]        #composer
  [full width]   #history
#detail : modal (image + key/value meta + actions)
```

- Result/preview becomes the leading surface: real empty state ("No image yet — describe something and press Generate", plus a subtle upload affordance for image modes), inline **4px** progress bar inside the preview card (indeterminate shimmer for queued/enhancing, % during steps), compact cancel button beside it.
- Three competing status surfaces (conn dot, status string, log) collapse into: status pill in topbar + status line inside the result card. Log stays for detail.

## Structure (index.html)

Mode control becomes a **chip grid** (7 buttons, single-select, `aria-pressed`), not a `<select>`.
Composer order (first thing the eye meets = subject):

1. **Mode** chips.
2. **Source** — per-mode upload/reference blocks (`#edit-controls` … `#pose-controls` shown per mode; pose keeps prompt/size/strength).
3. **Subject** — Genre + Size row, dynamic `#fields`; boolean genre fields render as inline toggle row.
4. **Look** — Style + Preset + Model + LoRAs.
5. **Sampling** `<details>` collapsed by default — negative prompt, seed/steps/guidance, lock-seed.
6. **Advanced** `<details>` collapsed — batch count, model/LoRA raw overrides.
7. Sticky footer action: **Generate** (full-width primary).

## app.js behavior changes (only what the new markup requires)

- Keep `renderMode()` / `renderForm()` / `stream()` / `loadHistory()` logic; re-home DOM references to the new structure.
- Mode chips + `<details>` sections update `aria-pressed` / `open` states.
- **Error humanization:** a shared `humanizeError(e)` maps known patterns:
  - `409 ... "generation already in progress"` → "Generator busy — this runs when the current job finishes."
  - `broken pipe` / `EOF` / `connect: connection refused` → "M6 sidecar unreachable."
  - `429` → "Inference queue full."
  - `404 ... not found` → "Endpoint unavailable."
  - default → first line only, truncated at 120 chars, **with IPs/hosts/`/Users/...` path fragments stripped**.
- History cards: image-first (fixed `1:1` `aspect-ratio`, `object-fit: cover`), status badge overlaid on hover; human error line for failures; a single `⋯` menu replacing the always-visible View/Edit/Upscale row (menu items: View, Use as edit source, Upscale 4×, Save to folder, Download). Meta lines (model · lora · style · seed) move into the detail modal as a key/value list.
- Empty states for history and log.

## Acceptance checks

- [ ] No uppercase/tracking labels, no `#7c6cff`/`#b06cff`, no dual radial glows anywhere in style.css.
- [ ] Preset, batch, pose all reachable: preset dropdown in Look; batch inside Advanced; pose chips + reference upload + strength slider.
- [ ] A failed generation shows a human one-line error in its history card (no raw host/Path).
- [ ] History renders image-first with `⋯` menu; detail modal shows full meta.
- [ ] Progress: indeterminate shimmer while queued/enhancing; % while generating; cancel available.
- [ ] Manual smoke in Playwright: generate 512×512 (8 steps) → done; preset selection applies; batch=2 produces two cards; pose submit requires reference (validation surfaces through UI).
- [ ] `go build ./... && go vet ./... && go test ./...` still green (embed unchanged shape).