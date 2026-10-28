package critic

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"

	"img-gen/internal/compose"
	"img-gen/internal/lattice"
)

// PromptTags tokenizes a danbooru-style prompt: comma-split, trim spaces,
// lowercase, interior spaces to underscores. Empty tokens are dropped.
// The order follows the prompt order.
func PromptTags(prompt string) []string {
	if prompt == "" {
		return nil
	}
	parts := strings.Split(prompt, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		t = strings.ToLower(t)
		t = strings.ReplaceAll(t, " ", "_")
		// collapse multiple underscores (from multiple spaces)
		for strings.Contains(t, "__") {
			t = strings.ReplaceAll(t, "__", "_")
		}
		out = append(out, t)
	}
	return out
}

// TagDrift detects which required prompt tags are missing or below the
// score floor in the tagger output.
//   - vocab: the WD14 vocabulary (tag name -> true). If nil, returns
//     known=false (upstream op error) and missing=nil — this is not drift.
//   - tags: the TagScore slice from the tagger sidecar.
//   - floor: minimum score for a tag to count as present (e.g., TagFloor 0.35).
//
// Returns missing tags in prompt order, and known=false only when vocab is nil.
func TagDrift(prompt string, vocab map[string]bool, tags []lattice.TagScore, floor float64) (missing []string, known bool) {
	if vocab == nil {
		return nil, false
	}
	required := PromptTags(prompt)
	if len(required) == 0 {
		return nil, true
	}
	// Build a set of tags that meet the floor
	present := make(map[string]bool, len(tags))
	for _, ts := range tags {
		if ts.Score >= floor {
			present[ts.Name] = true
		}
	}
	// Check each required tag in prompt order
	for _, req := range required {
		if vocab[req] && !present[req] {
			missing = append(missing, req)
		}
	}
	return missing, true
}

// PaletteContain checks whether an image's opaque pixels are contained within
// a target palette.
//   - targetHexes: palette colors as #rrggbb strings (caller validates; this
//     path is defensive and returns ok=false with detail on parse failure).
//   - pixelFixed: when true, requires EVERY opaque pixel to exactly match a
//     target color (exact containment). On pass, detail is
//     "pixel_fix: exact containment".
//   - when false, requires ≥ PaletteCoverage (0.98) of opaque pixels to be
//     within PaletteDeltaE (≤8.0) of the nearest target color via CIEDE2000.
//     On pass, detail includes the coverage fraction (e.g., "coverage 0.995").
//     On fail, detail names the fraction short of the threshold.
func PaletteContain(img image.Image, targetHexes []string, pixelFixed bool) (ok bool, detail string) {
	// Parse target palette to LAB
	targetLABs := make([][3]float64, 0, len(targetHexes))
	for _, h := range targetHexes {
		c, err := compose.ParseHexColor(h)
		if err != nil {
			return false, "palette hex parse failed: " + h
		}
		l, a, bb := compose.LAB(float64(c.R), float64(c.G), float64(c.B))
		targetLABs = append(targetLABs, [3]float64{l, a, bb})
	}
	if len(targetLABs) == 0 {
		return false, "palette: empty target"
	}

	b := img.Bounds()
	var totalOpaque, within int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			n := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if n.A < 128 {
				continue // transparent pixel, not counted
			}
			totalOpaque++
			l, a, bb := compose.LAB(float64(n.R), float64(n.G), float64(n.B))
			if pixelFixed {
				// Exact match required
				found := false
				for _, tl := range targetLABs {
					if l == tl[0] && a == tl[1] && bb == tl[2] {
						found = true
						break
					}
				}
				if !found {
					return false, "pixel_fix: off-palette pixel found"
				}
				within++
			} else {
				// Nearest target within ΔE ≤ PaletteDeltaE
				bestD := math.MaxFloat64
				for _, tl := range targetLABs {
					d := compose.CIEDE2000(l, a, bb, tl[0], tl[1], tl[2])
					if d < bestD {
						bestD = d
					}
				}
				if bestD <= PaletteDeltaE {
					within++
				}
			}
		}
	}
	if totalOpaque == 0 {
		// Vacuously true: no opaque pixels to check
		if pixelFixed {
			return true, "pixel_fix: exact containment"
		}
		return true, "coverage 1.000"
	}
	if pixelFixed {
		return true, "pixel_fix: exact containment"
	}
	coverage := float64(within) / float64(totalOpaque)
	if coverage >= PaletteCoverage {
		return true, "coverage " + formatCoverage(coverage)
	}
	return false, "coverage " + formatCoverage(coverage)
}

// formatCoverage formats a coverage fraction to 3 decimal places.
func formatCoverage(cov float64) string {
	s := fmt.Sprintf("%.3f", cov)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// TileMetric computes the normalized wrap-edge error for a tileable image.
//   - Converts image to NRGBA.
//   - edgeErr: mean per-channel |V(x,y) - V(wrapped counterpart)| across all
//     four wrapped boundary pairs (left↔right, top↔bottom).
//   - interiorErr: mean |V - V(neighbor)| sampled over interior rows/cols only
//     (skip outermost row and column on each side).
//   - ratio = edgeErr / max(interiorErr, 1e-6).
//   - ok = ratio ≤ TileMaxRatio (1.3) — bakes the default threshold; callers
//     can override by comparing ratio themselves.
//
// Flatness bail-out: if interiorErr == 0 and edgeErr == 0, ratio = 0.
func TileMetric(img image.Image) (ratio float64, ok bool) {
	nrgba := toNRGBA(img)
	b := nrgba.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 2 || h < 2 {
		// Trivially small: no interior, no edge difference possible
		return 0, true
	}

	// edgeErr: mean absolute per-channel difference across wrapped boundaries
	var edgeSum float64
	var edgeCount int

	// Left ↔ Right edges
	for y := 0; y < h; y++ {
		l := nrgba.NRGBAAt(0, y)
		r := nrgba.NRGBAAt(w-1, y)
		edgeSum += float64(absInt(int(l.R) - int(r.R)))
		edgeSum += float64(absInt(int(l.G) - int(r.G)))
		edgeSum += float64(absInt(int(l.B) - int(r.B)))
		edgeCount += 3
	}

	// Top ↔ Bottom edges
	for x := 0; x < w; x++ {
		t := nrgba.NRGBAAt(x, 0)
		btm := nrgba.NRGBAAt(x, h-1)
		edgeSum += float64(absInt(int(t.R) - int(btm.R)))
		edgeSum += float64(absInt(int(t.G) - int(btm.G)))
		edgeSum += float64(absInt(int(t.B) - int(btm.B)))
		edgeCount += 3
	}

	edgeErr := edgeSum / float64(edgeCount)

	// interiorErr: mean absolute per-channel difference to 4-neighbor
	// sampled over interior pixels only (skip outermost row/col)
	var interiorSum float64
	var interiorCount int
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			c := nrgba.NRGBAAt(x, y)
			// right neighbor
			rn := nrgba.NRGBAAt(x+1, y)
			interiorSum += float64(absInt(int(c.R) - int(rn.R)))
			interiorSum += float64(absInt(int(c.G) - int(rn.G)))
			interiorSum += float64(absInt(int(c.B) - int(rn.B)))
			interiorCount += 3
			// down neighbor
			dn := nrgba.NRGBAAt(x, y+1)
			interiorSum += float64(absInt(int(c.R) - int(dn.R)))
			interiorSum += float64(absInt(int(c.G) - int(dn.G)))
			interiorSum += float64(absInt(int(c.B) - int(dn.B)))
			interiorCount += 3
		}
	}

	var interiorErr float64
	if interiorCount > 0 {
		interiorErr = interiorSum / float64(interiorCount)
	}

	// Flatness bail-out: interiorErr == 0 and edgeErr == 0 -> ratio = 0
	if interiorErr == 0 && edgeErr == 0 {
		return 0, true
	}

	ratio = edgeErr / math.Max(interiorErr, 1e-6)
	ok = ratio <= TileMaxRatio
	return ratio, ok
}

// toNRGBA converts any image.Image to *image.NRGBA.
func toNRGBA(img image.Image) *image.NRGBA {
	if n, ok := img.(*image.NRGBA); ok {
		return n
	}
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.SetNRGBA(x-b.Min.X, y-b.Min.Y, color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA))
		}
	}
	return out
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
