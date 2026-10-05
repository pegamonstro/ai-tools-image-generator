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
