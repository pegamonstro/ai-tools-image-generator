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