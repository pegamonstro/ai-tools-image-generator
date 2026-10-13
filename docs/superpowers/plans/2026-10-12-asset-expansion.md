# Asset Expansion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add five engine-feasible asset features to img-gen — palette lock (set-wide CIEDE2000 coherence remap), nine-patch `frame` kind, `bar` kind with clipped fill states, `npc` kind (pinned-seed variant sheets + honest walk4), and `composite` GIF source (statics over an anim layer) — each with docs landing with its code.

**Architecture:** Pure post-process additions to the existing text+seed engine. `internal/compose` gains five small focused files (`remap.go`, `frame.go`, `bar.go`, `npc.go`, `composite.go`); `internal/queue/compose.go` gains one request field, four asset kinds (validation switch + children builder + executor case each), and a composite GIF source path. The palette lock is a single uniform hook in the asset-set executor: compose outputs are remapped **after** per-kind composition and **before** `SaveSetFile`, so every deliverable (element/kit/atlas/frame/bar/npc) inherits the set palette. v1 handlers and MCP schemas need only documentation-string edits — request fields flow through already.

**Tech Stack:** Go 1.x (stdlib `image`, `image/png`, `image/draw`, `encoding/json`), existing median-cut palette (`sharedPalette`, internal/compose/gif.go:89), existing storage set directories and `DeliverSet` delivery.

**Spec:** `docs/superpowers/specs/2026-10-12-asset-expansion-design.md` — the binding authority this plan argues from. Read it first; conflicts inside this plan resolve against the spec.

## File Structure

Files created or modified, grouped by responsibility:

- `internal/compose/remap.go` (create) — CIELAB conversion, CIEDE2000 distance, nearest-palette remap, `SetPalette` wrapper over the shared median-cut, palette JSON marshal. Responsibility: the color math for F1.
- `internal/compose/frame.go` (create) — nine-slice interior punch, `ComposeFrame`, the shared `marginsJSON` sidecar struct + `encodePNG` helper. Responsibility: F2 deliverable geometry.
- `internal/compose/bar.go` (create) — `ComposeBar` (punched track, full fill, per-state clips, bar JSON). Responsibility: F3 deliverables.
- `internal/compose/npc.go` (create) — `FlipHorizontal`, `ComposeNPCWalk4`. Responsibility: F4 cell construction (3-drawing mirror).
- `internal/compose/composite.go` (create) — `BlitImage` alpha-correct static compositing with fail-fast overhang. Responsibility: F5 layer math.
- `internal/queue/compose.go` (modify) — `AssetSetRequest`/`AssetSpecIn`/`assetSpec`/`manifestAsset` fields, `parseAssetSize`, `npcChildren`, kind-switch cases (validation + executor), `PaletteLock` in `composeSpec`, executor restructure (collect → remap → save), GIF composite types (`CompositeSource`, `StaticLayer`, `compositeLayerSpec`, `gifPayload` composite field), `FrameRef` set-file form, `resolveFrameRef` extension, `SubmitGIF` composite case, `composeGIF` static blit.
- `internal/queue/queue.go` — untouched (size validation `validAssetSize` stays; `parseAssetSize` lives in compose.go for locality).
- `internal/compose/*_test.go`, `internal/queue/compose_test.go` (modify/create) — one test file per new compose module; queue tests append to compose_test.go.
- `README.md`, `skills/img-gen-assets/SKILL.md`, `skills/img-gen-animation/SKILL.md`, `cmd/img-gen-mcp/main.go` (modify) — docs and schema strings, per feature, inside that feature's task.
- `docs/openapi.yaml` — untouched; v1 request types flow through `queue.AssetSetRequest` without schema-code edits (schemas there already describe free-form request bodies).

## Global Constraints

Every task implicitly includes these. Exact values verbatim:

- **Engine honesty:** the engine takes ONLY text prompt + integer seed. No conditioning, no img2img, no runtime LoRA. `walk4` ships standing direction views — NOT animation; docs must say so (the honesty contract below).
- **Attribution:** every git commit message ends with exactly `Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>`.
- **Green per commit:** every task ends with `go vet ./... && go test ./...` passing before committing. (Unit tests only — no real renders; the fake generator in tests returns synthetic PNGs in milliseconds.)
- **Naming layers:** asset/set names go through `validComposeAssetName` (queue) → lowercase `[a-z0-9-_.]`; set-file names are re-checked by `validSetFile` (storage), regex `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$` with no `..`.
- **Sizes:** asset sizes are `WxH` with both numbers multiples of 16 (`validAssetSize`, internal/queue/queue.go:555).
- **Limits:** ≤ 8 assets per set (`maxAssetsPerSet`), ≤ 64 children per set (`maxComposeChildren`), atlas/variants frames 2..64, fill states ≤ 16 values 1..100 ascending, statics ≤ 8.
- **GIF alpha:** 1-bit threshold α<128, pre-quantization compositing stays (compose happens before quantization in `EncodeGIF`).
- **Palette cap:** 255 entries max — one slot reserved for the transparent index, mirroring the GIF convention.
- **Docs with code:** each feature's README/skill/MCP-schema edits land in that feature's task.
- **Out of scope:** `keyframes` stays `not_implemented`; mflux tileable, RIFE sidecar, rembg, LayerDiffuse (Phase 2/3) untouched; separate nine-slice corner/edge files, separate bar caps, per-state re-renders all rejected by the spec (§9).
- **Delivery:** `DeliverSet(setID, setName, destDir)` renames `manifest.json` → `<setName>.manifest.json` (shipped 1f6d44c); nothing in this plan changes delivery.

---

### Task 1: compose remap.go — CIEDE2000 palette remap (F1 core)

**Files:**
- Create: `internal/compose/remap.go`
- Test: `internal/compose/remap_test.go`

**Interfaces:**
- Consumes: `sharedPalette(frames []image.Image, n int) color.Palette` (internal/compose/gif.go:89, same package).
- Produces (used by Task 2): `SetPalette(imgs []image.Image, n int) color.Palette`, `RemapImage(img image.Image, pal color.Palette) *image.NRGBA`, `RemapPNG(data []byte, pal color.Palette) ([]byte, error)`, `MarshalPalette(pal color.Palette) ([]byte, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/compose/remap_test.go`:

```go
package compose

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

// TestCIEDE2000Sharma pins the color-difference formula against published
// reference pairs (Sharma, Wu, Dalal 2005 test data). If the 7-term formula
// drifts, these are the pairs that catch it.
func TestCIEDE2000Sharma(t *testing.T) {
	cases := []struct {
		l1, a1, b1, l2, a2, b2, want float64
	}{
		{50.0000, 2.6772, -79.7751, 50.0000, 0.0000, -82.7485, 2.0425},
		{50.0000, -1.3802, -84.2814, 50.0000, 0.0000, -82.7485, 2.8615},
		{50.0000, 2.5000, 0.0000, 73.0000, 25.0000, -18.0000, 27.1492},
		{50.0000, 2.5000, 0.0000, 50.0000, 2.5000, 0.0000, 0.0000},
	}
	for i, c := range cases {
		got := cie2000(c.l1, c.a1, c.b1, c.l2, c.a2, c.b2)
		if math.Abs(got-c.want) > 0.001 {
			t.Fatalf("case %d: cie2000 = %v, want %v", i, got, c.want)
		}
	}
}

func TestRemapNearestColor(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 240, G: 40, B: 20, A: 255})
	pal := color.Palette{
		color.RGBA{R: 255, G: 0, B: 0, A: 255},
		color.RGBA{R: 0, G: 0, B: 255, A: 255},
	}
	got := RemapImage(src, pal).NRGBAAt(0, 0)
	if got.R != 255 || got.G != 0 || got.B != 0 {
		t.Fatalf("nearest = %+v, want red", got)
	}
}

func TestRemapImagePreservesTransparent(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 0})
	pal := color.Palette{color.RGBA{R: 200, G: 0, B: 0, A: 255}}
	out := RemapImage(src, pal)
	if got := out.NRGBAAt(0, 0); got.A != 255 || got.R != 200 || got.G != 0 || got.B != 0 {
		t.Fatalf("opaque pixel: %+v", got)
	}
	if got := out.NRGBAAt(1, 0); got.A != 0 {
		t.Fatalf("transparent pixel must stay alpha 0: %+v", got)
	}
}

func TestRemapPNGDeterministic(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(10 * x), G: uint8(20 * y), B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	pal := SetPalette([]image.Image{src}, 255)
	if len(pal) == 0 {
		t.Fatal("SetPalette returned an empty palette over opaque pixels")
	}
	a, err := RemapPNG(data, pal)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RemapPNG(data, pal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("remap is not deterministic")
	}
	decoded, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 4 || decoded.Bounds().Dy() != 4 {
		t.Fatalf("remapped bounds = %+v", decoded.Bounds())
	}
}

func TestMarshalPalette(t *testing.T) {
	pal := color.Palette{color.RGBA{R: 15, G: 118, B: 110, A: 255}}
	raw, err := MarshalPalette(pal)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"colors":["#0f766e"]}`
	if string(raw) != want {
		t.Fatalf("palette json = %s, want %s", raw, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run 'CIEDE2000|Remap|MarshalPalette' -v`
Expected: FAIL — `undefined: cie2000`, `undefined: RemapImage`, `undefined: SetPalette`, `undefined: MarshalPalette`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/compose/remap.go`:

```go
package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// SetPalette builds a set palette over imgs with the same deterministic
// median-cut the GIF encoder uses. 255 entries leaves the 1-bit transparent
// index free, mirroring the GIF convention.
func SetPalette(imgs []image.Image, n int) color.Palette {
	return sharedPalette(imgs, n)
}

// PaletteHexes renders the palette as #rrggbb strings, in palette order
// (median-cut emission order is deterministic, so the file bytes are too).
func PaletteHexes(pal color.Palette) []string {
	hexes := make([]string, 0, len(pal))
	for _, c := range pal {
		n := color.NRGBAModel.Convert(c).(color.NRGBA)
		hexes = append(hexes, fmt.Sprintf("#%02x%02x%02x", n.R, n.G, n.B))
	}
	return hexes
}

// MarshalPalette writes {"colors": ["#rrggbb", ...]} — the set palette file.
func MarshalPalette(pal color.Palette) ([]byte, error) {
	type paletteFile struct {
		Colors []string `json:"colors"`
	}
	return json.Marshal(paletteFile{Colors: PaletteHexes(pal)})
}

// RemapImage returns a copy of img with every opaque pixel snapped to its
// CIEDE2000-nearest palette color. Pixels with alpha < 128 are left fully
// transparent — the 1-bit alpha convention shared with the GIF encoder.
func RemapImage(img image.Image, pal color.Palette) *image.NRGBA {
	labs := paletteLAB(pal)
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			n := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if n.A < 128 {
				out.SetNRGBA(x-b.Min.X, y-b.Min.Y, color.NRGBA{})
				continue
			}
			l, a, bb := rgbToLAB(float64(n.R), float64(n.G), float64(n.B))
			best := 0
			bestD := math.MaxFloat64
			for i, lp := range labs {
				d := cie2000(l, a, bb, lp[0], lp[1], lp[2])
				if d < bestD {
					bestD = d
					best = i
				}
			}
			out.SetNRGBA(x-b.Min.X, y-b.Min.Y, nrgbaAt(pal[best]))
		}
	}
	return out
}

// RemapPNG decodes a PNG, remaps it, and re-encodes. Data must be a
// decodable PNG; anything else is an error.
func RemapPNG(data []byte, pal color.Palette) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("compose: remap: not a PNG: %v", err)
	}
	out := RemapImage(img, pal)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func nrgbaAt(c color.Color) color.NRGBA {
	return color.NRGBAModel.Convert(c).(color.NRGBA)
}

func paletteLAB(pal color.Palette) [][3]float64 {
	labs := make([][3]float64, len(pal))
	for i, c := range pal {
		r, g, b, _ := c.RGBA()
		labs[i] = rgbToLAB(float64(r>>8), float64(g>>8), float64(b>>8))
	}
	return labs
}

// rgbToLAB converts an sRGB color (8-bit components) to CIELAB (D65).
func rgbToLAB(r, g, b float64) (l, a, bb float64) {
	lin := func(v float64) float64 {
		v /= 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	rl, gl, bl := lin(r), lin(g), lin(b)
	x := 0.4124564*rl + 0.3575761*gl + 0.1804375*bl
	y := 0.2126729*rl + 0.7151522*gl + 0.0721750*bl
	z := 0.0193339*rl + 0.1191920*gl + 0.9503041*bl
	f := func(t float64) float64 {
		if t > 216.0/24389.0 {
			return math.Cbrt(t)
		}
		return (24389.0/27.0*t + 16) / 116
	}
	fx, fy, fz := f(x/0.95047), f(y/1.0), f(z/1.08883)
	return 116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)
}

// cie2000 is the full seven-term CIEDE2000 color difference between two
// CIELAB colors (hue terms work in degrees), per the Sharma/Wu/Dalal
// reference semantics: hue difference wraps at ±180, and when the adjusted
// chroma product is zero the hue-bar is the plain sum (not the mean) and the
// hue-difference term vanishes.
func cie2000(l1, a1, b1, l2, a2, b2 float64) float64 {
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }
	c1 := math.Hypot(a1, b1)
	c2 := math.Hypot(a2, b2)
	cbar := (c1 + c2) / 2
	g := 0.5 * (1 - math.Sqrt(math.Pow(cbar, 7)/(math.Pow(cbar, 7)+math.Pow(25, 7))))
	a1p := (1 + g) * a1
	a2p := (1 + g) * a2
	c1p := math.Hypot(a1p, b1)
	c2p := math.Hypot(a2p, b2)
	h1p := huep(a1p, b1)
	h2p := huep(a2p, b2)
	dlp := l2 - l1
	dcp := c2p - c1p
	dhp := 0.0
	if c1p*c2p != 0 {
		dhp = h2p - h1p
		if dhp > 180 {
			dhp -= 360
		} else if dhp < -180 {
			dhp += 360
		}
	}
	dhp2 := 2 * math.Sqrt(c1p*c2p) * math.Sin(rad(dhp)/2)
	lbp := (l1 + l2) / 2
	cbp := (c1p + c2p) / 2
	hbp := h1p + h2p
	if c1p*c2p != 0 {
		if math.Abs(h1p-h2p) > 180 {
			if h1p+h2p < 360 {
				hbp += 360
			} else {
				hbp -= 360
			}
		}
		hbp /= 2
	}
	t := 1 - 0.17*math.Cos(rad(hbp-30)) + 0.24*math.Cos(rad(2*hbp)) +
		0.32*math.Cos(rad(3*hbp+6)) - 0.20*math.Cos(rad(4*hbp-63))
	dtheta := 30 * math.Exp(-math.Pow((hbp-275)/25, 2))
	rc := 2 * math.Sqrt(math.Pow(cbp, 7)/(math.Pow(cbp, 7)+math.Pow(25, 7)))
	sl := 1 + 0.015*(lbp-50)*(lbp-50)/math.Sqrt(20+(lbp-50)*(lbp-50))
	sc := 1 + 0.045*cbp
	sh := 1 + 0.015*cbp*t
	rt := -math.Sin(rad(2*dtheta)) * rc
	return math.Sqrt(
		(dlp/sl)*(dlp/sl) +
			(dcp/sc)*(dcp/sc) +
			(dhp2/sh)*(dhp2/sh) +
			rt*(dcp/sc)*(dhp2/sh))
}

func huep(a, b float64) float64 {
	h := math.Atan2(b, a) * 180 / math.Pi
	if h < 0 {
		h += 360
	}
	return h
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compose/ -run 'CIEDE2000|Remap|MarshalPalette' -v`
Expected: PASS (all five tests). If a Sharma case misses by >0.001, the formula transcription is wrong — re-derive against the reference implementation before touching anything else.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/compose/remap.go internal/compose/remap_test.go
git commit -m "feat(compose): CIEDE2000 palette remap (palette lock core)

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 2: queue palette_lock — set coherence remap + palette file + docs (F1)

**Files:**
- Modify: `internal/queue/compose.go` (`AssetSetRequest` :323, `composeSpec` :26, executor `composeAssetSet` :492, delivery unchanged)
- Modify: `internal/queue/compose_test.go` (new helpers + test)
- Modify: `README.md` (Agent assets section), `skills/img-gen-assets/SKILL.md`, `cmd/img-gen-mcp/main.go` (:104-105)

**Interfaces:**
- Consumes: Task 1's `compose.SetPalette`, `compose.RemapPNG`, `compose.MarshalPalette` (exact signatures above).
- Produces (used by Tasks 4/6/8): the restructured executor — per-asset compose loop collects `outsByAsset [][]compose.OutputFile` (aligned with `spec.Assets` by index), then a palette pass, then a save loop. Later tasks only add `case` lines to the kind switches; they do not touch this structure. Also produces test helper `sizedGen`.
- Request field: `AssetSetRequest.PaletteLock bool` with JSON tag `palette_lock,omitempty`; persisted as `composeSpec.PaletteLock` same tag.

**Design ruling (from the spec's integration point):** remap runs AFTER per-kind composition and BEFORE `SaveSetFile` — one hook over all PNG outputs. During restructure, `SaveSetFile` failure moves from the per-asset loop into the save phase: a save failure there fails the parent immediately (`composeGIF` precedent) instead of per-asset break marking. Storage save failures were always catastrophic disk-level events; behavior change is covered by the suite.

- [ ] **Step 1: Write the failing test**

First add two pieces to `internal/queue/compose_test.go`:

(a) add `image/color` and `strconv` to the import block (they are not currently imported).

(b) add these helpers after `composeTestOpts` (Task 1 of this plan's sizedGen is reused by Tasks 4 and 6's tests):

```go
// sizedGen returns an ImageOps.Generate that renders a fully opaque red/green
// checker PNG at the requested size (parsing WxH), recording in cap. Real
// sizes matter: post-processing like the frame punch validates render
// dimensions, so the 8x8 default fake is too small.
func sizedGen(t *testing.T, cap *genCapture) func(context.Context, string, string, storage.ModelSpec, storage.SamplingParams) ([]byte, *int64, error) {
	t.Helper()
	return func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		ws, hs, _ := strings.Cut(size, "x")
		w, _ := strconv.Atoi(ws)
		h, _ := strconv.Atoi(hs)
		if w < 1 || h < 1 {
			w, h = 8, 8
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if (x/4+y/4)%2 == 0 {
					img.Set(x, y, color.RGBA{R: 255, A: 255})
				} else {
					img.Set(x, y, color.RGBA{G: 255, A: 255})
				}
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if cap != nil {
			cap.add(prompt, size, sp.Seed, buf.Bytes())
		}
		return buf.Bytes(), sp.Seed, nil
	}
}
```

(c) add the test:

```go
func TestSubmitAssetSetPaletteLock(t *testing.T) {
	o := composeTestOpts(t, t.TempDir(), nil)
	o.Ops.Generate = sizedGen(t, nil)
	m := New(o)
	seed := int64(41)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "cohere", Seed: &seed, PaletteLock: true,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "a", Prompt: "first", Size: "16x16"},
			{Kind: "element", Name: "b", Prompt: "second", Size: "16x16"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	raw, err := m.opts.Store.ReadSetFile(id, "cohere.palette.json")
	if err != nil {
		t.Fatalf("palette file: %v", err)
	}
	var pal struct {
		Colors []string `json:"colors"`
	}
	if err := json.Unmarshal(raw, &pal); err != nil {
		t.Fatal(err)
	}
	if len(pal.Colors) == 0 {
		t.Fatal("palette file has no colors")
	}
	for _, hx := range pal.Colors {
		if !strings.HasPrefix(hx, "#") || len(hx) != 7 {
			t.Fatalf("bad palette hex %q", hx)
		}
	}

	// Every opaque pixel of every PNG output lands inside the palette.
	allowed := map[string]bool{}
	for _, hx := range pal.Colors {
		allowed[strings.ToLower(hx)] = true
	}
	for _, name := range []string{"a.png", "b.png"} {
		data, err := m.opts.Store.ReadSetFile(id, name)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				n := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				if n.A < 128 {
					continue
				}
				hx := fmt.Sprintf("#%02x%02x%02x", n.R, n.G, n.B)
				if !allowed[hx] {
					t.Fatalf("%s pixel (%d,%d) = %s is outside the set palette", name, x, y, hx)
				}
			}
		}
	}
}
```

Note: `sizedGen` is deterministic, so the checker's red and green both survive into the ≤255 palette and the membership check holds.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run TestSubmitAssetSetPaletteLock -v`
Expected: FAIL — `unknown field "PaletteLock"` (compile error: `AssetSetRequest` has no such field) or `ReadSetFile: no such file` for the palette file.

- [ ] **Step 3: Implement the field + executor restructure**

(a) In `AssetSetRequest` (compose.go:323), add the field after `Seed`:

```go
type AssetSetRequest struct {
	Name        string        `json:"name"`           // file stem for the set (sanitized)
	Dest        string        `json:"dest,omitempty"` // absolute dir to deliver into; empty = EXPORT_DIR fallback chain
	Seed        *int64        `json:"seed"`           // required
	PaletteLock bool          `json:"palette_lock,omitempty"`
	Assets      []AssetSpecIn `json:"assets"`         // 1..8
}
```

(b) In `composeSpec` (compose.go:26), add the persisted field after `Dest`:

```go
type composeSpec struct {
	Kind        string      `json:"kind"` // "asset-set" | "gif"
	Name        string      `json:"name"`
	Dest        string      `json:"dest,omitempty"`
	PaletteLock bool        `json:"palette_lock,omitempty"`
	Assets      []assetSpec `json:"assets,omitempty"`
	GIF         *gifPayload `json:"gif,omitempty"`
}
```

(c) In `SubmitAssetSet` (:358), copy the field into the spec: change

```go
	spec := composeSpec{Kind: "asset-set", Name: setName, Dest: req.Dest}
```

to

```go
	spec := composeSpec{Kind: "asset-set", Name: setName, Dest: req.Dest, PaletteLock: req.PaletteLock}
```

(d) Restructure the executor `composeAssetSet` (:492). Three moves:

1. Declare the collection slice next to `files`, and replace the inline save block with collection. In the `broken == ""` block, the current code (compose.go:543-552):

```go
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
```

becomes:

```go
			if err == nil {
				outsByAsset[ai] = outs
			}
```

with the loop itself changed to track the asset index — change

```go
	for _, a := range spec.Assets {
```

to

```go
	outsByAsset := make([][]compose.OutputFile, len(spec.Assets))
	for ai, a := range spec.Assets {
```

2. After the per-asset loop closes (`man.Assets = append(man.Assets, ma)` / final `}` before `m.mu.Lock()`), insert the palette pass:

```go
	// Palette lock (F1): remap every PNG output of the set onto one shared
	// palette before anything is saved, then ship the palette file. Runs
	// after per-kind composition (the outputs exist as PNGs) and before
	// SaveSetFile, so every deliverable inherits the set palette. Non-image
	// outputs (atlas/frame/bar JSON) pass through untouched.
	var palJSON []byte
	if spec.PaletteLock && ok > 0 {
		var imgs []image.Image
		for ai := range outsByAsset {
			for _, of := range outsByAsset[ai] {
				if !strings.HasSuffix(of.Name, ".png") {
					continue
				}
				im, derr := png.Decode(bytes.NewReader(of.Data))
				if derr != nil {
					// Composed outputs are our own fresh encodes; this cannot
					// happen outside corrupted pipeline state. Leave the file
					// unremapped and log — remap never fails the set.
					m.log(id, "palette lock: %s undecodable, left unremapped: %v", of.Name, derr)
					continue
				}
				imgs = append(imgs, im)
			}
		}
		pal := compose.SetPalette(imgs, 255)
		if len(pal) > 0 {
			for ai := range outsByAsset {
				for j := range outsByAsset[ai] {
					of := &outsByAsset[ai][j]
					if !strings.HasSuffix(of.Name, ".png") {
						continue
					}
					nb, rerr := compose.RemapPNG(of.Data, pal)
					if rerr != nil {
						m.log(id, "palette lock: %s left unremapped: %v", of.Name, rerr)
						continue
					}
					of.Data = nb
				}
			}
			if pj, jerr := compose.MarshalPalette(pal); jerr != nil {
				m.log(id, "palette lock: palette file: %v", jerr)
			} else {
				palJSON = pj
			}
		}
	}
```

3. Then the save loop (replaces the deleted inline block's purpose), before `m.mu.Lock()`:

```go
	for ai := range outsByAsset {
		for _, of := range outsByAsset[ai] {
			if serr := m.opts.Store.SaveSetFile(id, of.Name, of.Data); serr != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: %v", serr))
				return
			}
			files = append(files, storage.FileRef{Name: of.Name, Primary: of.Primary})
			man.Assets[ai].Files = append(man.Assets[ai].Files, of.Name)
		}
	}
	if len(palJSON) > 0 {
		pname := spec.Name + ".palette.json"
		if serr := m.opts.Store.SaveSetFile(id, pname, palJSON); serr != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: %v", serr))
			return
		}
		files = append(files, storage.FileRef{Name: pname})
	}
```

`man.Assets[ai]` indexing is safe: assets are appended to `man.Assets` unconditionally in the first loop in `spec.Assets` order, so indexes align even for error-skipped assets.

No import changes: compose.go already imports `bytes`, `image`, `image/png`, `strings`, `fmt`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestSubmitAssetSetPaletteLock -v`
Expected: PASS.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS (the restructure is covered by every existing asset-set test — `TestComposeParentHandoff`, delivery tests, kit/atlas tests).

- [ ] **Step 6: Docs**

`README.md` — in the **Agent assets** bullet (the one starting "`element` (one image..."), append after "...per-asset seeds ride the child jobs).":

> Setting `"palette_lock": true` builds one shared palette (up to 255 colors) over every composed image of the set and remaps each pixel to its CIEDE2000-nearest palette color — batch coherence is won in post, not generation. The set ships `<name>.palette.json` with the colors (`{"colors":["#0f766e",...]}`), an ordinary set file delivered flat like the rest. GIFs built from locked sets inherit the coherence.

`skills/img-gen-assets/SKILL.md` — append a section after the Constraints section:

```markdown
## Palette lock

Set `"palette_lock": true` on the asset set request: every composed image is
remapped onto ONE shared palette (CIEDE2000 nearest color, ≤255 colors,
transparent pixels untouched) and the set ships `<name>.palette.json`. Use it
whenever separate deliverables must look like one art pass — UI bundles,
element families, sprite groups.

**Party pattern** (a group of distinct NPCs with coherent color): N `element`
assets in one set, each with its own prompt, `palette_lock: true`. Delivery
is Kenney-style separate sprites; no special kind.
```

`cmd/img-gen-mcp/main.go` — in the `request_asset_set` tool (:104-105), change the assets description part

```
assets[{kind, name, prompt, size (WxH, multiples of 16), negative, frames (atlas: 2-64), columns, transparent}]
```

to

```
assets[{kind, name, prompt, size (WxH, multiples of 16), negative, frames (atlas: 2-64), columns, transparent}] + palette_lock (build one shared coherent palette over the set's images, ships <name>.palette.json)
```

- [ ] **Step 7: Full suite green + commit**

```bash
go vet ./... && go test ./...
git add internal/queue/compose.go internal/queue/compose_test.go README.md skills/img-gen-assets/SKILL.md cmd/img-gen-mcp/main.go
git commit -m "feat(queue): palette_lock set coherence remap + palette file

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 3: compose frame.go — nine-slice punch + frame deliverable (F2 core)

**Files:**
- Create: `internal/compose/frame.go`
- Test: `internal/compose/frame_test.go`

**Interfaces:**
- Consumes: `ComposeElement(name string, white, black []byte, transparent bool) (OutputFile, error)` (internal/compose/compose.go:25).
- Produces (used by Task 4): `PunchNineSlice(img image.Image, m int) (*image.NRGBA, error)`, `ComposeFrame(name string, white, black []byte, transparent bool, margins int) ([]OutputFile, error)`; shared helper `encodePNG(img image.Image) ([]byte, error)` and struct `marginsJSON` — reused by bar.go (Task 5) and npc.go (Task 7).

- [ ] **Step 1: Write the failing test**

Create `internal/compose/frame_test.go`:

```go
package compose

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func frameInput(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 7, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPunchNineSlice(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(frameInput(t)))
	if err != nil {
		t.Fatal(err)
	}
	punched, err := PunchNineSlice(img, 16)
	if err != nil {
		t.Fatal(err)
	}
	if got := punched.NRGBAAt(32, 32); got.A != 0 {
		t.Fatalf("interior not punched: %+v", got)
	}
	if got := punched.NRGBAAt(0, 0); got != (color.NRGBA{R: 0, G: 0, B: 7, A: 255}) {
		t.Fatalf("corner pixel changed: %+v", got)
	}
	if got := punched.NRGBAAt(15, 32); got != (color.NRGBA{R: 15, G: 32, B: 7, A: 255}) {
		t.Fatalf("edge-zone pixel changed: %+v", got)
	}
	if _, err := PunchNineSlice(img, 0); err == nil {
		t.Fatal("want error for margin 0")
	}
	if _, err := PunchNineSlice(img, 32); err == nil {
		t.Fatal("want error when 2*m >= width")
	}
}

func TestComposeFrame(t *testing.T) {
	outs, err := ComposeFrame("panel", frameInput(t), nil, false, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 2 || outs[0].Name != "panel.png" || !outs[0].Primary || outs[1].Name != "panel.frame.json" {
		t.Fatalf("outputs = %+v", outs)
	}
	img, err := png.Decode(bytes.NewReader(outs[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	if img.NRGBAAt(32, 32).A != 0 {
		t.Fatal("deliverable interior not punched")
	}
	var meta struct {
		Layout  string `json:"layout"`
		Margins struct {
			Top, Right, Bottom, Left int
		} `json:"margins"`
	}
	if err := json.Unmarshal(outs[1].Data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Layout != "nine-slice" || meta.Margins.Top != 16 || meta.Margins.Right != 16 ||
		meta.Margins.Bottom != 16 || meta.Margins.Left != 16 {
		t.Fatalf("frame json = %+v", meta)
	}
}

func TestComposeFrameTransparentRequiresBothRenders(t *testing.T) {
	if _, err := ComposeFrame("panel", frameInput(t), nil, true, 16); err == nil {
		t.Fatal("transparent frame with nil black render must error (UnmixAlpha)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run 'PunchNineSlice|ComposeFrame' -v`
Expected: FAIL — `undefined: PunchNineSlice`, `undefined: ComposeFrame`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/compose/frame.go`:

```go
package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/color"
	"image/png"
)

// marginsJSON is the sidecar margins block shared by frame and bar files.
type marginsJSON struct {
	Top    int `json:"top"`
	Right  int `json:"right"`
	Bottom int `json:"bottom"`
	Left   int `json:"left"`
}

func uniformMargins(m int) marginsJSON {
	return marginsJSON{Top: m, Right: m, Bottom: m, Left: m}
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("compose: png encode: %v", err)
	}
	return buf.Bytes(), nil
}

// PunchNineSlice returns a copy with the interior rect [m, w-m)x[m, h-m)
// forced fully transparent. m must be 1..min(w,h)/2-1 — the same bounds the
// queue validates.
func PunchNineSlice(img image.Image, m int) (*image.NRGBA, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if m < 1 || 2*m >= w || 2*m >= h {
		return nil, fmt.Errorf("compose: nine-slice margin %d out of bounds for %dx%d", m, w, h)
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	for y := m; y < h-m; y++ {
		for x := m; x < w-m; x++ {
			out.SetNRGBA(x, y, color.NRGBA{})
		}
	}
	return out, nil
}

// ComposeFrame produces a nine-slice frame deliverable from the element
// render path: the border art PNG with the interior punched transparent,
// plus the margins sidecar.
func ComposeFrame(name string, white, black []byte, transparent bool, margins int) ([]OutputFile, error) {
	el, err := ComposeElement(name, white, black, transparent)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(el.Data))
	if err != nil {
		return nil, fmt.Errorf("compose: frame %q render is not a PNG: %v", name, err)
	}
	punched, err := PunchNineSlice(img, margins)
	if err != nil {
		return nil, err
	}
	data, err := encodePNG(punched)
	if err != nil {
		return nil, err
	}
	meta, err := json.Marshal(struct {
		Layout  string      `json:"layout"`
		Margins marginsJSON `json:"margins"`
	}{Layout: "nine-slice", Margins: uniformMargins(margins)})
	if err != nil {
		return nil, err
	}
	return []OutputFile{
		{Name: name + ".png", Data: data, Primary: true},
		{Name: name + ".frame.json", Data: meta},
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compose/ -run 'PunchNineSlice|ComposeFrame' -v`
Expected: PASS.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/compose/frame.go internal/compose/frame_test.go
git commit -m "feat(compose): nine-slice punch + frame deliverable

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 4: queue frame kind (F2)

**Files:**
- Modify: `internal/queue/compose.go` (`AssetSpecIn` :330, `assetSpec` :34, `SubmitAssetSet` validation switch :380, executor switch :524, new `parseAssetSize`)
- Modify: `internal/queue/compose_test.go` (test)
- Modify: `README.md`, `skills/img-gen-assets/SKILL.md`, `cmd/img-gen-mcp/main.go` (:105)

**Interfaces:**
- Consumes: Task 3's `compose.ComposeFrame(name, white, black []byte, transparent bool, margins int) ([]OutputFile, error)`; existing `elementChildren` (1-2 children).
- Produces: kind `"frame"` (request field `frame_margins`, deliverables `<name>.png` + `<name>.frame.json`); `parseAssetSize(size string) (w, h int, err error)` — reused by Task 6.
- Kind-switch contract for this and Tasks 6/8: every new kind adds (a) a `case` in the `SubmitAssetSet` validation switch building children, (b) fields on `AssetSpecIn` + `assetSpec`, (c) a `case` in the executor switch appending to `outs`. Both switch `default` messages and the `AssetSpecIn.Kind` doc comment are updated to list all supported kinds.

- [ ] **Step 1: Write the failing test**

Add to `internal/queue/compose_test.go`:

```go
func TestSubmitAssetSetFrameKind(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(11)

	// Validation: margins required, in 1..size/2-1.
	bad := AssetSetRequest{Name: "p", Seed: &seed, Assets: []AssetSpecIn{{Kind: "frame", Name: "p", Prompt: "stone", Size: "32x32"}}}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want frame_margins required error")
	}
	bad.Assets[0].FrameMargins = 16 // 2*16 >= 32
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want margins bound error")
	}

	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "frames", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "frame", Name: "panel", Prompt: "stone panel", Size: "32x32", FrameMargins: 8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	data, err := m.opts.Store.ReadSetFile(id, "panel.png")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.NRGBAAt(16, 16).A != 0 {
		t.Fatal("interior not punched")
	}
	if img.NRGBAAt(0, 0).A != 255 {
		t.Fatal("border lost")
	}
	mraw, err := m.opts.Store.ReadSetFile(id, "panel.frame.json")
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Margins struct{ Top int; Right int; Bottom int; Left int } `json:"margins"`
	}
	if err := json.Unmarshal(mraw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Margins.Top != 8 || meta.Margins.Bottom != 8 {
		t.Fatalf("margins = %+v", meta.Margins)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run TestSubmitAssetSetFrameKind -v`
Expected: FAIL — compile error (`AssetSpecIn` has no field `FrameMargins`) or `unknown kind "frame"`.

- [ ] **Step 3: Implement**

(a) `AssetSpecIn` (:330) gains the field and the kind comment:

```go
type AssetSpecIn struct {
	Kind         string `json:"kind"` // "element" | "kit" | "atlas" | "frame" | "bar" | "npc"
	Name         string `json:"name"`
	Prompt       string `json:"prompt"`
	Negative     string `json:"negative,omitempty"`
	Size         string `json:"size"`                  // WxH, multiples of 16
	Frames       int    `json:"frames,omitempty"`      // atlas: 2..64
	Columns      int    `json:"columns,omitempty"`     // atlas: 0 = single row
	Transparent  bool   `json:"transparent,omitempty"` // element: white+black double pass
	FrameMargins int    `json:"frame_margins,omitempty"` // frame|bar: 1..size/2-1
}
```

(b) `assetSpec` (:34) gains the persisted field (before `Children` so `Children` stays last):

```go
	Transparent  bool     `json:"transparent,omitempty"`
	FrameMargins int      `json:"frame_margins,omitempty"`
	Children     int      `json:"children"` // generation jobs this asset owns, in order
```

(c) Add `parseAssetSize` after `atlasChildren` (after compose.go:443):

```go
// parseAssetSize parses a size that already passed validAssetSize ("WxH",
// multiples of 16) into pixel ints, for post-processing bounds checks.
func parseAssetSize(size string) (w, h int, err error) {
	ws, hs, ok := strings.Cut(size, "x")
	if !ok {
		return 0, 0, fmt.Errorf("invalid size %q", size)
	}
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("invalid size %q", size)
	}
	return w, h, nil
}
```

Add `strconv` to compose.go's import block.

(d) Validation switch — after the existing `atlas` case (compose.go:386-391, leave that case's own text untouched), add the frame case and replace the kind-suffix in the fallthrough `default` (`unknown kind %q (want element, kit, atlas)` → six-kind list):

```go
		case "frame":
			if a.FrameMargins < 1 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins is required for frames", an)}
			}
			w, h, serr := parseAssetSize(a.Size)
			if serr != nil || 2*a.FrameMargins >= w || 2*a.FrameMargins >= h {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins must be 1..size/2-1", an)}
			}
			built = elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent)
		default:
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: unknown kind %q (want element, kit, atlas, frame, bar, npc)", an, a.Kind)}
```

(e) `as := assetSpec{...}` (:378) carries the field:

```go
		as := assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Frames: a.Frames, Columns: a.Columns, Transparent: a.Transparent, FrameMargins: a.FrameMargins}
```

(f) Executor switch — after the `atlas` case (compose.go:539), add the frame case and update the default message:

```go
			case "atlas":
				outs, err = compose.ComposeAtlas(a.Name, imgs, a.Columns)
			case "frame":
				black := []byte(nil)
				if len(imgs) == 2 {
					black = imgs[1]
				}
				outs, err = compose.ComposeFrame(a.Name, imgs[0], black, a.Transparent && len(imgs) == 2, a.FrameMargins)
			default:
				err = fmt.Errorf("unknown asset kind %q", a.Kind)
```

(The executor default message has no kind list today — leave that text as-is.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestSubmitAssetSetFrameKind -v`
Expected: PASS.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Docs**

`README.md` — in the **Agent assets** bullet, extend the kinds sentence so `atlas` reads:

> ...`atlas` (2–64 frame sheet + TexturePacker-style JSON), or `frame` (a nine-slice border PNG: `frame_margins` 1..size/2−1 punches the interior transparent, one sidecar `<name>.frame.json`).

`skills/img-gen-assets/SKILL.md` — add a table row after the atlas row and a constraints-neutral note:

```markdown
| `frame` | prompt, size, `frame_margins` 1..size/2-1 | `<name>.png` (interior punched transparent) + `<name>.frame.json` |
```

`cmd/img-gen-mcp/main.go` — request_asset_set schema (:105): inside the assets description, after `transparent`, add `, frame_margins (frame|bar: 1..size/2-1)` and change `(atlas: 2-64), columns, transparent}] + palette_lock` to `(atlas: 2-64), columns, transparent, frame_margins (frame|bar: 1..size/2-1)} + palette_lock`; and the tool description kind list becomes `(element | 20-class wang kit | atlas | frame)`.

- [ ] **Step 7: Full suite green + commit**

```bash
go vet ./... && go test ./...
git add internal/queue/compose.go internal/queue/compose_test.go README.md skills/img-gen-assets/SKILL.md cmd/img-gen-mcp/main.go
git commit -m "feat(queue): frame asset kind (nine-slice) + docs

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 5: compose bar.go — punched track, clipped fill states (F3 core)

**Files:**
- Create: `internal/compose/bar.go`
- Test: `internal/compose/bar_test.go`

**Interfaces:**
- Consumes: `ComposeElement` (compose.go:25), Task 3's `PunchNineSlice`, `marginsJSON`, `uniformMargins`, `encodePNG`.
- Produces (used by Task 6): `ClipFillPNG(img image.Image, pct int) (*image.NRGBA, error)`, `ComposeBar(name string, trackWhite, trackBlack []byte, transparent bool, margins int, fill []byte, states []int) ([]OutputFile, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/compose/bar_test.go`:

```go
package compose

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func barRender(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestClipFillPNG(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(barRender(t, 100, 20, color.NRGBA{R: 220, G: 20, B: 60, A: 255})))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ClipFillPNG(img, 25); err != nil || got.Bounds().Dx() != 25 {
		t.Fatalf("25%% width = %d, err=%v", got.Bounds().Dx(), err)
	}
	if got, err := ClipFillPNG(img, 100); err != nil || got.Bounds().Dx() != 100 {
		t.Fatalf("100%% width = %d, err=%v", got.Bounds().Dx(), err)
	}
	if got, err := ClipFillPNG(img, 1); err != nil || got.Bounds().Dx() != 1 {
		t.Fatalf("1%% width = %d (floor keeps 1px), err=%v", got.Bounds().Dx(), err)
	}
	if _, err := ClipFillPNG(img, 0); err == nil {
		t.Fatal("want error for 0")
	}
	if _, err := ClipFillPNG(img, 101); err == nil {
		t.Fatal("want error for 101")
	}
}

func TestComposeBar(t *testing.T) {
	track := barRender(t, 64, 16, color.NRGBA{R: 30, G: 30, B: 34, A: 255})
	fill := barRender(t, 64, 16, color.NRGBA{R: 220, G: 20, B: 60, A: 255})
	outs, err := ComposeBar("health", track, nil, false, 5, fill, []int{25, 50, 75, 100})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"health-track.png": false, "health-fill.png": false,
		"health-fill-025.png": false, "health-fill-050.png": false,
		"health-fill-075.png": false, "health-fill-100.png": false,
		"health.bar.json": false,
	}
	if len(outs) != len(want) {
		t.Fatalf("outputs = %d files, want %d", len(outs), len(want))
	}
	for _, of := range outs {
		if _, ok := want[of.Name]; !ok {
			t.Fatalf("unexpected output %q", of.Name)
		}
		want[of.Name] = true
		if of.Name == "health-track.png" && !of.Primary {
			t.Fatal("track must be primary")
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("missing output %q", name)
		}
	}
	// Track interior punched; a state clip is the right width.
	dataByID := map[string][]byte{}
	for _, of := range outs {
		dataByID[of.Name] = of.Data
	}
	trackImg, err := png.Decode(bytes.NewReader(dataByID["health-track.png"]))
	if err != nil {
		t.Fatal(err)
	}
	if trackImg.Bounds().Dx() != 64 || trackImg.NRGBAAt(10, 8).A != 0 {
		t.Fatal("track punch wrong")
	}
	if trackImg.NRGBAAt(10, 2).A != 255 {
		t.Fatal("track 5px border lost")
	}
	clip25, err := png.Decode(bytes.NewReader(dataByID["health-fill-025.png"]))
	if err != nil {
		t.Fatal(err)
	}
	if clip25.Bounds().Dx() != 16 || clip25.Bounds().Dy() != 16 {
		t.Fatalf("25%% clip = %dx%d, want 16x16", clip25.Bounds().Dx(), clip25.Bounds().Dy())
	}
	var meta struct {
		Track   string      `json:"track"`
		Fill    string      `json:"fill"`
		Margins marginsJSON `json:"margins"`
		States  []int       `json:"states"`
	}
	if err := json.Unmarshal(dataByID["health.bar.json"], &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Track != "health-track.png" || meta.Fill != "health-fill.png" || meta.Margins.Top != 5 {
		t.Fatalf("bar json = %+v", meta)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run 'ClipFill|ComposeBar' -v`
Expected: FAIL — `undefined: ClipFillPNG`, `undefined: ComposeBar`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/compose/bar.go`:

```go
package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

// ClipFillPNG returns img clipped to the left pct% of its width (full
// height). Smaller sizes floor to one pixel so even a 1% state shows a
// sliver of fill.
func ClipFillPNG(img image.Image, pct int) (*image.NRGBA, error) {
	if pct < 1 || pct > 100 {
		return nil, fmt.Errorf("compose: fill state %d out of range 1..100", pct)
	}
	b := img.Bounds()
	nw := b.Dx() * pct / 100
	if nw < 1 {
		nw = 1
	}
	out := image.NewNRGBA(image.Rect(0, 0, nw, b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	return out, nil
}

// ComposeBar produces a bar's deliverables: the nine-slice-punched track
// (primary), the full fill render, one clipped PNG per state, and the bar
// JSON. The fill is never re-rendered — every state is a horizontal clip of
// the single render.
func ComposeBar(name string, trackWhite, trackBlack []byte, transparent bool, margins int, fill []byte, states []int) ([]OutputFile, error) {
	el, err := ComposeElement(name, trackWhite, trackBlack, transparent)
	if err != nil {
		return nil, err
	}
	trackImg, err := png.Decode(bytes.NewReader(el.Data))
	if err != nil {
		return nil, fmt.Errorf("compose: bar %q track render is not a PNG: %v", name, err)
	}
	track, err := PunchNineSlice(trackImg, margins)
	if err != nil {
		return nil, err
	}
	fillImg, err := png.Decode(bytes.NewReader(fill))
	if err != nil {
		return nil, fmt.Errorf("compose: bar %q fill render is not a PNG: %v", name, err)
	}
	trackData, err := encodePNG(track)
	if err != nil {
		return nil, err
	}
	fillData, err := encodePNG(fillImg)
	if err != nil {
		return nil, err
	}
	files := []OutputFile{
		{Name: name + "-track.png", Data: trackData, Primary: true},
		{Name: name + "-fill.png", Data: fillData},
	}
	for _, pct := range states {
		clipped, cerr := ClipFillPNG(fillImg, pct)
		if cerr != nil {
			return nil, cerr
		}
		cd, cerr := encodePNG(clipped)
		if cerr != nil {
			return nil, cerr
		}
		files = append(files, OutputFile{Name: fmt.Sprintf("%s-fill-%03d.png", name, pct), Data: cd})
	}
	meta, err := json.Marshal(struct {
		Track   string      `json:"track"`
		Fill    string      `json:"fill"`
		Margins marginsJSON `json:"margins"`
		States  []int       `json:"states"`
	}{Track: name + "-track.png", Fill: name + "-fill.png", Margins: uniformMargins(margins), States: states})
	if err != nil {
		return nil, err
	}
	return append(files, OutputFile{Name: name + ".bar.json", Data: meta}), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compose/ -run 'ClipFill|ComposeBar' -v`
Expected: PASS.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/compose/bar.go internal/compose/bar_test.go
git commit -m "feat(compose): bar compose (punched track, clipped fill states)

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 6: queue bar kind (F3)

**Files:**
- Modify: `internal/queue/compose.go` (`AssetSpecIn`, `assetSpec`, validation switch, executor switch)
- Modify: `internal/queue/compose_test.go` (test)
- Modify: `README.md`, `skills/img-gen-assets/SKILL.md`, `cmd/img-gen-mcp/main.go` (:105)

**Interfaces:**
- Consumes: Task 5's `compose.ComposeBar(...)` signature above; Task 4's `parseAssetSize`; `elementChildren`.
- Produces: kind `"bar"` (fields `bar_fill`, `fill_states`; deliverables `<name>-track.png`, `<name>-fill.png`, `<name>-fill-<NNN>.png`, `<name>.bar.json`).

- [ ] **Step 1: Write the failing test**

Add to `internal/queue/compose_test.go`:

```go
func TestSubmitAssetSetBarKind(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(12)

	// Validation: bar_fill required, states in 1..100, <=16, ascending.
	bad := AssetSetRequest{Name: "b", Seed: &seed, Assets: []AssetSpecIn{{Kind: "bar", Name: "hb", Prompt: "track art", Size: "32x32", FrameMargins: 8}}}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want bar_fill required error")
	}
	bad.Assets[0].BarFill = "red liquid"
	bad.Assets[0].FillStates = []int{60, 30}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want ascending error")
	}
	bad.Assets[0].FillStates = []int{0, 30}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want 1..100 error")
	}

	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "bars", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "bar", Name: "health", Prompt: "wooden health bar", Size: "32x32", FrameMargins: 8, BarFill: "red liquid", FillStates: []int{30, 60}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	// Track + fill + exact state clips.
	for _, name := range []string{"health-track.png", "health-fill.png", "health-fill-030.png", "health-fill-060.png", "health.bar.json"} {
		if _, err := m.opts.Store.ReadSetFile(id, name); err != nil {
			t.Fatalf("set file %s: %v", name, err)
		}
	}
	clip, err := m.opts.Store.ReadSetFile(id, "health-fill-030.png")
	if err != nil {
		t.Fatal(err)
	}
	cimg, err := png.Decode(bytes.NewReader(clip))
	if err != nil {
		t.Fatal(err)
	}
	// Fill render is 32 wide; 30% floors to 9.
	if cimg.Bounds().Dx() != 9 || cimg.Bounds().Dy() != 32 {
		t.Fatalf("30%% clip = %dx%d, want 9x32", cimg.Bounds().Dx(), cimg.Bounds().Dy())
	}

	// Three children: track (1, opaque by default) + fill (1).
	cap.mu.Lock()
	n := len(cap.Prompts)
	cap.mu.Unlock()
	if n != 2 {
		t.Fatalf("children = %d, want 2", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run TestSubmitAssetSetBarKind -v`
Expected: FAIL — compile error (`AssetSpecIn` has no field `BarFill`) or `unknown kind "bar"`.

- [ ] **Step 3: Implement**

(a) `AssetSpecIn` gains (after `FrameMargins`):

```go
	BarFill       string `json:"bar_fill,omitempty"`       // bar: fill art prompt
	FillStates    []int  `json:"fill_states,omitempty"`    // bar: 1..100, <=16, ascending
```

(b) `assetSpec` gains the same fields (between `FrameMargins` and `Children`):

```go
	FrameMargins int      `json:"frame_margins,omitempty"`
	BarFill      string   `json:"bar_fill,omitempty"`
	FillStates   []int    `json:"fill_states,omitempty"`
	Children     int      `json:"children"`
```

(c) Restructure the persistence into the kind cases and add the bar case. The current loop declares `as := assetSpec{...}` before the switch (compose.go:378) and `as.Children = len(built)` after it — move the `as` construction INTO each `case` (every case assigns all-and-only fields its kind uses), delete the shared pre-switch line, and keep `as.Children = len(built)` after the switch. The loop body's switch becomes, complete (frame case included, Task 4's):

```go
		var built []SubmitRequest
		var as assetSpec
		switch a.Kind {
		case "element":
			built = elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Transparent: a.Transparent}
		case "kit":
			built = kitChildren(prompt, a.Negative, a.Size, *req.Seed)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size}
		case "atlas":
			if a.Frames < 2 || a.Frames > 64 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frames must be 2..64", an)}
			}
			built = atlasChildren(prompt, a.Negative, a.Size, *req.Seed, a.Frames)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Frames: a.Frames, Columns: a.Columns}
		case "frame":
			if a.FrameMargins < 1 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins is required for frames", an)}
			}
			w, h, serr := parseAssetSize(a.Size)
			if serr != nil || 2*a.FrameMargins >= w || 2*a.FrameMargins >= h {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins must be 1..size/2-1", an)}
			}
			built = elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Transparent: a.Transparent, FrameMargins: a.FrameMargins}
		case "bar":
			if a.BarFill == "" {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: bar_fill is required for bars", an)}
			}
			if a.FrameMargins < 1 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins is required for bars", an)}
			}
			w, h, serr := parseAssetSize(a.Size)
			if serr != nil || 2*a.FrameMargins >= w || 2*a.FrameMargins >= h {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins must be 1..size/2-1", an)}
			}
			states := a.FillStates
			if len(states) == 0 {
				states = []int{25, 50, 75, 100}
			}
			seenSt := map[int]bool{}
			uniq := []int{}
			for _, st := range states {
				if st < 1 || st > 100 {
					return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: fill_states values must be 1..100", an)}
				}
				if seenSt[st] {
					continue
				}
				seenSt[st] = true
				uniq = append(uniq, st)
			}
			if len(uniq) > 16 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: fill_states takes at most 16 states", an)}
			}
			for i := 1; i < len(uniq); i++ {
				if uniq[i] <= uniq[i-1] {
					return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: fill_states must be ascending", an)}
				}
			}
			built = append(elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent),
				elementChildren(a.BarFill, a.Negative, a.Size, *req.Seed, false)...)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Transparent: a.Transparent, FrameMargins: a.FrameMargins, BarFill: a.BarFill, FillStates: states}
		default:
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: unknown kind %q (want element, kit, atlas, frame, bar, npc)", an, a.Kind)}
		}
		as.Children = len(built)
```

The loop's existing dup-check/prompt/size scaffolding above the switch (compose.go:362-377) is untouched. Task 8 later inserts the `npc` case between `bar` and `default`, following the same per-case shape.

(d) Executor — after the `frame` case (Task 4):

```go
			case "bar":
				fill := imgs[len(imgs)-1]
				trackBlack := []byte(nil)
				if len(imgs) == 3 {
					trackBlack = imgs[1]
				}
				outs, err = compose.ComposeBar(a.Name, imgs[0], trackBlack, a.Transparent && len(imgs) == 3, a.FrameMargins, fill, a.FillStates)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestSubmitAssetSetBarKind -v`
Expected: PASS.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS (all existing element/kit/atlas/frame tests still pass after the `as` consolidation).

- [ ] **Step 6: Docs**

`README.md` kinds sentence gains: **`bar`** (track + fill pair: `bar_fill` prompt renders the fill; `fill_states` 1..100, ≤16 ascending, default 25/50/75/100 render as horizontal clips `<name>-fill-<NNN>.png`; the track interior punches transparent via `frame_margins`; sidecar `<name>.bar.json`).

`skills/img-gen-assets/SKILL.md` — add table row:

```markdown
| `bar` | prompt (track), `bar_fill` (fill), `frame_margins`, `fill_states` | `<name>-track.png`, `<name>-fill.png`, `<name>-fill-<NNN>.png`, `<name>.bar.json` |
```

MCP `request_asset_set` schema: assets description gains `, bar_fill (bar), fill_states (bar: 1..100, <=16, ascending, default 25/50/75/100)` after frame_margins; description kind list becomes `(element | 20-class wang kit | atlas | frame | bar)`.

- [ ] **Step 7: Full suite green + commit**

```bash
go vet ./... && go test ./...
git add internal/queue/compose.go internal/queue/compose_test.go README.md skills/img-gen-assets/SKILL.md cmd/img-gen-mcp/main.go
git commit -m "feat(queue): bar asset kind + docs

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 7: compose npc.go — mirror flip + walk4 compose (F4 core)

**Files:**
- Create: `internal/compose/npc.go`
- Test: `internal/compose/npc_test.go`

**Interfaces:**
- Consumes: `PackGrid(name string, frames []image.Image, columns int) ([]byte, []AtlasFrame, error)` and `MarshalAtlas(metas []AtlasFrame) ([]byte, error)` (internal/compose/atlas.go:31,70); `encodePNG`.
- Produces (used by Task 8): `FlipHorizontal(img image.Image) *image.NRGBA`, `ComposeNPCWalk4(name string, down, up, right []byte) ([]OutputFile, error)`. Variant sheets reuse `ComposeAtlas` — no new compose code for them.

- [ ] **Step 1: Write the failing test**

Create `internal/compose/npc_test.go`:

```go
package compose

import (
	"bytes"
	"image/png"
	"testing"
)

func TestFlipHorizontal(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	img.SetNRGBA(0, 0, colA)
	img.SetNRGBA(1, 0, colB)
	img.SetNRGBA(2, 0, colC)
	out := FlipHorizontal(img)
	if out.NRGBAAt(0, 0) != colC || out.NRGBAAt(1, 0) != colB || out.NRGBAAt(2, 0) != colA {
		t.Fatalf("flip wrong: %+v", out)
	}
}
```

with `colA`, `colB`, `colC` declared at the top of the file:

```go
var (
	colA = nrgba(255, 0, 0)
	colB = nrgba(0, 255, 0)
	colC = nrgba(0, 0, 255)
)

func nrgba(r, g, b uint8) (c color.NRGBA) { return color.NRGBA{R: r, G: g, B: b, A: 255} }
```

(add `"image/color"` to the imports). And the walk4 test:

```go
func TestComposeNPCWalk4(t *testing.T) {
	down := barRender(t, 32, 32, color.NRGBA{R: 10, A: 255})
	up := barRender(t, 32, 32, color.NRGBA{G: 10, A: 255})
	right := barRender(t, 32, 32, color.NRGBA{B: 10, A: 255})
	outs, err := ComposeNPCWalk4("guard", down, up, right)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string][]byte{}
	want := []string{"guard-walk.png", "guard-atlas.json", "guard-down.png", "guard-left.png", "guard-right.png", "guard-up.png"}
	if len(outs) != len(want) {
		t.Fatalf("outputs = %d, want %d", len(outs), len(want))
	}
	for _, of := range outs {
		byName[of.Name] = of.Data
	}
	for _, name := range want {
		if byName[name] == nil {
			t.Fatalf("missing %q", name)
		}
	}
	if !outs[0].Primary || outs[0].Name != "guard-walk.png" {
		t.Fatalf("primary = %q", outs[0].Name)
	}
	// Sheet is 4 rows x 1 col: 32x128.
	sheet, err := png.Decode(bytes.NewReader(byName["guard-walk.png"]))
	if err != nil {
		t.Fatal(err)
	}
	if sheet.Bounds().Dx() != 32 || sheet.Bounds().Dy() != 128 {
		t.Fatalf("sheet = %dx%d, want 32x128", sheet.Bounds().Dx(), sheet.Bounds().Dy())
	}
	// left is a true mirror of right.
	limg, err := png.Decode(bytes.NewReader(byName["guard-left.png"]))
	if err != nil {
		t.Fatal(err)
	}
	rimg, err := png.Decode(bytes.NewReader(byName["guard-right.png"]))
	if err != nil {
		t.Fatal(err)
	}
	w := rimg.Bounds().Dx()
	for y := 0; y < 32; y++ {
		for x := 0; x < w; x++ {
			if limg.NRGBAAt(x, y) != rimg.NRGBAAt(w-1-x, y) {
				t.Fatalf("left(%d,%d) != mirror right", x, y)
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run 'Flip|Walk4' -v`
Expected: FAIL — `undefined: FlipHorizontal`, `undefined: ComposeNPCWalk4`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/compose/npc.go`:

```go
package compose

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// FlipHorizontal mirrors img left-right: column x becomes column w-1-x.
func FlipHorizontal(img image.Image) *image.NRGBA {
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.SetNRGBA(b.Dx()-1-(x-b.Min.X), y, color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA))
		}
	}
	return out
}

type npcCell struct {
	Name string
	Img  image.Image
}

// ComposeNPCWalk4 packs the three direction renders (down, up, right) plus
// the mirrored left cell into the down, left, right, up sheet — RPG-Maker
// order, the 3-drawing rule (left is a flip of right, never re-rendered).
func ComposeNPCWalk4(name string, down, up, right []byte) ([]OutputFile, error) {
	var dirs []npcCell
	for i, b := range [][]byte{down, up, right} {
		im, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("compose: walk4 %q direction %d is not a PNG: %v", name, i, err)
		}
		dirs = append(dirs, npcCell{Img: im})
	}
	left := npcCell{Img: FlipHorizontal(dirs[2].Img)}
	cells := []npcCell{{Img: dirs[0].Img}, left, {Img: dirs[2].Img}, {Img: dirs[1].Img}}
	sheet, metas, err := PackGrid(name, []image.Image{cells[0].Img, cells[1].Img, cells[2].Img, cells[3].Img}, 1)
	if err != nil {
		return nil, err
	}
	metaJSON, err := MarshalAtlas(metas)
	if err != nil {
		return nil, err
	}
	files := []OutputFile{
		{Name: name + "-walk.png", Data: sheet, Primary: true},
		{Name: name + "-atlas.json", Data: metaJSON},
	}
	names := []string{"down", "left", "right", "up"}
	for i, c := range cells {
		data, err := encodePNG(c.Img)
		if err != nil {
			return nil, err
		}
		files = append(files, OutputFile{Name: fmt.Sprintf("%s-%s.png", name, names[i]), Data: data})
	}
	return files, nil
}
```

Note: the `npcCell` split keeps the loop readable; if you find it indirection-for-its-own-sake while implementing, collapse it to plain `[]image.Image` — behavior is identical (keep the direction index comment: order is down, up, right from the caller).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compose/ -run 'Flip|Walk4' -v`
Expected: PASS.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/compose/npc.go internal/compose/npc_test.go
git commit -m "feat(compose): walk4 npc compose (3-drawing mirror)

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 8: queue npc kind — pinned-seed variants + walk4 (F4)

**Files:**
- Modify: `internal/queue/compose.go` (`AssetSpecIn`, `assetSpec`, `manifestAsset`, validation switch, `npcChildren`, executor switch)
- Modify: `internal/queue/compose_test.go` (test)
- Modify: `README.md`, `skills/img-gen-assets/SKILL.md`, `cmd/img-gen-mcp/main.go` (:105)

**Interfaces:**
- Consumes: `compose.ComposeAtlas` (variants sheets — existing), Task 7's `compose.ComposeNPCWalk4(name, down, up, right []byte)`.
- Produces: kind `"npc"` with `shape` (`variants` | `walk4`), `variants` suffixes; children ALL at the set seed (no `+i`); deliverables per spec §6.
- Honesty contract (must appear in docs): walk4 delivers standing direction views whose palette/composition form a family — NOT a walk animation, not guaranteed single-identity across directions.

- [ ] **Step 1: Write the failing test**

Add to `internal/queue/compose_test.go`:

```go
func TestSubmitAssetSetNPCVariants(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(41)

	bad := AssetSetRequest{Name: "n", Seed: &seed, Assets: []AssetSpecIn{{Kind: "npc", Name: "vg", Prompt: "villager", Size: "32x32"}}}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want npc shape required error")
	}
	bad.Assets[0].Shape = "variants"
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want variants 2..64 error")
	}

	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "npc", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "npc", Name: "villager", Prompt: "villager sprite", Size: "32x32",
			Shape: "variants", Variants: []string{"idle", "wave", "angry"}, Columns: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	// Pinned seed: every child at the set seed, suffix appended.
	cap.mu.Lock()
	defer cap.mu.Unlock()
	wantP := []string{"villager sprite, idle", "villager sprite, wave", "villager sprite, angry"}
	for i, wp := range wantP {
		if cap.Prompts[i] != wp {
			t.Fatalf("prompt %d = %q, want %q", i, cap.Prompts[i], wp)
		}
		if cap.Seeds[i] != 41 {
			t.Fatalf("seed %d = %d, want pinned 41", i, cap.Seeds[i])
		}
	}
	if _, err := m.opts.Store.ReadSetFile(id, "villager-sheet.png"); err != nil {
		t.Fatalf("variant sheet: %v", err)
	}
}

func TestSubmitAssetSetNPCWalk4(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(41)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "npcw", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "npc", Name: "guard", Prompt: "town guard", Size: "32x32", Shape: "walk4"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	cap.mu.Lock()
	wantP := []string{
		"town guard, full front view, facing the viewer",
		"town guard, rear view, facing away from the viewer",
		"town guard, side profile view, facing right",
	}
	for i, wp := range wantP {
		if cap.Prompts[i] != wp {
			t.Fatalf("prompt %d = %q, want %q", i, cap.Prompts[i], wp)
		}
		if cap.Seeds[i] != 41 {
			t.Fatalf("seed %d = %d, want pinned 41", i, cap.Seeds[i])
		}
	}
	cap.mu.Unlock()
	for _, name := range []string{"guard-walk.png", "guard-atlas.json", "guard-down.png", "guard-left.png", "guard-right.png", "guard-up.png"} {
		if _, err := m.opts.Store.ReadSetFile(id, name); err != nil {
			t.Fatalf("set file %s: %v", name, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run TestSubmitAssetSetNPC -v`
Expected: FAIL — compile error (`AssetSpecIn` has no field `Shape`) or `unknown kind "npc"`.

- [ ] **Step 3: Implement**

(a) `AssetSpecIn` gains (after `FillStates`):

```go
	Shape         string   `json:"shape,omitempty"`         // npc: "variants" | "walk4"
	Variants      []string `json:"variants,omitempty"`      // npc variants: per-variant prompt suffixes
```

(b) `assetSpec` gains the same fields (between `FillStates` and `Children`).

(c) `manifestAsset` (:458) gains:

```go
type manifestAsset struct {
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Prompt   string   `json:"prompt,omitempty"`
	Size     string   `json:"size,omitempty"`
	Variants []string `json:"variants,omitempty"`
	Files    []string `json:"files,omitempty"`
	Error    string   `json:"error,omitempty"`
}
```

(d) `npcChildren` after `atlasChildren` (next to Task 4's `parseAssetSize`):

```go
// npcChildren builds one job per npc variant, ALL at the pinned set seed —
// no +i increment. Same composition scaffold, per-variant content: the
// variant-family mechanic (walk4 gets the three fixed direction suffixes;
// left is mirrored downstream, never rendered).
func npcChildren(prompt, negative, size string, seed int64, shape string, variants []string) []SubmitRequest {
	suffixes := variants
	if shape == "walk4" {
		suffixes = []string{
			"full front view, facing the viewer",
			"rear view, facing away from the viewer",
			"side profile view, facing right",
		}
	}
	kids := make([]SubmitRequest, 0, len(suffixes))
	for _, v := range suffixes {
		s := seed
		kids = append(kids, SubmitRequest{Mode: "asset", Prompt: prompt + ", " + v, NegativePrompt: negative, Size: size, Seed: &s})
	}
	return kids
}
```

(e) Validation switch — after the `bar` case:

```go
		case "npc":
			switch a.Shape {
			case "variants":
				if len(a.Variants) < 2 || len(a.Variants) > 64 {
					return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: npc variants takes 2..64 suffixes", an)}
				}
				for i, v := range a.Variants {
					if strings.TrimSpace(v) == "" {
						return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: npc variant %d is empty", an, i)}
					}
				}
			case "walk4":
				// Fixed direction suffixes; any provided variants list is ignored.
			default:
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: npc shape must be variants or walk4", an)}
			}
			built = npcChildren(prompt, a.Negative, a.Size, *req.Seed, a.Shape, a.Variants)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Shape: a.Shape, Variants: a.Variants}
```

(Using the Task 6 per-case `as` structure.)

(f) Executor — after the `bar` case:

```go
			case "npc":
				if a.Shape == "walk4" {
					outs, err = compose.ComposeNPCWalk4(a.Name, imgs[0], imgs[1], imgs[2])
				} else {
					outs, err = compose.ComposeAtlas(a.Name, imgs, a.Columns)
				}
```

(g) In the executor's `ma := manifestAsset{...}` line (compose.go:499), carry variants:

```go
		ma := manifestAsset{Kind: a.Kind, Name: a.Name, Prompt: a.Prompt, Size: a.Size, Variants: a.Variants}
```

(walk4 leaves `Variants` empty — it carries none.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/queue/ -run TestSubmitAssetSetNPC -v`
Expected: PASS.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Docs (incl. the honesty contract)**

`README.md` — kinds sentence gains: **`npc`** (`shape: "variants"` packs per-variant sprites — every variant renders with the SAME set seed and a `", "` + suffix prompt — into `<name>-sheet.png` + atlas JSON; `shape: "walk4"` renders three direction views (down/up/right suffixes fixed, left mirrored from right) into a 4-row RPG-Maker-order sheet `<name>-walk.png` with individual cells. Standing views, not walk animation.)

`skills/img-gen-assets/SKILL.md` — add table row + a section:

```markdown
| `npc` | prompt, `shape` ("variants"|"walk4"), `variants` (suffixes) | variants: `<name>-sheet.png` + `<name>-atlas.json`; walk4: `<name>-walk.png`, 4 cells, `<name>-atlas.json` |
```

```markdown
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
```

MCP schema: assets description gains `, shape (npc: variants|walk4), variants (npc: 2-64 prompt suffixes)`; kind list becomes `(element | 20-class wang kit | atlas | frame | bar | npc)`.

- [ ] **Step 7: Full suite green + commit**

```bash
go vet ./... && go test ./...
git add internal/queue/compose.go internal/queue/compose_test.go README.md skills/img-gen-assets/SKILL.md cmd/img-gen-mcp/main.go
git commit -m "feat(queue): npc asset kind (pinned-seed variants, walk4) + docs

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 9: compose composite.go — alpha-correct blit (F5 core)

**Files:**
- Create: `internal/compose/composite.go`
- Test: `internal/compose/composite_test.go`

**Interfaces:**
- Produces (used by Task 10): `BlitImage(base, overlay image.Image, x, y int) (*image.NRGBA, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/compose/composite_test.go`:

```go
package compose

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngBytes(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodePNG(t *testing.T, data []byte) *image.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	n := image.NewNRGBA(img.Bounds())
	// draw via NRGBAModel so any source representation lands in NRGBA space
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			n.SetNRGBA(x-img.Bounds().Min.X, y-img.Bounds().Min.Y, color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA))
		}
	}
	return n
}

func TestBlitImage(t *testing.T) {
	base := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			base.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	ovr := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			ovr.SetNRGBA(x, y, color.NRGBA{B: 255, A: 255})
		}
	}
	out, err := BlitImage(base, ovr, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NRGBAAt(4, 4); got != (color.NRGBA{B: 255, A: 255}) {
		t.Fatalf("blitted pixel = %+v", got)
	}
	if got := out.NRGBAAt(0, 0); got != (color.NRGBA{R: 255, A: 255}) {
		t.Fatalf("base pixel changed: %+v", got)
	}

	// Fully transparent overlay leaves base untouched (alpha-correct Over).
	clear := image.NewNRGBA(image.Rect(0, 0, 2, 2)) // all A=0
	out2, err := BlitImage(base, clear, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := out2.NRGBAAt(0, 0); got != (color.NRGBA{R: 255, A: 255}) {
		t.Fatalf("zero-alpha overlay altered base: %+v", got)
	}

	// Overhang: error naming the geometry.
	if _, err := BlitImage(base, ovr, 7, 7); err == nil {
		t.Fatal("want overhang error")
	}
	if _, err := BlitImage(base, ovr, -1, 0); err == nil {
		t.Fatal("want negative-pos error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run TestBlitImage -v`
Expected: FAIL — `undefined: BlitImage`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/compose/composite.go`:

```go
package compose

import (
	"fmt"
	"image"
	"image/draw"
)

// BlitImage alpha-composites overlay onto a copy of base with the overlay's
// top-left corner at (x, y). The overlay must fit inside the base canvas —
// an overhang is an error naming the geometry, never a silent crop.
func BlitImage(base, overlay image.Image, x, y int) (*image.NRGBA, error) {
	bb, ob := base.Bounds(), overlay.Bounds()
	if x < 0 || y < 0 || x+ob.Dx() > bb.Dx() || y+ob.Dy() > bb.Dy() {
		return nil, fmt.Errorf("compose: static layer %dx%d at (+%d,+%d) overhangs canvas %dx%d", ob.Dx(), ob.Dy(), x, y, bb.Dx(), bb.Dy())
	}
	out := image.NewNRGBA(image.Rect(0, 0, bb.Dx(), bb.Dy()))
	draw.Draw(out, out.Bounds(), base, bb.Min, draw.Src)
	draw.Draw(out, image.Rect(x, y, x+ob.Dx(), y+ob.Dy()), overlay, ob.Min, draw.Over)
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compose/ -run TestBlitImage -v`
Expected: PASS.

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/compose/composite.go internal/compose/composite_test.go
git commit -m "feat(compose): alpha-correct blit with overhang error

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 10: queue composite GIF source (F5)

**Files:**
- Modify: `internal/queue/compose.go` (`FrameRef` :650, `GifSource` :641, `resolveFrameRef` :658, `gifPayload` :45, `SubmitGIF` switch :722, `composeGIF` :787, const `maxComposeStatics`)
- Modify: `internal/queue/compose_test.go` (tests + `b64PNG` helper)
- Modify: `README.md`, `skills/img-gen-animation/SKILL.md`, `cmd/img-gen-mcp/main.go` (:109-110)

**Interfaces:**
- Consumes: Task 9's `compose.BlitImage(base, overlay image.Image, x, y int) (*image.NRGBA, error)`.
- Produces: `source.type: "composite"` on `POST /api/v1/gifs`; `FrameRef` gains `{set, file}` resolution; statics persist as `static-layer-<NN>.png`, anim frames as the existing `source-frame-<NNNN>.png`; `gifPayload.Composite` persisted so restarts recompose the layers.
- Ruling: anim frames enforce the existing 2..64 from_frames rule rather than the spec sketch's "≥ 1 anim layer" — a single-frame animated GIF is dead weight, and the existing rule keeps the uniform-dimensions validation intact (spec §7 prose vs from_frames precedent resolves to precedent).

- [ ] **Step 1: Write the failing tests**

Add to `internal/queue/compose_test.go` (add `image/draw`, `encoding/base64`, `image/color` to the import block):

```go
func b64PNG(t *testing.T, w, h int, c color.NRGBA) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestGifCompositeSource(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	red := b64PNG(t, 32, 32, color.NRGBA{R: 255, A: 255})
	blue := b64PNG(t, 8, 8, color.NRGBA{B: 255, A: 255})
	teal := b64PNG(t, 4, 4, color.NRGBA{B: 255, G: 255, A: 255})
	id, err := m.SubmitGIF(GifRequest{
		Name: "title",
		Source: GifSource{Type: "composite", Composite: &CompositeSource{
			Anim: []FrameRef{{B64: red}, {B64: red}},
			Statics: []StaticLayer{
				{From: FrameRef{B64: blue}, Pos: [2]int{24, 24}},
				{From: FrameRef{B64: teal}, Pos: [2]int{0, 0}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	j, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.ImagePath == "" {
		t.Fatal("gif not composed")
	}
	// Set artifacts: two anim frames, both static layers (list order = file
	// order), the animation.
	for _, name := range []string{"source-frame-0000.png", "source-frame-0001.png", "static-layer-00.png", "static-layer-01.png", "title.gif"} {
		if _, err := m.opts.Store.ReadSetFile(id, name); err != nil {
			t.Fatalf("set file %s: %v", name, err)
		}
	}
	// The composed frame carries the overlay: the saved source frames are
	// pre-composite anim frames; the GIF is the proof. Verify the static
	// layer round-trips as a readable PNG of the right size.
	data, err := m.opts.Store.ReadSetFile(id, "static-layer-00.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestGifCompositeOverhangFailsCompose(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	red := b64PNG(t, 32, 32, color.NRGBA{R: 255, A: 255})
	blue := b64PNG(t, 8, 8, color.NRGBA{B: 255, A: 255})
	id, err := m.SubmitGIF(GifRequest{
		Name: "badlayer",
		Source: GifSource{Type: "composite", Composite: &CompositeSource{
			Anim:    []FrameRef{{B64: red}, {B64: red}},
			Statics: []StaticLayer{{From: FrameRef{B64: blue}, Pos: [2]int{28, 28}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	j, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(j.Error, "static-layer") {
		t.Fatalf("overhang error must name the file: %q", j.Error)
	}
}

func TestResolveFrameRefSetFile(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(1)
	sid, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "src", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "dot", Prompt: "dot", Size: "16x16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, sid, "done")

	data, err := m.resolveFrameRef(FrameRef{Set: sid, File: "dot.png"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.resolveFrameRef(FrameRef{Set: sid}); err == nil {
		t.Fatal("want error for set without file")
	}
	if _, err := m.resolveFrameRef(FrameRef{Set: sid, File: "nope.png"}); err == nil {
		t.Fatal("want error for missing file")
	}
	if _, err := m.resolveFrameRef(FrameRef{Set: sid, File: "dot.png", Path: "x"}); err == nil {
		t.Fatal("want error for mixed source kinds")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/queue/ -run 'GifComposite|ResolveFrameRefSetFile' -v`
Expected: FAIL — compile error (`CompositeSource` undefined) or `source.type must be from_frames, from_spec, or keyframes`.

- [ ] **Step 3: Implement**

(a) `FrameRef` (:650) gains the set-file form and its comment changes:

```go
// FrameRef is one source frame: exactly one of Job (an img-gen job id),
// Path (data-dir-relative, no "..", no absolute paths), B64 (inline PNG),
// or the pair Set (storage set id) + File (set file name).
type FrameRef struct {
	Job  string `json:"job,omitempty"`
	Path string `json:"path,omitempty"`
	B64  string `json:"b64,omitempty"`
	Set  string `json:"set,omitempty"`
	File string `json:"file,omitempty"`
}
```

(b) `GifSource` (:641) gains the composite kind:

```go
type GifSource struct {
	Type      string           `json:"type"`              // from_frames | from_spec | composite | keyframes
	Frames    []FrameRef       `json:"frames,omitempty"`  // from_frames: 2..64 sources
	Request   *SubmitRequest   `json:"request,omitempty"` // from_spec: base asset request
	Count     int              `json:"count,omitempty"`   // from_spec: 2..64 frames
	Composite *CompositeSource `json:"composite,omitempty"`
}

// CompositeSource layers one animated frame list (the base canvas) under
// static RGBA elements composited in list order, pre-quantization.
type CompositeSource struct {
	Anim    []FrameRef    `json:"anim"`              // 2..64 frame refs — the base canvas
	Statics []StaticLayer `json:"statics,omitempty"` // 0..8
}

// StaticLayer places one static element: From is any frame ref form, Pos is
// the x,y origin on the anim canvas — the overlay must fit inside it.
type StaticLayer struct {
	From FrameRef `json:"from"`
	Pos  [2]int   `json:"pos"`
}
```

(c) Add the limit constant next to `maxAssetsPerSet`/`maxComposeChildren` (:312-313):

```go
const (
	maxAssetsPerSet     = 8
	maxComposeChildren  = 64
	maxComposeStatics   = 8
)
```

(keep the existing parenthesization style; just add the third const line, gofmt will align).

(d) `gifPayload` (:45) gains the persisted composite layers:

```go
type compositeLayerSpec struct {
	File string `json:"file"`
	Pos  [2]int `json:"pos"`
}

type gifPayload struct {
	Frames    []string             `json:"frames,omitempty"` // set-relative source files (from_frames/composite anim)
	Composite []compositeLayerSpec `json:"composite,omitempty"`
	DelayMS   int                  `json:"delay_ms"`
	Loop      int                  `json:"loop"`
	Formats   []string             `json:"formats"`
}
```

(e) `resolveFrameRef` (:658) — extend the exactly-one rule with the set-file pair:

```go
func (m *Manager) resolveFrameRef(ref FrameRef) ([]byte, error) {
	set := 0
	for _, s := range []string{ref.Job, ref.Path, ref.B64} {
		if s != "" {
			set++
		}
	}
	switch {
	case ref.Set != "" || ref.File != "":
		if ref.Set == "" || ref.File == "" {
			return nil, fmt.Errorf("set frame source needs both set and file")
		}
		if set != 0 {
			return nil, fmt.Errorf("frame source needs exactly one of job, path, b64, set+file")
		}
		data, serr := m.opts.Store.ReadSetFile(ref.Set, ref.File)
		if serr != nil {
			return nil, serr
		}
		if _, derr := png.Decode(bytes.NewReader(data)); derr != nil {
			return nil, fmt.Errorf("frame is not a decodable PNG: %v", derr)
		}
		return data, nil
	case set != 1:
		return nil, fmt.Errorf("frame source needs exactly one of job, path, b64, set+file")
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
```

If any existing test asserts the OLD error string `frame source needs exactly one of job, path, b64`, update that assertion to the new message — same rule, just updated wording.

(f) `SubmitGIF` switch — after the `from_spec` case, add `composite` and update the fallthrough message:

```go
	case "composite":
		cs := req.Source.Composite
		if cs == nil {
			return "", &ValidationError{Code: "validation_error", Msg: "composite source needs source.composite"}
		}
		if len(cs.Anim) < 2 || len(cs.Anim) > 64 {
			return "", &ValidationError{Code: "validation_error", Msg: "composite anim takes 2..64 frames"}
		}
		if len(cs.Statics) > maxComposeStatics {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("composite takes at most %d static layers", maxComposeStatics)}
		}
		id := newID()
		names := make([]string, 0, len(cs.Anim))
		for i, ref := range cs.Anim {
			data, err := m.resolveFrameRef(ref)
			if err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("anim frame %d: %v", i, err)}
			}
			fname := fmt.Sprintf("source-frame-%04d.png", i)
			if err := m.opts.Store.SaveSetFile(id, fname, data); err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("anim frame %d: %v", i, err)}
			}
			names = append(names, fname)
		}
		statics := make([]compositeLayerSpec, 0, len(cs.Statics))
		for i, l := range cs.Statics {
			if l.Pos[0] < 0 || l.Pos[1] < 0 {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("static layer %d: pos must be non-negative", i)}
			}
			data, err := m.resolveFrameRef(l.From)
			if err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("static layer %d: %v", i, err)}
			}
			sname := fmt.Sprintf("static-layer-%02d.png", i)
			if err := m.opts.Store.SaveSetFile(id, sname, data); err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("static layer %d: %v", i, err)}
			}
			statics = append(statics, compositeLayerSpec{File: sname, Pos: l.Pos})
		}
		spec.GIF.Frames = names
		spec.GIF.Composite = statics
		return m.submitComposeParent(id, spec, nil)
	default:
		return "", &ValidationError{Code: "validation_error", Msg: "source.type must be from_frames, from_spec, or composite"}
```

(The `keyframes` case stays first and unchanged; the old default message mentioning keyframes is replaced — keyframes validation lives in its own case.)

(g) `composeGIF` — after the frame-collection switch (`frames` is fully built, before `compose.ComposeGIF`), insert:

```go
	for _, l := range gp.Composite {
		data, err := m.opts.Store.ReadSetFile(id, l.File)
		if err != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: static layer %s: %v", l.File, err))
			return
		}
		ov, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: static layer %s: %v", l.File, err))
			return
		}
		for i := range frames {
			out, berr := compose.BlitImage(frames[i], ov, l.Pos[0], l.Pos[1])
			if berr != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: static layer %s: %v", l.File, berr))
				return
			}
			frames[i] = out
		}
	}
```

The overhang error names the static file AND the geometry — fail fast, no silent crop (spec §7).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/queue/ -run 'GifComposite|ResolveFrameRefSetFile' -v`
Expected: PASS (all three).

- [ ] **Step 5: Full suite green**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Docs**

`README.md` — in the **GIFs** bullet, after the `from_spec` clause, change:

> `keyframes` (needs the phase-3 RIFE sidecar; rejected with `not_implemented` until then)

to:

> `composite` (one animated base of 2–64 frames plus up to 8 static layers, each `{from: <frame ref>, pos: [x,y]}`, alpha-composited pre-quantization — statics from already-rendered sets cost zero renders; frame refs accept `{job}`, `{path}`, `{b64}`, or `{set, file}` for set files), and `keyframes` (needs the phase-3 RIFE sidecar; rejected with `not_implemented` until then)

`skills/img-gen-animation/SKILL.md` — after the `keyframes` bullet in Frame sources, add:

```markdown
- `composite` — layer statics under an animation: `{"type":"composite",
  "composite":{"anim":[<2-64 frame refs>],"statics":[{"from":<frame ref>,
  "pos":[x,y]}, ...]}}`. The overlay must fit inside the anim canvas or the
  GIF fails at compose (named error, no silent crop). Compositing happens
  pre-quantization, so 1-bit GIF alpha stays halo-free; statics stay crisp
  across frames. Combine with palette-locked sets for coherent title
  screens.
```

MCP `make_gif` description (:109-110): `source{type: from_frames|from_spec|keyframes, frames[{job|path|b64}], request, count}` becomes `source{type: from_frames|from_spec|composite|keyframes, frames[{job|path|b64|set+file}], request, count, composite{anim[2-64 refs], statics[{from, pos[x,y]}] up to 8}}`.

- [ ] **Step 7: Full suite green + commit**

```bash
go vet ./... && go test ./...
git add internal/queue/compose.go internal/queue/compose_test.go README.md skills/img-gen-animation/SKILL.md cmd/img-gen-mcp/main.go
git commit -m "feat(queue): composite gif source (statics over anim) + docs

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 11: Final verification & docs sweep

**Files:**
- Verify only; small fixes if the sweep finds gaps.

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Full build + suite**

Run: `go vet ./... && go test ./... && go build ./...`
Expected: PASS.

- [ ] **Step 2: Docs sweep**

Check every deliverable is documented (grep, fix anything missing):

Run: `grep -c palette_lock README.md skills/img-gen-assets/SKILL.md && grep -c frame_margins README.md skills/img-gen-assets/SKILL.md && grep -c bar_fill README.md skills/img-gen-assets/SKILL.md && grep -c walk4 README.md skills/img-gen-assets/SKILL.md && grep -c composite README.md skills/img-gen-animation/SKILL.md && grep -ci honesty skills/img-gen-assets/SKILL.md`

Expected: every grep ≥ 1 per file listed. The honesty contract text must exist in `skills/img-gen-assets/SKILL.md`.

- [ ] **Step 3: MCP surface**

Run: `go build ./cmd/img-gen-mcp`
Expected: PASS. The `request_asset_set` and `make_gif` schema strings mention palette_lock, frame_margins, bar_fill/fill_states, shape/variants, and composite.

- [ ] **Step 4: Commit (only if anything changed)**

If the sweep required fixes:

```bash
git add -A README.md skills/ cmd/
git commit -m "docs: asset expansion final sweep

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

## Spec coverage map (for the reviewer)

- F1 palette lock: Tasks 1–2 (remap math; queue field + output-side hook + palette file + docs).
- F2 nine-patch frame: Tasks 3–4 (punch geometry + deliverable; kind wiring + docs).
- F3 bar: Tasks 5–6 (clip math + compose; kind wiring + state validation + docs).
- F4 npc: Tasks 7–8 (mirror + walk4 compose; pinned-seed children + kind wiring + honesty contract + docs). `party` = documented pattern only (Task 2 docs), per spec §6/§9.
- F5 composite GIF: Tasks 9–10 (blit math; source type, set-file refs, static persistence, pre-quantization hook + docs).
- Non-goals untouched: `keyframes` message unchanged except its case order; no Phase 2/3 work anywhere.