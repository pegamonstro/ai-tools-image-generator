package compose

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// RegionMask builds a same-size fill mask PNG (base64): white inside rects
// (repaint), black elsewhere (keep). sd.cpp's masked img2img clamps every
// denoise step to the init latent where the mask is black, so the kept region
// converges back to the base figure.
func RegionMask(w, h int, rects ...image.Rectangle) (string, error) {
	if w <= 0 || h <= 0 {
		return "", fmt.Errorf("region mask: invalid size %dx%d", w, h)
	}
	if len(rects) == 0 {
		return "", fmt.Errorf("region mask: at least one rect is required")
	}
	mask := image.NewGray(image.Rect(0, 0, w, h))
	for _, r := range rects {
		clipped := r.Intersect(image.Rect(0, 0, w, h))
		if clipped.Empty() {
			return "", fmt.Errorf("region mask: rect %v falls outside the %dx%d canvas", [4]int{r.Min.X, r.Min.Y, r.Dx(), r.Dy()}, w, h)
		}
		drawRect(mask, clipped)
	}
	return encodeGrayPNG(mask)
}

// RegionMaskExcluding builds the same fill mask, then carves out the excluded
// image's opaque silhouette dilated by radius px — the rembg-assisted variant
// that keeps a region overlapping the base figure from cutting through it.
// If the carve leaves the rects fully covered, an error names the overlap so
// the caller can shrink the region.
func RegionMaskExcluding(w, h, radius int, exclude image.Image, rects ...image.Rectangle) (string, error) {
	g := dilatedAlphaLookup(exclude, radius, w, h)
	if g == nil {
		return "", fmt.Errorf("region mask: the excluded image does not fit the %dx%d canvas", w, h)
	}
	return RegionMaskExcludingLookups(g, w, h, rects...)
}

// RegionMaskExcludingLookups carves the dilated silhouette lookup `occupied`
// out of the rects' mask. Exposed for tests; RegionMaskExcluding is the
// caller-facing form.
func RegionMaskExcludingLookups(occupied []bool, w, h int, rects ...image.Rectangle) (string, error) {
	base, err := RegionMask(w, h, rects...)
	if err != nil {
		return "", err
	}
	raw, _ := base64.StdEncoding.DecodeString(base)
	mask, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("region mask: internal decode: %v", err)
	}
	g := mask.(*image.Gray)
	used := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if g.GrayAt(x, y).Y < 128 {
				continue
			}
			if occupied[y*w+x] {
				g.SetGray(x, y, color.Gray{0})
				continue
			}
			used++
		}
	}
	if used == 0 {
		return "", fmt.Errorf("region mask: the region leaves no repaintable space beside the subject's silhouette — shrink or move the rect")
	}
	return encodeGrayPNG(g)
}

// dilatedAlphaLookup rasterizes the image's opaque silhouette dilated by a
// circular radius into a w*h bool grid. nil when the image can't be mapped
// onto the canvas (degenerate bounds).
func dilatedAlphaLookup(exclude image.Image, radius, w, h int) []bool {
	if w <= 0 || h <= 0 {
		return nil
	}
	if radius < 0 {
		radius = 0
	}
	b := image.Rect(0, 0, w, h).Intersect(exclude.Bounds())
	if b.Empty() {
		return nil
	}
	dil := make([]bool, w*h)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var av uint32
			_, _, _, a4 := exclude.At(x, y).RGBA()
			av = a4
			if av>>8 < 64 {
				continue
			}
			for dy := -radius; dy <= radius; dy++ {
				for dx := -radius; dx <= radius; dx++ {
					if dx*dx+dy*dy > radius*radius {
						continue
					}
					xx, yy := x+dx, y+dy
					if xx >= 0 && xx < w && yy >= 0 && yy < h {
						dil[yy*w+xx] = true
					}
				}
			}
		}
	}
	return dil
}

func drawRect(g *image.Gray, r image.Rectangle) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			g.SetGray(x, y, color.Gray{255})
		}
	}
}

func encodeGrayPNG(g *image.Gray) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, g); err != nil {
		return "", fmt.Errorf("region mask: encode: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
