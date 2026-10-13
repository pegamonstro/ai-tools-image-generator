package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

// ClipFillPNG returns img clipped to the left pct% of its width (full
// height). Smaller sizes floor to one pixel so even a 1% state shows a
// sliver of fill.
func ClipFillPNG(img image.Image, pct int) (*image.NRGBA, error) {
	if pct < 1 || pct > 100 {
		return nil, fmt.Errorf("compose: fill state %d out of range 1..100", pct)
	}
	b := img.Bounds()
	nw := b.Dx() * pct / 100
	if nw < 1 {
		nw = 1
	}
	out := image.NewNRGBA(image.Rect(0, 0, nw, b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	return out, nil
}

// ComposeBar produces a bar's deliverables: the nine-slice-punched track
// (primary), the full fill render, one clipped PNG per state, and the bar
// JSON. The fill is never re-rendered — every state is a horizontal clip of
// the single render.
func ComposeBar(name string, trackWhite, trackBlack []byte, transparent bool, margins int, fill []byte, states []int) ([]OutputFile, error) {
	el, err := ComposeElement(name, trackWhite, trackBlack, transparent)
	if err != nil {
		return nil, err
	}
	trackImg, err := png.Decode(bytes.NewReader(el.Data))
	if err != nil {
		return nil, fmt.Errorf("compose: bar %q track render is not a PNG: %v", name, err)
	}
	track, err := PunchNineSlice(trackImg, margins)
	if err != nil {
		return nil, err
	}
	fillImg, err := png.Decode(bytes.NewReader(fill))
	if err != nil {
		return nil, fmt.Errorf("compose: bar %q fill render is not a PNG: %v", name, err)
	}
	trackData, err := encodePNG(track)
	if err != nil {
		return nil, err
	}
	fillData, err := encodePNG(fillImg)
	if err != nil {
		return nil, err
	}
	files := []OutputFile{
		{Name: name + "-track.png", Data: trackData, Primary: true},
		{Name: name + "-fill.png", Data: fillData},
	}
	for _, pct := range states {
		clipped, cerr := ClipFillPNG(fillImg, pct)
		if cerr != nil {
			return nil, cerr
		}
		cd, cerr := encodePNG(clipped)
		if cerr != nil {
			return nil, cerr
		}
		files = append(files, OutputFile{Name: fmt.Sprintf("%s-fill-%03d.png", name, pct), Data: cd})
	}
	meta, err := json.Marshal(struct {
		Track   string      `json:"track"`
		Fill    string      `json:"fill"`
		Margins marginsJSON `json:"margins"`
		States  []int       `json:"states"`
	}{Track: name + "-track.png", Fill: name + "-fill.png", Margins: uniformMargins(margins), States: states})
	if err != nil {
		return nil, err
	}
	return append(files, OutputFile{Name: name + ".bar.json", Data: meta}), nil
}
