package compose

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// OutputFile is defined in compose.go
// Rect is defined in atlas.go

func atlasOuts(t *testing.T, name string, w, h int, frames int) []OutputFile {
	// build outs the way ComposeAtlas does: encode `frames` distinct 16x16
	// NRGBA pngs, call ComposeAtlas(name, pngs, 0). Returns the 2-file outs.
	pngs := make([][]byte, frames)
	for i := 0; i < frames; i++ {
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		// Fill with a distinct color so frames are different
		c := color.NRGBA{R: uint8(i * 50), G: uint8(i * 30), B: uint8(i * 100), A: 255}
		for x := 0; x < w; x++ {
			for y := 0; y < h; y++ {
				img.SetNRGBA(x, y, c)
			}
		}
		buf := new(bytes.Buffer)
		if err := png.Encode(buf, img); err != nil {
			t.Fatalf("encode png: %v", err)
		}
		pngs[i] = buf.Bytes()
	}
	outs, err := ComposeAtlas(name, pngs, 0) // columns=0 means one row
	if err != nil {
		t.Fatalf("compose atlas: %v", err)
	}
	return outs
}

func composeNPCWalk4Outs(t *testing.T, name string) []OutputFile {
	// encode three 16x16 pngs for down, up, right
	pngs := make([][]byte, 3)
	for i := 0; i < 3; i++ {
		img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
		// Fill with distinct colors
		var c color.NRGBA
		switch i {
		case 0: // down
			c = color.NRGBA{R: 255, G: 0, B: 0, A: 255}
		case 1: // up
			c = color.NRGBA{R: 0, G: 255, B: 0, A: 255}
		case 2: // right
			c = color.NRGBA{R: 0, G: 0, B: 255, A: 255}
		}
		for x := 0; x < 16; x++ {
			for y := 0; y < 16; y++ {
				img.SetNRGBA(x, y, c)
			}
		}
		buf := new(bytes.Buffer)
		if err := png.Encode(buf, img); err != nil {
			t.Fatalf("encode png: %v", err)
		}
		pngs[i] = buf.Bytes()
	}
	outs, err := ComposeNPCWalk4(name, pngs[0], pngs[1], pngs[2])
	if err != nil {
		t.Fatalf("compose npc walk4: %v", err)
	}
	return outs
}

// RectWH matches the test-local type described in the brief
type RectWH struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func TestEmitMetaAtlasPhaser(t *testing.T) {
	outs := atlasOuts(t, "chips", 16, 16, 4)
	meta, err := EmitMeta("atlas", "chips", []string{"phaser"}, outs)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(meta) != 1 || meta[0].Name != "chips.phaser.json" {
		t.Fatalf("files: %+v", meta)
	}
	var doc struct {
		Frames map[string]struct {
			Frame            Rect           `json:"frame"`
			Rotated          bool           `json:"rotated"`
			Trimmed          bool           `json:"trimmed"`
			SpriteSourceSize Rect           `json:"spriteSourceSize"`
			SourceSize       map[string]int `json:"sourceSize"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(meta[0].Data, &doc); err != nil {
		t.Fatal(err)
	}
	f := doc.Frames["chips-0000"]
	if f.Frame != (Rect{X: 0, Y: 0, W: 16, H: 16}) || f.Rotated || f.Trimmed {
		t.Fatalf("phaser frame: %+v", f)
	}
	if doc.Meta["image"] != "chips-sheet.png" {
		t.Fatalf("meta image: %v", doc.Meta)
	}
}

func TestEmitMetaAtlasUnityAndSparrow(t *testing.T) {
	outs := atlasOuts(t, "chips", 16, 16, 4)
	meta, err := EmitMeta("atlas", "chips", []string{"unity", "sparrow"}, outs)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	// unity: {"sprites":[{"name":...,"rect":{"x","y","width","height"},"pivot":{"x":0.5,"y":0.5}}]}
	var unity struct {
		Sprites []struct {
			Name  string             `json:"name"`
			Rect  RectWH             `json:"rect"`
			Pivot map[string]float64 `json:"pivot"`
		} `json:"sprites"`
	}
	// Find the unity output
	var unityData []byte
	var sparrowData []byte
	for _, m := range meta {
		if m.Name == "chips.frames.json" {
			unityData = m.Data
		} else if m.Name == "chips.sparrow.xml" {
			sparrowData = m.Data
		}
	}
	if unityData == nil {
		t.Fatalf("unity output not found")
	}
	if err := json.Unmarshal(unityData, &unity); err != nil {
		t.Fatal(err)
	}
	if len(unity.Sprites) != 4 {
		t.Fatalf("expected 4 sprites, got %d", len(unity.Sprites))
	}
	// Check names and pivot
	expectedNames := []string{"chips-0000", "chips-0001", "chips-0002", "chips-0003"}
	for i, s := range unity.Sprites {
		if s.Name != expectedNames[i] {
			t.Fatalf("sprite %d name: expected %s, got %s", i, expectedNames[i], s.Name)
		}
		if s.Pivot["x"] != 0.5 || s.Pivot["y"] != 0.5 {
			t.Fatalf("sprite %d pivot: expected {x:0.5, y:0.5}, got %v", i, s.Pivot)
		}
	}
	// sparrow: parse "chips.sparrow.xml" with encoding/xml:
	var atlasXML struct {
		XMLName     xml.Name `xml:"TextureAtlas"`
		ImagePath   string   `xml:"imagePath,attr"`
		SubTextures []struct {
			Name string `xml:"name,attr"`
			X    int    `xml:"x,attr"`
			Y    int    `xml:"y,attr"`
			W    int    `xml:"width,attr"`
			H    int    `xml:"height,attr"`
		} `xml:"SubTexture"`
	}
	if sparrowData == nil {
		t.Fatalf("sparrow output not found")
	}
	if err := xml.Unmarshal(sparrowData, &atlasXML); err != nil {
		t.Fatal(err)
	}
	if atlasXML.ImagePath != "chips-sheet.png" {
		t.Fatalf("expected imagePath chips-sheet.png, got %s", atlasXML.ImagePath)
	}
	if len(atlasXML.SubTextures) != 4 {
		t.Fatalf("expected 4 subtextures, got %d", len(atlasXML.SubTextures))
	}
	// Check they're in y-then-x order (row-major)
	// For a 16x16 frame with columns=0 (one row), they should be at y=0, x=0,16,32,48
	expectedPositions := [][4]int{
		{0, 0, 16, 16},  // chips-0000
		{16, 0, 16, 16}, // chips-0001
		{32, 0, 16, 16}, // chips-0002
		{48, 0, 16, 16}, // chips-0003
	}
	for i, st := range atlasXML.SubTextures {
		if st.Name != expectedNames[i] {
			t.Fatalf("subtexture %d name: expected %s, got %s", i, expectedNames[i], st.Name)
		}
		if st.X != expectedPositions[i][0] || st.Y != expectedPositions[i][1] ||
			st.W != expectedPositions[i][2] || st.H != expectedPositions[i][3] {
			t.Fatalf("subtexture %d rect: expected %v, got %d,%d,%d,%d", i, expectedPositions[i], st.X, st.Y, st.W, st.H)
		}
	}
}

func TestEmitMetaNPCUsesWalkSheet(t *testing.T) {
	outs := composeNPCWalk4Outs(t, "runner")
	meta, err := EmitMeta("npc", "runner", []string{"phaser"}, outs)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(meta) != 1 || meta[0].Name != "runner.phaser.json" {
		t.Fatalf("files: %+v", meta)
	}
	var doc struct {
		Frames map[string]struct {
			Frame            Rect           `json:"frame"`
			Rotated          bool           `json:"rotated"`
			Trimmed          bool           `json:"trimmed"`
			SpriteSourceSize Rect           `json:"spriteSourceSize"`
			SourceSize       map[string]int `json:"sourceSize"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(meta[0].Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Meta["image"] != "runner-walk.png" {
		t.Fatalf("meta image: expected runner-walk.png, got %v", doc.Meta)
	}
	// Should have 4 frames (down, left, right, up)
	if len(doc.Frames) != 4 {
		t.Fatalf("expected 4 frames, got %d", len(doc.Frames))
	}
}

func TestEmitMetaFrameRectsReadFromAtlasJSON(t *testing.T) {
	// Create an atlas where we manually craft the -atlas.json to have different
	// frame rects than what PackGrid would produce, proving EmitMeta reads from
	// the file rather than re-deriving geometry.
	name := "customframe"

	// Create two frames: one 20x16 at (0,0), another 10x16 at (0,16)
	// But we'll craft the atlas.json to say they're at different positions
	pngs := make([][]byte, 2)

	// Frame 0: 20x16 red square
	img0 := image.NewNRGBA(image.Rect(0, 0, 20, 16))
	c0 := color.NRGBA{R: 255, G: 0, B: 0, A: 255}
	for x := 0; x < 20; x++ {
		for y := 0; y < 16; y++ {
			img0.SetNRGBA(x, y, c0)
		}
	}
	buf0 := new(bytes.Buffer)
	if err := png.Encode(buf0, img0); err != nil {
		t.Fatalf("encode png0: %v", err)
	}
	pngs[0] = buf0.Bytes()

	// Frame 1: 10x16 green square
	img1 := image.NewNRGBA(image.Rect(0, 0, 10, 16))
	c1 := color.NRGBA{R: 0, G: 255, B: 0, A: 255}
	for x := 0; x < 10; x++ {
		for y := 0; y < 16; y++ {
			img1.SetNRGBA(x, y, c1)
		}
	}
	buf1 := new(bytes.Buffer)
	if err := png.Encode(buf1, img1); err != nil {
		t.Fatalf("encode png1: %v", err)
	}
	pngs[1] = buf1.Bytes()

	// Create atlas with PackGrid but then manually modify the -atlas.json
	// to have different frame positions
	outs, err := ComposeAtlas(name, pngs, 1) // columns=1
	if err != nil {
		t.Fatalf("compose atlas: %v", err)
	}

	// Modify the atlas.json to have custom frame positions
	var atlasDoc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(outs[1].Data, &atlasDoc); err != nil {
		t.Fatalf("unmarshal atlas json: %v", err)
	}

	// Change frame positions to be different from PackGrid's output
	// Frame 0: move to (5, 5) instead of (0, 0)
	// Frame 1: move to (25, 25) instead of (0, 16)
	frame0 := atlasDoc.Frames[name+"-0000"]
	frame0.Frame = Rect{X: 5, Y: 5, W: 20, H: 16}
	atlasDoc.Frames[name+"-0000"] = frame0
	frame1 := atlasDoc.Frames[name+"-0001"]
	frame1.Frame = Rect{X: 25, Y: 25, W: 10, H: 16}
	atlasDoc.Frames[name+"-0001"] = frame1

	// Re-encode the modified atlas
	modifiedAtlasJSON, err := json.Marshal(atlasDoc)
	if err != nil {
		t.Fatalf("marshal modified atlas: %v", err)
	}

	// Create new outs with the modified atlas
	customOuts := []OutputFile{
		{Name: outs[0].Name, Data: outs[0].Data},      // -sheet.png unchanged
		{Name: outs[1].Name, Data: modifiedAtlasJSON}, // -atlas.json with custom frames
	}

	// Now test EmitMeta - it should use our custom frame positions
	meta, err := EmitMeta("atlas", name, []string{"phaser"}, customOuts)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(meta) != 1 || meta[0].Name != name+".phaser.json" {
		t.Fatalf("files: %+v", meta)
	}
	var resultDoc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(meta[0].Data, &resultDoc); err != nil {
		t.Fatal(err)
	}

	// Verify that the frame rects match what we put in the atlas.json, not PackGrid's defaults
	f0 := resultDoc.Frames[name+"-0000"]
	if f0.Frame != (Rect{X: 5, Y: 5, W: 20, H: 16}) {
		t.Fatalf("frame0 rect: expected {5,5,20,16} from atlas.json, got %+v", f0.Frame)
	}
	f1 := resultDoc.Frames[name+"-0001"]
	if f1.Frame != (Rect{X: 25, Y: 25, W: 10, H: 16}) {
		t.Fatalf("frame1 rect: expected {25,25,10,16} from atlas.json, got %+v", f1.Frame)
	}
}

// TestEmitMetaGoldenByteExact compares emitter output against golden fixtures.
// Run with -update flag to regenerate golden files.
var updateGolden = flag.Bool("update", false, "update golden files")

func TestEmitMetaGoldenByteExact(t *testing.T) {
	// Test cases: 4-frame atlas (16x16 frames, columns 2) × {phaser, unity, sparrow}
	// and one npc × {phaser}

	testCases := []struct {
		kind    string
		name    string
		formats []string
		w, h    int
		frames  int
		columns int
		suffix  string
	}{
		// 4-frame atlas, columns=2
		{"atlas", "atlas4col2", []string{"phaser"}, 16, 16, 4, 2, ".phaser.json"},
		{"atlas", "atlas4col2", []string{"unity"}, 16, 16, 4, 2, ".frames.json"},
		{"atlas", "atlas4col2", []string{"sparrow"}, 16, 16, 4, 2, ".sparrow.xml"},
		// 1-frame npc (for simplicity in golden test)
		{"npc", "npc1frame", []string{"phaser"}, 16, 16, 1, 1, ".phaser.json"},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s_%s_%v", tc.kind, tc.name, tc.formats), func(t *testing.T) {
			var outs []OutputFile
			var err error

			if tc.kind == "atlas" {
				// Create frames for atlas
				pngs := make([][]byte, tc.frames)
				for i := 0; i < tc.frames; i++ {
					img := image.NewNRGBA(image.Rect(0, 0, tc.w, tc.h))
					// Fill with distinct colors
					c := color.NRGBA{R: uint8(i * 50), G: uint8(i * 30), B: uint8(i * 100), A: 255}
					for x := 0; x < tc.w; x++ {
						for y := 0; y < tc.h; y++ {
							img.SetNRGBA(x, y, c)
						}
					}
					buf := new(bytes.Buffer)
					if err := png.Encode(buf, img); err != nil {
						t.Fatalf("encode png: %v", err)
					}
					pngs[i] = buf.Bytes()
				}
				outs, err = ComposeAtlas(tc.name, pngs, tc.columns)
				if err != nil {
					t.Fatalf("compose atlas: %v", err)
				}
			} else if tc.kind == "npc" {
				// Create frames for npc (just one frame for simplicity)
				pngs := make([][]byte, tc.frames)
				for i := 0; i < tc.frames; i++ {
					img := image.NewNRGBA(image.Rect(0, 0, tc.w, tc.h))
					// Fill with distinct color
					c := color.NRGBA{R: 255, G: 0, B: 0, A: 255}
					for x := 0; x < tc.w; x++ {
						for y := 0; y < tc.h; y++ {
							img.SetNRGBA(x, y, c)
						}
					}
					buf := new(bytes.Buffer)
					if err := png.Encode(buf, img); err != nil {
						t.Fatalf("encode png: %v", err)
					}
					pngs[i] = buf.Bytes()
				}
				outs, err = ComposeNPCWalk4(tc.name, pngs[0], pngs[0], pngs[0]) // same frame 3x
				if err != nil {
					t.Fatalf("compose npc walk4: %v", err)
				}
			}

			meta, err := EmitMeta(tc.kind, tc.name, tc.formats, outs)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}

			// Find the expected output file
			var goldenData []byte
			for _, m := range meta {
				if strings.HasSuffix(m.Name, tc.suffix) {
					goldenData = m.Data
					break
				}
			}
			if goldenData == nil {
				t.Fatalf("expected output file with suffix %s not found", tc.suffix)
			}

			// Construct golden file path
			goldenDir := filepath.Join("testdata", "golden")
			goldenFile := filepath.Join(goldenDir, tc.name+tc.suffix)

			if *updateGolden {
				// Update mode: write golden file
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatalf("create golden dir: %v", err)
				}
				if err := os.WriteFile(goldenFile, goldenData, 0o644); err != nil {
					t.Fatalf("write golden file: %v", err)
				}
				t.Logf("updated golden file: %s", goldenFile)
			} else {
				// Verify mode: compare against golden file
				expected, err := os.ReadFile(goldenFile)
				if err != nil {
					t.Fatalf("read golden file %s: %v", goldenFile, err)
				}
				if !bytes.Equal(goldenData, expected) {
					t.Fatalf("golden file mismatch for %s\nGot:\n%s\n\nWant:\n%s\n",
						tc.name+tc.suffix, string(goldenData), string(expected))
				}
			}
		})
	}
}

// findData returns the data of the output file with the exact name, or nil.
func findData(files []OutputFile, name string) []byte {
	for _, of := range files {
		if of.Name == name {
			return of.Data
		}
	}
	return nil
}

// tsxDoc mirrors the subset of Tiled's .tsx tileset XML the kit emitter
// writes; used to parse tsx output in the tests below.
type tsxDoc struct {
	XMLName   xml.Name `xml:"tileset"`
	Name      string   `xml:"name,attr"`
	TileCount int      `xml:"tilecount,attr"`
	Columns   int      `xml:"columns,attr"`
	Image     struct {
		Source string `xml:"source,attr"`
		W      int    `xml:"width,attr"`
		H      int    `xml:"height,attr"`
	} `xml:"image"`
	Wangset struct {
		Name       string `xml:"name,attr"`
		Type       string `xml:"type,attr"`
		EdgeColors []struct {
			Color string `xml:"color,attr"`
		} `xml:"wangedgecolors>color"`
		WangTiles []struct {
			TileID int    `xml:"tileid,attr"`
			WangID string `xml:"wangid,attr"`
		} `xml:"wangtiles>wangtile"`
	} `xml:"wangset"`
}

func tsxParse(b []byte) (tsxDoc, error) {
	var d tsxDoc
	err := xml.Unmarshal(b, &d)
	return d, err
}

// kitClassImages encodes one deterministic 16x16 flat png per kit class, in
// KitClasses order.
func kitClassImages(t *testing.T) [][]byte {
	images := make([][]byte, len(KitClasses))
	for i := range KitClasses {
		c := color.RGBA{R: uint8(i * 50), G: uint8(i * 30), B: uint8(i * 100), A: 255}
		images[i] = pngBytes(t, solid(16, 16, c))
	}
	return images
}

func TestEmitMetaKitTiled(t *testing.T) {
	images := kitClassImages(t)
	outs, err := ComposeKit("circuit", images)
	if err != nil {
		t.Fatalf("kit: %v", err)
	}
	meta, err := EmitMeta("kit", "circuit", []string{"tiled"}, outs)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(meta) != 2 {
		t.Fatalf("want tsx + wangset png, got %d files", len(meta))
	}
	var pngName, tsxName string
	for _, of := range meta {
		switch {
		case strings.HasSuffix(of.Name, ".tsx"):
			tsxName = of.Name
		case strings.HasSuffix(of.Name, ".png"):
			pngName = of.Name
		}
	}
	if tsxName != "circuit.tsx" || pngName != "circuit-wangset.png" {
		t.Fatalf("names: %s %s", tsxName, pngName)
	}
	// The wangset png must be a real png of 4 cols x 5 rows of 16px = 64x80.
	im, err := png.Decode(bytes.NewReader(findData(meta, pngName)))
	if err != nil {
		t.Fatal(err)
	}
	if im.Bounds().Dx() != 64 || im.Bounds().Dy() != 80 {
		t.Fatalf("wangset png %v", im.Bounds())
	}
	// tsx parses: tilecount 20, columns 4, image source + dims, and one
	// <wangtile> per class whose tileid ordering matches KitClasses[0..19]
	// laid out row-major into the 4-col sheet.
	doc, err := tsxParse(findData(meta, tsxName))
	if err != nil {
		t.Fatal(err)
	}
	if doc.TileCount != 20 || doc.Columns != 4 {
		t.Fatalf("tiles: %+v", doc)
	}
	if doc.Image.Source != "circuit-wangset.png" || doc.Image.W != 64 || doc.Image.H != 80 {
		t.Fatalf("image: %+v", doc.Image)
	}
	if len(doc.Wangset.WangTiles) != 20 {
		t.Fatalf("wangtiles: %d", len(doc.Wangset.WangTiles))
	}
	for i, wt := range doc.Wangset.WangTiles {
		if wt.TileID != i {
			t.Fatalf("wangtile %d tileid: want %d got %d", i, i, wt.TileID)
		}
	}
	// spot-check edge-id table (wang.go Hints are the semantic reference;
	// wangid values are Tiled's native 8-element [TL,T,TR,L,R,BL,B,BR] lists
	// with edges at 1=N, 3=W, 4=E, 6=S): line-h -> [0,1,0,1],
	// line-v -> [1,0,1,0], corner-out-nw -> [1,0,0,1], t-e -> [1,1,1,0]
	want := map[string]string{
		"line-h":        "0,0,0,1,1,0,0,0",
		"line-v":        "0,1,0,0,0,0,1,0",
		"corner-out-nw": "0,1,0,1,0,0,0,0",
		"t-e":           "0,1,0,0,1,0,1,0",
	}
	for i, kc := range KitClasses {
		if w, ok := want[kc.Key]; ok && doc.Wangset.WangTiles[i].WangID != w {
			t.Fatalf("wangid for %s: want %s got %s", kc.Key, w, doc.Wangset.WangTiles[i].WangID)
		}
	}
}

func TestEmitMetaKitTiledNegative(t *testing.T) {
	// atlas outs have no per-class files: the first missing class must name
	// itself in the error (honest failure, not a silent skip).
	outs := atlasOuts(t, "chips", 16, 16, 4)
	_, err := EmitMeta("kit", "chips", []string{"tiled"}, outs)
	if err == nil {
		t.Fatal("expected error for missing kit class file")
	}
	if !strings.Contains(err.Error(), "kit class file chips-line-h.png missing") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestKitTiledGoldenByteExact(t *testing.T) {
	// circuit.tsx and circuit-wangset.png bytes equal testdata/golden/…
	// (regen via -update; the tsx is deterministic — fixed 20-class order,
	// fixed wangid derivation, fixed color hexes — and png.Encode of the
	// same PackGrid output is byte-stable). Reuses the same -update flag as
	// TestEmitMetaGoldenByteExact.
	outs, err := ComposeKit("circuit", kitClassImages(t))
	if err != nil {
		t.Fatalf("kit: %v", err)
	}
	meta, err := EmitMeta("kit", "circuit", []string{"tiled"}, outs)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	pairs := []struct{ out, golden string }{
		{"circuit.tsx", filepath.Join("testdata", "golden", "circuit.tsx")},
		{"circuit-wangset.png", filepath.Join("testdata", "golden", "circuit-wangset.png")},
	}
	for _, p := range pairs {
		data := findData(meta, p.out)
		if data == nil {
			t.Fatalf("meta output %s missing", p.out)
		}
		if *updateGolden {
			if err := os.WriteFile(p.golden, data, 0o644); err != nil {
				t.Fatalf("write golden %s: %v", p.golden, err)
			}
			t.Logf("updated golden file: %s", p.golden)
			continue
		}
		expected, err := os.ReadFile(p.golden)
		if err != nil {
			t.Fatalf("read golden file %s: %v (regen with -update)", p.golden, err)
		}
		if !bytes.Equal(data, expected) {
			t.Fatalf("golden file mismatch for %s (regen with -update)", p.out)
		}
	}
}

func TestWangIDForClassCoversAllKitClasses(t *testing.T) {
	// Every KitClasses key must map (brief: "every key must be covered"); an
	// unmapped class would surface as a named error at emit time.
	for _, kc := range KitClasses {
		id, err := wangIDForClass(kc.Key)
		if err != nil {
			t.Fatalf("class %q: %v", kc.Key, err)
		}
		if id[0] < 0 || id[0] > 1 || id[1] < 0 || id[1] > 1 || id[2] < 0 || id[2] > 1 || id[3] < 0 || id[3] > 1 {
			t.Fatalf("class %q wangid out of range: %v", kc.Key, id)
		}
	}
	// semantic spot-checks beyond TestEmitMetaKitTiled's four
	want := map[string][4]int{
		"cross":        {1, 1, 1, 1},
		"single":       {0, 0, 0, 0},
		"cap-e":        {0, 1, 0, 0},
		"cap-w":        {0, 0, 0, 1},
		"cap-n":        {1, 0, 0, 0},
		"cap-s":        {0, 0, 1, 0},
		"t-w":          {1, 0, 1, 1},
		"t-n":          {1, 1, 0, 1},
		"t-s":          {0, 1, 1, 1},
		"corner-in-se": {0, 1, 1, 0}, // edge-color-only: joins S+E like corner-out-se
	}
	for key, wantID := range want {
		id, err := wangIDForClass(key)
		if err != nil {
			t.Fatalf("class %q: %v", key, err)
		}
		if id != wantID {
			t.Fatalf("class %q: want %v got %v", key, wantID, id)
		}
	}
	// unknown or malformed keys are named errors, never zero ids
	for _, key := range []string{"bogus", "empty", "t-x", "cap-x", "cap-toolong", "corner-out-xx"} {
		if _, err := wangIDForClass(key); err == nil {
			t.Fatalf("class %q: want error for unmapped key", key)
		}
	}
	// the Tiled lift produces 8-element wangid lists: corners (0,2,5,7) zero,
	// edge indices carrying the tuple exactly (1=N, 3=W, 4=E, 6=S)
	for _, kc := range KitClasses {
		id, err := wangIDForClass(kc.Key)
		if err != nil {
			t.Fatalf("class %q: %v", kc.Key, err)
		}
		w := tiledWangID(id)
		for _, ci := range []int{0, 2, 5, 7} {
			if w[ci] != 0 {
				t.Fatalf("class %q: corner index %d must stay 0, got %d", kc.Key, ci, w[ci])
			}
		}
		if w[1] != id[0] || w[4] != id[1] || w[6] != id[2] || w[3] != id[3] {
			t.Fatalf("class %q: lift mismatch edges %v -> %v", kc.Key, id, w)
		}
	}
	// representative edge-index layout checks: t-e (stem E) paints top+right+
	// bottom; cap-n paints only top; line-h paints left+right; cross paints
	// all four edge indices
	layout := map[string][8]int{
		"t-e":    {0, 1, 0, 0, 1, 0, 1, 0},
		"cap-n":  {0, 1, 0, 0, 0, 0, 0, 0},
		"line-h": {0, 0, 0, 1, 1, 0, 0, 0},
		"cross":  {0, 1, 0, 1, 1, 0, 1, 0},
	}
	for key, want := range layout {
		id, err := wangIDForClass(key)
		if err != nil {
			t.Fatalf("class %q: %v", key, err)
		}
		if got := tiledWangID(id); got != want {
			t.Fatalf("class %q: want %v got %v", key, want, got)
		}
	}
}

func TestEmitMetaRejectsAndIsStable(t *testing.T) {
	outs := atlasOuts(t, "chips", 16, 16, 4)
	// - unknown format "phaza" error
	_, err := EmitMeta("atlas", "chips", []string{"phaza"}, outs)
	if err == nil {
		t.Fatalf("expected error for unknown format")
	}
	if !strings.Contains(err.Error(), "meta format \"phaza\" unknown") {
		t.Fatalf("unexpected error: %v", err)
	}
	// - kind "element" error
	_, err = EmitMeta("element", "chips", []string{"phaser"}, outs)
	if err == nil {
		t.Fatalf("expected error for invalid kind")
	}
	if !strings.Contains(err.Error(), "does not apply to kind") {
		t.Fatalf("unexpected error: %v", err)
	}
	// - empty formats returns nil outs, nil err
	meta, err := EmitMeta("atlas", "chips", []string{}, outs)
	if err != nil {
		t.Fatalf("expected no error for empty formats: %v", err)
	}
	if meta != nil {
		t.Fatalf("expected nil meta for empty formats, got %v", meta)
	}
	// - atlas outs with no -atlas.json file -> error "atlas metadata missing"
	noAtlas := []OutputFile{{Name: "chips-sheet.png", Data: []byte{}}}
	_, err = EmitMeta("atlas", "chips", []string{"phaser"}, noAtlas)
	if err == nil {
		t.Fatalf("expected error for missing atlas metadata")
	}
	if !strings.Contains(err.Error(), "atlas metadata missing") {
		t.Fatalf("unexpected error: %v", err)
	}
}
