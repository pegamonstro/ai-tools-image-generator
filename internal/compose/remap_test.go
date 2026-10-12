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
		{50.0000, -1.3802, -84.2814, 50.0000, 0.0000, -82.7485, 1.0000},
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

// TestSetPaletteDropsReservedTransparent pins R-16: the reserved alpha-0
// index-0 entry the GIF encoder needs must never reach the set-palette remap
// path. Nearest-CIEDE2000 ties on opaque black resolve to it (it is pure
// black) and RemapImage copies its A=0 into the output, silently erasing
// pixels — and the marshaled file would exceed the 255-entry claim with a
// duplicate #000000.
func TestSetPaletteDropsReservedTransparent(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
		}
	}
	pal := SetPalette([]image.Image{src}, 255)
	got := RemapImage(src, pal).NRGBAAt(8, 8)
	if got.R != 0 || got.G != 0 || got.B != 0 || got.A != 255 {
		t.Fatalf("center pixel = %+v, want opaque black R0 G0 B0 A255", got)
	}
	hexes := PaletteHexes(pal)
	if len(hexes) > 255 {
		t.Fatalf("palette has %d colors, want at most 255", len(hexes))
	}
	for i, c := range pal {
		if _, _, _, a := c.RGBA(); a == 0 {
			t.Fatalf("palette entry %d is fully transparent: reserved index-0 entry leaked into the set palette", i)
		}
	}
	// An all-black render collapses to exactly one opaque entry — no
	// duplicate #000000 next to a transparent one.
	if len(hexes) != 1 || hexes[0] != "#000000" {
		t.Fatalf("palette hexes = %v, want a single opaque #000000", hexes)
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
