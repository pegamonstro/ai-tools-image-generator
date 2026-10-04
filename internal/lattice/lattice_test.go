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
