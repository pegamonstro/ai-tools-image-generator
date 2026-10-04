package genres

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "genres.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeTemp(t, `{
  "version": 1,
  "genres": {
    "landscape": {
      "label": "Landscape",
      "fields": [
        {"key": "setting", "label": "Setting", "type": "text", "placeholder": "valley"},
        {"key": "time", "label": "Time", "type": "select", "options": ["dawn", "dusk"], "default": "dawn"}
      ],
      "prompt_template": "A landscape at {time}: {setting}.",
      "sizes": ["768x512", "512x512"]
    }
  }
}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Genres) != 1 {
		t.Fatalf("want 1 genre, got %d", len(c.Genres))
	}
	g, ok := c.Genre("landscape")
	if !ok {
		t.Fatal("landscape genre missing")
	}
	if len(g.Fields) != 2 {
		t.Fatalf("want 2 fields, got %d", len(g.Fields))
	}
}

func TestLoadRejectsUnknownType(t *testing.T) {
	p := writeTemp(t, `{"version":1,"genres":{"x":{"label":"X","fields":[{"key":"a","label":"A","type":"bogus"}],"prompt_template":"{a}","sizes":["512x512"]}}}`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown field type")
	}
}

func TestLoadRejectsDuplicateKey(t *testing.T) {
	p := writeTemp(t, `{"version":1,"genres":{"x":{"label":"X","fields":[{"key":"a","label":"A","type":"text"},{"key":"a","label":"A","type":"text"}],"prompt_template":"{a}","sizes":["512x512"]}}}`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for duplicate key")
	}
}

func TestLoadRejectsBadSize(t *testing.T) {
	p := writeTemp(t, `{"version":1,"genres":{"x":{"label":"X","fields":[],"prompt_template":"hi","sizes":["wide"]}}}`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for bad size")
	}
}

func TestLoadRejectsSelectWithoutOptions(t *testing.T) {
	p := writeTemp(t, `{"version":1,"genres":{"x":{"label":"X","fields":[{"key":"a","label":"A","type":"select"}],"prompt_template":"{a}","sizes":["512x512"]}}}`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for select without options")
	}
}
