package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// marginsJSON is the sidecar margins block shared by frame and bar files.
type marginsJSON struct {
	Top    int `json:"top"`
	Right  int `json:"right"`
	Bottom int `json:"bottom"`
	Left   int `json:"left"`
}

func uniformMargins(m int) marginsJSON {
	return marginsJSON{Top: m, Right: m, Bottom: m, Left: m}
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("compose: png encode: %v", err)
	}
	return buf.Bytes(), nil
}

// PunchNineSlice returns a copy with the interior rect [m, w-m)x[m, h-m)
// forced fully transparent. m must be 1..min(w,h)/2-1 — the same bounds the
// queue validates.
func PunchNineSlice(img image.Image, m int) (*image.NRGBA, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if m < 1 || 2*m >= w || 2*m >= h {
		return nil, fmt.Errorf("compose: nine-slice margin %d out of bounds for %dx%d", m, w, h)
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	for y := m; y < h-m; y++ {
		for x := m; x < w-m; x++ {
			out.SetNRGBA(x, y, color.NRGBA{})
		}
	}
	return out, nil
}

// ComposeFrame produces a nine-slice frame deliverable from the element
// render path: the border art PNG with the interior punched transparent,
// plus the margins sidecar.
func ComposeFrame(name string, white, black []byte, transparent bool, margins int) ([]OutputFile, error) {
	el, err := ComposeElement(name, white, black, transparent)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(el.Data))
	if err != nil {
		return nil, fmt.Errorf("compose: frame %q render is not a PNG: %v", name, err)
	}
	punched, err := PunchNineSlice(img, margins)
	if err != nil {
		return nil, err
	}
	data, err := encodePNG(punched)
	if err != nil {
		return nil, err
	}
	meta, err := json.Marshal(struct {
		Layout  string      `json:"layout"`
		Margins marginsJSON `json:"margins"`
	}{Layout: "nine-slice", Margins: uniformMargins(margins)})
	if err != nil {
		return nil, err
	}
	return []OutputFile{
		{Name: name + ".png", Data: data, Primary: true},
		{Name: name + ".frame.json", Data: meta},
	}, nil
}
