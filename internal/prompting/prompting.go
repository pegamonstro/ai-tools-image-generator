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

// refusalPhrases are first-person, model-authored refusals. They are deliberate
// false-positive-averse: a phrase like "I cannot" only appears when the model is
// declining, never in image-prompt prose describing a subject. Keeping them
// first-person avoids flagging legitimate prompt text (e.g. "a man who cannot
// swim").
var refusalPhrases = []string{
	"i cannot",
	"i can't",
	"i could not",
	"i'm sorry",
	"i am sorry",
	"i apologize",
	"i apologise",
	"as an ai",
	"i am unable",
	"i'm unable",
	"i won't",
	"i will not",
	"i'm not able",
	"i am not able",
	"i am prohibited",
	"i'm prohibited",
	"i'm not allowed",
	"i am not allowed",
	"cannot fulfill",
	"cannot comply",
	"i can't assist",
	"i cannot assist",
	"i'm not comfortable",
}

// Refused reports whether a chat completion is a content refusal rather than a
// usable prompt. The img-gen tool is an uncensored image generator, so a
// censored model's refusal must be detected and bypassed rather than forwarded
// (where it would be styled into the image prompt).
func Refused(s string) bool {
	lower := strings.ToLower(s)
	for _, phrase := range refusalPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
