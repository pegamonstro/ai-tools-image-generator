package prompting

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"img-gen/internal/genres"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatFunc is implemented by the lattice client. prompting depends on it
// only through this interface so the enhance path is unit-testable.
type ChatFunc func(ctx context.Context, messages []Message) (string, error)

type FieldValues map[string]string

var tokenRe = regexp.MustCompile(`\{([a-z0-9_]+)\}`)

// Direct fills the template from values, dropping empty optional fields and
// tidying leftover separators.
func Direct(template string, values FieldValues) string {
	out := tokenRe.ReplaceAllStringFunc(template, func(m string) string {
		key := m[1 : len(m)-1]
		if v := strings.TrimSpace(values[key]); v != "" {
			return v
		}
		return "\x00"
	})
	return tidy(out)
}

func tidy(s string) string {
	for i := 0; i < 4; i++ {
		n := s
		s = strings.ReplaceAll(s, ", \x00", "")
		s = strings.ReplaceAll(s, "\x00, ", "")
		s = strings.ReplaceAll(s, " \x00", "")
		s = strings.ReplaceAll(s, "\x00 ", "")
		s = strings.ReplaceAll(s, "\x00", "")
		s = strings.ReplaceAll(s, " ,", ",")
		s = strings.ReplaceAll(s, ",,", ",")
		if s == n {
			break
		}
	}
	s = strings.Trim(s, ", ")
	return s
}

// Resolve maps submitted field values to prompt-ready strings. Boolean fields
// ("true"/"false") become their configured true_text/false_text.
func Resolve(fields []genres.Field, values FieldValues) FieldValues {
	out := make(FieldValues, len(values))
	for k, v := range values {
		out[k] = v
	}
	for _, f := range fields {
		if f.Type == genres.FieldBoolean {
			if values[f.Key] == "true" {
				out[f.Key] = f.TrueText
			} else {
				out[f.Key] = f.FalseText
			}
		}
	}
	return out
}

// Enhance asks the chat model to write a polished prompt from the structured
// fields.
func Enhance(ctx context.Context, chat ChatFunc, system string, values FieldValues) (string, error) {
	b, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return chat(ctx, []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: string(b)},
	})
}

// EnhancePrompt asks the chat model to polish a free-text image prompt (used by
// the edit/inpaint/blend modes, which take a raw prompt rather than structured
// genre fields).
func EnhancePrompt(ctx context.Context, chat ChatFunc, system, prompt string) (string, error) {
	return chat(ctx, []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	})
}
