package lattice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatVisionSendsContentParts(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["stream"] != false {
			t.Errorf("stream must be explicit false")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[{"id":0,"pass":true}]`}}},
		})
	}))
	defer ts.Close()
	c := New(ts.URL)
	out, err := c.ChatVision(context.Background(), "judge:7b", "be strict", "criteria: shape", "QUJD")
	if err != nil {
		t.Fatal(err)
	}
	if out != `[{"id":0,"pass":true}]` {
		t.Fatalf("reply = %q", out)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be strict" {
		t.Fatalf("system = %#v", sys)
	}
	user := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatal("user role")
	}
	parts := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	if tp := parts[0].(map[string]any); tp["type"] != "text" || tp["text"] != "criteria: shape" {
		t.Fatalf("text part = %#v", tp)
	}
	ip := parts[1].(map[string]any)
	if ip["type"] != "image_url" {
		t.Fatal("image part type")
	}
	url := ip["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,QUJD") {
		t.Fatalf("data url = %q", url)
	}
	if got["model"] != "judge:7b" {
		t.Fatalf("model = %#v", got["model"])
	}
}

func TestChatVisionNamesUpgradeOnPreUpgradeGateway(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"direct 400", http.StatusBadRequest, `{"error":{"message":"unknown field content"}}`},
		{"collapsed 500 (the real pre-upgrade shape)", http.StatusInternalServerError, `{"error":"ollama returned status 400"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.code)
				_, _ = w.Write([]byte(c.body))
			}))
			defer ts.Close()
			_, err := New(ts.URL).ChatVision(context.Background(), "m", "s", "u", "QUJD")
			if err == nil || !strings.Contains(err.Error(), "image content") {
				t.Fatalf("want named upgrade error, got %v", err)
			}
		})
	}
}
