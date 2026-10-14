package compose

import (
	"errors"
	"fmt"
	"image"
)

// SheetCell is one named view render on a character sheet.
type SheetCell struct {
	View string
	Img  image.Image
}

// ComposeSheet packs named view renders onto one uniform grid sheet and emits
// sheet metadata JSON naming each cell's view (the atlas.json contract, keyed
// by view for its different consumers). columns <= 0 lays one row. The PNG is
// <name>-sheet.png, the metadata <name>-sheet.json.
func ComposeSheet(name string, cells []SheetCell, columns int) ([]OutputFile, error) {
	if len(cells) == 0 {
		return nil, errors.New("compose: no sheet cells")
	}
	for i, c := range cells {
		if c.View == "" {
			return nil, fmt.Errorf("compose: sheet %q cell %d has no view name", name, i)
		}
		if c.Img == nil {
			return nil, fmt.Errorf("compose: sheet %q view %q render missing", name, c.View)
		}
	}
	frames := make([]image.Image, len(cells))
	for i, c := range cells {
		frames[i] = c.Img
	}
	sheet, metas, err := PackGrid(name, frames, columns)
	if err != nil {
		return nil, err
	}
	for i, c := range cells {
		metas[i].Name = c.View
	}
	metaJSON, err := MarshalAtlas(metas)
	if err != nil {
		return nil, err
	}
	return []OutputFile{
		{Name: name + "-sheet.png", Data: sheet, Primary: true},
		{Name: name + "-sheet.json", Data: metaJSON},
	}, nil
}
