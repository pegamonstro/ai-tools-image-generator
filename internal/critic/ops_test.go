package critic

import (
	"image"
	"image/color"
	"math"
	"testing"

	"img-gen/internal/lattice"
)

// TestPromptTags tests the danbooru tokenization: comma-split, trim,
// lowercase, spaces→underscores, empty tokens dropped.
func TestPromptTags(t *testing.T) {
	cases := []struct {
		prompt string
		want   []string
	}{
		{"1girl, red gloves, long hair", []string{"1girl", "red_gloves", "long_hair"}},
		{"  1girl  ,  red gloves ,  long hair  ", []string{"1girl", "red_gloves", "long_hair"}},
		{"cat, dog, bird", []string{"cat", "dog", "bird"}},
		{"multi word tag, another one", []string{"multi_word_tag", "another_one"}},
		{"", []string{}},
		{"   ", []string{}},
		{"tag1,,tag2, ,tag3", []string{"tag1", "tag2", "tag3"}},
		{"UPPERCASE, MiXeD", []string{"uppercase", "mixed"}},
		{"tag with   multiple   spaces", []string{"tag_with_multiple_spaces"}},
	}
	for _, tc := range cases {
		got := PromptTags(tc.prompt)
		if len(got) != len(tc.want) {
			t.Fatalf("PromptTags(%q) = %v, want %v", tc.prompt, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("PromptTags(%q)[%d] = %q, want %q", tc.prompt, i, got[i], tc.want[i])
			}
		}
	}
}

// TestTagDrift tests the tag drift detection.
func TestTagDrift(t *testing.T) {
	vocab := map[string]bool{"1girl": true, "red_gloves": true, "long_hair": true}
	tags := []lattice.TagScore{
		{Name: "1girl", Score: 0.9},
		{Name: "red_gloves", Score: 0.2}, // below floor 0.35
		{Name: "long_hair", Score: 0.8},
	}

	// missing "red_gloves" because its score is below floor
	missing, known := TagDrift("1girl, red gloves, long hair", vocab, tags, TagFloor)
	if !known {
		t.Fatal("known should be true when vocab is provided")
	}
	if len(missing) != 1 || missing[0] != "red_gloves" {
		t.Fatalf("missing = %v, want [red_gloves]", missing)
	}

	// All tags meet floor
	tags2 := []lattice.TagScore{
		{Name: "1girl", Score: 0.9},
		{Name: "red_gloves", Score: 0.5},
		{Name: "long_hair", Score: 0.8},
	}
	missing, known = TagDrift("1girl, red gloves, long hair", vocab, tags2, TagFloor)
	if !known {
		t.Fatal("known should be true")
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want []", missing)
	}

	// vocab is nil -> known=false (upstream op error, not drift)
	missing, known = TagDrift("1girl, gloves", nil, tags, TagFloor)
	if known {
		t.Fatal("known should be false when vocab is nil")
	}
	if len(missing) != 0 {
		t.Fatalf("missing should be empty when vocab is nil, got %v", missing)
	}

	// empty vocab -> known=true but no required tags (vocab empty), so missing empty
	missing, known = TagDrift("1girl, red gloves", map[string]bool{}, tags, TagFloor)
	if !known {
		t.Fatal("known should be true with empty vocab")
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want [] (no required tags when vocab empty)", missing)
	}

	// Deterministic order follows prompt order
	missing, known = TagDrift("red gloves, 1girl, long_hair", vocab, tags, TagFloor)
	if !known {
		t.Fatal("known should be true")
	}
	if len(missing) != 1 || missing[0] != "red_gloves" {
		t.Fatalf("missing order = %v, want [red_gloves] (prompt order)", missing)
	}
}

// TestPaletteContain tests palette containment checks.
func TestPaletteContain(t *testing.T) {
	// Target palette: red, green, blue
	targetHexes := []string{"#ff0000", "#00ff00", "#0000ff"}

	// (a) pixelFixed=true: all pixels exactly from target palette
	img1 := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	img1.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255}) // red
	img1.SetNRGBA(1, 0, color.NRGBA{0, 255, 0, 255}) // green
	img1.SetNRGBA(2, 0, color.NRGBA{0, 0, 255, 255}) // blue
	ok, detail := PaletteContain(img1, targetHexes, true)
	if !ok {
		t.Fatalf("pixelFixed=true exact palette: ok=false, detail=%q", detail)
	}
	if detail != "pixel_fix: exact containment" {
		t.Fatalf("detail = %q, want %q", detail, "pixel_fix: exact containment")
	}

	// One off-palette pixel
	img1b := image.NewNRGBA(image.Rect(0, 0, 4, 1))
	img1b.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	img1b.SetNRGBA(1, 0, color.NRGBA{0, 255, 0, 255})
	img1b.SetNRGBA(2, 0, color.NRGBA{0, 0, 255, 255})
	img1b.SetNRGBA(3, 0, color.NRGBA{128, 128, 128, 255}) // gray - not in palette
	ok, detail = PaletteContain(img1b, targetHexes, true)
	if ok {
		t.Fatalf("pixelFixed=true with off-palette pixel: ok=true, want false")
	}

	// (b) pixelFixed=false: ≥98% within ΔE≤8
	// Create a 100x100 green field
	img2 := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img2.SetNRGBA(x, y, color.NRGBA{0, 255, 0, 255})
		}
	}
	ok, detail = PaletteContain(img2, targetHexes, false)
	if !ok {
		t.Fatalf("green field: ok=false, detail=%q", detail)
	}
	// Should contain coverage fraction in detail
	expectedPrefix := "coverage "
	if len(detail) < len(expectedPrefix) || detail[:len(expectedPrefix)] != expectedPrefix {
		t.Fatalf("detail = %q, want prefix %q", detail, expectedPrefix)
	}

	// 5% yellow patch on a green field -> yellow not in palette (red, green, blue), huge ΔE -> fail
	img3 := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if x < 5 && y < 100 { // 5% yellow patch (5x100 = 500 pixels = 5%)
				img3.SetNRGBA(x, y, color.NRGBA{255, 255, 0, 255})
			} else {
				img3.SetNRGBA(x, y, color.NRGBA{0, 255, 0, 255})
			}
		}
	}
	ok, detail = PaletteContain(img3, targetHexes, false)
	if ok {
		t.Fatalf("5%% yellow patch on green: ok=true, want false")
	}
	// Detail should name the coverage fraction short of threshold
	if len(detail) < len("coverage ") || detail[:len("coverage ")] != "coverage " {
		t.Fatalf("detail = %q, want coverage fraction", detail)
	}

	// Target parse failures -> ok=false, detail names the bad hex
	// (caller-side validation gates this, but runtime path is defensive)
	ok, detail = PaletteContain(img2, []string{"#zzzzzz"}, false)
	if ok {
		t.Fatalf("bad hex: ok=true, want false")
	}
}

// TestTileMetric tests the tile seam metric.
func TestTileMetric(t *testing.T) {
	// Seamless checkerboard: wrapped edges continue pattern perfectly
	// Using 6x6 with 2x2 checkerboard cells (3x3 cells, odd count -> wraps seamlessly)
	img1 := image.NewNRGBA(image.Rect(0, 0, 6, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 6; x++ {
			c := uint8(0)
			if (x/2+y/2)%2 == 0 {
				c = 255
			}
			img1.SetNRGBA(x, y, color.NRGBA{c, c, c, 255})
		}
	}
	ratio, ok := TileMetric(img1)
	// For seamless pattern, edgeErr should be 0 (or very small), ratio ≤ 1.0
	if ratio > 1.0 {
		t.Fatalf("seamless checkerboard: ratio = %v, want ≤ 1.0", ratio)
	}
	if !ok {
		t.Fatalf("seamless checkerboard: ok=false, want true (ratio ≤ TileMaxRatio)")
	}

	// Synthetic image with deliberate mismatched edges: each edge a different color
	// 10x10 image, interior 8x8 is uniform gray, each edge is a distinct color.
	// When wrapped, left (red) meets right (blue), top (green) meets bottom (yellow) -> large edgeErr.
	// Interior is uniform -> interiorErr = 0 -> ratio = edgeErr / 1e-6 >> 1.
	img2 := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	interiorColor := color.NRGBA{100, 100, 100, 255}
	leftColor := color.NRGBA{255, 0, 0, 255}
	rightColor := color.NRGBA{0, 0, 255, 255}
	topColor := color.NRGBA{0, 255, 0, 255}
	bottomColor := color.NRGBA{255, 255, 0, 255}
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			switch {
			case x == 0:
				img2.SetNRGBA(x, y, leftColor)
			case x == 9:
				img2.SetNRGBA(x, y, rightColor)
			case y == 0:
				img2.SetNRGBA(x, y, topColor)
			case y == 9:
				img2.SetNRGBA(x, y, bottomColor)
			default:
				img2.SetNRGBA(x, y, interiorColor)
			}
		}
	}
	ratio, ok = TileMetric(img2)
	// Wrap-edge error should be large (left vs right, top vs bottom are different colors)
	// Interior variation should be 0 (uniform interior)
	// ratio = edgeErr / max(interiorErr, 1e-6) -> should be >> 1
	if ratio <= 2.0 {
		t.Fatalf("mismatched edges: ratio = %v, want > 2.0", ratio)
	}
	if ok {
		t.Fatalf("mismatched edges: ok=true, want false (ratio > TileMaxRatio)")
	}

	// Flat image: all same color -> interiorErr = 0, edgeErr = 0 -> ratio = 0 (bail-out)
	img3 := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			img3.SetNRGBA(x, y, color.NRGBA{128, 128, 128, 255})
		}
	}
	ratio, ok = TileMetric(img3)
	if ratio != 0 {
		t.Fatalf("flat image: ratio = %v, want 0 (flatness bail-out)", ratio)
	}
	if !ok {
		t.Fatalf("flat image: ok=false, want true (ratio=0 ≤ TileMaxRatio)")
	}

	// Non-square image: 10x6 with 2x2 cells (5x3 cells, both odd -> wraps seamlessly)
	img4 := image.NewNRGBA(image.Rect(0, 0, 10, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 10; x++ {
			c := uint8(0)
			if (x/2+y/2)%2 == 0 {
				c = 255
			}
			img4.SetNRGBA(x, y, color.NRGBA{c, c, c, 255})
		}
	}
	ratio, ok = TileMetric(img4)
	if ratio > 1.0 {
		t.Fatalf("non-square seamless: ratio = %v, want ≤ 1.0", ratio)
	}
	if !ok {
		t.Fatalf("non-square seamless: ok=false, want true")
	}

	// Test with math.Abs for float comparison
	_ = math.Abs
}
