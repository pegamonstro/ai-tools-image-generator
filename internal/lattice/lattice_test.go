package lattice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
	got, err := c.Generate(context.Background(), "a prompt", "512x512")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(got) != string(img) {
		t.Fatalf("got %q", got)
	}
}

func TestGenerateBadSize(t *testing.T) {
	c := New("http://unused")
	c.ImageURL = "http://unused"
	if _, err := c.Generate(context.Background(), "p", "bogus"); err == nil {
		t.Fatal("expected error for malformed size")
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
	if _, err := c.Generate(context.Background(), "p", "512x512"); err == nil {
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
	out, err := c.Edit(context.Background(), "make it snow", "512x512", "aW1n", 0.6)
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
