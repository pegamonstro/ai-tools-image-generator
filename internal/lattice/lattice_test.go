package lattice

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
		Seed     *int64   `json:"seed"`
		Steps    *int     `json:"steps"`
		Guidance *float64 `json:"guidance"`
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
	sp := storage.SamplingParams{Seed: &seed, Steps: &steps, Guidance: &guidance}
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
	for _, k := range []string{"seed", "steps", "guidance"} {
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
