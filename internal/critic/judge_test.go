package critic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"img-gen/internal/lattice"
)

func TestChatJudgeValidReply(t *testing.T) {
	var callCount int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		// Verify the model is threaded through
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "judge:7b" {
			t.Errorf("model not threaded: %v", body["model"])
		}
		// Verify criteria order preserved by id in user text
		msgs := body["messages"].([]any)
		user := msgs[1].(map[string]any)
		parts := user["content"].([]any)
		textPart := parts[0].(map[string]any)
		userText := textPart["text"].(string)
		if !strings.Contains(userText, "0. first criterion") || !strings.Contains(userText, "1. second criterion") {
			t.Errorf("criteria order not preserved in user text: %s", userText)
		}
		// Return valid JSON array with correct order
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[{"id":0,"pass":true,"reason":"ok","fix":""},{"id":1,"pass":false,"reason":"bad","fix":"improve"}]`}}},
		})
	}))
	defer ts.Close()

	c := lattice.New(ts.URL)
	judge := ChatJudge(c, "judge:7b")

	scores, err := judge(context.Background(), []string{"first criterion", "second criterion"}, "QUJD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(scores) != 2 {
		t.Fatalf("expected 2 scores, got %d", len(scores))
	}
	if scores[0].ID != 0 || scores[0].Pass != true || scores[0].Reason != "ok" || scores[0].Fix != "" {
		t.Errorf("score[0] mismatch: %+v", scores[0])
	}
	if scores[1].ID != 1 || scores[1].Pass != false || scores[1].Reason != "bad" || scores[1].Fix != "improve" {
		t.Errorf("score[1] mismatch: %+v", scores[1])
	}
	if atomic.LoadInt64(&callCount) != 1 {
		t.Errorf("expected 1 call, got %d", callCount)
	}
}

func TestChatJudgeMalformedReply(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `not json`}}},
		})
	}))
	defer ts.Close()

	c := lattice.New(ts.URL)
	judge := ChatJudge(c, "judge:7b")

	_, err := judge(context.Background(), []string{"criterion"}, "QUJD")
	if err == nil {
		t.Fatal("expected error for malformed reply")
	}
	if !errors.Is(err, ErrJudgeUnreadable) {
		t.Errorf("expected ErrJudgeUnreadable, got %v", err)
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "reply head:") {
		t.Errorf("error missing reply head truncation: %s", errStr)
	}
	if !strings.Contains(errStr, "not json") {
		t.Errorf("error missing reply head content: %s", errStr)
	}
}

func TestChatJudgeIncompleteReply(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Well-formed JSON but incomplete: only 1 score for 2 criteria
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[{"id":0,"pass":true,"reason":"ok"}]`}}},
		})
	}))
	defer ts.Close()

	c := lattice.New(ts.URL)
	judge := ChatJudge(c, "judge:7b")

	_, err := judge(context.Background(), []string{"first", "second"}, "QUJD")
	if err == nil {
		t.Fatal("expected error for incomplete reply")
	}
	if !errors.Is(err, ErrJudgeUnreadable) {
		t.Errorf("expected ErrJudgeUnreadable for incomplete reply, got %v", err)
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "reply head:") {
		t.Errorf("error missing reply head truncation: %s", errStr)
	}
}

func TestChatJudgeEmptyCriteria(t *testing.T) {
	var callCount int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[]`}}},
		})
	}))
	defer ts.Close()

	c := lattice.New(ts.URL)
	judge := ChatJudge(c, "judge:7b")

	scores, err := judge(context.Background(), []string{}, "QUJD")
	if err != nil {
		t.Fatalf("unexpected error for empty criteria: %v", err)
	}
	if len(scores) != 0 {
		t.Errorf("expected empty scores for empty criteria, got %d", len(scores))
	}
	if atomic.LoadInt64(&callCount) != 0 {
		t.Errorf("expected 0 calls for empty criteria, got %d", callCount)
	}
}

func TestChatJudgeNilCheck(t *testing.T) {
	var j Judge
	if !j.isNil() {
		t.Error("nil Judge should return true for isNil()")
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[]`}}},
		})
	}))
	defer ts.Close()
	c := lattice.New(ts.URL)
	j = ChatJudge(c, "judge:7b")
	if j.isNil() {
		t.Error("non-nil Judge should return false for isNil()")
	}
}
