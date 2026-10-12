# Agent-Facing Asset Generation & Animated GIF — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give coding agents one correlated call that generates named, style-locked image assets (UI elements, wang-style corner/junction kits, sprite-sheet atlases) and animated GIFs, delivered as real files into their project.

**Architecture:** A new `internal/compose` package holds the pure image work (GIF encode with a shared median-cut palette + inter-frame transparency, RGBA unmixing for transparency, grid atlas packing, the 20-class wang kit table). The queue gains a `compose` job mode whose parent never occupies the single worker slot while its `asset` children generate; the last child to reach a terminal state re-enqueues the parent, which then composes the deliverables into `DATA_DIR/sets/{setID}/`, writes a manifest, and delivers to `dest`. Two new v1 endpoints (`/api/v1/asset-sets`, `/api/v1/gifs` — plural, matching the existing `/api/v1/jobs` collection convention; the spec's prose says `asset-set`/`gif` singular and was renamed to match) plus set-read endpoints expose it; three MCP tools and two packaged skills make it callable by agents.

**Tech Stack:** Go 1.24, stdlib only (`image`, `image/gif`, `image/png`, `image/draw`, `encoding/json`); optional `gif2webp` (libwebp) on PATH for WebP variants.

**Spec:** `docs/superpowers/specs/2026-10-12-agent-assets-gif-design.md` — Phases 2 (sidecar tileability, `tile` kind) and 3 (RIFE keyframes, rembg, LayerDiffuse) are OUT of scope here and get their own plans.

## Global Constraints

- Go 1.24, **zero third-party dependencies** (`go.mod` is `module img-gen`; keep it that way). Stdlib and local shell-outs only.
- Legacy `/api/*` handlers in `cmd/img-gen/main.go` stay byte-identical. Everything new lives in `internal/*` and `cmd/img-gen/v1.go` (+ `registerV1`).
- All HTTP errors use the v1 envelope (`writeV1Err`); envelope codes: `validation_error` (submit-time limits), `not_implemented` (phase-gated `tile`/`keyframes`), `job_not_found`, `sidecar_timeout`, `invalid_size`. `compose_error` is **not** an envelope code — composite failures fail the job asynchronously, so they surface as 200 job JSON whose `error` field text is prefixed `compose_error:`.
- Existing `queue.ValidationError` msgs must not change; new code adds new ones freely.
- Hard limits (spec): assets per set ≤ 8; total generation children per parent ≤ 64; GIF/frames count 2..64; atlas frames 2..64; `delay_ms` 10..5000 (default 100); `loop` ≥ 0 (0 = forever); asset sizes must be valid `WxH` **and** multiples of 16 (both engines tile latents at 16px).
- Seed derivations (exact): kit class seed = `seed + KitClass.SeedOffset`; atlas frame seed = `seed + frameIndex`; GIF `from_spec` frame seed = `seed + i`. `generation.seed` is REQUIRED for asset sets and `from_spec` GIFs.
- Transparent elements are 2 generation jobs (same base seed, prompt suffixed `", on a plain solid white background"` / `", on a plain solid black background"`).
- Partial success: a set completes if ANY asset succeeded; per-asset failures are recorded in `manifest.json`.
- Delivery into `dest` refuses collisions (destination file exists → compose fails with `compose_error`); internal `SaveSetFile` may overwrite (restart re-compose works).
- Compose parents are never persisted with a worker slot: children re-enqueue the parent when the last child turns terminal.
- `kind: tile` is rejected with a message pointing at phase 2; `keyframes` source rejected pointing at phase 3 (`RIFE_URL`).
- Commit style: conventional prefix (`feat:`, `test:`, `docs:`), each commit ends with `Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>`.

## File Structure

| File | New/Modify | Responsibility |
|---|---|---|
| `internal/compose/gif.go` | Create | `EncodeGIF`: shared median-cut palette, inter-frame transparency, NETSCAPE loop |
| `internal/compose/alpha.go` | Create | `UnmixAlpha`: dual-render RGBA recovery |
| `internal/compose/atlas.go` | Create | `PackGrid`, `AtlasFrame`, `MarshalAtlas` (TexturePacker JSON-Hash) |
| `internal/compose/wang.go` | Create | `KitClass`, `KitClasses` (fixed 20-class table with stable seed offsets + prompt hints) |
| `internal/compose/compose.go` | Create | `OutputFile`, `ComposeElement`, `ComposeKit`, `ComposeAtlas`, `ComposeGIF`, `WebPAvailable`, `EncodeWebP` |
| `internal/compose/compose_test.go` | Create | All compose package tests |
| `internal/storage/storage.go` | Modify | `FileRef`; `Job.SetID/Children/Files/ComposeSpec/ComposeParent`; set-dir read/write/remove/deliver; `ReadImage`, `ReadDataFile` |
| `internal/storage/storage_test.go` | Modify | Set-dir tests |
| `internal/queue/compose.go` | Create | `SubmitAssetSet`, `SubmitGIF`, `submitComposeParent`, `runCompose` + executors, child bookkeeping, restore/cancel wiring |
| `internal/queue/queue.go` | Modify | `asset` mode in `submitOne`; `compose` short-circuit in `run`; `finish`→`childFinished` hook; `Cancel`/`Delete` compose branches; `Options.ExportDir` |
| `internal/queue/compose_test.go` | Create | Queue-level compose tests |
| `cmd/img-gen/v1.go` | Modify | `/api/v1/asset-sets`, `/api/v1/gifs`, `/api/v1/sets/...` handlers |
| `cmd/img-gen/main.go` | Modify | Wire `Options.ExportDir` (one line) |
| `cmd/img-gen/v1_compose_test.go` | Create | Endpoint tests |
| `cmd/img-gen-mcp/main.go` | Modify | `request_asset_set`, `make_gif`, `read_export` tools + schemas |
| `cmd/img-gen-mcp/main_test.go` | Modify | Tool tests |
| `docs/openapi.yaml` | Modify | New paths + schemas |
| `README.md` | Modify | Agent-workflow section |
| `skills/img-gen-assets/SKILL.md`, `skills/img-gen-animation/SKILL.md` | Create | Packaged Claude/agent skills |
| `.opencode/commands/img-gen-assets.md`, `.opencode/commands/img-gen-animation.md` | Create | OpenCode command docs |
| `deploy/install-skills.sh` | Create | Copies both skills into an agent's skills dir |

Task order = dependency order: pure compose package (1–4), storage (5), queue child mode (6), coordinator (7), set submission + execution (8), GIF submission + execution (9), HTTP (10–11), MCP (12), docs (13).

---

### Task 1: `internal/compose` — GIF encoder

**Files:**
- Create: `internal/compose/gif.go`
- Test: `internal/compose/compose_test.go`

**Interfaces:**
- Consumes: stdlib `image`, `image/gif`, `image/draw`.
- Produces: `EncodeGIF(frames []image.Image, delayMS, loop int) ([]byte, error)` — all frames must share frame 0's bounds; `delayMS` quantized to 1/100s (min 2cs); `loop` 0 = forever. Palette: one global median-cut palette across all frames, entry 0 reserved for the transparent index; pixels unchanged from the effective view of the previous frame are emitted as index 0 (inter-frame transparency), so frame N only stores its delta.

- [ ] **Step 1: Write the failing tests**

```go
package compose

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

func solid(w, h int, c color.RGBA) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: c.A})
		}
	}
	return img
}

func TestEncodeGIFBasic(t *testing.T) {
	red := color.RGBA{220, 40, 40, 255}
	blue := color.RGBA{40, 40, 220, 255}
	out, err := EncodeGIF([]image.Image{solid(16, 16, red), solid(16, 16, blue)}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Image) != 2 || len(g.Delay) != 2 {
		t.Fatalf("want 2 frames, got %d", len(g.Image))
	}
	if g.Delay[0] != 10 || g.Delay[1] != 10 {
		t.Fatalf("want delay 10cs, got %v", g.Delay)
	}
	if g.LoopCount != 0 {
		t.Fatalf("want loop forever (0), got %d", g.LoopCount)
	}
}

func TestEncodeGIFTransparentDelta(t *testing.T) {
	red := color.RGBA{220, 40, 40, 255}
	blue := color.RGBA{40, 40, 220, 255}
	out, err := EncodeGIF([]image.Image{solid(16, 16, red), solid(16, 16, red), solid(16, 16, blue)}, 40, 3)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if g.Delay[0] != 4 || g.LoopCount != 3 {
		t.Fatalf("delay/loop: %v/%d", g.Delay, g.LoopCount)
	}
	// Frame 1 is identical to frame 0: every pixel must be the transparent index.
	holes := 0
	for _, v := range g.Image[1].Pix {
		if v == 0 {
			holes++
		}
	}
	if holes != len(g.Image[1].Pix) {
		t.Fatalf("identical frame should be all-transparent: %d/%d holes", holes, len(g.Image[1].Pix))
	}
}

func TestEncodeGIFSizeMismatch(t *testing.T) {
	red := color.RGBA{220, 40, 40, 255}
	if _, err := EncodeGIF([]image.Image{solid(16, 16, red), solid(8, 8, red)}, 100, 0); err == nil {
		t.Fatal("want error for mismatched frame sizes")
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/compose/ -v`
Expected: FAIL — `undefined: EncodeGIF` (the new test file is the package's only file so far, so compilation stops on the missing symbol before any assertion runs).

- [ ] **Step 3: Implement `gif.go`**

```go
package compose

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
)

// EncodeGIF assembles frames into an animated GIF: one global median-cut
// palette shared by every frame (consistent colors across the animation),
// entry 0 reserved as the transparent index, and inter-frame optimization —
// any pixel whose color equals what the decoder already shows (the previous
// frame's effective view) is stored as the transparent index instead of being
// re-emitted. loop 0 means forever; GIF loop semantics are "extra repeats
// after the first pass".
func EncodeGIF(frames []image.Image, delayMS, loop int) ([]byte, error) {
	if len(frames) == 0 {
		return nil, errors.New("compose: no frames")
	}
	delay := delayMS / 10
	if delay < 2 {
		delay = 2
	}
	b0 := frames[0].Bounds()
	for i, f := range frames {
		if f.Bounds() != b0 {
			return nil, fmt.Errorf("compose: frame %d bounds %v do not match frame 0 %v", i, f.Bounds(), b0)
		}
	}
	pal := sharedPalette(frames, 255)
	type plane struct {
		img *image.Paletted
		eff []byte // effective view: the color a decoder shows per pixel
	}
	planes := make([]plane, len(frames))
	for i, f := range frames {
		p := image.NewPaletted(b0, pal)
		draw.Draw(p, b0, f, b0.Min, draw.Src) // draw.Src quantizes to the dst palette
		if i > 0 {
			prev := planes[i-1].eff
			for n := range p.Pix {
				if p.Pix[n] == prev[n] {
					p.Pix[n] = 0 // unchanged: let the previous frame show through
				}
			}
		}
		eff := make([]byte, len(p.Pix))
		copy(eff, p.Pix)
		if i > 0 {
			prev := planes[i-1].eff
			for n := range eff {
				if eff[n] == 0 {
					eff[n] = prev[n]
				}
			}
		}
		planes[i] = plane{img: p, eff: eff}
	}
	g := &gif.GIF{
		Image:     make([]*image.Paletted, len(planes)),
		Delay:     make([]int, len(planes)),
		LoopCount: loop,
		Config: image.Config{
			ColorModel: color.Palette(pal),
			Width:      b0.Dx(),
			Height:     b0.Dy(),
		},
	}
	for i, p := range planes {
		g.Image[i] = p.img
		g.Delay[i] = delay
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sharedPalette builds a fixed-stride histogram across every frame and median
// cuts it into at most n colors (plus the reserved transparent entry 0 that
// callers prepend elsewhere — this function returns opaque colors only, and
// EncodeGIF places them at index >= 1).
func sharedPalette(frames []image.Image, n int) color.Palette {
	type px struct{ r, g, b uint8 }
	counts := map[px]int{}
	for _, f := range frames {
		b := f.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y += 2 {
			for x := b.Min.X; x < b.Max.X; x += 2 {
				r, g, bl, a := f.At(x, y).RGBA()
				if a < 128<<8 {
					continue
				}
				counts[px{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)}]++
			}
		}
	}
	type entry struct {
		p     px
		count int
	}
	var all []entry
	for k, v := range counts {
		all = append(all, entry{k, v})
	}
	if len(all) == 0 {
		all = append(all, entry{{255, 255, 255}, 1})
	}
	boxes := [][]entry{all}
	for len(boxes) < n {
		bi, bc, best := -1, 0, 0
		for i, b := range boxes {
			if len(b) < 2 {
				continue
			}
			var lo, hi px
			lo, hi = b[0].p, b[0].p
			for _, e := range b {
				lo.r = min(lo.r, e.p.r)
				hi.r = max(hi.r, e.p.r)
				lo.g = min(lo.g, e.p.g)
				hi.g = max(hi.g, e.p.g)
				lo.b = min(lo.b, e.p.b)
				hi.b = max(hi.b, e.p.b)
			}
			ch, span := 0, int(hi.r)-int(lo.r)
			if int(hi.g)-int(lo.g) > span {
				ch, span = 1, int(hi.g)-int(lo.g)
			}
			if int(hi.b)-int(lo.b) > span {
				ch, span = 2, int(hi.b)-int(lo.b)
			}
			if span > best {
				best, bi, bc = span, i, ch
			}
		}
		if bi < 0 {
			break
		}
		b := boxes[bi]
		switch bc {
		case 0:
			sortEntries(b, func(e entry) int { return int(e.p.r) })
		case 1:
			sortEntries(b, func(e entry) int { return int(e.p.g) })
		default:
			sortEntries(b, func(e entry) int { return int(e.p.b) })
		}
		mid := len(b) / 2
		boxes[bi] = b[:mid]
		boxes = append(boxes, b[mid:])
	}
	pal := make(color.Palette, 0, n)
	pal = append(pal, color.RGBA{0, 0, 0, 0}) // index 0: reserved transparent
	for _, b := range boxes {
		if len(b) == 0 {
			continue
		}
		var r, g, bl, w int
		for _, e := range b {
			r += int(e.p.r) * e.count
			g += int(e.p.g) * e.count
			bl += int(e.p.b) * e.count
			w += e.count
		}
		pal = append(pal, color.RGBA{R: uint8(r / w), G: uint8(g / w), B: uint8(bl / w), A: 255})
	}
	return pal
}

func sortEntries(b []entry, key func(entry) int) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && key(b[j]) < key(b[j-1]); j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}
```

Note: `sharedPalette` returns the palette **with** index 0 already the transparent entry; the median-cut colors land at index ≥ 1, so the cut target stays `255` even though `make(color.Palette, ...)` starts from the reserve entry.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/compose/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/gif.go internal/compose/compose_test.go
git commit -m "feat(compose): shared-palette animated GIF encoder with inter-frame transparency

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 2: `internal/compose` — grid atlas packing

**Files:**
- Create: `internal/compose/atlas.go`
- Test: `internal/compose/compose_test.go` (append)

**Interfaces:**
- Consumes: stdlib `image/draw`, `encoding/json`.
- Produces:
  - `type Rect struct { X, Y, W, H int }` with `{x,y,w,h}` JSON tags.
  - `type AtlasFrame struct { Name string; Frame Rect }` with `name`/`frame` tags.
  - `PackGrid(name string, frames []image.Image, columns int) ([]byte, []AtlasFrame, error)` — returns the packed sheet as PNG bytes plus per-frame metadata. Cell size = max frame size; frames are centered in their cell; frame `i` goes to row `i/columns`, column `i%columns`. Frame names are `<name>-0000`… 4-digit padded.
  - `MarshalAtlas(metas []AtlasFrame) ([]byte, error)` — TexturePacker JSON-Hash: `{"frames": {"<name>": {"frame": {"x":..,"y":..,"w":..,"h":..}}}, "meta": {"app": "img-gen compose"}}`.

- [ ] **Step 1: Write the failing tests** (append to `compose_test.go`, add `"image/png"` import)

```go
func TestPackGrid(t *testing.T) {
	red := solid(8, 4, color.RGBA{220, 40, 40, 255})
	blue := solid(4, 4, color.RGBA{40, 40, 220, 255})
	sheet, metas, err := PackGrid("spark", []image.Image{red, blue}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("want 2 metas, got %d", len(metas))
	}
	img, err := png.Decode(bytes.NewReader(sheet))
	if err != nil {
		t.Fatalf("sheet is not a PNG: %v", err)
	}
	if img.Bounds().Dx() != 8 || img.Bounds().Dy() != 8 {
		t.Fatalf("want 8x8 sheet from 8x4 cell, got %v", img.Bounds())
	}
	if metas[0].Name != "spark-0000" || metas[0].Frame.W != 8 || metas[0].Frame.Y != 0 {
		t.Fatalf("meta0: %+v", metas[0])
	}
	if metas[1].Name != "spark-0001" || metas[1].Frame.Y != 4 || metas[1].Frame.W != 8 {
		t.Fatalf("meta1: %+v", metas[1])
	}
	// cell contents: blue centered horizontally in the 8-wide cell
	dr, _, _, _ := img.At(1, 5).RGBA()
	if dr != 40<<8 {
		t.Fatalf("blue frame not placed as expected")
	}
}

func TestMarshalAtlasShape(t *testing.T) {
	b, err := MarshalAtlas([]AtlasFrame{{Name: "spark-0000", Frame: Rect{0, 0, 8, 4}}})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Frames["spark-0000"].Frame.W != 8 || doc.Meta["app"] != "img-gen compose" {
		t.Fatalf("unexpected doc: %s", b)
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/compose/ -run 'TestPackGrid|TestMarshalAtlas' -v`
Expected: FAIL — `undefined: PackGrid`.

- [ ] **Step 3: Implement `atlas.go`**

```go
package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type AtlasFrame struct {
	Name  string `json:"name"`
	Frame Rect   `json:"frame"`
}

// PackGrid lays frames out on a uniform grid sheet. The cell is the max frame
// size and each frame is centered inside its cell, so TexturePacker-style
// consumers can slice by rectangle and game engines get stable cell geometry.
// columns <= 0 means "one row".
func PackGrid(name string, frames []image.Image, columns int) ([]byte, []AtlasFrame, error) {
	if len(frames) == 0 {
		return nil, nil, errors.New("compose: no frames")
	}
	if columns < 1 {
		columns = len(frames)
	}
	cw, ch := 0, 0
	for _, f := range frames {
		if b := f.Bounds(); b.Dx() > cw {
			cw = b.Dx()
		}
		if b := f.Bounds(); b.Dy() > ch {
			ch = b.Dy()
		}
	}
	rows := (len(frames) + columns - 1) / columns
	sheet := image.NewNRGBA(image.Rect(0, 0, cw*columns, ch*rows))
	metas := make([]AtlasFrame, 0, len(frames))
	for i, f := range frames {
		col, row := i%columns, i/columns
		b := f.Bounds()
		ox := col*cw + (cw-b.Dx())/2
		oy := row*ch + (ch-b.Dy())/2
		draw.Draw(sheet, image.Rect(ox, oy, ox+b.Dx(), oy+b.Dy()), f, b.Min, draw.Src)
		metas = append(metas, AtlasFrame{
			Name:  fmt.Sprintf("%s-%04d", name, i),
			Frame: Rect{X: col * cw, Y: row * ch, W: cw, H: ch},
		})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, sheet); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), metas, nil
}

// MarshalAtlas renders TexturePacker JSON-Hash metadata — the format Phaser,
// PixiJS and most engines read via their "JSON (Hash)" atlas loaders.
func MarshalAtlas(metas []AtlasFrame) ([]byte, error) {
	type atlasEntry struct {
		Frame Rect `json:"frame"`
	}
	type atlasDoc struct {
		Frames map[string]atlasEntry `json:"frames"`
		Meta   map[string]string     `json:"meta"`
	}
	doc := atlasDoc{Frames: map[string]atlasEntry{}, Meta: map[string]string{"app": "img-gen compose"}}
	for _, m := range metas {
		doc.Frames[m.Name] = atlasEntry{Frame: m.Frame}
	}
	return json.Marshal(doc)
}
```

(`atlas.go` test uses `encoding/json`; add that import to the test file.)

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/compose/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/atlas.go internal/compose/compose_test.go
git commit -m "feat(compose): grid atlas packing with TexturePacker JSON-Hash metadata

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 3: `internal/compose` — dual-render alpha unmixing

**Files:**
- Create: `internal/compose/alpha.go`
- Test: `internal/compose/compose_test.go` (append)

**Interfaces:**
- Consumes: `image/png` decode.
- Produces: `UnmixAlpha(whitePNG, blackPNG []byte) ([]byte, error)` — returns an NRGBA PNG. Per pixel with premultiplied-alpha compositing (`observed = alpha*F + (1-alpha)*background`): `alpha = (white - black)/255` averaged across channels and clamped to `[0,255]`; `F = black * 255 / alpha` per channel, clamped. `alpha == 0` → fully transparent pixel with color 0. Bounds mismatch → error. This is the v1 transparency technique (LayerDiffuse stays out — SDXL-only upstream, and phase 3).

- [ ] **Step 1: Write the failing test** (append to `compose_test.go`; adds `image/png` import plus the shared `pngBytes` helper — later tasks reuse it)

```go
func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnmixAlpha(t *testing.T) {
	fg := color.NRGBA{200, 100, 50, 127}
	wr := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	br := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	r8 := func(v uint32) uint8 { return uint8(v >> 8) } // 16->8 bit
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			r, g, b, _ := fg.RGBA() // premultiplied 16-bit
			pr, pg, pb := int(r8(r)), int(r8(g)), int(r8(b))
			// observed = P + (1 - P/255)*bg
			obs := func(p int, bg uint8) uint8 { return uint8(p + (255-p)*int(bg)/255) }
			wr.SetNRGBA(x, y, color.NRGBA{R: obs(pr, 255), G: obs(pg, 255), B: obs(pb, 255), A: 255})
			br.SetNRGBA(x, y, color.NRGBA{R: obs(pr, 0), G: obs(pg, 0), B: obs(pb, 0), A: 255})
		}
	}
	out, err := UnmixAlpha(pngBytes(t, wr), pngBytes(t, br))
	if err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	r, gr, b, a := got.At(0, 0).RGBA()
	cr, cg, cb, ca := fg.RGBA()
	want := [][2]int{
		{int(r >> 8), int(cr >> 8)}, {int(gr >> 8), int(cg >> 8)},
		{int(b >> 8), int(cb >> 8)}, {int(a >> 8), int(ca >> 8)},
	}
	for _, p := range want {
		if d := p[0] - p[1]; d < -2 || d > 2 {
			t.Fatalf("recovered channel off by %d: got/want %v (all %v)", d, p, want)
		}
	}
}

func TestUnmixAlphaBounds(t *testing.T) {
	red := pngBytes(t, solid(4, 4, color.RGBA{10, 10, 10, 255}))
	if _, err := UnmixAlpha(red, pngBytes(t, solid(8, 8, color.RGBA{0, 0, 0, 255}))); err == nil {
		t.Fatal("want error for bounds mismatch")
	}
}
```

Round-trip precision note: the test's forward compositing uses 8-bit premultiplied values, so recovered colors land within ±2 of the original — that is what the ±2 tolerance covers.

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/compose/ -run TestUnmix -v`
Expected: FAIL — `undefined: UnmixAlpha`.

- [ ] **Step 3: Implement `alpha.go`**

```go
package compose

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
)

// UnmixAlpha recovers an RGBA image from two renders of the same prompt/seed
// composited on pure white and pure black. With premultiplied alpha the
// observed pixel is alpha*F + (1-alpha)*background, so per pixel
// alpha = (white - black)/255 and F = black*255/alpha. The two renders differ
// by more than the background (the prompt differs by the background clause),
// so the math is noisy at edges; everything clamps. Only the element kind
// uses this; it doubles generation cost for transparent assets.
func UnmixAlpha(whitePNG, blackPNG []byte) ([]byte, error) {
	w, err := png.Decode(bytes.NewReader(whitePNG))
	if err != nil {
		return nil, fmt.Errorf("compose: white render: %v", err)
	}
	b, err := png.Decode(bytes.NewReader(blackPNG))
	if err != nil {
		return nil, fmt.Errorf("compose: black render: %v", err)
	}
	if w.Bounds() != b.Bounds() {
		return nil, errors.New("compose: white/black render bounds differ")
	}
	rect := w.Bounds()
	out := image.NewNRGBA(rect)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			wr, wg, wb, _ := w.At(x, y).RGBA()
			br, bg, bb, _ := b.At(x, y).RGBA()
			dr := int(wr>>8) - int(br>>8)
			dg := int(wg>>8) - int(bg>>8)
			db := int(wb>>8) - int(bb>>8)
			a := (dr + dg + db) / 3
			if a > 255 {
				a = 255
			}
			px := out.Pix[(y-rect.Min.Y)*out.Stride + (x-rect.Min.X)*4]
			if a < 0 {
				a = 0
			}
			if a == 0 {
				continue // no coverage: color channel stays 0, alpha 0
			}
			px[0] = clamp8(int(br>>8) * 255 / a)
			px[1] = clamp8(int(bg>>8) * 255 / a)
			px[2] = clamp8(int(bb>>8) * 255 / a)
			px[3] = uint8(a)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
```

Round-trip precision note: the test's forward compositing uses 8-bit premultiplied values, so recovered colors land within ±2 of the original — that is exactly why the test tolerance is ±2.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/compose/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/alpha.go internal/compose/compose_test.go
git commit -m "feat(compose): dual-render alpha unmixing for transparent elements

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 4: `internal/compose` — kit class table, deliverable wrappers, WebP shim

**Files:**
- Create: `internal/compose/wang.go`, `internal/compose/compose.go`
- Test: `internal/compose/compose_test.go` (append)

**Interfaces:**
- Consumes: Tasks 1–3 (`EncodeGIF`, `UnmixAlpha`, `PackGrid`, `MarshalAtlas`).
- Produces (everything the queue layer will call):
  - `type OutputFile struct { Name string; Data []byte; Primary bool }` — one deliverable; `Primary` marks the file `Job.ImagePath` points at and the MCP preview source.
  - `type KitClass struct { Key string; SeedOffset int64; Hint string }`.
  - `var KitClasses = []KitClass{...}` — fixed 20-class wang set, `SeedOffset` = index 0..19.
  - `ComposeElement(name string, white, black []byte, transparent bool) (OutputFile, error)` — `transparent=false` → `name+".png"` from `white`; `true` → `UnmixAlpha(white, black)`.
  - `ComposeKit(name string, imgs [][]byte) ([]OutputFile, error)` — one `<name>-<class>.png` per class, in `KitClasses` order; index 0 is Primary.
  - `ComposeAtlas(name string, framePNGs [][]byte, columns int) ([]OutputFile, error)` — `<name>-sheet.png` (Primary) + `<name>-atlas.json`.
  - `ComposeGIF(name string, frames []image.Image, delayMS, loop int, formats []string) ([]OutputFile, string, string, error)` — `files, encoder, webpStatus, err`; `<name>.gif` Primary, optional `<name>.webp` via `gif2webp`; unknown format → error.
  - `WebPAvailable() bool`, `EncodeWebP(gifBytes []byte) ([]byte, error)` (10s timeout, temp-dir handoff).

- [ ] **Step 1: Write the failing tests** (append; add `"os/exec"` not needed in tests)

```go
func TestKitClassesShape(t *testing.T) {
	if len(KitClasses) != 20 {
		t.Fatalf("want 20 classes, got %d", len(KitClasses))
	}
	seen := map[string]bool{}
	for i, c := range KitClasses {
		if c.Key == "" || c.Hint == "" || c.SeedOffset != int64(i) {
			t.Fatalf("bad class %d: %+v", i, c)
		}
		if seen[c.Key] {
			t.Fatalf("duplicate key %q", c.Key)
		}
		seen[c.Key] = true
	}
	if _, ok := seen["cross"]; !ok {
		t.Fatal("wang set must contain the four-way junction")
	}
	if KitClasses[0].SeedOffset != 0 || KitClasses[0].Key != "line-h" {
		t.Fatalf("line-h must be the first (primary/preview) class, got %+v", KitClasses[0])
	}
}

func TestComposeElement(t *testing.T) {
	img := solid(4, 4, color.RGBA{200, 100, 50, 255})
	pb := pngBytes(t, img)
	of, err := ComposeElement("panel", pb, pb, false)
	if err != nil || of.Name != "panel.png" || !of.Primary {
		t.Fatalf("opaque element: %+v err=%v", of, err)
	}
	img.SetNRGBA(0, 0, color.NRGBA{0, 0, 0, 0}) // unused; keeps linters quiet about reuse
}

func TestComposeKit(t *testing.T) {
	var imgs [][]byte
	for i := 0; i < 20; i++ {
		imgs = append(imgs, pngBytes(t, solid(4, 4, color.RGBA{uint8(i * 3), 1, 2, 255})))
	}
	files, err := ComposeKit("tilekit", imgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 20 || !files[0].Primary || files[0].Name != "tilekit-line-h.png" {
		t.Fatalf("kit files: %d, first=%+v", len(files), files[0])
	}
	if _, err := ComposeKit("tilekit", imgs[:19]); err == nil {
		t.Fatal("want error for missing class renders")
	}
}

func TestComposeAtlas(t *testing.T) {
	files, err := ComposeAtlas("spark", [][]byte{
		pngBytes(t, solid(4, 4, color.RGBA{1, 2, 3, 255})),
		pngBytes(t, solid(4, 4, color.RGBA{4, 5, 6, 255})),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "spark-sheet.png" || files[1].Name != "spark-atlas.json" {
		t.Fatalf("atlas files: %+v", files)
	}
	var meta map[string]any
	if err := json.Unmarshal(files[1].Data, &meta); err != nil {
		t.Fatalf("atlas.json invalid: %v", err)
	}
}

func TestComposeGIF(t *testing.T) {
	red := color.RGBA{220, 40, 40, 255}
	blue := color.RGBA{40, 40, 220, 255}
	files, encoder, webpStatus, err := ComposeGIF("anim", []image.Image{
		solid(8, 8, red), solid(8, 8, blue),
	}, 100, 0, []string{"gif", "webp"})
	if err != nil {
		t.Fatal(err)
	}
	if encoder == "" || files[0].Name != "anim.gif" || !files[0].Primary {
		t.Fatalf("gif file/encoder: %+v %q", files, encoder)
	}
	t.Logf("webpStatus=%q", webpStatus) // gif2webp may be absent; skip is recorded, not an error
	var webp bool
	for _, f := range files {
		if f.Name == "anim.webp" {
			webp = true
		}
	}
	if webp != (webpStatus == "") {
		t.Fatalf("webp file/status mismatch: files=%v status=%q", files, webpStatus)
	}
	if _, _, _, err := ComposeGIF("anim", nil, 100, 0, []string{"gif"}); err == nil {
		t.Fatal("want error for zero frames")
	}
	if _, _, _, err := ComposeGIF("anim", []image.Image{solid(8, 8, red)}, 100, 0, []string{"mp4"}); err == nil {
		t.Fatal("want error for unknown format")
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/compose/ -run 'TestKitClasses|TestCompose' -v`
Expected: FAIL — `undefined: KitClasses`, `undefined: ComposeElement`, ….

- [ ] **Step 3: Implement `wang.go`**

```go
package compose

// KitClass is one compositional piece of a 20-class wang-tile kit. Key is the
// file suffix; Hint is appended to the kit prompt to steer the render toward
// the piece; SeedOffset is the stable additive offset so a kit's classes are
// reproducible from one base seed.
type KitClass struct {
	Key        string `json:"key"`
	SeedOffset int64  `json:"seed_offset"`
	Hint       string `json:"hint"`
}

// KitClasses is the fixed 20-class wang set. Order matters: index 0
// ("line-h") is the kit's primary/preview file, and SeedOffsets are the
// class's identity — never reorder or renumber.
var KitClasses = []KitClass{
	{"line-h", 0, "a straight horizontal line spanning edge to edge, centered vertically"},
	{"line-v", 1, "a straight vertical line spanning edge to edge, centered horizontally"},
	{"corner-out-nw", 2, "an outer corner: the line enters the north edge and exits the west edge, turning through a quarter-circle in the top-left region"},
	{"corner-out-ne", 3, "an outer corner: the line enters the north edge and exits the east edge, turning through a quarter-circle in the top-right region"},
	{"corner-out-sw", 4, "an outer corner: the line enters the south edge and exits the west edge, turning through a quarter-circle in the bottom-left region"},
	{"corner-out-se", 5, "an outer corner: the line enters the south edge and exits the east edge, turning through a quarter-circle in the bottom-right region"},
	{"corner-in-nw", 6, "an inner corner: lines run along the top edge and the left edge, joining with a concave quarter-circle notch facing the bottom-right"},
	{"corner-in-ne", 7, "an inner corner joining the top edge and the right edge with a concave quarter-circle notch facing the bottom-left"},
	{"corner-in-sw", 8, "an inner corner joining the bottom edge and the left edge with a concave quarter-circle notch facing the top-right"},
	{"corner-in-se", 9, "an inner corner joining the bottom edge and the right edge with a concave quarter-circle notch facing the top-left"},
	{"t-e", 10, "a T junction with three branches meeting at the center and the stem exiting the east edge"},
	{"t-w", 11, "a T junction with three branches meeting at the center and the stem exiting the west edge"},
	{"t-n", 12, "a T junction with three branches meeting at the center and the stem exiting the north edge"},
	{"t-s", 13, "a T junction with three branches meeting at the center and the stem exiting the south edge"},
	{"cross", 14, "a four-way cross junction, one branch exiting each of the four edges"},
	{"cap-n", 15, "a single line entering from the north edge and terminating at a rounded endpoint in the tile center"},
	{"cap-s", 16, "a single line entering from the south edge and terminating at a rounded endpoint in the tile center"},
	{"cap-e", 17, "a single line entering from the east edge and terminating at a rounded endpoint in the tile center"},
	{"cap-w", 18, "a single line entering from the west edge and terminating at a rounded endpoint in the tile center"},
	{"single", 19, "a short standalone motif centered in the tile, touching none of the edges"},
}
```

- [ ] **Step 4: Implement `compose.go`**

```go
package compose

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os/exec"
	"time"
)

// OutputFile is one deliverable the compose layer hands to storage. Primary
// marks the file Job.ImagePath points at and the MCP preview serves.
type OutputFile struct {
	Name    string
	Data    []byte
	Primary bool
}

// ComposeElement produces an element's deliverable. transparent=false uses
// the single render as-is; transparent=true recovers RGBA from the
// white-background and black-background sibling renders.
func ComposeElement(name string, white, black []byte, transparent bool) (OutputFile, error) {
	if transparent {
		un, err := UnmixAlpha(white, black)
		if err != nil {
			return OutputFile{}, err
		}
		return OutputFile{Name: name + ".png", Data: un, Primary: true}, nil
	}
	if white == nil {
		return OutputFile{}, fmt.Errorf("compose: element %q render missing", name)
	}
	return OutputFile{Name: name + ".png", Data: white, Primary: true}, nil
}

// ComposeKit turns per-class renders into one deliverable per class. imgs is
// parallel to KitClasses. A kit with any failed class is wholly failed: the
// point of a kit is the 20 pieces that tile together.
func ComposeKit(name string, imgs [][]byte) ([]OutputFile, error) {
	if len(imgs) != len(KitClasses) {
		return nil, fmt.Errorf("compose: kit %q needs %d renders, got %d", name, len(KitClasses), len(imgs))
	}
	files := make([]OutputFile, 0, len(imgs))
	for i, img := range imgs {
		if img == nil {
			return nil, fmt.Errorf("compose: kit %q class %q render failed", name, KitClasses[i].Key)
		}
		files = append(files, OutputFile{
			Name:    fmt.Sprintf("%s-%s.png", name, KitClasses[i].Key),
			Data:    img,
			Primary: i == 0,
		})
	}
	return files, nil
}

// ComposeAtlas packs frame renders into a sheet plus TexturePacker metadata.
func ComposeAtlas(name string, framePNGs [][]byte, columns int) ([]OutputFile, error) {
	frames := make([]image.Image, 0, len(framePNGs))
	for i, fb := range framePNGs {
		img, err := png.Decode(bytes.NewReader(fb))
		if err != nil {
			return nil, fmt.Errorf("compose: atlas %q frame %d is not a PNG: %v", name, i, err)
		}
		frames = append(frames, img)
	}
	sheet, metas, err := PackGrid(name, frames, columns)
	if err != nil {
		return nil, err
	}
	metaJSON, err := MarshalAtlas(metas)
	if err != nil {
		return nil, err
	}
	return []OutputFile{
		{Name: name + "-sheet.png", Data: sheet, Primary: true},
		{Name: name + "-atlas.json", Data: metaJSON},
	}, nil
}

// ComposeGIF encodes the animation and optional WebP variant. webpStatus is
// empty on success and records the skip reason when gif2webp is absent or
// fails — WebP is an extra, never a failure of the GIF.
func ComposeGIF(name string, frames []image.Image, delayMS, loop int, formats []string) (files []OutputFile, encoder, webpStatus string, err error) {
	if len(frames) == 0 {
		return nil, "", "", fmt.Errorf("compose: gif %q needs at least one frame", name)
	}
	gifBytes, err := EncodeGIF(frames, delayMS, loop)
	if err != nil {
		return nil, "", "", err
	}
	files = []OutputFile{{Name: name + ".gif", Data: gifBytes, Primary: true}}
	encoder = "stdlib shared median-cut palette"
	if len(formats) == 0 {
		formats = []string{"gif"}
	}
	for _, f := range formats {
		switch f {
		case "gif":
			// primary already emitted
		case "webp":
			if !WebPAvailable() {
				webpStatus = "gif2webp not found on PATH; webp variant skipped"
				continue
			}
			wb, werr := EncodeWebP(gifBytes)
			if werr != nil {
				webpStatus = fmt.Sprintf("gif2webp failed: %v", werr)
				continue
			}
			files = append(files, OutputFile{Name: name + ".webp", Data: wb})
		default:
			return nil, "", "", fmt.Errorf("compose: unknown output format %q (want \"gif\" or \"webp\")", f)
		}
	}
	return files, encoder, webpStatus, nil
}

// WebPAvailable reports whether gif2webp (libwebp) is installed. It is
// consulted at compose time, not build time, so the WebP extra is optional
// machinery with zero dependency cost.
func WebPAvailable() bool {
	_, err := exec.LookPath("gif2webp")
	return err == nil
}

// EncodeWebP converts an animated GIF into an animated WebP via gif2webp.
func EncodeWebP(gifBytes []byte) ([]byte, error) {
	if len(gifBytes) == 0 {
		return nil, fmt.Errorf("compose: empty gif")
	}
	dir, err := os.MkdirTemp("", "compose-webp-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := dir + "/in.gif"
	out := dir + "/out.webp"
	if err := os.WriteFile(in, gifBytes, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gif2webp", "-m", in, "-o", out)
	if ob, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gif2webp: %v: %s", err, truncate(ob, 200))
	}
	return os.ReadFile(out)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
```

Add `"os"` to the import block (`os.MkdirTemp/ReadFile/RemoveAll` are used; `os/exec` is already listed).

- [ ] **Step 5: Run tests, verify pass**

Run: `go test ./internal/compose/ -v`
Expected: PASS (TestComposeGIF may log a webp skip reason — that is the contract, not a failure).

- [ ] **Step 6: Full package vet + gofmt**

Run: `gofmt -l internal/compose/ && go vet ./internal/compose/`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/compose/wang.go internal/compose/compose.go internal/compose/compose_test.go
git commit -m "feat(compose): wang kit classes, deliverable wrappers, optional gif2webp

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 5: storage — set directories, FileRef, compose job fields

**Files:**
- Modify: `internal/storage/storage.go`
- Test: `internal/storage/storage_test.go` (append; create if absent)

**Interfaces:**
- Consumes: existing `Store` (`dir`, `ValidImageID`).
- Produces:
  - `type FileRef struct { Name string; Primary bool }` with `name`/`primary,omitempty` JSON tags.
  - `Job` gains: `SetID string` (`set_id,omitempty`), `ComposeParent string` (`compose_parent,omitempty`), `Children []string` (`children,omitempty`), `Files []FileRef` (`files,omitempty`), `ComposeSpec string` (`compose_spec,omitempty`).
  - `SetDir(setID string) string` → `<DATA_DIR>/sets/<setID>`.
  - `SaveSetFile(setID, name string, data []byte) error` — overwrites (restart re-compose must work); validates `name`.
  - `ReadSetFile(setID, name string) ([]byte, error)`, `ListSetFiles(setID string) ([]string, error)` (sorted), `RemoveSet(setID string) error`.
  - `ReadImage(id string) ([]byte, error)` — read back `images/<id>.png`.
  - `ReadDataFile(rel string) ([]byte, error)` — DATA_DIR-relative read with traversal guards (GIF `path` refs).
  - `DeliverSet(setID, dest string) ([]string, error)` — copies every set file to `dest/<name>`; an existing destination file is an error (collision rejection).
  - File-name rule: `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$` (no `/`, no dotfiles, `..` structurally impossible); set IDs/image IDs re-validated with `ValidImageID` everywhere they form a path.

- [ ] **Step 1: Write the failing tests** (create or append `internal/storage/storage_test.go`)

```go
package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSetFilesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveSetFile("0123456789abcdef", "panel.png", []byte("PNGDATA")); err != nil {
		t.Fatal(err)
	}
	b, err := s.ReadSetFile("0123456789abcdef", "panel.png")
	if err != nil || string(b) != "PNGDATA" {
		t.Fatalf("read back: %q %v", b, err)
	}
	names, err := s.ListSetFiles("0123456789abcdef")
	if err != nil || len(names) != 1 || names[0] != "panel.png" {
		t.Fatalf("list: %v %v", names, err)
	}
}

func TestSetFileTraversalRejected(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{
		"../evil.png", "sub/dir/x.png", "/abs.png", ".hidden", "", "x/y", "a..b",
	} {
		if err := s.SaveSetFile("0123456789abcdef", bad, []byte("x")); err == nil {
			t.Fatalf("want rejection for %q", bad)
		}
		if _, err := s.ReadSetFile("0123456789abcdef", bad); err == nil {
			t.Fatalf("read: want rejection for %q", bad)
		}
	}
}

func TestSetIDValidated(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveSetFile("../../etc", "x.png", []byte("x")); err == nil {
		t.Fatal("want rejection for non-hex set id")
	}
	if _, err := s.ReadSetFile("0123456789abcdeg", "x.png"); err == nil { // 'g' is not hex
		t.Fatal("want rejection for non-hex set id on read")
	}
}

func TestReadDataFileGuards(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "sub", "f.png"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := s.ReadDataFile("sub/f.png")
	if err != nil || string(b) != "ok" {
		t.Fatalf("rel read: %q %v", b, err)
	}
	for _, bad := range []string{"../f.png", "/etc/passwd", "sub/../sub/f.png"} {
		if _, err := s.ReadDataFile(bad); err == nil {
			t.Fatalf("want rejection for %q", bad)
		}
	}
}

func TestDeliverSetCollision(t *testing.T) {
	s := newTestStore(t)
	set := "0123456789abcdef"
	if err := s.SaveSetFile(set, "panel.png", []byte("A")); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	got, err := s.DeliverSet(set, dest)
	if err != nil || len(got) != 1 {
		t.Fatalf("deliver: %v %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "panel.png")); string(b) != "A" {
		t.Fatal("delivered content mismatch")
	}
	if _, err := s.DeliverSet(set, dest); err == nil {
		t.Fatal("want collision error on second deliver")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error text: %v", err)
	}
}

func TestReadImageAndRemoveSet(t *testing.T) {
	s := newTestStore(t)
	png := []byte("PNG")
	if _, err := s.SaveImage("0123456789abcdef", png); err != nil {
		t.Fatal(err)
	}
	b, err := s.ReadImage("0123456789abcdef")
	if err != nil || string(b) != "PNG" {
		t.Fatalf("ReadImage: %q %v", b, err)
	}
	if _, err := s.ReadImage("zz"); err == nil {
		t.Fatal("want rejection for invalid image id")
	}
	if err := s.RemoveSet("0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSetFile("0123456789abcdef", "panel.png"); err == nil {
		t.Fatal("set dir should be gone")
	}
}
```

`validSetFile`'s final rule is the regex **and** a `..` contains-check — that is why `a..b` is in the rejection list above.

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/storage/ -v`
Expected: FAIL — methods undefined (`SaveSetFile`, …), `FileRef` unused.

- [ ] **Step 3: Implement in `internal/storage/storage.go`**

(a) Extend `Job` (insert after field `GenID`):

```go
	// --- compose (asset-set / gif) bookkeeping ---
	// SetID identifies the DATA_DIR/sets/<SetID>/ directory holding a parent's
	// deliverables. On compose parents SetID == ID (one id, one set).
	SetID string `json:"set_id,omitempty"`
	// ComposeParent marks a generation child; children re-enqueue their
	// parent when the last of them turns terminal.
	ComposeParent string `json:"compose_parent,omitempty"`
	// Children lists the child job ids of a compose parent (informational —
	// ordering is restored by scanning children's ComposeParent).
	Children []string `json:"children,omitempty"`
	// Files lists the named deliverables under SetDir (queue-set at compose
	// completion).
	Files []FileRef `json:"files,omitempty"`
	// ComposeSpec is the parent's request as JSON, so a restart can recompose.
	ComposeSpec string `json:"compose_spec,omitempty"`
```

and above `Job`:

```go
// FileRef is one named deliverable inside a job's set directory.
type FileRef struct {
	Name    string `json:"name"`
	Primary bool   `json:"primary,omitempty"`
}
```

(b) Set-file internals (append near bottom):

```go
// --- set directories (compose deliverables) ---

// SetDir is a compose parent's deliverable directory. SetID is always a job
// id, so it reuses ValidImageID's 16-hex shape as the traversal guard.
func (s *Store) SetDir(setID string) string {
	return filepath.Join(s.dir, "sets", setID)
}

var setFileRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// validSetFile enforces the set-file name rule: no separators, no leading
// dot, and no ".." anywhere, so traversal is structurally impossible.
func validSetFile(name string) bool {
	return setFileRe.MatchString(name) && !strings.Contains(name, "..")
}

func validSetID(setID string) bool { return ValidImageID(setID) }

// SaveSetFile writes one deliverable into the set dir, creating the dir and
// overwriting freely — a re-run after an interrupted compose must land on the
// same paths. Collision refusal applies to delivery (DeliverSet), not here.
func (s *Store) SaveSetFile(setID, name string, data []byte) error {
	if !validSetID(setID) {
		return fmt.Errorf("invalid set id %q", setID)
	}
	if !validSetFile(name) {
		return fmt.Errorf("invalid set file name %q", name)
	}
	if err := os.MkdirAll(s.SetDir(setID), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.SetDir(setID), name), data, 0o644)
}

// ReadSetFile reads a deliverable back. The name guard makes the read safe
// even though callers pass HTTP path segments.
func (s *Store) ReadSetFile(setID, name string) ([]byte, error) {
	if !validSetID(setID) {
		return nil, fmt.Errorf("invalid set id %q", setID)
	}
	if !validSetFile(name) {
		return nil, fmt.Errorf("invalid set file name %q", name)
	}
	return os.ReadFile(filepath.Join(s.SetDir(setID), name))
}

// ListSetFiles returns the set's file names, sorted.
func (s *Store) ListSetFiles(setID string) ([]string, error) {
	if !validSetID(setID) {
		return nil, fmt.Errorf("invalid set id %q", setID)
	}
	entries, err := os.ReadDir(s.SetDir(setID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// RemoveSet deletes the whole set directory. A missing dir is success so a
// retried delete converges (mirrors RemoveImage).
func (s *Store) RemoveSet(setID string) error {
	if !validSetID(setID) {
		return fmt.Errorf("invalid set id %q", setID)
	}
	if err := os.RemoveAll(s.SetDir(setID)); err != nil {
		return err
	}
	return nil
}

// DeliverSet copies every set file into dest under its own name. A file that
// already exists at the destination is a collision error: delivered names are
// semantic ("panel.png", "tilekit-cross.png") and silently overwriting a
// project file is worse than making the caller pick another dest. Returns the
// written destination paths.
//
// Traversal and symlink safety fall out of the collision rule: set-file names
// match setFileRe (a leading alphanumeric followed by [._-]-only runes), so no
// name contains a separator or a whole ".." segment, and an existing entry —
// including a planted symlink pointing anywhere — is refused before any write,
// never followed.
func (s *Store) DeliverSet(setID, destDir string) ([]string, error) {
	names, err := s.ListSetFiles(setID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		b, err := s.ReadSetFile(setID, name)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(destDir, name)
		if _, err := os.Lstat(dst); err == nil {
			return out, fmt.Errorf("compose_error: destination file %s already exists", dst)
		} else if !os.IsNotExist(err) {
			return out, err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return out, err
		}
		out = append(out, dst)
	}
	return out, nil
}

// ReadImage reads a stored generation result back by id.
func (s *Store) ReadImage(id string) ([]byte, error) {
	if !ValidImageID(id) {
		return nil, fmt.Errorf("invalid image id %q", id)
	}
	return os.ReadFile(filepath.Join(s.dir, "images", id+".png"))
}

// ReadDataFile reads a DATA_DIR-relative file. Clean relative paths only:
// absolute paths and any ".." are rejected, so a GIF's path frame source can
// only reference data the service already owns.
func (s *Store) ReadDataFile(rel string) ([]byte, error) {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("invalid data path %q", rel)
	}
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") {
		return nil, fmt.Errorf("invalid data path %q", rel)
	}
	return os.ReadFile(filepath.Join(s.dir, clean))
}
```

(c) Imports: add `"sort"` and `"strings"` to `storage.go`'s import block.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/storage/ -v && go vet ./internal/storage/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/storage.go internal/storage/storage_test.go
git commit -m "feat(storage): set directories, FileRef, compose job fields, delivery writer

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 6: queue — `asset` child mode

**Files:**
- Modify: `internal/queue/queue.go`
- Test: `internal/queue/compose_test.go` (create)

**Interfaces:**
- Consumes: `submitOne`, `resolveTextPrompt`, `dispatch` (existing behavior in `queue.go`: non-"generate" modes resolve via `resolveTextPrompt`; `dispatch`'s `default` branch already calls `Ops.Generate`).
- Produces:
  - `SubmitRequest.ComposeParent string` (`json:"compose_parent,omitempty"`) — set by compose submission; copied onto the job by `submitOne`.
  - Mode `"asset"`: a free-prompt, engine-size generation child. Validation: `prompt` required, `validSize(size)`, size `W`/`H` both multiples of 16; no genre/fields; `Enhance`/`Style`/`Preset`/model/Loras/sampling all pass through unchanged. Dispatches via the existing `default` → `Ops.Generate` path.
  - SDXL engine gate broadened: `mode != "generate" && mode != "edit" && mode != "asset"` (asset is generate-equivalent).
  - `storage.Job.ComposeParent` field (Task 5) is filled from the request.

- [ ] **Step 1: Write the failing tests** (create `internal/queue/compose_test.go`)

This file will grow across Tasks 6–9; it starts with its own stubs that capture what the dispatcher saw, because queue-level compose tests need real decodable PNGs (the parent composes from child bytes) and per-child observables.

```go
package queue

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"sync"
	"testing"

	"img-gen/internal/prompting"
	"img-gen/internal/storage"
)

// genCapture records every Generate call the dispatch path made.
type genCapture struct {
	mu      sync.Mutex
	Prompts []string
	Sizes   []string
	Seeds   []int64
	Images  [][]byte
}

func (c *genCapture) add(prompt, size string, seed *int64, img []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Prompts = append(c.Prompts, prompt)
	c.Sizes = append(c.Sizes, size)
	s := int64(-1)
	if seed != nil {
		s = *seed
	}
	c.Seeds = append(c.Seeds, s)
	c.Images = append(c.Images, img)
}

// composeTestOpts returns Options whose Generate returns a real 8x8 PNG (the
// parent composes from child bytes, so "PNG" byte strings won't do).
func composeTestOpts(t *testing.T, dir string, cap *genCapture) Options {
	t.Helper()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Genres: testCatalog(),
		Store:  s,
		Ops: ImageOps{
			Generate: func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
				img := image.NewRGBA(image.Rect(0, 0, 8, 8))
				var buf bytes.Buffer
				if err := png.Encode(&buf, img); err != nil {
					t.Fatal(err)
				}
				cap.add(prompt, size, sp.Seed, buf.Bytes())
				return buf.Bytes(), sp.Seed, nil
			},
		},
		Chat: func(ctx context.Context, msgs []prompting.Message) (string, error) { return "enhanced", nil },
	}
}

func TestAssetChildSubmit(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(7)
	id, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "steel bolt icon", Size: "256x256", Seed: &seed})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 1 || cap.Prompts[0] != "steel bolt icon" || cap.Sizes[0] != "256x256" || cap.Seeds[0] != 7 {
		t.Fatalf("dispatch saw: prompts=%q sizes=%q seeds=%q", cap.Prompts, cap.Sizes, cap.Seeds)
	}
	cap.mu.Unlock()
}

func TestAssetChildValidation(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	if _, err := m.Submit(SubmitRequest{Mode: "asset", Size: "256x256"}); err == nil {
		t.Fatal("want prompt-required error")
	}
	if _, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "x", Size: "250x250"}); err == nil {
		t.Fatal("want 16-multiple error for 250x250")
	}
	if _, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "x", Size: "bogus"}); err == nil {
		t.Fatal("want invalid size error")
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/queue/ -run 'TestAssetChild' -v`
Expected: FAIL — `unknown_mode "asset"` (first test fails on submit error; second test may pass the first two asserts and fail where `"250x250"` still passes `validSize` — i.e. the 16-multiple check is missing).

- [ ] **Step 3: Implement in `internal/queue/queue.go`**

(a) `SubmitRequest` gains one field (after `NegativePrompt`):

```go
	// ComposeParent is set by the compose submission path; submitOne copies it
	// onto the job so the child can re-enqueue its parent at terminal state.
	ComposeParent string `json:"compose_parent,omitempty"`
```

(b) In `submitOne`'s mode switch, add before the `default:` case (after `"pose"`):

```go
	case "asset":
		// compose generation child: free prompt, engine size, no genre/fields
		if strings.TrimSpace(req.Prompt) == "" {
			return "", &ValidationError{Code: "validation_error", Msg: "prompt is required for asset"}
		}
		if !validAssetSize(req.Size) {
			return "", &ValidationError{Code: "invalid_size", Msg: fmt.Sprintf("invalid size %q for asset (want WxH, both multiples of 16)", req.Size)}
		}
		inputs.Image = "" // generate-equivalent: no image inputs
```

Wait — the `inputs` field is only read by dispatch for image modes; `asset` takes the `default` dispatch branch (`Ops.Generate`) and never reads `inputs`. Do NOT assign `inputs.Image`; leave `inputs` zero. Also don't store `m.inputs[id]` for asset — the existing `if mode != "generate"` branch writes `m.inputs[id]` even with a zero value, which is harmless, but cleaner to skip: change that condition to `if mode != "generate" && mode != "asset"`.

(c) Add the size helper near `validSize`:

```go
// validAssetSize is validSize plus the engines' 16px latent granularity —
// both sidecars accept sizes that tile (FLUX latent grid, SDXL multiples of
// 8; 16 is the safe common denominator) and atlases assume stable cells.
func validAssetSize(size string) bool {
	if !validSize(size) {
		return false
	}
	w, h, _ := strings.Cut(size, "x")
	wi, _ := strconv.Atoi(w)
	hi, _ := strconv.Atoi(h)
	return wi%16 == 0 && hi%16 == 0
}
```

(d) Broaden the SDXL gate (queue.go:449):

```go
	if req.Sidecar == "sdxl" && mode != "generate" && mode != "edit" && mode != "asset" {
```

(e) Copy the parent link in the job construction block (queue.go:454-475 add field):

```go
		ComposeParent:  req.ComposeParent,
```

(f) `run()`'s prompt resolution: `"generate"` keeps the genre path; **`"asset"` and every other mode** flow through `resolveTextPrompt` already (the `else` branch) — no code change, but verify. Also add one log line inside `run` for asset? Not needed.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/queue/ -v`
Expected: PASS (all pre-existing tests too — `composeTestOpts` mirrors `testOptsAt`'s shape).

- [ ] **Step 5: Commit**

```bash
git add internal/queue/queue.go internal/queue/compose_test.go
git commit -m "feat(queue): asset generation mode for compose children

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 7: queue — compose coordinator (parent registration, child handoff, run/cancel/restore)

**Files:**
- Create: `internal/queue/compose.go`
- Modify: `internal/queue/queue.go`
- Test: `internal/queue/compose_test.go` (append)

**Interfaces:**
- Consumes: Task 5 storage fields/methods; Task 6 `asset` mode.
- Produces (used by Tasks 8–9 and v1):
  - `Manager.childLeft map[string]int`, `Manager.work` reuse (no new channels).
  - `submitComposeParent(parentID string, spec composeSpec, children []SubmitRequest) (string, error)` — registers the parent (in-memory + persisted queued line) BEFORE submitting children, presets `childLeft[parentID] = len(children)`, submits children with `ComposeParent` set, fills `parent.Children`, persists the parent again.
  - `composeSpec` (JSON persisted in `Job.ComposeSpec`): `{kind: "asset-set"|"gif", name, dest, assets: [assetSpec], gif: *gifPayload}`; `assetSpec`: `{kind, name, prompt, size, frames, columns, transparent, children: N}` (count only — child ids are never in the spec); `gifPayload`: `{frames: [set-relative filenames], delay_ms, loop, formats}`.
  - `runCompose(id)`, `composeAssetSet(id, spec, children)`, `composeGIF(id, spec, children)` — Task 7 lands `runCompose` skeleton + `childResult`; Tasks 8–9 fill the executors.
  - `childFinished(childID)` hook called from `finish()` — decrements the parent's remaining count; at zero re-enqueues the parent (non-blocking send; a blocked send retries in a goroutine).
  - `abandonCompose(parentID, submitted []string)` — on mid-submit failure: drop the parent, cancel submitted children.
  - `run()` short-circuits `mode == "compose"` to `runCompose`.
  - `Cancel(id)` on a compose parent also cancels its non-terminal children (children scan via `ComposeParent == id`); `Delete(id)` on a compose parent removes the set dir.
  - `restore()` second pass: non-terminal compose parents get `childLeft` = count of non-terminal children; if none remain, the parent is enqueued immediately (recompose from whatever persisted results survived; from-frames GIFs recompose from their saved sources).

- [ ] **Step 1: Write the failing tests** (append to `compose_test.go`)

Two tests. `TestComposeParentHandoff` pins the coordination contract — parent registered before children, children dispatched through the normal worker, parent re-enqueued by the last child. It expects `"failed"` with `executor not implemented` because Task 7 lands only the coordination skeleton; Task 8's first step flips this expectation to `"done"` when the real executor arrives.

```go
func TestComposeParentHandoff(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	spec := composeSpec{Kind: "asset-set", Name: "demo", Assets: []assetSpec{
		{Kind: "element", Name: "a", Prompt: "a", Size: "16x16", Children: 1},
		{Kind: "element", Name: "b", Prompt: "b", Size: "16x16", Children: 1},
	}}
	kids := []SubmitRequest{
		{Mode: "asset", Prompt: "a", Size: "16x16"},
		{Mode: "asset", Prompt: "b", Size: "16x16"},
	}
	parentID, err := m.submitComposeParent(newID(), spec, kids)
	if err != nil {
		t.Fatal(err)
	}

	// Task 7's runCompose fails honestly — no executor until Task 8.
	waitFor(t, m, parentID, "failed")
	j, _ := m.Get(parentID)
	if !strings.Contains(j.Error, "executor not implemented") {
		t.Fatalf("parent error = %q", j.Error)
	}
	if len(j.Children) != 2 {
		t.Fatalf("children = %d, want 2", len(j.Children))
	}

	// Exactly the two children went through Ops.Generate: the parent
	// bypasses dispatch (run() short-circuits mode "compose" to runCompose).
	cap.mu.Lock()
	n := len(cap.Prompts)
	cap.mu.Unlock()
	if n != 2 {
		t.Fatalf("dispatches = %d, want 2", n)
	}
}

func TestComposeParentCancel(t *testing.T) {
	cap := &genCapture{}
	// Slow the children so Cancel lands while they are running; with instant
	// stub children the parent could go terminal before Cancel executes and
	// the cancel would race.
	opts := composeTestOpts(t, t.TempDir(), cap)
	base := opts.Ops.Generate
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		time.Sleep(100 * time.Millisecond)
		return base(ctx, prompt, size, spec, sp)
	}
	m := New(opts)
	spec := composeSpec{Kind: "gif", GIF: &gifPayload{DelayMS: 100, Loop: 0, Formats: []string{"gif"}}}
	kids := []SubmitRequest{
		{Mode: "asset", Prompt: "a", Size: "16x16"},
		{Mode: "asset", Prompt: "b", Size: "16x16"},
	}
	parentID, err := m.submitComposeParent(newID(), spec, kids)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(parentID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, parentID, "cancelled")
}
```

If `strings` or `time` are not already in `compose_test.go`'s import block, add them (`context` and `storage` are already there via the Task 6 stub signature). With these appended, the file declares four tests: `TestAssetChildSubmit` and `TestAssetChildValidation` (Task 6) plus these two.

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/queue/ -run 'TestComposeParent' -v`
Expected: FAIL — `composeSpec`/`submitComposeParent` undefined.

- [ ] **Step 3: Implement `internal/queue/compose.go`**

```go
package queue

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"img-gen/internal/compose"
	"img-gen/internal/storage"
)

// --- persisted parent spec ------------------------------------------------

// composeSpec is what a parent job remembers (Job.ComposeSpec) so a restart
// can re-compose. Child job ids are deliberately absent: children are
// recovered by scanning for jobs whose ComposeParent == parent ID, so there
// is no registration race between spec and child list.
type composeSpec struct {
	Kind   string      `json:"kind"` // "asset-set" | "gif"
	Name   string      `json:"name"`
	Dest   string      `json:"dest,omitempty"`
	Assets []assetSpec `json:"assets,omitempty"`
	GIF    *gifPayload `json:"gif,omitempty"`
}

type assetSpec struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Prompt      string `json:"prompt"`
	Size        string `json:"size"`
	Frames      int    `json:"frames,omitempty"`
	Columns     int    `json:"columns,omitempty"`
	Transparent bool   `json:"transparent,omitempty"`
	Children    int    `json:"children"` // generation jobs this asset owns, in order
}

type gifPayload struct {
	Frames  []string `json:"frames,omitempty"` // set-relative source files (from_frames)
	DelayMS int      `json:"delay_ms"`
	Loop    int      `json:"loop"`
	Formats []string `json:"formats"`
}

// --- parent registration --------------------------------------------------

// submitComposeParent registers a compose parent and submits its generation
// children. The parent is registered (in-memory + persisted) BEFORE any child
// is submitted and its remaining-child count presets to len(children), so a
// child that finishes within microseconds of its submit can never outrun the
// parent's registration. Parents occupy no worker slot while children run —
// the last child to turn terminal re-enqueues the parent (childFinished).
func (m *Manager) submitComposeParent(parentID string, spec composeSpec, children []SubmitRequest) (string, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("compose spec: %v", err)
	}
	parent := &storage.Job{
		ID:          parentID,
		Mode:        "compose",
		Prompt:      strings.TrimSpace("compose " + spec.Kind),
		SetID:       parentID,
		ComposeSpec: string(specJSON),
		Status:      "queued",
		CreatedAt:   time.Now(),
	}
	m.mu.Lock()
	m.jobs[parentID] = parent
	m.order = append(m.order, parentID)
	m.childLeft[parentID] = len(children)
	m.mu.Unlock()
	if err := m.opts.Store.AppendHistory(*parent); err != nil {
		log.Printf("queue: persist queued compose parent %s: %v", parentID, err)
	}
	if len(children) == 0 {
		// No generation children (a from-frames GIF): compose immediately.
		m.enqueueCompose(parentID)
		m.mu.Lock()
		j := *parent
		m.mu.Unlock()
		_ = j
		return parentID, nil
	}
	var ids []string
	for i := range children {
		children[i].ComposeParent = parentID
		id, serr := m.submitOne(children[i], "")
		if serr != nil {
			m.abandonCompose(parentID, ids)
			return "", serr
		}
		ids = append(ids, id)
	}
	m.mu.Lock()
	parent.Children = ids
	j := *parent
	m.mu.Unlock()
	if err := m.opts.Store.AppendHistory(j); err != nil {
		log.Printf("queue: persist compose parent children %s: %v", parentID, err)
	}
	return parentID, nil
}

// abandonCompose unwinds a half-submitted parent: drop the in-memory parent
// so later child finishes no-op (childFinished finds no parent job), and
// cancel whatever children already went out.
func (m *Manager) abandonCompose(parentID string, submitted []string) {
	m.mu.Lock()
	delete(m.jobs, parentID)
	delete(m.childLeft, parentID)
	for i, o := range m.order {
		if o == parentID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	for _, cid := range submitted {
		_ = m.Cancel(cid) // best effort: terminal children were their own story
	}
}

// enqueueCompose pushes a parent onto the work channel. The channel is small
// (cap 64) and the worker may be blocked finishing the child that triggered
// this send, so a non-blocking send falls back to a goroutine that waits —
// never to a dropped enqueue.
func (m *Manager) enqueueCompose(parentID string) {
	select {
	case m.work <- parentID:
	default:
		go func() { m.work <- parentID }()
	}
}

// --- child -> parent handoff ----------------------------------------------

// childFinished is called from finish() for every terminal job. Compose
// children decrement their parent's remaining count; the child that drops it
// to zero re-enqueues the parent so the worker composes the finished parts.
// This is the single-slot coordination: the parent never occupies a slot
// while its children generate.
//
// A parent deleted mid-run (abandonCompose) is nil in jobs, so its children's
// finishes no-op here.
func (m *Manager) childFinished(childID string) {
	m.mu.Lock()
	j := m.jobs[childID]
	if j == nil || j.ComposeParent == "" {
		m.mu.Unlock()
		return
	}
	parent := j.ComposeParent
	if m.jobs[parent] == nil {
		m.mu.Unlock()
		return
	}
	if m.childLeft[parent] > 0 {
		m.childLeft[parent]--
	}
	if m.childLeft[parent] > 0 {
		m.mu.Unlock()
		return
	}
	delete(m.childLeft, parent)
	m.mu.Unlock()
	m.enqueueCompose(parent)
}

// --- compose run -----------------------------------------------------------

// runCompose executes a worker-dequeued compose parent. The job's deliverable
// work is CPU-local (no sidecar), so it runs synchronously inside the worker.
func (m *Manager) runCompose(id string) {
	m.mu.Lock()
	job := m.jobs[id]
	var spec composeSpec
	if job != nil && json.Unmarshal([]byte(job.ComposeSpec), &spec) != nil {
		job = nil // unparsable spec: handled below as a hard failure
	}
	m.mu.Unlock()
	if job == nil {
		m.finish(id, "failed", "compose_error: compose spec unparsable or parent gone")
		return
	}
	m.log(id, "compose start (kind=%s)", spec.Kind)
	if m.isCancelled(id) {
		m.finish(id, "cancelled", "")
		return
	}
	m.setStatus(id, "generating", "")

	// Children in submission order: m.order records submit order and every
	// child carries ComposeParent, so the reconstruction needs no persisted
	// child list.
	m.mu.Lock()
	var children []string
	for _, oid := range m.order {
		if c := m.jobs[oid]; c != nil && c.ComposeParent == id {
			children = append(children, c.ID)
		}
	}
	m.mu.Unlock()

	switch spec.Kind {
	case "asset-set":
		m.composeAssetSet(id, &spec, children)
	case "gif":
		m.composeGIF(id, &spec, children)
	default:
		m.finish(id, "failed", fmt.Sprintf("compose_error: unknown compose kind %q", spec.Kind))
	}
}

// childResult fetches a child's outcome: status, error text, and — when the
// child succeeded — its stored PNG bytes.
func (m *Manager) childResult(childID string) (status, errMsg string, png []byte) {
	m.mu.Lock()
	j := m.jobs[childID]
	m.mu.Unlock()
	if j == nil {
		return "failed", "child job record missing", nil
	}
	if j.Status != "done" {
		msg := j.Error
		if msg == "" {
			msg = "child " + j.Status
		}
		return j.Status, msg, nil
	}
	b, err := m.opts.Store.ReadImage(childID)
	if err != nil {
		return "failed", err.Error(), nil
	}
	return "done", "", b
}

// childrenResults loads a parent's child images in order. specChildren is the
// child count the spec expects; a child that is not done contributes nil with
// its error string recorded.
func (m *Manager) childrenResults(specChildren int, ids []string, logf func(f string, a ...any)) (imgs [][]byte, errs []string) {
	if len(ids) != specChildren {
		// Defensive: a drifted child set fails the parent loudly instead of
		// silently composing the wrong artifact.
		return nil, []string{fmt.Sprintf("expected %d child result(s), found %d", specChildren, len(ids))}
	}
	imgs = make([][]byte, len(ids))
	for i, cid := range ids {
		st, em, png := m.childResult(cid)
		if st != "done" {
			errs = append(errs, em)
			logf("child %s: %s", cid, em)
		}
		imgs[i] = png
	}
	return imgs, errs
}

// resolveExportDest applies the delivery fallback chain: explicit dest →
// EXPORT_DIR → ~/Downloads/img-gen (the same chain the v1 export endpoint
// uses). An empty dest after the chain means no delivery.
func (m *Manager) resolveExportDest(dest string) string {
	if dest == "" {
		dest = m.opts.ExportDir
	}
	if dest == "" {
		home, _ := os.UserHomeDir()
		dest = filepath.Join(home, "Downloads", "img-gen")
	}
	return filepath.Clean(dest)
}

// validComposeAssetName lowercases, squashes disallowed characters, and trims
// leading/trailing separators and dots. Any non-empty result passes storage's
// validSetFile rule (that helper is unexported, so queue mirrors its shape
// here — SaveSetFile re-checks the real rule as defense in depth). An empty
// return means nothing sane remained; callers reject with a validation error.
func validComposeAssetName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" || strings.Contains(out, "..") || len(out) > 128 {
		return ""
	}
	return out
}
```

…with the file's import block exactly as written at the top of this step: `encoding/json`, `fmt`, `log`, `os`, `path/filepath`, `strings`, `time`, `img-gen/internal/compose`, `img-gen/internal/storage`. `os`/`path/filepath` serve `resolveExportDest`; `encoding/hex` and `crypto/rand` are NOT needed (`newID` already lives in queue.go). `compose` has no caller in this task — Task 8's executors use it — so keep the import and add one compile hold at the bottom of the file, deleted in Task 8:

```go
var _ = compose.KitClasses // referenced by composeAssetSet (Task 8)
```

- [ ] **Step 4: Modify `internal/queue/queue.go`**

(a) `Manager` gains one field (init in `New`):

```go
	childLeft  map[string]int
```
and in `New`: `childLeft: map[string]int{},`.

(b) `Options` gains:

```go
	// ExportDir is the delivery fallback when a compose request omits dest.
	ExportDir string
```

(c) `finish()` calls the hook after the unlock + broadcast, at the very end:

```go
	m.broadcast(Event{JobID: id, Status: status, Error: errMsg, Ts: now, Seed: j.Seed})
	m.childFinished(id)
```

(d) `run()` short-circuit — right after the `if job == nil` block and before the mode-based prompt resolution, insert:

```go
	if job.Mode == "compose" {
		// Compose parents are not dispatcher jobs: the worker runs the local
		// composition once their children reached terminal states. They carry
		// no prompt, no sidecar work, and no progress polling.
		m.runCompose(id)
		return
	}
```

(e) `Cancel()` compose propagation — read children under the existing lock and cancel them after the parent's own cancellation has been recorded:

```go
	m.mu.Lock()
	m.cancelled[id] = true
	m.mu.Unlock()
```
becomes
```go
	var childIDs []string
	if job.Mode == "compose" {
		m.mu.Lock()
		for _, oid := range m.order {
			if c := m.jobs[oid]; c != nil && c.ComposeParent == id && !isTerminal(c.Status) {
				childIDs = append(childIDs, c.ID)
			}
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.cancelled[id] = true
	m.mu.Unlock()
	for _, cid := range childIDs {
		m.mu.Lock()
		m.cancelled[cid] = true
		cfn := m.cancels[cid]
		m.mu.Unlock()
		if cfn != nil {
			cfn()
		}
	}
```
(The single `Ops.Cancel` call after this stays as-is: the sidecar is single-flight, so one call frees whichever child is in flight; queued children are flag-cancelled and their `run()` will finish them "cancelled", which feeds `childFinished` as usual — eventually the re-enqueued parent sees itself cancelled and finishes "cancelled".)

(f) `Delete()` compose branch — after the `RemoveImage` call add:

```go
	if job.Mode == "compose" && job.SetID != "" {
		if err := m.opts.Store.RemoveSet(job.SetID); err != nil {
			return err
		}
	}
```

(g) `restore()` second pass — just before the `for _, id := range attachIDs { go m.attach(id) }` loop:

```go
	// Compose parents enqueue themselves once their remaining child count is
	// known. Attach-restored children keep status "generating" and will
	// re-enqueue the parent through childFinished when they land; everything
	// else (children already terminal, or honestly failed above with no
	// sidecar gen id) recomposes from whatever persisted results survived —
	// partial-success semantics apply.
	for _, oid := range m.order {
		j := m.jobs[oid]
		if j == nil || j.Mode != "compose" || isTerminal(j.Status) {
			continue
		}
		remaining := 0
		for _, cid := range m.order {
			c := m.jobs[cid]
			if c == nil || c.ComposeParent != j.ID {
				continue
			}
			if !isTerminal(c.Status) {
				remaining++
			}
		}
		if remaining > 0 {
			m.childLeft[j.ID] = remaining
			continue
		}
		m.enqueueCompose(j.ID)
	}
```

The child-side of restore needs no extra work: children rehydrated as `"generating"` (with a `GenID`) carry `ComposeParent` on their record and finish through `attach`, firing the hook; children honestly failed above got `Status = "failed"` without calling `finish()`, so they won't double-decrement — the second pass counted them as already-terminal.

- [ ] **Step 5: Run tests, verify pass**

Run: `go test ./internal/queue/ -v`
Expected: PASS. The executors behind `runCompose`'s dispatch are honest stubs for this task — a parent reported "done" without files would lie, so both fail loudly with the spec intact:

```go
func (m *Manager) composeAssetSet(id string, spec *composeSpec, children []string) {
	// Filled by Task 8 (per-asset composition). Until then a parent composed
	// as done without files would lie, so fail honestly with the spec intact.
	m.finish(id, "failed", "compose_error: asset-set executor not implemented")
}
func (m *Manager) composeGIF(id string, spec *composeSpec, children []string) {
	m.finish(id, "failed", "compose_error: gif executor not implemented")
}
```

Task 8's first step implements the real `composeAssetSet` and flips `TestComposeParentHandoff` to expect `"done"`.

- [ ] **Step 6: Commit**

```bash
git add internal/queue/compose.go internal/queue/queue.go internal/queue/compose_test.go
git commit -m "feat(queue): compose coordinator — parent/child handoff, cancel, restore

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 8: queue — `SubmitAssetSet` + execution (composition, manifest, delivery)

**Files:**
- Modify: `internal/queue/compose.go` (append; replaces the Task 7 honest-failure executor stubs)
- Test: `internal/queue/compose_test.go` (append)

**Interfaces:**
- Consumes: Task 4 `compose.{ComposeElement,ComposeKit,ComposeAtlas,OutputFile,KitClasses}`; Task 5 `storage.{FileRef,SaveSetFile,ReadSetFile,ReadImage,DeliverSet,RemoveSet}`; Task 7 `submitComposeParent/composeSpec/assetSpec/childResult/resolveExportDest/validComposeAssetName`; Task 6 `validAssetSize`; queue internals `newID`, `finish`, `setStatus`, `log` (all verified present in `queue.go`).
- Produces (Task 10's HTTP layer and Task 12's MCP call these):
  - `AssetSetRequest{Name, Dest, Seed *int64, Assets []AssetSpecIn}`; `AssetSpecIn{Kind, Name, Prompt, Negative, Size, Frames, Columns, Transparent}`.
  - `SubmitAssetSet(req AssetSetRequest) (string, error)` — returns the parent job id (which doubles as the set id).
  - `setManifest`, `manifestAsset` (persisted as `sets/<id>/manifest.json`).
  - Queue `Options.ExportDir` (Task 7) feeds the delivery fallback chain.

- [ ] **Step 1: Write the failing tests** (append to `internal/queue/compose_test.go`)

```go
// testPNG is a real 8x8 PNG: compose parents decode child bytes, so stubs
// must return decodable images.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// failCapture extends genCapture with call-indexed failure injection:
// FailCall >= 0 fails exactly that (0-based) Generate call; FailCall < 0
// with FailAll fails every call.
type failCapture struct {
	genCapture
	FailCall int
	FailAll  bool
	n        int
}

func composeTestOptsFail(t *testing.T, dir string, cap *failCapture) Options {
	t.Helper()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Genres: testCatalog(),
		Store:  s,
		Ops: ImageOps{
			Generate: func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
				cap.mu.Lock()
				call := cap.n
				cap.n++
				fail := cap.FailAll || (cap.FailCall >= 0 && call == cap.FailCall)
				cap.mu.Unlock()
				if fail {
					return nil, nil, fmt.Errorf("sidecar boom")
				}
				img := testPNG(t)
				cap.add(prompt, size, sp.Seed, img)
				return img, sp.Seed, nil
			},
		},
		Chat: func(ctx context.Context, msgs []prompting.Message) (string, error) { return "enhanced", nil },
	}
}

func TestSubmitAssetSetElement(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(42)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "bolt", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "bolt", Prompt: "hex bolt", Size: "64x64"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 1 || cap.Prompts[0] != "hex bolt" || cap.Seeds[0] != 42 {
		t.Fatalf("dispatch: %q %q", cap.Prompts, cap.Seeds)
	}
	cap.mu.Unlock()
	data, err := m.opts.Store.ReadSetFile(id, "bolt.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("bolt.png not a PNG: %v", err)
	}
	j, _ := m.Get(id)
	if len(j.Files) != 1 || j.Files[0].Name != "bolt.png" || !j.Files[0].Primary {
		t.Fatalf("files: %+v", j.Files)
	}
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.Kind != "asset-set" || len(man.Assets) != 1 || man.Assets[0].Name != "bolt" || man.Assets[0].Error != "" {
		t.Fatalf("manifest: %+v", man)
	}
	if len(man.Assets[0].Files) != 1 || man.Assets[0].Files[0] != "bolt.png" {
		t.Fatalf("manifest files: %+v", man.Assets[0].Files)
	}
	if j.ImagePath != "sets/"+id+"/bolt.png" {
		t.Fatalf("image path %q", j.ImagePath)
	}
}

func TestSubmitAssetSetTransparent(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(7)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "star", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "star", Prompt: "star icon", Size: "32x32", Transparent: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 2 {
		t.Fatalf("want 2 children (white+black), got %q", cap.Prompts)
	}
	if cap.Seeds[0] != 7 || cap.Seeds[1] != 7 {
		t.Fatalf("backgrounds share the seed: %q", cap.Seeds)
	}
	if !strings.Contains(cap.Prompts[0], "white") || !strings.Contains(cap.Prompts[1], "black") {
		t.Fatalf("bg suffixes missing: %q", cap.Prompts)
	}
	cap.mu.Unlock()
	data, err := m.opts.Store.ReadSetFile(id, "star.png")
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := im.(*image.NRGBA); !ok {
		t.Fatalf("unmixed output should be NRGBA, got %T", im)
	}
}

func TestSubmitAssetSetKit(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(100)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "pipes", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "kit", Name: "pipes", Prompt: "blue pipes", Size: "64x64"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != len(compose.KitClasses) {
		t.Fatalf("want %d children, got %d", len(compose.KitClasses), len(cap.Prompts))
	}
	for i := range compose.KitClasses {
		if want := seed + int64(compose.KitClasses[i].SeedOffset); cap.Seeds[i] != want {
			t.Fatalf("child %d seed %d, want %d", i, cap.Seeds[i], want)
		}
		if !strings.Contains(cap.Prompts[i], compose.KitClasses[i].Hint) {
			t.Fatalf("child %d prompt %q lacks hint %q", i, cap.Prompts[i], compose.KitClasses[i].Hint)
		}
	}
	cap.mu.Unlock()
	j, _ := m.Get(id)
	if len(j.Files) != len(compose.KitClasses) {
		t.Fatalf("kit files: %d", len(j.Files))
	}
	// Primary is class index 0.
	if j.Files[0].Name != "pipes-line-h.png" || !j.Files[0].Primary {
		t.Fatalf("kit primary: %+v", j.Files[0])
	}
	for _, f := range j.Files {
		if _, err := m.opts.Store.ReadSetFile(id, f.Name); err != nil {
			t.Fatalf("kit file %s: %v", f.Name, err)
		}
	}
}

func TestSubmitAssetSetAtlas(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(5)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "walk", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "atlas", Name: "walk", Prompt: "robot walk", Size: "64x64", Frames: 3, Columns: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 3 || cap.Seeds[0] != 5 || cap.Seeds[2] != 7 {
		t.Fatalf("atlas children: %q %q", cap.Prompts, cap.Seeds)
	}
	cap.mu.Unlock()
	j, _ := m.Get(id)
	var names []string
	for _, f := range j.Files {
		names = append(names, f.Name)
	}
	if len(names) != 2 || names[0] != "walk-sheet.png" || names[1] != "walk-atlas.json" {
		t.Fatalf("atlas files: %q", names)
	}
	araw, err := m.opts.Store.ReadSetFile(id, "walk-atlas.json")
	if err != nil {
		t.Fatal(err)
	}
	var atlas struct {
		Frames map[string]struct {
			Frame struct {
				X, Y, W, H int `json:"x,y,w,h"`
			} `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(araw, &atlas); err != nil {
		t.Fatal(err)
	}
	if len(atlas.Frames) != 3 {
		t.Fatalf("atlas frames: %d", len(atlas.Frames))
	}
}

func TestSubmitAssetSetValidation(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	bad := []AssetSetRequest{
		{Name: "x", Assets: []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "p", Size: "64x64"}}},                      // seed required
		{Name: "x", Seed: ptr64(1)},                                                                                      // no assets
		{Name: "x", Seed: ptr64(1), Assets: tooMany},                                                                     // 9 assets
		{Name: "x/..", Seed: ptr64(1), Assets: one},                                                                      // name sanitizes to empty
		{Name: "x", Seed: ptr64(1), Dest: "rel/path", Assets: one},                                                       // dest not absolute
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "p", Size: "250x250"}}},   // 16-multiple
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "", Size: "64x64"}}},      // prompt required
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "widget", Name: "a", Prompt: "p", Size: "64x64"}}},      // unknown kind
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "atlas", Name: "a", Prompt: "p", Size: "64x64", Frames: 1}}}, // frames >= 2
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "p", Size: "64x64"}, {Kind: "element", Name: "a", Prompt: "p", Size: "64x64"}}}, // dup names
		{Name: "big", Seed: ptr64(1), Assets: bigChildren},                                                               // > 64 children
	}
	for i, req := range bad {
		if _, err := m.SubmitAssetSet(req); err == nil {
			t.Fatalf("case %d: want validation error", i)
		}
	}
}

func TestSubmitAssetSetPartialFailure(t *testing.T) {
	// element + kit; kit's 5th call (0-based 5) fails. The set still
	// completes: the element composes, the kit's error lands in manifest.
	cap := &failCapture{FailCall: 5}
	m := New(composeTestOptsFail(t, t.TempDir(), cap))
	seed := int64(9)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "mix", Seed: &seed,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "dot", Prompt: "dot", Size: "32x32"},
			{Kind: "kit", Name: "tiles", Prompt: "tiles", Size: "32x32"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.Assets[0].Error != "" || len(man.Assets[0].Files) != 1 {
		t.Fatalf("element should have succeeded: %+v", man.Assets[0])
	}
	if man.Assets[1].Error == "" || man.Assets[1].Error != "sidecar boom" {
		t.Fatalf("kit error should be recorded: %+v", man.Assets[1])
	}
	if _, err := m.opts.Store.ReadSetFile(id, "dot.png"); err != nil {
		t.Fatalf("dot.png should exist: %v", err)
	}
}

func TestSubmitAssetSetAllFail(t *testing.T) {
	cap := &failCapture{FailAll: true}
	m := New(composeTestOptsFail(t, t.TempDir(), cap))
	seed := int64(3)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "none", Seed: &seed,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "a", Prompt: "a", Size: "32x32"},
			{Kind: "element", Name: "b", Prompt: "b", Size: "32x32"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	j, _ := m.Get(id)
	if !strings.Contains(j.Error, "compose_error") || !strings.Contains(j.Error, "all assets failed") {
		t.Fatalf("error: %q", j.Error)
	}
	if _, err := m.opts.Store.ReadSetFile(id, "manifest.json"); err != nil {
		t.Fatalf("manifest still written on total failure: %v", err)
	}
}

func TestSubmitAssetSetDelivery(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "proj")
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(11)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "icon", Seed: &seed, Dest: dest,
		Assets: []AssetSpecIn{{Kind: "element", Name: "icon", Prompt: "icon", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	for _, name := range []string{"icon.png", "manifest.json"} {
		if _, err := os.ReadFile(filepath.Join(dest, name)); err != nil {
			t.Fatalf("delivered %s: %v", name, err)
		}
	}
	mraw, _ := m.opts.Store.ReadSetFile(id, "manifest.json")
	var man setManifest
	_ = json.Unmarshal(mraw, &man)
	if len(man.Delivered) != 1 || man.Delivered[0] != filepath.Join(dest, "icon.png") {
		t.Fatalf("delivered record: %+v", man.Delivered)
	}
}

func TestSubmitAssetSetDeliveryCollision(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "icon.png"), []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(13)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "icon", Seed: &seed, Dest: dest,
		Assets: []AssetSpecIn{{Kind: "element", Name: "icon", Prompt: "icon", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	j, _ := m.Get(id)
	if !strings.Contains(j.Error, "compose_error") || !strings.Contains(j.Error, "already exists") {
		t.Fatalf("error: %q", j.Error)
	}
}
```

Also append these tiny helpers used above (package-level, `compose_test.go`):

```go
func ptr64(v int64) *int64 { return &v }

var one = []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "p", Size: "64x64"}}

var tooMany = func() []AssetSpecIn {
	var out []AssetSpecIn
	for i := 0; i < 9; i++ {
		out = append(out, AssetSpecIn{Kind: "element", Name: fmt.Sprintf("a%d", i), Prompt: "p", Size: "64x64"})
	}
	return out
}()

var bigChildren = func() []AssetSpecIn {
	// 8 atlases x 9 frames = 72 children > 64.
	var out []AssetSpecIn
	for i := 0; i < 8; i++ {
		out = append(out, AssetSpecIn{Kind: "atlas", Name: fmt.Sprintf("at%d", i), Prompt: "p", Size: "64x64", Frames: 9})
	}
	return out
}()
```

Add imports the file may lack: `"os"`, `"path/filepath"`, `"fmt"`, `"encoding/json"` — compose_test.go's Task 6 import block was `bytes`, `context`, `image`, `image/png`, `sync`, `testing` plus the `img-gen/internal/{prompting,storage}` imports, and these four join it for the manifest and set-dir assertions.

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/queue/ -run 'TestSubmitAssetSet' -v`
Expected: FAIL — `AssetSetRequest`/`SubmitAssetSet`/`setManifest` undefined.

- [ ] **Step 3: Implement — replace the Task 7 honest-failure stubs in `internal/queue/compose.go`**

Replace `composeAssetSet` (the "not implemented" stub) with the real executor, and append the request types, validation, child builders, and manifest helpers. `composeGIF` stays a stub until Task 9 (its test from Task 7 exercised asset-set only).

```go
// --- public asset-set submission ------------------------------------------

const (
	maxAssetsPerSet    = 8
	maxComposeChildren = 64
)

const (
	whiteBgSuffix = ", isolated on plain white background"
	blackBgSuffix = ", isolated on plain black background"
)

// AssetSetRequest is the public asset-set submission. Seed is required:
// a set's parts must be reproducible for iteration on a design.
type AssetSetRequest struct {
	Name   string        `json:"name"`           // file stem for the set (sanitized)
	Dest   string        `json:"dest,omitempty"` // absolute dir to deliver into; empty = EXPORT_DIR fallback chain
	Seed   *int64        `json:"seed"`           // required
	Assets []AssetSpecIn `json:"assets"`         // 1..8
}

type AssetSpecIn struct {
	Kind        string `json:"kind"` // "element" | "kit" | "atlas"
	Name        string `json:"name"`
	Prompt      string `json:"prompt"`
	Negative    string `json:"negative,omitempty"`
	Size        string `json:"size"` // WxH, multiples of 16
	Frames      int    `json:"frames,omitempty"`  // atlas: 2..64
	Columns     int    `json:"columns,omitempty"` // atlas: 0 = single row
	Transparent bool   `json:"transparent,omitempty"` // element: white+black double pass
}

// SubmitAssetSet validates, registers the compose parent (set id = job id),
// and submits the generation children. It returns as soon as children are
// queued — the caller polls the job or GETs the set.
func (m *Manager) SubmitAssetSet(req AssetSetRequest) (string, error) {
	setName := validComposeAssetName(req.Name)
	if setName == "" {
		return "", &ValidationError{Code: "validation_error", Msg: "name is required for asset sets"}
	}
	if req.Seed == nil {
		return "", &ValidationError{Code: "validation_error", Msg: "seed is required for asset sets"}
	}
	if req.Dest != "" && !filepath.IsAbs(req.Dest) {
		return "", &ValidationError{Code: "validation_error", Msg: "dest must be an absolute path"}
	}
	if len(req.Assets) == 0 || len(req.Assets) > maxAssetsPerSet {
		return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset sets take 1..%d assets", maxAssetsPerSet)}
	}
	spec := composeSpec{Kind: "asset-set", Name: setName, Dest: req.Dest}
	var kids []SubmitRequest
	seen := map[string]bool{}
	total := 0
	for _, a := range req.Assets {
		an := validComposeAssetName(a.Name)
		if an == "" {
			return "", &ValidationError{Code: "validation_error", Msg: "asset name is required"}
		}
		if seen[an] {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("duplicate asset name %q", an)}
		}
		seen[an] = true
		prompt := strings.TrimSpace(a.Prompt)
		if prompt == "" {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: prompt is required", an)}
		}
		if !validAssetSize(a.Size) {
			return "", &ValidationError{Code: "invalid_size", Msg: fmt.Sprintf("asset %q: invalid size %q (want WxH, multiples of 16)", an, a.Size)}
		}
		as := assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Frames: a.Frames, Columns: a.Columns, Transparent: a.Transparent}
		var built []SubmitRequest
		switch a.Kind {
		case "element":
			built = elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent)
		case "kit":
			built = kitChildren(prompt, a.Negative, a.Size, *req.Seed)
		case "atlas":
			if a.Frames < 2 || a.Frames > 64 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frames must be 2..64", an)}
			}
			built = atlasChildren(prompt, a.Negative, a.Size, *req.Seed, a.Frames)
		default:
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: unknown kind %q (want element, kit, atlas)", an, a.Kind)}
		}
		as.Children = len(built)
		total += as.Children
		spec.Assets = append(spec.Assets, as)
		kids = append(kids, built...)
	}
	if total > maxComposeChildren {
		return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset set needs %d generation jobs; limit is %d", total, maxComposeChildren)}
	}
	return m.submitComposeParent(newID(), spec, kids)
}

// elementChildren builds the 1 job (opaque) or 2 jobs (transparent: white
// and black pass, same seed — only the background prompt differs).
func elementChildren(prompt, negative, size string, seed int64, transparent bool) []SubmitRequest {
	if !transparent {
		s := seed
		return []SubmitRequest{{Mode: "asset", Prompt: prompt, NegativePrompt: negative, Size: size, Seed: &s}}
	}
	ws := seed
	bs := seed
	return []SubmitRequest{
		{Mode: "asset", Prompt: prompt + whiteBgSuffix, NegativePrompt: negative, Size: size, Seed: &ws},
		{Mode: "asset", Prompt: prompt + blackBgSuffix, NegativePrompt: negative, Size: size, Seed: &bs},
	}
}

// kitChildren builds one job per wang class, seeded seed+SeedOffset, with
// the class hint appended so each tile knows its edge role.
func kitChildren(prompt, negative, size string, seed int64) []SubmitRequest {
	kids := make([]SubmitRequest, 0, len(compose.KitClasses))
	for _, kc := range compose.KitClasses {
		s := seed + int64(kc.SeedOffset)
		p := prompt
		if kc.Hint != "" {
			p = prompt + ", " + kc.Hint
		}
		kids = append(kids, SubmitRequest{Mode: "asset", Prompt: p, NegativePrompt: negative, Size: size, Seed: &s})
	}
	return kids
}

// atlasChildren builds one job per animation frame, seeded seed+i.
func atlasChildren(prompt, negative, size string, seed, frames int) []SubmitRequest {
	kids := make([]SubmitRequest, 0, frames)
	for i := 0; i < frames; i++ {
		s := seed + int64(i)
		p := fmt.Sprintf("%s, animation frame %d of %d, consistent style and lighting", prompt, i+1, frames)
		kids = append(kids, SubmitRequest{Mode: "asset", Prompt: p, NegativePrompt: negative, Size: size, Seed: &s})
	}
	return kids
}
```

Then the executor — full replacement of the Task 7 stub:

```go
// --- manifest --------------------------------------------------------------

type setManifest struct {
	SetID     string          `json:"set_id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	CreatedAt time.Time       `json:"created_at"`
	Assets    []manifestAsset `json:"assets,omitempty"`
	GIF       *manifestGIF    `json:"gif,omitempty"`
	Delivered []string        `json:"delivered,omitempty"`
	Dest      string          `json:"dest,omitempty"`
}

type manifestAsset struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Prompt string   `json:"prompt,omitempty"`
	Size   string   `json:"size,omitempty"`
	Files  []string `json:"files,omitempty"`
	Error  string   `json:"error,omitempty"`
}

type manifestGIF struct {
	Name     string   `json:"name"`
	Files    []string `json:"files,omitempty"`
	DelayMS  int      `json:"delay_ms"`
	Loop     int      `json:"loop"`
	Formats  []string `json:"formats,omitempty"`
	WebP     string   `json:"webp,omitempty"` // "ok" | "unavailable: reason"
	Error    string   `json:"error,omitempty"`
}

func (m *Manager) saveManifest(id string, man *setManifest) {
	specJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		m.log(id, "manifest marshal: %v", err)
		return
	}
	if err := m.opts.Store.SaveSetFile(id, "manifest.json", specJSON); err != nil {
		m.log(id, "manifest save: %v", err)
	}
}

// composeAssetSet turns terminal children into set files, writes the
// manifest, delivers when a dest is set, and finishes the parent. Partial
// success: the set completes if ANY asset composed; per-asset errors are
// recorded in the manifest.
func (m *Manager) composeAssetSet(id string, spec *composeSpec, children []string) {
	man := setManifest{SetID: id, Kind: "asset-set", Name: spec.Name, CreatedAt: time.Now(), Assets: []manifestAsset{}}
	cursor := 0
	ok := 0
	var firstErr string
	var files []storage.FileRef
	for _, a := range spec.Assets {
		ma := manifestAsset{Kind: a.Kind, Name: a.Name, Prompt: a.Prompt, Size: a.Size}
		end := cursor + a.Children
		if cursor+a.Children > len(children) {
			ma.Error = fmt.Sprintf("expected %d child job(s), found %d", a.Children, len(children)-cursor)
			man.Assets = append(man.Assets, ma)
			if firstErr == "" {
				firstErr = ma.Error
			}
			continue
		}
		kids := children[cursor:end]
		cursor = end
		broken := ""
		imgs := make([][]byte, len(kids))
		for i, cid := range kids {
			st, em, png := m.childResult(cid)
			if st != "done" {
				broken = em
				break
			}
			imgs[i] = png
		}
		if broken == "" {
			var outs []compose.OutputFile
			var err error
			switch a.Kind {
			case "element":
				// ComposeElement yields one deliverable; kits and atlases yield many.
				var of compose.OutputFile
				if a.Transparent && len(imgs) == 2 {
					of, err = compose.ComposeElement(a.Name, imgs[0], imgs[1], true)
				} else {
					of, err = compose.ComposeElement(a.Name, imgs[0], nil, false)
				}
				if err == nil {
					outs = []compose.OutputFile{of}
				}
			case "kit":
				outs, err = compose.ComposeKit(a.Name, imgs)
			case "atlas":
				outs, err = compose.ComposeAtlas(a.Name, imgs, a.Columns)
			default:
				err = fmt.Errorf("unknown asset kind %q", a.Kind)
			}
			if err == nil {
				for _, of := range outs {
					if serr := m.opts.Store.SaveSetFile(id, of.Name, of.Data); serr != nil {
						err = serr
						break
					}
					files = append(files, storage.FileRef{Name: of.Name, Primary: of.Primary})
					ma.Files = append(ma.Files, of.Name)
				}
			}
			if err != nil {
				broken = err.Error()
			}
		}
		if broken != "" {
			ma.Error = broken
			if firstErr == "" {
				firstErr = broken
			}
		} else {
			ok++
		}
		man.Assets = append(man.Assets, ma)
	}
	m.mu.Lock()
	job := m.jobs[id]
	if job != nil {
		job.Files = files
		if ok > 0 {
			job.ImagePath = "sets/" + id + "/" + primaryName(files)
		}
		// Files are set on the in-memory job before finish() persists it, so
		// history and restore() carry the set outputs.
	}
	m.mu.Unlock()
	m.saveManifest(id, &man)
	if ok == 0 {
		m.finish(id, "failed", "compose_error: all assets failed: "+firstErr)
		return
	}
	if spec.Dest != "" {
		dest := m.resolveExportDest(spec.Dest)
		served, err := m.opts.Store.DeliverSet(id, dest)
		if err != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: %v", err))
			return
		}
		man.Delivered = served
		man.Dest = dest
		m.saveManifest(id, &man)
	}
	m.log(id, "compose done: %d/%d asset(s)", ok, len(spec.Assets))
	m.finish(id, "done", "")
}

// primaryName is the first Primary FileRef, falling back to the first file.
func primaryName(refs []storage.FileRef) string {
	for _, r := range refs {
		if r.Primary {
			return r.Name
		}
	}
	if len(refs) > 0 {
		return refs[0].Name
	}
	return ""
}
```

Note on `compose` import: Task 7 added `var _ = compose.KitClasses` as a compile hold — **delete that line** now that real references exist.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/queue/ -v`
Expected: PASS — all `TestSubmitAssetSet*`, plus Task 6/7 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/compose.go internal/queue/compose_test.go
git commit -m "feat(queue): asset-set submission, composition, manifest, delivery

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 9: queue — `SubmitGIF` + `composeGIF` executor

**Files:**
- Modify: `internal/queue/compose.go` (append; replaces the `composeGIF` stub)
- Test: `internal/queue/compose_test.go` (append)

**Interfaces:**
- Consumes: Task 1 `compose.EncodeGIF` (via `ComposeGIF`); Task 5 `storage.{ReadImage,ReadDataFile,SaveSetFile,ReadSetFile,DeliverSet}`; Task 7 `submitComposeParent/gifPayload/childrenResults/childResult/resolveExportDest/validComposeAssetName` and `composeGIF` stub; `queue.SubmitRequest` (the from-spec base request) and asset child mode from Task 6.
- Produces (Task 10 HTTP + Task 12 MCP):
  - `GifRequest{Name, Dest, DelayMS, Loop, Formats, Source GifSource}`; `GifSource{Type, Frames []FrameRef, Request *SubmitRequest, Count}`; `FrameRef{Job, Path, B64}`.
  - `SubmitGIF(req GifRequest) (string, error)` — parent job id = set id.
  - RIFE keyframes: rejected at submit with `not_implemented` and the message `keyframes interpolation requires the RIFE sidecar (RIFE_URL); planned for phase 3` (spec: Phase 3 out of scope; validate now so the request shape is stable).

- [ ] **Step 1: Write the failing tests** (append to `internal/queue/compose_test.go`)

```go
func TestSubmitGIFFromFrames(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	j1, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "f1", Size: "32x32"})
	if err != nil {
		t.Fatal(err)
	}
	j2, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "f2", Size: "32x32"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j1, "done")
	waitFor(t, m, j2, "done")
	id, err := m.SubmitGIF(GifRequest{
		Name: "spin",
		Source: GifSource{Type: "from_frames", Frames: []FrameRef{{Job: j1}, {Job: j2}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.opts.Store.ReadSetFile(id, "source-frame-0001.png"); err != nil {
		t.Fatalf("source frames persisted at submit: %v", err)
	}
	waitFor(t, m, id, "done")
	raw, err := m.opts.Store.ReadSetFile(id, "spin.gif")
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("spin.gif not a GIF: %v", err)
	}
	if len(g.Image) != 2 {
		t.Fatalf("gif frames: %d", len(g.Image))
	}
	if g.Delay[0] != 10 { // default delay_ms=100 → centiseconds
		t.Fatalf("delay %d", g.Delay[0])
	}
	j, _ := m.Get(id)
	if len(j.Files) != 1 || j.Files[0].Name != "spin.gif" || !j.Files[0].Primary {
		t.Fatalf("files: %+v", j.Files)
	}
}

func TestSubmitGIFFromSpec(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(30)
	id, err := m.SubmitGIF(GifRequest{
		Name: "pulse",
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "pulse orb", Size: "32x32", Seed: &seed},
			Count:   4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 4 || cap.Seeds[0] != 30 || cap.Seeds[3] != 33 {
		t.Fatalf("from_spec children: %q %q", cap.Prompts, cap.Seeds)
	}
	if !strings.Contains(cap.Prompts[1], "frame 2 of 4") {
		t.Fatalf("frame prompt: %q", cap.Prompts[1])
	}
	cap.mu.Unlock()
	raw, _ := m.opts.Store.ReadSetFile(id, "pulse.gif")
	if _, err := gif.DecodeAll(bytes.NewReader(raw)); err != nil {
		t.Fatalf("pulse.gif: %v", err)
	}
}

func TestSubmitGIFPathRef(t *testing.T) {
	cap := &genCapture{}
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A real decodable PNG under the data dir the path ref points at.
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveImage("abcdef0123456789", buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	m := New(composeTestOpts(t, store.dir, cap)) // store shared by queue and refs
	id, err := m.SubmitGIF(GifRequest{
		Name: "via-path",
		DelayMS: 50,
		Source: GifSource{Type: "from_frames", Frames: []FrameRef{
			{Path: "images/abcdef0123456789.png"},
			{Path: "images/abcdef0123456789.png"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	raw, _ := m.opts.Store.ReadSetFile(id, "via-path.gif")
	if _, err := gif.DecodeAll(bytes.NewReader(raw)); err != nil {
		t.Fatalf("via-path.gif: %v", err)
	}
}

func TestSubmitGIFWebPFormatRecorded(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(2)
	id, err := m.SubmitGIF(GifRequest{
		Name: "webp", Formats: []string{"gif", "webp"},
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "w", Size: "32x32", Seed: &seed},
			Count:   2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done") // webp availability never fails the set
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.GIF == nil || man.GIF.WebP == "" {
		t.Fatalf("webp status must be recorded: %+v", man.GIF)
	}
}

func TestSubmitGIFFrameIncomplete(t *testing.T) {
	cap := &failCapture{FailCall: 1}
	m := New(composeTestOptsFail(t, t.TempDir(), cap))
	seed := int64(4)
	id, err := m.SubmitGIF(GifRequest{
		Name: "gaps",
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "g", Size: "32x32", Seed: &seed},
			Count:   3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	j, _ := m.Get(id)
	if !strings.Contains(j.Error, "compose_error") || !strings.Contains(j.Error, "frame") {
		t.Fatalf("error: %q", j.Error)
	}
}

func TestSubmitGIFValidation(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(1)
	base := &SubmitRequest{Mode: "asset", Prompt: "p", Size: "32x32", Seed: &seed}
	cases := []struct {
		name string
		req  GifRequest
		want string // substring of the error message
	}{
		{"keyframes not implemented", GifRequest{Name: "k", Source: GifSource{Type: "keyframes", Count: 4}}, "RIFE"},
		{"source type", GifRequest{Name: "k", Source: GifSource{Type: "wat"}}, "from_frames, from_spec"},
		{"one frame", GifRequest{Name: "k", Source: GifSource{Type: "from_frames", Frames: []FrameRef{{Job: "x"}}}}, "2..64"},
		{"no request", GifRequest{Name: "k", Source: GifSource{Type: "from_spec", Count: 3}}, "request"},
		{"nil seed", GifRequest{Name: "k", Source: GifSource{Type: "from_spec", Request: &SubmitRequest{Mode: "asset", Prompt: "p", Size: "32x32"}, Count: 3}}, "seed"},
		{"count high", GifRequest{Name: "k", Source: GifSource{Type: "from_spec", Request: base, Count: 65}}, "2..64"},
		{"count low", GifRequest{Name: "k", Source: GifSource{Type: "from_spec", Request: base, Count: 1}}, "2..64"},
		{"delay low", GifRequest{Name: "k", DelayMS: 5, Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "delay_ms"},
		{"delay high", GifRequest{Name: "k", DelayMS: 6000, Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "delay_ms"},
		{"loop negative", GifRequest{Name: "k", Loop: -1, Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "loop"},
		{"format", GifRequest{Name: "k", Formats: []string{"apng"}, Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "formats"},
		{"dest", GifRequest{Name: "k", Dest: "rel", Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "absolute"},
		{"multi source", GifRequest{Name: "k", Source: GifSource{Type: "from_frames", Frames: []FrameRef{{Job: "a", B64: "AA"}, {Job: "b"}}}}, "exactly one"},
	}
	for _, tc := range cases {
		_, err := m.SubmitGIF(tc.req)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want substring %q", tc.name, err, tc.want)
		}
	}
	// traversal: a path source escaping the data dir is a submit-time error
	_, err := m.SubmitGIF(GifRequest{Name: "k", Source: GifSource{Type: "from_frames", Frames: []FrameRef{
		{Path: "../../etc/passwd"}, {Path: "images/x.png"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "data directory") {
		t.Fatalf("traversal: %v", err)
	}
	// and an unknown job id produces a submit-time error, not a stuck parent
	_, err = m.SubmitGIF(GifRequest{Name: "k2", Source: GifSource{Type: "from_frames", Frames: []FrameRef{
		{Job: "deadbeefdeadbeef"}, {Job: "deadbeefdeadbeef"},
	}}})
	if err == nil {
		t.Fatal("unknown job frame ref should fail at submit")
	}
	// every submit above failed at validation or frame resolution: no parent
	// and no partial child set may have been registered.
	if n := len(m.List()); n != 0 {
		t.Fatalf("failed submits left %d job(s) registered", n)
	}
}
```

Add imports if missing: `"image/gif"`.

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/queue/ -run 'TestSubmitGIF' -v`
Expected: FAIL — `GifRequest`/`SubmitGIF`/`FrameRef` undefined.

- [ ] **Step 3: Implement in `internal/queue/compose.go`** (replace the Task 7 `composeGIF` stub)

```go
// --- public gif submission -------------------------------------------------

const (
	defaultGifDelayMS = 100
	minGifDelayMS     = 10
	maxGifDelayMS     = 5000
)

// GifRequest builds an animated GIF. Three source modes:
// from_frames (assemble given images), from_spec (generate the frames here),
// keyframes (interpolate between keyframes — requires the phase-3 RIFE
// sidecar and is rejected with not_implemented for now).
type GifRequest struct {
	Name    string    `json:"name"`
	Dest    string    `json:"dest,omitempty"`
	DelayMS int       `json:"delay_ms,omitempty"` // 10..5000; 0 = default 100
	Loop    int       `json:"loop,omitempty"`     // >= 0; 0 = infinite loop
	Formats []string  `json:"formats,omitempty"`  // subset of {gif,webp}; default ["gif"]
	Source  GifSource `json:"source"`
}

type GifSource struct {
	Type    string         `json:"type"` // from_frames | from_spec | keyframes
	Frames  []FrameRef     `json:"frames,omitempty"`   // from_frames: 2..64 sources
	Request *SubmitRequest `json:"request,omitempty"`  // from_spec: base asset request
	Count   int            `json:"count,omitempty"`    // from_spec: 2..64 frames
}

// FrameRef is one source frame: exactly one of Job (an img-gen job id),
// Path (data-dir-relative, no "..", no absolute paths), or B64 (inline PNG).
type FrameRef struct {
	Job  string `json:"job,omitempty"`
	Path string `json:"path,omitempty"`
	B64  string `json:"b64,omitempty"`
}

// resolveFrameRef fetches the PNG bytes for a source frame. Decodability is
// checked here so bad sources fail the submit, not the compose.
func (m *Manager) resolveFrameRef(ref FrameRef) ([]byte, error) {
	set := 0
	for _, s := range []string{ref.Job, ref.Path, ref.B64} {
		if s != "" {
			set++
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("frame source needs exactly one of job, path, b64")
	}
	var data []byte
	var err error
	switch {
	case ref.Job != "":
		data, err = m.opts.Store.ReadImage(ref.Job)
	case ref.Path != "":
		if filepath.IsAbs(ref.Path) || strings.Contains(ref.Path, "..") {
			return nil, fmt.Errorf("path source must be relative to the data directory without '..'")
		}
		data, err = m.opts.Store.ReadDataFile(ref.Path)
	default:
		data, err = base64.StdEncoding.DecodeString(ref.B64)
	}
	if err != nil {
		return nil, err
	}
	if _, derr := png.Decode(bytes.NewReader(data)); derr != nil {
		return nil, fmt.Errorf("frame is not a decodable PNG: %v", derr)
	}
	return data, nil
}

// SubmitGIF registers the compose parent and queues frame generation when
// the source is from_spec. from_frames resolves and persists every source
// into the set at submit time (source-frame-%04d.png) so a restart can
// recompose without re-fetching.
func (m *Manager) SubmitGIF(req GifRequest) (string, error) {
	name := validComposeAssetName(req.Name)
	if name == "" {
		return "", &ValidationError{Code: "validation_error", Msg: "name is required for gifs"}
	}
	delay := req.DelayMS
	if delay == 0 {
		delay = defaultGifDelayMS
	}
	if delay < minGifDelayMS || delay > maxGifDelayMS {
		return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("delay_ms must be %d..%d (0 = default %d)", minGifDelayMS, maxGifDelayMS, defaultGifDelayMS)}
	}
	if req.Loop < 0 {
		return "", &ValidationError{Code: "validation_error", Msg: "loop must be >= 0 (0 = infinite)"}
	}
	formats := req.Formats
	if len(formats) == 0 {
		formats = []string{"gif"}
	}
	for _, f := range formats {
		if f != "gif" && f != "webp" {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("formats must be a subset of [gif, webp], got %q", f)}
		}
	}
	if req.Dest != "" && !filepath.IsAbs(req.Dest) {
		return "", &ValidationError{Code: "validation_error", Msg: "dest must be an absolute path"}
	}
	spec := composeSpec{Kind: "gif", Name: name, Dest: req.Dest, GIF: &gifPayload{DelayMS: delay, Loop: req.Loop, Formats: formats}}
	switch req.Source.Type {
	case "keyframes":
		return "", &ValidationError{Code: "not_implemented", Msg: "keyframes interpolation requires the RIFE sidecar (RIFE_URL); planned for phase 3"}
	case "from_frames":
		if len(req.Source.Frames) < 2 || len(req.Source.Frames) > 64 {
			return "", &ValidationError{Code: "validation_error", Msg: "from_frames takes 2..64 frames"}
		}
		id := newID()
		names := make([]string, 0, len(req.Source.Frames))
		for i, ref := range req.Source.Frames {
			data, err := m.resolveFrameRef(ref)
			if err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("frame %d: %v", i, err)}
			}
			fname := fmt.Sprintf("source-frame-%04d.png", i)
			if err := m.opts.Store.SaveSetFile(id, fname, data); err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("frame %d: %v", i, err)}
			}
			names = append(names, fname)
		}
		spec.GIF.Frames = names
		return m.submitComposeParent(id, spec, nil)
	case "from_spec":
		base := req.Source.Request
		if base == nil {
			return "", &ValidationError{Code: "validation_error", Msg: "from_spec needs source.request"}
		}
		if base.Seed == nil {
			return "", &ValidationError{Code: "validation_error", Msg: "seed is required for generated gif frames"}
		}
		if strings.TrimSpace(base.Prompt) == "" {
			return "", &ValidationError{Code: "validation_error", Msg: "prompt is required for generated gif frames"}
		}
		if !validAssetSize(base.Size) {
			return "", &ValidationError{Code: "invalid_size", Msg: fmt.Sprintf("invalid size %q for gif frames (want WxH, multiples of 16)", base.Size)}
		}
		if req.Source.Count < 2 || req.Source.Count > 64 {
			return "", &ValidationError{Code: "validation_error", Msg: "gif frames must be 2..64"}
		}
		return m.submitComposeParent(newID(), spec, specFrames(*base, req.Source.Count))
	default:
		return "", &ValidationError{Code: "validation_error", Msg: "source.type must be from_frames, from_spec, or keyframes"}
	}
}

// specFrames mirrors atlasChildren over an arbitrary base request: one
// asset-mode job per frame, seed+i, frame index appended for consistency.
func specFrames(base SubmitRequest, count int) []SubmitRequest {
	kids := make([]SubmitRequest, 0, count)
	for i := 0; i < count; i++ {
		c := base
		s := *base.Seed + int64(i)
		c.Seed = &s
		c.Mode = "asset"
		c.Prompt = fmt.Sprintf("%s, animation frame %d of %d, consistent style and lighting", strings.TrimSpace(c.Prompt), i+1, count)
		kids = append(kids, c)
	}
	return kids
}
```

Then the executor — full replacement of the Task 7 stub:

```go
// composeGIF assembles the set's GIF. Unlike asset sets a GIF is one
// artifact: every frame must be present, so any failed child fails the
// parent. webp unavailability is recorded, not fatal.
func (m *Manager) composeGIF(id string, spec *composeSpec, children []string) {
	gp := spec.GIF
	man := setManifest{SetID: id, Kind: "gif", Name: spec.Name, CreatedAt: time.Now(),
		GIF: &manifestGIF{Name: spec.Name, DelayMS: gp.DelayMS, Loop: gp.Loop, Formats: gp.Formats}}
	var frames []image.Image
	switch {
	case len(gp.Frames) > 0:
		for i, name := range gp.Frames {
			data, err := m.opts.Store.ReadSetFile(id, name)
			if err != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: source frame %d: %v", i, err))
				return
			}
			im, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: source frame %d: %v", i, err))
				return
			}
			frames = append(frames, im)
		}
	case len(children) > 0:
		imgs, errs := m.childrenResults(len(children), children, func(f string, a ...any) { m.log(id, f, a...) })
		if len(imgs) == 0 {
			m.finish(id, "failed", "compose_error: gif frames: "+errs[0])
			return
		}
		for i, img := range imgs {
			if img == nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: gif frame %d incomplete: %s", i, strings.Join(errs, "; ")))
				return
			}
			im, err := png.Decode(bytes.NewReader(img))
			if err != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: gif frame %d: %v", i, err))
				return
			}
			frames = append(frames, im)
		}
	default:
		m.finish(id, "failed", "compose_error: gif spec has neither frames nor children")
		return
	}
	outs, _, webpStatus, err := compose.ComposeGIF(spec.Name, frames, gp.DelayMS, gp.Loop, gp.Formats)
	if err != nil {
		m.finish(id, "failed", fmt.Sprintf("compose_error: %v", err))
		return
	}
	man.GIF.WebP = webpStatus
	var files []storage.FileRef
	for _, of := range outs {
		if err := m.opts.Store.SaveSetFile(id, of.Name, of.Data); err != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: %v", err))
			return
		}
		files = append(files, storage.FileRef{Name: of.Name, Primary: of.Primary})
		man.GIF.Files = append(man.GIF.Files, of.Name)
	}
	m.mu.Lock()
	job := m.jobs[id]
	if job != nil {
		job.Files = files
		job.ImagePath = "sets/" + id + "/" + primaryName(files)
	}
	m.mu.Unlock()
	m.saveManifest(id, &man)
	if spec.Dest != "" {
		dest := m.resolveExportDest(spec.Dest)
		served, err := m.opts.Store.DeliverSet(id, dest)
		if err != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: %v", err))
			return
		}
		man.Delivered = served
		man.Dest = dest
		m.saveManifest(id, &man)
	}
	m.log(id, "compose done: gif %s (%d frames)", spec.Name, len(frames))
	m.finish(id, "done", "")
}
```

**Import note for compose.go:** Task 7's compose.go imports `img-gen/internal/compose` only; this task adds `"bytes"`, `"encoding/base64"`, `"image"`, `"image/png"` to its import block (compose.go is the only queue file allowed to import image/png — it sits under the `internal/compose` boundary, which returns PNG bytes). `childrenResults` (Task 7) becomes live here — GIF is its first caller (asset-set gathers inline because it treats child failure as per-asset, not whole-set).

Note: `childrenResults`' defensive drift check (`len(ids) != specChildren`) can only fire on corrupted state — `composeGIF` passes `len(children)` as specChildren, so the drift path is unreachable here.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/queue/ -v`
Expected: PASS — all queue tests.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/compose.go internal/queue/compose_test.go
git commit -m "feat(queue): gif submission — from_frames, from_spec, keyframes gate

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 10: v1 HTTP — `POST /api/v1/asset-sets`, `POST /api/v1/gifs`

**Files:**
- Modify: `cmd/img-gen/v1.go`
- Test: `cmd/img-gen/v1_compose_test.go` (create)

**Interfaces:**
- Consumes: Task 8 `queue.SubmitAssetSet`, Task 9 `queue.SubmitGIF`; `mgr.Wait` (existing); `v1SubmitErr`/`writeV1Err` (existing, v1.go).
- Produces:
  - `POST /api/v1/asset-sets` with body `{"name","dest?","seed","assets":[...],"wait_seconds"?}` → 202 `{"job_id","set_id"}` (both equal) or, when `wait_seconds > 0`, 200 with the final job JSON / 504 `sidecar_timeout`.
  - `POST /api/v1/gifs` with body `{"name","dest?","delay_ms?","loop?","formats?","source":{...},"wait_seconds"?}` → same contract.
  - `waitForCompose` — the shared wait branch (mirrors `handleV1Generate`'s).
  - GET `/api/v1/sets/...` lands in Task 11.

- [ ] **Step 1: Write the failing tests** (create `cmd/img-gen/v1_compose_test.go`)

The existing mock in `newV1Server` (v1_test.go) returns `"fake-png-bytes"`, which compose would fail to decode — this file has its own harness with a real PNG.

```go
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newV1ComposeServer is newV1Server with a real 8x8 PNG payload: compose
// parents decode child bytes, so the fake-png harness would fail every set.
func newV1ComposeServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	real := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, real); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			w.Write([]byte(`{"data":[{"b64_json":"` + b64 + `","seed":7}]}`))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"enhanced prompt"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	cfg := config{
		LatticeURL:   mock.URL,
		ImageURL:     mock.URL,
		DataDir:      t.TempDir(),
		GenresFile:   writeGenres(t),
		ModelsFile:   writeModels(t),
		PresetsFile:  writePresets(t),
		EnhanceModel: "flux-dev",
		ImageTimeout: 5 * time.Second,
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, mock.URL
}

func postV1(t *testing.T, srv *httptest.Server, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	return resp.StatusCode, out
}

const assetSetBody = `{"name":"bolt","seed":1,"assets":[{"kind":"element","name":"bolt","prompt":"hex bolt","size":"16x16"}],"wait_seconds":15}`

func TestV1AssetSetAndWait(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	_, out := postV1(t, srv, "/api/v1/asset-sets", assetSetBody)
	if out["status"] != "done" {
		t.Fatalf("job: %v", out)
	}
	files, ok := out["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files: %v", out["files"])
	}
	f := files[0].(map[string]any)
	if f["name"] != "bolt.png" || f["primary"] != true {
		t.Fatalf("file ref: %v", f)
	}
	if out["set_id"] == "" || out["set_id"] != out["id"] {
		t.Fatalf("set id: %v", out)
	}
}

func TestV1AssetSetNoWait(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	body := strings.Replace(assetSetBody, `"wait_seconds":15`, `"wait_seconds":0`, 1)
	code, out := postV1(t, srv, "/api/v1/asset-sets", body)
	if code != http.StatusAccepted {
		t.Fatalf("status %d: %v", code, out)
	}
	id, ok := out["job_id"].(string)
	if !ok || id == "" || out["set_id"] != id {
		t.Fatalf("no-wait body: %v", out)
	}
	waitForJobDone(t, srv, id)
}

func TestV1GIFFromSpecAndWait(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	body := `{"name":"pulse","seed":5,"source":{"type":"from_spec","request":{"mode":"asset","prompt":"pulse orb","size":"16x16","seed":5},"count":3},"wait_seconds":15}`
	_, out := postV1(t, srv, "/api/v1/gifs", body)
	if out["status"] != "done" {
		t.Fatalf("job: %v", out)
	}
}

func TestV1ComposeValidationEnvelope(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	// asset set without seed → 400 validation_error
	code, out := postV1(t, srv, "/api/v1/asset-sets", `{"name":"x","assets":[]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
	e := out["error"].(map[string]any)
	if e["code"] != "validation_error" {
		t.Fatalf("code: %v", e)
	}
	// keyframes gif → 400 not_implemented
	code, out = postV1(t, srv, "/api/v1/gifs", `{"name":"k","source":{"type":"keyframes","count":4}}`)
	if code != http.StatusBadRequest || out["error"].(map[string]any)["code"] != "not_implemented" {
		t.Fatalf("keyframes: %d %v", code, out)
	}
}
```

Note the writeModels/writeGenres/writePresets helpers and `waitForJobDone` come from the existing test files in this package — no duplicates.

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./cmd/img-gen/ -run 'TestV1AssetSet|TestV1GIF|TestV1Compose' -v`
Expected: FAIL — 404 routes (`/api/v1/asset-sets` has no handler yet).

- [ ] **Step 3: Implement in `cmd/img-gen/v1.go`**

In `registerV1`, after the `/api/v1/export` block:

```go
	mux.HandleFunc("/api/v1/asset-sets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
			return
		}
		d.handleV1AssetSet(w, r)
	})

	mux.HandleFunc("/api/v1/gifs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
			return
		}
		d.handleV1GIF(w, r)
	})
```

After `handleV1Export`'s closing brace:

```go
type setAndWaitRequest struct {
	queue.AssetSetRequest
	WaitSeconds int `json:"wait_seconds"`
}

type gifAndWaitRequest struct {
	queue.GifRequest
	WaitSeconds int `json:"wait_seconds"`
}

// handleV1AssetSet submits an asset set. wait_seconds > 0 blocks on the
// parent job; otherwise the caller gets the ids and polls. The set id equals
// the job id, so GET /api/v1/sets/{id} takes either value.
func (d *v1Deps) handleV1AssetSet(w http.ResponseWriter, r *http.Request) {
	var req setAndWaitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body", "")
		return
	}
	id, err := d.mgr.SubmitAssetSet(req.AssetSetRequest)
	if err != nil {
		v1SubmitErr(w, err)
		return
	}
	waitForCompose(w, d.mgr, id, req.WaitSeconds)
}

func (d *v1Deps) handleV1GIF(w http.ResponseWriter, r *http.Request) {
	var req gifAndWaitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body", "")
		return
	}
	id, err := d.mgr.SubmitGIF(req.GifRequest)
	if err != nil {
		v1SubmitErr(w, err)
		return
	}
	waitForCompose(w, d.mgr, id, req.WaitSeconds)
}

// waitForCompose is the shared wait branch for the compose endpoints:
// 202 {"job_id","set_id"} when not waiting, the final job JSON otherwise,
// 504 sidecar_timeout when wait_seconds elapses first.
func waitForCompose(w http.ResponseWriter, mgr *queue.Manager, id string, waitSeconds int) {
	if waitSeconds <= 0 {
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"job_id": id, "set_id": id})
		return
	}
	j, done := mgr.Wait(id, time.Duration(waitSeconds)*time.Second)
	if !done {
		writeV1Err(w, http.StatusGatewayTimeout, "sidecar_timeout", "not terminal within wait_seconds", id)
		return
	}
	writeJSON(w, j)
}
```

v1.go already imports everything used (`encoding/json`, `net/http`, `time`, `queue`).

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./cmd/img-gen/ -v`
Expected: PASS (all existing tests too — legacy `/api/*` untouched).

- [ ] **Step 5: Commit**

```bash
git add cmd/img-gen/v1.go cmd/img-gen/v1_compose_test.go
git commit -m "feat(v1): asset-set and gif submit endpoints with wait_seconds

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 11: v1 HTTP — `GET /api/v1/sets/{id}` and `GET /api/v1/sets/{id}/files/{name}`

**Files:**
- Modify: `cmd/img-gen/v1.go`
- Test: `cmd/img-gen/v1_compose_test.go` (append)

**Interfaces:**
- Consumes: Task 10 routes/harness; Task 5 `storage.ValidImageID`, `store.ReadSetFile`; `mgr.Get`.
- Produces (Task 12 MCP reads sets this way):
  - `GET /api/v1/sets/{id}` → `{"set_id": id, "job": <job json>, "files": [<FileRef json>]}`; 404 `job_not_found` "set not found" when unknown OR not a compose parent (`Mode != "compose"`).
  - `GET /api/v1/sets/{id}/files/{name}` → raw bytes with type by suffix (png/gif/webp/json/octet-stream); `?download=1` adds `Content-Disposition`; 404 on missing, traversal, or malformed names (Task 5's `ReadSetFile` rules enforce the set-file regex and the `..` ban).

- [ ] **Step 1: Write the failing tests** (append to `v1_compose_test.go`)

```go
func TestV1SetGet(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	_, out := postV1(t, srv, "/api/v1/asset-sets", assetSetBody)
	if out["status"] != "done" {
		t.Fatalf("job: %v", out)
	}
	setID := out["set_id"].(string)
	resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["set_id"] != setID {
		t.Fatalf("set_id: %v", body["set_id"])
	}
	job := body["job"].(map[string]any)
	if job["status"] != "done" || job["mode"] != "compose" {
		t.Fatalf("job: %v", job)
	}
	files := body["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["name"] != "bolt.png" {
		t.Fatalf("files: %v", files)
	}
}

func TestV1SetNotFound(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	code, out := postGet(t, srv, "/api/v1/sets/0000000000000000")
	if code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", code)
	}
	if out["error"].(map[string]any)["code"] != "job_not_found" {
		t.Fatalf("code: %v", out)
	}
	// a non-compose job id must not read as a set
	sub := v1Submit(t, srv, `{"mode":"asset","prompt":"plain","size":"64x64","seed":1}`, "")
	plain := sub["job_id"].(string)
	waitForJobDone(t, srv, plain)
	code, out = postGet(t, srv, "/api/v1/sets/"+plain)
	if code != http.StatusNotFound {
		t.Fatalf("non-compose set: %d", code)
	}
}

func TestV1SetFileServe(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	_, out := postV1(t, srv, "/api/v1/asset-sets", assetSetBody)
	if out["status"] != "done" {
		t.Fatalf("job: %v", out)
	}
	setID := out["set_id"].(string)
	func() {
		resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID + "/files/bolt.png")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("png: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if _, err := png.Decode(resp.Body); err != nil {
			t.Fatalf("served body not a png: %v", err)
		}
	}()
	// manifest.json serves as json
	func() {
		resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID + "/files/manifest.json")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("json content type: %s", resp.Header.Get("Content-Type"))
		}
	}()
	// download param adds the disposition header
	func() {
		resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID + "/files/bolt.png?download=1")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Content-Disposition") == "" {
			t.Fatal("download=1 must set Content-Disposition")
		}
	}()
	// missing file, name with ".." (the Task 5 rule reaches HTTP), bad set id
	for _, p := range []string{"/files/nope.png", "/files/x..y.png"} {
		resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID + p)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("expected 404 for %s", p)
		}
		resp.Body.Close()
	}
	resp, err := http.Get(srv.URL + "/api/v1/sets/not-hex/files/bolt.png")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad set id: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// postGet issues a GET and decodes the envelope-shaped body regardless of
// status, so 404 tests can assert on the error envelope.
func postGet(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	return resp.StatusCode, out
}

```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./cmd/img-gen/ -run 'TestV1Set' -v`
Expected: FAIL — `/api/v1/sets/` unrouted ( ServeMux returns 404 HTML, so `postGet` errors decoding it).

- [ ] **Step 3: Implement in `cmd/img-gen/v1.go`**

In `registerV1`, after the `/api/v1/gifs` block:

```go
	mux.HandleFunc("/api/v1/sets/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sets/")
		var setID, file string
		if i := strings.Index(rest, "/"); i >= 0 {
			setID, file = rest[:i], rest[i+1:]
		} else {
			setID = rest
		}
		if strings.HasPrefix(file, "files/") && len(file) > len("files/") {
			d.serveSetFile(w, r, setID, strings.TrimPrefix(file, "files/"))
			return
		}
		if file != "" {
			writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
			return
		}
		d.serveSet(w, r, setID)
	})
```

After `waitForCompose`:

```go
// serveSet returns the parent job and its file refs. Anything that is not a
// compose parent reads as "set not found" so plain jobs can't be enumerated
// as sets.
func (d *v1Deps) serveSet(w http.ResponseWriter, r *http.Request, setID string) {
	if !storage.ValidImageID(setID) {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
		return
	}
	j, ok := d.mgr.Get(setID)
	if !ok || j.Mode != "compose" {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
		return
	}
	writeJSON(w, map[string]any{"set_id": setID, "job": j, "files": j.Files})
}

// setContentType maps a set file's suffix to its serving type; unknown
// suffixes download as octet-stream rather than guessing.
func setContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".gif"):
		return "image/gif"
	case strings.HasSuffix(name, ".webp"):
		return "image/webp"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// serveSetFile streams one set file. ReadSetFile enforces the set-file name
// rule and the data-dir boundary, so traversal and malformed names 404 here.
func (d *v1Deps) serveSetFile(w http.ResponseWriter, r *http.Request, setID, name string) {
	if !storage.ValidImageID(setID) {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
		return
	}
	j, ok := d.mgr.Get(setID)
	if !ok || j.Mode != "compose" {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
		return
	}
	data, err := d.store.ReadSetFile(setID, name)
	if err != nil {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set file not found", setID)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	}
	w.Header().Set("Content-Type", setContentType(name))
	_, _ = w.Write(data)
}
```

v1.go already imports `storage`, `fmt`, `strings`.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./cmd/img-gen/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/img-gen/v1.go cmd/img-gen/v1_compose_test.go
git commit -m "feat(v1): set metadata and set-file serving endpoints

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 12: MCP — `request_asset_set`, `make_gif`, `read_export`

**Files:**
- Modify: `cmd/img-gen-mcp/main.go`
- Test: `cmd/img-gen-mcp/main_test.go` (append)

**Interfaces:**
- Consumes: Task 10 REST routes (`POST /api/v1/asset-sets`, `POST /api/v1/gifs`, `wait_seconds`), Task 11 set endpoints (`GET /api/v1/sets/{id}`, `GET /api/v1/sets/{id}/files/{name}`); existing MCP plumbing (`tools` slice, `callTool`, `textCall`, `raw`, `roundTrip`, `defaultWaitSecs` = 900).
- Produces:
  - Three new tools appended to `tools` (tools/list picks them up automatically).
  - `composeCall(path, args)` — shared request-merge + default-wait + preview logic; `withPreview(res)`; `jobView`/`fileRefView`; `mimeBySuffix`.
  - `request_asset_set` / `make_gif` return text (job JSON) **plus** an inline image preview of the primary set file when status is `done`; not-done returns text only (agents poll `get_job`).
  - `read_export` returns text (set view) + one content block per FileRef (images inline; JSON as text) + `manifest.json` as text.

- [ ] **Step 1: Write the failing tests** (append to `cmd/img-gen-mcp/main_test.go`)

```go
// composeMock backs a backend with the compose REST surface and a real PNG
// for previews. It returns the mock and the last asset-set body it saw.
func composeMock(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	seen := new(string)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/asset-sets":
			b, _ := io.ReadAll(r.Body)
			*seen = string(b)
			w.Write([]byte(`{"id":"set0001","set_id":"set0001","status":"done","files":[{"name":"bolt.png","primary":true}]}`))
		case r.URL.Path == "/api/v1/gifs":
			w.Write([]byte(`{"id":"set0002","set_id":"set0002","status":"done","files":[{"name":"spin.gif","primary":true}]}`))
		case r.URL.Path == "/api/v1/sets/set0001":
			w.Write([]byte(`{"set_id":"set0001","job":{"id":"set0001","status":"done"},"files":[{"name":"bolt.png","primary":true},{"name":"walk-atlas.json","primary":false}]}`))
		case r.URL.Path == "/api/v1/sets/set0001/files/bolt.png":
			w.Write(buf.Bytes())
		case r.URL.Path == "/api/v1/sets/set0001/files/walk-atlas.json":
			w.Write([]byte(`{"frames":{}}`))
		case r.URL.Path == "/api/v1/sets/set0002/files/spin.gif":
			w.Write(buf.Bytes())
		case strings.HasPrefix(r.URL.Path, "/api/v1/sets/set0001/files/") && strings.HasSuffix(r.URL.Path, "manifest.json"):
			w.Write([]byte(`{"set_id":"set0001","kind":"asset-set"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	return mock, seen
}

func callOnce(t *testing.T, mockURL, args string) map[string]any {
	t.Helper()
	resp := roundTrip(t, mcpcfg{baseURL: mockURL},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"request_asset_set","arguments":`+args+`}}`)
	if resp[0]["error"] != nil {
		t.Fatalf("protocol error: %v", resp[0]["error"])
	}
	return resp[0]["result"].(map[string]any)
}

func TestRequestAssetSetTool(t *testing.T) {
	mock, seen := composeMock(t)
	r := callOnce(t, mock.URL, `{"request":{"name":"bolt","seed":1,"assets":[{"kind":"element","name":"bolt","prompt":"hex bolt","size":"16x16"}]}}`)
	cs := r["content"].([]any)
	if len(cs) != 2 {
		t.Fatalf("want text + preview, got %d: %v", len(cs), cs)
	}
	text := cs[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"set_id":"set0001"`) {
		t.Fatalf("text: %s", text)
	}
	img := cs[1].(map[string]any)
	if img["type"] != "image" || img["mimeType"] != "image/png" {
		t.Fatalf("preview: %v", img)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(*seen), &body); err != nil {
		t.Fatal(err)
	}
	if body["wait_seconds"] != float64(defaultWaitSecs) {
		t.Fatalf("default wait: %v", body["wait_seconds"])
	}
	if _, ok := body["name"].(string); !ok || body["name"] != "bolt" {
		t.Fatalf("request passthrough: %v", body)
	}
}

func TestRequestAssetSetNotDone(t *testing.T) {
	// status generating → text only, no preview call
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"set0009","set_id":"set0009","status":"queued"}`))
	}))
	defer mock.Close()
	r := callOnce(t, mock.URL, `{"request":{"name":"x","seed":1,"assets":[]}}`)
	if cs := r["content"].([]any); len(cs) != 1 {
		t.Fatalf("no preview before done: %v", cs)
	}
}

func TestMakeGifTool(t *testing.T) {
	mock, seen := composeMock(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"make_gif","arguments":{"request":{"name":"spin","source":{"type":"from_spec","request":{"mode":"asset","prompt":"p","size":"16x16","seed":5},"count":3}}}}}`)
	res := resp[0]["result"].(map[string]any)
	cs := res["content"].([]any)
	if len(cs) != 2 || cs[1].(map[string]any)["mimeType"] != "image/gif" {
		t.Fatalf("gif preview: %v", cs)
	}
	var body map[string]any
	json.Unmarshal([]byte(*seen), &body)
	if _, ok := body["source"].(map[string]any); !ok {
		t.Fatalf("source passthrough: %v", body)
	}
}

func TestReadExportTool(t *testing.T) {
	mock, _ := composeMock(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_export","arguments":{"set_id":"set0001"}}}`)
	res := resp[0]["result"].(map[string]any)
	cs := res["content"].([]any)
	// set view text, bolt.png image, walk-atlas.json text, manifest.json text
	if len(cs) != 4 {
		t.Fatalf("content blocks: %v", cs)
	}
	want := []string{"text", "image", "text", "text"}
	for i, w := range want {
		if got := cs[i].(map[string]any)["type"]; got != w {
			t.Fatalf("block %d: got %v want %s", i, got, w)
		}
	}
	manifest := cs[3].(map[string]any)["text"].(string)
	if !strings.Contains(manifest, `"kind":"asset-set"`) {
		t.Fatalf("manifest text: %s", manifest)
	}
}
```

Add imports if missing: `"image"`, `"image/png"` (the file already has `bytes`, `io`, `net/http`, `httptest`, `strings`, `encoding/json`, `encoding/base64`).

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./cmd/img-gen-mcp/ -v`
Expected: FAIL — `composeMock` compiles but `request_asset_set`/`read_export` are unknown tools (errResult "unknown tool"), so content assertions fail.

- [ ] **Step 3: Implement in `cmd/img-gen-mcp/main.go`**

(a) Append to the `tools` slice:

```go
	{
		name:        "request_asset_set",
		description: "Submit an asset set (element | 20-class wang kit | atlas) and block until composed. Returns the parent job JSON plus an inline preview of the primary file. Files deliver to dest, else EXPORT_DIR, else ~/Downloads/img-gen.",
		schema:      `{"type":"object","required":["request"],"properties":{"request":{"type":"object","description":"img-gen AssetSetRequest: name, seed (required), dest, assets[{kind, name, prompt, size (WxH, multiples of 16), negative, frames (atlas: 2-64), columns, transparent}]"},"wait_seconds":{"type":"integer","minimum":1,"maximum":43200,"default":900}}}`,
	},
	{
		name:        "make_gif",
		description: "Build an animated GIF: from_frames (given sources), from_spec (generates the frames), or keyframes (interpolation needs the phase-3 RIFE sidecar and is rejected). Returns the parent job JSON plus an inline preview of the GIF.",
		schema:      `{"type":"object","required":["request"],"properties":{"request":{"type":"object","description":"img-gen GifRequest: name, dest, delay_ms (10-5000, default 100), loop (>=0), formats [gif,webp], source{type: from_frames|from_spec|keyframes, frames[{job|path|b64}], request, count}"},"wait_seconds":{"type":"integer","minimum":1,"maximum":43200,"default":900}}}`,
	},
	{
		name:        "read_export",
		description: "Fetch one set (asset set or GIF) by id: the job view text, every composed file (images inline as content, JSON as text), and manifest.json.",
		schema:      `{"type":"object","required":["set_id"],"properties":{"set_id":{"type":"string"}}}`,
	},
```

(b) Add to `callTool`'s switch (after `get_image`):

```go
	case "request_asset_set":
		return c.composeCall("/api/v1/asset-sets", args)

	case "make_gif":
		return c.composeCall("/api/v1/gifs", args)

	case "read_export":
		id, _ := args["set_id"].(string)
		if id == "" {
			return errResult("set_id is required")
		}
		return c.readExport(id)
```

(c) Append the helpers below `textCall`:

```go
// jobView is the subset of the parent job JSON the compose tools act on.
type jobView struct {
	ID     string        `json:"id"`
	SetID  string        `json:"set_id"`
	Status string        `json:"status"`
	Error  string        `json:"error,omitempty"`
	Files  []fileRefView `json:"files,omitempty"`
}

type fileRefView struct {
	Name    string `json:"name"`
	Primary bool   `json:"primary,omitempty"`
}

// mimeBySuffix mirrors the content types img-gen serves for set files.
func mimeBySuffix(name string) string {
	switch {
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".gif"):
		return "image/gif"
	case strings.HasSuffix(name, ".webp"):
		return "image/webp"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// composeCall wraps the two compose POST endpoints: request passthrough,
// default wait, then a preview of the finished set's primary file.
func (c mcpcfg) composeCall(path string, args map[string]any) toolResult {
	req, ok := args["request"].(map[string]any)
	if !ok {
		return errResult("request object is required")
	}
	body := map[string]any{}
	for k, v := range req {
		body[k] = v
	}
	wait := defaultWaitSecs
	if w, ok := args["wait_seconds"].(float64); ok && w >= 1 {
		wait = int(w)
	}
	body["wait_seconds"] = wait
	res := c.textCall(http.MethodPost, path, body)
	if res.IsError {
		return res
	}
	return c.withPreview(res)
}

// withPreview appends the primary set file as an image content block when
// the set finished. Not-done or preview fetch failure keeps the text result.
func (c mcpcfg) withPreview(res toolResult) toolResult {
	var jv jobView
	if len(res.Content) == 0 || res.Content[0].Type != "text" {
		return res
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &jv); err != nil || jv.SetID == "" || jv.Status != "done" {
		return res
	}
	primary := ""
	for _, f := range jv.Files {
		if f.Primary {
			primary = f.Name
			break
		}
	}
	if primary == "" {
		return res
	}
	raw, err := c.raw(http.MethodGet, "/api/v1/sets/"+jv.SetID+"/files/"+primary, nil)
	if err != nil {
		return res
	}
	return toolResult{Content: append(res.Content, content{
		Type:     "image",
		Data:     base64.StdEncoding.EncodeToString(raw),
		MimeType: mimeBySuffix(primary),
	})}
}

// readExport gathers one set: the set view JSON as text, then every composed
// file in FileRef order (images inline, json as text), then manifest.json.
func (c mcpcfg) readExport(id string) toolResult {
	raw, err := c.raw(http.MethodGet, "/api/v1/sets/"+id, nil)
	if err != nil {
		return errResult(err.Error())
	}
	var sv struct {
		Files []fileRefView `json:"files"`
	}
	if err := json.Unmarshal(raw, &sv); err != nil {
		return errResult("bad set JSON: " + err.Error())
	}
	tr := toolResult{Content: []content{{Type: "text", Text: string(raw)}}}
	for _, f := range sv.Files {
		fraw, ferr := c.raw(http.MethodGet, "/api/v1/sets/"+id+"/files/"+f.Name, nil)
		if ferr != nil {
			continue
		}
		mt := mimeBySuffix(f.Name)
		if mt == "application/json" {
			tr.Content = append(tr.Content, content{Type: "text", Text: string(fraw)})
		} else {
			tr.Content = append(tr.Content, content{
				Type:     "image",
				Data:     base64.StdEncoding.EncodeToString(fraw),
				MimeType: mt,
			})
		}
	}
	if mraw, merr := c.raw(http.MethodGet, "/api/v1/sets/"+id+"/files/manifest.json", nil); merr == nil {
		tr.Content = append(tr.Content, content{Type: "text", Text: string(mraw)})
	}
	return tr
}
```

No new imports — `main.go` already has base64, json, http, strings.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./cmd/img-gen-mcp/ -v`
Expected: PASS (`TestToolsListShapes` adapts automatically via `len(tools)`).

- [ ] **Step 5: Commit**

```bash
git add cmd/img-gen-mcp/main.go cmd/img-gen-mcp/main_test.go
git commit -m "feat(mcp): asset set, gif, and read_export tools with inline previews

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 13: docs — openapi, README, packaged skills, opencode commands, installer

**Files:**
- Modify: `docs/openapi.yaml`
- Modify: `README.md`
- Create: `skills/img-gen-assets/SKILL.md`, `skills/img-gen-animation/SKILL.md`
- Create: `.opencode/commands/img-gen-assets.md`, `.opencode/commands/img-gen-animation.md`
- Create: `deploy/install-skills.sh` (chmod +x)

**Interfaces:**
- Consumes: the final REST surface from Tasks 10–11 and MCP tools from Task 12.
- Produces: the agent-facing documentation the spec requires (spec §7). No Go code.

- [ ] **Step 1: Append the four paths to `docs/openapi.yaml`**

After the last path entry (`/api/v1/export`, ends near line 455), append:

```yaml
  /api/v1/asset-sets:
    post:
      operationId: submitAssetSet
      summary: Submit an asset set and optionally wait for the composed result
      description: |
        Registers a compose parent job, queues 1-64 generation children
        (mode `asset`), and composes the set when the last child lands.
        wait_seconds > 0 blocks; otherwise poll the job or GET the set.
        The set id equals the job id.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              allOf:
                - $ref: "#/components/schemas/AssetSetRequest"
                - type: object
                  properties:
                    wait_seconds:
                      type: integer
                      minimum: 0
                      default: 0
      responses:
        "202":
          description: Accepted; {"job_id","set_id"} (wait_seconds <= 0).
        "200":
          description: Terminal compose job (wait_seconds > 0).
        "400":
          description: Validation failure.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/ErrorResponse"
        "504":
          description: Not terminal within wait_seconds.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/ErrorResponse"

  /api/v1/gifs:
    post:
      operationId: submitGif
      summary: Build an animated GIF and optionally wait
      description: |
        Sources: from_frames (assemble given images), from_spec (generate
        the frames here), keyframes (phase-3 RIFE interpolation — rejected
        with code not_implemented while the sidecar is absent).
      requestBody:
        required: true
        content:
          application/json:
            schema:
              allOf:
                - $ref: "#/components/schemas/GifRequest"
                - type: object
                  properties:
                    wait_seconds:
                      type: integer
                      minimum: 0
                      default: 0
      responses:
        "202":
          description: Accepted; {"job_id","set_id"} (wait_seconds <= 0).
        "200":
          description: Terminal compose job (wait_seconds > 0).
        "400":
          description: Validation failure.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/ErrorResponse"
        "504":
          description: Not terminal within wait_seconds.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/ErrorResponse"

  /api/v1/sets/{set_id}:
    get:
      operationId: getSet
      summary: Set metadata — parent job and file refs
      parameters:
        - in: path
          name: set_id
          required: true
          schema: { type: string }
      responses:
        "200":
          description: The compose parent and its files
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/SetView"
        "404":
          description: Unknown set id or not a compose job.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/ErrorResponse"

  /api/v1/sets/{set_id}/files/{name}:
    get:
      operationId: getSetFile
      summary: One set file, streamed by content type
      parameters:
        - in: path
          name: set_id
          required: true
          schema: { type: string }
        - in: path
          name: name
          required: true
          schema: { type: string }
        - in: query
          name: download
          schema:
            type: string
            enum: ["1"]
          description: >
            "1" adds a Content-Disposition attachment header.
      responses:
        "200":
          description: File bytes (image/png, image/gif, image/webp, application/json, or application/octet-stream)
        "404":
          description: Unknown set id or invalid file name.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/ErrorResponse"
```

The file has no `components.responses` section — every error response is an inline block with a `description` plus `$ref: "#/components/schemas/ErrorResponse"`, and status keys are double-quoted (see `/api/v1/generate` and `/api/v1/jobs/{id}` — this was verified when `/api/v1/asset-sets` and `/api/v1/gifs` were written). The blocks above already use that style, and `SetView` is a new component schema added in Step 2; do not invent `components.responses` entries or reference them.

Also update the existing `components.schemas.Error` — add `not_implemented` to the `code` enum (after `bad_idempotency_key`) and a `description:` on the enum:

```yaml
          description: >
            Async composite failures do not use an envelope code: the job turns
            failed with its error field prefixed "compose_error:".
```

- [ ] **Step 2: Append the schemas to `docs/openapi.yaml`**

In `components.schemas` (or wherever the job/error schemas live — same style note as above):

```yaml
    AssetSetRequest:
      type: object
      required: [name, seed, assets]
      properties:
        name:
          type: string
          description: File stem for the set (letters, digits, -, _, .)
        dest:
          type: string
          description: Absolute directory to deliver composed files into; empty uses EXPORT_DIR, then ~/Downloads/img-gen
        seed:
          type: integer
          format: int64
        assets:
          type: array
          maxItems: 8
          items: { $ref: "#/components/schemas/AssetSpecIn" }
    AssetSpecIn:
      type: object
      required: [kind, name, prompt, size]
      properties:
        kind:
          type: string
          enum: [element, kit, atlas]
        name: { type: string }
        prompt: { type: string }
        negative: { type: string }
        size:
          type: string
          description: WxH, both multiples of 16
        frames:
          type: integer
          minimum: 2
          maximum: 64
          description: atlas only
        columns:
          type: integer
          description: atlas atlas-sheet columns; 0 = single row
        transparent:
          type: boolean
          description: element only — white+black double pass, alpha unmixed
    GifRequest:
      type: object
      required: [name, source]
      properties:
        name: { type: string }
        dest: { type: string }
        delay_ms:
          type: integer
          minimum: 10
          maximum: 5000
          default: 100
        loop:
          type: integer
          minimum: 0
          description: 0 = infinite
        formats:
          type: array
          items:
            type: string
            enum: [gif, webp]
          default: [gif]
        source: { $ref: "#/components/schemas/GifSource" }
    GifSource:
      type: object
      required: [type]
      properties:
        type:
          type: string
          enum: [from_frames, from_spec, keyframes]
        frames:
          type: array
          minItems: 2
          maxItems: 64
          items: { $ref: "#/components/schemas/FrameRef" }
          description: from_frames
        request:
          allOf:
            - $ref: "#/components/schemas/SubmitRequest"
          description: from_spec base request (seed required)
        count:
          type: integer
          minimum: 2
          maximum: 64
          description: from_spec frames
    FrameRef:
      type: object
      description: Exactly one of job, path, b64
      properties:
        job:
          type: string
          description: An img-gen job id whose saved image becomes this frame
        path:
          type: string
          description: Data-dir-relative path (e.g. images/<id>.png); no leading / or ..
        b64:
          type: string
          description: Inline base64 PNG
    FileRef:
      type: object
      properties:
        name: { type: string }
        primary: { type: boolean }
    SetView:
      type: object
      properties:
        set_id: { type: string }
        job: { $ref: "#/components/schemas/Job" }
        files:
          type: array
          items: { $ref: "#/components/schemas/FileRef" }
```

`#/components/schemas/SubmitRequest` already exists in the file (the `/api/v1/jobs` path references it), so the `allOf: ["#/components/schemas/SubmitRequest"]` in `GifSource.request` above is correct as-is; do not re-define it.

Also extend the `description:` block at the top of the file with one line after the modes list:

```yaml
    Compose jobs (mode `compose`) assemble delivered sets from `asset`-mode
    children via /api/v1/asset-sets and /api/v1/gifs; fetch results with
    /api/v1/sets/{set_id}.
```

- [ ] **Step 3: README — agent assets & GIFs section**

In `README.md`, at the end of the `## Agent API (v1) & MCP` section, append:

```markdown
### Agent assets & animated GIFs

img-gen doubles as an asset factory for coding agents. Two workflows:

- **Asset sets** — `POST /api/v1/asset-sets` with 1–8 assets of kind
  `element` (one image, optional `transparent` double-pass), `kit`
  (a fixed 20-class wang tileset: h/v lines, outer/inner corners, T's,
  cross, caps, single), or `atlas` (2–64 frame sheet + TexturePacker-style
  JSON). Every asset needs a prompt and a 16-multiple `WxH` size. A
  `manifest.json` lands in the set with per-asset files, seeds and errors.
- **GIFs** — `POST /api/v1/gifs` with `source.type` `from_frames`
  (assemble existing job ids, data-dir paths, or inline base64),
  `from_spec` (img-gen generates the 2–64 frames at `seed+i`),
  or `keyframes` (needs the phase-3 RIFE sidecar; rejected with
  `not_implemented` until then). `delay_ms` 10–5000 (default 100),
  `loop` >= 0, optional `formats: ["gif","webp"]` (`webp` is a
  best-effort extra — gif2webp on PATH or the skip is recorded).

Both endpoints accept `wait_seconds`; with `0` (default) they return
`202 {"job_id","set_id"}` immediately. Fetch results via
`GET /api/v1/sets/{id}` (job + file list) and
`GET /api/v1/sets/{id}/files/{name}` (bytes, content-typed; `?download=1`
for a download header). `files[].primary` marks the asset the job points
at. Sets live under `DATA_DIR/sets/{set_id}/`; `dest` delivers copies
into an agent's project directory (absolute path required — delivery
refuses to overwrite, so retry after clearing the destination).

MCP tools: `request_asset_set`, `make_gif`, `read_export`.

Install the packaged agent skills:

```bash
deploy/install-skills.sh
```
```

- [ ] **Step 4: Create the packaged skills**

`skills/img-gen-assets/SKILL.md`:

```markdown
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

Sizes must be `WxH` with both multiples of 16. `seed` is required — a set
must be reproducible while a design iterates.

## Workflow

1. Design the set: which standalone elements, whether a wang kit covers the
   tile work, whether animation frames belong in an atlas.
2. Submit (`request_asset_set` MCP tool, or POST /api/v1/asset-sets on the
   REST surface). One tool call returns the composed job plus an inline
   preview of the primary file.
3. Deliver into the project: pass `"dest": "/abs/path/to/project/assets"` —
   delivery copies the files there and records the destination in
   `manifest.json`. Delivery never overwrites: clear the destination file
   and retry if you need to replace one.
4. Inspect or re-fetch anything later with `read_export` (MCP) or
   `GET /api/v1/sets/{id}`.

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
```

`skills/img-gen-animation/SKILL.md`:

```markdown
---
name: img-gen-animation
description: Build animated GIFs with the img-gen service — assemble frames from existing generations, generate frame sequences, or reference future keyframe interpolation. Use when a project needs animated images, loading indicators, or sprite playback.
---

# img-gen animation

`make_gif` (MCP) or `POST /api/v1/gifs` turns frame sequences into GIFs.

## Frame sources

- `from_frames` — assemble given images. Each frame is
  `{job: <img-gen job id>}` or `{path: "<data-dir-relative path>"}` or
  `{b64: "<base64 png>"}`. 2-64 frames.
- `from_spec` — img-gen generates the frames: `{"type":"from_spec",
  "request":{"mode":"asset","prompt":"<subject>","size":"64x64","seed":7},
  "count":8}`. Frames get `seed+i` and a frame-index suffix; keep prompts
  style-stable (state the shared style once and describe only per-frame
  motion).
- `keyframes` — interpolation between keyframes needs the RIFE sidecar;
  rejected with `not_implemented` until phase 3.

## Timing and formats

`delay_ms` 10-5000 (default 100; GIF delays are centiseconds, so 100ms =
10). `loop: 0` loops forever. `formats: ["gif","webp"]` also writes an
animated WebP when `gif2webp` is on the server's PATH — otherwise the
skip is recorded in `manifest.json` (`gif.webp`), not an error.

## Workflow

1. Submit and wait (the MCP tool blocks): text result is the parent job
   JSON; the GIF itself arrives as an inline preview.
2. `read_export` (MCP) returns the job view, the GIF, and `manifest.json`.
3. Deliver with `"dest"` or copy from the set dir
   (`DATA_DIR/sets/{set_id}/<name>.gif`).
```

- [ ] **Step 5: Create the opencode commands**

`.opencode/commands/img-gen-assets.md`:

```markdown
---
description: Generate image assets for this project with img-gen (elements, wang kits, atlases)
---
Follow the img-gen-assets skill ($HOME/.claude/skills/img-gen-assets/SKILL.md,
or the packaged skills/ directory of the img-gen repo). Ask me for the asset
list if I have not described one: kinds, prompts, sizes, transparency, and the
destination directory inside this project. Then submit via img-gen
(request_asset_set MCP tool or POST /api/v1/asset-sets), deliver the files,
and report the manifest.
```

`.opencode/commands/img-gen-animation.md`:

```markdown
---
description: Build an animated GIF with img-gen (from frames, or generate a frame sequence)
---
Follow the img-gen-animation skill ($HOME/.claude/skills/img-gen-animation/SKILL.md,
or the packaged skills/ directory of the img-gen repo). Ask me for the frame
source (generation spec or existing frames), timing, and destination. Then
submit via img-gen (make_gif MCP tool or POST /api/v1/gifs), fetch the result
with read_export, and report the manifest.
```

- [ ] **Step 6: Create `deploy/install-skills.sh`** (`chmod +x`)

```bash
#!/bin/bash
# Install img-gen's packaged agent skills and opencode commands into the
# current user's toolboxes. Idempotent: re-running refreshes the copies.
set -euo pipefail

repo_dir="$(cd "$(dirname "$0")/.." && pwd)"

claude_skills="${CLAUDE_SKILLS_DIR:-$HOME/.claude/skills}"
opencode_cmds="${OPENCODE_CMDS_DIR:-$HOME/.opencode/commands}"

mkdir -p "$claude_skills" "$opencode_cmds"

for skill_dir in "$repo_dir"/skills/*/; do
  name="$(basename "$skill_dir")"
  rm -rf "$claude_skills/$name"
  cp -R "$skill_dir" "$claude_skills/$name"
  echo "installed skill $name -> $claude_skills/$name"
done

for cmd in "$repo_dir"/.opencode/commands/*.md; do
  [ -e "$cmd" ] || continue
  cp "$cmd" "$opencode_cmds/"
  echo "installed command $(basename "$cmd") -> $opencode_cmds/"
done
```

- [ ] **Step 7: Verify**

Run:
```bash
bash -n deploy/install-skills.sh
grep -c "asset-sets" docs/openapi.yaml
grep -c "request_asset_set" README.md docs/openapi.yaml skills/img-gen-assets/SKILL.md
```
Expected: `bash -n` silent; openapi count ≥ 4 (paths + description); each grep ≥ 1. Also `python3 -c "import yaml,sys; d=yaml.safe_load(open('docs/openapi.yaml')); assert '/api/v1/asset-sets' in d['paths']"` to prove the YAML still parses on any machine with PyYAML (skip if python3/yaml is absent — do not install anything).

(No CI exists in this repo — no `.github/workflows` — so the spec's "CI grep that openapi.yaml mentions every shipped endpoint" runs here as a plain step instead; Task 14 Step 1 re-runs the full suite over everything.)

Skill-landing test — run the installer against throwaway dirs and check the frontmatter:
```bash
s=$(mktemp -d); c=$(mktemp -d)
CLAUDE_SKILLS_DIR="$s" OPENCODE_CMDS_DIR="$c" bash deploy/install-skills.sh
ls "$s/img-gen-assets/SKILL.md" "$s/img-gen-animation/SKILL.md" "$c/img-gen-assets.md" "$c/img-gen-animation.md"
head -2 "$s/img-gen-assets/SKILL.md"
test "$(head -1 "$s/img-gen-assets/SKILL.md")" = "---" && grep -q "^name:" "$s/img-gen-assets/SKILL.md" && echo frontmatter-ok
rm -rf "$s" "$c"
```
Expected: the four `ls` paths exist; the installer echoes two `installed skill` + two `installed command` lines; `head -2` shows `---` frontmatter; `frontmatter-ok` prints. Re-running is idempotent (the script refreshes copies).

- [ ] **Step 8: Commit**

```bash
git add docs/openapi.yaml README.md skills .opencode deploy/install-skills.sh
git commit -m "docs: agent asset & gif surface — openapi, README, skills, installer

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 14: full verification & manual smoke

**Files:**
- None (verification only). Fixes, if anything surfaces, belong to the task where the bug was introduced — do not bundle silent corrections into the smoke-test commit.

**Interfaces:**
- Consumes: everything. Produces: confidence, and the working tree this plan hands back.

- [ ] **Step 1: Full test suite & vet**

Run: `go vet ./... && go test ./...`
Expected: PASS everywhere. If a package fails, fix at the failing task's code and add the missing test there before proceeding.

- [ ] **Step 2: `go build ./...`** — confirms all three binaries (img-gen, img-gen-mcp, and any others in cmd/) compile.

- [ ] **Step 3: Manual smoke against a real server**

Start the server as usual (e.g. `DATA_DIR=$(mktemp -d) IMG_ROUTING... LISTEN=:9100 go run ./cmd/img-gen`, using whatever env you normally use) and then, with real engine latency in mind (an 8-asset kit can take a few minutes on mflux):

```bash
curl -s http://127.0.0.1:9100/api/v1/asset-sets -d '{"name":"smoke","seed":42,"assets":[{"kind":"element","name":"dot","prompt":"flat red dot, minimal","size":"64x64"}],"wait_seconds":0}'
```
Expected: `{"job_id":"...","set_id":"..."}` — poll `GET /api/v1/jobs/{id}` to `done`, then:
```bash
curl -s http://127.0.0.1:9100/api/v1/sets/{set_id}
curl -s -o /tmp/dot.png http://127.0.0.1:9100/api/v1/sets/{set_id}/files/dot.png
curl -s http://127.0.0.1:9100/api/v1/gifs -d '{"name":"twirl","source":{"type":"from_spec","request":{"mode":"asset","prompt":"spinning star","size":"64x64","seed":9},"count":3},"delay_ms":200,"wait_seconds":0}'
```
Expected: the set view shows `files: [{name:"dot.png","primary":true}]`; the GIF compose job completes with `twirl.gif` as primary. Confirm `DATA_DIR/sets/{set_id}/manifest.json` exists and parses. Then delete both parents via `DELETE /api/v1/jobs/{id}` and confirm the set dir is gone.

Skip what cannot run in this environment (no sidecar): the unit/integration tests already cover the composition paths with decodable stubs — note honestly in the PR which smoke steps were actually executed.

- [ ] **Step 4: Commit (only if fixes were needed)**

```bash
git add -A
git commit -m "fix: adjustments found in phase 1 smoke verification

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```
If everything passed, there is nothing to commit — this task produces no commit.

---

## Completion

The plan is done when: every task's checkbox is ticked, `go vet ./...` and `go test ./...` are green, the three binaries build, and the smoke test (or its documented skip) is recorded. Phase 2 (tileable seam control via the mflux sidecar) and Phase 3 (RIFE keyframes, rembg, LayerDiffuse) get their own plans later — do not start them from here.