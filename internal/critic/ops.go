package critic

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"

	"img-gen/internal/compose"
	"img-gen/internal/lattice"
	"img-gen/internal/storage"
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
//     known=false (upstream op error: vocab fetch failed) and missing=nil
//     — this is not drift. An available-but-empty vocab means nothing is
//     required (vacuous pass); the fail-open guard is only for fetch failure.
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
				// Exact match required (with 1e-10 tolerance per component).
				// 1e-10 is far below any perceivable/quantized difference;
				// it only tolerates cross-pipeline float noise.
				const labEps = 1e-10
				found := false
				for _, tl := range targetLABs {
					if math.Abs(l-tl[0]) <= labEps && math.Abs(a-tl[1]) <= labEps && math.Abs(bb-tl[2]) <= labEps {
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

// formatCoverage formats a coverage fraction to exactly 3 decimal places.
func formatCoverage(cov float64) string {
	return fmt.Sprintf("%.3f", cov)
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

// Run executes the checklist in order against the given image.
// All vlm_check criteria are batched into a single Judge call.
// Returns a slice of CriticCheck results in the same order as the checklist.
func Run(ctx context.Context, cl Checklist, imageB64 string, e Env) []storage.CriticCheck {
	if e.Judge == nil {
		panic("critic: Env.Judge is nil")
	}
	if e.Tagger == nil {
		panic("critic: Env.Tagger is nil")
	}
	if e.Vocab == nil {
		panic("critic: Env.Vocab is nil")
	}

	var results []storage.CriticCheck

	// First pass: collect vlm_check criteria and their indices
	vlmCriteria := []string{}
	vlmIndices := []int{}
	for i, check := range cl.Checks {
		if check.Op == "vlm_check" {
			vlmCriteria = append(vlmCriteria, check.Spec)
			vlmIndices = append(vlmIndices, i)
		}
	}

	// Batch all vlm_check into one Judge call
	vlmScores := make([]VLMScore, len(vlmCriteria))
	vlmErr := error(nil)
	if len(vlmCriteria) > 0 {
		vlmScores, vlmErr = e.Judge(ctx, vlmCriteria, imageB64)
	}

	// Second pass: process all checks in order
	for i, check := range cl.Checks {
		var result storage.CriticCheck
		result.Op = check.Op
		result.Spec = check.Spec

		switch check.Op {
		case "vlm_check":
			// Find the index in vlmCriteria
			idx := -1
			for j, vi := range vlmIndices {
				if vi == i {
					idx = j
					break
				}
			}
			if vlmErr != nil {
				result.Status = "undetermined"
				result.Reason = strings.TrimSpace(vlmErr.Error())
			} else if idx >= 0 && idx < len(vlmScores) {
				score := vlmScores[idx]
				if score.Pass {
					result.Status = "pass"
					result.Reason = score.Reason
				} else {
					result.Status = "fail"
					result.Reason = score.Reason
					result.Suggestion = score.Fix
				}
			} else {
				// Judge omitted this criterion
				result.Status = "undetermined"
				result.Reason = "judge omitted criterion"
			}

		case "tag_roundtrip":
			// Fetch vocab first
			vocabList, vocabErr := e.Vocab(ctx)
			if vocabErr != nil {
				result.Status = "error"
				result.Reason = strings.TrimSpace(vocabErr.Error())
				results = append(results, result)
				continue
			}
			vocab := make(map[string]bool, len(vocabList))
			for _, v := range vocabList {
				vocab[v] = true
			}

			// Compute required set: prompt tokens that are in vocab
			requiredTags := PromptTags(cl.Prompt)
			requiredCount := 0
			for _, t := range requiredTags {
				if vocab[t] {
					requiredCount++
				}
			}

			// Call tagger
			tags, taggerErr := e.Tagger(ctx, imageB64)
			if taggerErr != nil {
				result.Status = "error"
				result.Reason = strings.TrimSpace(taggerErr.Error())
				results = append(results, result)
				continue
			}

			missing, _ := TagDrift(cl.Prompt, vocab, tags, TagFloor)
			// known is always true here since vocab fetch succeeded
			if len(missing) == 0 {
				if requiredCount == 0 {
					result.Status = "pass"
					result.Reason = "no known tags to verify"
				} else {
					result.Status = "pass"
					result.Reason = "no known tags missing"
				}
			} else {
				result.Status = "fail"
				result.Reason = "danbooru drift"
				result.Suggestion = "strengthen: " + strings.Join(missing, ", ")
			}

		case "palette_conform":
			if len(cl.Target) == 0 {
				result.Status = "error"
				result.Reason = "no target palette"
				results = append(results, result)
				continue
			}

			// Decode image
			img, err := decodeBase64Image(imageB64)
			if err != nil {
				result.Status = "error"
				result.Reason = err.Error()
				results = append(results, result)
				continue
			}

			ok, detail := PaletteContain(img, cl.Target, false)
			if ok {
				result.Status = "pass"
				result.Reason = detail
			} else {
				result.Status = "fail"
				result.Reason = detail
				result.Suggestion = "palette drift"
			}

		case "tile_conform":
			// Decode image
			img, err := decodeBase64Image(imageB64)
			if err != nil {
				result.Status = "error"
				result.Reason = err.Error()
				results = append(results, result)
				continue
			}

			ratio, ok := TileMetric(img)
			maxRatio := check.MaxRatio
			if maxRatio == 0 {
				maxRatio = TileMaxRatio
			}
			if ok {
				result.Status = "pass"
				result.Reason = fmt.Sprintf("wrap-edge ratio %.2f", ratio)
			} else {
				result.Status = "fail"
				result.Reason = fmt.Sprintf("wrap-edge ratio %.2f > %.2f", ratio, maxRatio)
				result.Suggestion = "seam present at tile boundary"
			}

		default:
			result.Status = "error"
			result.Reason = "unknown check op: " + check.Op
		}

		results = append(results, result)
	}

	return results
}

// decodeBase64Image decodes a base64-encoded PNG image.
func decodeBase64Image(b64 string) (image.Image, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return img, nil
}

// Aggregate returns the overall verdict from a slice of CriticCheck results.
// "failed" if any check failed, "indeterminate" if no pass/fail (all inert),
// otherwise "pass".
func Aggregate(rs []storage.CriticCheck) string {
	hasPass := false
	hasFail := false
	for _, r := range rs {
		switch r.Status {
		case "pass":
			hasPass = true
		case "fail":
			hasFail = true
		}
	}
	if hasFail {
		return "failed"
	}
	if !hasPass && !hasFail {
		return "indeterminate"
	}
	return "pass"
}

// Score returns pass/(pass+fail) as a float64, 0 when both are zero.
func Score(rs []storage.CriticCheck) float64 {
	pass := 0
	fail := 0
	for _, r := range rs {
		switch r.Status {
		case "pass":
			pass++
		case "fail":
			fail++
		}
	}
	if pass+fail == 0 {
		return 0
	}
	return float64(pass) / float64(pass+fail)
}

// Fails returns the count of checks with status "fail".
func Fails(rs []storage.CriticCheck) int {
	count := 0
	for _, r := range rs {
		if r.Status == "fail" {
			count++
		}
	}
	return count
}

// Relevant returns the count of checks with status "pass" or "fail".
func Relevant(rs []storage.CriticCheck) int {
	count := 0
	for _, r := range rs {
		if r.Status == "pass" || r.Status == "fail" {
			count++
		}
	}
	return count
}

// MutatedPrompt collects suggestions from FAILING checks only, in order,
// deduplicates case-insensitively, skips suggestions already substrings of base,
// joins with ", ", appends to base, and caps at MutationMaxChars (truncating
// at the last comma boundary that fits). Returns base unchanged when nothing
// survives.
func MutatedPrompt(base string, rs []storage.CriticCheck) string {
	var suggestions []string
	seen := make(map[string]bool)

	for _, r := range rs {
		if r.Status != "fail" {
			continue
		}
		sug := strings.TrimSpace(r.Suggestion)
		if sug == "" {
			continue
		}
		lower := strings.ToLower(sug)
		if seen[lower] {
			continue
		}
		// Skip if already a substring of base (case-insensitive)
		if strings.Contains(strings.ToLower(base), lower) {
			continue
		}
		seen[lower] = true
		suggestions = append(suggestions, sug)
	}

	if len(suggestions) == 0 {
		return base
	}

	joined := strings.Join(suggestions, ", ")
	var result string
	if base == "" {
		result = joined
	} else {
		result = base + ", " + joined
	}

	// Cap at MutationMaxChars, truncating at last comma boundary
	if len(result) <= MutationMaxChars {
		return result
	}

	// Find the last comma boundary that fits
	truncated := result[:MutationMaxChars]
	lastComma := strings.LastIndex(truncated, ",")
	if lastComma < 0 {
		// No comma boundary fits; return base unchanged
		return base
	}
	// Truncate at the comma (include the comma)
	return result[:lastComma+1]
}
