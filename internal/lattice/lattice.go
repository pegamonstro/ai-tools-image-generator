package lattice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"img-gen/internal/prompting"
	"img-gen/internal/storage"
)

type Client struct {
	BaseURL      string // chat/enhance frontend (OpenAI-compatible /v1/chat/completions)
	ImageURL     string // mflux sidecar (POST /generate)
	FillURL      string // optional separate mflux sidecar for /fill (inpainting); defaults to ImageURL
	ReduxURL     string // optional separate mflux sidecar for /redux (multi-reference); defaults to ImageURL
	UpscaleURL   string // optional Real-ESRGAN sidecar for /upscale; defaults to ImageURL
	SDXLImageURL string // optional stable-diffusion.cpp sidecar; spec.Sidecar "sdxl" routes here
	HTTP         *http.Client
	// ImagesViaLattice routes Generate/Edit through the frontend's OpenAI
	// Images routes (all inference through the lattice) instead of direct
	// sidecar calls. The remaining ops and the job progress/cancel calls stay
	// direct: the gateway does not proxy /pose, /fill, /redux, /upscale yet,
	// and the sidecar-side gen id recovery depends on them being reachable.
	ImagesViaLattice bool
}

func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{}}
}

// Generate requests one image from the mflux sidecar and returns its raw PNG
// bytes plus the seed the sidecar used (nil when the sidecar does not report
// one). The sidecar takes explicit width/height rather than an OpenAI "size"
// string, so "1024x576" is split into its dimensions.
func (c *Client) Generate(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
	if c.ImagesViaLattice {
		return c.latticeImage(ctx, "/v1/images/generations", prompt, size, "", 0, spec, sp)
	}
	w, h, err := parseSize(size)
	if err != nil {
		return nil, nil, err
	}
	body := map[string]any{"prompt": prompt, "width": w, "height": h}
	applyModelSpec(body, spec)
	applyGenParams(body, sp)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return c.postImage(ctx, "/generate", b, spec.Sidecar)
}

// Edit requests an image-to-image edit: it keeps the source image's content
// while applying the prompt. imageB64 is the source image (base64); strength is
// the denoise strength in [0,1] (higher departs further from the source).
func (c *Client) Edit(ctx context.Context, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
	if c.ImagesViaLattice {
		return c.latticeImage(ctx, "/v1/images/edits", prompt, size, imageB64, strength, spec, sp)
	}
	w, h, err := parseSize(size)
	if err != nil {
		return nil, nil, err
	}
	body := map[string]any{
		"prompt": prompt, "width": w, "height": h,
		"init_image": imageB64, "strength": strength,
	}
	applyModelSpec(body, spec)
	applyGenParams(body, sp)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return c.postImage(ctx, "/edit", b, spec.Sidecar)
}

// latticeImage runs one generate or edit through the frontend's OpenAI Images
// routes: the model field carries the lattice registry name (the submitted
// catalog key, falling back to the raw value), the size stays the "WxH" string,
// and the Lattice extensions (loras + sampling knobs, plus edit's image/strength)
// pass through so settings survive the proxy to the gateway. data[0] returns the
// PNG and the seed the engine's run actually used.
func (c *Client) latticeImage(ctx context.Context, path, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
	if c.BaseURL == "" {
		return nil, nil, errors.New("lattice image routing is on but no lattice base URL is configured (set IMAGE_ROUTING=direct to use sidecars directly)")
	}
	body := map[string]any{"prompt": prompt, "size": size, "n": 1, "response_format": "b64_json"}
	name := spec.LatticeModel
	if name == "" {
		name = spec.Model
	}
	if name != "" {
		body["model"] = name
	}
	if imageB64 != "" {
		body["image"] = imageB64
		body["strength"] = strength
	}
	if len(spec.Loras) > 0 {
		body["loras"] = spec.Loras
	}
	applyGenParams(body, sp)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("lattice %s: %s: %s", strings.TrimPrefix(path, "/v1/images/"), resp.Status, msg)
	}
	var lr struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			Seed    *int64 `json:"seed"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, nil, err
	}
	if len(lr.Data) != 1 {
		return nil, nil, fmt.Errorf("lattice image: got %d results, want 1", len(lr.Data))
	}
	if lr.Data[0].B64JSON == "" {
		return nil, nil, fmt.Errorf("lattice image: empty image")
	}
	png, err := base64.StdEncoding.DecodeString(lr.Data[0].B64JSON)
	return png, lr.Data[0].Seed, err
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

// applyGenParams injects the optional sampling knobs, leaving them absent when
// unset so the sidecar keeps its own defaults.
func applyGenParams(body map[string]any, sp storage.SamplingParams) {
	if sp.Seed != nil {
		body["seed"] = *sp.Seed
	}
	if sp.Steps != nil {
		body["steps"] = *sp.Steps
	}
	if sp.Guidance != nil {
		body["guidance"] = *sp.Guidance
	}
	if sp.NegativePrompt != "" {
		body["negative_prompt"] = sp.NegativePrompt
	}
}

// Controlnet requests an edge-guided (pose) generation via the sidecar's /pose
// endpoint: it conditions generation on a reference image's detected edges.
// imageB64 is the reference image (base64); strength is the ControlNet strength
// in [0,1] (higher follows the pose more rigidly).
func (c *Client) Controlnet(ctx context.Context, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, nil, err
	}
	body := map[string]any{
		"prompt": prompt, "width": w, "height": h,
		"image": imageB64, "strength": strength,
	}
	applyModelSpec(body, spec)
	applyGenParams(body, sp)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return c.postImage(ctx, "/pose", b, spec.Sidecar)
}

// Inpaint repaints only the masked region of imageB64. maskB64 is a same-sized
// mask (white = regenerate, black = keep); output size matches the source.
func (c *Client) Inpaint(ctx context.Context, prompt, imageB64, maskB64 string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"prompt": prompt, "image": imageB64, "mask": maskB64})
	if err != nil {
		return nil, err
	}
	png, _, err := c.postImage(ctx, "/fill", body, "")
	return png, err
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
	png, _, err := c.postImage(ctx, "/redux", body, "")
	return png, err
}

// Upscale runs imageB64 through the Real-ESRGAN sidecar and returns the 4x
// super-resolved PNG. It is promptless (a deterministic model pass), so it does
// not use postImage's seed handling.
func (c *Client) Upscale(ctx context.Context, imageB64 string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"image": imageB64})
	if err != nil {
		return nil, err
	}
	png, _, err := c.postImage(ctx, "/upscale", body, "")
	return png, err
}

// statusResp is the sidecar's GET /status payload.
type statusResp struct {
	State     string `json:"state"`
	ID        string `json:"id"`
	Step      int    `json:"step"`
	Total     int    `json:"total"`
	Cancelled bool   `json:"cancelled"`
}

// ErrResultNotFound is returned by Result when the sidecar reports 404 for the
// gen id — the generation either never ran there or its persisted file is gone.
var ErrResultNotFound = errors.New("result not found")

// sidecarForMode resolves the sidecar base URL for a job mode, mirroring the
// URL selection postImage uses (fill/redux may live on a different host). A
// spec with an "sdxl" sidecar overrides the mode routing: its /generate and
// /edit speak the same body contract as mflux, just on another engine.
func (c *Client) sidecarForMode(mode, sidecar string) string {
	if sidecar == "sdxl" && c.SDXLImageURL != "" {
		return c.SDXLImageURL
	}
	switch mode {
	case "inpaint", "outpaint":
		if c.FillURL != "" {
			return c.FillURL
		}
	case "blend":
		if c.ReduxURL != "" {
			return c.ReduxURL
		}
	case "upscale":
		if c.UpscaleURL != "" {
			return c.UpscaleURL
		}
	}
	return c.ImageURL
}

// Progress polls the sidecar's /status endpoint and returns the in-flight
// job's step/total plus the sidecar's gen id (empty when idle). The poll uses
// a short timeout so a dead sidecar never stalls the queue loop.
func (c *Client) Progress(ctx context.Context, mode, sidecar string) (int, int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sidecarForMode(mode, sidecar)+"/status", nil)
	if err != nil {
		return 0, 0, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, "", fmt.Errorf("mflux status: %s", resp.Status)
	}
	var sr statusResp
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return 0, 0, "", err
	}
	if sr.State != "running" || sr.Total <= 0 {
		return 0, 0, "", nil
	}
	return sr.Step, sr.Total, sr.ID, nil
}

// Status returns the gen id the sidecar is currently generating ("" when
// idle), so a booted client can tell whether a persisted gen id is still
// in flight on the sidecar.
func (c *Client) Status(ctx context.Context, mode, sidecar string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sidecarForMode(mode, sidecar)+"/status", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mflux status: %s", resp.Status)
	}
	var sr statusResp
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", err
	}
	if sr.State != "running" {
		return "", nil
	}
	return sr.ID, nil
}

// Result fetches a completed generation's PNG from the sidecar's persistent
// results store. ErrResultNotFound marks a 404 (unknown/expired gen id);
// transport errors are returned as-is so a caller can keep retrying.
func (c *Client) Result(ctx context.Context, mode, genID, sidecar string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sidecarForMode(mode, sidecar)+"/result/"+url.PathEscape(genID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		png, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return nil, err
		}
		return png, nil
	case http.StatusNotFound:
		return nil, ErrResultNotFound
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("mflux result %s: %s", resp.Status, msg)
	}
}

// Cancel asks the sidecar to terminate its in-flight mflux subprocess and
// release its single-flight lock. It is a no-op when the sidecar is idle.
func (c *Client) Cancel(ctx context.Context, mode, sidecar string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sidecarForMode(mode, sidecar)+"/cancel", nil)
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

// postImage sends one sidecar request and decodes the returned base64 PNG and
// (when present) the seed the sidecar used.
func (c *Client) postImage(ctx context.Context, path string, body []byte, sidecar string) ([]byte, *int64, error) {
	name := strings.TrimPrefix(path, "/")
	base := c.ImageURL
	if sidecar == "sdxl" && c.SDXLImageURL != "" {
		base = c.SDXLImageURL
	} else if path == "/fill" && c.FillURL != "" {
		base = c.FillURL
	} else if path == "/redux" && c.ReduxURL != "" {
		base = c.ReduxURL
	} else if path == "/upscale" && c.UpscaleURL != "" {
		base = c.UpscaleURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("mflux %s: %s: %s", name, resp.Status, msg)
	}
	var gr struct {
		Image string `json:"image"`
		Seed  *int64 `json:"seed"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, nil, err
	}
	if gr.Image == "" {
		if gr.Error != "" {
			return nil, nil, fmt.Errorf("mflux %s: %s", name, gr.Error)
		}
		return nil, nil, fmt.Errorf("mflux %s: empty image", name)
	}
	png, err := base64.StdEncoding.DecodeString(gr.Image)
	return png, gr.Seed, err
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
