// Package models loads the local model/LoRA catalog (models.json).
package models

import (
	"encoding/json"
	"os"
)

type Entry struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value string `json:"value"`
}

type Catalog struct {
	Models []Entry `json:"models"`
	Loras  []Entry `json:"loras"`
}

// Load reads a catalog JSON file. A missing file yields an empty catalog
// (generation still works on the sidecar's default model), not an error.
func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Catalog{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// LookupModel returns the mflux --model value for a catalog key.
func (c *Catalog) LookupModel(key string) (string, bool) {
	return lookup(c.Models, key)
}

// LookupLora returns the mflux --lora value (path or HF id) for a catalog key.
func (c *Catalog) LookupLora(key string) (string, bool) {
	return lookup(c.Loras, key)
}

func lookup(entries []Entry, key string) (string, bool) {
	if key == "" {
		return "", false
	}
	for _, e := range entries {
		if e.Key == key {
			return e.Value, true
		}
	}
	return "", false
}
