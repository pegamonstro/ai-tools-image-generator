package queue

import (
	"context"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"img-gen/internal/critic"
	"img-gen/internal/models"
	"img-gen/internal/storage"
)


// canvasPNG builds a WxH solid-color PNG for stub ops.
func canvasPNG(t *testing.T, c color.RGBA, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// regionalTestPNG builds a small solid-color PNG for stub ops.
func regionalTestPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func regionalRequest() SubmitRequest {
	return SubmitRequest{
		Mode:   "regional",
		Model:  "/models/illustrious.safetensors",
		Size:   "512x768",
		Seed:   int64p(700001),
		Regions: []storage.RegionSpec{
			{Prompt: "girl kneeling on the bed, left side"},
			{Prompt: "man standing at the right", Rect: [4]int{300, 40, 212, 700}},
		},
	}
}

func int64p(v int64) *int64 { return &v }

func TestRegionalValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*SubmitRequest)
		wantSub string
	}{
		{"too few regions", func(r *SubmitRequest) { r.Regions = r.Regions[:1] }, "at least two regions"},
		{"empty base prompt", func(r *SubmitRequest) { r.Regions[0].Prompt = "  " }, "regions[0].prompt (the base region) is required"},
		{"bad size", func(r *SubmitRequest) { r.Size = "nope" }, "invalid size"},
		{"empty fill prompt", func(r *SubmitRequest) { r.Regions[1].Prompt = "" }, "regions[1].prompt is required"},
		{"rect outside canvas", func(r *SubmitRequest) { r.Regions[1].Rect = [4]int{500, 40, 200, 700} }, "falls outside the 512x768 canvas"},
		{"strength out of range", func(r *SubmitRequest) { r.Regions[1].Strength = 1.5 }, "regions[1].strength"},
		{"enhance refused", func(r *SubmitRequest) { r.Enhance = true }, "enhance is not supported on regional"},
		{"non-sdxl model", func(r *SubmitRequest) { r.Model = "persephone" }, "engine unsupported for regional"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := regionalTestManager(t)
			req := regionalRequest()
			tc.mutate(&req)
			_, err := m.Submit(req)
			if err == nil {
				t.Fatalf("want validation error containing %q, got none", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
	t.Run("no fill op", func(t *testing.T) {
		m := New(testOpts(t))
		_, err := m.Submit(regionalRequest())
		if err == nil || !strings.Contains(err.Error(), "regional is not available") {
			t.Fatalf("want unavailable error, got %v", err)
		}
	})
	t.Run("exclude_occupancy without rembg", func(t *testing.T) {
		opts := testOpts(t)
		opts.Ops.Fill = func(ctx context.Context, prompt, size, imageB64, maskB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
			return []byte("PNG"), nil, nil
		}
		m := New(opts)
		req := regionalRequest()
		req.Regions[1].ExcludeOccupancy = true
		_, err := m.Submit(req)
		if err == nil || !strings.Contains(err.Error(), "exclude_occupancy needs the rembg sidecar") {
			t.Fatalf("want rembg-sidecar validation error, got %v", err)
		}
	})
}

type fillCall struct {
	prompt, size, imageB64, maskB64 string
	strength                        float64
	spec                            storage.ModelSpec
	sp                              storage.SamplingParams
}

// regionalTestManager wires stub generate/fill/rembg ops that record calls and
// return distinctive images per stage.
// regionalModelCatalog declares an sdxl-sidecar model key the tests submit with.
func regionalModelCatalog() *models.Catalog {
	return &models.Catalog{
		Models: []models.Entry{
			{Key: "sdxl-illustrious", Value: "/models/illustrious.safetensors", Sidecar: "sdxl"},
		},
	}
}

func regionalTestManager(t *testing.T) (*Manager, *[]fillCall, *[]string) {
	t.Helper()
	opts := testOpts(t)
	opts.Models = regionalModelCatalog()
	var callsMu sync.Mutex
	fills := []fillCall{}
	bases := []string{}
	basePNG := regionalTestPNG(t, color.RGBA{40, 0, 0, 255})
	fillPNG := regionalTestPNG(t, color.RGBA{0, 40, 0, 255})
	alphaPNG := func() []byte {
		img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
		for y := 0; y < 16; y++ {
			for x := 0; x < 16; x++ {
				img.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 200})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}()
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		callsMu.Lock()
		bases = append(bases, prompt)
		callsMu.Unlock()
		seed := sp.Seed
		return basePNG, seed, nil
	}
	opts.Ops.Fill = func(ctx context.Context, prompt, size, imageB64, maskB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		callsMu.Lock()
		fills = append(fills, fillCall{prompt, size, imageB64, maskB64, strength, spec, sp})
		callsMu.Unlock()
		return fillPNG, nil, nil
	}
	opts.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) {
		return alphaPNG, nil
	}
	m := New(opts)
	return m, &fills, &bases
}

func TestRegionalPipelineRunsStages(t *testing.T) {
	m, fills, bases := regionalTestManager(t)
	req := regionalRequest()
	id, err := m.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := m.Get(id)
		if ok && j.Status == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, ok := m.Get(id)
	if !ok || j.Status != "done" {
		t.Fatalf("job not done: %+v", j)
	}
	if len(*bases) != 1 || (*bases)[0] != req.Regions[0].Prompt {
		t.Fatalf("base render calls wrong: %v", *bases)
	}
	if len(*fills) != 1 {
		t.Fatalf("want 1 fill call, got %d", len(*fills))
	}
	f := (*fills)[0]
	if f.prompt != req.Regions[1].Prompt {
		t.Fatalf("fill prompt = %q", f.prompt)
	}
	if f.size != "512x768" {
		t.Fatalf("fill size = %q", f.size)
	}
	if f.strength != 1.0 {
		t.Fatalf("fill strength default = %v (want 1.0)", f.strength)
	}
	// mask: decode and confirm only the rect is white
	raw, err := base64.StdEncoding.DecodeString(f.maskB64)
	if err != nil {
		t.Fatal(err)
	}
	maskImg, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mask, ok := maskImg.(*image.Gray)
	if !ok {
		t.Fatalf("mask is %T", maskImg)
	}
	for y := 0; y < 768; y++ {
		for x := 0; x < 512; x++ {
			inRect := x >= 300 && x < 512 && y >= 40 && y < 740
			white := mask.GrayAt(x, y).Y >= 128
			if inRect != white {
				t.Fatalf("mask at (%d,%d): inRect=%v white=%v", x, y, inRect, white)
			}
		}
	}
	// job displays the base prompt; regions persisted
	if j.Prompt != req.Regions[0].Prompt {
		t.Fatalf("job prompt = %q, want base region prompt", j.Prompt)
	}
	if len(j.Regions) != 2 {
		t.Fatalf("job regions = %d", len(j.Regions))
	}
}

func TestRegionalExcludeOccupancyCarvesSilhouette(t *testing.T) {
	// rembg stub: an opaque silhouette covering the LEFT half of the canvas —
	// figure A stands at x<256; the dilated carve protects x<270.
	sil := image.NewNRGBA(image.Rect(0, 0, 512, 768))
	for y := 0; y < 768; y++ {
		for x := 0; x < 512; x++ {
			if x < 256 {
				sil.SetNRGBA(x, y, color.NRGBA{R: 255, A: 200})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, sil); err != nil {
		t.Fatal(err)
	}
	silPNG := buf.Bytes()

	var callsMu sync.Mutex
	maskB64s := []string{}
	opts := testOptsAt(t, t.TempDir())
	opts.Models = regionalModelCatalog()
	basePNG := regionalTestPNG(t, color.RGBA{40, 0, 0, 255})
	fillPNG := regionalTestPNG(t, color.RGBA{0, 40, 0, 255})
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		return basePNG, sp.Seed, nil
	}
	opts.Ops.Fill = func(ctx context.Context, prompt, size, imageB64, maskB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		callsMu.Lock()
		maskB64s = append(maskB64s, maskB64)
		callsMu.Unlock()
		return fillPNG, nil, nil
	}
	opts.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) {
		return silPNG, nil
	}
	m := New(opts)

	req := regionalRequest()
	// rect overlaps the silhouette's right edge: white survives only where
	// the dilation (radius 14) ends.
	req.Regions[1].Rect = [4]int{100, 40, 412, 700}
	req.Regions[1].ExcludeOccupancy = true
	id, err := m.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := m.Get(id)
		if j.Status == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := m.Get(id)
	if j.Status != "done" {
		t.Fatalf("job not done: %+v", j)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if len(maskB64s) != 1 {
		t.Fatalf("want 1 fill, got %d", len(maskB64s))
	}
	raw, _ := base64.StdEncoding.DecodeString(maskB64s[0])
	maskImg, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mask := maskImg.(*image.Gray)
	for y := 0; y < 768; y++ {
		for x := 100; x < 512; x++ {
			inRect := y >= 40 && y < 740
			wantWhite := inRect && x >= 270 // silhouette x<256 + radius 14
			gotWhite := mask.GrayAt(x, y).Y >= 128
			if wantWhite != gotWhite {
				t.Fatalf("mask at (%d,%d): wantWhite=%v got=%v", x, y, wantWhite, gotWhite)
			}
		}
	}
}

func TestRegionalSeedsFlowToFill(t *testing.T) {
	m, fills, _ := regionalTestManager(t)
	req := regionalRequest()
	req.Seed = int64p(4242)
	id, err := m.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := m.Get(id)
		if j.Status == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(*fills) == 0 {
		t.Fatal("no fill calls")
	}
	sp := (*fills)[0].sp
	if sp.Seed == nil || *sp.Seed != 4242 {
		t.Fatalf("fill sampling seed = %v, want 4242", sp.Seed)
	}
}

var errNoFill = errors.New("no fill")

func TestRegionalFillFailureFailsJob(t *testing.T) {
	opts := testOpts(t)
	opts.Models = regionalModelCatalog()
	opts.Ops.Fill = func(ctx context.Context, prompt, size, imageB64, maskB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		return nil, nil, errNoFill
	}
	m := New(opts)
	req := regionalRequest()
	id, err := m.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := m.Get(id)
		if j.Status == "failed" {
			if !strings.Contains(j.Error, errNoFill.Error()) {
				t.Fatalf("error text %q misses fill cause", j.Error)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := m.Get(id)
	t.Fatalf("job not failed after deadline: %+v", j)
}

func TestRegionalPresenceGateRetries(t *testing.T) {
	opts := testOptsAt(t, t.TempDir())
	opts.Models = regionalModelCatalog()
	basePNG := regionalTestPNG(t, color.RGBA{40, 0, 0, 255})
	fillPNG := regionalTestPNG(t, color.RGBA{0, 40, 0, 255})
	var mu sync.Mutex
	generates, fills := 0, 0
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		mu.Lock()
		generates++
		mu.Unlock()
		seed := sp.Seed
		return basePNG, seed, nil
	}
	opts.Ops.Fill = func(ctx context.Context, prompt, size, imageB64, maskB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		mu.Lock()
		fills++
		mu.Unlock()
		return fillPNG, nil, nil
	}
	opts.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) {
		return basePNG, nil // opaque stand-in silhouette
	}
	opts.Critic.Count = func(ctx context.Context, imageB64 string) (int, error) {
		return 1, nil // always missing figure B: gate must fire exactly once
	}
	m := New(opts)
	req := regionalRequest()
	req.Seed = int64p(900)
	id, err := m.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := m.Get(id)
		if j.Status == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := m.Get(id)
	if j.Status != "done" {
		t.Fatalf("job not done: %+v (err %q)", j.Status, j.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if generates != 2 || fills != 2 {
		t.Fatalf("want exactly one retry (2 generate/2 fill got), got %d generate %d fill", generates, fills)
	}
}

func TestRegionalPresenceGateOffWithoutCount(t *testing.T) {
	m, fills, bases := regionalTestManager(t)
	req := regionalRequest()
	id, err := m.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := m.Get(id)
		if j.Status == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(*bases) != 1 || len(*fills) != 1 {
		t.Fatalf("gate-off must run exactly one pipeline, got bases=%d fills=%d", len(*bases), len(*fills))
	}
}

func TestRegionalValidateJudgesPerRegionCrops(t *testing.T) {
	// full-canvas base and fill images; the judge must receive the full
	// canvas once and a [212x700] region crop once
	basePNG := canvasPNG(t, color.RGBA{40, 0, 0, 255}, 512, 768)
	fillPNG := canvasPNG(t, color.RGBA{0, 40, 0, 255}, 512, 768)
	var mu sync.Mutex
	seen := [][2]int{} // decoded PNG dims the judge received, in call order
	opts := testOptsAt(t, t.TempDir())
	opts.Models = regionalModelCatalog()
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		return basePNG, sp.Seed, nil
	}
	opts.Ops.Fill = func(ctx context.Context, prompt, size, imageB64, maskB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		return fillPNG, nil, nil
	}
	// the judge must see the full canvas once and the region crop once
	opts.Critic.Judge = func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
		raw, err := base64.StdEncoding.DecodeString(imageB64)
		if err != nil {
			t.Errorf("judge payload: %v", err)
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Errorf("judge payload decode: %v", err)
		}
		mu.Lock()
		seen = append(seen, [2]int{img.Bounds().Dx(), img.Bounds().Dy()})
		mu.Unlock()
		scores := make([]critic.VLMScore, len(criteria))
		for i := range criteria {
			scores[i] = critic.VLMScore{ID: i, Pass: true, Reason: "ok"}
		}
		return scores, nil
	}
	m := New(opts)
	req := regionalRequest()
	req.Validate = &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"anatomy ok"`)}}}
	id, err := m.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := m.Get(id)
		if j.Status == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := m.Get(id)
	if j.Status != "done" {
		t.Fatalf("job not done: %+v", j)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("judge saw %d legs, want 2 (base + region crop): %v", len(seen), seen)
	}
	if seen[0] != [2]int{512, 768} || seen[1] != [2]int{212, 700} {
		t.Fatalf("judge legs = %v, want [512x768] then [212x700])", seen)
	}
}
