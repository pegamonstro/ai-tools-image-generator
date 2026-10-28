package lattice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func resetTagVocabCache() {
	tagVocabMu.Lock()
	tagVocabCached = nil
	tagVocabMu.Unlock()
}

func TestTaggerSendsImageAndDecodesTags(t *testing.T) {
	resetTagVocabCache()
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["image"] != "QUJD" {
			t.Errorf("image body = %v", got["image"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tags": []any{
				map[string]any{"name": "cat", "score": 0.95},
				map[string]any{"name": "animal", "score": 0.87},
			},
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	c.TaggerURL = ts.URL
	tags, err := c.Tagger(context.Background(), "QUJD")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("want 2 tags, got %d", len(tags))
	}
	if tags[0].Name != "cat" || tags[0].Score != 0.95 {
		t.Fatalf("tag 0 = %+v", tags[0])
	}
	if tags[1].Name != "animal" || tags[1].Score != 0.87 {
		t.Fatalf("tag 1 = %+v", tags[1])
	}
}

func TestTaggerVocabCachedOnSecondCall(t *testing.T) {
	resetTagVocabCache()
	var callCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tags": []string{"tag1", "tag2", "tag3"},
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	c.TaggerURL = ts.URL

	// First call
	v1, err := c.TaggerVocab(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(v1) != 3 {
		t.Fatalf("first vocab len = %d", len(v1))
	}

	// Second call should hit cache, not server
	v2, err := c.TaggerVocab(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(v2) != 3 {
		t.Fatalf("second vocab len = %d", len(v2))
	}

	if callCount.Load() != 1 {
		t.Fatalf("vocab handler called %d times, want 1", callCount.Load())
	}
}

func TestTaggerVocabRetriesOnFailure(t *testing.T) {
	resetTagVocabCache()
	var callCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		if callCount.Load() == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("server error"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tags": []string{"retry-tag"},
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	c.TaggerURL = ts.URL

	// First call fails
	_, err := c.TaggerVocab(context.Background())
	if err == nil {
		t.Fatal("first call should fail")
	}

	// Second call should retry and succeed
	v2, err := c.TaggerVocab(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(v2) != 1 || v2[0] != "retry-tag" {
		t.Fatalf("retry vocab = %+v", v2)
	}

	if callCount.Load() != 2 {
		t.Fatalf("vocab handler called %d times, want 2", callCount.Load())
	}
}

func TestTaggerErrorPrefix(t *testing.T) {
	resetTagVocabCache()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer ts.Close()

	c := New(ts.URL)
	c.TaggerURL = ts.URL
	_, err := c.Tagger(context.Background(), "QUJD")
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "tagger sidecar:") {
		t.Fatalf("error prefix missing: %v", err)
	}
}

func TestTaggerVocabErrorPrefix(t *testing.T) {
	resetTagVocabCache()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	c := New(ts.URL)
	c.TaggerURL = ts.URL
	_, err := c.TaggerVocab(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "tagger sidecar") {
		t.Fatalf("error prefix missing: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
