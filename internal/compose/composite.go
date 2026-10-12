package compose

import (
	"fmt"
	"image"
	"image/draw"
)

// BlitImage alpha-composites overlay onto a copy of base with the overlay's
// top-left corner at (x, y). The overlay must fit inside the base canvas —
// an overhang is an error naming the geometry, never a silent crop.
func BlitImage(base, overlay image.Image, x, y int) (*image.NRGBA, error) {
	bb, ob := base.Bounds(), overlay.Bounds()
	if x < 0 || y < 0 || x+ob.Dx() > bb.Dx() || y+ob.Dy() > bb.Dy() {
		return nil, fmt.Errorf("compose: static layer %dx%d at (+%d,+%d) overhangs canvas %dx%d", ob.Dx(), ob.Dy(), x, y, bb.Dx(), bb.Dy())
	}
	out := image.NewNRGBA(image.Rect(0, 0, bb.Dx(), bb.Dy()))
	draw.Draw(out, out.Bounds(), base, bb.Min, draw.Src)
	draw.Draw(out, image.Rect(x, y, x+ob.Dx(), y+ob.Dy()), overlay, ob.Min, draw.Over)
	return out, nil
}
