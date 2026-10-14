package queue

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"img-gen/internal/compose"
	"img-gen/internal/lattice"
	"img-gen/internal/models"
	"img-gen/internal/prompting"
	"img-gen/internal/storage"
)

// genCapture records every Generate call the dispatch path made.
type genCapture struct {
	mu      sync.Mutex
	Prompts []string
	Sizes   []string
	Seeds   []int64
	Images  [][]byte
}

func (c *genCapture) add(prompt, size string, seed *int64, img []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Prompts = append(c.Prompts, prompt)
	c.Sizes = append(c.Sizes, size)
	s := int64(-1)
	if seed != nil {
		s = *seed
	}
	c.Seeds = append(c.Seeds, s)
	c.Images = append(c.Images, img)
}

// composeTestOpts returns Options whose Generate returns a real 8x8 PNG (the
// parent composes from child bytes, so "PNG" byte strings won't do). Delivery
// is unconditional on compose success, so the helper pins a scratch ExportDir
// too — otherwise unset-dest tests would write into the real
// ~/Downloads/img-gen fallback chain.
func composeTestOpts(t *testing.T, dir string, cap *genCapture) Options {
	t.Helper()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Genres:    testCatalog(),
		Store:     s,
		ExportDir: filepath.Join(t.TempDir(), "exports"),
		Ops: ImageOps{
			Generate: func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
				img := image.NewRGBA(image.Rect(0, 0, 8, 8))
				var buf bytes.Buffer
				if err := png.Encode(&buf, img); err != nil {
					t.Fatal(err)
				}
				cap.add(prompt, size, sp.Seed, buf.Bytes())
				return buf.Bytes(), sp.Seed, nil
			},
		},
		Chat: func(ctx context.Context, msgs []prompting.Message) (string, error) { return "enhanced", nil },
	}
}

// sizedGen returns an ImageOps.Generate that renders a fully opaque red/green
// checker PNG at the requested size (parsing WxH), recording in cap. Real
// sizes matter: post-processing like the frame punch validates render
// dimensions, so the 8x8 default fake is too small.
func sizedGen(t *testing.T, cap *genCapture) func(context.Context, string, string, *storage.ControlParams, *storage.IPAdapterParams, storage.ModelSpec, storage.SamplingParams) ([]byte, *int64, error) {
	t.Helper()
	return func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		ws, hs, _ := strings.Cut(size, "x")
		w, _ := strconv.Atoi(ws)
		h, _ := strconv.Atoi(hs)
		if w < 1 || h < 1 {
			w, h = 8, 8
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if (x/4+y/4)%2 == 0 {
					img.Set(x, y, color.RGBA{R: 255, A: 255})
				} else {
					img.Set(x, y, color.RGBA{G: 255, A: 255})
				}
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if cap != nil {
			cap.add(prompt, size, sp.Seed, buf.Bytes())
		}
		return buf.Bytes(), sp.Seed, nil
	}
}

func TestSubmitAssetSetPaletteLock(t *testing.T) {
	o := composeTestOpts(t, t.TempDir(), nil)
	o.Ops.Generate = sizedGen(t, nil)
	m := New(o)
	seed := int64(41)
	// The remap hook covers every kind: two elements plus an atlas, whose
	// sheet deliverable must land inside the palette too.
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "cohere", Seed: &seed, PaletteLock: true,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "a", Prompt: "first", Size: "16x16"},
			{Kind: "element", Name: "b", Prompt: "second", Size: "16x16"},
			{Kind: "atlas", Name: "sheet", Prompt: "sheet anim", Size: "16x16", Frames: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	raw, err := m.opts.Store.ReadSetFile(id, "cohere.palette.json")
	if err != nil {
		t.Fatalf("palette file: %v", err)
	}
	var pal struct {
		Colors []string `json:"colors"`
	}
	if err := json.Unmarshal(raw, &pal); err != nil {
		t.Fatal(err)
	}
	if len(pal.Colors) == 0 {
		t.Fatal("palette file has no colors")
	}
	for _, hx := range pal.Colors {
		if !strings.HasPrefix(hx, "#") || len(hx) != 7 {
			t.Fatalf("bad palette hex %q", hx)
		}
	}

	// Every opaque pixel of every PNG output lands inside the palette,
	// including the atlas sheet.
	allowed := map[string]bool{}
	for _, hx := range pal.Colors {
		allowed[strings.ToLower(hx)] = true
	}
	for _, name := range []string{"a.png", "b.png", "sheet-sheet.png"} {
		data, err := m.opts.Store.ReadSetFile(id, name)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				n := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				if n.A < 128 {
					continue
				}
				hx := fmt.Sprintf("#%02x%02x%02x", n.R, n.G, n.B)
				if !allowed[hx] {
					t.Fatalf("%s pixel (%d,%d) = %s is outside the set palette", name, x, y, hx)
				}
			}
		}
	}
}

func TestAssetChildSubmit(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(7)
	id, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "steel bolt icon", Size: "256x256", Seed: &seed})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 1 || cap.Prompts[0] != "steel bolt icon" || cap.Sizes[0] != "256x256" || cap.Seeds[0] != 7 {
		t.Fatalf("dispatch saw: prompts=%q sizes=%q seeds=%q", cap.Prompts, cap.Sizes, cap.Seeds)
	}
	cap.mu.Unlock()
}

func TestAssetChildValidation(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	if _, err := m.Submit(SubmitRequest{Mode: "asset", Size: "256x256"}); err == nil {
		t.Fatal("want prompt-required error")
	}
	if _, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "x", Size: "250x250"}); err == nil {
		t.Fatal("want 16-multiple error for 250x250")
	}
	if _, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "x", Size: "bogus"}); err == nil {
		t.Fatal("want invalid size error")
	}
}

func TestComposeParentHandoff(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	spec := composeSpec{Kind: "asset-set", Name: "demo", Assets: []assetSpec{
		{Kind: "element", Name: "a", Prompt: "a", Size: "16x16", Children: 1},
		{Kind: "element", Name: "b", Prompt: "b", Size: "16x16", Children: 1},
	}}
	kids := []SubmitRequest{
		{Mode: "asset", Prompt: "a", Size: "16x16"},
		{Mode: "asset", Prompt: "b", Size: "16x16"},
	}
	parentID, err := m.submitComposeParent(newID(), spec, kids)
	if err != nil {
		t.Fatal(err)
	}

	// Task 8's executor composes the two opaque elements: the parent's set
	// holds one PNG per element plus the manifest.
	waitFor(t, m, parentID, "done")
	j, _ := m.Get(parentID)
	if len(j.Files) != 2 || j.Files[0].Name != "a.png" || !j.Files[0].Primary || j.Files[1].Name != "b.png" || !j.Files[1].Primary {
		t.Fatalf("files = %+v", j.Files)
	}
	for _, name := range []string{"a.png", "b.png", "manifest.json"} {
		if _, err := m.opts.Store.ReadSetFile(parentID, name); err != nil {
			t.Fatalf("set file %s: %v", name, err)
		}
	}
	if len(j.Children) != 2 {
		t.Fatalf("children = %d, want 2", len(j.Children))
	}

	// Exactly the two children went through Ops.Generate: the parent
	// bypasses dispatch (run() short-circuits mode "compose" to runCompose).
	cap.mu.Lock()
	n := len(cap.Prompts)
	cap.mu.Unlock()
	if n != 2 {
		t.Fatalf("dispatches = %d, want 2", n)
	}
}

func TestComposeParentCancel(t *testing.T) {
	cap := &genCapture{}
	// Slow the children so Cancel lands while they are running; with instant
	// stub children the parent could go terminal before Cancel executes and
	// the cancel would race.
	opts := composeTestOpts(t, t.TempDir(), cap)
	base := opts.Ops.Generate
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		time.Sleep(100 * time.Millisecond)
		return base(ctx, prompt, size, nil, nil, spec, sp)
	}
	m := New(opts)
	spec := composeSpec{Kind: "gif", GIF: &gifPayload{DelayMS: 100, Loop: 0, Formats: []string{"gif"}}}
	kids := []SubmitRequest{
		{Mode: "asset", Prompt: "a", Size: "16x16"},
		{Mode: "asset", Prompt: "b", Size: "16x16"},
	}
	parentID, err := m.submitComposeParent(newID(), spec, kids)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(parentID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, parentID, "cancelled")
}

func TestComposeParentRestore(t *testing.T) {
	cap := &genCapture{}
	opts := composeTestOpts(t, t.TempDir(), cap)
	m := New(opts)
	// Seed the store's history with a mid-flight compose parent and its
	// already-done child, exactly as a restart would find them: the parent is
	// non-terminal with no GenID (compose parents are not sidecar jobs), so
	// restore's first pass must leave it alone and the second pass must
	// re-enqueue it.
	jparent := storage.Job{
		ID:     "p1",
		Mode:   "compose",
		Status: "queued",
		SetID:  "p1",
		Prompt: "compose asset-set",
		ComposeSpec: `{"kind":"asset-set","name":"demo",` +
			`"assets":[{"kind":"element","name":"a","prompt":"a","size":"16x16","children":1}]}`,
	}
	jchild := storage.Job{ID: "c1", Mode: "asset", Status: "done", ComposeParent: "p1"}
	if err := m.opts.Store.AppendHistory(jparent); err != nil {
		t.Fatal(err)
	}
	if err := m.opts.Store.AppendHistory(jchild); err != nil {
		t.Fatal(err)
	}
	m.restore()
	// The parent reaching "failed" with a compose executor error proves
	// restore RE-ENQUEUED it (runCompose ran the real composeAssetSet): the
	// first pass would instead have failed it with "interrupted by a service
	// restart…". The synthetic done child has no stored image, so the honest
	// recompose fails on its child result.
	j := waitFor(t, m, "p1", "failed")
	if !strings.Contains(j.Error, "compose_error") || strings.Contains(j.Error, "interrupted") {
		t.Fatalf("parent error = %q", j.Error)
	}
}

// testPNG is a real 8x8 PNG: compose parents decode child bytes, so stubs
// must return decodable images.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// failCapture extends genCapture with call-indexed failure injection:
// FailCall >= 0 fails exactly that (0-based) Generate call; FailCall < 0
// with FailAll fails every call.
type failCapture struct {
	genCapture
	FailCall int
	FailAll  bool
	n        int
}

func composeTestOptsFail(t *testing.T, dir string, cap *failCapture) Options {
	t.Helper()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Genres:    testCatalog(),
		Store:     s,
		ExportDir: filepath.Join(t.TempDir(), "exports"),
		Ops: ImageOps{
			Generate: func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
				cap.mu.Lock()
				call := cap.n
				cap.n++
				fail := cap.FailAll || (cap.FailCall >= 0 && call == cap.FailCall)
				cap.mu.Unlock()
				if fail {
					return nil, nil, fmt.Errorf("sidecar boom")
				}
				img := testPNG(t)
				cap.add(prompt, size, sp.Seed, img)
				return img, sp.Seed, nil
			},
		},
		Chat: func(ctx context.Context, msgs []prompting.Message) (string, error) { return "enhanced", nil },
	}
}

func TestSubmitAssetSetElement(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(42)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "bolt", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "bolt", Prompt: "hex bolt", Size: "64x64"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 1 || cap.Prompts[0] != "hex bolt" || cap.Seeds[0] != 42 {
		t.Fatalf("dispatch: %q %q", cap.Prompts, cap.Seeds)
	}
	cap.mu.Unlock()
	data, err := m.opts.Store.ReadSetFile(id, "bolt.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("bolt.png not a PNG: %v", err)
	}
	j, _ := m.Get(id)
	if len(j.Files) != 1 || j.Files[0].Name != "bolt.png" || !j.Files[0].Primary {
		t.Fatalf("files: %+v", j.Files)
	}
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.Kind != "asset-set" || len(man.Assets) != 1 || man.Assets[0].Name != "bolt" || man.Assets[0].Error != "" {
		t.Fatalf("manifest: %+v", man)
	}
	if len(man.Assets[0].Files) != 1 || man.Assets[0].Files[0] != "bolt.png" {
		t.Fatalf("manifest files: %+v", man.Assets[0].Files)
	}
	if j.ImagePath != "sets/"+id+"/bolt.png" {
		t.Fatalf("image path %q", j.ImagePath)
	}
}

func TestComposedOutputsCarryTextParams(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(7)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "txt-elem", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "b", Prompt: "a button", Size: "16x16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	raw, err := m.opts.Store.ReadSetFile(id, "b.png")
	if err != nil {
		t.Fatal(err)
	}
	// ScanTextChunks comes from Task 1 (compose package); it fully
	// per-chunk-verifies CRCs, so it doubles as a structural PNG check here.
	got, err := compose.ScanTextChunks(raw)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got["software"] != "img-gen" || got["kind"] != "element" || got["name"] != "b" || got["set"] != id {
		t.Fatalf("unexpected params: %v", got)
	}
}

func TestSubmitAssetSetTransparent(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(7)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "star", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "star", Prompt: "star icon", Size: "32x32", Transparent: TransparentDual}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 2 {
		t.Fatalf("want 2 children (white+black), got %q", cap.Prompts)
	}
	if cap.Seeds[0] != 7 || cap.Seeds[1] != 7 {
		t.Fatalf("backgrounds share the seed: %q", cap.Seeds)
	}
	if !strings.Contains(cap.Prompts[0], "white") || !strings.Contains(cap.Prompts[1], "black") {
		t.Fatalf("bg suffixes missing: %q", cap.Prompts)
	}
	cap.mu.Unlock()
	data, err := m.opts.Store.ReadSetFile(id, "star.png")
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// The stub's white and black renders are identical transparent black, so
	// UnmixAlpha takes the ill-conditioned fallback (dr=dg=db=0 → a=255,
	// F=black): the output is opaque black. The stdlib PNG writer upgrades a
	// fully-opaque NRGBA to truecolor, so decode may return *image.RGBA —
	// assert pixels, not the concrete decode type.
	r, g, b, a := im.At(0, 0).RGBA()
	if r != 0 || g != 0 || b != 0 || a != 0xffff {
		t.Fatalf("unmixed output should be opaque black, got %d %d %d %d", r, g, b, a)
	}
}

func TestSubmitAssetSetKit(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(100)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "pipes", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "kit", Name: "pipes", Prompt: "blue pipes", Size: "64x64"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != len(compose.KitClasses) {
		t.Fatalf("want %d children, got %d", len(compose.KitClasses), len(cap.Prompts))
	}
	for i := range compose.KitClasses {
		if want := seed + int64(compose.KitClasses[i].SeedOffset); cap.Seeds[i] != want {
			t.Fatalf("child %d seed %d, want %d", i, cap.Seeds[i], want)
		}
		if !strings.Contains(cap.Prompts[i], compose.KitClasses[i].Hint) {
			t.Fatalf("child %d prompt %q lacks hint %q", i, cap.Prompts[i], compose.KitClasses[i].Hint)
		}
	}
	cap.mu.Unlock()
	j, _ := m.Get(id)
	if len(j.Files) != len(compose.KitClasses) {
		t.Fatalf("kit files: %d", len(j.Files))
	}
	// Primary is class index 0.
	if j.Files[0].Name != "pipes-line-h.png" || !j.Files[0].Primary {
		t.Fatalf("kit primary: %+v", j.Files[0])
	}
	for _, f := range j.Files {
		if _, err := m.opts.Store.ReadSetFile(id, f.Name); err != nil {
			t.Fatalf("kit file %s: %v", f.Name, err)
		}
	}
}

func TestSubmitAssetSetAtlas(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(5)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "walk", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "atlas", Name: "walk", Prompt: "robot walk", Size: "64x64", Frames: 3, Columns: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 3 || cap.Seeds[0] != 5 || cap.Seeds[2] != 7 {
		t.Fatalf("atlas children: %q %q", cap.Prompts, cap.Seeds)
	}
	cap.mu.Unlock()
	j, _ := m.Get(id)
	var names []string
	for _, f := range j.Files {
		names = append(names, f.Name)
	}
	if len(names) != 2 || names[0] != "walk-sheet.png" || names[1] != "walk-atlas.json" {
		t.Fatalf("atlas files: %q", names)
	}
	araw, err := m.opts.Store.ReadSetFile(id, "walk-atlas.json")
	if err != nil {
		t.Fatal(err)
	}
	var atlas struct {
		Frames map[string]struct {
			Frame struct {
				X int `json:"x"`
				Y int `json:"y"`
				W int `json:"w"`
				H int `json:"h"`
			} `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(araw, &atlas); err != nil {
		t.Fatal(err)
	}
	if len(atlas.Frames) != 3 {
		t.Fatalf("atlas frames: %d", len(atlas.Frames))
	}
}

func TestAssetSetEmitsMetaFiles(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(19)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "walk", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "atlas", Name: "walk", Prompt: "robot walk", Size: "32x32", Frames: 3, Columns: 0, MetaFormats: []string{"phaser"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("set: %s %q", j.Status, j.Error)
	}

	// The phaser JSON is a real set file: 3 frames, image ref on the sheet.
	raw, err := m.opts.Store.ReadSetFile(id, "walk.phaser.json")
	if err != nil {
		t.Fatalf("phaser meta: %v", err)
	}
	var ph struct {
		Frames map[string]struct {
			Frame compose.Rect `json:"frame"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(raw, &ph); err != nil {
		t.Fatalf("phaser JSON: %v", err)
	}
	if len(ph.Frames) != 3 {
		t.Fatalf("phaser frames: %d", len(ph.Frames))
	}
	if ph.Meta["image"] != "walk-sheet.png" {
		t.Fatalf("phaser meta.image = %q", ph.Meta["image"])
	}

	// GET /api/v1/sets/<id> serves job.Files (v1.go serveSet) and set read
	// endpoints list per-asset files from the manifest — assert both,
	// identically to how TestSubmitAssetSetAtlas asserts atlas.json.
	found := false
	for _, f := range j.Files {
		if f.Name == "walk.phaser.json" {
			found = true
		}
	}
	if !found {
		t.Fatalf("job files missing meta deliverable: %+v", j.Files)
	}
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	mfiles := man.Assets[0].Files
	if len(mfiles) != 3 || mfiles[2] != "walk.phaser.json" {
		t.Fatalf("manifest files: %q", mfiles)
	}

	// The persisted spec carries meta_formats, so a mid-render restart
	// rebuilds the same set (same field on assetSpec and AssetSpecIn).
	var spec composeSpec
	if err := json.Unmarshal([]byte(j.ComposeSpec), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Assets) != 1 || len(spec.Assets[0].MetaFormats) != 1 || spec.Assets[0].MetaFormats[0] != "phaser" {
		t.Fatalf("persisted meta_formats: %+v", spec.Assets)
	}
}

func TestAssetSetEmitsKitTiled(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "pipes", Seed: ptr64(71),
		Assets: []AssetSpecIn{{Kind: "kit", Name: "pipes", Prompt: "blue pipes", Size: "64x64", MetaFormats: []string{"tiled"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("set: %s %q", j.Status, j.Error)
	}
	// tsx + assembled wangset sheet persist under names that pass the set
	// file rule unchanged (no new escaping).
	for _, name := range []string{"pipes.tsx", "pipes-wangset.png"} {
		if _, err := m.opts.Store.ReadSetFile(id, name); err != nil {
			t.Fatalf("kit meta %s: %v", name, err)
		}
	}
	n := 0
	for _, f := range j.Files {
		if f.Name == "pipes.tsx" || f.Name == "pipes-wangset.png" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("job files: %+v", j.Files)
	}
	wraw, err := m.opts.Store.ReadSetFile(id, "pipes-wangset.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(wraw)); err != nil {
		t.Fatalf("wangset sheet: %v", err)
	}
	txt, err := m.opts.Store.ReadSetFile(id, "pipes.tsx")
	if err != nil {
		t.Fatal(err)
	}
	// Full tsx shape: one tileset per 20-kit, one wangtile per class.
	for _, want := range []string{`tilecount="20"`, `<wangset name="pipes"`} {
		if !strings.Contains(string(txt), want) {
			t.Fatalf("pipes.tsx missing %q", want)
		}
	}
	if got := strings.Count(string(txt), "<wangtile "); got != len(compose.KitClasses) {
		t.Fatalf("wangtiles: %d, want %d", got, len(compose.KitClasses))
	}
}

func TestAssetSetEmitsNPCVariantsMeta(t *testing.T) {
	// Variants npcs compose via the atlas path (-sheet.png), not walk4
	// (-walk.png): the meta emitters must find the sheet under either name,
	// and the metadata must reference it as <name>-sheet.png.
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(43)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "npcm", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "npc", Name: "villager", Prompt: "villager sprite", Size: "32x32",
			Shape: "variants", Variants: []string{"idle", "wave", "angry"}, Columns: 0, MetaFormats: []string{"phaser"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("set: %s %q", j.Status, j.Error)
	}
	raw, err := m.opts.Store.ReadSetFile(id, "villager.phaser.json")
	if err != nil {
		t.Fatalf("phaser meta: %v", err)
	}
	var ph struct {
		Frames map[string]struct {
			Frame compose.Rect `json:"frame"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(raw, &ph); err != nil {
		t.Fatalf("phaser JSON: %v", err)
	}
	if len(ph.Frames) != 3 {
		t.Fatalf("phaser frames: %d, want 3", len(ph.Frames))
	}
	if ph.Meta["image"] != "villager-sheet.png" {
		t.Fatalf("phaser meta.image = %q, want villager-sheet.png", ph.Meta["image"])
	}
	found := false
	for _, f := range j.Files {
		if f.Name == "villager.phaser.json" {
			found = true
		}
	}
	if !found {
		t.Fatalf("job files missing meta deliverable: %+v", j.Files)
	}
}

func TestSubmitAssetSetFrameKind(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(11)

	// Validation: margins required, in 1..size/2-1.
	bad := AssetSetRequest{Name: "p", Seed: &seed, Assets: []AssetSpecIn{{Kind: "frame", Name: "p", Prompt: "stone", Size: "32x32"}}}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want frame_margins required error")
	}
	bad.Assets[0].FrameMargins = 16 // 2*16 >= 32
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want margins bound error")
	}

	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "frames", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "frame", Name: "panel", Prompt: "stone panel", Size: "32x32", FrameMargins: 8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	data, err := m.opts.Store.ReadSetFile(id, "panel.png")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// png.Decode's static type is image.Image; NRGBAAt lives on the concrete
	// type. The punched frame encodes with transparent interior pixels (color
	// type 6), so decode must yield NRGBA.
	nr, ok := img.(*image.NRGBA)
	if !ok {
		t.Fatalf("punched frame decoded as %T", img)
	}
	if nr.NRGBAAt(16, 16).A != 0 {
		t.Fatal("interior not punched")
	}
	if nr.NRGBAAt(0, 0).A != 255 {
		t.Fatal("border lost")
	}
	mraw, err := m.opts.Store.ReadSetFile(id, "panel.frame.json")
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Margins struct {
			Top    int
			Right  int
			Bottom int
			Left   int
		} `json:"margins"`
	}
	if err := json.Unmarshal(mraw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Margins.Top != 8 || meta.Margins.Bottom != 8 {
		t.Fatalf("margins = %+v", meta.Margins)
	}
}

func TestSubmitAssetSetBarKind(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(12)

	// Validation: bar_fill required, states in 1..100, <=16, ascending.
	bad := AssetSetRequest{Name: "b", Seed: &seed, Assets: []AssetSpecIn{{Kind: "bar", Name: "hb", Prompt: "track art", Size: "32x32", FrameMargins: 8}}}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want bar_fill required error")
	}
	bad.Assets[0].BarFill = "red liquid"
	bad.Assets[0].FillStates = []int{60, 30}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want ascending error")
	}
	bad.Assets[0].FillStates = []int{0, 30}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want 1..100 error")
	}

	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "bars", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "bar", Name: "health", Prompt: "wooden health bar", Size: "32x32", FrameMargins: 8, BarFill: "red liquid", FillStates: []int{30, 60}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	// Track + fill + exact state clips.
	for _, name := range []string{"health-track.png", "health-fill.png", "health-fill-030.png", "health-fill-060.png", "health.bar.json"} {
		if _, err := m.opts.Store.ReadSetFile(id, name); err != nil {
			t.Fatalf("set file %s: %v", name, err)
		}
	}
	clip, err := m.opts.Store.ReadSetFile(id, "health-fill-030.png")
	if err != nil {
		t.Fatal(err)
	}
	cimg, err := png.Decode(bytes.NewReader(clip))
	if err != nil {
		t.Fatal(err)
	}
	// Fill render is 32 wide; 30% floors to 9.
	if cimg.Bounds().Dx() != 9 || cimg.Bounds().Dy() != 32 {
		t.Fatalf("30%% clip = %dx%d, want 9x32", cimg.Bounds().Dx(), cimg.Bounds().Dy())
	}

	// Three children: track (1, opaque by default) + fill (1).
	cap.mu.Lock()
	n := len(cap.Prompts)
	cap.mu.Unlock()
	if n != 2 {
		t.Fatalf("children = %d, want 2", n)
	}
}

func TestSubmitAssetSetNPCVariants(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(41)

	bad := AssetSetRequest{Name: "n", Seed: &seed, Assets: []AssetSpecIn{{Kind: "npc", Name: "vg", Prompt: "villager", Size: "32x32"}}}
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want npc shape required error")
	}
	bad.Assets[0].Shape = "variants"
	if _, err := m.SubmitAssetSet(bad); err == nil {
		t.Fatal("want variants 2..64 error")
	}

	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "npc", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "npc", Name: "villager", Prompt: "villager sprite", Size: "32x32",
			Shape: "variants", Variants: []string{"idle", "wave", "angry"}, Columns: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	// Pinned seed: every child at the set seed, suffix appended.
	cap.mu.Lock()
	defer cap.mu.Unlock()
	wantP := []string{"villager sprite, idle", "villager sprite, wave", "villager sprite, angry"}
	for i, wp := range wantP {
		if cap.Prompts[i] != wp {
			t.Fatalf("prompt %d = %q, want %q", i, cap.Prompts[i], wp)
		}
		if cap.Seeds[i] != 41 {
			t.Fatalf("seed %d = %d, want pinned 41", i, cap.Seeds[i])
		}
	}
	sheet, err := m.opts.Store.ReadSetFile(id, "villager-sheet.png")
	if err != nil {
		t.Fatalf("variant sheet: %v", err)
	}
	simg, err := png.Decode(bytes.NewReader(sheet))
	if err != nil {
		t.Fatal(err)
	}
	// R-16: the npc variants spec must carry Columns (2) into the composed
	// atlas. 3 variants @ 32px with 2 columns pack 2 rows → 64x64; the
	// single-form pack (Columns dropped → 1 row) is 96x32.
	if simg.Bounds().Dx() != 64 || simg.Bounds().Dy() != 64 {
		t.Fatalf("variant sheet = %dx%d, want 64x64 (columns 2 packs 2 rows of 32px cells)",
			simg.Bounds().Dx(), simg.Bounds().Dy())
	}

	// Ruling R-6: the manifest asset entry must record the variants list
	// itself, not merely show a successful sheet composition.
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Assets) != 1 || man.Assets[0].Error != "" {
		t.Fatalf("manifest assets: %+v", man.Assets)
	}
	got := man.Assets[0].Variants
	if len(got) != 3 || got[0] != "idle" || got[1] != "wave" || got[2] != "angry" {
		t.Fatalf("manifest variants = %q, want [idle wave angry]", got)
	}
}

func TestSubmitAssetSetNPCWalk4(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(41)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "npcw", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "npc", Name: "guard", Prompt: "town guard", Size: "32x32", Shape: "walk4"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	cap.mu.Lock()
	wantP := []string{
		"town guard, full front view, facing the viewer",
		"town guard, rear view, facing away from the viewer",
		"town guard, side profile view, facing right",
	}
	for i, wp := range wantP {
		if cap.Prompts[i] != wp {
			t.Fatalf("prompt %d = %q, want %q", i, cap.Prompts[i], wp)
		}
		if cap.Seeds[i] != 41 {
			t.Fatalf("seed %d = %d, want pinned 41", i, cap.Seeds[i])
		}
	}
	cap.mu.Unlock()
	for _, name := range []string{"guard-walk.png", "guard-atlas.json", "guard-down.png", "guard-left.png", "guard-right.png", "guard-up.png"} {
		if _, err := m.opts.Store.ReadSetFile(id, name); err != nil {
			t.Fatalf("set file %s: %v", name, err)
		}
	}
}

func TestSubmitAssetSetValidation(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	bad := []AssetSetRequest{
		{Name: "x", Assets: []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "p", Size: "64x64"}}}, // seed required
		{Name: "x", Seed: ptr64(1)},                                // no assets
		{Name: "x", Seed: ptr64(1), Assets: tooMany},               // 9 assets
		{Name: ".", Seed: ptr64(1), Assets: one},                   // name sanitizes to empty
		{Name: "x", Seed: ptr64(1), Dest: "rel/path", Assets: one}, // dest not absolute
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "p", Size: "250x250"}}},                                                         // 16-multiple
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "", Size: "64x64"}}},                                                            // prompt required
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "widget", Name: "a", Prompt: "p", Size: "64x64"}}},                                                            // unknown kind
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "atlas", Name: "a", Prompt: "p", Size: "64x64", Frames: 1}}},                                                  // frames >= 2
		{Name: "x", Seed: ptr64(1), Assets: []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "p", Size: "64x64"}, {Kind: "element", Name: "a", Prompt: "p", Size: "64x64"}}}, // dup names
		{Name: "big", Seed: ptr64(1), Assets: bigChildren},                                                                                                                     // > 64 children
	}
	for i, req := range bad {
		if _, err := m.SubmitAssetSet(req); err == nil {
			t.Fatalf("case %d: want validation error", i)
		}
	}
}

// setWithMeta builds a minimal one-asset asset-set request carrying
// meta_formats. The atlas-valid Frames minimum keeps the helper single-shape;
// element and kit ignore frames.
func setWithMeta(kind string, formats []string) AssetSetRequest {
	return AssetSetRequest{
		Name: "meta", Seed: ptr64(17),
		Assets: []AssetSpecIn{{Kind: kind, Name: "meta", Prompt: "meta asset", Size: "32x32", Frames: 3, MetaFormats: formats}},
	}
}

func TestMetaFormatsValidation(t *testing.T) {
	m := New(composeTestOpts(t, t.TempDir(), &genCapture{}))

	// Submit-time subset validation: format names outside
	// {phaser, unity, sparrow, tiled} are validation_error before anything
	// ships. Kind-compat is EmitMeta's job at compose time (below) — only the
	// name check is queue-side.
	bad := func(req AssetSetRequest, want string) {
		_, err := m.SubmitAssetSet(req)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	bad(setWithMeta("atlas", []string{"nope"}), "unknown")
	bad(setWithMeta("atlas", []string{"tiled", "nope"}), "unknown")
	bad(setWithMeta("element", []string{"nope"}), "unknown")
	// Duplicates would emit two identically named files — rejected at submit.
	bad(setWithMeta("atlas", []string{"phaser", "phaser"}), "duplicate")

	// Kind-compat failures surface honestly at compose time, never silently:
	// a valid format name on an unserving kind submits fine and breaks the
	// asset during composition — a single-asset set therefore fails the
	// parent with the named reason, whole-asset (its own composed outputs do
	// not ship either).
	broken := func(kind string, formats []string, want string) {
		id, err := m.SubmitAssetSet(setWithMeta(kind, formats))
		if err != nil {
			t.Fatalf("%s %v: submit: %v", kind, formats, err)
		}
		j := waitFor(t, m, id, "failed")
		if j.Status != "failed" || !strings.Contains(j.Error, "compose_error") || !strings.Contains(j.Error, want) {
			t.Fatalf("%s %v: parent = %s %q, want failure naming %q", kind, formats, j.Status, j.Error, want)
		}
	}
	broken("element", []string{"phaser"}, `does not apply to kind "element"`)
	broken("atlas", []string{"tiled"}, `"tiled" does not apply to kind "atlas"`)
	broken("kit", []string{"phaser"}, `does not apply to kind "kit"`)

	// Partial success holds for meta failures too: in a mixed set the healthy
	// asset ships and the broken-meta asset records the reason in the
	// manifest with zero files.
	seed := int64(23)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "mixmeta", Seed: &seed,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "ok", Prompt: "ok", Size: "16x16"},
			{Kind: "atlas", Name: "sheet", Prompt: "sheet", Size: "16x16", Frames: 2, MetaFormats: []string{"tiled"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("mixed set: %s %q", j.Status, j.Error)
	}
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.Assets[0].Error != "" || len(man.Assets[0].Files) != 1 {
		t.Fatalf("element should have succeeded: %+v", man.Assets[0])
	}
	if !strings.Contains(man.Assets[1].Error, `does not apply to kind "atlas"`) {
		t.Fatalf("atlas meta error must be recorded: %+v", man.Assets[1])
	}
	if len(man.Assets[1].Files) != 0 {
		t.Fatalf("broken asset ships no files: %+v", man.Assets[1])
	}
	if _, err := m.opts.Store.ReadSetFile(id, "sheet-sheet.png"); err == nil {
		t.Fatal("broken asset's composed outputs must not ship")
	}
}

func TestSubmitAssetSetPartialFailure(t *testing.T) {
	// element + kit; kit's 5th call (0-based 5) fails. The set still
	// completes: the element composes, the kit's error lands in manifest.
	cap := &failCapture{FailCall: 5}
	m := New(composeTestOptsFail(t, t.TempDir(), cap))
	seed := int64(9)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "mix", Seed: &seed,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "dot", Prompt: "dot", Size: "32x32"},
			{Kind: "kit", Name: "tiles", Prompt: "tiles", Size: "32x32"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.Assets[0].Error != "" || len(man.Assets[0].Files) != 1 {
		t.Fatalf("element should have succeeded: %+v", man.Assets[0])
	}
	if man.Assets[1].Error == "" || man.Assets[1].Error != "sidecar boom" {
		t.Fatalf("kit error should be recorded: %+v", man.Assets[1])
	}
	if _, err := m.opts.Store.ReadSetFile(id, "dot.png"); err != nil {
		t.Fatalf("dot.png should exist: %v", err)
	}
}

func TestSubmitAssetSetAllFail(t *testing.T) {
	cap := &failCapture{FailAll: true}
	m := New(composeTestOptsFail(t, t.TempDir(), cap))
	seed := int64(3)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "none", Seed: &seed,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "a", Prompt: "a", Size: "32x32"},
			{Kind: "element", Name: "b", Prompt: "b", Size: "32x32"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	j, _ := m.Get(id)
	if !strings.Contains(j.Error, "compose_error") || !strings.Contains(j.Error, "all assets failed") {
		t.Fatalf("error: %q", j.Error)
	}
	if _, err := m.opts.Store.ReadSetFile(id, "manifest.json"); err != nil {
		t.Fatalf("manifest still written on total failure: %v", err)
	}
}

func TestSubmitAssetSetDelivery(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "proj")
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(11)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "icon", Seed: &seed, Dest: dest,
		Assets: []AssetSpecIn{{Kind: "element", Name: "icon", Prompt: "icon", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	for _, name := range []string{"icon.png", "icon.manifest.json"} {
		if _, err := os.ReadFile(filepath.Join(dest, name)); err != nil {
			t.Fatalf("delivered %s: %v", name, err)
		}
	}
	mraw, _ := m.opts.Store.ReadSetFile(id, "manifest.json")
	var man setManifest
	_ = json.Unmarshal(mraw, &man)
	// DeliverSet delivers every set file (icon.png + the manifest renamed to
	// icon.manifest.json, sorted) and has created the not-yet-existing dest
	// directory.
	if len(man.Delivered) != 2 || man.Delivered[0] != filepath.Join(dest, "icon.png") || man.Delivered[1] != filepath.Join(dest, "icon.manifest.json") {
		t.Fatalf("delivered record: %+v", man.Delivered)
	}
}

func TestSubmitAssetSetDeliveryCollision(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "icon.png"), []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(13)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "icon", Seed: &seed, Dest: dest,
		Assets: []AssetSpecIn{{Kind: "element", Name: "icon", Prompt: "icon", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	j, _ := m.Get(id)
	if !strings.Contains(j.Error, "compose_error") || !strings.Contains(j.Error, "already exists") {
		t.Fatalf("error: %q", j.Error)
	}
}

func TestSubmitAssetSetUnsetDestDelivers(t *testing.T) {
	// Spec: unset dest inherits the export fallback chain (EXPORT_DIR →
	// ~/Downloads/img-gen). The executor delivers on every success, so with
	// Options.ExportDir pinned to a scratch dir the set lands there — under a
	// set-id subdirectory, because two fallback sets share the root and
	// identically named deliverables collide.
	cap := &genCapture{}
	dest := filepath.Join(t.TempDir(), "exports")
	opts := composeTestOpts(t, t.TempDir(), cap)
	opts.ExportDir = dest
	m := New(opts)
	seed := int64(21)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "export", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "export", Prompt: "e", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	for _, name := range []string{"export.png", "export.manifest.json"} {
		if _, err := os.ReadFile(filepath.Join(dest, id, name)); err != nil {
			t.Fatalf("delivered %s: %v", name, err)
		}
	}
	mraw, _ := m.opts.Store.ReadSetFile(id, "manifest.json")
	var man setManifest
	_ = json.Unmarshal(mraw, &man)
	// The manifest records the actual delivered paths, set-id subdir included.
	if man.Dest != filepath.Join(dest, id) {
		t.Fatalf("manifest dest = %q, want %q", man.Dest, filepath.Join(dest, id))
	}
	if len(man.Delivered) != 2 || man.Delivered[1] != filepath.Join(dest, id, "export.manifest.json") {
		t.Fatalf("delivered record: %+v", man.Delivered)
	}
}

func TestSubmitSetFallbackDestNoCollision(t *testing.T) {
	// Regression (Task 14 smoke): two no-dest sets sharing one fallback root
	// used to collide on a flat manifest.json — the second delivery hit
	// "destination file …/manifest.json already exists" and its parent
	// finished failed despite composing fine. Fallback deliveries now
	// namespace by set id, so both parents finish done with their own
	// subdirectory and nothing lands flat at the root.
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	root := m.opts.ExportDir
	first, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "one", Seed: ptr64(31),
		Assets: []AssetSpecIn{{Kind: "element", Name: "one", Prompt: "one", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "two", Seed: ptr64(32),
		Assets: []AssetSpecIn{{Kind: "element", Name: "two", Prompt: "two", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, m, first, "done"); j.Status != "done" {
		t.Fatalf("first set: %s %q", j.Status, j.Error)
	}
	if j := waitFor(t, m, second, "done"); j.Status != "done" {
		t.Fatalf("second set: %s %q", j.Status, j.Error)
	}
	for _, s := range []struct {
		id    string
		names []string
	}{{first, []string{"one.png", "one.manifest.json"}}, {second, []string{"two.png", "two.manifest.json"}}} {
		for _, name := range s.names {
			if _, err := os.ReadFile(filepath.Join(root, s.id, name)); err != nil {
				t.Fatalf("set %s delivered %s: %v", s.id, name, err)
			}
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			t.Fatalf("flat entry left at fallback root: %s", e.Name())
		}
	}
}

func ptr64(v int64) *int64 { return &v }

var one = []AssetSpecIn{{Kind: "element", Name: "a", Prompt: "p", Size: "64x64"}}

var tooMany = func() []AssetSpecIn {
	var out []AssetSpecIn
	for i := 0; i < 9; i++ {
		out = append(out, AssetSpecIn{Kind: "element", Name: fmt.Sprintf("a%d", i), Prompt: "p", Size: "64x64"})
	}
	return out
}()

var bigChildren = func() []AssetSpecIn {
	// 8 atlases x 9 frames = 72 children > 64.
	var out []AssetSpecIn
	for i := 0; i < 8; i++ {
		out = append(out, AssetSpecIn{Kind: "atlas", Name: fmt.Sprintf("at%d", i), Prompt: "p", Size: "64x64", Frames: 9})
	}
	return out
}()

var nineStatics = func() []StaticLayer {
	// StaticLayer forms are not validated until resolution, so B64 "AA"
	// (undecodable) is fine — the statics-count check fires first.
	var out []StaticLayer
	for i := 0; i < 9; i++ {
		out = append(out, StaticLayer{From: FrameRef{B64: "AA"}})
	}
	return out
}()

func TestSubmitGIFFromFrames(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	j1, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "f1", Size: "32x32"})
	if err != nil {
		t.Fatal(err)
	}
	j2, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: "f2", Size: "32x32"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j1, "done")
	waitFor(t, m, j2, "done")
	id, err := m.SubmitGIF(GifRequest{
		Name:   "spin",
		Source: GifSource{Type: "from_frames", Frames: []FrameRef{{Job: j1}, {Job: j2}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.opts.Store.ReadSetFile(id, "source-frame-0001.png"); err != nil {
		t.Fatalf("source frames persisted at submit: %v", err)
	}
	waitFor(t, m, id, "done")
	raw, err := m.opts.Store.ReadSetFile(id, "spin.gif")
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("spin.gif not a GIF: %v", err)
	}
	if len(g.Image) != 2 {
		t.Fatalf("gif frames: %d", len(g.Image))
	}
	if g.Delay[0] != 10 { // default delay_ms=100 → centiseconds
		t.Fatalf("delay %d", g.Delay[0])
	}
	j, _ := m.Get(id)
	if len(j.Files) != 1 || j.Files[0].Name != "spin.gif" || !j.Files[0].Primary {
		t.Fatalf("files: %+v", j.Files)
	}
}

func TestSubmitGIFFromSpec(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(30)
	id, err := m.SubmitGIF(GifRequest{
		Name: "pulse",
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "pulse orb", Size: "32x32", Seed: &seed},
			Count:   4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 4 || cap.Seeds[0] != 30 || cap.Seeds[3] != 33 {
		t.Fatalf("from_spec children: %q %q", cap.Prompts, cap.Seeds)
	}
	if !strings.Contains(cap.Prompts[1], "frame 2 of 4") {
		t.Fatalf("frame prompt: %q", cap.Prompts[1])
	}
	cap.mu.Unlock()
	raw, _ := m.opts.Store.ReadSetFile(id, "pulse.gif")
	if _, err := gif.DecodeAll(bytes.NewReader(raw)); err != nil {
		t.Fatalf("pulse.gif: %v", err)
	}
}

func TestComposeGifNilGifSpecFailsParent(t *testing.T) {
	// composeGIF dereferenced spec.GIF before any nil check, so a
	// hand-corrupted history.jsonl line — a compose parent whose spec parses
	// as {"kind":"gif"} with no gif block — panicked the worker goroutine and
	// crashed the whole process on restart. The corrupted parent must finish
	// failed instead (the injection pattern mirrors
	// TestRestoreMarksInterruptedJobsFailed).
	dir := t.TempDir()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHistory(storage.Job{
		ID: "0123456789abcdef", SetID: "0123456789abcdef", Mode: "compose",
		Prompt: "compose gif", Status: "queued",
		ComposeSpec: `{"kind":"gif","name":"corrupt"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	g, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	opts := composeTestOpts(t, dir, &genCapture{})
	opts.Store = g
	m := New(opts)
	if _, ok := m.Get("0123456789abcdef"); !ok {
		t.Fatal("corrupt parent not restored")
	}
	j := waitFor(t, m, "0123456789abcdef", "failed")
	if j.Status != "failed" || !strings.Contains(j.Error, "gif spec missing gif block") {
		t.Fatalf("corrupt parent: %s %q", j.Status, j.Error)
	}
}

func TestSubmitGIFPathRef(t *testing.T) {
	cap := &genCapture{}
	dir := t.TempDir()
	store, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A real decodable PNG under the data dir the path ref points at.
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveImage("abcdef0123456789", buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	m := New(composeTestOpts(t, dir, cap)) // same data dir the Path refs resolve against
	id, err := m.SubmitGIF(GifRequest{
		Name:    "via-path",
		DelayMS: 50,
		Source: GifSource{Type: "from_frames", Frames: []FrameRef{
			{Path: "images/abcdef0123456789.png"},
			{Path: "images/abcdef0123456789.png"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	raw, _ := m.opts.Store.ReadSetFile(id, "via-path.gif")
	if _, err := gif.DecodeAll(bytes.NewReader(raw)); err != nil {
		t.Fatalf("via-path.gif: %v", err)
	}
}

func TestSubmitGIFWebPFormatRecorded(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(2)
	id, err := m.SubmitGIF(GifRequest{
		Name: "webp", Formats: []string{"gif", "webp"},
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "w", Size: "32x32", Seed: &seed},
			Count:   2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done") // webp availability never fails the set
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.GIF == nil {
		t.Fatalf("gif manifest missing: %+v", man)
	}
	// Mirrors compose's GIF test invariant: the webp variant file exists iff
	// no skip reason was recorded. On machines with gif2webp the recorded
	// status is legitimately empty (the variant was produced); without it the
	// skip reason is recorded. Either way webp availability never fails the
	// set, and the manifest records the outcome.
	webp := false
	for _, f := range man.GIF.Files {
		if f == "webp.webp" {
			webp = true
		}
	}
	if webp != (man.GIF.WebP == "") {
		t.Fatalf("webp file/status mismatch: %+v", man.GIF)
	}
}

func TestSubmitGIFFrameIncomplete(t *testing.T) {
	cap := &failCapture{FailCall: 1}
	m := New(composeTestOptsFail(t, t.TempDir(), cap))
	seed := int64(4)
	id, err := m.SubmitGIF(GifRequest{
		Name: "gaps",
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "g", Size: "32x32", Seed: &seed},
			Count:   3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	j, _ := m.Get(id)
	if !strings.Contains(j.Error, "compose_error") || !strings.Contains(j.Error, "frame") {
		t.Fatalf("error: %q", j.Error)
	}
}

func TestSubmitGIFValidation(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(1)
	base := &SubmitRequest{Mode: "asset", Prompt: "p", Size: "32x32", Seed: &seed}
	cases := []struct {
		name string
		req  GifRequest
		want string // substring of the error message
	}{
		{"keyframes not implemented", GifRequest{Name: "k", Source: GifSource{Type: "keyframes", Count: 4}}, "RIFE"},
		{"source type", GifRequest{Name: "k", Source: GifSource{Type: "wat"}}, "from_frames, from_spec"},
		{"one frame", GifRequest{Name: "k", Source: GifSource{Type: "from_frames", Frames: []FrameRef{{Job: "x"}}}}, "2..64"},
		{"no request", GifRequest{Name: "k", Source: GifSource{Type: "from_spec", Count: 3}}, "request"},
		{"nil seed", GifRequest{Name: "k", Source: GifSource{Type: "from_spec", Request: &SubmitRequest{Mode: "asset", Prompt: "p", Size: "32x32"}, Count: 3}}, "seed"},
		{"count high", GifRequest{Name: "k", Source: GifSource{Type: "from_spec", Request: base, Count: 65}}, "2..64"},
		{"count low", GifRequest{Name: "k", Source: GifSource{Type: "from_spec", Request: base, Count: 1}}, "2..64"},
		{"delay low", GifRequest{Name: "k", DelayMS: 5, Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "delay_ms"},
		{"delay high", GifRequest{Name: "k", DelayMS: 6000, Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "delay_ms"},
		{"loop negative", GifRequest{Name: "k", Loop: -1, Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "loop"},
		{"format", GifRequest{Name: "k", Formats: []string{"apng"}, Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "formats"},
		{"dest", GifRequest{Name: "k", Dest: "rel", Source: GifSource{Type: "from_spec", Request: base, Count: 2}}, "absolute"},
		{"multi source", GifRequest{Name: "k", Source: GifSource{Type: "from_frames", Frames: []FrameRef{{Job: "a", B64: "AA"}, {Job: "b"}}}}, "exactly one"},
		// composite source validation: pointer, anim bounds, statics cap.
		{"composite missing", GifRequest{Name: "k", Source: GifSource{Type: "composite"}}, "source.composite"},
		{"composite anim low", GifRequest{Name: "k", Source: GifSource{Type: "composite", Composite: &CompositeSource{Anim: []FrameRef{{B64: "AA"}}}}}, "2..64"},
		{"statics high", GifRequest{Name: "k", Source: GifSource{Type: "composite", Composite: &CompositeSource{Anim: []FrameRef{{B64: "AA"}, {B64: "AA"}}, Statics: nineStatics}}}, "static layers"},
	}
	for _, tc := range cases {
		_, err := m.SubmitGIF(tc.req)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want substring %q", tc.name, err, tc.want)
		}
	}
	// traversal: a path source escaping the data dir is a submit-time error
	_, err := m.SubmitGIF(GifRequest{Name: "k", Source: GifSource{Type: "from_frames", Frames: []FrameRef{
		{Path: "../../etc/passwd"}, {Path: "images/x.png"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "data directory") {
		t.Fatalf("traversal: %v", err)
	}
	// and an unknown job id produces a submit-time error, not a stuck parent
	_, err = m.SubmitGIF(GifRequest{Name: "k2", Source: GifSource{Type: "from_frames", Frames: []FrameRef{
		{Job: "deadbeefdeadbeef"}, {Job: "deadbeefdeadbeef"},
	}}})
	if err == nil {
		t.Fatal("unknown job frame ref should fail at submit")
	}
	// every submit above failed at validation or frame resolution: no parent
	// and no partial child set may have been registered.
	if n := len(m.List()); n != 0 {
		t.Fatalf("failed submits left %d job(s) registered", n)
	}
}

func TestSubmitGIFFromSpecGhostParent(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(9)
	_, err := m.SubmitGIF(GifRequest{
		Name: "ghost",
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "p", Size: "32x32", Seed: &seed, Style: "bogus"},
			Count:   2,
		},
	})
	// submitOne's validation error carries the code unknown_style but its
	// Error() is the Msg alone: `unknown style "bogus"`.
	if err == nil || !strings.Contains(err.Error(), "unknown style") {
		t.Fatalf("child submit: %v", err)
	}
	// The base request passes SubmitGIF's pre-validation (style is only checked
	// per-child in submitOne), so the parent WAS persisted "queued" before the
	// child was rejected — the unwind must leave neither memory nor history.
	if n := len(m.List()); n != 0 {
		t.Fatalf("ghost parent registered: %d", n)
	}
	hist, err := m.opts.Store.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range hist {
		if j.Mode == "compose" {
			t.Fatalf("ghost compose parent in history: %+v", j)
		}
	}
}

// b64PNG renders a solid-color WxH PNG as inline base64, the {b64} frame-ref
// form for composite sources.
func b64PNG(t *testing.T, w, h int, c color.NRGBA) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestGifCompositeSource(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	red := b64PNG(t, 32, 32, color.NRGBA{R: 255, A: 255})
	blue := b64PNG(t, 8, 8, color.NRGBA{B: 255, A: 255})
	teal := b64PNG(t, 4, 4, color.NRGBA{B: 255, G: 255, A: 255})
	id, err := m.SubmitGIF(GifRequest{
		Name: "title",
		Source: GifSource{Type: "composite", Composite: &CompositeSource{
			Anim: []FrameRef{{B64: red}, {B64: red}},
			Statics: []StaticLayer{
				{From: FrameRef{B64: blue}, Pos: [2]int{24, 24}},
				{From: FrameRef{B64: teal}, Pos: [2]int{0, 0}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	j, ok := m.Get(id)
	if !ok {
		t.Fatal("job not registered")
	}
	if j.ImagePath == "" {
		t.Fatal("gif not composed")
	}
	// Set artifacts: two anim frames, both static layers (list order = file
	// order), the animation.
	for _, name := range []string{"source-frame-0000.png", "source-frame-0001.png", "static-layer-00.png", "static-layer-01.png", "title.gif"} {
		if _, err := m.opts.Store.ReadSetFile(id, name); err != nil {
			t.Fatalf("set file %s: %v", name, err)
		}
	}
	// The composed frame carries the overlay: the saved source frames are
	// pre-composite anim frames; the GIF is the proof. Verify the static
	// layer round-trips as a readable PNG of the right size.
	data, err := m.opts.Store.ReadSetFile(id, "static-layer-00.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// Ruling R-8: decode the produced GIF — the frame count matches the anim
	// list and the canvas is the animation's frame dims (statics blit inside,
	// never resize).
	raw, err := m.opts.Store.ReadSetFile(id, "title.gif")
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("title.gif not a GIF: %v", err)
	}
	if len(g.Image) != 2 {
		t.Fatalf("gif frames = %d, want 2", len(g.Image))
	}
	if g.Config.Width != 32 || g.Config.Height != 32 {
		t.Fatalf("gif canvas %dx%d, want 32x32", g.Config.Width, g.Config.Height)
	}
}

func TestGifCompositeOverhangFailsCompose(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	red := b64PNG(t, 32, 32, color.NRGBA{R: 255, A: 255})
	blue := b64PNG(t, 8, 8, color.NRGBA{B: 255, A: 255})
	id, err := m.SubmitGIF(GifRequest{
		Name: "badlayer",
		Source: GifSource{Type: "composite", Composite: &CompositeSource{
			Anim:    []FrameRef{{B64: red}, {B64: red}},
			Statics: []StaticLayer{{From: FrameRef{B64: blue}, Pos: [2]int{28, 28}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	j, ok := m.Get(id)
	if !ok {
		t.Fatal("job not registered")
	}
	if !strings.Contains(j.Error, "static-layer") {
		t.Fatalf("overhang error must name the file: %q", j.Error)
	}
}

func TestResolveFrameRefSetFile(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(1)
	sid, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "src", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "dot", Prompt: "dot", Size: "16x16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, sid, "done")

	data, err := m.resolveFrameRef(FrameRef{Set: sid, File: "dot.png"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.resolveFrameRef(FrameRef{Set: sid}); err == nil {
		t.Fatal("want error for set without file")
	}
	if _, err := m.resolveFrameRef(FrameRef{Set: sid, File: "nope.png"}); err == nil {
		t.Fatal("want error for missing file")
	}
	if _, err := m.resolveFrameRef(FrameRef{Set: sid, File: "dot.png", Path: "x"}); err == nil {
		t.Fatal("want error for mixed source kinds")
	}
}

// harnessWithoutRife builds the plain fake-ops manager: composeTestOpts never
// wires Ops.Rife, so keyframes submits must report the unconfigured sidecar.
func harnessWithoutRife(t *testing.T) *Manager {
	t.Helper()
	return New(composeTestOpts(t, t.TempDir(), &genCapture{}))
}

// harnessWithFakeRife wires a stub Rife (ImageOps.Rife was never wired in
// composeTestOpts): echo the anchors back as the interpolated frames, counting
// calls. Submit-path validation resolves anchors itself and must never reach
// the sidecar — the second return lets tests assert that with *rifeCalls.
func harnessWithFakeRife(t *testing.T) (*Manager, *int) {
	t.Helper()
	o := composeTestOpts(t, t.TempDir(), &genCapture{})
	rifeCalls := 0
	o.Ops.Rife = func(ctx context.Context, anchors [][]byte, multiplier int) ([][]byte, error) {
		rifeCalls++
		return anchors, nil
	}
	return New(o), &rifeCalls
}

func TestSubmitGIFKeyframesValidation(t *testing.T) {
	// FrameRef B64 sources need no setup — decoding happens inside
	// resolveFrameRef at submit. Two harness variants prove both error
	// families: a manager WITHOUT Ops.Rife (unconfigured sidecar) and the
	// fake-ops manager WITH a stub Rife wired (never reached during submit).
	bad := func(m *Manager, req GifRequest, want string) {
		_, err := m.SubmitGIF(req)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q err, got %v", want, err)
		}
	}
	anchor := func(b64 string) FrameRef { return FrameRef{B64: b64} }
	red := b64PNG(t, 4, 4, color.NRGBA{R: 255, A: 255})
	blue := b64PNG(t, 4, 4, color.NRGBA{B: 255, A: 255})
	a := anchor(red)
	b := anchor(blue)

	bad(harnessWithoutRife(t),
		GifRequest{Name: "k0", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: []FrameRef{a, b}}}},
		"requires the RIFE sidecar (RIFE_URL)")

	mgr, rife := harnessWithFakeRife(t)
	bad(mgr, GifRequest{Name: "k1", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: []FrameRef{a}}}},
		"anchors takes 2..16")
	bad(mgr, GifRequest{Name: "k2", Source: GifSource{Type: "keyframes"}},
		"keyframes source requires anchors")
	bad(mgr, GifRequest{Name: "k3", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: []FrameRef{a, b}, Multiplier: 11}}},
		"multiplier must be 2..10")
	many := make([]FrameRef, 17)
	for i := range many {
		many[i] = anchor(red)
	}
	bad(mgr, GifRequest{Name: "k3b", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: many}}},
		"anchors takes 2..16")

	// 16 anchors x multiplier 5 => (16-1)*5+1 = 76 frames > the 64 cap; the
	// suggestion names the true bound (63/15 = 4), not the breaching 5.
	anchors := make([]FrameRef, 16)
	for i := range anchors {
		anchors[i] = anchor(red)
	}
	bad(mgr, GifRequest{Name: "k4", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: anchors, Multiplier: 5}}},
		"cap is 64 — use multiplier ≤ 4")
	// Every validation-erroring submit above failed before any sidecar work:
	// the stub Rife was never reached during submit.
	if *rife != 0 {
		t.Fatalf("submit-path validation reached the Rife stub %d time(s)", *rife)
	}
	// The same anchors at the default multiplier ((16-1)*3+1 = 46 <= 64)
	// pass validation and submit; the executor (Task 5) interpolates through
	// the fake Rife echoing the anchors, so the parent must compose done.
	id, err := mgr.SubmitGIF(GifRequest{Name: "k5", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: anchors}}})
	if err != nil {
		t.Fatalf("16 anchors at default multiplier must submit: %v", err)
	}
	j := waitFor(t, mgr, id, "done")
	if j.Status != "done" {
		t.Fatalf("k5 parent must compose: %s %q", j.Status, j.Error)
	}
	raw, rerr := mgr.opts.Store.ReadSetFile(id, "k5.gif")
	if rerr != nil {
		t.Fatalf("k5.gif missing: %v", rerr)
	}
	if g, gerr := gif.DecodeAll(bytes.NewReader(raw)); gerr != nil || len(g.Image) != 16 {
		t.Fatalf("k5.gif: %v frames=%d", gerr, len(g.Image))
	}
	// Every failed submit above never registered a parent: only k5 remains.
	if n := len(mgr.List()); n != 1 {
		t.Fatalf("failed submits left %d extra job(s) registered", n-1)
	}
}

func TestSubmitGIFKeyframesStoresAnchors(t *testing.T) {
	// Keyframes submit mirrors from_frames: each anchor resolves exactly once
	// and persists as keyframe-%04d.png, with spec.GIF.Keyframes carrying the
	// resolved set-relative files so a restart recomposes without re-resolving.
	// The anchor echo from the harness's fake Rife composes the parent done:
	// 2 anchors → 2 interpolated frames → a real decoded GIF.
	m, _ := harnessWithFakeRife(t)
	anchors := []FrameRef{
		{B64: b64PNG(t, 4, 4, color.NRGBA{R: 255, A: 255})},
		{B64: b64PNG(t, 4, 4, color.NRGBA{B: 255, A: 255})},
	}
	id, err := m.SubmitGIF(GifRequest{
		Name:   "kf",
		Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: anchors}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"keyframe-0000.png", "keyframe-0001.png"} {
		if _, err := m.opts.Store.ReadSetFile(id, f); err != nil {
			t.Fatalf("%s persisted at submit: %v", f, err)
		}
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("keyframes parent must compose: %s %q", j.Status, j.Error)
	}
	raw, rerr := m.opts.Store.ReadSetFile(id, "kf.gif")
	if rerr != nil {
		t.Fatalf("kf.gif missing: %v", rerr)
	}
	if g, gerr := gif.DecodeAll(bytes.NewReader(raw)); gerr != nil || len(g.Image) != 2 {
		t.Fatalf("kf.gif: %v frames=%d", gerr, len(g.Image))
	}
	// Rebuild the spec the way a restart does: the persisted ComposeSpec from
	// history round-trips through JSON with the anchors resolved in place.
	var spec composeSpec
	if err := json.Unmarshal([]byte(j.ComposeSpec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.GIF == nil || spec.GIF.Keyframes == nil {
		t.Fatalf("spec missing keyframes block: %+v", spec)
	}
	files := spec.GIF.Keyframes.Files
	if len(files) != 2 || files[0] != "keyframe-0000.png" || files[1] != "keyframe-0001.png" {
		t.Fatalf("keyframes files: %v", files)
	}
	if spec.GIF.Keyframes.Multiplier != 3 {
		t.Fatalf("multiplier: %d", spec.GIF.Keyframes.Multiplier)
	}
}

// rifeCapture records what the fake sidecar observed in the request body.
// Mutex-guarded per the genCapture idiom: the httptest handler goroutine
// writes and the test goroutine reads, so bare fields would be a data race.
type rifeCapture struct {
	mu         sync.Mutex
	Images     int
	Multiplier int
}

func (c *rifeCapture) set(images, multiplier int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Images, c.Multiplier = images, multiplier
}

func (c *rifeCapture) get() (images, multiplier int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Images, c.Multiplier
}

func TestComposeGIFKeyframesFakeSidecar(t *testing.T) {
	// GOLDEN PATH — the spec's testing section requires the full
	// keyframes→gif pipeline to run against an HTTP fake sidecar, so Ops.Rife
	// here is lattice.Client.Rife talking to an httptest server (never a
	// hand-rolled Go func): both the request body contract and the response
	// decode go through the real client code path.
	cap := &rifeCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Images     []string `json:"images"`
			Multiplier int      `json:"multiplier"`
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		cap.set(len(req.Images), req.Multiplier)
		// Task 3 contract: (n-1)*mult+1 distinct 16x16 NRGBA PNGs, b64 in
		// order under {"frames": [...]}.
		n := (len(req.Images)-1)*req.Multiplier + 1
		frames := make([]string, 0, n)
		for i := 0; i < n; i++ {
			img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
			c := color.NRGBA{R: uint8((i * 47) % 256), G: uint8(64 + (i*13)%128), B: 200, A: 255}
			draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
				return
			}
			frames = append(frames, base64.StdEncoding.EncodeToString(buf.Bytes()))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()
	lat := lattice.New("")
	lat.RifeURL = srv.URL
	lat.HTTP = srv.Client()
	o := composeTestOpts(t, t.TempDir(), &genCapture{})
	o.Ops.Rife = lat.Rife
	m := New(o)

	id, err := m.SubmitGIF(GifRequest{
		Name:    "kf",
		Formats: []string{"gif", "webp"},
		Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{
			Anchors: []FrameRef{
				{B64: b64PNG(t, 16, 16, color.NRGBA{R: 255, A: 255})},
				{B64: b64PNG(t, 16, 16, color.NRGBA{B: 255, A: 255})},
			},
			Multiplier: 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("keyframes parent must compose: %s %q", j.Status, j.Error)
	}
	if imgs, mult := cap.get(); imgs != 2 || mult != 2 {
		t.Fatalf("sidecar contract: images=%d multiplier=%d", imgs, mult)
	}
	// Anchor persistence + the composed deliverable: 2×mult 2 → 3 frames.
	for _, f := range []string{"keyframe-0000.png", "keyframe-0001.png"} {
		if _, err := m.opts.Store.ReadSetFile(id, f); err != nil {
			t.Fatalf("set file %s: %v", f, err)
		}
	}
	raw, err := m.opts.Store.ReadSetFile(id, "kf.gif")
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("kf.gif not a GIF: %v", err)
	}
	if len(g.Image) != 3 {
		t.Fatalf("gif frames: %d, want 3", len(g.Image))
	}
	primary := ""
	for _, f := range j.Files {
		if f.Primary {
			primary = f.Name
		}
	}
	if primary != "kf.gif" {
		t.Fatalf("primary deliverable: %+v", j.Files)
	}
	// WebP is an extra: its variant file exists iff the manifest recorded no
	// skip reason (TestSubmitGIFWebPFormatRecorded's convention).
	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	webp := false
	for _, f := range man.GIF.Files {
		if f == "kf.webp" {
			webp = true
		}
	}
	if webp != (man.GIF.WebP == "") {
		t.Fatalf("webp file/status mismatch: %+v", man.GIF)
	}
}

func TestComposeGIFKeyframesSidecarFailure(t *testing.T) {
	// A failing sidecar fails the parent honestly. The nearest sidecar
	// precedent — the Ops.Upscale error path around queue.go's dispatch —
	// classifies no retryable sidecar classes: any op error is a flat
	// finish(id, "failed", ...), so the compose_error finish applies here.
	// Invariants: the error text carries the rife cause, the set holds no
	// partial gif, and the manager ends in the normal failed state.
	o := composeTestOpts(t, t.TempDir(), &genCapture{})
	o.Ops.Rife = func(ctx context.Context, anchors [][]byte, multiplier int) ([][]byte, error) {
		return nil, fmt.Errorf("rife sidecar: HTTP 500")
	}
	m := New(o)
	id, err := m.SubmitGIF(GifRequest{
		Name: "kf-fail",
		Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{
			Anchors: []FrameRef{
				{B64: b64PNG(t, 4, 4, color.NRGBA{R: 255, A: 255})},
				{B64: b64PNG(t, 4, 4, color.NRGBA{B: 255, A: 255})},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "failed")
	if j.Status != "failed" || !strings.Contains(j.Error, "rife:") {
		t.Fatalf("parent must fail with rife cause: %s %q", j.Status, j.Error)
	}
	// Anchor persistence survives; no deliverable is emitted.
	for _, f := range []string{"keyframe-0000.png", "keyframe-0001.png"} {
		if _, err := m.opts.Store.ReadSetFile(id, f); err != nil {
			t.Fatalf("anchors persist on failure: %v", err)
		}
	}
	if _, err := m.opts.Store.ReadSetFile(id, "kf-fail.gif"); err == nil {
		t.Fatal("partial gif saved despite sidecar failure")
	}
	if _, err := m.opts.Store.ReadSetFile(id, "manifest.json"); err == nil {
		t.Fatal("manifest saved despite sidecar failure")
	}
}

func TestComposeGIFKeyframesUnwiredRifeFailsParent(t *testing.T) {
	// spec.GIF.Keyframes only persists when Ops.Rife was non-nil at submit,
	// but a restart can rehydrate that spec into a manager built without
	// RIFE_URL. The executor must fail the parent honestly with the rife
	// cause — never a nil-function panic inside the worker goroutine.
	dir := t.TempDir()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"kind":"gif","name":"kfr","gif":{"delay_ms":100,"loop":0,"formats":["gif"],` +
		`"keyframes":{"files":["keyframe-0000.png","keyframe-0001.png"],"multiplier":3}}}`
	if err := s.AppendHistory(storage.Job{
		ID: "0123456789abcef0", SetID: "0123456789abcef0", Mode: "compose",
		Prompt: "compose gif", Status: "queued", ComposeSpec: spec,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	g, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	opts := composeTestOpts(t, dir, &genCapture{}) // Ops.Rife stays nil
	opts.Store = g
	m := New(opts)
	if _, ok := m.Get("0123456789abcef0"); !ok {
		t.Fatal("keyframes parent not restored")
	}
	j := waitFor(t, m, "0123456789abcef0", "failed")
	if j.Status != "failed" || !strings.Contains(j.Error, "rife:") {
		t.Fatalf("unwired rife parent: %s %q", j.Status, j.Error)
	}
}

// --- model inheritance on compose children ---------------------------------

// Compose children must never reach the lattice with an empty model: the
// registry routes by model name and the frontend rejects model "" with
// "no adequate gateway for capability local model". Under lattice routing the
// queue falls back to the configured default catalog key (options.DefaultModel
// seeded from IMAGE_MODEL), so asset sets work without carrying a model field.
func TestComposeChildrenLatticeDefaultModel(t *testing.T) {
	opts := composeTestOpts(t, t.TempDir(), &genCapture{})
	opts.ImagesViaLattice = true
	opts.DefaultModel = "asset-default"
	opts.Models = &models.Catalog{Models: []models.Entry{{Key: "asset-default", Value: "/m/asset-default-4bit"}}}
	m := New(opts)
	seed := int64(9)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "lattice", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "orb", Prompt: "orb", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	p, _ := m.Get(id)
	if len(p.Children) != 1 {
		t.Fatalf("children: %d", len(p.Children))
	}
	c, ok := m.Get(p.Children[0])
	if !ok {
		t.Fatal("child job missing")
	}
	if c.Model != "/m/asset-default-4bit" || c.ModelKey != "asset-default" {
		t.Fatalf("child not seeded with the lattice default model: model=%q key=%q", c.Model, c.ModelKey)
	}
}

// An explicit request-level model (catalog key) wins over the lattice default
// and carries sidecar derivation through the same path caller-submitted
// models take: the sdxl entry must move the child onto the sdxl engine.
func TestAssetSetRequestModelOverridesDefault(t *testing.T) {
	opts := composeTestOpts(t, t.TempDir(), &genCapture{})
	opts.ImagesViaLattice = true
	opts.DefaultModel = "asset-default"
	opts.Models = &models.Catalog{Models: []models.Entry{
		{Key: "asset-default", Value: "/m/asset-default-4bit"},
		{Key: "sdxl-ckpt", Value: "/m/sdxl-ckpt.safetensors", Sidecar: "sdxl"},
	}}
	m := New(opts)
	seed := int64(9)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "pick", Seed: &seed, Model: "sdxl-ckpt",
		Assets: []AssetSpecIn{{Kind: "element", Name: "orb", Prompt: "orb", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	p, _ := m.Get(id)
	if len(p.Children) != 1 {
		t.Fatalf("children: %d", len(p.Children))
	}
	c, _ := m.Get(p.Children[0])
	if c.Model != "/m/sdxl-ckpt.safetensors" || c.ModelKey != "sdxl-ckpt" || c.Sidecar != "sdxl" {
		t.Fatalf("request model override: model=%q key=%q sidecar=%q", c.Model, c.ModelKey, c.Sidecar)
	}
}

// Direct routing keeps the engine-default contract: Model stays empty even
// when a lattice default is configured — the sidecar's own default model is
// meaningful there, so the fallback must not rewrite it.
func TestComposeChildrenDirectRoutingKeepsEngineDefault(t *testing.T) {
	opts := composeTestOpts(t, t.TempDir(), &genCapture{})
	opts.DefaultModel = "asset-default"
	opts.Models = &models.Catalog{Models: []models.Entry{{Key: "asset-default", Value: "/m/asset-default-4bit"}}}
	m := New(opts)
	seed := int64(9)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "direct", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "orb", Prompt: "orb", Size: "32x32"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	p, _ := m.Get(id)
	if len(p.Children) != 1 {
		t.Fatalf("children: %d", len(p.Children))
	}
	c, _ := m.Get(p.Children[0])
	if c.Model != "" || c.ModelKey != "" {
		t.Fatalf("direct routing must keep engine default: model=%q key=%q", c.Model, c.ModelKey)
	}
}

// GIF from_spec children inherit GifRequest.Model (a catalog key or value)
// when the caller leaves source.request.Model empty; an explicit base request
// model still wins so per-gif requests keep the tighter contract.
func TestGIFFromSpecModelInheritance(t *testing.T) {
	catalog := &models.Catalog{Models: []models.Entry{{Key: "asset-default", Value: "/m/asset-default-4bit"}}}
	seed := int64(30)
	run := func(seedReq GifRequest, wantModel, wantKey string) {
		opts := composeTestOpts(t, t.TempDir(), &genCapture{})
		opts.ImagesViaLattice = true
		opts.DefaultModel = "asset-default"
		opts.Models = catalog
		m := New(opts)
		id, err := m.SubmitGIF(seedReq)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, m, id, "done")
		p, _ := m.Get(id)
		if len(p.Children) != 4 {
			t.Fatalf("children: %d", len(p.Children))
		}
		c, _ := m.Get(p.Children[0])
		if c.Model != wantModel || c.ModelKey != wantKey {
			t.Fatalf("gif child model: model=%q key=%q, want %q/%q", c.Model, c.ModelKey, wantModel, wantKey)
		}
	}
	run(GifRequest{
		Name: "pulse", Model: "asset-default",
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "pulse orb", Size: "32x32", Seed: &seed},
			Count:   4,
		},
	}, "/m/asset-default-4bit", "asset-default")
	run(GifRequest{
		Name: "pulse2",
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "pulse orb", Size: "32x32", Seed: &seed, Model: "asset-default"},
			Count:   4,
		},
	}, "/m/asset-default-4bit", "asset-default")
}

func TestTransparentSpecParse(t *testing.T) {
	cases := []struct {
		in   string
		want TransparentSpec
	}{
		{`{"transparent":true}`, TransparentDual},
		{`{"transparent":false}`, ""},
		{`{"transparent":null}`, ""},
		{`{}`, ""},
		{`{"transparent":""}`, ""},
		{`{"transparent":"rembg"}`, TransparentRembg},
		{`{"transparent":"dual"}`, TransparentDual},
		// Unknown strings survive decode verbatim so submit validation can
		// return a proper error message instead of a 400 decode failure.
		{`{"transparent":"wat"}`, "wat"},
	}
	for _, tc := range cases {
		var in AssetSpecIn
		if err := json.Unmarshal([]byte(tc.in), &in); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if in.Transparent != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.in, in.Transparent, tc.want)
		}
	}
}

func TestSubmitAssetSetRembgValidation(t *testing.T) {
	seed := int64(7)
	base := func() AssetSetRequest {
		return AssetSetRequest{Name: "star", Seed: &seed, Assets: []AssetSpecIn{{
			Kind: "element", Name: "star", Prompt: "star icon", Size: "32x32", Transparent: TransparentRembg,
		}}}
	}
	// No Ops.Rembg wired → honest validation error naming the env.
	m := New(composeTestOpts(t, t.TempDir(), &genCapture{}))
	_, err := m.SubmitAssetSet(base())
	verr, ok := err.(*ValidationError)
	if !ok || !strings.Contains(verr.Msg, "REMBG_URL") {
		t.Fatalf("err = %v", err)
	}
	// rembg is element/frame/bar only. kit/atlas fixtures pass their own kind
	// validations (kit has none; atlas needs Frames=2, npc needs a Shape) and
	// reach the rembg kind gate; bogus never leaves the kind switch.
	o := composeTestOpts(t, t.TempDir(), &genCapture{})
	o.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) { return nil, nil }
	m2 := New(o)
	kinds := map[string]string{
		"kit":   `transparent "rembg" is supported for element, frame and bar`,
		"atlas": `transparent "rembg" is supported for element, frame and bar`,
		"npc":   `transparent "rembg" is supported for element, frame and bar`,
		"bogus": `unknown kind "bogus"`,
	}
	for kind, want := range kinds {
		req := base()
		req.Assets[0].Kind = kind
		if kind == "atlas" {
			req.Assets[0].Frames = 2
		}
		if kind == "npc" {
			req.Assets[0].Shape = "walk4"
		}
		_, err := m2.SubmitAssetSet(req)
		verr, ok := err.(*ValidationError)
		if !ok || !strings.Contains(verr.Msg, want) {
			t.Fatalf("kind %s: err = %v, want substring %q", kind, err, want)
		}
	}
	// Unknown transparent value → validation error, not decode failure.
	req := base()
	req.Assets[0].Transparent = "wat"
	_, err = m2.SubmitAssetSet(req)
	verr, ok = err.(*ValidationError)
	if !ok || !strings.Contains(verr.Msg, `transparent must be true, false, or "rembg"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitAssetSetRembgSingleChild(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	rembgImg := image.NewNRGBA(image.Rect(0, 0, 8, 8)) // fully transparent
	var rb bytes.Buffer
	if err := png.Encode(&rb, rembgImg); err != nil {
		t.Fatal(err)
	}
	rembgCalled := 0
	o.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) {
		rembgCalled++
		return rb.Bytes(), nil
	}
	m := New(o)
	seed := int64(7)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "star", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "star", Prompt: "star icon", Size: "32x32", Transparent: TransparentRembg}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 1 {
		t.Fatalf("rembg renders once, got %q", cap.Prompts)
	}
	// The rembg child has a plain prompt — no "isolated on white" suffix;
	// U²-Net works best on natural backgrounds.
	if strings.Contains(cap.Prompts[0], "white") || strings.Contains(cap.Prompts[0], "black") {
		t.Fatalf("rembg prompt carries no bg suffix: %q", cap.Prompts[0])
	}
	cap.mu.Unlock()
	if rembgCalled != 1 {
		t.Fatalf("rembg calls = %d", rembgCalled)
	}
	data, err := m.opts.Store.ReadSetFile(id, "star.png")
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := im.At(0, 0).RGBA(); a != 0 {
		t.Fatalf("rembg output must keep alpha, got a=%d", a)
	}
}

func TestSubmitAssetSetRembgBarKeepsFill(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	// Same-size output is what the sidecar produces (U²-Net returns the input
	// dimensions as RGBA); the 32x32 rect also satisfies the nine-slice rule
	// (2*FrameMargins < size) that punches the rembg-extracted track.
	rembgImg := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	var rb bytes.Buffer
	if err := png.Encode(&rb, rembgImg); err != nil {
		t.Fatal(err)
	}
	rembgCalled := 0
	o.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) {
		rembgCalled++
		return rb.Bytes(), nil
	}
	m := New(o)
	seed := int64(7)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hp", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "bar", Name: "hp", Prompt: "steel frame", Size: "32x32",
			Transparent: TransparentRembg, FrameMargins: 4, BarFill: "red potion fill"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.Prompts) != 2 {
		t.Fatalf("rembg bar = track + fill renders, got %q", cap.Prompts)
	}
	if !strings.Contains(cap.Prompts[0], "steel frame") || strings.Contains(cap.Prompts[0], "white") {
		t.Fatalf("track prompt plain: %q", cap.Prompts[0])
	}
	if !strings.Contains(cap.Prompts[1], "red potion fill") {
		t.Fatalf("fill prompt: %q", cap.Prompts[1])
	}
	if _, err := m.opts.Store.ReadSetFile(id, "hp-track.png"); err != nil {
		t.Fatalf("track file: %v", err)
	}
	if _, err := m.opts.Store.ReadSetFile(id, "hp-fill.png"); err != nil {
		t.Fatalf("fill file: %v", err)
	}
	if rembgCalled != 1 {
		t.Fatalf("rembg calls = %d, want 1 (track only — fill is never rembg-processed)", rembgCalled)
	}
}

func TestSubmitAssetSetSheet(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap) // real sizes for the grid packer
	m := New(o)
	seed := int64(42)
	views := []string{"front", "side", "back", "casting"}
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hero", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "sheet", Name: "hero", Prompt: "knight", Size: "16x16", Columns: 2, Views: views}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 4 {
		t.Fatalf("4 children, got %q", cap.Prompts)
	}
	if cap.Seeds[0] != 42 || cap.Seeds[1] != 42 || cap.Seeds[2] != 42 || cap.Seeds[3] != 42 {
		t.Fatalf("every view shares the pinned seed: %v", cap.Seeds)
	}
	for i, want := range views {
		if !strings.Contains(cap.Prompts[i], ", "+want+" view") {
			t.Fatalf("prompt %d missing view suffix: %q", i, cap.Prompts[i])
		}
	}
	cap.mu.Unlock()
	if _, err := m.opts.Store.ReadSetFile(id, "hero-sheet.png"); err != nil {
		t.Fatalf("sheet png: %v", err)
	}
	sj, err := m.opts.Store.ReadSetFile(id, "hero-sheet.json")
	if err != nil {
		t.Fatalf("sheet json: %v", err)
	}
	for _, v := range views {
		if !strings.Contains(string(sj), `"`+v+`"`) {
			t.Fatalf("sheet json missing view %q: %s", v, sj)
		}
	}
	// manifest records the views
	mj, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mj), `"front"`) || !strings.Contains(string(mj), `"views"`) {
		t.Fatalf("manifest views: %s", mj)
	}
}

func TestSubmitAssetSetSheetValidation(t *testing.T) {
	seed := int64(1)
	mk := func(a AssetSpecIn) AssetSetRequest {
		return AssetSetRequest{Name: "hero", Seed: &seed, Assets: []AssetSpecIn{a}}
	}
	base := AssetSpecIn{Kind: "sheet", Name: "hero", Prompt: "knight", Size: "16x16"}
	m := New(composeTestOpts(t, t.TempDir(), nil))
	cases := []struct {
		name string
		set  func(*AssetSpecIn)
		want string
	}{
		{"one view", func(a *AssetSpecIn) { a.Views = []string{"front"} }, "sheet views takes 2..8"},
		{"nine views", func(a *AssetSpecIn) { a.Views = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} }, "sheet views takes 2..8"},
		{"empty view", func(a *AssetSpecIn) { a.Views = []string{"front", " "} }, "sheet view 1 is empty"},
		{"dup view", func(a *AssetSpecIn) { a.Views = []string{"front", "front"} }, `duplicate sheet view "front"`},
		{"columns too many", func(a *AssetSpecIn) { a.Views = []string{"front", "side"}; a.Columns = 3 }, "columns must be 0 (single row) or 1..2"},
		{"columns negative", func(a *AssetSpecIn) { a.Views = []string{"front", "side"}; a.Columns = -1 }, "columns must be 0 (single row) or 1..2"},
	}
	for _, tc := range cases {
		req := mk(base)
		tc.set(&req.Assets[0])
		_, err := m.SubmitAssetSet(req)
		verr, ok := err.(*ValidationError)
		if !ok || !strings.Contains(verr.Msg, tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if verr.Code != "validation_error" {
			t.Fatalf("%s: code = %q", tc.name, verr.Code)
		}
	}
}

func TestSubmitAssetSetSheetOneRowDefault(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(9)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hero", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "sheet", Name: "hero", Prompt: "knight", Size: "16x16", Views: []string{"front", "side", "back"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	data, err := m.opts.Store.ReadSetFile(id, "hero-sheet.png")
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := im.Bounds(); b.Dx() != 48 || b.Dy() != 16 {
		t.Fatalf("one-row sheet dims = %dx%d", b.Dx(), b.Dy())
	}
}

func TestSubmitAssetSetSheetMetaFormats(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(5)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hero", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "sheet", Name: "hero", Prompt: "knight", Size: "16x16",
			Views: []string{"front", "side"}, MetaFormats: []string{"phaser", "unity", "sparrow"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	for _, f := range []string{"hero.phaser.json", "hero.frames.json", "hero.sparrow.xml"} {
		if _, err := m.opts.Store.ReadSetFile(id, f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}
