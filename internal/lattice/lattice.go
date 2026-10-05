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
	FillURL  string // optional separate mflux sidecar for /fill (inpainting); defaults to ImageURL
	ReduxURL string // optional separate mflux sidecar for /redux (multi-reference); defaults to ImageURL
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
	return c.postImage(ctx, "/generate", body)
}

// Edit requests an image-to-image edit: it keeps the source image's content
// while applying the prompt. imageB64 is the source image (base64); strength is
// the denoise strength in [0,1] (higher departs further from the source).
func (c *Client) Edit(ctx context.Context, prompt, size, imageB64 string, strength float64) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"prompt": prompt, "width": w, "height": h,
		"init_image": imageB64, "strength": strength,
	})
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/edit", body)
}

// Inpaint repaints only the masked region of imageB64. maskB64 is a same-sized
// mask (white = regenerate, black = keep); output size matches the source.
func (c *Client) Inpaint(ctx context.Context, prompt, imageB64, maskB64 string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"prompt": prompt, "image": imageB64, "mask": maskB64})
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/fill", body)
}

// Blend generates a new image from a prompt plus reference images (base64) and
// per-reference strengths.
func (c *Client) Blend(ctx context.Context, prompt, size string, imagesB64 []string, strengths []float64) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"prompt": prompt, "width": w, "height": h,
		"images": imagesB64, "strengths": strengths,
	})
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/redux", body)
}

// postImage sends one sidecar request and decodes the returned base64 PNG.
func (c *Client) postImage(ctx context.Context, path string, body []byte) ([]byte, error) {
	name := strings.TrimPrefix(path, "/")
	base := c.ImageURL
	if path == "/fill" && c.FillURL != "" {
		base = c.FillURL
	} else if path == "/redux" && c.ReduxURL != "" {
		base = c.ReduxURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
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
		return nil, fmt.Errorf("mflux %s: %s: %s", name, resp.Status, msg)
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
			return nil, fmt.Errorf("mflux %s: %s", name, gr.Error)
		}
		return nil, fmt.Errorf("mflux %s: empty image", name)
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
