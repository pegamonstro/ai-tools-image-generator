package lattice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"img-gen/internal/storage"
)

func TestGenerate(t *testing.T) {
	img := []byte("fake-png-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req struct {
			Prompt string `json:"prompt"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.Width != 512 || req.Height != 512 {
			t.Errorf("size = %dx%d", req.Width, req.Height)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	got, _, err := c.Generate(context.Background(), "a prompt", "512x512", storage.ModelSpec{}, storage.SamplingParams{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(got) != string(img) {
		t.Fatalf("got %q", got)
	}
}

func TestGenerateModelSpec(t *testing.T) {
	img := []byte("fake-png-bytes")
	var got struct {
		Model string            `json:"model"`
		Loras []storage.LoraRef `json:"loras"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	spec := storage.ModelSpec{
		Model: "/m/dev",
		Loras: []storage.LoraRef{{Name: "shauray/flux-uncensored-lora", Scale: 0.8}},
	}
	if _, _, err := c.Generate(context.Background(), "a cat", "512x512", spec, storage.SamplingParams{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Model != "/m/dev" {
		t.Fatalf("model = %q", got.Model)
	}
	if len(got.Loras) != 1 || got.Loras[0].Name != "shauray/flux-uncensored-lora" || got.Loras[0].Scale != 0.8 {
		t.Fatalf("loras = %+v", got.Loras)
	}
}

func TestGenerateOmitsEmptySpec(t *testing.T) {
	img := []byte("fake-png-bytes")
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	if _, _, err := c.Generate(context.Background(), "a cat", "512x512", storage.ModelSpec{}, storage.SamplingParams{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, ok := got["model"]; ok {
		t.Fatalf("empty spec must omit model, got %+v", got)
	}
	if _, ok := got["loras"]; ok {
		t.Fatalf("empty spec must omit loras, got %+v", got)
	}
}

func TestGenerateBadSize(t *testing.T) {
	c := New("http://unused")
	c.ImageURL = "http://unused"
	if _, _, err := c.Generate(context.Background(), "p", "bogus", storage.ModelSpec{}, storage.SamplingParams{}); err == nil {
		t.Fatal("expected error for malformed size")
	}
}

func TestGenerateSamplingParams(t *testing.T) {
	img := []byte("fake-png-bytes")
	var got struct {
		Seed           *int64   `json:"seed"`
		Steps          *int     `json:"steps"`
		Guidance       *float64 `json:"guidance"`
		NegativePrompt string   `json:"negative_prompt"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `","seed":1234}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL

	seed := int64(42)
	steps := 30
	guidance := 3.5
	sp := storage.SamplingParams{Seed: &seed, Steps: &steps, Guidance: &guidance, NegativePrompt: "blurry, watermark"}
	_, used, err := c.Generate(context.Background(), "a cat", "512x512", storage.ModelSpec{}, sp)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Seed == nil || *got.Seed != 42 {
		t.Fatalf("seed = %v, want 42", got.Seed)
	}
	if got.Steps == nil || *got.Steps != 30 {
		t.Fatalf("steps = %v, want 30", got.Steps)
	}
	if got.Guidance == nil || *got.Guidance != 3.5 {
		t.Fatalf("guidance = %v, want 3.5", got.Guidance)
	}
	if got.NegativePrompt != "blurry, watermark" {
		t.Fatalf("negative_prompt = %q, want %q", got.NegativePrompt, "blurry, watermark")
	}
	// The sidecar-reported seed (1234) is returned, not the requested one.
	if used == nil || *used != 1234 {
		t.Fatalf("used seed = %v, want 1234", used)
	}
}

func TestGenerateOmitsEmptySamplingParams(t *testing.T) {
	img := []byte("fake-png-bytes")
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	if _, _, err := c.Generate(context.Background(), "a cat", "512x512", storage.ModelSpec{}, storage.SamplingParams{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, k := range []string{"seed", "steps", "guidance", "negative_prompt"} {
		if _, ok := got[k]; ok {
			t.Fatalf("empty sampling params must omit %q, got %+v", k, got)
		}
	}
}

func TestChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"polished"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.Chat(context.Background(), "some-model", nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got != "polished" {
		t.Fatalf("got %q", got)
	}
}

func TestGenerateNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	if _, _, err := c.Generate(context.Background(), "p", "512x512", storage.ModelSpec{}, storage.SamplingParams{}); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestEdit(t *testing.T) {
	img := []byte("fake-png")
	var got struct {
		Prompt    string  `json:"prompt"`
		Width     int     `json:"width"`
		Height    int     `json:"height"`
		InitImage string  `json:"init_image"`
		Strength  float64 `json:"strength"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/edit" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	out, _, err := c.Edit(context.Background(), "make it snow", "512x512", "aW1n", 0.6, storage.ModelSpec{}, storage.SamplingParams{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if string(out) != string(img) {
		t.Fatalf("got %q", out)
	}
	if got.InitImage != "aW1n" || got.Strength != 0.6 || got.Width != 512 || got.Height != 512 {
		t.Fatalf("body = %+v", got)
	}
}

func TestInpaint(t *testing.T) {
	img := []byte("fake-png")
	var got struct {
		Image string `json:"image"`
		Mask  string `json:"mask"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fill" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	if _, err := c.Inpaint(context.Background(), "add a cat", "aW1n", "bWFzaw=="); err != nil {
		t.Fatalf("Inpaint: %v", err)
	}
	if got.Image != "aW1n" || got.Mask != "bWFzaw==" {
		t.Fatalf("body = %+v", got)
	}
}

func TestInpaintUsesFillURL(t *testing.T) {
	img := []byte("fake-png")
	fillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fill" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer fillSrv.Close()

	// A server that must NOT receive the /fill request when FillURL is set.
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Inpaint hit ImageURL (%s) instead of FillURL", r.URL.Path)
	}))
	defer otherSrv.Close()

	c := New("http://unused")
	c.ImageURL = otherSrv.URL
	c.FillURL = fillSrv.URL
	if _, err := c.Inpaint(context.Background(), "add a cat", "aW1n", "bWFzaw=="); err != nil {
		t.Fatalf("Inpaint: %v", err)
	}
}

func TestBlendUsesReduxURL(t *testing.T) {
	img := []byte("fake-png")
	reduxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/redux" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer reduxSrv.Close()

	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Blend hit ImageURL (%s) instead of ReduxURL", r.URL.Path)
	}))
	defer otherSrv.Close()

	c := New("http://unused")
	c.ImageURL = otherSrv.URL
	c.ReduxURL = reduxSrv.URL
	if _, err := c.Blend(context.Background(), "a statue", "1024x1024", []string{"aW1n"}, []float64{0.8}); err != nil {
		t.Fatalf("Blend: %v", err)
	}
}

func TestUpscale(t *testing.T) {
	img := []byte("fake-png")
	var got struct {
		Image string `json:"image"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upscale" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	if _, err := c.Upscale(context.Background(), "aW1n"); err != nil {
		t.Fatalf("Upscale: %v", err)
	}
	if got.Image != "aW1n" {
		t.Fatalf("body = %+v", got)
	}
}

func TestUpscaleUsesUpscaleURL(t *testing.T) {
	img := []byte("fake-png")
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upscale" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer upSrv.Close()

	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Upscale hit ImageURL (%s) instead of UpscaleURL", r.URL.Path)
	}))
	defer otherSrv.Close()

	c := New("http://unused")
	c.ImageURL = otherSrv.URL
	c.UpscaleURL = upSrv.URL
	if _, err := c.Upscale(context.Background(), "aW1n"); err != nil {
		t.Fatalf("Upscale: %v", err)
	}
}

func TestBlend(t *testing.T) {
	img := []byte("fake-png")
	var got struct {
		Images    []string  `json:"images"`
		Strengths []float64 `json:"strengths"`
		Width     int       `json:"width"`
		Height    int       `json:"height"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/redux" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	_, err := c.Blend(context.Background(), "a statue", "1024x1024", []string{"aW1n"}, []float64{0.8})
	if err != nil {
		t.Fatalf("Blend: %v", err)
	}
	if len(got.Images) != 1 || got.Images[0] != "aW1n" || len(got.Strengths) != 1 || got.Strengths[0] != 0.8 {
		t.Fatalf("body = %+v", got)
	}
}

func TestResultReturnsBytes(t *testing.T) {
	png := []byte("fake-png-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/result/abc123" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	got, err := c.Result(context.Background(), "generate", "abc123", "")
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if string(got) != string(png) {
		t.Fatalf("got %q", got)
	}
}

func TestResultMissingIsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"no result"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	_, err := c.Result(context.Background(), "generate", "unknown", "")
	if !errors.Is(err, ErrResultNotFound) {
		t.Fatalf("want ErrResultNotFound, got %v", err)
	}
}

func TestResultTransportErrorDistinct(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed: connection refused

	c := New("http://unused")
	c.ImageURL = srv.URL
	_, err := c.Result(context.Background(), "generate", "x", "")
	if err == nil || errors.Is(err, ErrResultNotFound) {
		t.Fatalf("want transport error, got %v", err)
	}
}

func TestStatusReturnsRunningGenID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"state":"running","id":"gen42","step":3,"total":10}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	id, err := c.Status(context.Background(), "generate", "")
	if err != nil || id != "gen42" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestStatusIdleReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"idle"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	id, err := c.Status(context.Background(), "generate", "")
	if err != nil || id != "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestProgressSurfacesGenID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"running","id":"gen42","step":7,"total":10}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	step, total, genID, err := c.Progress(context.Background(), "generate", "")
	if err != nil || step != 7 || total != 10 || genID != "gen42" {
		t.Fatalf("step=%d total=%d id=%q err=%v", step, total, genID, err)
	}
}

func TestSidecarRouting(t *testing.T) {
	c := New("http://unused")
	c.ImageURL = "http://flux"
	c.SDXLImageURL = "http://sdxl"
	c.FillURL = "http://fill"
	cases := []struct {
		mode, sidecar, want string
	}{
		{"generate", "", "http://flux"},
		{"edit", "", "http://flux"},
		{"inpaint", "", "http://fill"},
		{"generate", "sdxl", "http://sdxl"},
		{"edit", "sdxl", "http://sdxl"},
		{"upscale", "", "http://flux"},
		{"generate", "unknown", "http://flux"},
	}
	for _, tc := range cases {
		if got := c.sidecarForMode(tc.mode, tc.sidecar); got != tc.want {
			t.Errorf("mode=%q sidecar=%q: got %q want %q", tc.mode, tc.sidecar, got, tc.want)
		}
	}
}

func TestGenerateRoutesToSdxlSidecar(t *testing.T) {
	img := []byte("fake-png-bytes")
	fluxHit := false
	flux := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fluxHit = true
		w.Write([]byte(`{"image":"should-not-be-here"}`))
	}))
	defer flux.Close()
	sdxl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" {
			t.Errorf("sdxl path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer sdxl.Close()

	c := New("http://unused")
	c.ImageURL = flux.URL
	c.SDXLImageURL = sdxl.URL
	spec := storage.ModelSpec{Model: "/m/sdxl/pony.safetensors", Sidecar: "sdxl"}
	got, _, err := c.Generate(context.Background(), "a prompt", "512x512", spec, storage.SamplingParams{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(got) != string(img) {
		t.Fatalf("got %q", got)
	}
	if fluxHit {
		t.Fatal("generation reached the mflux sidecar")
	}
}

func TestEditRoutesToSdxlSidecar(t *testing.T) {
	sdxl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/edit" {
			t.Errorf("sdxl path = %s", r.URL.Path)
		}
		var body struct {
			InitImage string `json:"init_image"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		if body.InitImage != "aW1n" {
			t.Errorf("init_image = %q", body.InitImage)
		}
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString([]byte("png")) + `"}`))
	}))
	defer sdxl.Close()

	c := New("http://unused")
	c.ImageURL = "http://flux-should-not-be-hit"
	c.SDXLImageURL = sdxl.URL
	spec := storage.ModelSpec{Sidecar: "sdxl"}
	if _, _, err := c.Edit(context.Background(), "a prompt", "512x512", "aW1n", 0.4, spec, storage.SamplingParams{}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
}

func TestProgressRoutesToSdxlSidecar(t *testing.T) {
	fluxHit := false
	flux := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fluxHit = true
		w.Write([]byte(`{"state":"idle"}`))
	}))
	defer flux.Close()
	sdxl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"running","id":"gen7","step":3,"total":20}`))
	}))
	defer sdxl.Close()

	c := New("http://unused")
	c.ImageURL = flux.URL
	c.SDXLImageURL = sdxl.URL
	step, total, genID, err := c.Progress(context.Background(), "generate", "sdxl")
	if err != nil || step != 3 || total != 20 || genID != "gen7" {
		t.Fatalf("step=%d total=%d id=%q err=%v", step, total, genID, err)
	}
	if fluxHit {
		t.Fatal("progress polled the mflux sidecar")
	}
}
