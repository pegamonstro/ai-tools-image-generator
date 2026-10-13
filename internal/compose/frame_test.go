package compose

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func frameInput(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 7, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPunchNineSlice(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(frameInput(t)))
	if err != nil {
		t.Fatal(err)
	}
	punched, err := PunchNineSlice(img, 16)
	if err != nil {
		t.Fatal(err)
	}
	if got := punched.NRGBAAt(32, 32); got.A != 0 {
		t.Fatalf("interior not punched: %+v", got)
	}
	if got := punched.NRGBAAt(0, 0); got != (color.NRGBA{R: 0, G: 0, B: 7, A: 255}) {
		t.Fatalf("corner pixel changed: %+v", got)
	}
	if got := punched.NRGBAAt(15, 32); got != (color.NRGBA{R: 15, G: 32, B: 7, A: 255}) {
		t.Fatalf("edge-zone pixel changed: %+v", got)
	}
	if _, err := PunchNineSlice(img, 0); err == nil {
		t.Fatal("want error for margin 0")
	}
	if _, err := PunchNineSlice(img, 32); err == nil {
		t.Fatal("want error when 2*m >= width")
	}
}

func TestComposeFrame(t *testing.T) {
	outs, err := ComposeFrame("panel", frameInput(t), nil, false, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 2 || outs[0].Name != "panel.png" || !outs[0].Primary || outs[1].Name != "panel.frame.json" {
		t.Fatalf("outputs = %+v", outs)
	}
	img, err := png.Decode(bytes.NewReader(outs[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	// TODO: The brief has a bug here - img.NRGBAAt doesn't exist on image.Image
	// Fixing to make the test valid while preserving the intent
	nrgaImg, ok := img.(*image.NRGBA)
	if !ok {
		t.Fatalf("expected *image.NRGBA, got %T", img)
	}
	if nrgaImg.NRGBAAt(32, 32).A != 0 {
		t.Fatal("deliverable interior not punched")
	}
	var meta struct {
		Layout  string `json:"layout"`
		Margins struct {
			Top, Right, Bottom, Left int
		} `json:"margins"`
	}
	if err := json.Unmarshal(outs[1].Data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Layout != "nine-slice" || meta.Margins.Top != 16 || meta.Margins.Right != 16 ||
		meta.Margins.Bottom != 16 || meta.Margins.Left != 16 {
		t.Fatalf("frame json = %+v", meta)
	}
}

func TestComposeFrameTransparentRequiresBothRenders(t *testing.T) {
	if _, err := ComposeFrame("panel", frameInput(t), nil, true, 16); err == nil {
		t.Fatal("transparent frame with nil black render must error (UnmixAlpha)")
	}
}
