package genres

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type FieldType string

const (
	FieldText     FieldType = "text"
	FieldTextarea FieldType = "textarea"
	FieldSelect   FieldType = "select"
	FieldNumber   FieldType = "number"
	FieldBoolean  FieldType = "boolean"
)

type Field struct {
	Key         string    `json:"key"`
	Label       string    `json:"label"`
	Type        FieldType `json:"type"`
	Placeholder string    `json:"placeholder,omitempty"`
	Hint        string    `json:"hint,omitempty"`
	Options     []string  `json:"options,omitempty"`
	Default     any       `json:"default,omitempty"`
	Min         *float64  `json:"min,omitempty"`
	Max         *float64  `json:"max,omitempty"`
	TrueText    string    `json:"true_text,omitempty"`
	FalseText   string    `json:"false_text,omitempty"`
	Required    bool      `json:"required,omitempty"`
}

type Genre struct {
	Label       string  `json:"label"`
	Description string  `json:"description,omitempty"`
	Fields      []Field `json:"fields"`
	// PromptTemplate: {key} tokens fall back to empty; a clause
	// {?key: fragment} renders the fragment (surrounding literals included)
	// only when the key has a submitted value.
	PromptTemplate string   `json:"prompt_template"`
	Sizes          []string `json:"sizes"`
}

// Style is a global prompt modifier applied across every mode. Prompt is a
// fragment prepended to the resolved prompt (e.g. "watercolor painting, soft
// washes"), so a style is plain prompt engineering rather than a LoRA.
type Style struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
}

type Catalog struct {
	Version int              `json:"version"`
	Genres  map[string]Genre `json:"genres"`
	Styles  []Style          `json:"styles,omitempty"`
}

func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &c, nil
}

var sizeRe = regexp.MustCompile(`^\d+x\d+$`)

func (c *Catalog) Validate() error {
	if len(c.Genres) == 0 {
		return fmt.Errorf("no genres defined")
	}
	for name, g := range c.Genres {
		if strings.TrimSpace(g.Label) == "" {
			return fmt.Errorf("genre %q: empty label", name)
		}
		if strings.TrimSpace(g.PromptTemplate) == "" {
			return fmt.Errorf("genre %q: empty prompt_template", name)
		}
		if len(g.Sizes) == 0 {
			return fmt.Errorf("genre %q: no sizes", name)
		}
		for _, s := range g.Sizes {
			if !sizeRe.MatchString(s) {
				return fmt.Errorf("genre %q: bad size %q", name, s)
			}
		}
		seen := map[string]bool{}
		for _, f := range g.Fields {
			if f.Key == "" || f.Label == "" {
				return fmt.Errorf("genre %q: field with empty key or label", name)
			}
			if seen[f.Key] {
				return fmt.Errorf("genre %q: duplicate field key %q", name, f.Key)
			}
			seen[f.Key] = true
			switch f.Type {
			case FieldText, FieldTextarea, FieldBoolean:
			case FieldSelect:
				if len(f.Options) == 0 {
					return fmt.Errorf("genre %q field %q: select needs options", name, f.Key)
				}
			case FieldNumber:
			default:
				return fmt.Errorf("genre %q field %q: unknown type %q", name, f.Key, f.Type)
			}
		}
	}
	seenStyles := map[string]bool{}
	for _, s := range c.Styles {
		if strings.TrimSpace(s.Key) == "" || strings.TrimSpace(s.Label) == "" || strings.TrimSpace(s.Prompt) == "" {
			return fmt.Errorf("style with empty key, label, or prompt")
		}
		if seenStyles[s.Key] {
			return fmt.Errorf("duplicate style key %q", s.Key)
		}
		seenStyles[s.Key] = true
	}
	return nil
}

func (c *Catalog) Genre(name string) (Genre, bool) {
	g, ok := c.Genres[name]
	return g, ok
}

func (c *Catalog) Style(key string) (Style, bool) {
	for _, s := range c.Styles {
		if s.Key == key {
			return s, true
		}
	}
	return Style{}, false
}
