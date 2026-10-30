package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeGenres(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "genres.json")
	content := `{
  "version": 1,
  "genres": {
    "landscape": {
      "label": "Landscape",
      "fields": [{"key": "setting", "label": "Setting", "type": "text", "required": true}],
      "prompt_template": "A landscape: {setting}.",
      "sizes": ["512x512"]
    }
  }
}`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeModels(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "models.json")
	content := `{
  "models": [{"key": "dev", "label": "FLUX.1-dev", "value": "/m/dev"}],
  "loras": [{"key": "uncensored", "label": "Uncensored", "value": "shauray/flux-uncensored-lora"}]
}`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writePresets(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "presets.json")
	content := `{
  "presets": [{"key": "aria", "label": "Aria", "trigger": "aria, silver hair"}]
}`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// latticeImageReply is the frontend's OpenAI Images response for one fake PNG.
func latticeImageReply(img []byte) string {
	return `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(img) + `","seed":123}]}`
}

// newTestHandler builds a handler backed by a fast mock lattice server.
// ImageRouting stays the production default (lattice), so the mock serves the
// frontend's OpenAI Images routes — the golden path the runtime actually walks.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	img := []byte("fake-png-bytes")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations", "/v1/images/edits":
			w.Write([]byte(latticeImageReply(img)))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"enhanced prompt"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)

	cfg := config{
		LatticeURL:    mock.URL,
		ImageURL:      mock.URL,
		DataDir:       t.TempDir(),
		GenresFile:    writeGenres(t),
		EnhanceModel:  "flux-dev",
		EnhanceSystem: "sys",
		ImageTimeout:  5 * time.Second,
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestEndToEnd(t *testing.T) {
	img := []byte("fake-png-bytes")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			w.Write([]byte(latticeImageReply(img)))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"enhanced prompt"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	dataDir := t.TempDir()
	cfg := config{
		LatticeURL:    mock.URL,
		ImageURL:      mock.URL,
		DataDir:       dataDir,
		GenresFile:    writeGenres(t),
		EnhanceModel:  "flux-dev",
		EnhanceSystem: "sys",
		ImageTimeout:  5 * time.Second,
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Submit a job.
	body := []byte(`{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`)
	resp, err := http.Post(srv.URL+"/api/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sub struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		t.Fatal(err)
	}
	if sub.JobID == "" {
		t.Fatal("empty job_id")
	}

	// Poll until done.
	deadline := time.Now().Add(5 * time.Second)
	var job struct {
		Status    string `json:"status"`
		Prompt    string `json:"prompt"`
		ImagePath string `json:"image_path"`
	}
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/jobs/" + sub.JobID)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(r.Body).Decode(&job)
		r.Body.Close()
		if job.Status == "done" || job.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != "done" {
		t.Fatalf("status = %q", job.Status)
	}
	if job.Prompt != "A landscape: a valley." {
		t.Fatalf("prompt = %q", job.Prompt)
	}

	// Fetch the image.
	r, err := http.Get(srv.URL + "/api/images/" + sub.JobID + ".png")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	got, _ := io.ReadAll(r.Body)
	if string(got) != string(img) {
		t.Fatalf("image bytes = %q", got)
	}

	// History list contains the job.
	r, _ = http.Get(srv.URL + "/api/jobs")
	var list []map[string]any
	json.NewDecoder(r.Body).Decode(&list)
	r.Body.Close()
	if len(list) == 0 {
		t.Fatal("empty job list")
	}

	// genres endpoint works.
	r, _ = http.Get(srv.URL + "/api/genres")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("genres status = %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestModelsAndExport(t *testing.T) {
	img := []byte("fake-png-bytes")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			w.Write([]byte(latticeImageReply(img)))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"enhanced prompt"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	cfg := config{
		LatticeURL:    mock.URL,
		ImageURL:      mock.URL,
		DataDir:       t.TempDir(),
		GenresFile:    writeGenres(t),
		ModelsFile:    writeModels(t),
		PresetsFile:   writePresets(t),
		EnhanceModel:  "flux-dev",
		EnhanceSystem: "sys",
		ImageTimeout:  5 * time.Second,
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// /api/models returns the catalog.
	r, err := http.Get(srv.URL + "/api/models")
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Models []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"models"`
		Loras []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"loras"`
	}
	json.NewDecoder(r.Body).Decode(&cat)
	r.Body.Close()
	if len(cat.Models) != 1 || cat.Models[0].Value != "/m/dev" {
		t.Fatalf("models = %+v", cat.Models)
	}
	if len(cat.Loras) != 1 || cat.Loras[0].Value != "shauray/flux-uncensored-lora" {
		t.Fatalf("loras = %+v", cat.Loras)
	}

	// /api/presets returns the preset catalog.
	r2, err := http.Get(srv.URL + "/api/presets")
	if err != nil {
		t.Fatal(err)
	}
	var pcat struct {
		Presets []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"presets"`
	}
	json.NewDecoder(r2.Body).Decode(&pcat)
	r2.Body.Close()
	if len(pcat.Presets) != 1 || pcat.Presets[0].Key != "aria" || pcat.Presets[0].Label != "Aria" {
		t.Fatalf("presets = %+v", pcat.Presets)
	}

	// Generate one image, then export it to a temp dir.
	resp, err := http.Post(srv.URL+"/api/jobs", "application/json",
		bytes.NewReader([]byte(`{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512","model":"/m/dev"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var sub struct {
		JobID string `json:"job_id"`
	}
	json.NewDecoder(resp.Body).Decode(&sub)
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	var job struct {
		Status string `json:"status"`
		Model  string `json:"model"`
	}
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/jobs/" + sub.JobID)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(r.Body).Decode(&job)
		r.Body.Close()
		if job.Status == "done" || job.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != "done" {
		t.Fatalf("status = %q", job.Status)
	}
	if job.Model != "/m/dev" {
		t.Fatalf("model = %q, want /m/dev", job.Model)
	}

	dest := filepath.Join(t.TempDir(), "out")
	resp, err = http.Post(srv.URL+"/api/export", "application/json",
		bytes.NewReader([]byte(`{"ids":["`+sub.JobID+`"],"dest":"`+dest+`"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Copied  []string `json:"copied"`
		Skipped []string `json:"skipped"`
	}
	json.NewDecoder(resp.Body).Decode(&exp)
	resp.Body.Close()
	if len(exp.Copied) != 1 || len(exp.Skipped) != 0 {
		t.Fatalf("export = %+v", exp)
	}
	if _, err := os.Stat(exp.Copied[0]); err != nil {
		t.Fatalf("copied file missing: %v", err)
	}
}

func TestSubmitBadGenre(t *testing.T) {
	cfg := config{GenresFile: writeGenres(t), DataDir: t.TempDir()}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := []byte(`{"genre":"nope","fields":{},"size":"512x512"}`)
	resp, _ := http.Post(srv.URL+"/api/jobs", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestServeIndex(t *testing.T) {
	cfg := config{GenresFile: writeGenres(t), DataDir: t.TempDir()}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "<html") {
		t.Fatal("index.html not served")
	}
}

func TestImagePathTraversal(t *testing.T) {
	h := newTestHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{
		"/api/images/../../etc/passwd.png",
		"/api/images/ZZZZZZZZZZZZZZZZ.png",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestSSEStreamsDoneEvent(t *testing.T) {
	h := newTestHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Submit a job.
	body := []byte(`{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`)
	resp, err := http.Post(srv.URL+"/api/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sub struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		t.Fatal(err)
	}
	if sub.JobID == "" {
		t.Fatal("empty job_id")
	}

	// Open the SSE stream and read until the server closes it after the
	// terminal event.
	client := &http.Client{Timeout: 5 * time.Second}
	stream, err := client.Get(srv.URL + "/api/jobs/" + sub.JobID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if ct := stream.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	got, err := io.ReadAll(stream.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"status":"done"`) {
		t.Fatalf("SSE stream missing done event: %q", got)
	}
	if strings.Contains(string(got), `"status":"failed"`) {
		t.Fatalf("SSE stream reported failure: %q", got)
	}
}

func TestSSEUnknownJob(t *testing.T) {
	h := newTestHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/jobs/0000000000000000/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEditRoundTrip(t *testing.T) {
	img := []byte("fake-png-bytes")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/edits":
			w.Write([]byte(latticeImageReply(img)))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"enhanced prompt"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	cfg := config{
		LatticeURL:    mock.URL,
		ImageURL:      mock.URL,
		DataDir:       t.TempDir(),
		GenresFile:    writeGenres(t),
		EnhanceModel:  "flux-dev",
		EnhanceSystem: "sys",
		ImageTimeout:  5 * time.Second,
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := []byte(`{"mode":"edit","prompt":"make it snow","size":"512x512","image":"aW1n"}`)
	resp, err := http.Post(srv.URL+"/api/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sub struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		t.Fatal(err)
	}
	if sub.JobID == "" {
		t.Fatal("empty job_id")
	}

	deadline := time.Now().Add(5 * time.Second)
	var job struct {
		Mode   string `json:"mode"`
		Status string `json:"status"`
	}
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/jobs/" + sub.JobID)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(r.Body).Decode(&job)
		r.Body.Close()
		if job.Status == "done" || job.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != "done" {
		t.Fatalf("status = %q", job.Status)
	}
	if job.Mode != "edit" {
		t.Fatalf("mode = %q, want edit", job.Mode)
	}
}

// The legacy route stays operable as a rollback: IMAGE_ROUTING=direct sends
// generate to the sidecar's /generate (the direct dialect) instead of the
// frontend's OpenAI Images route.
func TestDirectImageRouting(t *testing.T) {
	img := []byte("fake-png-bytes")
	sawPath := ""
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		switch r.URL.Path {
		case "/generate":
			w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"enhanced prompt"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	cfg := config{
		LatticeURL:    mock.URL,
		ImageRouting:  "direct",
		ImageURL:      mock.URL,
		DataDir:       t.TempDir(),
		GenresFile:    writeGenres(t),
		EnhanceModel:  "flux-dev",
		EnhanceSystem: "sys",
		ImageTimeout:  5 * time.Second,
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := []byte(`{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`)
	resp, err := http.Post(srv.URL+"/api/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var sub struct {
		JobID string `json:"job_id"`
	}
	json.NewDecoder(resp.Body).Decode(&sub)
	resp.Body.Close()
	if sub.JobID == "" {
		t.Fatal("empty job_id")
	}

	deadline := time.Now().Add(5 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/jobs/" + sub.JobID)
		if err != nil {
			t.Fatal(err)
		}
		var job struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&job)
		r.Body.Close()
		if job.Status == "done" || job.Status == "failed" {
			status = job.Status
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status != "done" {
		t.Fatalf("status = %q", status)
	}
	if sawPath != "/generate" {
		t.Fatalf("path = %q, want /generate in direct mode", sawPath)
	}
}

func TestLegacyDeleteJob(t *testing.T) {
	h := newTestHandler(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := []byte(`{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`)
	resp, err := http.Post(srv.URL+"/api/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var sub struct {
		JobID string `json:"job_id"`
	}
	json.NewDecoder(resp.Body).Decode(&sub)
	resp.Body.Close()
	if sub.JobID == "" {
		t.Fatal("no job id")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := http.Get(srv.URL + "/api/jobs/" + sub.JobID)
		if err != nil {
			t.Fatal(err)
		}
		var j map[string]any
		json.NewDecoder(r.Body).Decode(&j)
		r.Body.Close()
		if j["status"] == "done" || j["status"] == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished: %v", j)
		}
		time.Sleep(10 * time.Millisecond)
	}

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/jobs/"+sub.JobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()
	var out map[string]any
	json.NewDecoder(del.Body).Decode(&out)
	if del.StatusCode != http.StatusOK || out["status"] != "deleted" {
		t.Fatalf("delete = %d %v", del.StatusCode, out)
	}
	get, err := http.Get(srv.URL + "/api/jobs/" + sub.JobID)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d", get.StatusCode)
	}
	req2, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/jobs/"+sub.JobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	del2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer del2.Body.Close()
	if del2.StatusCode != http.StatusNotFound {
		t.Fatalf("repeat delete = %d", del2.StatusCode)
	}
}

// TestLoadConfigCriticEnvs pins the env-derived critic config: CRITIC_MODEL
// defaults to the full judge model tag (control-plane values pass through
// verbatim), CRITIC_TIMEOUT_S to 120, TAGGER_URL to empty (the tag ops then
// fail submission honestly) and TAGGER_TIMEOUT_S to 60.
func TestLoadConfigCriticEnvs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("CRITIC_MODEL", "")
		t.Setenv("CRITIC_TIMEOUT_S", "")
		t.Setenv("TAGGER_URL", "")
		t.Setenv("TAGGER_TIMEOUT_S", "")
		cfg := loadConfig()
		if cfg.CriticModel != "huihui_ai/qwen2.5-vl-abliterated:latest" {
			t.Fatalf("CriticModel = %q", cfg.CriticModel)
		}
		if cfg.CriticTimeout != 120*time.Second {
			t.Fatalf("CriticTimeout = %s", cfg.CriticTimeout)
		}
		if cfg.TaggerURL != "" {
			t.Fatalf("TaggerURL = %q", cfg.TaggerURL)
		}
		if cfg.TaggerTimeout != 60*time.Second {
			t.Fatalf("TaggerTimeout = %s", cfg.TaggerTimeout)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("CRITIC_MODEL", "huihui_ai/qwen3-vl-abliterated:latest")
		t.Setenv("CRITIC_TIMEOUT_S", "45")
		t.Setenv("TAGGER_URL", "http://127.0.0.1:8904")
		t.Setenv("TAGGER_TIMEOUT_S", "30")
		cfg := loadConfig()
		if cfg.CriticModel != "huihui_ai/qwen3-vl-abliterated:latest" {
			t.Fatalf("CriticModel = %q", cfg.CriticModel)
		}
		if cfg.CriticTimeout != 45*time.Second {
			t.Fatalf("CriticTimeout = %s", cfg.CriticTimeout)
		}
		if cfg.TaggerURL != "http://127.0.0.1:8904" {
			t.Fatalf("TaggerURL = %q", cfg.TaggerURL)
		}
		if cfg.TaggerTimeout != 30*time.Second {
			t.Fatalf("TaggerTimeout = %s", cfg.TaggerTimeout)
		}
	})

	t.Run("zero parses fall back to the default", func(t *testing.T) {
		// 0 would mean an already-expired deadline for the sync validate
		// call and instant tagger timeouts — non-positive parses are errors,
		// so they fall back rather than break the servers they set.
		t.Setenv("CRITIC_TIMEOUT_S", "0")
		t.Setenv("TAGGER_TIMEOUT_S", "-4")
		cfg := loadConfig()
		if cfg.CriticTimeout != 120*time.Second || cfg.TaggerTimeout != 60*time.Second {
			t.Fatalf("timeouts = %s, %s; want the 120s/60s defaults", cfg.CriticTimeout, cfg.TaggerTimeout)
		}
	})
}

// TestV1ValidateCriticWiring proves the config-built critic env reaches the
// v1 sync endpoint: with production-default config (no tagger configured) a
// deterministic tile check still evaluates and passes. Before the critic env
// was wired the endpoint ran on a nil-leg env — pre-C2 that panicked.
func TestV1ValidateCriticWiring(t *testing.T) {
	h := newTestHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	img := image.NewNRGBA(image.Rect(0, 0, 4, 4)) // uniform pixels -> tile pass
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"image":  base64.StdEncoding.EncodeToString(buf.Bytes()),
		"checks": []map[string]any{{"op": "tile_conform"}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/api/v1/validate", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var out struct {
		Verdict string `json:"verdict"`
		Checks  []struct {
			Op     string `json:"op"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Verdict != "pass" {
		t.Fatalf("verdict = %q (%+v) — critic env not wired through to the sync endpoint", out.Verdict, out.Checks)
	}
	if len(out.Checks) != 1 || out.Checks[0].Op != "tile_conform" || out.Checks[0].Status != "pass" {
		t.Fatalf("checks = %+v", out.Checks)
	}
}
