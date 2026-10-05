package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

// newTestHandler builds a handler backed by a fast mock lattice server.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	img := []byte("fake-png-bytes")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/generate":
			w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
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
		case "/generate":
			w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
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
		case "/generate":
			w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
		case "/edit":
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
