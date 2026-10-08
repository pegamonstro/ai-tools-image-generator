package models

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.json")
	if err := os.WriteFile(p, []byte(`{"models":[{"key":"dev","label":"FLUX.1-dev","value":"/m/dev"}],"loras":[{"key":"uncensored","label":"Uncensored","value":"shauray/flux-uncensored-lora"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Models) != 1 || c.Models[0].Key != "dev" || c.Models[0].Value != "/m/dev" {
		t.Fatalf("models: %+v", c.Models)
	}
	if len(c.Loras) != 1 || c.Loras[0].Value != "shauray/flux-uncensored-lora" {
		t.Fatalf("loras: %+v", c.Loras)
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if c == nil || len(c.Models) != 0 || len(c.Loras) != 0 {
		t.Fatalf("want empty catalog, got %+v", c)
	}
}

func TestLoadBadJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("want error for bad JSON")
	}
}

func TestLookupSidecar(t *testing.T) {
	c := &Catalog{Models: []Entry{
		{Key: "pony", Label: "Pony V6 XL", Value: "/m/sdxl/pony.safetensors", Sidecar: "sdxl"},
		{Key: "persephone", Label: "Persephone 2.0", Value: "/m/persephone-4bit"},
	}}
	if got := c.LookupSidecar("pony"); got != "sdxl" {
		t.Fatalf("by key: got %q", got)
	}
	if got := c.LookupSidecar("/m/sdxl/pony.safetensors"); got != "sdxl" {
		t.Fatalf("by raw value: got %q", got)
	}
	if got := c.LookupSidecar("persephone"); got != "" {
		t.Fatalf("entry without sidecar: got %q", got)
	}
	if got := c.LookupSidecar("nope"); got != "" {
		t.Fatalf("unknown key: got %q", got)
	}
}

func TestLoadParsesSidecar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(p, []byte(
		`{"models":[{"key":"pony","label":"Pony","value":"/m/sdxl/pony.safetensors","sidecar":"sdxl"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Models) != 1 || c.Models[0].Sidecar != "sdxl" {
		t.Fatalf("sidecar not parsed: %+v", c.Models)
	}
}
