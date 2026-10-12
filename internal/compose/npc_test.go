package compose

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"testing"
)

var (
	colA = nrgba(255, 0, 0)
	colB = nrgba(0, 255, 0)
	colC = nrgba(0, 0, 255)
)

func nrgba(r, g, b uint8) (c color.NRGBA) { return color.NRGBA{R: r, G: g, B: b, A: 255} }

// solidColorPNG returns a PNG encoding of a solid color image of the given size and color.
func solidColorPNG(t *testing.T, w, h int, c color.NRGBA) []byte {
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

// decodePNGToNRGBA decodes a PNG and returns it as *image.NRGBA.
func decodePNGToNRGBA(t *testing.T, data []byte) *image.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// Convert to *image.NRGBA
	nrgba := image.NewNRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			nrgba.SetNRGBA(x, y, color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA))
		}
	}
	return nrgba
}

func TestFlipHorizontal(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	img.SetNRGBA(0, 0, colA)
	img.SetNRGBA(1, 0, colB)
	img.SetNRGBA(2, 0, colC)
	out := FlipHorizontal(img)
	if out.NRGBAAt(0, 0) != colC || out.NRGBAAt(1, 0) != colB || out.NRGBAAt(2, 0) != colA {
		t.Fatalf("flip wrong: %+v", out)
	}
}

func TestComposeNPCWalk4(t *testing.T) {
	down := solidColorPNG(t, 32, 32, color.NRGBA{R: 10, A: 255})
	up := solidColorPNG(t, 32, 32, color.NRGBA{G: 10, A: 255})
	right := solidColorPNG(t, 32, 32, color.NRGBA{B: 10, A: 255})
	outs, err := ComposeNPCWalk4("guard", down, up, right)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string][]byte{}
	want := []string{"guard-walk.png", "guard-atlas.json", "guard-down.png", "guard-left.png", "guard-right.png", "guard-up.png"}
	if len(outs) != len(want) {
		t.Fatalf("outputs = %d, want %d", len(outs), len(want))
	}
	for _, of := range outs {
		byName[of.Name] = of.Data
	}
	for _, name := range want {
		if byName[name] == nil {
			t.Fatalf("missing %q", name)
		}
	}
	if !outs[0].Primary || outs[0].Name != "guard-walk.png" {
		t.Fatalf("primary = %q", outs[0].Name)
	}
	// Sheet is 4 rows x 1 col: 32x128.
	sheet, err := png.Decode(bytes.NewReader(byName["guard-walk.png"]))
	if err != nil {
		t.Fatal(err)
	}
	if sheet.Bounds().Dx() != 32 || sheet.Bounds().Dy() != 128 {
		t.Fatalf("sheet = %dx%d, want 32x128", sheet.Bounds().Dx(), sheet.Bounds().Dy())
	}
	// left is a true mirror of right.
	limg := decodePNGToNRGBA(t, byName["guard-left.png"])
	rimg := decodePNGToNRGBA(t, byName["guard-right.png"])
	w := rimg.Bounds().Dx()
	for y := 0; y < 32; y++ {
		for x := 0; x < w; x++ {
			if limg.NRGBAAt(x, y) != rimg.NRGBAAt(w-1-x, y) {
				t.Fatalf("left(%d,%d) != mirror right", x, y)
			}
		}
	}
	// R-7: additionally assert the atlas.json cell order is down, left, right, up.
	var atlasDoc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(outs[1].Data, &atlasDoc); err != nil {
		t.Fatalf("failed to unmarshal atlas: %v", err)
	}
	baseName := "guard"
	// down: baseName + "-0000" -> Y = 0
	if atlasDoc.Frames[baseName+"-0000"].Frame.Y != 0 {
		t.Fatalf("down frame expected Y=0, got %d", atlasDoc.Frames[baseName+"-0000"].Frame.Y)
	}
	// left: baseName + "-0001" -> Y = 32 (cell height)
	if atlasDoc.Frames[baseName+"-0001"].Frame.Y != 32 {
		t.Fatalf("left frame expected Y=32, got %d", atlasDoc.Frames[baseName+"-0001"].Frame.Y)
	}
	// right: baseName + "-0002" -> Y = 64
	if atlasDoc.Frames[baseName+"-0002"].Frame.Y != 64 {
		t.Fatalf("right frame expected Y=64, got %d", atlasDoc.Frames[baseName+"-0002"].Frame.Y)
	}
	// up: baseName + "-0003" -> Y = 96
	if atlasDoc.Frames[baseName+"-0003"].Frame.Y != 96 {
		t.Fatalf("up frame expected Y=96, got %d", atlasDoc.Frames[baseName+"-0003"].Frame.Y)
	}
}
