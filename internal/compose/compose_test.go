package compose

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"
)

func solid(w, h int, c color.RGBA) *image.NRGBA {
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

func TestEncodeGIFDeterministicPalette(t *testing.T) {
	// Colors that tie on red channel (100) but differ in green and blue.
	c1 := color.RGBA{100, 0, 0, 255}
	c2 := color.RGBA{100, 255, 0, 255}
	c3 := color.RGBA{100, 0, 255, 255}
	frames := []image.Image{
		solid(16, 16, c1),
		solid(16, 16, c2),
		solid(16, 16, c3),
	}
	out1, err := EncodeGIF(frames, 100, 0)
	if err != nil {
		t.Fatalf("first encode: %v", err)
	}
	out2, err := EncodeGIF(frames, 100, 0)
	if err != nil {
		t.Fatalf("second encode: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("palette not deterministic: outputs differ")
	}
	// Additionally, verify that we can decode and get three frames.
	g, err := gif.DecodeAll(bytes.NewReader(out1))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Image) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(g.Image))
	}
}

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
	// blue 4x4 is centered in the 8-wide cell: gutter at (1,5) is transparent,
	// far edge (5,5) is blue — distinguishes centering from left-alignment
	if _, _, _, a := img.At(1, 5).RGBA(); a != 0 {
		t.Fatalf("pixel (1,5): want transparent gutter")
	}
	if dr, _, _, _ := img.At(5, 5).RGBA(); dr != 40*257 {
		t.Fatalf("blue frame not centered: (5,5) red channel %d, want %d", dr, 40*257)
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
			// observed = p + (1-alpha)*bg — the (1-alpha) weight uses the ALPHA (fg.A=127), not the premultiplied value
			obs := func(p int, bg uint8) uint8 { return uint8(p + (255-int(fg.A))*int(bg)/255) }
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
