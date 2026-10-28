package lattice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// TagScore is one WD14 tag and its model score.
type TagScore struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
}

var (
	tagVocabMu     sync.Mutex
	tagVocabCached []string
)

// Tagger sends one image to the WD14 sidecar, returning tags >= its server
// threshold with scores. Errors carry the "tagger sidecar:" prefix like the
// rembg/rife clients.
func (c *Client) Tagger(ctx context.Context, imageB64 string) ([]TagScore, error) {
	body, err := json.Marshal(map[string]any{"image": imageB64})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.TaggerURL, "/")+"/tag", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tagger sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("tagger sidecar: HTTP %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Tags []TagScore `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("tagger sidecar body: %w", err)
	}
	return out.Tags, nil
}

// TaggerVocab returns the WD14 tag vocabulary (GET /meta), fetched once and
// cached; a failed fetch is retried on the next call. A nil TaggerURL must
// never reach here (submit validation gates tag ops on the client being wired).
func (c *Client) TaggerVocab(ctx context.Context) ([]string, error) {
	tagVocabMu.Lock()
	defer tagVocabMu.Unlock()
	if tagVocabCached != nil {
		return tagVocabCached, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.TaggerURL, "/")+"/meta", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tagger sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tagger sidecar meta: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tagger sidecar meta body: %w", err)
	}
	tagVocabCached = out.Tags
	return tagVocabCached, nil
}
