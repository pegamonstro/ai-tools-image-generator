package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
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
// the class files (kit, see emitKitMeta). formats must be a subset of
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

// emitStripMeta reads <name>-atlas.json + locates the sheet and emits one
// file per requested format. Both npc shapes ship exactly one sheet under
// different names: walk4 composes <name>-walk.png, variants composes through
// the atlas path as <name>-sheet.png — the probe tries walk first
// (deterministic order; both present cannot occur in practice) and only
// errors when neither exists, naming both candidates.
func emitStripMeta(kind, name string, formats []string, outs []OutputFile) ([]OutputFile, error) {
	aj, ok := findFile(outs, "-atlas.json")
	if !ok {
		return nil, fmt.Errorf("atlas metadata missing for %q", name)
	}
	var doc atlasJSONDoc
	if err := json.Unmarshal(aj.Data, &doc); err != nil {
		return nil, fmt.Errorf("atlas metadata %s: %w", aj.Name, err)
	}
	sheetSuffixes := []string{"-sheet.png"}
	if kind == "npc" {
		sheetSuffixes = []string{"-walk.png", "-sheet.png"}
	}
	var sheet OutputFile
	found := false
	for _, suffix := range sheetSuffixes {
		if of, ok := findFile(outs, suffix); ok {
			sheet, found = of, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("sheet file %s missing for %q", strings.Join(sheetSuffixes, " or "), name)
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
			"name":  e.name,
			"rect":  map[string]int{"x": e.r.X, "y": e.r.Y, "width": e.r.W, "height": e.r.H},
			"pivot": map[string]float64{"x": 0.5, "y": 0.5},
		})
	}
	return map[string]any{"sprites": sprites}
}

func buildSparrow(sheet string, entries []metaEntry) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&buf, `<TextureAtlas imagePath=%q>`+"\n", sheet)
	for _, e := range entries {
		fmt.Fprintf(&buf, "  <SubTexture name=%q x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\"/>\n",
			e.name, e.r.X, e.r.Y, e.r.W, e.r.H)
	}
	buf.WriteString("</TextureAtlas>\n")
	return buf.Bytes(), nil
}

// emitKitMeta assembles the 20 class PNGs (fixed KitClasses order, 4
// columns) into one wangset sheet via PackGrid and emits <name>.tsx
// (tileset + edge-color wangset). Every class file must be present and every
// KitClasses key must map through wangIDForClass — gaps are named errors,
// never silent skips.
func emitKitMeta(name string, formats []string, outs []OutputFile) ([]OutputFile, error) {
	for _, f := range formats {
		if f != "tiled" {
			return nil, fmt.Errorf("kit meta format %q unsupported (only tiled)", f)
		}
	}
	frames := make([]image.Image, 0, len(KitClasses))
	for _, kc := range KitClasses {
		of, ok := findFile(outs, "-"+kc.Key+".png")
		if !ok {
			return nil, fmt.Errorf("kit class file %s-%s.png missing", name, kc.Key)
		}
		img, err := png.Decode(bytes.NewReader(of.Data))
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", of.Name, err)
		}
		frames = append(frames, img)
	}
	sheetPNG, metas, err := PackGrid(name+"-wangset", frames, 4)
	if err != nil {
		return nil, fmt.Errorf("kit wangset sheet: %w", err)
	}
	// Dims come from PackGrid's own layout: every cell shares the first
	// frame's W/H, 20 classes / 4 columns = 5 rows.
	tileW := metas[0].Frame.W
	tileH := metas[0].Frame.H
	columns := 4
	rows := (len(metas) + columns - 1) / columns
	var buf bytes.Buffer
	buf.WriteString("<?xml version=\"1.0\"?>\n")
	fmt.Fprintf(&buf, `<tileset version="1.10" name=%q tilewidth="%d" tileheight="%d" tilecount="%d" columns="%d">`+"\n",
		name, tileW, tileH, len(KitClasses), columns)
	fmt.Fprintf(&buf, ` <image source=%q width="%d" height="%d"/>`+"\n",
		name+"-wangset.png", tileW*columns, tileH*rows)
	fmt.Fprintf(&buf, ` <wangset name=%q type="edge" tile="-1">`+"\n", name)
	buf.WriteString(`  <wangedgecolors>` + "\n")
	buf.WriteString(`   <color color="#000000"/>` + "\n")
	buf.WriteString(`   <color color="#ff8800"/>` + "\n")
	buf.WriteString(`  </wangedgecolors>` + "\n")
	buf.WriteString(`  <wangtiles>` + "\n")
	for i, kc := range KitClasses {
		id, err := wangIDForClass(kc.Key)
		if err != nil {
			return nil, err
		}
		w := tiledWangID(id)
		fmt.Fprintf(&buf, `   <wangtile tileid="%d" wangid="%d,%d,%d,%d,%d,%d,%d,%d"/>`+"\n", i, w[0], w[1], w[2], w[3], w[4], w[5], w[6], w[7])
	}
	buf.WriteString(`  </wangtiles>` + "\n")
	buf.WriteString(` </wangset>` + "\n")
	buf.WriteString("</tileset>\n")
	return []OutputFile{
		{Name: name + "-wangset.png", Data: sheetPNG},
		{Name: name + ".tsx", Data: buf.Bytes()},
	}, nil
}

// tiledWangID lifts an [north, east, south, west] edge tuple into Tiled's
// native 8-element wangid layout [TL, T, TR, L, R, BL, B, BR]: the edge
// positions are 1=Top(N), 3=Left(W), 4=Right(E), 6=Bottom(S), and the corner
// positions 0, 2, 5, 7 stay 0 (every kit class is a pure edge configuration).
func tiledWangID(edges [4]int) [8]int {
	var id [8]int
	id[1] = edges[0] // top == north
	id[3] = edges[3] // left == west
	id[4] = edges[1] // right == east
	id[6] = edges[2] // bottom == south
	return id
}

// wangIDForClass derives a kit class's edge-connectivity tuple
// [north, east, south, west] with colors 0=none 1=line-color, from the
// class's connectivity as documented in wang.go's Hint strings. emitKitMeta
// lifts this tuple into Tiled's native 8-element wangid format via
// tiledWangID. NOTE (documented limitation, see spec): this is an
// EDGE-color-only wangset — inner and outer corner classes share
// connectivity, so Tiled's autotile may pick either. The full 20 classes
// remain individually selectable in the terrain brush. The single source of
// truth for the mapping is this function; reconcile it against the real
// KitClasses table (wang.go:13) when implementing — every key must be
// covered.
//
// Semantic reference (verified against wang.go Hints): line-h runs E–W;
// line-v runs N–S; corner-out-<q> enters one edge and exits the adjacent
// edge through quarter-circle <q>; corner-in-* joins the same edge pair;
// t-<dir> has its crossbar spanning the two edges perpendicular to <dir>
// with the stem exiting <dir> (t-e: crossbar N–S + stem E → [1,1,1,0];
// t-w: crossbar N–S + stem W → [1,0,1,1]; t-n: crossbar E–W + stem N →
// [1,1,0,1]; t-s: crossbar E–W + stem S → [0,1,1,1]); cap-<dir> carries
// line on the single <dir> edge only; cross joins all four; single
// touches no edge.
func wangIDForClass(key string) ([4]int, error) {
	switch {
	case key == "line-h":
		return [4]int{0, 1, 0, 1}, nil
	case key == "line-v":
		return [4]int{1, 0, 1, 0}, nil
	case key == "cross":
		return [4]int{1, 1, 1, 1}, nil
	case key == "single":
		return [4]int{0, 0, 0, 0}, nil
	case strings.HasPrefix(key, "t-"):
		switch key[2:] {
		case "e":
			return [4]int{1, 1, 1, 0}, nil // crossbar N–S, stem E
		case "w":
			return [4]int{1, 0, 1, 1}, nil // crossbar N–S, stem W
		case "n":
			return [4]int{1, 1, 0, 1}, nil // crossbar E–W, stem N
		case "s":
			return [4]int{0, 1, 1, 1}, nil // crossbar E–W, stem S
		}
		return [4]int{}, fmt.Errorf("kit class %q: bad t-stem", key)
	case strings.HasPrefix(key, "cap-"):
		if len(key) != 5 {
			return [4]int{}, fmt.Errorf("kit class %q: bad cap", key)
		}
		id := [4]int{0, 0, 0, 0}
		switch key[4:] {
		case "e":
			id[1] = 1
		case "w":
			id[3] = 1
		case "n":
			id[0] = 1
		case "s":
			id[2] = 1
		default:
			return [4]int{}, fmt.Errorf("kit class %q: bad cap", key)
		}
		return id, nil
	case strings.HasPrefix(key, "corner-out-"), strings.HasPrefix(key, "corner-in-"):
		id := [4]int{0, 0, 0, 0}
		switch key[len(key)-2:] {
		case "nw":
			id[0], id[3] = 1, 1
		case "ne":
			id[0], id[1] = 1, 1
		case "sw":
			id[2], id[3] = 1, 1
		case "se":
			id[1], id[2] = 1, 1
		default:
			return [4]int{}, fmt.Errorf("kit class %q: bad corner", key)
		}
		return id, nil
	}
	return [4]int{}, fmt.Errorf("kit class %q: no wang mapping", key)
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
