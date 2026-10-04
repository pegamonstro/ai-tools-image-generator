package lattice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"img-gen/internal/prompting"
)

type Client struct {
	BaseURL string
	Model   string // image model, e.g. "flux-dev"
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, Model: "flux-dev", HTTP: &http.Client{}}
}

type genRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Size           string `json:"size,omitempty"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
}

type genResponse struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
	} `json:"data"`
}

// Generate requests one image and returns its raw PNG bytes.
func (c *Client) Generate(ctx context.Context, prompt, size string) ([]byte, error) {
	body := genRequest{Model: c.Model, Prompt: prompt, Size: size, N: 1, ResponseFormat: "b64_json"}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/images/generations", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("lattice images: %s: %s", resp.Status, msg)
	}
	var gr genResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, err
	}
	if len(gr.Data) == 0 {
		return nil, fmt.Errorf("lattice images: empty data")
	}
	return base64.StdEncoding.DecodeString(gr.Data[0].B64JSON)
}

type chatRequest struct {
	Model    string              `json:"model"`
	Messages []prompting.Message `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Chat sends messages to the chat-completions endpoint and returns the
// assistant reply. model is passed per call (the enhance LLM may differ from
// the image model).
func (c *Client) Chat(ctx context.Context, model string, messages []prompting.Message) (string, error) {
	b, err := json.Marshal(chatRequest{Model: model, Messages: messages})
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
		return "", fmt.Errorf("lattice chat: %s: %s", resp.Status, msg)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("lattice chat: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}
