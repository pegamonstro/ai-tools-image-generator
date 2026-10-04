package lattice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"img-gen/internal/prompting"
)

type Client struct {
	BaseURL  string // chat/enhance frontend (OpenAI-compatible /v1/chat/completions)
	ImageURL string // mflux sidecar (POST /generate)
	HTTP     *http.Client
}

func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{}}
}

// Generate requests one image from the mflux sidecar and returns its raw PNG
// bytes. The sidecar takes explicit width/height rather than an OpenAI "size"
// string, so "1024x576" is split into its dimensions.
func (c *Client) Generate(ctx context.Context, prompt, size string) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"prompt": prompt, "width": w, "height": h})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ImageURL+"/generate", bytes.NewReader(body))
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
		return nil, fmt.Errorf("mflux generate: %s: %s", resp.Status, msg)
	}
	var gr struct {
		Image string `json:"image"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, err
	}
	if gr.Image == "" {
		if gr.Error != "" {
			return nil, fmt.Errorf("mflux generate: %s", gr.Error)
		}
		return nil, fmt.Errorf("mflux generate: empty image")
	}
	return base64.StdEncoding.DecodeString(gr.Image)
}

func parseSize(size string) (int, int, error) {
	wStr, hStr, ok := strings.Cut(size, "x")
	if !ok {
		return 0, 0, fmt.Errorf("invalid size %q", size)
	}
	w, errW := strconv.Atoi(wStr)
	h, errH := strconv.Atoi(hStr)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid size %q", size)
	}
	return w, h, nil
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
