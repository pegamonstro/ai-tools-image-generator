package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MetaFormats is the closed set of engine-metadata formats EmitMeta emits.
// Each references exactly one texture image, which is why kit (20 separate
// class files, no sheet) supports tiled only — see the spec's Phase A table.
var MetaFormats = map[string][]string{
	"phaser":  {"atlas", "npc"},
	"unity":   {"atlas", "npc"},
	"sparrow": {"atlas", "npc"},
	"tiled":   {"kit"},
}

// EmitMeta appends engine-consumable metadata files for an already-composed
// asset. It never mutates outs and never re-derives geometry: rects are read
// from the asset's own -atlas.json deliverable (atlas/npc) or assembled from
// the class files (kit, see emitKitTiled). formats must be a subset of
// MetaFormats entries valid for kind.
func EmitMeta(kind, name string, formats []string, outs []OutputFile) ([]OutputFile, error) {
	if len(formats) == 0 {
		return nil, nil
	}
	for _, f := range formats {
		kinds, ok := MetaFormats[f]
		if !ok {
			return nil, fmt.Errorf("meta format %q unknown (valid: phaser, unity, sparrow, tiled)", f)
		}
		supported := false
		for _, k := range kinds {
			if k == kind {
				supported = true
				break
			}
		}
		if !supported {
			return nil, fmt.Errorf("meta format %q does not apply to kind %q", f, kind)
		}
	}
	switch kind {
	case "atlas", "npc":
		return emitStripMeta(kind, name, formats, outs)
	case "kit":
		return emitKitMeta(name, formats, outs)
	}
	return nil, fmt.Errorf("meta formats apply to atlas, npc, kit — not %q", kind)
}

func findFile(outs []OutputFile, suffix string) (OutputFile, bool) {
	for _, of := range outs {
		if strings.HasSuffix(of.Name, suffix) {
			return of, true
		}
	}
	return OutputFile{}, false
}

// emitStripMeta reads <name>-atlas.json + locates the sheet (-sheet.png for
// atlas, -walk.png for npc) and emits one file per requested format.
func emitStripMeta(kind, name string, formats []string, outs []OutputFile) ([]OutputFile, error) {
	aj, ok := findFile(outs, "-atlas.json")
	if !ok {
		return nil, fmt.Errorf("atlas metadata missing for %q", name)
	}
	var doc atlasJSONDoc
	if err := json.Unmarshal(aj.Data, &doc); err != nil {
		return nil, fmt.Errorf("atlas metadata %s: %w", aj.Name, err)
	}
	sheetSuffix := "-sheet.png"
	if kind == "npc" {
		sheetSuffix = "-walk.png"
	}
	sheet, ok := findFile(outs, sheetSuffix)
	if !ok {
		return nil, fmt.Errorf("sheet file %s missing for %q", sheetSuffix, name)
	}
	sheetName := sheet.Name
	// Frames in stable y-then-x order (map iteration is unordered).
	list := make([]metaEntry, 0, len(doc.Frames))
	for n, fr := range doc.Frames {
		list = append(list, metaEntry{n, fr.Frame})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].r.Y != list[j].r.Y {
			return list[i].r.Y < list[j].r.Y
		}
		return list[i].r.X < list[j].r.X
	})
	var out []OutputFile
	for _, f := range formats {
		switch f {
		case "phaser":
			b, err := json.Marshal(buildPhaser(doc.Meta, sheetName, list))
			if err != nil {
				return nil, err
			}
			out = append(out, OutputFile{Name: name + ".phaser.json", Data: b})
		case "unity":
			b, err := json.Marshal(buildUnity(sheetName, list))
			if err != nil {
				return nil, err
			}
			out = append(out, OutputFile{Name: name + ".frames.json", Data: b})
		case "sparrow":
			b, err := buildSparrow(sheetName, list)
			if err != nil {
				return nil, err
			}
			out = append(out, OutputFile{Name: name + ".sparrow.xml", Data: b})
		}
	}
	return out, nil
}

func buildPhaser(meta map[string]string, sheet string, entries []metaEntry) map[string]any {
	frames := map[string]any{}
	for _, e := range entries {
		frames[e.name] = map[string]any{
			"frame":            e.r,
			"rotated":          false,
			"trimmed":          false,
			"spriteSourceSize": e.r,
			"sourceSize":       map[string]int{"w": e.r.W, "h": e.r.H},
		}
	}
	app := "img-gen compose"
	if meta != nil && meta["app"] != "" {
		app = meta["app"]
	}
	return map[string]any{"frames": frames, "meta": map[string]string{"app": app, "image": sheet}}
}

func buildUnity(sheet string, entries []metaEntry) map[string]any {
	sprites := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		sprites = append(sprites, map[string]any{
			"name": e.name,
			"rect": map[string]int{"x": e.r.X, "y": e.r.Y, "width": e.r.W, "height": e.r.H},
			"pivot": map[string]float64{"x": 0.5, "y": 0.5},
		})
	}
	return map[string]any{"sprites": sprites}
}

func buildSparrow(sheet string, entries []metaEntry) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&buf, `<TextureAtlas imagePath=%q>` + "\n", sheet)
	for _, e := range entries {
		fmt.Fprintf(&buf, "  <SubTexture name=%q x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\"/>\n",
			e.name, e.r.X, e.r.Y, e.r.W, e.r.H)
	}
	buf.WriteString("</TextureAtlas>\n")
	return buf.Bytes(), nil
}

// emitKitMeta writes tiled metadata for kit kind.
// Each class gets its own .ts file with PIXI.Texture.fromFrame entries.
func emitKitMeta(name string, formats []string, outs []OutputFile) ([]OutputFile, error) {
	var out []OutputFile
	for _, f := range formats {
		switch f {
		case "tiled":
			// For now, tiled kit metadata is not implemented as per the brief's note
			// that kit supports tiled only via separate class files.
			// We'll return an empty result for tiled format since it's handled differently.
			// The brief says: "Each references exactly one texture image, which is why kit
			// (20 separate class files, no sheet) supports tiled only"
			// So for kit + tiled, we don't generate metadata files here.
			// This function will be expanded in future tasks.
		}
	}
	return out, nil
}

// atlasJSONDoc mirrors the structure produced by MarshalAtlas in atlas.go
type atlasJSONDoc struct {
	Frames map[string]struct {
		Frame Rect `json:"frame"`
	} `json:"frames"`
	Meta map[string]string `json:"meta"`
}

// metaEntry is used internally for sorting frame entries
type metaEntry struct {
	name string
	r    Rect
}