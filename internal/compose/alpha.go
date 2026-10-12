package compose

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
)

// UnmixAlpha recovers an RGBA image from two renders of the same prompt/seed
// composited on pure white and pure black. With premultiplied alpha the
// observed pixel is alpha*F + (1-alpha)*background, so per pixel
// alpha = 255 - (white - black) (white - black = (1-alpha)*255) and F = black*255/alpha. The two renders differ
// by more than the background (the prompt differs by the background clause),
// so the math is noisy at edges; everything clamps. Only the element kind
// uses this; it doubles generation cost for transparent assets.
func UnmixAlpha(whitePNG, blackPNG []byte) ([]byte, error) {
	w, err := png.Decode(bytes.NewReader(whitePNG))
	if err != nil {
		return nil, fmt.Errorf("compose: white render: %v", err)
	}
	b, err := png.Decode(bytes.NewReader(blackPNG))
	if err != nil {
		return nil, fmt.Errorf("compose: black render: %v", err)
	}
	if w.Bounds() != b.Bounds() {
		return nil, errors.New("compose: white/black render bounds differ")
	}
	rect := w.Bounds()
	out := image.NewNRGBA(rect)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			wr, wg, wb, _ := w.At(x, y).RGBA()
			br, bg, bb, _ := b.At(x, y).RGBA()
			dr := int(wr>>8) - int(br>>8)
			dg := int(wg>>8) - int(bg>>8)
			db := int(wb>>8) - int(bb>>8)
			a := 255 - (dr+dg+db)/3
			if a > 255 {
				a = 255
			}
			if a < 0 {
				a = 0
			}
			if a == 0 {
				// transparent: color 0, alpha 0
				continue
			}
			offset := (y-rect.Min.Y)*out.Stride + (x-rect.Min.X)*4
			out.Pix[offset] = clamp8(int(br>>8) * 255 / a)
			out.Pix[offset+1] = clamp8(int(bg>>8) * 255 / a)
			out.Pix[offset+2] = clamp8(int(bb>>8) * 255 / a)
			out.Pix[offset+3] = uint8(a)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
