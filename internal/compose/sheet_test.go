package compose

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func solidImage(w, h int, c color.Color) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBAModel.Convert(c).(color.NRGBA))
		}
	}
	return img
}

func TestComposeSheet(t *testing.T) {
	cells := []sheetCell{
		{View: "front", Img: solidImage(16, 16, color.RGBA{R: 255, A: 255})},
		{View: "side", Img: solidImage(16, 16, color.RGBA{G: 255, A: 255})},
		{View: "back", Img: solidImage(16, 16, color.RGBA{B: 255, A: 255})},
		{View: "casting", Img: solidImage(16, 16, color.RGBA{R: 255, B: 255, A: 255})},
	}
	files, err := ComposeSheet("hero", cells, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d", len(files))
	}
	if files[0].Name != "hero-sheet.png" || !files[0].Primary {
		t.Fatalf("png file = %+v", files[0])
	}
	if files[1].Name != "hero-sheet.json" || files[1].Primary {
		t.Fatalf("json file = %+v", files[1])
	}
	im, err := png.Decode(bytes.NewReader(files[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	if b := im.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
		t.Fatalf("sheet dims = %dx%d", b.Dx(), b.Dy())
	}
	var doc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(files[1].Data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range cells {
		fr, ok := doc.Frames[c.View]
		if !ok {
			t.Fatalf("view %q missing from %v", c.View, doc.Frames)
		}
		if fr.Frame.W != 16 || fr.Frame.H != 16 {
			t.Fatalf("view %q rect = %+v", c.View, fr.Frame)
		}
	}
	if doc.Frames["front"].Frame.X != 0 || doc.Frames["front"].Frame.Y != 0 {
		t.Fatalf("front rect = %+v", doc.Frames["front"].Frame)
	}
	if doc.Frames["side"].Frame.X != 16 || doc.Frames["side"].Frame.Y != 0 {
		t.Fatalf("side rect = %+v", doc.Frames["side"].Frame)
	}
	if doc.Frames["back"].Frame.X != 0 || doc.Frames["back"].Frame.Y != 16 {
		t.Fatalf("back rect = %+v", doc.Frames["back"].Frame)
	}
}

func TestComposeSheetOneRowDefault(t *testing.T) {
	cells := []sheetCell{
		{View: "front", Img: solidImage(16, 16, color.RGBA{A: 255})},
		{View: "side", Img: solidImage(16, 16, color.RGBA{A: 255})},
		{View: "back", Img: solidImage(16, 16, color.RGBA{A: 255})},
	}
	files, err := ComposeSheet("hero", cells, 0)
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(files[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	if b := im.Bounds(); b.Dx() != 48 || b.Dy() != 16 {
		t.Fatalf("one-row dims = %dx%d", b.Dx(), b.Dy())
	}
	var doc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(files[1].Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Frames["back"].Frame.Y != 0 {
		t.Fatalf("back row = %+v", doc.Frames["back"].Frame)
	}
}

func TestComposeSheetValidation(t *testing.T) {
	if _, err := ComposeSheet("hero", nil, 0); err == nil {
		t.Fatal("empty cells")
	}
	if _, err := ComposeSheet("hero", []sheetCell{{View: "", Img: solidImage(1, 1, color.Black)}}, 0); err == nil || !strings.Contains(err.Error(), "view") {
		t.Fatalf("empty view name: %v", err)
	}
	if _, err := ComposeSheet("hero", []sheetCell{{View: "front"}}, 0); err == nil || !strings.Contains(err.Error(), "render") {
		t.Fatalf("nil img: %v", err)
	}
}
