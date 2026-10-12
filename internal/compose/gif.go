package compose

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
)

// EncodeGIF assembles frames into an animated GIF: one global median-cut
// palette shared by every frame (consistent colors across the animation),
// entry 0 reserved as the transparent index, and inter-frame optimization —
// any pixel whose color equals what the decoder already shows (the previous
// frame's effective view) is stored as the transparent index instead of being
// re-emitted. loop 0 means forever; GIF loop semantics are "extra repeats
// after the first pass".
func EncodeGIF(frames []image.Image, delayMS, loop int) ([]byte, error) {
	if len(frames) == 0 {
		return nil, errors.New("compose: no frames")
	}
	delay := delayMS / 10
	if delay < 2 {
		delay = 2
	}
	b0 := frames[0].Bounds()
	for i, f := range frames {
		if f.Bounds() != b0 {
			return nil, fmt.Errorf("compose: frame %d bounds %v do not match frame 0 %v", i, f.Bounds(), b0)
		}
	}
	pal := sharedPalette(frames, 255)
	type plane struct {
		img *image.Paletted
		eff []byte // effective view: the color a decoder shows per pixel
	}
	planes := make([]plane, len(frames))
	for i, f := range frames {
		p := image.NewPaletted(b0, pal)
		draw.Draw(p, b0, f, b0.Min, draw.Src) // draw.Src quantizes to the dst palette
		if i > 0 {
			prev := planes[i-1].eff
			for n := range p.Pix {
				if p.Pix[n] == prev[n] {
					p.Pix[n] = 0 // unchanged: let the previous frame show through
				}
			}
		}
		eff := make([]byte, len(p.Pix))
		copy(eff, p.Pix)
		if i > 0 {
			prev := planes[i-1].eff
			for n := range eff {
				if eff[n] == 0 {
					eff[n] = prev[n]
				}
			}
		}
		planes[i] = plane{img: p, eff: eff}
	}
	g := &gif.GIF{
		Image:     make([]*image.Paletted, len(planes)),
		Delay:     make([]int, len(planes)),
		LoopCount: loop,
		Config: image.Config{
			ColorModel: color.Palette(pal),
			Width:      b0.Dx(),
			Height:     b0.Dy(),
		},
	}
	for i, p := range planes {
		g.Image[i] = p.img
		g.Delay[i] = delay
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sharedPalette builds a fixed-stride histogram across every frame and median
// cuts it into at most n colors (plus the reserved transparent entry 0 that
// callers prepend elsewhere — this function returns opaque colors only, and
// EncodeGIF places them at index >= 1).
func sharedPalette(frames []image.Image, n int) color.Palette {
	type px struct{ r, g, b uint8 }
	type entry struct {
		p     px
		count int
	}
	// sortEntries sorts entries in-place by the given key function.
	sortEntries := func(b []entry, key func(entry) int) {
		for i := 1; i < len(b); i++ {
			for j := i; j > 0 && key(b[j]) < key(b[j-1]); j-- {
				b[j], b[j-1] = b[j-1], b[j]
			}
		}
	}
	counts := map[px]int{}
	for _, f := range frames {
		b := f.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y += 2 {
			for x := b.Min.X; x < b.Max.X; x += 2 {
				r, g, bl, a := f.At(x, y).RGBA()
				if a < 128<<8 {
					continue
				}
				counts[px{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)}]++
			}
		}
	}
	var all []entry
	for k, v := range counts {
		all = append(all, entry{p: k, count: v})
	}
	if len(all) == 0 {
		all = append(all, entry{p: px{255, 255, 255}, count: 1})
	}
	// Sort entries deterministically by (r,g,b) before median-cut.
	sortEntries(all, func(e entry) int {
		// Lexicographic order over (r,g,b) as a single integer.
		// Since each component is uint8, we can compute r<<16 | g<<8 | b.
		return int(e.p.r)<<16 | int(e.p.g)<<8 | int(e.p.b)
	})
	boxes := [][]entry{all}
	for len(boxes) < n {
		bi, bc, best := -1, 0, 0
		for i, b := range boxes {
			if len(b) < 2 {
				continue
			}
			var lo, hi px
			lo, hi = b[0].p, b[0].p
			for _, e := range b {
				if e.p.r < lo.r {
					lo.r = e.p.r
				}
				if e.p.r > hi.r {
					hi.r = e.p.r
				}
				if e.p.g < lo.g {
					lo.g = e.p.g
				}
				if e.p.g > hi.g {
					hi.g = e.p.g
				}
				if e.p.b < lo.b {
					lo.b = e.p.b
				}
				if e.p.b > hi.b {
					hi.b = e.p.b
				}
			}
			ch, span := 0, int(hi.r)-int(lo.r)
			if int(hi.g)-int(lo.g) > span {
				ch, span = 1, int(hi.g)-int(lo.g)
			}
			if int(hi.b)-int(lo.b) > span {
				ch, span = 2, int(hi.b)-int(lo.b)
			}
			if span > best {
				best, bi, bc = span, i, ch
			}
		}
		if bi < 0 {
			break
		}
		b := boxes[bi]
		// Sort the selected box along the axis of greatest spread.
		switch bc {
		case 0:
			sortEntries(b, func(e entry) int { return int(e.p.r) })
		case 1:
			sortEntries(b, func(e entry) int { return int(e.p.g) })
		default: // bc == 2
			sortEntries(b, func(e entry) int { return int(e.p.b) })
		}
		mid := len(b) / 2
		boxes[bi] = b[:mid]
		boxes = append(boxes, b[mid:])
	}
	pal := make(color.Palette, 0, n)
	pal = append(pal, color.RGBA{0, 0, 0, 0}) // index 0: reserved transparent
	for _, b := range boxes {
		if len(b) == 0 {
			continue
		}
		var r, g, bl, w int
		for _, e := range b {
			r += int(e.p.r) * e.count
			g += int(e.p.g) * e.count
			bl += int(e.p.b) * e.count
			w += e.count
		}
		pal = append(pal, color.RGBA{R: uint8(r / w), G: uint8(g / w), B: uint8(bl / w), A: 255})
	}
	return pal
}