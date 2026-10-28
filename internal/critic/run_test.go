package critic

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"img-gen/internal/lattice"
	"img-gen/internal/storage"
)

// base64RedImage returns a minimal valid base64 PNG of a 4x4 red image.
func base64RedImage() string {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// base64BadTileImage returns a base64 PNG of a 10x10 image with mismatched edges.
func base64BadTileImage() string {
	img := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			switch {
			case x == 0:
				img.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 255})
			case x == 9:
				img.SetNRGBA(x, y, color.NRGBA{0, 0, 255, 255})
			case y == 0:
				img.SetNRGBA(x, y, color.NRGBA{0, 255, 0, 255})
			case y == 9:
				img.SetNRGBA(x, y, color.NRGBA{255, 255, 0, 255})
			default:
				img.SetNRGBA(x, y, color.NRGBA{100, 100, 100, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	cl := Checklist{
		Checks: []Check{
			{Op: "vlm_check", Spec: "subject is a cat"},
			{Op: "vlm_check", Spec: "background is white"},
			{Op: "tag_roundtrip"},
			{Op: "palette_conform"},
			{Op: "tile_conform", MaxRatio: 1.5},
		},
		Target: []string{"#ff0000", "#00ff00"},
	}

	// --- Case 1: All pass ---
	t.Run("AllPass", func(t *testing.T) {
		judgeCalls := 0
		taggerCalls := 0
		vocabCalls := 0

		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				judgeCalls++
				if len(criteria) != 2 {
					t.Fatalf("Judge called with %d criteria, want 2 (batched)", len(criteria))
				}
				return []VLMScore{
					{ID: 0, Pass: true, Reason: "cat detected"},
					{ID: 1, Pass: true, Reason: "white background confirmed"},
				}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				taggerCalls++
				return []lattice.TagScore{
					{Name: "1girl", Score: 0.9},
					{Name: "red_gloves", Score: 0.8},
				}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				vocabCalls++
				return []string{"1girl", "red_gloves", "long_hair"}, nil
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		if judgeCalls != 1 {
			t.Fatalf("Judge called %d times, want 1 (batched)", judgeCalls)
		}
		if taggerCalls != 1 {
			t.Fatalf("Tagger called %d times, want 1", taggerCalls)
		}
		if vocabCalls != 1 {
			t.Fatalf("Vocab called %d times, want 1", vocabCalls)
		}

		// Check results: 5 checks in order
		if len(rs) != 5 {
			t.Fatalf("got %d results, want 5", len(rs))
		}

		// vlm_check #1
		if rs[0].Op != "vlm_check" || rs[0].Status != "pass" || rs[0].Reason != "cat detected" {
			t.Errorf("check 0: got %+v", rs[0])
		}
		// vlm_check #2
		if rs[1].Op != "vlm_check" || rs[1].Status != "pass" || rs[1].Reason != "white background confirmed" {
			t.Errorf("check 1: got %+v", rs[1])
		}
		// tag_roundtrip - prompt has no tags, so vacuous pass
		if rs[2].Op != "tag_roundtrip" || rs[2].Status != "pass" {
			t.Errorf("check 2: got %+v", rs[2])
		}
		// palette_conform - exact match on red
		if rs[3].Op != "palette_conform" || rs[3].Status != "pass" {
			t.Errorf("check 3: got %+v", rs[3])
		}
		// tile_conform - seamless red image
		if rs[4].Op != "tile_conform" || rs[4].Status != "pass" {
			t.Errorf("check 4: got %+v", rs[4])
		}
	})

	// --- Case 2: VLM judge error -> all vlm_check undetermined ---
	t.Run("VLMJudgeError", func(t *testing.T) {
		judgeCalls := 0
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				judgeCalls++
				return nil, ErrJudgeUnreadable
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		if judgeCalls != 1 {
			t.Fatalf("Judge called %d times, want 1", judgeCalls)
		}

		// Both vlm_check should be undetermined
		if rs[0].Status != "undetermined" || !strings.Contains(rs[0].Reason, "unreadable") {
			t.Errorf("check 0: got %+v", rs[0])
		}
		if rs[1].Status != "undetermined" || !strings.Contains(rs[1].Reason, "unreadable") {
			t.Errorf("check 1: got %+v", rs[1])
		}
		// Other checks should still run
		if rs[2].Status != "pass" {
			t.Errorf("check 2 (tag): got %+v", rs[2])
		}
	})

	// --- Case 3: VLM returns fewer scores than criteria ---
	t.Run("VLMPartialReply", func(t *testing.T) {
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil // only 1 of 2
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		// First criterion gets the score (idx 0 exists), second gets "judge omitted criterion"
		if rs[0].Status != "pass" {
			t.Errorf("check 0: got %+v, want pass", rs[0])
		}
		if rs[1].Status != "undetermined" || rs[1].Reason != "judge omitted criterion" {
			t.Errorf("check 1: got %+v, want undetermined with 'judge omitted criterion'", rs[1])
		}
	})

	// --- Case 4: VLM fail with Fix ---
	t.Run("VLMFailWithFix", func(t *testing.T) {
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{
					{ID: 0, Pass: false, Reason: "no cat visible", Fix: "add cat to prompt"},
					{ID: 1, Pass: true, Reason: "white bg ok"},
				}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		if rs[0].Status != "fail" || rs[0].Reason != "no cat visible" || rs[0].Suggestion != "add cat to prompt" {
			t.Errorf("check 0 (fail): got %+v", rs[0])
		}
		if rs[1].Status != "pass" {
			t.Errorf("check 1 (pass): got %+v", rs[1])
		}
	})

	// --- Case 5: tag_roundtrip vocab fetch error -> error (inert) ---
	t.Run("TagVocabError", func(t *testing.T) {
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return nil, assertError("vocab fetch failed")
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		// tag_roundtrip should be error (inert)
		if rs[2].Op != "tag_roundtrip" || rs[2].Status != "error" || !strings.Contains(rs[2].Reason, "vocab fetch failed") {
			t.Errorf("check 2 (tag vocab error): got %+v", rs[2])
		}
	})

	// --- Case 6: tag_roundtrip tagger call error -> error (inert) ---
	t.Run("TaggerCallError", func(t *testing.T) {
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return nil, assertError("tagger sidecar down")
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl", "red_gloves"}, nil
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		if rs[2].Status != "error" || !strings.Contains(rs[2].Reason, "tagger sidecar down") {
			t.Errorf("check 2 (tagger error): got %+v", rs[2])
		}
	})

	// --- Case 7: tag_roundtrip missing tags -> fail with strengthen suggestion ---
	t.Run("TagDriftMissing", func(t *testing.T) {
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{
					{Name: "1girl", Score: 0.9},
					{Name: "long_hair", Score: 0.8},
				}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl", "red_gloves", "long_hair"}, nil
			},
		}

		// Checklist with prompt tags that include missing red_gloves
		clWithPrompt := Checklist{
			Checks: []Check{
				{Op: "vlm_check", Spec: "ok"},
				{Op: "tag_roundtrip"},
			},
			Prompt: "1girl, red gloves, long hair",
		}

		rs := Run(ctx, clWithPrompt, base64RedImage(), e)

		// tag_roundtrip should fail with strengthen suggestion
		if len(rs) < 2 {
			t.Fatalf("got %d results, want 2", len(rs))
		}
		if rs[1].Op != "tag_roundtrip" || rs[1].Status != "fail" {
			t.Errorf("check 1 (tag drift): got %+v", rs[1])
		}
		if !strings.Contains(rs[1].Reason, "danbooru drift") {
			t.Errorf("tag drift reason: got %q", rs[1].Reason)
		}
		if !strings.Contains(rs[1].Suggestion, "strengthen: red_gloves") {
			t.Errorf("tag drift suggestion: got %q", rs[1].Suggestion)
		}
	})

	// --- Case 7b: tag_roundtrip empty vocab -> "no known tags to verify" ---
	t.Run("TagEmptyVocab", func(t *testing.T) {
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{}, nil // empty vocab
			},
		}

		clEmptyVocab := Checklist{
			Checks: []Check{
				{Op: "vlm_check", Spec: "ok"},
				{Op: "tag_roundtrip"},
			},
			Prompt: "1girl, red gloves", // prompt has tags but vocab is empty
		}

		rs := Run(ctx, clEmptyVocab, base64RedImage(), e)

		if len(rs) < 2 {
			t.Fatalf("got %d results, want 2", len(rs))
		}
		if rs[1].Op != "tag_roundtrip" || rs[1].Status != "pass" {
			t.Errorf("check 1 (tag empty vocab): got %+v", rs[1])
		}
		if rs[1].Reason != "no known tags to verify" {
			t.Errorf("tag empty vocab reason: got %q, want %q", rs[1].Reason, "no known tags to verify")
		}
	})

	// --- Case 7c: tag_roundtrip non-empty vocab but prompt tokens unknown -> "no known tags to verify" ---
	t.Run("TagPromptUnknownTokens", func(t *testing.T) {
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl", "red_gloves", "long_hair"}, nil
			},
		}

		// Prompt uses tags NOT in vocab
		clUnknownTokens := Checklist{
			Checks: []Check{
				{Op: "vlm_check", Spec: "ok"},
				{Op: "tag_roundtrip"},
			},
			Prompt: "unknown_tag, another_unknown",
		}

		rs := Run(ctx, clUnknownTokens, base64RedImage(), e)

		if len(rs) < 2 {
			t.Fatalf("got %d results, want 2", len(rs))
		}
		if rs[1].Op != "tag_roundtrip" || rs[1].Status != "pass" {
			t.Errorf("check 1 (tag unknown tokens): got %+v", rs[1])
		}
		if rs[1].Reason != "no known tags to verify" {
			t.Errorf("tag unknown tokens reason: got %q, want %q", rs[1].Reason, "no known tags to verify")
		}
	})

	// --- Case 8: palette_conform no target -> error ---
	t.Run("PaletteNoTarget", func(t *testing.T) {
		clNoTarget := Checklist{
			Checks: []Check{{Op: "palette_conform"}},
			Target: nil,
		}
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, clNoTarget, base64RedImage(), e)

		if rs[0].Op != "palette_conform" || rs[0].Status != "error" || rs[0].Reason != "no target palette" {
			t.Errorf("palette no target: got %+v", rs[0])
		}
	})

	// --- Case 9: palette_conform pass ---
	t.Run("PalettePass", func(t *testing.T) {
		cl := Checklist{
			Checks: []Check{{Op: "palette_conform"}},
			Target: []string{"#ff0000"},
		}
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		if rs[0].Status != "pass" {
			t.Errorf("palette pass: got %+v", rs[0])
		}
	})

	// --- Case 10: palette_conform fail ---
	t.Run("PaletteFail", func(t *testing.T) {
		cl := Checklist{
			Checks: []Check{{Op: "palette_conform"}},
			Target: []string{"#00ff00"}, // green, but image is red
		}
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		if rs[0].Status != "fail" || rs[0].Suggestion != "palette drift" {
			t.Errorf("palette fail: got %+v", rs[0])
		}
	})

	// --- Case 11: tile_conform pass ---
	t.Run("TilePass", func(t *testing.T) {
		cl := Checklist{
			Checks: []Check{{Op: "tile_conform"}},
		}
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, cl, base64RedImage(), e)

		if rs[0].Status != "pass" {
			t.Errorf("tile pass: got %+v", rs[0])
		}
	})

	// --- Case 12: tile_conform fail with ratio in reason ---
	t.Run("TileFail", func(t *testing.T) {
		// Create an image with mismatched edges (will fail tile check)
		badTileImg := image.NewNRGBA(image.Rect(0, 0, 10, 10))
		for y := 0; y < 10; y++ {
			for x := 0; x < 10; x++ {
				switch {
				case x == 0:
					badTileImg.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 255})
				case x == 9:
					badTileImg.SetNRGBA(x, y, color.NRGBA{0, 0, 255, 255})
				case y == 0:
					badTileImg.SetNRGBA(x, y, color.NRGBA{0, 255, 0, 255})
				case y == 9:
					badTileImg.SetNRGBA(x, y, color.NRGBA{255, 255, 0, 255})
				default:
					badTileImg.SetNRGBA(x, y, color.NRGBA{100, 100, 100, 255})
				}
			}
		}
		// Encode to base64
		badTileB64 := base64BadTileImage()

		cl := Checklist{
			Checks: []Check{{Op: "tile_conform", MaxRatio: 1.3}},
		}
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, cl, badTileB64, e)

		if rs[0].Status != "fail" {
			t.Errorf("tile fail: got %+v", rs[0])
		}
		if !strings.Contains(rs[0].Reason, "wrap-edge ratio") {
			t.Errorf("tile fail reason should mention wrap-edge ratio: %q", rs[0].Reason)
		}
		if rs[0].Suggestion != "seam present at tile boundary" {
			t.Errorf("tile fail suggestion: got %q, want %q", rs[0].Suggestion, "seam present at tile boundary")
		}
	})

	// --- Case 13: Image decode error for palette/tile -> error (inert) ---
	t.Run("ImageDecodeError", func(t *testing.T) {
		cl := Checklist{
			Checks: []Check{{Op: "palette_conform"}},
			Target: []string{"#ff0000"},
		}
		e := Env{
			Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
				return []VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
			},
			Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
				return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
			},
			Vocab: func(ctx context.Context) ([]string, error) {
				return []string{"1girl"}, nil
			},
		}

		rs := Run(ctx, cl, "not-a-valid-base64", e)

		if rs[0].Status != "error" || !strings.Contains(rs[0].Reason, "decode") {
			t.Errorf("image decode error: got %+v", rs[0])
		}
	})
}

func TestAggregate(t *testing.T) {
	cases := []struct {
		name   string
		checks []storage.CriticCheck
		want   string
	}{
		{"has fail", []storage.CriticCheck{
			{Status: "pass"}, {Status: "fail"}}, "failed"},
		{"multiple fails", []storage.CriticCheck{
			{Status: "fail"}, {Status: "fail"}}, "failed"},
		{"all pass", []storage.CriticCheck{
			{Status: "pass"}, {Status: "pass"}}, "pass"},
		{"single pass", []storage.CriticCheck{{Status: "pass"}}, "pass"},
		{"all inert (undetermined)", []storage.CriticCheck{
			{Status: "undetermined"}, {Status: "undetermined"}}, "indeterminate"},
		{"all inert (error)", []storage.CriticCheck{
			{Status: "error"}, {Status: "error"}}, "indeterminate"},
		{"mix inert and pass", []storage.CriticCheck{
			{Status: "pass"}, {Status: "undetermined"}}, "pass"},
		{"mix inert and fail", []storage.CriticCheck{
			{Status: "fail"}, {Status: "error"}}, "failed"},
		{"empty", []storage.CriticCheck{}, "indeterminate"},
		{"pass+undetermined", []storage.CriticCheck{
			{Status: "pass"}, {Status: "undetermined"}}, "pass"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Aggregate(tc.checks)
			if got != tc.want {
				t.Fatalf("Aggregate = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScore(t *testing.T) {
	cases := []struct {
		name   string
		checks []storage.CriticCheck
		want   float64
	}{
		{"all pass", []storage.CriticCheck{{Status: "pass"}, {Status: "pass"}}, 1.0},
		{"half pass", []storage.CriticCheck{{Status: "pass"}, {Status: "fail"}}, 0.5},
		{"all fail", []storage.CriticCheck{{Status: "fail"}, {Status: "fail"}}, 0.0},
		{"pass+fail+undetermined", []storage.CriticCheck{
			{Status: "pass"}, {Status: "fail"}, {Status: "undetermined"}}, 0.5},
		{"only inert", []storage.CriticCheck{
			{Status: "undetermined"}, {Status: "error"}}, 0.0},
		{"empty", []storage.CriticCheck{}, 0.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Score(tc.checks)
			if got != tc.want {
				t.Fatalf("Score = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFails(t *testing.T) {
	cases := []struct {
		name   string
		checks []storage.CriticCheck
		want   int
	}{
		{"no fails", []storage.CriticCheck{{Status: "pass"}, {Status: "undetermined"}}, 0},
		{"one fail", []storage.CriticCheck{{Status: "fail"}}, 1},
		{"two fails", []storage.CriticCheck{{Status: "fail"}, {Status: "fail"}}, 2},
		{"fail + pass", []storage.CriticCheck{{Status: "fail"}, {Status: "pass"}}, 1},
		{"empty", []storage.CriticCheck{}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Fails(tc.checks)
			if got != tc.want {
				t.Fatalf("Fails = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRelevant(t *testing.T) {
	cases := []struct {
		name   string
		checks []storage.CriticCheck
		want   int
	}{
		{"pass+fail", []storage.CriticCheck{{Status: "pass"}, {Status: "fail"}}, 2},
		{"pass only", []storage.CriticCheck{{Status: "pass"}}, 1},
		{"fail only", []storage.CriticCheck{{Status: "fail"}}, 1},
		{"inert only", []storage.CriticCheck{{Status: "undetermined"}, {Status: "error"}}, 0},
		{"mixed", []storage.CriticCheck{{Status: "pass"}, {Status: "fail"}, {Status: "undetermined"}}, 2},
		{"empty", []storage.CriticCheck{}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Relevant(tc.checks)
			if got != tc.want {
				t.Fatalf("Relevant = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMutatedPrompt(t *testing.T) {
	base := "a cat, red background, high quality"

	t.Run("basic mutation from fails", func(t *testing.T) {
		rs := []storage.CriticCheck{
			{Op: "vlm_check", Status: "fail", Suggestion: "add cat ears"},
			{Op: "tag_roundtrip", Status: "fail", Suggestion: "strengthen: whiskers, tail"},
			{Op: "palette_conform", Status: "fail", Suggestion: "palette drift"},
		}
		got := MutatedPrompt(base, rs)
		want := "a cat, red background, high quality, add cat ears, strengthen: whiskers, tail, palette drift"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("dedupe case-insensitive", func(t *testing.T) {
		rs := []storage.CriticCheck{
			{Status: "fail", Suggestion: "add cat"},
			{Status: "fail", Suggestion: "ADD CAT"}, // duplicate case-insensitive
			{Status: "fail", Suggestion: "add dog"},
		}
		got := MutatedPrompt(base, rs)
		want := "a cat, red background, high quality, add cat, add dog"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("skip if substring of base", func(t *testing.T) {
		rs := []storage.CriticCheck{
			{Status: "fail", Suggestion: "cat"},     // "cat" is in base
			{Status: "fail", Suggestion: "red"},     // "red" is in base
			{Status: "fail", Suggestion: "add dog"}, // not in base
		}
		got := MutatedPrompt(base, rs)
		want := "a cat, red background, high quality, add dog"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("skip empty suggestion", func(t *testing.T) {
		rs := []storage.CriticCheck{
			{Status: "fail", Suggestion: ""},
			{Status: "fail", Suggestion: "add dog"},
		}
		got := MutatedPrompt(base, rs)
		want := "a cat, red background, high quality, add dog"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("only pass checks -> no mutation", func(t *testing.T) {
		rs := []storage.CriticCheck{
			{Status: "pass", Suggestion: "something"},
			{Status: "pass"},
		}
		got := MutatedPrompt(base, rs)
		if got != base {
			t.Errorf("got %q, want base unchanged %q", got, base)
		}
	})

	t.Run("empty checks -> no mutation", func(t *testing.T) {
		got := MutatedPrompt(base, []storage.CriticCheck{})
		if got != base {
			t.Errorf("got %q, want base unchanged %q", got, base)
		}
	})

	t.Run("truncation at MutationMaxChars at comma boundary", func(t *testing.T) {
		// Build a base + suggestions that exceeds MutationMaxChars (1200)
		// Base is 500 chars, suggestions push it over 1200
		longBase := strings.Repeat("a, ", 200) // ~600 chars
		rs := []storage.CriticCheck{
			{Status: "fail", Suggestion: strings.Repeat("suggestion, ", 30)}, // ~390 chars
		}
		got := MutatedPrompt(longBase, rs)
		if len(got) > MutationMaxChars {
			t.Errorf("length %d exceeds MutationMaxChars %d", len(got), MutationMaxChars)
		}
		// Should be truncated at comma boundary
		if got != longBase && !strings.HasSuffix(got, ",") {
			t.Errorf("got %q should end at comma boundary or be base unchanged", got)
		}
	})

	t.Run("tag_roundtrip suggestion format strengthen: a, b", func(t *testing.T) {
		rs := []storage.CriticCheck{
			{Op: "tag_roundtrip", Status: "fail", Suggestion: "strengthen: whiskers, tail"},
		}
		got := MutatedPrompt(base, rs)
		if !strings.Contains(got, "strengthen: whiskers, tail") {
			t.Errorf("got %q, want to contain strengthen suggestion", got)
		}
	})

	t.Run("empty base returns suggestions without leading comma", func(t *testing.T) {
		rs := []storage.CriticCheck{
			{Status: "fail", Suggestion: "add cat"},
			{Status: "fail", Suggestion: "add dog"},
		}
		got := MutatedPrompt("", rs)
		want := "add cat, add dog"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if strings.HasPrefix(got, ", ") {
			t.Errorf("got %q should not have leading comma", got)
		}
	})

	t.Run("truncation exactly at MutationMaxChars boundary", func(t *testing.T) {
		// Build base + suggestions that exactly equals MutationMaxChars
		// Base is "base" (4 chars), need 1196 more to reach 1200
		// "a, " is 3 chars, 1196/3 ≈ 398
		longBase := "base" + strings.Repeat("a, ", 398) // 4 + 1194 = 1198
		rs := []storage.CriticCheck{
			{Status: "fail", Suggestion: "end"}, // 3 chars + 2 for ", " = 5, total = 1203 > 1200
		}
		got := MutatedPrompt(longBase, rs)
		if len(got) > MutationMaxChars {
			t.Errorf("length %d exceeds MutationMaxChars %d", len(got), MutationMaxChars)
		}
		// Should be truncated at comma boundary
		if got != longBase && !strings.HasSuffix(got, ",") {
			t.Errorf("got %q should end at comma boundary or be base unchanged", got)
		}
	})
}

// assertError is a test helper to create an error.
func assertError(msg string) error {
	return &testError{msg}
}

type testError struct {
	msg string
}

func (e *testError) Error() string { return e.msg }

// imageToBase64 encodes an image to base64 PNG for testing.
func imageToBase64(t *testing.T, img image.Image) string {
	t.Helper()
	// Simple base64 encoding without external deps - we'll just use a known valid one
	// For testing, we'll use a minimal valid PNG
	return "iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAYAAACNMs+9AAAAFUlEQVR4Ae3BMQEAAADCoPdPbQ43oAAAAAAAAAAAAACY8gIRAAABkVJfAAAAAElFTkSuQmCC"
}
