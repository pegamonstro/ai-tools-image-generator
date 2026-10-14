# Phase C — Crisp Pixel Post-Process Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add pixel-art repair (grid detect → dominant-palette quantize → downscale), a quantize-before-outline post-op, set-level coherent palettes for atlas frames, and a `POST /api/v1/repair` standalone endpoint — all Go stdlib compose-side, no sidecars.

**Architecture:** Two new files in `internal/compose` (`pixelart.go`, `outline.go`) expose pure image functions reused by both the queue's compose executor (new opt-in post-ops on asset-set children) and the new sync HTTP endpoint. Queue changes thread two new fields (`pixel_fix`, `outline`) through `AssetSpecIn`/`assetSpec` with submit-time gates and fixed executor phases; C3 wires the atlas path to the existing palette machinery (one palette implementation — `SetPalette`/`RemapImage`/`RemapPNG`/`MarshalPalette` — no new palette math).

**Tech Stack:** Go stdlib `image`, `image/color`, `image/png`; existing `internal/compose` palette machinery; httptest-based tests.

**Spec:** docs/superpowers/specs/2026-10-14-capability-roadmap-round2-design.md — lines 351-408 ("Phase C — Crisp pixel post-process"). Phase C testing bullets: unit grid-detection on synthetic drifted sprites (known cell → recovered ±1px), palette N-cap + CIEDE2000 merge, quantize→outline ordering (outline pixels equal a quantized-input reference), atlas coherent-palette test (all frames share the set palette); golden before/after pairs for one real artifact per repair stage; smoke: repair a genuinely noisy render and compare (use the :9106 render instance's output style as reference). Compatibility: default behavior unchanged, new fields opt-in.

## Global Constraints

- Go stdlib only — no new module dependencies, no go.mod edits.
- Every task ends green: `go vet ./... && go test ./...` must pass before the commit.
- Byte-for-byte defaults: every new field is opt-in; omitting all new fields produces byte-identical request payloads, job records, and deliverables. (C3 carve-out: palette_lock + atlas behavior change is spec-mandated — decision R8 below.)
- Honest failure: unset sidecar URLs and unreachable sidecars produce validation errors or failed jobs with named reasons — never silent field drops, never panics.
- Tests bind random ports (httptest). No secrets in tracked files.
- Every git commit message ends with the attribution trailer:
  `Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>`

## Decisions locked at plan time (binding; recorded in the SDD ledger as R1-R10)

- **R1 (quantize discipline):** outline alone quantizes internally via `SetPalette([img], 64)`; when `pixel_fix` also applies, the deliverable is already quantized (SetPalette over an already-snapped image returns its exact colors — idempotent).
- **R2 (kind gates):** `pixel_fix` on element + atlas ONLY; `outline` on element + frame ONLY; other kinds rejected at submit with `validation_error`.
- **R3 (defaults):** omitted outline color `""` behaves as `"auto"`; outline width omitted/0 → 1, normalized at submit so the persisted spec is explicit; explicit width outside 1..2 → error.
- **R4 (no-alpha):** an opaque deliverable → `OutlineStrokes` no-ops (no transparent pixels to stroke); NOT an error.
- **R5 (shared cell):** grid detection runs ONCE per asset on its first render; the found cell repairs every render of that asset → dual-pass pairs keep equal bounds for `UnmixAlpha`, atlas frames keep one size.
- **R6 (C3 palette source):** `setRenderPalette` = `SetPalette(decoded first DECODABLE done render among children in order, 255)` — children[0] in the healthy case ("render 0 of the set" per spec); no decodable render → nil → skip pre-assembly remap (post-assembly palette pass still governs); per-frame `RemapPNG` failures are logged, the frame is left as-is, and never breaks the asset.
- **R7:** pixel_fix compose post-op uses `RepairDefaultColors` = 32.
- **R8 (C3 carve-out):** palette_lock + atlas outputs intentionally differ from prior versions — spec-mandated; "byte-for-byte defaults" applies to new fields only; no-palette_lock behavior byte-identical.
- **R9 (sizes):** pixel_fix assets capped at ≤1024×1024 renders at submit (`validAssetSize` has no upper bound, so the cap mirrors the endpoint limit) — honest validation error.
- **R10 (executor order):** children → pixel_fix block → rembg block → kind switch (C3 remap inside the atlas case, before `ComposeAtlas`) → outline block → EmitMeta → palette-lock pass → tEXt → save.

---

### Task 1: pixelart.go — grid detection

**Files:**
- Create: `internal/compose/pixelart.go`
- Test: `internal/compose/pixelart_test.go`

**Interfaces:**
- Consumes: `nrgbaAt(c color.Color) color.NRGBA` (internal/compose/remap.go:94).
- Produces (later tasks rely on these exact names): `ErrNotPixelGrid = errors.New("not a pixel-art grid")`; consts `RepairMinCell = 2`, `RepairMaxCell = 64`, `RepairMaxColors = 64`, `RepairDefaultColors = 32`, `RepairMaxDim = 1024`; `DetectGridCell(img image.Image) (int, error)`. Test helpers live in `pixelart_test.go` (same package): `spriteImage(w, h, cell, jitter int) *image.NRGBA`, `blockImage(w, h, cellX, cellY int) *image.NRGBA`, `noiseImage`, `rampImage`, `radialImage`, `encodePNGBytes(img image.Image) []byte`, `countColors(img image.Image) int` — Tasks 2 and 4 reuse them.

- [ ] **Step 1: Write the failing tests**

Create `internal/compose/pixelart_test.go`:

```go
package compose

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

var spritePalette = [5][3]int{
	{33, 26, 82},
	{219, 62, 175},
	{31, 177, 76},
	{244, 208, 63},
	{46, 89, 225},
}

// blockImage fills w×h with cellX×cellY blocks colored from spritePalette,
// pattern ((x/cellX)*3+(y/cellY)*7)%5 — every horizontal and vertical
// neighbor cell differs.
func blockImage(w, h, cellX, cellY int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := spritePalette[((x/cellX)*3+(y/cellY)*7)%5]
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(c[0]), G: uint8(c[1]), B: uint8(c[2]), A: 255})
		}
	}
	return img
}

// spriteImage is blockImage with deterministic per-channel brightness noise
// of amplitude `jitter` (±j < half the minimum palette distance, so cell
// boundaries keep dominating the edge signal — the drifted render the spec's
// ±1px tolerance asks about).
func spriteImage(w, h, cell, jitter int) *image.NRGBA {
	img := blockImage(w, h, cell, cell)
	if jitter == 0 {
		return img
	}
	seed := uint32(1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := nrgbaAt(img.At(x, y))
			r, g, b := int(c.R), int(c.G), int(c.B)
				seed = seed*1664525 + 1013904223
				r = clamp255(r + int((seed>>16)&0xff)%(2*jitter) - jitter)
				seed = seed*1664525 + 1013904223
				g = clamp255(g + int((seed>>16)&0xff)%(2*jitter) - jitter)
				seed = seed*1664525 + 1013904223
				b = clamp255(b + int((seed>>16)&0xff)%(2*jitter) - jitter)
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255})
		}
	}
	return img
}

func clamp255(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func noiseImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			r := uint8((seed >> 16) & 0xff)
			seed = seed*1664525 + 1013904223
			g := uint8((seed >> 16) & 0xff)
			seed = seed*1664525 + 1013904223
			b := uint8((seed >> 16) & 0xff)
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img
}

func rampImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x * 255) / maxInt(w-1, 1))
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

func radialImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	cx, cy := float64(w)/2, float64(h)/2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d := math.Hypot(float64(x)-cx, float64(y)-cy)
			v := uint8(clamp255(int(d * 4)))
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

func encodePNGBytes(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func countColors(img image.Image) int {
	b := img.Bounds()
	seen := map[[4]uint8]bool{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			n := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			seen[[4]uint8{n.R, n.G, n.B, n.A}] = true
		}
	}
	return len(seen)
}

func TestDetectGridCellExact(t *testing.T) {
	for cell := 2; cell <= 64; cell++ {
		got, err := DetectGridCell(blockImage(256, 256, cell, cell))
		if err != nil {
			t.Fatalf("cell %d: %v", cell, err)
		}
		if got != cell {
			t.Fatalf("cell %d: recovered %d", cell, got)
		}
	}
}

func TestDetectGridCellToleratesDrift(t *testing.T) {
	got, err := DetectGridCell(spriteImage(128, 128, 16, 12))
	if err != nil {
		t.Fatalf("cell 16 jitter 12: %v", err)
	}
	if got != 16 {
		t.Fatalf("cell 16 jitter 12: recovered %d", got)
	}
}

func TestDetectGridCellIgnoresPhaseOffset(t *testing.T) {
	// 130×130 with cell 16 puts the boundary columns mid-image, away from
	// the edge — detection must be phase-independent.
	got, err := DetectGridCell(blockImage(130, 130, 16, 16))
	if err != nil {
		t.Fatalf("offset grid: %v", err)
	}
	if got != 16 {
		t.Fatalf("offset grid: recovered %d", got)
	}
}

func TestDetectGridCellRejectsNonGrid(t *testing.T) {
	cases := []struct {
		name string
		img  *image.NRGBA
	}{
		{"noise", noiseImage(64, 64)},
		{"ramp", rampImage(64, 64)},
		{"radial", radialImage(64, 64)},
	}
	for _, tc := range cases {
		if _, err := DetectGridCell(tc.img); !errors.Is(err, ErrNotPixelGrid) {
			t.Fatalf("%s: want ErrNotPixelGrid, got %v", tc.name, err)
		}
	}
}

func TestDetectGridCellRejectsAxisMismatch(t *testing.T) {
	// Cells 8 wide × 24 tall: only one axis self-aligns at the same lag.
	if _, err := DetectGridCell(blockImage(128, 128, 8, 24)); !errors.Is(err, ErrNotPixelGrid) {
		t.Fatalf("want ErrNotPixelGrid for 8x24 cells, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify the tests fail to compile**

Run: `go test ./internal/compose/ -run TestDetectGridCell`
Expected: FAIL — `undefined: DetectGridCell` (and `maxInt`/`minInt` unresolved in the ramp helper; they arrive with the implementation in Step 3 — for this step that compile failure is the signal the tests are wired).

- [ ] **Step 3: Write the implementation**

Create `internal/compose/pixelart.go` (detection half; the repair transform joins this file in Task 2):

```go
package compose

import (
	"errors"
	"image"
	"image/color"
	"math"
	"sort"
)

var ErrNotPixelGrid = errors.New("not a pixel-art grid")

// Repair contract constants shared by the compose post-op and the
// /api/v1/repair endpoint.
const (
	RepairMinCell       = 2
	RepairMaxCell       = 64
	RepairMaxColors     = 64
	RepairDefaultColors = 32
	RepairMaxDim        = 1024
)

// DetectGridCell estimates the pixel-art cell size of img by
// autocorrelating its edge-energy signal: a pixel-art grid spikes at every
// cell boundary and stays quiet inside cells, so the smoothed signal
// self-aligns at lag = cell. Horizontal and vertical lags must agree, else
// the image is refused (ErrNotPixelGrid) rather than repaired with a
// guessed cell. The image must span at least ~2 cells (plus 4 px) per axis.
func DetectGridCell(img image.Image) (int, error) {
	colEdges, rowEdges := edgesAlong(img)
	colLag, err := detectCellAxis(smooth1(colEdges))
	if err != nil {
		return 0, err
	}
	rowLag, err := detectCellAxis(smooth1(rowEdges))
	if err != nil {
		return 0, err
	}
	if colLag != rowLag {
		return 0, ErrNotPixelGrid
	}
	return colLag, nil
}

// edgesAlong sums absolute channel differences across each interior line:
// colEdges[x] is the boundary between columns x and x+1 (a vertical-line
// sum over every row), rowEdges[y] the same for horizontal boundaries.
func edgesAlong(img image.Image) (colEdges, rowEdges []int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > 1 {
		colEdges = make([]int, w-1)
		for x := 0; x < w-1; x++ {
			s := 0
			for y := 0; y < h; y++ {
				s += diff3(nrgbaAt(img.At(x+b.Min.X, y+b.Min.Y)), nrgbaAt(img.At(x+1+b.Min.X, y+b.Min.Y)))
			}
			colEdges[x] = s
		}
	}
	if h > 1 {
		rowEdges = make([]int, h-1)
		for y := 0; y < h-1; y++ {
			s := 0
			for x := 0; x < w; x++ {
				s += diff3(nrgbaAt(img.At(x+b.Min.X, y+b.Min.Y)), nrgbaAt(img.At(x+b.Min.X, y+1+b.Min.Y)))
			}
			rowEdges[y] = s
		}
	}
	return colEdges, rowEdges
}

func diff3(a, c color.NRGBA) int {
	return intAbs(int(a.R)-int(c.R)) + intAbs(int(a.G)-int(c.G)) + intAbs(int(a.B)-int(c.B))
}

func intAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// smooth1 box-filters arr with radius 1 (edge-padded), absorbing the ±1 px
// boundary drift the spec's tolerance allows.
func smooth1(arr []int) []float64 {
	n := len(arr)
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		sum, cnt := 0, 0
		for d := -1; d <= 1; d++ {
			j := i + d
			if j >= 0 && j < n {
				sum += arr[j]
				cnt++
			}
		}
		out[i] = float64(sum) / float64(cnt)
	}
	return out
}

// detectCellAxis returns the smallest lag whose smoothed edge signal
// self-aligns (cosine similarity ≥ 0.85). The concentration guard rejects
// signals whose energy carries no dominant spikes — flat fields, ramps, and
// smooth gradients cannot be pixel-art grids — and the lag window
// (2 .. min(64, n/2−4)) requires roughly two cells per axis.
func detectCellAxis(edges []float64) (int, error) {
	n := len(edges)
	if n < 8 {
		return 0, ErrNotPixelGrid
	}
	total := 0.0
	for _, e := range edges {
		total += e
	}
	if total <= 0 {
		return 0, ErrNotPixelGrid
	}
	top := maxInt(1, n/10)
	sorted := make([]float64, n)
	copy(sorted, edges)
	sort.Float64s(sorted)
	spikeEnergy := 0.0
	for i := n - top; i < n; i++ {
		spikeEnergy += sorted[i]
	}
	if spikeEnergy < 0.30*total {
		return 0, ErrNotPixelGrid
	}
	maxLag := minInt(RepairMaxCell, n/2-4)
	for lag := RepairMinCell; lag <= maxLag; lag++ {
		num, la, lb := 0.0, 0.0, 0.0
		for i := 0; i+lag < n; i++ {
			num += edges[i] * edges[i+lag]
			la += edges[i] * edges[i]
			lb += edges[i+lag] * edges[i+lag]
		}
		if la > 0 && lb > 0 && num/math.Sqrt(la*lb) >= 0.85 {
			return lag, nil
		}
	}
	return 0, ErrNotPixelGrid
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

- [ ] **Step 4: Run to verify the tests pass**

Run: `go test ./internal/compose/ -v -run 'TestDetectGridCell'`
Expected: PASS (all five tests). If a drift or reject case misses, re-check the thresholds above (0.30 concentration, 0.85 cosine) against the failing case's numbers — do not silently loosen either.

- [ ] **Step 5: Full green + commit**

Run: `go vet ./... && go test ./...`
Expected: PASS.

```bash
git add internal/compose/pixelart.go internal/compose/pixelart_test.go
git commit -m "$(cat <<'EOF'
feat(compose): pixel-art grid detection (edge autocorrelation)

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: pixelart.go — RepairImage (quantize + downscale) and RepairPNG

**Files:**
- Modify: `internal/compose/pixelart.go` (append)
- Test: `internal/compose/pixelart_test.go` (append)

**Interfaces:**
- Consumes (from Task 1, exact): `ErrNotPixelGrid`, `DetectGridCell(img image.Image) (int, error)`, consts `RepairMinCell=2` / `RepairMaxCell=64` / `RepairMaxColors=64` / `RepairDefaultColors=32` / `RepairMaxDim=1024`; from remap.go: `SetPalette(imgs []image.Image, n int) color.Palette`, `RemapImage(img image.Image, pal color.Palette) *image.NRGBA`; test helpers `blockImage`, `spriteImage`, `noiseImage`, `encodePNGBytes`, `countColors`.
- Produces: `DownscaleByCell(src image.Image, cell int) *image.NRGBA`; `RepairImage(src image.Image, cell, colors int) (*image.NRGBA, error)`; `RepairPNG(data []byte, cell, colors int) ([]byte, error)`. Task 3 calls `RepairPNG` with an explicit detected cell and `RepairDefaultColors`; Task 6 calls `RepairPNG` with the endpoint's values (cell 0 = auto).

- [ ] **Step 1: Write the failing tests**

Append to `internal/compose/pixelart_test.go`:

```go
func TestRepairImageQuantizeAndDownscale(t *testing.T) {
	// 64×64, cell 8, three exact colors: repaired to 8×8 with exactly the
	// three palette colors (median cut of exact colors is exact).
	src := blockImage(64, 64, 8, 8)
	out, err := RepairImage(src, 8, 5)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if b := out.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("want 8x8, got %dx%d", b.Dx(), b.Dy())
	}
	if n := countColors(out); n > 5 {
		t.Fatalf("want at most 5 colors, got %d", n)
	}
}

func TestRepairImageAutoDetect(t *testing.T) {
	out, err := RepairImage(blockImage(64, 64, 8, 8), 0, 32)
	if err != nil {
		t.Fatalf("auto cell: %v", err)
	}
	if b := out.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("auto cell: want 8x8, got %dx%d", b.Dx(), b.Dy())
	}
}

func TestRepairImagePaletteCap(t *testing.T) {
	// 12 near-duplicate greens; colors=4 must cap the palette at 4 entries
	// (the N-cap over the existing median-cut + CIEDE2000 remap machinery).
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	shades := make([][3]uint8, 12)
	for i := range shades {
		shades[i] = [3]uint8{8, uint8(200 - i*8), uint8(60 + i*4)}
	}
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			s := shades[((x/8)+(y/8)*2)%12]
			img.SetNRGBA(x, y, color.NRGBA{R: s[0], G: s[1], B: s[2], A: 255})
		}
	}
	out, err := RepairImage(img, 8, 4)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if n := countColors(out); n > 4 {
		t.Fatalf("want at most 4 colors after the N cap, got %d", n)
	}
}

func TestRepairImageKeepsTransparency(t *testing.T) {
	src := blockImage(16, 16, 8, 8)
	// clear cell (0,1): rows 8..15, cols 0..7 fully transparent
	for y := 8; y < 16; y++ {
		for x := 0; x < 8; x++ {
			src.SetNRGBA(x, y, color.NRGBA{})
		}
	}
	out, err := RepairImage(src, 8, 32)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if c := out.NRGBAAt(0, 1); c.A != 0 {
		t.Fatalf("cleared cell must stay transparent, got %v", c)
	}
}

func TestRepairPNGNotAGrid(t *testing.T) {
	_, err := RepairPNG(encodePNGBytes(noiseImage(64, 64)), 0, 32)
	if !errors.Is(err, ErrNotPixelGrid) {
		t.Fatalf("want ErrNotPixelGrid, got %v", err)
	}
}

func TestRepairPNGErrors(t *testing.T) {
	data := encodePNGBytes(blockImage(32, 32, 8, 8))
	if _, err := RepairPNG(nil, 8, 32); err == nil || !strings.Contains(err.Error(), "not a PNG") {
		t.Fatalf("want not-a-PNG error, got %v", err)
	}
	if _, err := RepairPNG(data, 65, 32); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("want cell range error, got %v", err)
	}
	if _, err := RepairPNG(data, 1, 32); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("want cell range error for 1, got %v", err)
	}
	if _, err := RepairPNG(data, 8, 65); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("want colors range error, got %v", err)
	}
	if _, err := RepairPNG(data, 8, 0); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("want colors range error for 0, got %v", err)
	}
}

func TestRepairPNGIsDeterministic(t *testing.T) {
	data := encodePNGBytes(spriteImage(64, 64, 16, 12))
	a, err := RepairPNG(data, 0, 5)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	b, err := RepairPNG(data, 0, 5)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two identical repairs diverged")
	}
}
```

The tests use `strings` — add `"strings"` to the test file's imports.

- [ ] **Step 2: Run to verify the tests fail to compile**

Run: `go test ./internal/compose/ -run 'TestRepair'`
Expected: FAIL — `undefined: RepairImage` (and `RepairPNG`).

- [ ] **Step 3: Write the implementation**

Append to `internal/compose/pixelart.go` (imports gain `"bytes"`, `"fmt"`, `"image/png"`):

```go
// DownscaleByCell shrinks src to floor(w/cell)×floor(h/cell) by sampling
// each cell's center pixel; a partial trailing cell (bounds not a multiple
// of cell) is dropped. RepairImage rejects non-multiples before this ever
// sees one.
func DownscaleByCell(src image.Image, cell int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx()/cell, b.Dy()/cell
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out.SetNRGBA(x, y, nrgbaAt(src.At(b.Min.X+x*cell+cell/2, b.Min.Y+y*cell+cell/2)))
		}
	}
	return out
}

// RepairImage quantizes src onto its dominant palette (at most colors
// colors, the ONE palette implementation from remap.go) and downscales by
// its pixel-art cell. cell 0 means auto-detect; explicit cells must be
// inside 2..64. Transparency is preserved: pixels with alpha < 128 snap to
// fully transparent, the 1-bit convention RemapImage shares with the GIF
// encoder.
func RepairImage(src image.Image, cell, colors int) (*image.NRGBA, error) {
	if colors < 1 || colors > RepairMaxColors {
		return nil, fmt.Errorf("compose: repair: colors %d out of range 1..%d", colors, RepairMaxColors)
	}
	switch {
	case cell == 0:
		detected, err := DetectGridCell(src)
		if err != nil {
			return nil, err
		}
		cell = detected
	case cell < RepairMinCell || cell > RepairMaxCell:
		return nil, fmt.Errorf("compose: repair: cell %d out of range %d..%d", cell, RepairMinCell, RepairMaxCell)
	}
	b := src.Bounds()
	if b.Dx()%cell != 0 || b.Dy()%cell != 0 {
		return nil, fmt.Errorf("compose: repair: %dx%d is not a multiple of cell %d", b.Dx(), b.Dy(), cell)
	}
	pal := SetPalette([]image.Image{src}, colors)
	quantized := RemapImage(src, pal)
	return DownscaleByCell(quantized, cell), nil
}

// RepairPNG is RepairImage over raw PNG bytes: "compose: repair: ..." errors.
func RepairPNG(data []byte, cell, colors int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("compose: repair: not a PNG: %v", err)
	}
	out, err := RepairImage(img, cell, colors)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("compose: repair: encode: %v", err)
	}
	return buf.Bytes(), nil
}
```

- [ ] **Step 4: Run to verify the tests pass**

Run: `go test ./internal/compose/ -v -run 'TestRepair|TestDetectGridCell'`
Expected: PASS (Task 1's tests still green).

- [ ] **Step 5: Full green + commit**

Run: `go vet ./... && go test ./...`
Expected: PASS.

```bash
git add internal/compose/pixelart.go internal/compose/pixelart_test.go
git commit -m "$(cat <<'EOF'
feat(compose): pixel-art repair — palette quantize + cell downscale

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: queue pixel_fix post-op (element + atlas)

**Files:**
- Modify: `internal/queue/compose.go` — `AssetSpecIn` field (after `MetaFormats`, ~:402), `assetSpec` field (after `MetaFormats`, ~:86), submit gate (after the rembg gate block, after the kind switch's closing brace ~:582), executor block (inside `if broken == ""`, before the B2 rembg comment block ~:770)
- Test: Create `internal/queue/compose_postops_test.go`

**Interfaces:**
- Consumes (from Tasks 1-2, exact): `compose.DetectGridCell(img image.Image) (int, error)`, `compose.RepairPNG(data []byte, cell, colors int) ([]byte, error)`, `compose.RepairDefaultColors`, `compose.RepairMaxDim`, error text `"not a pixel-art grid"`.
- Produces: `AssetSpecIn.PixelFix bool` + `assetSpec.PixelFix bool`, both `json:"pixel_fix,omitempty"`; the executor ordering contract (R10) that Tasks 5 and 7 build on; the test harness file `compose_postops_test.go` that Tasks 5 and 7 append to (with the shared `pixelGridGen` fake, whose generator signature is this repo's `ImageOps.Generate` shape).

- [ ] **Step 1: Write the failing tests**

Create `internal/queue/compose_postops_test.go`:

```go
package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
	"testing"

	"img-gen/internal/compose"
)

// pixelGridGen renders block art with a known cell: the requested WxH filled
// with `cell`-px cells cycling through gridColors ((x/cell)+(y/cell)*2 mod
// len — adjacent cells always differ), so the repair pipeline can find the
// exact grid. Signature mirrors the ImageOps.Generate fakes in
// compose_test.go; `cap` may be nil (pass a genCapture to record renders).
func pixelGridGen(t *testing.T, cap *genCapture, cell int, gridColors [][3]uint8) func(context.Context, string, string, *storage.ControlParams, *storage.IPAdapterParams, storage.ModelSpec, storage.SamplingParams) ([]byte, *int64, error) {
	t.Helper()
	return func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		ws, hs, _ := strings.Cut(size, "x")
		w, _ := strconv.Atoi(ws)
		h, _ := strconv.Atoi(hs)
		if w < 1 || h < 1 {
			w, h = 8, 8
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := gridColors[((x/cell)+(y/cell)*2)%len(gridColors)]
				img.Set(x, y, color.RGBA{R: c[0], G: c[1], B: c[2], A: 255})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if cap != nil {
			cap.mu.Lock()
			cap.Prompts = append(cap.Prompts, prompt)
			cap.Sizes = append(cap.Sizes, size)
			cap.Seeds = append(cap.Seeds, sp.Seed)
			cap.Images = append(cap.Images, buf.Bytes())
			cap.mu.Unlock()
		}
		return buf.Bytes(), sp.Seed, nil
	}
}

func samePixelData(t *testing.T, a, b image.Image) {
	t.Helper()
	ba, bb := a.Bounds(), b.Bounds()
	if ba.Dx() != bb.Dx() || ba.Dy() != bb.Dy() {
		t.Fatalf("bounds differ: %v vs %v", ba, bb)
	}
	for y := 0; y < ba.Dy(); y++ {
		for x := 0; x < ba.Dx(); x++ {
			ca := color.RGBAModel.Convert(a.At(ba.Min.X+x, ba.Min.Y+y)).(color.RGBA)
			cb := color.RGBAModel.Convert(b.At(bb.Min.X+x, bb.Min.Y+y)).(color.RGBA)
			if ca != cb {
				t.Fatalf("pixel (%d,%d) differs: %v vs %v", x, y, ca, cb)
			}
		}
	}
}

func TestComposePixelFixElement(t *testing.T) {
	colors3 := [][3]uint8{{20, 24, 31}, {232, 93, 4}, {251, 242, 54}}
	gcap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), gcap)
	o.Ops.Generate = pixelGridGen(t, gcap, 8, colors3)
	m := New(o)
	seed := int64(41)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "pix", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "el", Prompt: "pixel sprite", Size: "64x64", PixelFix: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	data, err := m.opts.Store.ReadSetFile(id, "el.png")
	if err != nil {
		t.Fatalf("deliverable: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("want repaired 8x8, got %dx%d", b.Dx(), b.Dy())
	}
	if n := countColorsCompat(img); n != 3 {
		t.Fatalf("want exactly the 3 palette colors, got %d", n)
	}
	// The deliverable equals repairing the raw render directly.
	gcap.mu.Lock()
	render := gcap.Images[0]
	gcap.mu.Unlock()
	want, err := compose.RepairPNG(render, 8, compose.RepairDefaultColors)
	if err != nil {
		t.Fatalf("direct repair: %v", err)
	}
	wantImg, err := png.Decode(bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	samePixelData(t, wantImg, img)
}

func countColorsCompat(img image.Image) int {
	b := img.Bounds()
	seen := map[color.RGBA]bool{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			seen[color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)] = true
		}
	}
	return len(seen)
}

func TestComposePixelFixAtlas(t *testing.T) {
	colors3 := [][3]uint8{{20, 24, 31}, {232, 93, 4}, {251, 242, 54}}
	o := composeTestOpts(t, t.TempDir(), nil)
	o.Ops.Generate = pixelGridGen(t, nil, 8, colors3)
	m := New(o)
	seed := int64(43)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "atl", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "atlas", Name: "sheet", Prompt: "anim", Size: "32x32", Frames: 2, PixelFix: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	// Each repaired frame is 4x4 (32/8); two frames, single row → 8x4 sheet.
	data, err := m.opts.Store.ReadSetFile(id, "sheet-sheet.png")
	if err != nil {
		t.Fatalf("sheet: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 4 {
		t.Fatalf("want 8x4 sheet, got %dx%d", b.Dx(), b.Dy())
	}
}

func TestComposePixelFixNotAGrid(t *testing.T) {
	o := composeTestOpts(t, t.TempDir(), nil)
	o.Ops.Generate = noiseGen(t)
	m := New(o)
	seed := int64(41)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "noise", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "el", Prompt: "noisy", Size: "64x64", PixelFix: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Single broken asset → the parent finishes failed (the executor's
	// partial-success rule needs ok > 0 for done); the manifest still lands.
	waitFor(t, m, id, "failed")
	raw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man struct {
		Assets []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Assets) != 1 || man.Assets[0].Name != "el" {
		t.Fatalf("manifest assets: %+v", man.Assets)
	}
	if !strings.Contains(man.Assets[0].Error, "pixel_fix") ||
		!strings.Contains(man.Assets[0].Error, "not a pixel-art grid") {
		t.Fatalf("want pixel_fix reason in manifest, got %q", man.Assets[0].Error)
	}
}
```

`noiseGen` (same file, below the tests) — renders a deterministic RGB-noise image at the requested size, no grid anywhere:

```go
func noiseGen(t *testing.T) func(context.Context, string, string, *storage.ControlParams, *storage.IPAdapterParams, storage.ModelSpec, storage.SamplingParams) ([]byte, *int64, error) {
	t.Helper()
	return func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		ws, hs, _ := strings.Cut(size, "x")
		w, _ := strconv.Atoi(ws)
		h, _ := strconv.Atoi(hs)
		if w < 1 || h < 1 {
			w, h = 8, 8
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		seed := uint32(1)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				seed = seed*1664525 + 1013904223
				r := uint8((seed >> 16) & 0xff)
				seed = seed*1664525 + 1013904223
				g := uint8((seed >> 16) & 0xff)
				seed = seed*1664525 + 1013904223
				b := uint8((seed >> 16) & 0xff)
				img.Set(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes(), sp.Seed, nil
	}
}
```

Then the validation tests:

```go
func TestSubmitAssetSetPixelFixValidation(t *testing.T) {
	m := New(composeTestOpts(t, t.TempDir(), nil))
	seed := int64(1)
	_, err := m.SubmitAssetSet(AssetSetRequest{Name: "bad", Seed: &seed, Assets: []AssetSpecIn{
		{Kind: "npc", Name: "n", Prompt: "p", Size: "64x64", Shape: "walk4", PixelFix: true},
	}})
	if err == nil || !strings.Contains(err.Error(), "pixel_fix is supported for element and atlas") {
		t.Fatalf("want pixel_fix kind gate, got %v", err)
	}
	_, err = m.SubmitAssetSet(AssetSetRequest{Name: "big", Seed: &seed, Assets: []AssetSpecIn{
		{Kind: "element", Name: "e", Prompt: "p", Size: "1056x16", PixelFix: true},
	}})
	if err == nil || !strings.Contains(err.Error(), "pixel_fix supports sizes up to 1024x1024") {
		t.Fatalf("want pixel_fix size cap, got %v", err)
	}
}
```

Add `"img-gen/internal/storage"` to the test file's imports (the fake generator signatures reference it).

- [ ] **Step 2: Run to verify the tests fail to compile**

Run: `go test ./internal/queue/ -run 'TestComposePixelFix'`
Expected: FAIL to compile — `unknown field PixelFix in struct literal`; the compose functions already exist from Tasks 1-2, so the struct-field errors are the signal.

- [ ] **Step 3: Wire the field and gates**

In `internal/queue/compose.go`:

1. `AssetSpecIn` — add after the `MetaFormats` line (inside the struct, before the closing brace):

```go
	PixelFix      bool            `json:"pixel_fix,omitempty"` // pixel-art repair post-op (opt-in)
```

2. `assetSpec` — add after its `MetaFormats` line:

```go
	PixelFix      bool            `json:"pixel_fix,omitempty"` // pixel-art repair post-op (opt-in)
```

(Align the field's whitespace with the struct's existing column formatting; do not reorder existing fields — persistence keeps byte-identical for specs without the new field since `omitempty` drops it.)

3. Submit gate — insert immediately after the rembg kind-gate block (the block ending with `Msg: fmt.Sprintf("asset %q: transparent %q requires the rembg sidecar (REMBG_URL)", ...)` and its closing brace), still inside the per-asset loop, before the model-seeding loop:

```go
		if a.PixelFix {
			switch a.Kind {
			case "element", "atlas":
			default:
				return "", &ValidationError{Code: "validation_error",
					Msg: fmt.Sprintf("asset %q: pixel_fix is supported for element and atlas", an)}
			}
			w, h, serr := parseAssetSize(a.Size)
			if serr != nil || w > compose.RepairMaxDim || h > compose.RepairMaxDim {
				return "", &ValidationError{Code: "validation_error",
					Msg: fmt.Sprintf("asset %q: pixel_fix supports sizes up to %dx%d", an, compose.RepairMaxDim, compose.RepairMaxDim)}
			}
		}
		as.PixelFix = a.PixelFix
```

- [ ] **Step 4: Wire the executor block**

In `composeAssetSet`, inside `if broken == "" {`, immediately BEFORE the B2 rembg comment block (`// B2 rembg branch: ...`), insert:

```go
			// C1 pixel repair (opt-in): quantize every render onto its
			// dominant palette and downscale by the detected grid cell. The
			// cell is detected ONCE on the first render, so dual-pair bounds
			// stay equal for UnmixAlpha and atlas frames keep one size. Runs
			// BEFORE the rembg branch: repair stabilizes every later step.
			if a.PixelFix {
				first, derr := png.Decode(bytes.NewReader(imgs[0]))
				if derr != nil {
					broken = fmt.Sprintf("pixel_fix: %v", derr)
				} else {
					cell, gerr := compose.DetectGridCell(first)
					if gerr != nil {
						broken = fmt.Sprintf("pixel_fix: %v", gerr)
					} else {
						for i := range imgs {
							fixed, rerr := compose.RepairPNG(imgs[i], cell, compose.RepairDefaultColors)
							if rerr != nil {
								broken = fmt.Sprintf("pixel_fix: %v", rerr)
								break
							}
							imgs[i] = fixed
						}
					}
				}
			}
```

- [ ] **Step 5: Run to verify the tests pass**

Run: `go test ./internal/queue/ -v -run 'TestComposePixelFix|TestSubmitAssetSetPixelFix'`
Expected: PASS (all four tests).

- [ ] **Step 6: Full green + commit**

Run: `go vet ./... && go test ./...`
Expected: PASS — including `TestSubmitAssetSetPaletteLock` (unchanged behavior without pixel_fix).

```bash
git add internal/queue/compose.go internal/queue/compose_postops_test.go
git commit -m "$(cat <<'EOF'
feat(queue): pixel_fix compose post-op for element and atlas

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: compose outline strokes (quantize-before-outline)

**Files:**
- Create: `internal/compose/outline.go`
- Test: Create `internal/compose/outline_test.go`

**Interfaces:**
- Consumes (from Tasks 1/2, exact): test helpers in `pixelart_test.go` (`spriteImage`, `encodePNGBytes`) — same package, no import needed; `compose.SetPalette(imgs []image.Image, n int) color.Palette` and `compose.RemapImage(img image.Image, pal color.Palette) *image.NRGBA` from `remap.go`.
- Produces (Task 5 consumes, exact): `compose.OutlineStrokes(src *image.NRGBA, width int, stroke color.NRGBA) *image.NRGBA`; `compose.ParseHexColor(spec string) (color.NRGBA, error)`; `compose.ParseOutlineColor(spec string, pal color.Palette) (color.NRGBA, error)`; `compose.OutlinePNG(data []byte, width int, colorSpec string) ([]byte, error)` — width validated to 1..2 with error `compose: outline: width %d out of range 1..2`; `""` and `"auto"` pick the darkest palette color.

- [ ] **Step 1: Write the failing tests**

Create `internal/compose/outline_test.go`:

```go
package compose

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func decodePNGTest(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestOutlineStrokesRing(t *testing.T) {
	// 8×8 transparent canvas, 4×4 opaque red block at (2..5)².
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if x >= 2 && x <= 5 && y >= 2 && y <= 5 {
				src.Set(x, y, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
			}
		}
	}
	out := OutlineStrokes(src, 1, color.NRGBA{R: 0, G: 255, B: 0, A: 255})
	// (2,1): above the block's left-top corner column → stroked (4-neighbor of opaque (2,2)).
	if got := out.At(2, 1); got != (color.NRGBA{R: 0, G: 255, B: 0, A: 255}) {
		t.Fatalf("(2,1) want stroke, got %v", got)
	}
	// (1,1): diagonal-only corner at width 1 → NOT stroked.
	if got := out.At(1, 1); got != (color.NRGBA{A: 0}) {
		t.Fatalf("(1,1) want still transparent at width 1, got %v", got)
	}
	// Interior stays the block color.
	if got := out.At(3, 3); got != (color.NRGBA{R: 255, G: 0, B: 0, A: 255}) {
		t.Fatalf("(3,3) want block red, got %v", got)
	}
	// Width 2 reaches the diagonal corner via the second dilation round.
	out2 := OutlineStrokes(src, 2, color.NRGBA{R: 0, G: 255, B: 0, A: 255})
	if got := out2.At(1, 1); got != (color.NRGBA{R: 0, G: 255, B: 0, A: 255}) {
		t.Fatalf("(1,1) want stroked at width 2, got %v", got)
	}
	// Width-2 ring is strictly larger: (0,3) stroked at 2 but not at 1.
	if got := out.At(0, 3); got != (color.NRGBA{A: 0}) {
		t.Fatalf("(0,3) want transparent at width 1, got %v", got)
	}
	if got := out2.At(0, 3); got != (color.NRGBA{R: 0, G: 255, B: 0, A: 255}) {
		t.Fatalf("(0,3) want stroked at width 2, got %v", got)
	}
}

func TestOutlineStrokesOpaqueIsNoOp(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			src.Set(x, y, color.NRGBA{R: 200, G: 10, B: 10, A: 255})
		}
	}
	out := OutlineStrokes(src, 2, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	// Nothing to dilate into: output identical to input.
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if out.At(x, y) != src.At(x, y) {
				t.Fatalf("(%d,%d) changed on fully-opaque input", x, y)
			}
		}
	}
}

func TestOutlineAutoDarkest(t *testing.T) {
	img := spriteImage(16, 16, 4, 0)
	// Force black into the palette: paint the top-left cell black.
	img.Set(0, 0, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	pal := SetPalette([]image.Image{img}, 64)
	got, err := ParseOutlineColor("auto", pal)
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if got != (color.NRGBA{R: 0, G: 0, B: 0, A: 255}) {
		t.Fatalf("auto should pick black, got %v", got)
	}
	if _, err := ParseOutlineColor("auto", nil); err == nil {
		t.Fatal("auto with empty palette must error")
	}
}

func TestParseHexColor(t *testing.T) {
	if got, err := ParseHexColor("#34c"); err != nil || got != (color.NRGBA{R: 0x33, G: 0x44, B: 0xcc, A: 255}) {
		t.Fatalf("#34c: %v %v", got, err)
	}
	if got, err := ParseHexColor("#3344cc"); err != nil || got != (color.NRGBA{R: 0x33, G: 0x44, B: 0xcc, A: 255}) {
		t.Fatalf("#3344cc: %v %v", got, err)
	}
	for _, bad := range []string{"red", "#12", "#12345", "3344cc", ""} {
		if _, err := ParseHexColor(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestOutlinePNGQuantizeBeforeOutline(t *testing.T) {
	// Noisy sprite whose jittered pixels would otherwise multiply the
	// outline boundary; quantizing first collapses them onto the palette,
	// and the output must equal outlining the already-quantized image.
	src := spriteImage(32, 32, 8, 6) // cell 8, jitter 6
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	got, err := OutlinePNG(buf.Bytes(), 1, "#000000")
	if err != nil {
		t.Fatalf("OutlinePNG: %v", err)
	}
	gotImg := decodePNGTest(t, got)
	pal := SetPalette([]image.Image{src}, 64)
	quantized := RemapImage(src, pal)
	ref := OutlineStrokes(quantized, 1, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	b := gotImg.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if gotImg.At(x, y) != ref.At(x, y) {
				t.Fatalf("(%d,%d): %v != reference %v", x, y, gotImg.At(x, y), ref.At(x, y))
			}
		}
	}
	// And every output pixel IS a palette color (quantization holds).
	palSet := map[color.NRGBA]bool{}
	for _, c := range pal {
		palSet[nrgbaAt(c)] = true
	}
	palSet[color.NRGBA{R: 0, G: 0, B: 0, A: 255}] = true // the stroke itself
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if c := nrgbaAt(gotImg.At(x, y)); !palSet[c] {
				t.Fatalf("(%d,%d) off-palette: %v", x, y, c)
			}
		}
	}
}

func TestOutlinePNGErrors(t *testing.T) {
	if _, err := OutlinePNG([]byte("junk"), 1, "#000000"); err == nil || !strings.Contains(err.Error(), "not a PNG") {
		t.Fatalf("want not-a-PNG, got %v", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	if _, err := OutlinePNG(buf.Bytes(), 0, "#000000"); err == nil || !strings.Contains(err.Error(), "out of range 1..2") {
		t.Fatalf("width 0: %v", err)
	}
	if _, err := OutlinePNG(buf.Bytes(), 3, "#000000"); err == nil || !strings.Contains(err.Error(), "out of range 1..2") {
		t.Fatalf("width 3: %v", err)
	}
}
```

(If `spriteImage`/`blockImage`/`encodePNGBytes` signatures in the final `pixelart_test.go` from Task 1 drift, align this file to them — they are the helpers of record.)

- [ ] **Step 2: Run to verify the tests fail to compile**

Run: `go test ./internal/compose/ -run 'TestOutline'`
Expected: FAIL — `undefined: OutlineStrokes` (and friends).

- [ ] **Step 3: Implement `internal/compose/outline.go`**

```go
package compose

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
)

// OutlineStrokes paints stroke onto every fully-transparent pixel that is a
// 4-neighbor of an opaque pixel, repeating width rounds of growth (rounds
// see the previous round's strokes, so width 2 reaches diagonal corners).
// Round-snapshot dilation: all fill candidates come from the round's start
// snapshot, then paint. Stroke alpha is forced opaque.
func OutlineStrokes(src *image.NRGBA, width int, stroke color.NRGBA) *image.NRGBA {
	stroke.A = 255
	out := cloneNRGBA(src)
	b := out.Bounds()
	touches := func(x, y int) bool {
		return (x > b.Min.X && alphaAt(out, x-1, y) >= 128) ||
			(x < b.Max.X-1 && alphaAt(out, x+1, y) >= 128) ||
			(y > b.Min.Y && alphaAt(out, x, y-1) >= 128) ||
			(y < b.Max.Y-1 && alphaAt(out, x, y+1) >= 128)
	}
	for round := 0; round < width; round++ {
		var fills []image.Point
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if alphaAt(out, x, y) < 128 && touches(x, y) {
					fills = append(fills, image.Point{x, y})
				}
			}
		}
		for _, p := range fills {
			out.Set(p.X, p.Y, stroke)
		}
		if len(fills) == 0 {
			break
		}
	}
	return out
}

func alphaAt(img *image.NRGBA, x, y int) uint8 {
	return img.NRGBAAt(x, y).A
}

func cloneNRGBA(src *image.NRGBA) *image.NRGBA {
	dst := *src
	dst.Pix = append([]uint8(nil), src.Pix...)
	return &dst
}

// ParseHexColor parses #rgb / #rrggbb into a fully opaque color.
func ParseHexColor(spec string) (color.NRGBA, error) {
	h := strings.TrimPrefix(spec, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return color.NRGBA{}, fmt.Errorf("compose: outline: bad color %q: want #rgb or #rrggbb", spec)
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return color.NRGBA{}, fmt.Errorf("compose: outline: bad color %q: want #rgb or #rrggbb", spec)
	}
	v := uint32(raw[0])<<16 | uint32(raw[1])<<8 | uint32(raw[2])
	return color.NRGBA{
		R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255,
	}, nil
}

// ParseOutlineColor resolves an outline color spec: "" or "auto" pick the
// darkest (luma-weighted) palette color; anything else is a hex spec.
func ParseOutlineColor(spec string, pal color.Palette) (color.NRGBA, error) {
	if spec == "" || spec == "auto" {
		best := color.NRGBA{}
		found := false
		bestLuma := float64(0)
		for _, c := range pal {
			r := nrgbaAt(c)
			if r.A == 0 {
				continue
			}
			luma := 0.2126*float64(r.R) + 0.7152*float64(r.G) + 0.0722*float64(r.B)
			if !found || luma < bestLuma {
				best, found, bestLuma = r, true, luma
			}
		}
		if !found {
			return color.NRGBA{}, errors.New("compose: outline: auto color needs a non-empty palette")
		}
		return best, nil
	}
	return ParseHexColor(spec)
}

// OutlinePNG quantizes the PNG onto its top-64 palette, resolves the stroke
// color, then strokes transparent pixels adjacent to opaque ones.
func OutlinePNG(data []byte, width int, colorSpec string) ([]byte, error) {
	if width < 1 || width > 2 {
		return nil, fmt.Errorf("compose: outline: width %d out of range 1..2", width)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("compose: outline: not a PNG: %v", err)
	}
	pal := SetPalette([]image.Image{img}, 64)
	stroke, err := ParseOutlineColor(colorSpec, pal)
	if err != nil {
		return nil, err
	}
	stroked := OutlineStrokes(RemapImage(img, pal), width, stroke)
	var out bytes.Buffer
	if err := png.Encode(&out, stroked); err != nil {
		return nil, fmt.Errorf("compose: outline: encode: %v", err)
	}
	return out.Bytes(), nil
}
```

Note: strconv is not needed — check imports; remove unused ones to keep vet green.

- [ ] **Step 4: Run to verify the tests pass**

Run: `go test ./internal/compose/ -v -run 'TestOutline|TestParseHexColor'`
Expected: PASS.

- [ ] **Step 5: Full green + commit**

Run: `go vet ./... && go test ./...`
Expected: PASS.

```bash
git add internal/compose/outline.go internal/compose/outline_test.go
git commit -m "$(cat <<'EOF'
feat(compose): outline strokes — quantize-before-outline

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: queue outline post-op (element + frame)

**Files:**
- Modify: `internal/queue/compose.go` — `OutlineSpec` type (near `AssetSpecIn`), `AssetSpecIn.Outline` + `assetSpec.Outline`, submit gate (after the pixel_fix gate from Task 3), executor block (after the kind switch, before EmitMeta)
- Test: Modify `internal/queue/compose_postops_test.go` (append; file created in Task 3)

**Interfaces:**
- Consumes (from Task 4, exact): `compose.OutlinePNG(data []byte, width int, colorSpec string) ([]byte, error)`, `compose.ParseHexColor(spec string) (color.NRGBA, error)`.
- Consumes (from Task 3, exact): test file `compose_postops_test.go` (append below its existing tests — its imports already include bytes, image, image/color, image/png, strings; add `"encoding/base64"` only if not already present).
- Produces: `queue.OutlineSpec{Width int, Color string}` with json tags `width,omitempty` / `color,omitempty`; `AssetSpecIn.Outline *OutlineSpec` + `assetSpec.Outline *OutlineSpec`, json `outline,omitempty`; the executor placement contract (outline runs after the kind switch, before EmitMeta / palette-lock / tEXt / save — R10).

- [ ] **Step 1: Write the failing tests**

Append to `internal/queue/compose_postops_test.go`:

```go
func TestComposeOutlineOnRembgElement(t *testing.T) {
	o := composeTestOpts(t, t.TempDir(), nil)
	o.Ops.Generate = pixelGridGen(t, nil, 8, [][3]uint8{{20, 24, 31}})
	// Stand-in rembg: return a crafted frame — 16×16 transparent with a 4×4
	// opaque red block at (8..11)² — so the outline ring is deterministic.
	// (The submit size must be a multiple of 16; the rembg output defines
	// the actual deliverable geometry.)
	rembgImg := func() []byte {
		src := image.NewNRGBA(image.Rect(0, 0, 16, 16))
		for y := 8; y <= 11; y++ {
			for x := 8; x <= 11; x++ {
				src.Set(x, y, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}()
	o.Ops.Rembg = func(_ context.Context, _ string) (string, error) {
		return base64.StdEncoding.EncodeToString(rembgImg), nil
	}
	m := New(o)
	seed := int64(45)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "outl", Seed: &seed,
		Assets: []AssetSpecIn{{
			Kind: "element", Name: "el", Prompt: "cutout", Size: "16x16",
			Transparent: TransparentRembg, // B2 tri-state ("rembg") — not a bool
			Outline:     &OutlineSpec{Width: 1, Color: "#00ff00"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	data, err := m.opts.Store.ReadSetFile(id, "el.png")
	if err != nil {
		t.Fatalf("deliverable: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.NRGBAModel.Convert(img.At(8, 7)); got != (color.NRGBA{R: 0, G: 255, B: 0, A: 255}) {
		t.Fatalf("(8,7) want green stroke, got %v", got)
	}
	if got := color.NRGBAModel.Convert(img.At(7, 7)); got != (color.NRGBA{A: 0}) {
		t.Fatalf("(7,7) want still transparent at width 1, got %v", got)
	}
	if got := color.NRGBAModel.Convert(img.At(9, 9)); got != (color.NRGBA{R: 255, G: 0, B: 0, A: 255}) {
		t.Fatalf("(9,9) want red block, got %v", got)
	}
}

func TestComposeOutlineOpaqueIsNoOp(t *testing.T) {
	o := composeTestOpts(t, t.TempDir(), nil)
	// dual element: both renders fully opaque solid red → UnmixAlpha leaves
	// an opaque deliverable → outline (auto) must be a no-op, not an error.
	o.Ops.Generate = solidGen(t)
	m := New(o)
	seed := int64(45)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "dual", Seed: &seed,
		Assets: []AssetSpecIn{{
			Kind: "element", Name: "el", Prompt: "solid", Size: "16x16", Shape: "dual",
			Outline: &OutlineSpec{Width: 1, Color: "auto"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	data, err := m.opts.Store.ReadSetFile(id, "el.png")
	if err != nil {
		t.Fatalf("deliverable: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	// Precondition: deliverable fully opaque (else this test's premise is off).
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA); c.A < 128 {
				t.Fatalf("precondition: deliverable not opaque at (%d,%d): %v", x, y, c)
			}
		}
	}
	// Outline of an opaque image must reproduce it pixel-for-pixel.
	ag, err := compose.OutlinePNG(data, 1, "auto")
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	aim, err := png.Decode(bytes.NewReader(ag))
	if err != nil {
		t.Fatal(err)
	}
	samePixelData(t, aim, img)
}

func TestSubmitAssetSetOutlineValidation(t *testing.T) {
	m := New(composeTestOpts(t, t.TempDir(), nil))
	seed := int64(1)
	_, err := m.SubmitAssetSet(AssetSetRequest{Name: "bad", Seed: &seed, Assets: []AssetSpecIn{
		{Kind: "atlas", Name: "a", Prompt: "p", Size: "64x64", Frames: 2,
			Outline: &OutlineSpec{Width: 1}},
	}})
	if err == nil || !strings.Contains(err.Error(), "outline is supported for element and frame") {
		t.Fatalf("want outline kind gate, got %v", err)
	}
	_, err = m.SubmitAssetSet(AssetSetRequest{Name: "w", Seed: &seed, Assets: []AssetSpecIn{
		{Kind: "element", Name: "e", Prompt: "p", Size: "16x16",
			Outline: &OutlineSpec{Width: 3}},
	}})
	if err == nil || !strings.Contains(err.Error(), "outline width must be 1..2") {
		t.Fatalf("want width gate, got %v", err)
	}
	_, err = m.SubmitAssetSet(AssetSetRequest{Name: "c", Seed: &seed, Assets: []AssetSpecIn{
		{Kind: "element", Name: "e", Prompt: "p", Size: "16x16",
			Outline: &OutlineSpec{Width: 1, Color: "red"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "bad color") {
		t.Fatalf("want color gate, got %v", err)
	}
}
```

**Check before writing:** `solidGen` and `genCapture` names must match the fakes in `compose_test.go`; if `compose_test.go` has no solid-opaque generator fake, add a local `solidGen(t *testing.T)` in `compose_postops_test.go` producing an opaque `(255,0,0)` PNG at the requested size, mirroring `pixelGridGen`'s signature. The rembg opt-in is the B2 tri-state `AssetSpecIn.Transparent = TransparentRembg` (verify the const's exact name in compose.go before transcribing).

- [ ] **Step 2: Run to verify the tests fail to compile**

Run: `go test ./internal/queue/ -run 'TestComposeOutline|TestSubmitAssetSetOutline'`
Expected: FAIL — `undefined: OutlineSpec`.

- [ ] **Step 3: Wire the type, field, and gate**

In `internal/queue/compose.go`:

1. Define near `AssetSpecIn`:

```go
// OutlineSpec opts an element-like asset into quantize-before-outline
// post-processing (ArcBrush rule: strokes follow palette-cleaned edges).
type OutlineSpec struct {
	Width int    `json:"width,omitempty"` // 1..2; omitted/0 normalizes to 1
	Color string `json:"color,omitempty"` // ""/"auto" = darkest palette entry; else #rgb/#rrggbb
}
```

2. `AssetSpecIn` — after the `PixelFix` line from Task 3:

```go
	Outline        *OutlineSpec    `json:"outline,omitempty"` // element + frame only
```

3. `assetSpec` — after its `PixelFix` line:

```go
	Outline        *OutlineSpec    `json:"outline,omitempty"` // element + frame only
```

4. Submit gate — insert immediately after the pixel_fix gate block (`as.PixelFix = a.PixelFix`), still before the model-seeding loop:

```go
		if a.Outline != nil {
			switch a.Kind {
			case "element", "frame":
			default:
				return "", &ValidationError{Code: "validation_error",
					Msg: fmt.Sprintf("asset %q: outline is supported for element and frame", an)}
			}
			if a.Outline.Width < 0 || a.Outline.Width > 2 {
				return "", &ValidationError{Code: "validation_error",
					Msg: fmt.Sprintf("asset %q: outline width must be 1..2", an)}
			}
			if a.Outline.Width == 0 {
				a.Outline.Width = 1
			}
			switch c := a.Outline.Color; c {
			case "", "auto":
			default:
				if _, cerr := compose.ParseHexColor(c); cerr != nil {
					return "", &ValidationError{Code: "validation_error",
						Msg: fmt.Sprintf("asset %q: %v", an, cerr)}
				}
			}
		}
		as.Outline = a.Outline
```

(Note: Width negatives are rejected with the same 1..2 message; `compose.OutlinePNG` re-validates at execution time, so the gate is defense-in-depth.)

- [ ] **Step 4: Wire the executor block**

In `composeAssetSet`, immediately AFTER the kind switch closes and BEFORE EmitMeta (`if err == nil && len(a.MetaFormats) > 0`), insert:

```go
			// C2 outline post-op (opt-in): quantize-then-stroke so the ring
			// follows palette-cleaned edges. Transparent deliverables gain a
			// hard (1-bit) alpha edge via the shared palette remap.
			if err == nil && a.Outline != nil {
				for j := range outs {
					if !strings.HasSuffix(outs[j].Name, ".png") {
						continue
					}
					nb, oerr := compose.OutlinePNG(outs[j].Data, a.Outline.Width, a.Outline.Color)
					if oerr != nil {
						err = oerr
						break
					}
					outs[j].Data = nb
				}
			}
```

(If the surrounding variable names at the insertion site differ — the outputs slice may not be `outs` — adapt the loop to the actual slice name in place; the placement and semantics are fixed.)

- [ ] **Step 5: Run to verify the tests pass**

Run: `go test ./internal/queue/ -v -run 'TestComposeOutline|TestSubmitAssetSetOutline'`
Expected: PASS.

- [ ] **Step 6: Full green + commit**

Run: `go vet ./... && go test ./...`
Expected: PASS.

```bash
git add internal/queue/compose.go internal/queue/compose_postops_test.go
git commit -m "$(cat <<'EOF'
feat(queue): outline compose post-op (quantize-before-outline)

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: v1 sync endpoint POST /api/v1/repair

**Files:**
- Modify: `cmd/img-gen/v1.go` — registration beside the `/api/v1/generate` handler (~:40 block in registerV1), plus the `compose` import
- Test: Create `cmd/img-gen/v1_repair_test.go`

**Interfaces:**
- Consumes (from Task 2, exact): `compose.RepairPNG(data []byte, cell, colors int) ([]byte, error)`, `compose.RepairDefaultColors` (=32), `compose.RepairMaxDim` (=1024), consts `compose.RepairMinCell`(=2)/`compose.RepairMaxCell`(=64).
- Consumes (repo-existing, exact): `registerV1(mux, h)` handler pattern from v1.go — `mux.HandleFunc("/api/v1/<path>", func(w, r))` + method guard `writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")`; `writeV1Err(w, status, code, message, jobID)` envelope (jobID `""`).
- Produces: the public REST surface `POST /api/v1/repair {image: base64-PNG, cell?: 2..64, colors?: 1..64}` → 200 `image/png` raw bytes; 400 `validation_error` envelope on every bad request — documented in Task 8's openapi/README/MCP sweeps.

- [ ] **Step 1: Write the failing tests**

Create `cmd/img-gen/v1_repair_test.go`:

```go
package main

import (
	"bytes"
	"encoding/base64"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testPixelGrid builds an exact cell-grid PNG of the requested size.
func testPixelGrid(t *testing.T, w, h, cell int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	colors := [][3]uint8{{20, 24, 31}, {232, 93, 4}, {251, 242, 54}}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := colors[((x/cell)+(y/cell)*2)%len(colors)]
			img.Set(x, y, color.RGBA{R: c[0], G: c[1], B: c[2], A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// postV1Repair posts raw JSON and returns status, Content-Type, and body.
func postV1Repair(t *testing.T, srv *httptest.Server, body string) (int, string, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/v1/repair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), raw
}

func mustB64(t *testing.T, data []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(data)
}

func TestV1RepairExplicitCell(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	defer srv.Close()
	in := testPixelGrid(t, 64, 64, 8)
	status, ctype, raw := postV1Repair(t, srv, `{"image":"`+mustB64(t, in)+`","cell":8}`)
	if status != 200 || !strings.HasPrefix(ctype, "image/png") {
		t.Fatalf("status %d ctype %q body %.200s", status, ctype, raw)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("want 8x8, got %dx%d", b.Dx(), b.Dy())
	}
}

func TestV1RepairAutoDetect(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	defer srv.Close()
	in := testPixelGrid(t, 64, 64, 8)
	status, _, raw := postV1Repair(t, srv, `{"image":"`+mustB64(t, in)+`"}`)
	if status != 200 {
		t.Fatalf("status %d body %.200s", status, raw)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("auto-detect want 8x8, got %dx%d", b.Dx(), b.Dy())
	}
}

func TestV1RepairErrors(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	defer srv.Close()
	in := testPixelGrid(t, 64, 64, 8)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing image", `{}`, "image is required"},
		{"bad base64", `{"image":"@@@not-b64@@@"}`, "base64"},
		{"not a PNG", `{"image":"` + mustB64(t, []byte("junk")) + `"}`, "not a PNG"},
		{"too large", `{"image":"` + mustB64(t, testPixelGrid(t, 1056, 16, 4)) + `","cell":4}`, "repair supports up to 1024x1024"},
		{"cell high", `{"image":"` + mustB64(t, in) + `","cell":65}`, "cell must be between 2 and 64"},
		{"cell low", `{"image":"` + mustB64(t, in) + `","cell":1}`, "cell must be between 2 and 64"},
		{"colors high", `{"image":"` + mustB64(t, in) + `","cell":8,"colors":65}`, "colors must be between 1 and 64"},
		{"colors low", `{"image":"` + mustB64(t, in) + `","cell":8,"colors":-1}`, "colors must be between 1 and 64"},
		{"not a grid", `{"image":"` + mustB64(t, v1NoisePNG(t)) + `","cell":8}`, "not a pixel-art grid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, raw := postV1Repair(t, srv, tc.body)
			if status != 400 {
				t.Fatalf("want 400, got %d (%.200s)", status, raw)
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Fatalf("want %q in %s", tc.want, raw)
			}
			if !strings.Contains(string(raw), `"validation_error"`) {
				t.Fatalf("want validation_error code in %s", raw)
			}
		})
	}
}

// v1NoisePNG builds a deterministic non-grid noise PNG for the negative
// detection case.
func v1NoisePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	seed := uint32(1)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			seed = seed*1664525 + 1013904223
			r := uint8((seed >> 16) & 0xff)
			seed = seed*1664525 + 1013904223
			g := uint8((seed >> 16) & 0xff)
			seed = seed*1664525 + 1013904223
			b := uint8((seed >> 16) & 0xff)
			img.Set(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
```

(Handler rule the table encodes: `cell` 0 → auto-detect, outside 2..64 → 400; `colors` 0 → default 32, outside 1..64 → 400.)

- [ ] **Step 2: Run to verify the tests fail to compile / 404**

Run: `go test ./cmd/... -run 'TestV1Repair'`
Expected: FAIL — either compile error (no `postV1Repair`) or 404-once-wired; both are the failing signal.

- [ ] **Step 3: Implement the endpoint**

In `cmd/img-gen/v1.go`, inside `registerV1`, beside the generate registration:

```go
	mux.HandleFunc("/api/v1/repair", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
			return
		}
		var req repairRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
			writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body: "+err.Error(), "")
			return
		}
		if req.Image == "" {
			writeV1Err(w, http.StatusBadRequest, "validation_error", "image is required", "")
			return
		}
		if req.Cell != 0 && (req.Cell < compose.RepairMinCell || req.Cell > compose.RepairMaxCell) {
			writeV1Err(w, http.StatusBadRequest, "validation_error",
				fmt.Sprintf("cell must be between %d and %d", compose.RepairMinCell, compose.RepairMaxCell), "")
			return
		}
		colors := req.Colors
		if colors == 0 {
			colors = compose.RepairDefaultColors
		}
		if colors < 1 || colors > compose.RepairMaxColors {
			writeV1Err(w, http.StatusBadRequest, "validation_error",
				fmt.Sprintf("colors must be between 1 and %d", compose.RepairMaxColors), "")
			return
		}
		raw, err := base64.StdEncoding.DecodeString(req.Image)
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "validation_error", "image is not valid base64", "")
			return
		}
		pre, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "validation_error", "image is not a PNG", "")
			return
		}
		pb := pre.Bounds()
		if pb.Dx() > compose.RepairMaxDim || pb.Dy() > compose.RepairMaxDim {
			writeV1Err(w, http.StatusBadRequest, "validation_error",
				fmt.Sprintf("image is %dx%d; repair supports up to %dx%d", pb.Dx(), pb.Dy(), compose.RepairMaxDim, compose.RepairMaxDim), "")
			return
		}
		out, err := compose.RepairPNG(raw, req.Cell, colors)
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "validation_error", err.Error(), "")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	})
```

And near the top of v1.go (beside the other small request structs):

```go
type repairRequest struct {
	Image  string `json:"image"`
	Cell   int    `json:"cell,omitempty"`
	Colors int    `json:"colors,omitempty"`
}
```

Add imports the file lacks: `"encoding/base64"`, `"image/png"`, `"img-gen/internal/compose"` (check `"bytes"` and `"fmt"` — likely already imported).

- [ ] **Step 4: Run to verify the tests pass**

Run: `go test ./cmd/... -v -run 'TestV1Repair'`
Expected: PASS (auto-detect test relies on `compose.DetectGridCell` from Task 1 with the exact-grid input).

- [ ] **Step 5: Full green + commit**

Run: `go vet ./... && go test ./...`
Expected: PASS.

```bash
git add cmd/img-gen/v1.go cmd/img-gen/v1_repair_test.go
git commit -m "$(cat <<'EOF'
feat(v1): sync pixel-art repair endpoint

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: C3 — set-level coherent palette for atlas frames under palette_lock

**Files:**
- Modify: `internal/queue/compose.go` — `setRenderPalette` closure + pre-assembly frame remap inside the atlas case; if `"image/color"` is not already imported, add it
- Test: Modify `internal/queue/compose_postops_test.go` (append; created in Task 3)

**Interfaces:**
- Consumes (repo-existing, exact): `compose.SetPalette([]image.Image{img}, n) color.Palette`, `compose.RemapPNG(data []byte, pal color.Palette) ([]byte, error)`, `png.Decode`, `m.log(id, format, args...)` (executor log convention), the atlas case's `imgs [][]byte` renders and `spec.PaletteLock` (palette_lock flows through the existing set-level pass; this task adds the pre-assembly pass described below).
- Produces: the C3 behavior itself — when `palette_lock` is set on a set containing an atlas, every atlas frame is remapped against the set palette **before** `compose.ComposeAtlas` assembles the frames; the post-assembly palette pass (existing, Phase B task 2) still runs and governs everything in-palette.

**C3 mechanics (ruling R6):** the set palette is computed ONCE from the first DECODABLE done render among the set's children, in child order (`compose.SetPalette([img], 255)`) — seeded inside the per-child result collection loop, so a set with an element before the atlas locks onto the element's render (the set's render 0), and a first-asset atlas locks onto frame 0 (its first child) rather than swallowing frames 1..N's colors. Per-frame `RemapPNG` failure is logged via `m.log` and skipped — it must never break the asset. Without `palette_lock` none of this runs (R8: deliberate carve-out from the byte-for-byte defaults rule — spec-mandated behavior change for opted-in sets).

**Why this is needed (load-bearing reasoning for the reviewer):** the existing post-assembly palette pass computes its palette over ALL final outputs — including a deviant frame — so a magenta frame contributes magenta to the post palette and survives it. Only a pre-assembly remap against a palette derived from the coherent set (frame 0's render) removes the deviant colors. The test below proves exactly that.

- [ ] **Step 1: Write the failing test**

Append to `internal/queue/compose_postops_test.go`:

```go
// frameVaryingGen renders per-frame-varying block art: the frame index
// (seed - base) selects a palette rotation, and frame 2 additionally paints
// its top-left cell magenta. Frames are 2×2 blocks of quarter-size cells, so
// every frame decomposes on the same 3-color grid.
func frameVaryingGen(t *testing.T, base int64) func(context.Context, string, string, *storage.ControlParams, *storage.IPAdapterParams, storage.ModelSpec, storage.SamplingParams) ([]byte, *int64, error) {
	t.Helper()
	return func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		ws, hs, _ := strings.Cut(size, "x")
		w, _ := strconv.Atoi(ws)
		h, _ := strconv.Atoi(hs)
		if w < 1 || h < 1 {
			w, h = 8, 8
		}
		frame := int(sp.Seed - base)
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		cyan := color.RGBA{R: 0, G: 255, B: 255, A: 255}
		black := color.RGBA{A: 255}
		white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
		magenta := color.RGBA{R: 255, G: 0, B: 255, A: 255}
		for y := 0; y < h; y += h / 2 {
			for x := 0; x < w; x += w / 2 {
				var c color.RGBA
				switch (x/(w/2) + y/(h/2)) % 3 {
				case 0:
					c = cyan
				case 1:
					c = black
				default:
					c = white
				}
				if frame == 2 && x < w/2 && y < h/2 {
					c = magenta // the deviant only frame 2 paints
				}
				for yy := y; yy < y+h/2; yy++ {
					for xx := x; xx < x+w/2; xx++ {
						img.Set(xx, yy, c)
					}
				}
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes(), sp.Seed, nil
	}
}

func hasMagenta(t *testing.T, data []byte) bool {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 == 255 && g>>8 == 0 && bl>>8 == 255 {
				return true
			}
		}
	}
	return false
}

func TestAtlasSetPaletteCoherent(t *testing.T) {
	seed := int64(70)
	o := composeTestOpts(t, t.TempDir(), nil)
	o.Ops.Generate = frameVaryingGen(t, seed)
	m := New(o)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "coh", Seed: &seed, PaletteLock: true,
		Assets: []AssetSpecIn{{Kind: "atlas", Name: "sheet", Prompt: "cycle", Size: "32x32", Frames: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	data, err := m.opts.Store.ReadSetFile(id, "sheet-sheet.png")
	if err != nil {
		t.Fatalf("sheet: %v", err)
	}
	if hasMagenta(t, data) {
		t.Fatal("magenta survived under palette_lock — C3 pre-assembly remap missing")
	}

	// Control: without palette_lock the old behavior keeps magenta.
	o2 := composeTestOpts(t, t.TempDir(), nil)
	o2.Ops.Generate = frameVaryingGen(t, seed)
	m2 := New(o2)
	id2, err := m2.SubmitAssetSet(AssetSetRequest{
		Name: "coh2", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "atlas", Name: "sheet", Prompt: "cycle", Size: "32x32", Frames: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m2, id2, "done")
	data2, err := m2.opts.Store.ReadSetFile(id2, "sheet-sheet.png")
	if err != nil {
		t.Fatalf("sheet2: %v", err)
	}
	if !hasMagenta(t, data2) {
		t.Fatal("magenta vanished without palette_lock — C3 leaked into defaults (R8 violation)")
	}
}
```

**Check before writing:** `AtlasSetPaletteCoherent`'s frames convention — the atlas kind renders each frame as its own child job; the executor passes each child `sp.Seed`, advancing per frame (Phase B's compose_test shows the exact progression; align `frame` = `sp.Seed - seed` with the real offset — the plan's frame-2 marker follows). Deliverable naming is already confirmed: `ComposeAtlas` emits `<name>-sheet.png`, so the atlas named "sheet" yields `sheet-sheet.png`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/queue/ -run 'TestAtlasSetPaletteCoherent'`
Expected: FAIL — "magenta survived under palette_lock" (the pre-assembly remap does not exist yet).

- [ ] **Step 3: Implement**

In `internal/queue/compose.go`:

1. Declare the executor-level palette state beside `outsByAsset` (top of `composeAssetSet`):

```go
	var setPal color.Palette
	setPalLocked := false
```

2. Inside the per-asset loop, at the top of the `if broken == "" {` block — BEFORE the pixel-fix block from Task 3, out of the per-child scope (where `m.childResult`'s local return shadows the `image/png` package name) — seed the set palette once:

```go
			// C3: the coherent-palette source is the first decodable render
			// among the set's children in order (render 0 of the set).
			if spec.PaletteLock && !setPalLocked {
				for _, data := range imgs {
					if sim, derr := png.Decode(bytes.NewReader(data)); derr == nil {
						setPal = compose.SetPalette([]image.Image{sim}, 255)
						setPalLocked = true
						break
					}
				}
			}
```

3. Inside the atlas case (`case "atlas":` at ~:802-803), immediately before the `compose.ComposeAtlas` call:

```go
				if spec.PaletteLock && setPalLocked {
					for i := range imgs {
						nb, rerr := compose.RemapPNG(imgs[i], setPal)
						if rerr != nil {
							m.log(id, "set palette: frame %d left unremapped: %v", i, rerr)
							continue
						}
						imgs[i] = nb
					}
				}
```

(When `setPalLocked` is false — no decodable render anywhere — the atlas passes through unchanged; a one-line `m.log(id, "set palette: no decodable render; atlas left unremapped")` in that branch is welcome but optional. The npc kind's fall-back `ComposeAtlas` call is NOT covered: variant packing composes different sprites by design, not coherent frames.)

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/queue/ -v -run 'TestAtlasSetPaletteCoherent'`
Expected: PASS (both branches).

- [ ] **Step 5: Full green + commit**

Run: `go vet ./... && go test ./...`
Expected: PASS — in particular `TestSubmitAssetSetPaletteLock` (the Phase B test with an atlas under palette_lock) stays green: its frames render on one checker generator, so its palette is coherent and the pre-assembly remap is idempotent onto the same colors.

```bash
git add internal/queue/compose.go internal/queue/compose_postops_test.go
git commit -m "$(cat <<'EOF'
feat(queue): set-level coherent palette for atlas frames under palette_lock

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: docs sweep, goldens, full verification + smoke

**Files:**
- Modify: `docs/openapi.yaml` (AssetSpecIn properties ~:304-376; asset-set path ~:721; new components schema), `README.md` (new subsection near the compose post-ops docs), `cmd/img-gen-mcp/main.go` (`request_asset_set` schema string ~:105)
- Create: `internal/compose/testdata/repair/input.png` (copied, real artifact), `internal/compose/testdata/repair/palette.json`, `quantized.png`, `downscaled.png` (generated once — see below)
- Test: Create `internal/compose/golden_repair_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-7 (final state of the branch).
- Produces: the documented public surface (REST + MCP + openapi) and the committed golden fixtures the spec's testing bullet requires ("golden before/after pairs for one real artifact per repair stage").

- [ ] **Step 1: openapi.yaml**

In `docs/openapi.yaml`:

1. `AssetSpecIn` schema — after the `meta_formats` property, add:

```yaml
        pixel_fix:
          type: boolean
          default: false
          description: >-
            Opt-in pixel-art repair post-op: quantize every render onto its
            dominant palette (top-64, CIEDE2000-merged) and downscale by the
            detected grid cell before delivery. Runs before the rembg step.
            Element and atlas assets only; sizes up to 1024x1024.
        outline:
          nullable: true
          description: >-
            Opt-in outline post-op on element and frame assets: quantize,
            then stroke fully-transparent pixels adjacent to opaque ones.
            Strokes follow palette-cleaned edges.
          additionalProperties: false
          properties:
            width:
              type: integer
              minimum: 1
              maximum: 2
              description: Stroke width in pixels; omitted or 0 means 1.
            color:
              type: string
              description: '#rgb / #rrggbb, or empty / auto for the darkest palette color.'
```

2. New path beside the other `/api/v1` paths:

```yaml
  /api/v1/repair:
    post:
      summary: Repair a pixel-art image (sync)
      description: >-
        Detects the grid cell of a pixel-art render (or accepts an explicit
        cell), quantizes it onto a dominant palette, and downscales by the
        cell with center sampling. Synchronous; up to 1024x1024 input.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/RepairRequest'
      responses:
        '200':
          description: Repaired PNG
          content:
            image/png:
              schema:
                type: string
                format: binary
        '400':
          $ref: '#/components/responses/ValidationError'
```

(If `components/responses/ValidationError` does not exist in the file, use the existing error-response pattern used by the generate endpoint instead.)

3. New components schema:

```yaml
    RepairRequest:
      type: object
      required: [image]
      properties:
        image:
          type: string
          format: byte
          description: base64-encoded PNG of the pixel-art render (up to 1024x1024).
        cell:
          type: integer
          minimum: 2
          maximum: 64
          description: Explicit grid cell in pixels; omit to auto-detect (fails with 'not a pixel-art grid' when the signal is weak).
        colors:
          type: integer
          minimum: 1
          maximum: 64
          default: 32
          description: Number of dominant palette colors to quantize onto.
```

- [ ] **Step 2: README subsection**

In `README.md`, find the existing compose post-ops / asset-set section and append:

```markdown
### Pixel-art repair and outline

Phase C adds pixel post-processing, all in Go inside the compose pipeline:

- `POST /api/v1/repair` — synchronous repair of one base64 PNG. Pass
  `cell` (2..64) to pin the grid cell or omit it to auto-detect (the
  endpoint errors with `not a pixel-art grid` on noisy, non-sprite input),
  and `colors` (1..64, default 32) for the quantization palette size.
  Returns the repaired PNG (quantized + downscaled by the cell).
- Asset post-ops on `POST /api/v1/assets` elements:
  - `"pixel_fix": true` on **element** and **atlas** assets — every render
    is repaired (auto-detected cell, top-32 palette) *before* the rembg
    step, so background removal and any later step see clean sprite grids.
    Sizes up to 1024x1024.
  - `"outline": {"width": 1..2, "color": "auto" | "#hex"}` on **element**
    and **frame** assets — quantize-then-outline: strokes follow the
    palette-cleaned edges. `auto` picks the darkest palette color; an
    explicit hex overrides. A fully-opaque deliverable takes no stroke.
- Palette-coherent atlas frames: when `palette_lock` is set on a set that
  contains an atlas, every frame is remapped against the set's palette
  (computed from the first decodable render) *before* the frames are
  assembled, so off-palette frame variants cannot leak stray colors into
  the sheet.

Default behavior without these fields is unchanged.
```

- [ ] **Step 3: MCP tool schema**

In `cmd/img-gen-mcp/main.go`, extend the `request_asset_set` input-schema JSON string's element/asset property block with:

```json
"pixel_fix": {"type": "boolean", "default": false, "description": "pixel-art repair: quantize + downscale by detected cell before delivery (element and atlas only, up to 1024x1024)"},
"outline": {"type": "object", "nullable": true, "properties": {"width": {"type": "integer", "minimum": 1, "maximum": 2}, "color": {"type": "string", "description": "#hex or auto (darkest palette color)"}}, "description": "quantize-then-outline strokes on element and frame assets"}
```

(match the file's existing JSON-in-string formatting and property ordering).

- [ ] **Step 4: Real-artifact goldens**

1. Copy the real artifact into the package:

```bash
mkdir -p internal/compose/testdata/repair
cp .claude/mock-site-bb/assets/circuit-cap-n.png internal/compose/testdata/repair/input.png
```

(If `.claude/mock-site-bb/assets/circuit-cap-n.png` is gone from the main checkout's tree — it is gitignored except its assets — the copy must come from the main checkout at `/Users/<you>/Projects/img-gen/.claude/mock-site-bb/assets/circuit-cap-n.png`; verify `-s` first. If that too is gone, pick another tracked real render under `internal/compose/testdata/` with sprite-like structure instead and record the substitution in the report.)

2. Generate the golden outputs with a throwaway program **in the SDD workspace** (NOT committed) — `go run` it from the worktree root:

```go
// main.go for the throwaway golden generator (go run ./<workspace path>)
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"

	"img-gen/internal/compose"
)

func main() {
	raw, err := os.ReadFile("internal/compose/testdata/repair/input.png")
	if err != nil {
		panic(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		panic(err)
	}
	b := img.Bounds()
	fmt.Printf("dims=%dx%d\n", b.Dx(), b.Dy())
	cell, derr := compose.DetectGridCell(img)
	fmt.Printf("detect: cell=%d err=%v\n", cell, derr)
	if derr != nil {
		// Fallback ladder: fixed cells until one is divisible.
		for _, c := range []int{8, 16} {
			if b.Dx()%c == 0 && b.Dy()%c == 0 {
				cell = c
				derr = nil
				break
			}
		}
		if derr != nil {
			panic("input fits neither detected nor fixed cells; substitute the tile per the task note")
		}
	}
	pal := compose.SetPalette([]image.Image{img}, 32)
	pj, err := compose.MarshalPalette(pal)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("internal/compose/testdata/repair/palette.json", pj, 0o644); err != nil {
		panic(err)
	}
	q := compose.RemapImage(img, pal)
	f, err := os.Create("internal/compose/testdata/repair/quantized.png")
	if err != nil {
		panic(err)
	}
	if err := png.Encode(f, q); err != nil {
		panic(err)
	}
	f.Close()
	down, err := compose.RepairImage(raw, cell, 32)
	if err != nil {
		panic(err)
	}
	f2, err := os.Create("internal/compose/testdata/repair/downscaled.png")
	if err != nil {
		panic(err)
	}
	if err := png.Encode(f2, down); err != nil {
		panic(err)
	}
	f2.Close()
	fmt.Printf("cell=%d palette=%d colors\n", cell, len(pal))
}
```

(Record the detected cell + dims the program prints in `task-8-report.md`; the program itself stays out of git.)

3. Committed golden test — `internal/compose/golden_repair_test.go`:

```go
package compose

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"testing"
)

// TestRepairGoldenCircuitCap recomputes the repair stages for the committed
// real artifact and asserts byte equality with the committed goldens.
// Provenance: input.png is circuit-cap-n.png from the mock-site render run
// (see the task report that committed it).
func TestRepairGoldenCircuitCap(t *testing.T) {
	raw, err := os.ReadFile("testdata/repair/input.png")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	cell, derr := DetectGridCell(img)
	if derr != nil {
		t.Fatalf("input no longer detects (regenerated goldens would drift): %v", derr)
	}
	pal := SetPalette([]image.Image{img}, 32)

	// Stage 1 golden: quantized buffer (pre-downscale).
	var qbuf bytes.Buffer
	if err := png.Encode(&qbuf, RemapImage(img, pal)); err != nil {
		t.Fatal(err)
	}
	qc, err := os.ReadFile("testdata/repair/quantized.png")
	if err != nil {
		t.Fatalf("quantized golden missing: %v", err)
	}
	if !bytes.Equal(qbuf.Bytes(), qc) {
		t.Fatal("quantized golden drifted")
	}

	// Stage 2 golden: palette envelope.
	pj, err := MarshalPalette(pal)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := os.ReadFile("testdata/repair/palette.json")
	if err != nil {
		t.Fatalf("palette golden missing: %v", err)
	}
	if !bytes.Equal(pj, pc) {
		t.Fatal("palette golden drifted")
	}

	// Stage 3 golden: full repair (quantize + downscale by the detected cell).
	qfull, err := RepairPNG(raw, cell, 32)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	dc, err := os.ReadFile("testdata/repair/downscaled.png")
	if err != nil {
		t.Fatalf("downscaled golden missing: %v", err)
	}
	if !bytes.Equal(qfull, dc) {
		t.Fatal("downscaled golden drifted")
	}
}
```

Golden rule: goldens are produced ONLY by the exact public functions the test recomputes with (SetPalette/RemapImage/MarshalPalette/RepairPNG at these same argument values); byte equality is the assertion.

- [ ] **Step 5: Full verification**

Run: `go vet ./... && go test ./... && go build ./...`
Expected: PASS — every existing suite green, including the Phase B atlas/palette-lock suites and Phase C suites.

- [ ] **Step 6: Smoke on real input (report only)**

Repair the committed real artifact through `composes.RepairPNG`-equivalent via the v1 endpoint of a locally-booted test server (or the golden generator's output): record before/after dims, unique-color count (before: jittered artifact count; after: ≤ 32), and byte sizes in `task-8-report.md`. No live instance is touched (they are out of scope for this worktree).

```bash
git add docs/openapi.yaml README.md cmd/img-gen-mcp/main.go internal/compose/testdata/repair internal/compose/golden_repair_test.go
git commit -m "$(cat <<'EOF'
docs: phase C surface — pixel_fix, outline, repair endpoint, C3 + goldens

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Verification notes for the SDD controller

- Tasks 1→2 share `internal/compose/pixelart.go` / `pixelart_test.go` sequentially (T2 extends T1's file, reads its helpers). Tasks 3, 5, 7 all touch `internal/queue/compose.go` and append to `internal/queue/compose_postops_test.go` — strictly sequential in task order; T5's tests append below T3's.
- T2's `RepairPNG`/`RepairImage` feed T3 and T6; T4's `OutlinePNG`/`ParseHexColor` feed T5. Interfaces blocks above chain these names verbatim.
- Rulings R1-R10 (locked above) are ledgered at SDD start; if execution hits a conflict, rule + ledger, keep going.
- Global Constraints restate: Go stdlib only; green `go vet ./...` + `go test ./...`; byte-for-byte defaults except R8's carve-out; honest-failure manifests; commits end with the Claude trailer.