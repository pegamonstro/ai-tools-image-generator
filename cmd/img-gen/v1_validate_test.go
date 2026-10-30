package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"img-gen/internal/critic"
	"img-gen/internal/lattice"
)

// makePNG returns a base64-encoded PNG of the given dimensions.
func makePNG(w, h int) string {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 128, G: 128, B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// mockJudge is a fake critic.Judge that returns a fixed result for vlm_check.
func mockJudge(pass bool) critic.Judge {
	return func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
		var out []critic.VLMScore
		for range criteria {
			out = append(out, critic.VLMScore{
				Pass:   pass,
				Reason: "mock reason",
				Fix:    "mock fix",
			})
		}
		return out, nil
	}
}

// mockTagger is a fake critic Tagger func.
func mockTagger(tags []lattice.TagScore) func(context.Context, string) ([]lattice.TagScore, error) {
	return func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
		return tags, nil
	}
}

// mockVocab is a fake critic Vocab func.
func mockVocab(vocab []string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		return vocab, nil
	}
}

// testDeps builds a v1Deps with fake Env closures for validate tests.
func testDeps(t *testing.T, judge critic.Judge, tagger func(context.Context, string) ([]lattice.TagScore, error), vocab func(context.Context) ([]string, error)) *v1Deps {
	t.Helper()
	d := &v1Deps{}
	d.critic = critic.Env{
		Judge:  judge,
		Tagger: tagger,
		Vocab:  vocab,
	}
	d.criticTimeout = 5 * time.Second
	return d
}

// testDepsNoTagger builds a v1Deps with nil Tagger (for tag_roundtrip without sidecar tests).
func testDepsNoTagger(t *testing.T, judge critic.Judge) *v1Deps {
	t.Helper()
	d := &v1Deps{}
	d.critic = critic.Env{
		Judge:  judge,
		Tagger: nil,
		Vocab:  func(ctx context.Context) ([]string, error) { return nil, nil },
	}
	d.criticTimeout = 5 * time.Second
	return d
}

func TestValidatePassVerdict(t *testing.T) {
	// All checks pass -> verdict "pass"
	d := testDeps(t, mockJudge(true), mockTagger([]lattice.TagScore{{Name: "cat", Score: 0.9}}), mockVocab([]string{"cat", "dog"}))
	img := makePNG(64, 64)
	body := fmt.Sprintf(`{"image":"%s","checks":[{"op":"vlm_check","arg":"\"a cat\""}],"palette":[],"budget":0}`, img)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/validate", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handleV1Validate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["verdict"] != "pass" {
		t.Fatalf("verdict = %v, want pass", resp["verdict"])
	}
	checks := resp["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["status"] != "pass" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestValidateFailedVerdict(t *testing.T) {
	// One check fails -> verdict "failed"
	d := testDeps(t, mockJudge(false), mockTagger([]lattice.TagScore{{Name: "cat", Score: 0.9}}), mockVocab([]string{"cat", "dog"}))
	img := makePNG(64, 64)
	body := fmt.Sprintf(`{"image":"%s","checks":[{"op":"vlm_check","arg":"\"a cat\""}],"palette":[],"budget":0}`, img)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/validate", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handleV1Validate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["verdict"] != "failed" {
		t.Fatalf("verdict = %v, want failed", resp["verdict"])
	}
	checks := resp["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["status"] != "fail" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestValidateIndeterminateVerdict(t *testing.T) {
	// All checks inert (error/undetermined) -> verdict "indeterminate"
	d := testDeps(t, mockJudge(true), mockTagger([]lattice.TagScore{}), mockVocab([]string{}))
	img := makePNG(64, 64)
	// vlm_check with judge returning error -> undetermined
	d.critic.Judge = func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
		return nil, fmt.Errorf("judge down")
	}
	body := fmt.Sprintf(`{"image":"%s","checks":[{"op":"vlm_check","arg":"\"a cat\""}],"palette":[],"budget":0}`, img)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/validate", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handleV1Validate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["verdict"] != "indeterminate" {
		t.Fatalf("verdict = %v, want indeterminate", resp["verdict"])
	}
}

func TestValidate400Matrix(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantCode      int
		wantMsgSubstr string
	}{
		{
			name:          "bad base64",
			body:          `{"image":"!!!bad!!!"}`,
			wantCode:      http.StatusBadRequest,
			wantMsgSubstr: "image is not valid base64",
		},
		{
			name:          "non-PNG",
			body:          fmt.Sprintf(`{"image":"%s"}`, base64.StdEncoding.EncodeToString([]byte("not a png"))),
			wantCode:      http.StatusBadRequest,
			wantMsgSubstr: "image is not a PNG",
		},
		{
			name:          "oversize dims",
			body:          fmt.Sprintf(`{"image":"%s"}`, makePNG(1025, 1025)),
			wantCode:      http.StatusBadRequest,
			wantMsgSubstr: "validate supports up to 1024x1024",
		},
		{
			name:          "unknown op",
			body:          fmt.Sprintf(`{"image":"%s","checks":[{"op":"unknown_op"}]}`, makePNG(64, 64)),
			wantCode:      http.StatusBadRequest,
			wantMsgSubstr: "unknown check op",
		},
		{
			name:          "tag op with nil Tagger",
			body:          fmt.Sprintf(`{"image":"%s","checks":[{"op":"tag_roundtrip"}]}`, makePNG(64, 64)),
			wantCode:      http.StatusBadRequest,
			wantMsgSubstr: "TAGGER_URL",
		},
		{
			name:          "palette_conform without palette (compile-time)",
			body:          fmt.Sprintf(`{"image":"%s","checks":[{"op":"palette_conform"}]}`, makePNG(64, 64)),
			wantCode:      http.StatusBadRequest,
			wantMsgSubstr: "palette_conform requires a palette",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &v1Deps{}
			d.critic = critic.Env{
				Judge:  mockJudge(true),
				Tagger: nil,
				Vocab:  func(ctx context.Context) ([]string, error) { return nil, nil },
			}
			d.criticTimeout = 5 * time.Second
			req := httptest.NewRequest(http.MethodPost, "/api/v1/validate", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			d.handleV1Validate(w, req)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tc.wantCode, w.Body.String())
			}
			var env v1Env
			json.NewDecoder(w.Body).Decode(&env)
			if !strings.Contains(env.Error.Message, tc.wantMsgSubstr) {
				t.Fatalf("message = %q, want substring %q", env.Error.Message, tc.wantMsgSubstr)
			}
		})
	}
}

func TestValidateOpenAPISync(t *testing.T) {
	// Assert docs/openapi.yaml contains the validate path and CriticCheckSpec component.
	// Relative path from cmd/img-gen to docs: ../../docs/openapi.yaml
	const rel = "../../docs/openapi.yaml"
	content, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	s := string(content)
	if !strings.Contains(s, "/api/v1/validate") {
		t.Fatal("openapi.yaml missing /api/v1/validate path")
	}
	if !strings.Contains(s, "CriticCheckSpec") {
		t.Fatal("openapi.yaml missing CriticCheckSpec component")
	}
	if !strings.Contains(s, "ValidateResponse") {
		t.Fatal("openapi.yaml missing ValidateResponse component")
	}
	if !strings.Contains(s, "ValidateSpec") {
		t.Fatal("openapi.yaml missing ValidateSpec component")
	}
	// Verify validate field is on the request schemas that accept it (substring, not indentation-sensitive).
	if !strings.Contains(s, "validate:") || !strings.Contains(s, "ValidateSpec") {
		t.Fatal("openapi.yaml missing validate field on SubmitRequest/AssetSpecIn/GifRequest")
	}
}
