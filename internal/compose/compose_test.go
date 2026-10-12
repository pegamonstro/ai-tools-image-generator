package compose

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

func solid(w, h int, c color.RGBA) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: c.A})
		}
	}
	return img
}

func TestEncodeGIFBasic(t *testing.T) {
	red := color.RGBA{220, 40, 40, 255}
	blue := color.RGBA{40, 40, 220, 255}
	out, err := EncodeGIF([]image.Image{solid(16, 16, red), solid(16, 16, blue)}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Image) != 2 || len(g.Delay) != 2 {
		t.Fatalf("want 2 frames, got %d", len(g.Image))
	}
	if g.Delay[0] != 10 || g.Delay[1] != 10 {
		t.Fatalf("want delay 10cs, got %v", g.Delay)
	}
	if g.LoopCount != 0 {
		t.Fatalf("want loop forever (0), got %d", g.LoopCount)
	}
}

func TestEncodeGIFTransparentDelta(t *testing.T) {
	red := color.RGBA{220, 40, 40, 255}
	blue := color.RGBA{40, 40, 220, 255}
	out, err := EncodeGIF([]image.Image{solid(16, 16, red), solid(16, 16, red), solid(16, 16, blue)}, 40, 3)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if g.Delay[0] != 4 || g.LoopCount != 3 {
		t.Fatalf("delay/loop: %v/%d", g.Delay, g.LoopCount)
	}
	// Frame 1 is identical to frame 0: every pixel must be the transparent index.
	holes := 0
	for _, v := range g.Image[1].Pix {
		if v == 0 {
			holes++
		}
	}
	if holes != len(g.Image[1].Pix) {
		t.Fatalf("identical frame should be all-transparent: %d/%d holes", holes, len(g.Image[1].Pix))
	}
}

func TestEncodeGIFSizeMismatch(t *testing.T) {
	red := color.RGBA{220, 40, 40, 255}
	if _, err := EncodeGIF([]image.Image{solid(16, 16, red), solid(8, 8, red)}, 100, 0); err == nil {
		t.Fatal("want error for mismatched frame sizes")
	}
}

func TestEncodeGIFDeterministicPalette(t *testing.T) {
	// Colors that tie on red channel (100) but differ in green and blue.
	c1 := color.RGBA{100, 0, 0, 255}
	c2 := color.RGBA{100, 255, 0, 255}
	c3 := color.RGBA{100, 0, 255, 255}
	frames := []image.Image{
		solid(16, 16, c1),
		solid(16, 16, c2),
		solid(16, 16, c3),
	}
	out1, err := EncodeGIF(frames, 100, 0)
	if err != nil {
		t.Fatalf("first encode: %v", err)
	}
	out2, err := EncodeGIF(frames, 100, 0)
	if err != nil {
		t.Fatalf("second encode: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("palette not deterministic: outputs differ")
	}
	// Additionally, verify that we can decode and get three frames.
	g, err := gif.DecodeAll(bytes.NewReader(out1))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Image) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(g.Image))
	}
}