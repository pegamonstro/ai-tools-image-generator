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
	"time"

	"img-gen/internal/prompting"
	"img-gen/internal/storage"
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
func (c *Client) Generate(ctx context.Context, prompt, size string, spec storage.ModelSpec) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"prompt": prompt, "width": w, "height": h}
	applyModelSpec(body, spec)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/generate", b)
}

// Edit requests an image-to-image edit: it keeps the source image's content
// while applying the prompt. imageB64 is the source image (base64); strength is
// the denoise strength in [0,1] (higher departs further from the source).
func (c *Client) Edit(ctx context.Context, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"prompt": prompt, "width": w, "height": h,
		"init_image": imageB64, "strength": strength,
	}
	applyModelSpec(body, spec)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/edit", b)
}

// applyModelSpec injects the optional model/LoRA selection into a request body,
// leaving the fields absent when unset so the sidecar uses its defaults.
func applyModelSpec(body map[string]any, spec storage.ModelSpec) {
	if spec.Model != "" {
		body["model"] = spec.Model
	}
	if len(spec.Loras) > 0 {
		body["loras"] = spec.Loras
	}
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

// statusResp is the sidecar's GET /status payload.
type statusResp struct {
	State     string `json:"state"`
	Step      int    `json:"step"`
	Total     int    `json:"total"`
	Cancelled bool   `json:"cancelled"`
}

// sidecarForMode resolves the sidecar base URL for a job mode, mirroring the
// URL selection postImage uses (fill/redux may live on a different host).
func (c *Client) sidecarForMode(mode string) string {
	switch mode {
	case "inpaint":
		if c.FillURL != "" {
			return c.FillURL
		}
	case "blend":
		if c.ReduxURL != "" {
			return c.ReduxURL
		}
	}
	return c.ImageURL
}

// Progress polls the sidecar's /status endpoint and returns the in-flight
// job's step/total. It returns (0, 0, nil) when the sidecar is idle. The poll
// uses a short timeout so a dead sidecar never stalls the queue loop.
func (c *Client) Progress(ctx context.Context, mode string) (int, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sidecarForMode(mode)+"/status", nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("mflux status: %s", resp.Status)
	}
	var sr statusResp
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return 0, 0, err
	}
	if sr.State != "running" || sr.Total <= 0 {
		return 0, 0, nil
	}
	return sr.Step, sr.Total, nil
}

// Cancel asks the sidecar to terminate its in-flight mflux subprocess and
// release its single-flight lock. It is a no-op when the sidecar is idle.
func (c *Client) Cancel(ctx context.Context, mode string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sidecarForMode(mode)+"/cancel", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("mflux cancel: %s: %s", resp.Status, msg)
	}
	return nil
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
