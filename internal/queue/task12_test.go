package queue

// Task 12 compose-integration tests: validated children compose from their
// chosen attempt, asset/GIF manifests carry the critic summaries, GIF
// provided frames are checked pre-encoding, and palette-lock remap still
// runs after attempts. Kept separate from compose_test.go (2600+ lines) so
// this task's surface stays reviewable; all helpers come from the shared
// test files in this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"img-gen/internal/critic"
	"img-gen/internal/lattice"
	"img-gen/internal/storage"
)

func ptrInt(v int) *int { return &v }

// driftCriticEnv wires an all-present judge and a tagger that always reports
// the "1girl" tag. The vocab knows "1girl" and "drift_tag", so a job whose
// prompt asks for drift_tag drifts on every render — the drift lives in the
// frame's source-job prompt, not in tagger variance.
func driftCriticEnv() critic.Env {
	return critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			scores := make([]critic.VLMScore, len(criteria))
			for i := range criteria {
				scores[i] = critic.VLMScore{ID: i, Pass: true, Reason: "ok"}
			}
			return scores, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "drift_tag"}, nil
		},
	}
}

// submitFrameJobs renders two plain asset jobs and waits for both. Prompts
// are comma-separated danbooru-style tokens so PromptTags tokenizes them;
// each job's final prompt (job.Prompt after run()) is the drift reference
// the compose side records for its frame.
func submitFrameJobs(t *testing.T, m *Manager, promptA, promptB string) (jobA, jobB string) {
	t.Helper()
	j1, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: promptA, Size: "32x32"})
	if err != nil {
		t.Fatal(err)
	}
	j2, err := m.Submit(SubmitRequest{Mode: "asset", Prompt: promptB, Size: "32x32"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j1, j2} {
		j := waitFor(t, m, id, "done")
		if j.Status != "done" {
			t.Fatalf("frame job %s: %s %q", id, j.Status, j.Error)
		}
	}
	return j1, j2
}

// --- asset manifest summaries ----------------------------------------------

// TestComposeAssetSetManifestValidateAttempts verifies the manifest records
// the first child's Validate summary and Attempts per asset.
func TestComposeAssetSetManifestValidateAttempts(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(42)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "validated-set", Seed: &seed,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "a", Prompt: "asset a", Size: "16x16",
				Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test a"`)}}, Budget: ptrInt(2)},
			},
			{Kind: "element", Name: "b", Prompt: "asset b", Size: "16x16",
				Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test b"`)}}, Budget: ptrInt(2)},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("set: %s %q", j.Status, j.Error)
	}

	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}

	if len(man.Assets) != 2 {
		t.Fatalf("want 2 assets, got %d", len(man.Assets))
	}
	for i, ma := range man.Assets {
		if ma.Validate == nil {
			t.Fatalf("asset %d: Validate is nil", i)
		}
		if ma.Validate.Verdict != "pass" {
			t.Fatalf("asset %d: verdict = %q, want pass", i, ma.Validate.Verdict)
		}
		if ma.Validate.Attempts != 1 {
			t.Fatalf("asset %d: attempts = %d, want 1", i, ma.Validate.Attempts)
		}
		if len(ma.Attempts) != 1 {
			t.Fatalf("asset %d: Attempts len = %d, want 1", i, len(ma.Attempts))
		}
		if !ma.Attempts[0].Chosen {
			t.Fatalf("asset %d: attempt not marked chosen", i)
		}
		if ma.Attempts[0].Image == "" {
			t.Fatalf("asset %d: attempt image id empty", i)
		}
		if len(ma.Attempts[0].Checks) != 1 || ma.Attempts[0].Checks[0].Op != "vlm_check" {
			t.Fatalf("asset %d: checks mismatch: %+v", i, ma.Attempts[0].Checks)
		}
	}
}

// TestComposeAssetSetValidatedFailRetryPass verifies that an asset child
// that fails then retries and passes composes from the CHOSEN attempt
// (childResult must read the child's ImagePath — attempt renders are saved
// under attempt ids, not the job id) and that the manifest shows attempts=2
// with the mutated retry prompt.
func TestComposeAssetSetValidatedFailRetryPass(t *testing.T) {
	judgeCalls := 0
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			judgeCalls++
			scores := make([]critic.VLMScore, len(criteria))
			if judgeCalls == 1 {
				for i := range criteria {
					scores[i] = critic.VLMScore{ID: i, Pass: false, Reason: "fail first", Fix: "retry harder"}
				}
			} else {
				for i := range criteria {
					scores[i] = critic.VLMScore{ID: i, Pass: true, Reason: "ok on retry"}
				}
			}
			return scores, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "test", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"test"}, nil
		},
	}
	m := New(o)
	seed := int64(200)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "retry-asset", Seed: &seed,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "a", Prompt: "retry me", Size: "16x16",
				Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test"`)}}, Budget: ptrInt(2)},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("set: %s %q", j.Status, j.Error)
	}

	// The retry really re-rendered once: two dispatch calls, second with the
	// mutated prompt and the unchanged seed.
	cap.mu.Lock()
	if len(cap.Prompts) != 2 {
		cap.mu.Unlock()
		t.Fatalf("want 2 renders, got %d", len(cap.Prompts))
	}
	if !strings.Contains(cap.Prompts[1], "retry harder") {
		cap.mu.Unlock()
		t.Fatalf("attempt-2 prompt = %q, want mutated with suggestion", cap.Prompts[1])
	}
	if cap.Seeds[0] != 200 || cap.Seeds[1] != 200 {
		cap.mu.Unlock()
		t.Fatalf("seeds = %v, want 200 on both attempts (pinned-seed retry)", cap.Seeds)
	}
	cap.mu.Unlock()

	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Assets) != 1 {
		t.Fatalf("want 1 asset, got %d", len(man.Assets))
	}
	ma := man.Assets[0]
	if ma.Validate == nil {
		t.Fatal("Validate is nil")
	}
	if ma.Validate.Attempts != 2 {
		t.Fatalf("Validate.Attempts = %d, want 2", ma.Validate.Attempts)
	}
	if len(ma.Attempts) != 2 {
		t.Fatalf("Attempts len = %d, want 2", len(ma.Attempts))
	}
	if ma.Attempts[0].Chosen {
		t.Fatal("first attempt should not be chosen")
	}
	if !ma.Attempts[1].Chosen {
		t.Fatal("second attempt should be chosen")
	}
	if !strings.Contains(ma.Attempts[1].Prompt, "retry harder") {
		t.Fatalf("retry prompt missing suggestion: %q", ma.Attempts[1].Prompt)
	}

	// The set composed at all — childResult resolved the chosen attempt's
	// image through the child's ImagePath.
	if _, err := m.opts.Store.ReadSetFile(id, "a.png"); err != nil {
		t.Fatalf("set deliverable missing (compose skipped the chosen attempt): %v", err)
	}
}

// --- GIF provided-frame checks ---------------------------------------------

// TestGIFFromFramesValidateOnFailDeliver verifies that a source frame whose
// roundtrip drifts (the frame's source-job prompt asks for drift_tag) is
// recorded and the GIF still assembles under the deliver default.
func TestGIFFromFramesValidateOnFailDeliver(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Critic = driftCriticEnv()
	m := New(o)

	j1, j2 := submitFrameJobs(t, m, "1girl", "1girl, drift_tag")

	id, err := m.SubmitGIF(GifRequest{
		Name: "spin-drift",
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "tag_roundtrip"}},
			OnFail: "deliver",
		},
		Source: GifSource{Type: "from_frames", Frames: []FrameRef{{Job: j1}, {Job: j2}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("gif parent: %s %q", j.Status, j.Error)
	}

	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.GIF == nil {
		t.Fatal("gif manifest missing")
	}
	if len(man.GIF.FrameChecks) != 2 {
		t.Fatalf("want 2 frame checks, got %d", len(man.GIF.FrameChecks))
	}
	fc0 := man.GIF.FrameChecks[0]
	if fc0.File != "source-frame-0000.png" {
		t.Fatalf("frame 0 file = %q", fc0.File)
	}
	if len(fc0.Checks) != 1 || fc0.Checks[0].Op != "tag_roundtrip" || fc0.Checks[0].Status != "pass" {
		t.Fatalf("frame 0: want tag_roundtrip pass, got %+v", fc0.Checks)
	}
	fc1 := man.GIF.FrameChecks[1]
	if fc1.File != "source-frame-0001.png" {
		t.Fatalf("frame 1 file = %q", fc1.File)
	}
	if len(fc1.Checks) != 1 || fc1.Checks[0].Status != "fail" {
		t.Fatalf("frame 1: want fail, got %+v", fc1.Checks)
	}
	if !strings.Contains(fc1.Checks[0].Reason, "danbooru drift") {
		t.Fatalf("frame 1: want drift reason, got %q", fc1.Checks[0].Reason)
	}
	if !strings.Contains(fc1.Checks[0].Suggestion, "strengthen: drift_tag") {
		t.Fatalf("frame 1: want strengthen suggestion, got %q", fc1.Checks[0].Suggestion)
	}
	if _, err := m.opts.Store.ReadSetFile(id, "spin-drift.gif"); err != nil {
		t.Fatalf("gif not delivered despite deliver on_fail: %v", err)
	}
}

// TestGIFFromFramesValidateOnFailFail verifies that with on_fail=fail a
// drifting frame fails the parent, naming the frame and the operation.
func TestGIFFromFramesValidateOnFailFail(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Critic = driftCriticEnv()
	m := New(o)

	j1, j2 := submitFrameJobs(t, m, "1girl", "1girl, drift_tag")

	id, err := m.SubmitGIF(GifRequest{
		Name: "spin-fail",
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "tag_roundtrip"}},
			OnFail: "fail",
		},
		Source: GifSource{Type: "from_frames", Frames: []FrameRef{{Job: j1}, {Job: j2}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "failed")
	if j.Status != "failed" {
		t.Fatalf("want failed, got %s", j.Status)
	}
	if !strings.Contains(j.Error, "compose_error: frame 1: tag_roundtrip failed") ||
		!strings.Contains(j.Error, "danbooru drift") {
		t.Fatalf("error = %q, want compose_error naming frame 1, the op and the reason", j.Error)
	}
}

// TestGIFCompositeAnimValidateOnFailDeliver verifies that composite anim
// frames (gp.Frames from a composite source) are checked when Validate is
// set, while the static layers are not frame-checked and the GIF still
// assembles with the drift recorded.
func TestGIFCompositeAnimValidateOnFailDeliver(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Critic = driftCriticEnv()
	m := New(o)

	j1, j2 := submitFrameJobs(t, m, "1girl", "1girl, drift_tag")

	id, err := m.SubmitGIF(GifRequest{
		Name: "comp-drift",
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "tag_roundtrip"}},
			OnFail: "deliver",
		},
		Source: GifSource{
			Type: "composite",
			Composite: &CompositeSource{
				Anim: []FrameRef{{Job: j1}, {Job: j2}},
				Statics: []StaticLayer{
					{From: FrameRef{B64: b64PNG(t, 8, 8, color.NRGBA{R: 255, A: 255})}, Pos: [2]int{0, 0}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("gif parent: %s %q", j.Status, j.Error)
	}

	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if len(man.GIF.FrameChecks) != 2 {
		t.Fatalf("want 2 frame checks (statics excluded), got %d", len(man.GIF.FrameChecks))
	}
	for _, fc := range man.GIF.FrameChecks {
		if strings.HasPrefix(fc.File, "static-layer-") {
			t.Fatalf("static layer was frame-checked: %q", fc.File)
		}
	}
	if fc1 := man.GIF.FrameChecks[1]; len(fc1.Checks) != 1 || fc1.Checks[0].Status != "fail" {
		t.Fatalf("frame 1: want fail, got %+v", fc1.Checks)
	}
	if _, err := m.opts.Store.ReadSetFile(id, "comp-drift.gif"); err != nil {
		t.Fatalf("gif not delivered: %v", err)
	}
}

// TestGIFKeyframesNotFrameChecked verifies that keyframes anchors are NOT
// frame-checked — a validating spec with on_fail=fail and a judge that fails
// everything must still compile done with zero judge calls, since the
// anchors interpolate through the RIFE sidecar and were never meant as
// checked sources.
func TestGIFKeyframesNotFrameChecked(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	judgeCalls := 0
	var mu sync.Mutex
	o.Critic.Judge = func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
		mu.Lock()
		judgeCalls++
		mu.Unlock()
		return []critic.VLMScore{{ID: 0, Pass: false, Reason: "should never be called"}}, nil
	}
	o.Ops.Rife = func(ctx context.Context, anchors [][]byte, multiplier int) ([][]byte, error) {
		return anchors, nil
	}
	m := New(o)

	anchors := []FrameRef{
		{B64: b64PNG(t, 4, 4, color.NRGBA{R: 255, A: 255})},
		{B64: b64PNG(t, 4, 4, color.NRGBA{B: 255, A: 255})},
	}
	id, err := m.SubmitGIF(GifRequest{
		Name: "kf-no-check",
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test"`)}},
			Budget: ptrInt(2),
			OnFail: "fail",
		},
		Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: anchors}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("keyframes parent: %s %q", j.Status, j.Error)
	}
	mu.Lock()
	calls := judgeCalls
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("judge called %d time(s) for keyframes anchors", calls)
	}

	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.GIF != nil && len(man.GIF.FrameChecks) != 0 {
		t.Fatalf("keyframes should not have frame_checks, got %d", len(man.GIF.FrameChecks))
	}
}

// --- FrameRef resolution for validated jobs (C3) ----------------------------

// TestResolveFrameRefValidatedJobAttemptFallback verifies the C3 fallback: a
// validated job saves its chosen render under the attempt id, so a FrameRef
// with that job id resolves through the job's recorded ImagePath instead of
// the nonexistent images/<jobID>.png.
func TestResolveFrameRefValidatedJobAttemptFallback(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap)) // judge passes on the first attempt
	seed := int64(400)
	jid, err := m.Submit(SubmitRequest{
		Mode: "asset", Prompt: "frame source", Size: "16x16", Seed: &seed,
		Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"ok"`)}}, Budget: ptrInt(2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, jid, "done")
	if j.Status != "done" {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}

	// Precondition: validated jobs have no job-id image — the chosen attempt
	// id holds the bytes.
	if _, err := m.opts.Store.ReadImage(jid); err == nil {
		t.Fatal("precondition violated: images/<jobID>.png exists for a validated job")
	}
	attID := strings.TrimSuffix(strings.TrimPrefix(j.ImagePath, "images/"), ".png")
	if attID == "" || j.ImagePath == "" {
		t.Fatalf("chosen attempt ImagePath missing: %q", j.ImagePath)
	}
	want, err := m.opts.Store.ReadImage(attID)
	if err != nil {
		t.Fatalf("chosen attempt unreadable: %v", err)
	}

	got, err := m.resolveFrameRef(FrameRef{Job: jid})
	if err != nil {
		t.Fatalf("fallback failed even though the job record holds ImagePath %q: %v", j.ImagePath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("fallback resolved the wrong bytes: %d vs %d", len(got), len(want))
	}

	// The resolved bytes are the attempt PNG, decodable — compose actually
	// uses them as a frame.
	if _, err := png.Decode(bytes.NewReader(got)); err != nil {
		t.Fatalf("resolved frame is not a decodable PNG: %v", err)
	}
}

// TestResolveFrameRefFallbackErrorPreservation verifies the fallback never
// masks the original not-found: a missing job record, a record with no
// ImagePath, a non-attempt-shaped path, and an attempt file that is itself
// missing all keep ReadImage's original error (which names the job id).
func TestResolveFrameRefFallbackErrorPreservation(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	const ghostJob = "aabbccddeeff0011"

	inject := func(id, imagePath string) {
		m.mu.Lock()
		m.jobs[id] = &storage.Job{ID: id, ImagePath: imagePath}
		m.mu.Unlock()
	}

	t.Run("no job record", func(t *testing.T) {
		_, err := m.resolveFrameRef(FrameRef{Job: ghostJob})
		if err == nil {
			t.Fatal("want error for missing job frame source")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("want the original not-found shape, got %v", err)
		}
		if !strings.Contains(err.Error(), ghostJob) {
			t.Fatalf("error should name the job id, got %q", err.Error())
		}
	})

	t.Run("record with empty ImagePath", func(t *testing.T) {
		inject(ghostJob, "")
		_, err := m.resolveFrameRef(FrameRef{Job: ghostJob})
		if err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), ghostJob) {
			t.Fatalf("original not-found not preserved, got %v", err)
		}
	})

	t.Run("record with non-attempt-shaped ImagePath", func(t *testing.T) {
		inject(ghostJob, "images/nested/dir.png")
		_, err := m.resolveFrameRef(FrameRef{Job: ghostJob})
		if err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), ghostJob) {
			t.Fatalf("original not-found not preserved, got %v", err)
		}
	})

	t.Run("attempt file also missing keeps the job-id error", func(t *testing.T) {
		inject(ghostJob, "images/deadbeef00000000.png")
		_, err := m.resolveFrameRef(FrameRef{Job: ghostJob})
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("want not-found, got %v", err)
		}
		// The preserved error is the ORIGINAL ReadImage(ref.Job) failure — it
		// names images/<jobID>.png, not the attempt path (which would have
		// produced a different path in the message).
		if !strings.Contains(err.Error(), ghostJob) {
			t.Fatalf("preserved error must name the job id path, got %q", err.Error())
		}
		if strings.Contains(err.Error(), "deadbeef00000000") {
			t.Fatalf("error should be the original job-id read failure, got %q", err.Error())
		}
	})
}

// TestGIFFromSpecChildrenManifestVerify verifies that from_spec GIF children
// (validated in their own jobs) record the first child's Validate/Attempts on
// the GIF manifest.
func TestGIFFromSpecChildrenManifestVerify(t *testing.T) {
	cap := &genCapture{}
	m := New(composeTestOpts(t, t.TempDir(), cap))
	seed := int64(100)
	id, err := m.SubmitGIF(GifRequest{
		Name: "fromspec-validate",
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"gif test"`)}},
			Budget: ptrInt(2),
		},
		Source: GifSource{
			Type:    "from_spec",
			Request: &SubmitRequest{Mode: "asset", Prompt: "pulse orb", Size: "32x32", Seed: &seed},
			Count:   3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("gif: %s %q", j.Status, j.Error)
	}

	mraw, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var man setManifest
	if err := json.Unmarshal(mraw, &man); err != nil {
		t.Fatal(err)
	}
	if man.GIF == nil {
		t.Fatal("gif manifest missing")
	}
	if man.GIF.Validate == nil {
		t.Fatal("GIF Validate is nil")
	}
	if man.GIF.Validate.Verdict != "pass" {
		t.Fatalf("GIF verdict = %q, want pass", man.GIF.Validate.Verdict)
	}
	if man.GIF.Validate.Attempts != 1 {
		t.Fatalf("GIF attempts = %d, want 1", man.GIF.Validate.Attempts)
	}
	if len(man.GIF.Attempts) != 1 {
		t.Fatalf("GIF Attempts len = %d, want 1", len(man.GIF.Attempts))
	}
	if !man.GIF.Attempts[0].Chosen {
		t.Fatal("GIF attempt not marked chosen")
	}
}

// --- palette lock ordering --------------------------------------------------

// TestPaletteLockValidatedChildren verifies the palette-lock remap still runs
// after validation attempts settle: children retry (fail→retry→pass), and the
// final deliverables' opaque pixels stay inside the set palette.
func TestPaletteLockValidatedChildren(t *testing.T) {
	judgeCalls := 0
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	o.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			judgeCalls++
			scores := make([]critic.VLMScore, len(criteria))
			for i := range criteria {
				if judgeCalls%2 == 1 {
					scores[i] = critic.VLMScore{ID: i, Pass: false, Reason: "no", Fix: "try again"}
				} else {
					scores[i] = critic.VLMScore{ID: i, Pass: true, Reason: "ok"}
				}
			}
			return scores, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "test", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"test"}, nil
		},
	}
	m := New(o)
	seed := int64(300)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "pal-lock-validated", Seed: &seed, PaletteLock: true,
		Assets: []AssetSpecIn{
			{Kind: "element", Name: "a", Prompt: "red thing", Size: "16x16",
				Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test"`)}}, Budget: ptrInt(2)},
			},
			{Kind: "element", Name: "b", Prompt: "blue thing", Size: "16x16",
				Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test"`)}}, Budget: ptrInt(2)},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Status != "done" {
		t.Fatalf("set: %s %q", j.Status, j.Error)
	}
	// Both children settled their attempt chains (fail → retry → pass).
	cap.mu.Lock()
	if len(cap.Prompts) != 4 {
		cap.mu.Unlock()
		t.Fatalf("want 4 renders (2 children x 2 attempts), got %d", len(cap.Prompts))
	}
	cap.mu.Unlock()

	mraw, err := m.opts.Store.ReadSetFile(id, "pal-lock-validated.palette.json")
	if err != nil {
		t.Fatalf("palette file: %v", err)
	}
	var pal struct {
		Colors []string `json:"colors"`
	}
	if err := json.Unmarshal(mraw, &pal); err != nil {
		t.Fatal(err)
	}
	if len(pal.Colors) == 0 {
		t.Fatal("palette file has no colors")
	}
	allowed := map[string]bool{}
	for _, hx := range pal.Colors {
		if !strings.HasPrefix(hx, "#") || len(hx) != 7 {
			t.Fatalf("bad palette hex %q", hx)
		}
		allowed[strings.ToLower(hx)] = true
	}
	for _, name := range []string{"a.png", "b.png"} {
		data, err := m.opts.Store.ReadSetFile(id, name)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				n := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				if n.A < 128 {
					continue
				}
				hx := fmt.Sprintf("#%02x%02x%02x", n.R, n.G, n.B)
				if !allowed[hx] {
					t.Fatalf("%s pixel (%d,%d) = %s is outside the set palette", name, x, y, hx)
				}
			}
		}
	}
}
