package presets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndByKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "presets.json")
	raw := `{"presets":[{"key":"aria","label":"Aria","trigger":"aria, silver hair","model":"/m/persephone","loras":[{"name":"alvdansen/illustration-1.0-flux-dev","scale":0.8}],"negative_prompt":"blurry","steps":24,"guidance":3.5}]}`
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := c.ByKey("aria")
	if !ok {
		t.Fatal("aria not found")
	}
	if a.Trigger != "aria, silver hair" || a.Model != "/m/persephone" || len(a.Loras) != 1 {
		t.Fatalf("aria = %+v", a)
	}
	if a.Loras[0].Name != "alvdansen/illustration-1.0-flux-dev" || a.Loras[0].Scale != 0.8 {
		t.Fatalf("aria loras = %+v", a.Loras)
	}
	if a.Steps == nil || *a.Steps != 24 || a.Guidance == nil || *a.Guidance != 3.5 {
		t.Fatalf("aria sampling = %+v / %+v", a.Steps, a.Guidance)
	}
	if a.NegativePrompt != "blurry" {
		t.Fatalf("aria negative = %q", a.NegativePrompt)
	}
	if _, ok := c.ByKey("missing"); ok {
		t.Fatal("missing should not resolve")
	}
}

func TestLoadMissingFileYieldsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(c.Presets) != 0 {
		t.Fatalf("want empty catalog, got %+v", c.Presets)
	}
}

func TestByKeyEmptyCatalog(t *testing.T) {
	var c Catalog
	if _, ok := c.ByKey("anything"); ok {
		t.Fatal("empty catalog must not resolve")
	}
}
