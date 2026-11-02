package queue

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"

	"img-gen/internal/models"
	"img-gen/internal/prompting"
	"img-gen/internal/storage"
)

// condCapture records the conditioning each Generate dispatch received.
type condCapture struct {
	mu         sync.Mutex
	Prompts    []string
	Seeds      []int64
	Controls   []*storage.ControlParams
	IPAdapters []*storage.IPAdapterParams
}

func (c *condCapture) add(prompt string, seed *int64, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Prompts = append(c.Prompts, prompt)
	s := int64(-1)
	if seed != nil {
		s = *seed
	}
	c.Seeds = append(c.Seeds, s)
	c.Controls = append(c.Controls, ctrl)
	c.IPAdapters = append(c.IPAdapters, ipa)
}

func condOpts(t *testing.T, cap *condCapture, dwpose func(ctx context.Context, imageB64 string) ([]float32, error)) Options {
	t.Helper()
	s, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Genres: testCatalog(),
		Store:  s,
		// Character conditioning is SDXL-engine-only; the catalog must carry
		// an sdxl-sidecar key for the character submit gate to accept the set.
		Models:    &models.Catalog{Models: []models.Entry{{Key: "sdxl-test", Value: "/m/pony.safetensors", Sidecar: "sdxl"}}},
		ExportDir: t.TempDir(),
		Ops: ImageOps{
			Generate: func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
				img := image.NewRGBA(image.Rect(0, 0, 8, 8))
				var buf bytes.Buffer
				if err := png.Encode(&buf, img); err != nil {
					t.Fatal(err)
				}
				cap.add(prompt, sp.Seed, ctrl, ipa)
				return buf.Bytes(), sp.Seed, nil
			},
			DWPose: dwpose,
		},
		Chat: func(ctx context.Context, msgs []prompting.Message) (string, error) { return "enhanced", nil },
	}
}

// identityPNG returns a small opaque PNG as base64 for character_identity.
func identityPNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{120, 90, 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// posePNGB64 returns a small PNG as base64 for a pose image.
func posePNGB64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{200, 200, 210, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func dwposeKeypoints() []float32 {
	// 17 x,y,conf triplets of a standing figure in 0..1 coords.
	return []float32{
		0.5, 0.1, 1, 0.48, 0.09, 1, 0.52, 0.09, 1, 0.47, 0.1, 1, 0.53, 0.1, 1,
		0.4, 0.3, 1, 0.6, 0.3, 1, 0.35, 0.5, 1, 0.65, 0.5, 1,
		0.33, 0.7, 1, 0.67, 0.7, 1, 0.45, 0.6, 1, 0.55, 0.6, 1,
		0.43, 0.8, 1, 0.57, 0.8, 1, 0.42, 0.98, 1, 0.58, 0.98, 1,
	}
}

func TestCharacterNamedPosePromptOnly(t *testing.T) {
	cap := &condCapture{}
	m := New(condOpts(t, cap, nil))
	seed := int64(42)
	identity := identityPNG(t)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hero", Seed: &seed,
		Model: "sdxl-test",
		Assets: []AssetSpecIn{{
			Kind: "character", Name: "hero", Prompt: "a knight",
			Size: "64x64", CharacterIdentity: identity,
			Poses: []string{"standing tall", "arms crossed"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.Prompts) != 2 {
		t.Fatalf("renders = %d, want 2 poses", len(cap.Prompts))
	}
	var sawStanding, sawCrossed bool
	for i, p := range cap.Prompts {
		if !strings.Contains(p, "a knight") {
			t.Fatalf("prompt %d missing base prompt: %q", i, p)
		}
		if strings.Contains(p, "pose reference") {
			t.Fatalf("named pose must not say pose reference: %q", p)
		}
		sawStanding = sawStanding || strings.Contains(p, "standing tall")
		sawCrossed = sawCrossed || strings.Contains(p, "arms crossed")
		if cap.Controls[i] != nil {
			t.Fatalf("named pose %d must carry no control, got %+v", i, cap.Controls[i])
		}
		ipa := cap.IPAdapters[i]
		if ipa == nil || ipa.Image != identity {
			t.Fatalf("named pose %d lost IPAdapter identity", i)
		}
	}
	if !sawStanding || !sawCrossed {
		t.Fatalf("pose names missing from prompts: %v", cap.Prompts)
	}
	if cap.Seeds[0] != 42 || cap.Seeds[1] != 42 {
		t.Fatalf("poses must share the pinned seed: %v", cap.Seeds)
	}
}

func TestCharacterPoseImageExtractsSkeleton(t *testing.T) {
	cap := &condCapture{}
	pose := posePNGB64(t)
	var called bool
	m := New(condOpts(t, cap, func(ctx context.Context, imageB64 string) ([]float32, error) {
		called = true
		if imageB64 != pose {
			t.Errorf("DWPose got %q…, want the pose photo", imageB64[:min(24, len(imageB64))])
		}
		return dwposeKeypoints(), nil
	}))
	seed := int64(7)
	identity := identityPNG(t)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "knight", Seed: &seed,
		Model: "sdxl-test",
		Assets: []AssetSpecIn{{
			Kind: "character", Name: "knight", Prompt: "a knight",
			Size: "64x64", CharacterIdentity: identity,
			Poses: []string{pose},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")

	if !called {
		t.Fatal("DWPose sidecar was never called")
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.Controls) != 1 || cap.Controls[0] == nil {
		t.Fatalf("pose-image child dispatched without control: %+v", cap.Controls)
	}
	ctrl := cap.Controls[0]
	if ctrl.Net != "pose" {
		t.Fatalf("control net = %q, want pose", ctrl.Net)
	}
	if ctrl.Image == pose {
		t.Fatal("control.image is still the raw pose photo — skeleton replacement missing")
	}
	raw, derr := base64.StdEncoding.DecodeString(ctrl.Image)
	if derr != nil {
		t.Fatalf("replaced control.image is not base64: %v", derr)
	}
	if _, perr := png.Decode(bytes.NewReader(raw)); perr != nil {
		t.Fatalf("replaced control.image is not a PNG skeleton: %v", perr)
	}
	if !strings.Contains(cap.Prompts[0], "pose reference") {
		t.Fatalf("pose-image child prompt missing pose reference suffix: %q", cap.Prompts[0])
	}
	ipa := cap.IPAdapters[0]
	if ipa == nil || ipa.Image != identity {
		t.Fatal("pose-image child lost IPAdapter identity")
	}
}

func TestCharacterPoseImageWithoutDWPoseFailsSubmit(t *testing.T) {
	cap := &condCapture{}
	m := New(condOpts(t, cap, nil)) // no DWPose wired
	seed := int64(9)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "ghost", Seed: &seed,
		Model: "sdxl-test",
		Assets: []AssetSpecIn{{
			Kind: "character", Name: "knight", Prompt: "a knight",
			Size: "64x64", CharacterIdentity: identityPNG(t),
			Poses: []string{posePNGB64(t)},
		}},
	})
	if err == nil {
		m.Cancel(id)
		t.Fatal("pose-image character must fail submit without DWPose")
	}
	if !strings.Contains(err.Error(), "DWPose sidecar not configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCharacterSkeletonFailureFailsJob(t *testing.T) {
	cap := &condCapture{}
	m := New(condOpts(t, cap, func(ctx context.Context, imageB64 string) ([]float32, error) {
		return nil, context.DeadlineExceeded
	}))
	seed := int64(11)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "skeleton-fail", Seed: &seed,
		Model: "sdxl-test",
		Assets: []AssetSpecIn{{
			Kind: "character", Name: "knight", Prompt: "a knight",
			Size: "64x64", CharacterIdentity: identityPNG(t),
			Poses: []string{posePNGB64(t)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "failed")
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.Prompts) != 0 {
		t.Fatalf("failed extraction must not render: %+v", cap.Prompts)
	}
}
