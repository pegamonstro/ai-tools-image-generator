package compose

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngBytesBlit(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodePNGBlit(t *testing.T, data []byte) *image.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	n := image.NewNRGBA(img.Bounds())
	// draw via NRGBAModel so any source representation lands in NRGBA space
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			n.SetNRGBA(x-img.Bounds().Min.X, y-img.Bounds().Min.Y, color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA))
		}
	}
	return n
}

func TestBlitImage(t *testing.T) {
	base := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			base.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	ovr := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			ovr.SetNRGBA(x, y, color.NRGBA{B: 255, A: 255})
		}
	}
	out, err := BlitImage(base, ovr, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NRGBAAt(4, 4); got != (color.NRGBA{B: 255, A: 255}) {
		t.Fatalf("blitted pixel = %+v", got)
	}
	if got := out.NRGBAAt(0, 0); got != (color.NRGBA{R: 255, A: 255}) {
		t.Fatalf("base pixel changed: %+v", got)
	}

	// Fully transparent overlay leaves base untouched (alpha-correct Over).
	clear := image.NewNRGBA(image.Rect(0, 0, 2, 2)) // all A=0
	out2, err := BlitImage(base, clear, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := out2.NRGBAAt(0, 0); got != (color.NRGBA{R: 255, A: 255}) {
		t.Fatalf("zero-alpha overlay altered base: %+v", got)
	}

	// Overhang: error naming the geometry.
	if _, err := BlitImage(base, ovr, 7, 7); err == nil {
		t.Fatal("want overhang error")
	}
	if _, err := BlitImage(base, ovr, -1, 0); err == nil {
		t.Fatal("want negative-pos error")
	}
}
