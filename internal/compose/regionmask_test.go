package compose

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func decodeMaskB64(t *testing.T, b64 string) *image.Gray {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	g, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("mask decode: got %T want *image.Gray", img)
	}
	return g
}

// opaqueNRGBAImage builds an NRGBA test image with the given alpha everywhere.
func opaqueNRGBAImage(w, h int, alpha uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: alpha})
		}
	}
	return img
}

func TestRegionMaskWhiteRectBlackElsewhere(t *testing.T) {
	b64, err := RegionMask(64, 32, image.Rect(8, 4, 33, 20))
	if err != nil {
		t.Fatal(err)
	}
	g := decodeMaskB64(t, b64)
	if g.Bounds().Dx() != 64 || g.Bounds().Dy() != 32 {
		t.Fatalf("mask size %v", g.Bounds())
	}
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			wantWhite := x >= 8 && x < 33 && y >= 4 && y < 20
			got := g.GrayAt(x, y).Y >= 128
			if wantWhite != got {
				t.Fatalf("pixel (%d,%d): wantWhite=%v got=%v", x, y, wantWhite, got)
			}
		}
	}
}

func TestRegionMaskRectOutsideCanvas(t *testing.T) {
	if _, err := RegionMask(64, 32, image.Rect(70, 0, 80, 10)); err == nil {
		t.Fatal("want error for out-of-canvas rect")
	}
	if _, err := RegionMask(64, 32); err == nil {
		t.Fatal("want error for no rects")
	}
}

func TestRegionMaskExcludingCarvesSilhouette(t *testing.T) {
	// exclusion: the left half is opaque
	exclude := opaqueNRGBAImage(64, 32, 255)
	for y := 0; y < 32; y++ {
		for x := 32; x < 64; x++ {
			exclude.SetNRGBA(x, y, color.NRGBA{A: 0})
		}
	}
	b64, err := RegionMaskExcluding(64, 32, 2, exclude, image.Rect(0, 0, 64, 32))
	if err != nil {
		t.Fatal(err)
	}
	g := decodeMaskB64(t, b64)
	for y := 0; y < 32; y++ {
		x := 10 // deep inside the opaque half + dilation
		if g.GrayAt(x, y).Y >= 128 {
			t.Fatalf("pixel (%d,%d) inside silhouette should be carved", x, y)
		}
		x = 48 // transparent half stays white
		if g.GrayAt(x, y).Y < 128 {
			t.Fatalf("pixel (%d,%d) outside silhouette should be white", x, y)
		}
	}
}

func TestRegionMaskExcludingFullyCoveredErrors(t *testing.T) {
	exclude := opaqueNRGBAImage(64, 32, 255)
	if _, err := RegionMaskExcluding(64, 32, 4, exclude, image.Rect(0, 0, 64, 32)); err == nil {
		t.Fatal("want error when the silhouette covers the whole region")
	}
}
