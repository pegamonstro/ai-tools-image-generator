package lattice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type visionTextPart struct {
	Type string `json:"type"` // "text"
	Text string `json:"text,omitempty"`
}

type visionImagePart struct {
	Type     string             `json:"type"` // "image_url"
	ImageURL visionImageURLData `json:"image_url"`
}

type visionImageURLData struct {
	URL string `json:"url"`
}

// ChatVision sends one non-streaming chat completion whose user message
// carries the text prompt and ONE image (PNG base64 -> data URL). It is the
// Phase D critic judge transport: the lattice gateway translates content
// parts into Ollama's native images array (see the lattice gateway plan).
// A gateway that predates the translation rejects the body with 400 -- that
// error is surfaced with a named upgrade note, never silently downgraded.
func (c *Client) ChatVision(ctx context.Context, model, system, userText, imageB64 string) (string, error) {
	if c.BaseURL == "" {
		return "", fmt.Errorf("chat vision: no lattice base URL configured")
	}
	body := map[string]any{
		"model":  model,
		"stream": false,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": []any{
				visionTextPart{Type: "text", Text: userText},
				visionImagePart{Type: "image_url", ImageURL: visionImageURLData{
					URL: "data:image/png;base64," + imageB64,
				}},
			}},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Pre-upgrade gateway: the parts body reaches ollama's native endpoint,
		// ollama rejects it, and the gateway's Execute collapses upstream
		// non-200s into "ollama returned status N" written as HTTP 500. The
		// named-upgrade error must fire on that real signature (and on any
		// directly-migrated 400) -- never on the generic path.
		upgrade := resp.StatusCode == http.StatusBadRequest ||
			(resp.StatusCode == http.StatusInternalServerError &&
				strings.Contains(string(msg), "ollama returned status"))
		if upgrade {
			return "", fmt.Errorf("lattice chat vision: gateway rejected the image content body (%s) -- upgrade the lattice gateway to one that translates chat image content parts, or unset the critic's checklist ops that need the judge", strings.TrimSpace(string(msg)))
		}
		return "", fmt.Errorf("lattice chat vision: %s: %s", resp.Status, msg)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("lattice chat vision: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}
