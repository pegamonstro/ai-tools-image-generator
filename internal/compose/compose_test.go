package compose

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/png"
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

func TestPackGrid(t *testing.T) {
	red := solid(8, 4, color.RGBA{220, 40, 40, 255})
	blue := solid(4, 4, color.RGBA{40, 40, 220, 255})
	sheet, metas, err := PackGrid("spark", []image.Image{red, blue}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("want 2 metas, got %d", len(metas))
	}
	img, err := png.Decode(bytes.NewReader(sheet))
	if err != nil {
		t.Fatalf("sheet is not a PNG: %v", err)
	}
	if img.Bounds().Dx() != 8 || img.Bounds().Dy() != 8 {
		t.Fatalf("want 8x8 sheet from 8x4 cell, got %v", img.Bounds())
	}
	if metas[0].Name != "spark-0000" || metas[0].Frame.W != 8 || metas[0].Frame.Y != 0 {
		t.Fatalf("meta0: %+v", metas[0])
	}
	if metas[1].Name != "spark-0001" || metas[1].Frame.Y != 4 || metas[1].Frame.W != 8 {
		t.Fatalf("meta1: %+v", metas[1])
	}
	// blue 4x4 is centered in the 8-wide cell: gutter at (1,5) is transparent,
	// far edge (5,5) is blue — distinguishes centering from left-alignment
	if _, _, _, a := img.At(1, 5).RGBA(); a != 0 {
		t.Fatalf("pixel (1,5): want transparent gutter")
	}
	if dr, _, _, _ := img.At(5, 5).RGBA(); dr != 40*257 {
		t.Fatalf("blue frame not centered: (5,5) red channel %d, want %d", dr, 40*257)
	}
}

func TestMarshalAtlasShape(t *testing.T) {
	b, err := MarshalAtlas([]AtlasFrame{{Name: "spark-0000", Frame: Rect{0, 0, 8, 4}}})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Frames["spark-0000"].Frame.W != 8 || doc.Meta["app"] != "img-gen compose" {
		t.Fatalf("unexpected doc: %s", b)
	}
}
