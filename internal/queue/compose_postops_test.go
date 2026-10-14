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
	"img-gen/internal/storage"
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
			// cap.add (compose_test.go) dereferences SamplingParams.Seed —
			// *int64 — into the capture's int64 record, the same every real
			// capture fake uses.
			cap.add(prompt, size, sp.Seed, buf.Bytes())
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

// noiseGen renders a deterministic RGB-noise image at the requested size, no
// grid anywhere — DetectGridCell must refuse it.
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
