package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

// Rect represents a rectangle in the atlas.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// AtlasFrame represents a single frame in the atlas.
type AtlasFrame struct {
	Name  string `json:"name"`
	Frame Rect   `json:"frame"`
}

// PackGrid lays frames out on a uniform grid sheet. The cell is the max frame
// size and each frame is centered inside its cell, so TexturePacker-style
// consumers can slice by rectangle and game engines get stable cell geometry.
// columns <= 0 means "one row".
func PackGrid(name string, frames []image.Image, columns int) ([]byte, []AtlasFrame, error) {
	if len(frames) == 0 {
		return nil, nil, errors.New("compose: no frames")
	}
	if columns < 1 {
		columns = len(frames)
	}
	cw, ch := 0, 0
	for _, f := range frames {
		if b := f.Bounds(); b.Dx() > cw {
			cw = b.Dx()
		}
		if b := f.Bounds(); b.Dy() > ch {
			ch = b.Dy()
		}
	}
	rows := (len(frames) + columns - 1) / columns
	sheet := image.NewNRGBA(image.Rect(0, 0, cw*columns, ch*rows))
	metas := make([]AtlasFrame, 0, len(frames))
	for i, f := range frames {
		col, row := i%columns, i/columns
		b := f.Bounds()
		ox := col*cw + (cw-b.Dx())/2
		oy := row*ch + (ch-b.Dy())/2
		draw.Draw(sheet, image.Rect(ox, oy, ox+b.Dx(), oy+b.Dy()), f, b.Min, draw.Src)
		metas = append(metas, AtlasFrame{
			Name:  fmt.Sprintf("%s-%04d", name, i),
			Frame: Rect{X: col * cw, Y: row * ch, W: cw, H: ch},
		})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, sheet); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), metas, nil
}

// MarshalAtlas renders TexturePacker JSON-Hash metadata — the format Phaser,
// PixiJS and most engines read via their "JSON (Hash)" atlas loaders.
func MarshalAtlas(metas []AtlasFrame) ([]byte, error) {
	type atlasEntry struct {
		Frame Rect `json:"frame"`
	}
	type atlasDoc struct {
		Frames map[string]atlasEntry `json:"frames"`
		Meta   map[string]string     `json:"meta"`
	}
	doc := atlasDoc{Frames: map[string]atlasEntry{}, Meta: map[string]string{"app": "img-gen compose"}}
	for _, m := range metas {
		doc.Frames[m.Name] = atlasEntry{Frame: m.Frame}
	}
	return json.Marshal(doc)
}
