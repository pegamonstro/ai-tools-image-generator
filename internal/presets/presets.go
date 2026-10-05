// Package presets loads the character-preset catalog (presets.json).
//
// A preset bundles the choices that give a character a consistent identity —
// a trigger phrase, an optional base model, style LoRAs, a global style, a
// negative prompt, and sampling defaults. The queue applies these as defaults
// (an explicit request field wins) and prepends the trigger to the prompt.
package presets

import (
	"encoding/json"
	"os"

	"img-gen/internal/storage"
)

type Preset struct {
	Key            string            `json:"key"`
	Label          string            `json:"label"`
	Trigger        string            `json:"trigger"`         // prepended to the prompt (character identity)
	Model          string            `json:"model,omitempty"` // mflux --model; "" = sidecar default
	Loras          []storage.LoraRef `json:"loras,omitempty"`
	Style          string            `json:"style,omitempty"` // global style key; "" = none
	NegativePrompt string            `json:"negative_prompt,omitempty"`
	Steps          *int              `json:"steps,omitempty"`
	Guidance       *float64          `json:"guidance,omitempty"`
}

type Catalog struct {
	Presets []Preset `json:"presets"`
}

// Load reads a catalog JSON file. A missing file yields an empty catalog
// (no presets configured), not an error — mirroring models.Load.
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

func (c *Catalog) ByKey(key string) (Preset, bool) {
	for _, p := range c.Presets {
		if p.Key == key {
			return p, true
		}
	}
	return Preset{}, false
}
