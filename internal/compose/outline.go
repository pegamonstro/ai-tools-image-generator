package compose

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
)

// OutlineStrokes paints stroke onto every fully-transparent pixel that is a
// 4-neighbor of an opaque pixel, repeating width rounds of growth (rounds
// see the previous round's strokes, so width 2 reaches diagonal corners).
// Round-snapshot dilation: all fill candidates come from the round's start
// snapshot, then paint. Stroke alpha is forced opaque.
func OutlineStrokes(src *image.NRGBA, width int, stroke color.NRGBA) *image.NRGBA {
	stroke.A = 255
	out := cloneNRGBA(src)
	b := out.Bounds()
	touches := func(x, y int) bool {
		return (x > b.Min.X && alphaAt(out, x-1, y) >= 128) ||
			(x < b.Max.X-1 && alphaAt(out, x+1, y) >= 128) ||
			(y > b.Min.Y && alphaAt(out, x, y-1) >= 128) ||
			(y < b.Max.Y-1 && alphaAt(out, x, y+1) >= 128)
	}
	for round := 0; round < width; round++ {
		var fills []image.Point
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if alphaAt(out, x, y) < 128 && touches(x, y) {
					fills = append(fills, image.Point{x, y})
				}
			}
		}
		for _, p := range fills {
			out.Set(p.X, p.Y, stroke)
		}
		if len(fills) == 0 {
			break
		}
	}
	return out
}

func alphaAt(img *image.NRGBA, x, y int) uint8 {
	return img.NRGBAAt(x, y).A
}

func cloneNRGBA(src *image.NRGBA) *image.NRGBA {
	dst := *src
	dst.Pix = append([]uint8(nil), src.Pix...)
	return &dst
}

// ParseHexColor parses #rgb / #rrggbb into a fully opaque color.
func ParseHexColor(spec string) (color.NRGBA, error) {
	if !strings.HasPrefix(spec, "#") {
		return color.NRGBA{}, fmt.Errorf("compose: outline: bad color %q: want #rgb or #rrggbb", spec)
	}
	h := strings.TrimPrefix(spec, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return color.NRGBA{}, fmt.Errorf("compose: outline: bad color %q: want #rgb or #rrggbb", spec)
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return color.NRGBA{}, fmt.Errorf("compose: outline: bad color %q: want #rgb or #rrggbb", spec)
	}
	v := uint32(raw[0])<<16 | uint32(raw[1])<<8 | uint32(raw[2])
	return color.NRGBA{
		R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255,
	}, nil
}

// ParseOutlineColor resolves an outline color spec: "" or "auto" pick the
// darkest (luma-weighted) palette color; anything else is a hex spec.
func ParseOutlineColor(spec string, pal color.Palette) (color.NRGBA, error) {
	if spec == "" || spec == "auto" {
		best := color.NRGBA{}
		found := false
		bestLuma := float64(0)
		for _, c := range pal {
			r := nrgbaAt(c)
			if r.A == 0 {
				continue
			}
			luma := 0.2126*float64(r.R) + 0.7152*float64(r.G) + 0.0722*float64(r.B)
			if !found || luma < bestLuma {
				best, found, bestLuma = r, true, luma
			}
		}
		if !found {
			return color.NRGBA{}, errors.New("compose: outline: auto color needs a non-empty palette")
		}
		return best, nil
	}
	return ParseHexColor(spec)
}

// OutlinePNG quantizes the PNG onto its top-64 palette, resolves the stroke
// color, then strokes transparent pixels adjacent to opaque ones.
func OutlinePNG(data []byte, width int, colorSpec string) ([]byte, error) {
	if width < 1 || width > 2 {
		return nil, fmt.Errorf("compose: outline: width %d out of range 1..2", width)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("compose: outline: not a PNG: %v", err)
	}
	pal := SetPalette([]image.Image{img}, 64)
	stroke, err := ParseOutlineColor(colorSpec, pal)
	if err != nil {
		return nil, err
	}
	stroked := OutlineStrokes(RemapImage(img, pal), width, stroke)
	var out bytes.Buffer
	if err := png.Encode(&out, stroked); err != nil {
		return nil, fmt.Errorf("compose: outline: encode: %v", err)
	}
	return out.Bytes(), nil
}