package compose

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// FlipHorizontal mirrors img left-right: column x becomes column w-1-x.
func FlipHorizontal(img image.Image) *image.NRGBA {
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.SetNRGBA(b.Dx()-1-(x-b.Min.X), y, color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA))
		}
	}
	return out
}

type npcCell struct {
	Name string
	Img  image.Image
}

// ComposeNPCWalk4 packs the three direction renders (down, up, right) plus
// the mirrored left cell into the down, left, right, up sheet — RPG-Maker
// order, the 3-drawing rule (left is a flip of right, never re-rendered).
func ComposeNPCWalk4(name string, down, up, right []byte) ([]OutputFile, error) {
	var dirs []npcCell
	for i, b := range [][]byte{down, up, right} {
		im, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("compose: walk4 %q direction %d is not a PNG: %v", name, i, err)
		}
		dirs = append(dirs, npcCell{Img: im})
	}
	left := npcCell{Img: FlipHorizontal(dirs[2].Img)}
	cells := []npcCell{{Img: dirs[0].Img}, left, {Img: dirs[2].Img}, {Img: dirs[1].Img}}
	sheet, metas, err := PackGrid(name, []image.Image{cells[0].Img, cells[1].Img, cells[2].Img, cells[3].Img}, 1)
	if err != nil {
		return nil, err
	}
	metaJSON, err := MarshalAtlas(metas)
	if err != nil {
		return nil, err
	}
	files := []OutputFile{
		{Name: name + "-walk.png", Data: sheet, Primary: true},
		{Name: name + "-atlas.json", Data: metaJSON},
	}
	names := []string{"down", "left", "right", "up"}
	for i, c := range cells {
		data, err := encodePNG(c.Img)
		if err != nil {
			return nil, err
		}
		files = append(files, OutputFile{Name: fmt.Sprintf("%s-%s.png", name, names[i]), Data: data})
	}
	return files, nil
}
