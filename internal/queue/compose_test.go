package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"img-gen/internal/compose"
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
			Generate: func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
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
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		time.Sleep(100 * time.Millisecond)
		return base(ctx, prompt, size, spec, sp)
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
			Generate: func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
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

func TestSubmitAssetSetTransparent(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(7)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "star", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "star", Prompt: "star icon", Size: "32x32", Transparent: true}},
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
	for _, name := range []string{"icon.png", "manifest.json"} {
		if _, err := os.ReadFile(filepath.Join(dest, name)); err != nil {
			t.Fatalf("delivered %s: %v", name, err)
		}
	}
	mraw, _ := m.opts.Store.ReadSetFile(id, "manifest.json")
	var man setManifest
	_ = json.Unmarshal(mraw, &man)
	// DeliverSet delivers every set file (icon.png + manifest.json, sorted)
	// and has created the not-yet-existing dest directory.
	if len(man.Delivered) != 2 || man.Delivered[0] != filepath.Join(dest, "icon.png") || man.Delivered[1] != filepath.Join(dest, "manifest.json") {
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
	// Options.ExportDir pinned to a scratch dir the set lands there.
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
	for _, name := range []string{"export.png", "manifest.json"} {
		if _, err := os.ReadFile(filepath.Join(dest, name)); err != nil {
			t.Fatalf("delivered %s: %v", name, err)
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
