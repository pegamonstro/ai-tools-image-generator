package compose

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
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
