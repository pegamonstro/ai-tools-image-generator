// Package models loads the local model/LoRA catalog (models.json) and model profiles (models_profiles.yaml).
package models

import (
	"encoding/json"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Entry struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Sidecar string `json:"sidecar,omitempty"` // "sdxl" routes this model to the SDXL sidecar; "" = default mflux sidecar
}

type Catalog struct {
	Models []Entry `json:"models"`
	Loras  []Entry `json:"loras"`
}

// ProfileDefaults holds the per-family default sampling parameters.
type ProfileDefaults struct {
	PredictionType   string  `json:"prediction_type,omitempty" yaml:"prediction_type,omitempty"`     // "v" for v-pred models
	CfgRescale       float64 `json:"cfg_rescale,omitempty" yaml:"cfg_rescale,omitempty"`             // CFG rescale for v-pred
	ScoreTags        string  `json:"score_tags,omitempty" yaml:"score_tags,omitempty"`               // Pony score tags
	Negative         string  `json:"negative,omitempty" yaml:"negative,omitempty"`                   // Negative prompt
	PositivePrefix   string  `json:"positive_prefix,omitempty" yaml:"positive_prefix,omitempty"`     // Prefix for positive prompt
	NegativePrefix   string  `json:"negative_prefix,omitempty" yaml:"negative_prefix,omitempty"`     // Prefix for negative prompt
	Sampler          string  `json:"sampler,omitempty" yaml:"sampler,omitempty"`                     // Sampler name
	Steps            int     `json:"steps,omitempty" yaml:"steps,omitempty"`                         // Diffusion steps
	CfgScale         float64 `json:"cfg_scale,omitempty" yaml:"cfg_scale,omitempty"`                 // CFG scale
}

type Profile struct {
	Name       string           `json:"name" yaml:"name"`
	Models     []string         `json:"models" yaml:"models"`
	Defaults   ProfileDefaults  `json:"defaults" yaml:"defaults"`
	Unsupported []string        `json:"unsupported,omitempty" yaml:"unsupported,omitempty"`
}

type ProfilesConfig struct {
	Profiles []Profile `json:"profiles" yaml:"profiles"`
}

func LoadProfiles(path string) (*ProfilesConfig, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &ProfilesConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c ProfilesConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ApplyProfile applies the model profile defaults to a request if the model key matches.
func (c *ProfilesConfig) ApplyProfile(modelKey string, prompt *string, negative *string, sampling *map[string]any) {
	for _, p := range c.Profiles {
		for _, m := range p.Models {
			if strings.EqualFold(m, modelKey) {
				// Check unsupported
				for _, u := range p.Unsupported {
					if strings.EqualFold(u, modelKey) {
						return // Don't apply unsupported profile
					}
				}
				d := p.Defaults
				if prompt != nil && d.PositivePrefix != "" {
					*prompt = d.PositivePrefix + *prompt
				}
				if negative != nil {
					if d.NegativePrefix != "" {
						*negative = d.NegativePrefix + *negative
					}
					if d.Negative != "" {
						if *negative != "" {
							*negative += ", "
						}
						*negative += d.Negative
					}
				}
				if sampling != nil {
					if d.PredictionType != "" {
						(*sampling)["prediction_type"] = d.PredictionType
					}
					if d.CfgRescale > 0 {
						(*sampling)["cfg_rescale"] = d.CfgRescale
					}
					if d.Sampler != "" {
						(*sampling)["sampler"] = d.Sampler
					}
					if d.Steps > 0 {
						(*sampling)["steps"] = d.Steps
					}
					if d.CfgScale > 0 {
						(*sampling)["cfg_scale"] = d.CfgScale
					}
				}
				return
			}
		}
	}
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

// LookupSidecar resolves the engine selection for a model key or raw value.
// It returns "" when the key matches no entry (the caller keeps the default
// sidecar), so unknown/LoRA-only lookups never invent an engine.
func (c *Catalog) LookupSidecar(keyOrValue string) string {
	if keyOrValue == "" {
		return ""
	}
	for _, e := range c.Models {
		if e.Key == keyOrValue || e.Value == keyOrValue {
			return e.Sidecar
		}
	}
	return ""
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
