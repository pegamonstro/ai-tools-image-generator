package lattice

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerate(t *testing.T) {
	img := []byte("fake-png-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(img) + `"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.Generate(context.Background(), "a prompt", "512x512")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(got) != string(img) {
		t.Fatalf("got %q", got)
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

	c := New(srv.URL)
	if _, err := c.Generate(context.Background(), "p", "512x512"); err == nil {
		t.Fatal("expected error on 500")
	}
}
