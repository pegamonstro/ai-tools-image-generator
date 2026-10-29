package queue

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"img-gen/internal/compose"
	"img-gen/internal/critic"
	"img-gen/internal/lattice"
	"img-gen/internal/prompting"
	"img-gen/internal/storage"
)

// fakeRenderCall captures a render call for test verification.
type fakeRenderCall struct {
	Prompt string
	Seed   *int64
}

// fakeRenderResult defines a scripted render result for tests.
type fakeRenderResult struct {
	PNG  []byte
	Seed *int64
	Err  error
}

// tinyPNG returns a minimal valid PNG (1x1 red pixel) as bytes.
func tinyPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// tinyPNGBase64 returns a minimal valid PNG as base64 string.
func tinyPNGBase64() string {
	return base64.StdEncoding.EncodeToString(tinyPNG())
}

// makeTestJob creates a test Manager with a fake renderer that captures
// (prompt, seed) per call and returns scripted results.
// Returns the Manager and a function to retrieve captured calls.
func makeTestJob(t *testing.T, script []fakeRenderResult) (*Manager, func() []fakeRenderCall) {
	t.Helper()
	idx := 0
	var mu sync.Mutex
	var calls []fakeRenderCall

	opts := Options{
		Genres: testCatalog(),
		Store: func() *storage.Store {
			s, err := storage.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		}(),
		Ops: ImageOps{
			Generate: func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
				mu.Lock()
				callIdx := idx
				if idx < len(script) {
					idx++
				}
				mu.Unlock()

				call := fakeRenderCall{
					Prompt: prompt,
					Seed:   sp.Seed,
				}
				if sp.Seed != nil {
					v := *sp.Seed
					call.Seed = &v
				}
				calls = append(calls, call)

				if callIdx >= len(script) {
					return nil, nil, errors.New("render script exhausted")
				}
				r := script[callIdx]
				if r.Err != nil {
					return nil, nil, r.Err
				}
				return r.PNG, r.Seed, nil
			},
		},
		Chat: func(ctx context.Context, msgs []prompting.Message) (string, error) { return "enhanced", nil },
	}

	m := New(opts)
	getCalls := func() []fakeRenderCall {
		mu.Lock()
		defer mu.Unlock()
		// Return a copy to avoid races
		copy := make([]fakeRenderCall, len(calls))
		for i, c := range calls {
			copy[i] = c
		}
		return copy
	}
	return m, getCalls
}

// waitForCriticDone waits for a job to reach a terminal state (done, failed, cancelled).
func waitForCriticDone(t *testing.T, m *Manager, id string) storage.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := m.Get(id)
		if ok && (j.Status == "done" || j.Status == "failed" || j.Status == "cancelled") {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never reached terminal state", id)
	return storage.Job{}
}

// collectEvents subscribes and collects all events until terminal state.
func collectEvents(t *testing.T, m *Manager, id string) []Event {
	t.Helper()
	ch, cancel := m.Subscribe(id)
	defer cancel()
	var events []Event
	for ev := range ch {
		events = append(events, ev)
		if ev.Status == "done" || ev.Status == "failed" || ev.Status == "cancelled" {
			break
		}
	}
	return events
}

// makeFailingChecklist creates a checklist that will fail on first attempt.
func makeFailingChecklist() critic.Checklist {
	return critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: "must have cat"}},
		Budget: 2,
		OnFail: "deliver",
	}
}

// makeZeroBudgetChecklist creates a checklist with budget 0 (evaluate only).
func makeZeroBudgetChecklist(pass bool) critic.Checklist {
	spec := "test criterion"
	if pass {
		return critic.Checklist{
			Checks: []critic.Check{{Op: "vlm_check", Spec: spec}},
			Budget: 0,
			OnFail: "deliver",
		}
	}
	return critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: spec}},
		Budget: 0,
		OnFail: "deliver",
	}
}

// makeOnFailFailChecklist creates a checklist with on_fail=fail.
func makeOnFailFailChecklist() critic.Checklist {
	return critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: "must have cat"}},
		Budget: 0,
		OnFail: "fail",
	}
}

// makeIndeterminateChecklist creates a checklist with all checks undetermined.
func makeIndeterminateChecklist() critic.Checklist {
	return critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: "test"}},
		Budget: 0,
		OnFail: "deliver",
	}
}

// criticEnvForTest returns a critic.Env where all checks pass (for the fake renderer to use).
func criticEnvForTest(pass bool) critic.Env {
	return critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			return []critic.VLMScore{{ID: 0, Pass: pass, Reason: "ok", Fix: "add cat"}}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}
}

// criticEnvForIndeterminate returns a critic.Env where all checks are undetermined.
func criticEnvForIndeterminate() critic.Env {
	return critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			return nil, errors.New("judge unreadable")
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}
}

func TestCriticLoopPassFirstAttempt(t *testing.T) {
	// Script: first (and only) render returns a valid PNG
	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m, getCalls := makeTestJob(t, script)
	// Inject critic env
	m.opts.Critic = criticEnvForTest(true)

	id, err := m.Submit(SubmitRequest{
		Genre:    "landscape",
		Fields:   map[string]string{"setting": "a valley"},
		Size:     "512x512",
		Seed:     int64Ptr(42),
		Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test criterion"`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	// Assertions
	if j.Status != "done" {
		t.Fatalf("status = %q, want done", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", j.Critic.Verdict)
	}
	if j.Critic.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", j.Critic.Attempts)
	}
	if j.ImagePath == "" {
		t.Fatal("ImagePath empty")
	}
	// The chosen image should be the attempt image
	if !strings.HasPrefix(j.ImagePath, "images/") || !strings.HasSuffix(j.ImagePath, ".png") {
		t.Fatalf("ImagePath = %q, want images/<id>.png", j.ImagePath)
	}

	// Verify attempt image exists on disk
	attemptID := strings.TrimSuffix(strings.TrimPrefix(j.ImagePath, "images/"), ".png")
	if _, err := os.Stat(filepath.Join(m.opts.Store.Dir(), "images", attemptID+".png")); err != nil {
		t.Fatalf("attempt image not on disk: %v", err)
	}

	// Verify exactly one renderer call
	calls := getCalls()
	if len(calls) != 1 {
		t.Fatalf("renderer called %d times, want 1", len(calls))
	}
	if calls[0].Prompt != "A landscape: a valley." {
		t.Fatalf("prompt = %q, want base prompt", calls[0].Prompt)
	}
	if calls[0].Seed == nil || *calls[0].Seed != 42 {
		t.Fatalf("seed = %v, want 42", calls[0].Seed)
	}

	// Verify tEXt chunks on chosen PNG
	data, err := m.opts.Store.ReadImage(attemptID)
	if err != nil {
		t.Fatalf("read attempt image: %v", err)
	}
	chunks, err := compose.ScanTextChunks(data)
	if err != nil {
		t.Fatalf("scan chunks: %v", err)
	}
	if chunks["critic_verdict"] != "pass" {
		t.Fatalf("critic_verdict = %q, want pass", chunks["critic_verdict"])
	}
	if chunks["critic_attempt"] != "1" {
		t.Fatalf("critic_attempt = %q, want 1", chunks["critic_attempt"])
	}
	if chunks["critic_vlm_check"] != "pass" {
		t.Fatalf("critic_vlm_check = %q, want pass", chunks["critic_vlm_check"])
	}
}

func TestCriticLoopFailThenMutateThenPass(t *testing.T) {
	// Budget 2 allows 1+2 = 3 attempts
	_ = makeFailingChecklist() // Budget=2, OnFail=deliver

	// Script: 3 renders - attempt 1 fails (judge returns fail), attempt 2 fails, attempt 3 passes
	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)}, // attempt 1
		{PNG: tinyPNG(), Seed: int64Ptr(100)}, // attempt 2 (same seed)
		{PNG: tinyPNG(), Seed: int64Ptr(101)}, // attempt 3 (seed+1)
	}

	m, getCalls := makeTestJob(t, script)
	// Custom env: first two calls fail, third passes
	callCount := 0
	m.opts.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			callCount++
			if callCount <= 2 {
				return []critic.VLMScore{{ID: 0, Pass: false, Reason: "no cat visible", Fix: "add cat"}}, nil
			}
			return []critic.VLMScore{{ID: 0, Pass: true, Reason: "cat detected"}}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(100),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)}},
			Budget: intPtr(2),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	// Assertions
	if j.Status != "done" {
		t.Fatalf("status = %q, want done", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass (attempt 3 passed)", j.Critic.Verdict)
	}
	if j.Critic.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", j.Critic.Attempts)
	}

	// Verify exactly 3 renderer calls
	if len(getCalls()) != 3 {
		t.Fatalf("renderer called %d times, want 3", len(getCalls()))
	}

	// Verify prompts: [base, mutation, mutation]
	basePrompt := "A landscape: a valley."
	expectedMutation := basePrompt + ", add cat" // deduped from "add cat"
	if (getCalls())[0].Prompt != basePrompt {
		t.Fatalf("attempt 1 prompt = %q, want %q", (getCalls())[0].Prompt, basePrompt)
	}
	if (getCalls())[1].Prompt != expectedMutation {
		t.Fatalf("attempt 2 prompt = %q, want %q", (getCalls())[1].Prompt, expectedMutation)
	}
	if (getCalls())[2].Prompt != expectedMutation {
		t.Fatalf("attempt 3 prompt = %q, want %q", (getCalls())[2].Prompt, expectedMutation)
	}

	// Verify seeds: [100, 100, 101]
	if (getCalls())[0].Seed == nil || *(getCalls())[0].Seed != 100 {
		t.Fatalf("attempt 1 seed = %v, want 100", (getCalls())[0].Seed)
	}
	if (getCalls())[1].Seed == nil || *(getCalls())[1].Seed != 100 {
		t.Fatalf("attempt 2 seed = %v, want 100 (same seed)", (getCalls())[1].Seed)
	}
	if (getCalls())[2].Seed == nil || *(getCalls())[2].Seed != 101 {
		t.Fatalf("attempt 3 seed = %v, want 101 (seed+1)", (getCalls())[2].Seed)
	}

	// Verify chosen attempt is the last one (attempt 3)
	chosenID := strings.TrimSuffix(strings.TrimPrefix(j.ImagePath, "images/"), ".png")
	if _, err := os.Stat(filepath.Join(m.opts.Store.Dir(), "images", chosenID+".png")); err != nil {
		t.Fatalf("chosen image not on disk: %v", err)
	}

	// Verify attempt images are distinct
	attemptIDs := make(map[string]bool)
	for _, a := range j.Attempts {
		attemptIDs[a.Image] = true
	}
	if len(attemptIDs) != 3 {
		t.Fatalf("expected 3 distinct attempt images, got %d", len(attemptIDs))
	}
	if attemptIDs[chosenID] != true {
		t.Fatal("chosen image not among attempt images")
	}
}

func TestCriticLoopBudgetZeroFailOnFailDeliver(t *testing.T) {
	_ = makeZeroBudgetChecklist(false) // Budget=0, OnFail=deliver

	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m, getCalls := makeTestJob(t, script)
	m.opts.Critic = criticEnvForTest(false) // always fail

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(42),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test criterion"`)}},
			Budget: intPtr(0),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	// Budget 0 = evaluate only (1 attempt max)
	if j.Status != "done" {
		t.Fatalf("status = %q, want done (on_fail=deliver)", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "best_of_n" {
		t.Fatalf("verdict = %q, want best_of_n", j.Critic.Verdict)
	}
	if j.Critic.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", j.Critic.Attempts)
	}
	if len(getCalls()) != 1 {
		t.Fatalf("renderer called %d times, want 1", len(getCalls()))
	}
}

func TestCriticLoopBudgetZeroFailOnFailFail(t *testing.T) {
	_ = makeOnFailFailChecklist() // Budget=0, OnFail=fail

	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m, getCalls := makeTestJob(t, script)
	m.opts.Critic = criticEnvForTest(false) // always fail

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(42),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)}},
			Budget: intPtr(0),
			OnFail: "fail",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	// on_fail=fail should fail the job
	if j.Status != "failed" {
		t.Fatalf("status = %q, want failed (on_fail=fail)", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "failed" {
		t.Fatalf("verdict = %q, want failed", j.Critic.Verdict)
	}
	if !strings.Contains(j.Error, "critic failed:") {
		t.Fatalf("error = %q, want to contain 'critic failed:'", j.Error)
	}
	if len(getCalls()) != 1 {
		t.Fatalf("renderer called %d times, want 1", len(getCalls()))
	}
}

func TestCriticLoopAllIndeterminate(t *testing.T) {
	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m, getCalls := makeTestJob(t, script)
	m.opts.Critic = criticEnvForIndeterminate()

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(42),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test"`)}},
			Budget: intPtr(0),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	if j.Status != "done" {
		t.Fatalf("status = %q, want done", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "indeterminate" {
		t.Fatalf("verdict = %q, want indeterminate", j.Critic.Verdict)
	}
	if j.Critic.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", j.Critic.Attempts)
	}
	if len(getCalls()) != 1 {
		t.Fatalf("renderer called %d times, want 1", len(getCalls()))
	}
}

func TestCriticLoopCancelMidValidating(t *testing.T) {
	_ = makeFailingChecklist() // Budget=2

	// Use a channel to block the Judge during validation
	judgeBlock := make(chan struct{})
	judgeUnblocked := make(chan struct{})

	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	}

	m, _ := makeTestJob(t, script)
	callCount := 0
	m.opts.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			callCount++
			if callCount == 1 {
				// Block during first validation
				close(judgeUnblocked) // signal that we've entered validation
				select {
				case <-judgeBlock:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return []critic.VLMScore{{ID: 0, Pass: false, Reason: "no cat", Fix: "add cat"}}, nil
			}
			if callCount <= 2 {
				return []critic.VLMScore{{ID: 0, Pass: false, Reason: "no cat", Fix: "add cat"}}, nil
			}
			return []critic.VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(100),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)}},
			Budget: intPtr(2),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for first validation to start (Judge is called)
	<-judgeUnblocked

	// Now we're in the validating phase - cancel
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}

	// Unblock the judge so it can clean up
	close(judgeBlock)

	j := waitForCriticDone(t, m, id)

	if j.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", j.Status)
	}
}

func TestCriticLoopSSEValidatingEvents(t *testing.T) {
	_ = makeFailingChecklist() // Budget=2

	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	}

	m, _ := makeTestJob(t, script)
	callCount := 0
	m.opts.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			callCount++
			if callCount <= 2 {
				return []critic.VLMScore{{ID: 0, Pass: false, Reason: "no cat", Fix: "add cat"}}, nil
			}
			return []critic.VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(100),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)}},
			Budget: intPtr(2),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(t, m, id)

	// Verify at least one "validating" status event was seen
	hasValidating := false
	for _, ev := range events {
		if ev.Status == "validating" {
			hasValidating = true
			break
		}
	}
	if !hasValidating {
		t.Fatal("no 'validating' status event received via SSE")
	}

	// Final status should be done
	finalStatus := events[len(events)-1].Status
	if finalStatus != "done" {
		t.Fatalf("final status = %q, want done", finalStatus)
	}
}

func TestCriticLoopAttemptImagesPersisted(t *testing.T) {
	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	}

	m, _ := makeTestJob(t, script)
	callCount := 0
	m.opts.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			callCount++
			if callCount <= 2 {
				return []critic.VLMScore{{ID: 0, Pass: false, Reason: "no cat", Fix: "add cat"}}, nil
			}
			return []critic.VLMScore{{ID: 0, Pass: true, Reason: "ok"}}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(100),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)}},
			Budget: intPtr(2),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	// Verify 3 distinct attempt images exist on disk
	attemptIDs := make(map[string]bool)
	for _, a := range j.Attempts {
		attemptIDs[a.Image] = true
		path := filepath.Join(m.opts.Store.Dir(), "images", a.Image+".png")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("attempt image %s not on disk: %v", a.Image, err)
		}
	}
	if len(attemptIDs) != 3 {
		t.Fatalf("expected 3 distinct attempt images on disk, got %d", len(attemptIDs))
	}

	// Verify chosen == ImagePath
	chosenID := strings.TrimSuffix(strings.TrimPrefix(j.ImagePath, "images/"), ".png")
	if attemptIDs[chosenID] != true {
		t.Fatal("chosen image ID not among attempt images")
	}
}

func TestCriticLoopChosenPNGHasCriticTextChunks(t *testing.T) {
	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m, _ := makeTestJob(t, script)
	m.opts.Critic = criticEnvForTest(true)

	id, err := m.Submit(SubmitRequest{
		Genre:    "landscape",
		Fields:   map[string]string{"setting": "a valley"},
		Size:     "512x512",
		Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test criterion"`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	chosenID := strings.TrimSuffix(strings.TrimPrefix(j.ImagePath, "images/"), ".png")
	data, err := m.opts.Store.ReadImage(chosenID)
	if err != nil {
		t.Fatalf("read chosen image: %v", err)
	}
	chunks, err := compose.ScanTextChunks(data)
	if err != nil {
		t.Fatalf("scan chunks: %v", err)
	}

	// Verify critic_* tEXt chunks
	if chunks["critic_verdict"] != "pass" {
		t.Fatalf("critic_verdict = %q, want pass", chunks["critic_verdict"])
	}
	if chunks["critic_attempt"] != "1" {
		t.Fatalf("critic_attempt = %q, want 1", chunks["critic_attempt"])
	}
	if chunks["critic_vlm_check"] != "pass" {
		t.Fatalf("critic_vlm_check = %q, want pass", chunks["critic_vlm_check"])
	}
}

func TestCriticLoopMutationDerivation(t *testing.T) {
	// Test that attempt 2 prompt = base + failing check's suggestion (deduped)

	// First attempt fails both, second passes first but fails second, third passes
	callCount := 0
	m, getCalls := makeTestJob(t, []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	})
	m.opts.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			callCount++
			if callCount == 1 {
				// Both fail with suggestions
				return []critic.VLMScore{
					{ID: 0, Pass: false, Reason: "no cat", Fix: "add cat"},
					{ID: 1, Pass: false, Reason: "no dog", Fix: "add dog"},
				}, nil
			}
			if callCount == 2 {
				// First passes (cat added), second still fails
				return []critic.VLMScore{
					{ID: 0, Pass: true, Reason: "cat detected"},
					{ID: 1, Pass: false, Reason: "no dog", Fix: "add dog"},
				}, nil
			}
			return []critic.VLMScore{
				{ID: 0, Pass: true, Reason: "cat detected"},
				{ID: 1, Pass: true, Reason: "dog detected"},
			}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat", "dog"}, nil
		},
	}

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(100),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{
				{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)},
				{Op: "vlm_check", Arg: json.RawMessage(`"must have dog"`)},
			},
			Budget: intPtr(2),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitForCriticDone(t, m, id)

	// Verify mutation derivation: attempt 2 prompt = base + "add cat, add dog" (deduped, both missing from base)
	basePrompt := "A landscape: a valley."
	expectedMutation1 := basePrompt + ", add cat, add dog"

	if len(getCalls()) != 3 {
		t.Fatalf("renderer called %d times, want 3", len(getCalls()))
	}
	if (getCalls())[0].Prompt != basePrompt {
		t.Fatalf("attempt 1 prompt = %q, want %q", (getCalls())[0].Prompt, basePrompt)
	}
	if (getCalls())[1].Prompt != expectedMutation1 {
		t.Fatalf("attempt 2 prompt = %q, want %q", (getCalls())[1].Prompt, expectedMutation1)
	}
	if (getCalls())[2].Prompt != expectedMutation1 {
		t.Fatalf("attempt 3 prompt = %q, want %q", (getCalls())[2].Prompt, expectedMutation1)
	}
}

func TestCriticLoopBudgetTwoAllFail(t *testing.T) {
	// Budget 2 = 1+2 = 3 attempts max. All 3 fail → verdict "best_of_n" (on_fail=deliver)
	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	}

	m, getCalls := makeTestJob(t, script)
	callCount := 0
	m.opts.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			callCount++
			return []critic.VLMScore{{ID: 0, Pass: false, Reason: "no cat", Fix: "add cat"}}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(100),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)}},
			Budget: intPtr(2),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	if j.Status != "done" {
		t.Fatalf("status = %q, want done", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "best_of_n" {
		t.Fatalf("verdict = %q, want best_of_n", j.Critic.Verdict)
	}
	if j.Critic.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", j.Critic.Attempts)
	}
	if len(getCalls()) != 3 {
		t.Fatalf("renderer called %d times, want 3", len(getCalls()))
	}
}

func TestCriticLoopBestOfTieBreak(t *testing.T) {
	// 3 attempts with equal failing scores → chosen = attempt 1 (lowest index)
	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	}

	m, getCalls := makeTestJob(t, script)
	callCount := 0
	m.opts.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			callCount++
			// All attempts fail with same score (0 pass, 1 fail = 0.0 score)
			return []critic.VLMScore{{ID: 0, Pass: false, Reason: "no cat", Fix: "add cat"}}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(100),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)}},
			Budget: intPtr(2),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	j := waitForCriticDone(t, m, id)

	if j.Status != "done" {
		t.Fatalf("status = %q, want done", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "best_of_n" {
		t.Fatalf("verdict = %q, want best_of_n", j.Critic.Verdict)
	}
	if j.Critic.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", j.Critic.Attempts)
	}

	// Verify attempt 1 (index 0) is chosen (lowest index tie-break)
	chosenID := strings.TrimSuffix(strings.TrimPrefix(j.ImagePath, "images/"), ".png")
	if j.Attempts[0].Chosen != true {
		t.Fatal("attempt 1 should be chosen (lowest index tie-break)")
	}
	if j.Attempts[0].Image != chosenID {
		t.Fatal("chosen image should be attempt 1's image")
	}
	if len(getCalls()) != 3 {
		t.Fatalf("renderer called %d times, want 3", len(getCalls()))
	}
}

func TestCriticLoopCancelsEmptyAfterValidatedDone(t *testing.T) {
	// After a validated chain resolves to done, m.cancels should have no entry for the job.
	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m, _ := makeTestJob(t, script)
	m.opts.Critic = criticEnvForTest(true)

	id, err := m.Submit(SubmitRequest{
		Genre:    "landscape",
		Fields:   map[string]string{"setting": "a valley"},
		Size:     "512x512",
		Seed:     int64Ptr(42),
		Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test criterion"`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitForCriticDone(t, m, id)

	// Check that m.cancels has no entry for this job
	m.mu.Lock()
	_, hasCancel := m.cancels[id]
	m.mu.Unlock()
	if hasCancel {
		t.Fatal("m.cancels should be empty for job after validated chain resolves to done")
	}
}

func TestCriticLoopCancelsEmptyAfterRenderError(t *testing.T) {
	// After a render-error failure, m.cancels should have no entry for the job.
	script := []fakeRenderResult{{PNG: nil, Seed: nil, Err: errors.New("render failed")}}

	m, _ := makeTestJob(t, script)
	m.opts.Critic = criticEnvForTest(true)

	id, err := m.Submit(SubmitRequest{
		Genre:    "landscape",
		Fields:   map[string]string{"setting": "a valley"},
		Size:     "512x512",
		Seed:     int64Ptr(42),
		Validate: &critic.ValidateSpec{Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"test criterion"`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitForCriticDone(t, m, id)

	// Check that m.cancels has no entry for this job
	m.mu.Lock()
	_, hasCancel := m.cancels[id]
	m.mu.Unlock()
	if hasCancel {
		t.Fatal("m.cancels should be empty for job after render-error failure")
	}
}

func TestCriticLoopCancelDuringFinalChecks(t *testing.T) {
	// Budget 1 = 1+1 = 2 attempts max.
	// Attempt 1 fails. Attempt 2 renders, then Judge/checks BLOCK on a channel.
	// Cancel() while status is "validating" on the final attempt.
	// Unblock -> assert job status "cancelled" (NOT done/best_of_n).
	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
	}

	m, _ := makeTestJob(t, script)

	judgeBlock := make(chan struct{})
	judgeUnblocked := make(chan struct{})

	m.opts.Critic = critic.Env{
		Judge: func(ctx context.Context, criteria []string, imageB64 string) ([]critic.VLMScore, error) {
			// First call (attempt 1) returns fail
			// Second call (attempt 2) blocks
			close(judgeUnblocked)
			select {
			case <-judgeBlock:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []critic.VLMScore{{ID: 0, Pass: false, Reason: "no cat", Fix: "add cat"}}, nil
		},
		Tagger: func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			return []lattice.TagScore{{Name: "1girl", Score: 0.9}}, nil
		},
		Vocab: func(ctx context.Context) ([]string, error) {
			return []string{"1girl", "cat"}, nil
		},
	}

	id, err := m.Submit(SubmitRequest{
		Genre:  "landscape",
		Fields: map[string]string{"setting": "a valley"},
		Size:   "512x512",
		Seed:   int64Ptr(100),
		Validate: &critic.ValidateSpec{
			Checks: []critic.CheckSpec{{Op: "vlm_check", Arg: json.RawMessage(`"must have cat"`)}},
			Budget: intPtr(1),
			OnFail: "deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for first attempt to complete and second attempt to start validating
	<-judgeUnblocked

	// Now we're in the validating phase of the final attempt - cancel
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}

	// Unblock the judge so it can clean up
	close(judgeBlock)

	j := waitForCriticDone(t, m, id)

	if j.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled (not done/best_of_n)", j.Status)
	}
}

// ============================================================================
// Task 11: Restart recovery of validated jobs — RED tests
// ============================================================================

// TestCriticRestoreKillDuringChecks verifies that a job killed while checks
// are running (persisted Attempts >= CriticPending) restarts WITHOUT calling
// the sidecar's Result — checks re-run idempotently from the stored attempt
// bytes, and the chain finalizes.
func TestCriticRestoreKillDuringChecks(t *testing.T) {
	// Pre-persist a job that completed attempt 1 (rendered + checks run)
	// but died before the chain could finalize. CriticPending=1, Attempts=[1].
	// Since the render completed, it MUST have a GenID (captured during render).
	dir := t.TempDir()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	attempt1ID := "1111111111111111"
	attempt1PNG := tinyPNG()
	if _, err := s.SaveImage(attempt1ID, attempt1PNG); err != nil {
		t.Fatal(err)
	}

	cl := critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: "must have cat"}},
		Budget: 2,
		OnFail: "deliver",
	}
	clBytes, _ := json.Marshal(cl)

	job := storage.Job{
		ID:               "restore-checks",
		Genre:            "landscape",
		Mode:             "generate",
		Prompt:           "A landscape: a valley.",
		Size:             "512x512",
		Seed:             int64Ptr(42),
		Status:           "validating",
		CreatedAt:        time.Now().Add(-time.Minute),
		GenID:            "gen42", // Render completed, so GenID was captured
		CriticSpec:       string(clBytes),
		CriticPending:    intPtr(1),
		CriticNextPrompt: "A landscape: a valley., add cat",
		Attempts: []storage.CriticAttempt{
			{Image: attempt1ID, Prompt: "A landscape: a valley.", Seed: int64Ptr(42), Checks: []storage.CriticCheck{{Op: "vlm_check", Spec: "must have cat", Status: "fail", Reason: "no cat"}}},
		},
	}
	if err := s.AppendHistory(job); err != nil {
		t.Fatal(err)
	}

	// Fake Ops: Result must NOT be called (we track this)
	resultCalled := false
	opts := testOptsAt(t, dir)
	opts.Ops.Result = func(ctx context.Context, mode, genID, sidecar string) ([]byte, error) {
		resultCalled = true
		return nil, lattice.ErrResultNotFound
	}
	// Critic env: checks FAIL on re-run (idempotent with stored failed checks)
	opts.Critic = criticEnvForTest(false)

	m := New(opts)

	// Wait for terminal state
	j := waitForCriticDone(t, m, "restore-checks")

	// Assert: Result was never called (no sidecar re-attach)
	if resultCalled {
		t.Fatal("Result must NOT be called when kill-during-checks recovers")
	}

	// Assert: job finalizes with verdict from stored checks
	if j.Status != "done" {
		t.Fatalf("status = %q, want done", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "best_of_n" {
		t.Fatalf("verdict = %q, want best_of_n (stored attempt failed)", j.Critic.Verdict)
	}
	if j.Critic.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no new attempt rendered)", j.Critic.Attempts)
	}
	// The chosen image should be attempt 1's image
	chosenID := strings.TrimSuffix(strings.TrimPrefix(j.ImagePath, "images/"), ".png")
	if chosenID != attempt1ID {
		t.Fatalf("chosen image = %q, want %q (stored attempt)", chosenID, attempt1ID)
	}

	// Assert: CriticPending and CriticNextPrompt cleared
	if j.CriticPending != nil {
		t.Fatal("CriticPending must be cleared on terminal")
	}
	if j.CriticNextPrompt != "" {
		t.Fatal("CriticNextPrompt must be cleared on terminal")
	}
}

// TestCriticRestoreKillDuringRenderAttempt2 verifies that a job killed
// during attempt-2 render (GenID learned) restarts by calling the sidecar's
// Result to recover the PNG, records it as the pending attempt with
// CriticNextPrompt as its prompt, runs its checks, and finalizes WITHOUT
// attempt 3 (no-retry limitation). The double-image landmine is gone:
// exactly ONE attempt image id recorded; the job's own id is NOT used as
// an attempt image.
func TestCriticRestoreKillDuringRenderAttempt2(t *testing.T) {
	// Pre-persist a job that completed attempt 1, started attempt 2 (GenID captured),
	// but died during the render. CriticPending=2, Attempts=[1], GenID=set.
	dir := t.TempDir()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	attempt1ID := "1111111111111111"
	attempt1PNG := tinyPNG()
	if _, err := s.SaveImage(attempt1ID, attempt1PNG); err != nil {
		t.Fatal(err)
	}

	cl := critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: "must have cat"}},
		Budget: 2,
		OnFail: "deliver",
	}
	clBytes, _ := json.Marshal(cl)

	job := storage.Job{
		ID:               "restore-render2",
		Genre:            "landscape",
		Mode:             "generate",
		Prompt:           "A landscape: a valley.",
		Size:             "512x512",
		Seed:             int64Ptr(42),
		Status:           "generating",
		CreatedAt:        time.Now().Add(-time.Minute),
		GenID:            "gen42", // GenID was captured before death
		CriticSpec:       string(clBytes),
		CriticPending:    intPtr(2),
		CriticNextPrompt: "A landscape: a valley., add cat",
		Attempts: []storage.CriticAttempt{
			{Image: attempt1ID, Prompt: "A landscape: a valley.", Seed: int64Ptr(42), Checks: []storage.CriticCheck{{Op: "vlm_check", Spec: "must have cat", Status: "fail", Reason: "no cat"}}},
		},
	}
	if err := s.AppendHistory(job); err != nil {
		t.Fatal(err)
	}

	// Fake Ops: Result returns the recovered PNG for attempt 2
	recoveredPNG := tinyPNG()
	resultCallCount := 0
	opts := testOptsAt(t, dir)
	opts.Ops.Result = func(ctx context.Context, mode, genID, sidecar string) ([]byte, error) {
		resultCallCount++
		if mode != "generate" || genID != "gen42" {
			t.Errorf("Result(mode=%s genID=%s)", mode, genID)
		}
		return recoveredPNG, nil
	}
	// Critic env: checks PASS on the recovered attempt
	opts.Critic = criticEnvForTest(true)

	m := New(opts)

	// Wait for terminal state
	j := waitForCriticDone(t, m, "restore-render2")

	// Assert: Result was called exactly once (sidecar re-attach)
	if resultCallCount != 1 {
		t.Fatalf("Result called %d times, want 1", resultCallCount)
	}

	// Assert: job finalizes (no attempt 3 — no-retry limitation)
	if j.Status != "done" {
		t.Fatalf("status = %q, want done", j.Status)
	}
	if j.Critic == nil {
		t.Fatal("Critic summary missing")
	}
	if j.Critic.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass (recovered attempt passed)", j.Critic.Verdict)
	}
	if j.Critic.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (no attempt 3)", j.Critic.Attempts)
	}

	// Assert: exactly 2 distinct attempt images (attempt 1 + recovered attempt 2)
	// The job's own id must NOT be used as an attempt image (double-image landmine)
	attemptIDs := make(map[string]bool)
	for _, a := range j.Attempts {
		attemptIDs[a.Image] = true
		if a.Image == j.ID {
			t.Fatal("job ID must not appear as an attempt image (double-image landmine)")
		}
	}
	if len(attemptIDs) != 2 {
		t.Fatalf("expected 2 distinct attempt images, got %d: %v", len(attemptIDs), attemptIDs)
	}

	// Assert: attempt 2 has the correct prompt (CriticNextPrompt) and seed
	if len(j.Attempts) < 2 {
		t.Fatal("missing attempt 2")
	}
	if j.Attempts[1].Prompt != "A landscape: a valley., add cat" {
		t.Fatalf("attempt 2 prompt = %q, want CriticNextPrompt", j.Attempts[1].Prompt)
	}
	if j.Attempts[1].Seed == nil || *j.Attempts[1].Seed != 42 {
		t.Fatalf("attempt 2 seed = %v, want 42", j.Attempts[1].Seed)
	}

	// Assert: CriticPending and CriticNextPrompt cleared
	if j.CriticPending != nil {
		t.Fatal("CriticPending must be cleared on terminal")
	}
	if j.CriticNextPrompt != "" {
		t.Fatal("CriticNextPrompt must be cleared on terminal")
	}
}

// TestCriticRestoreNoGenIDHonestFail verifies that a job killed mid-chain
// with NO GenID (never learned sidecar gen id) fails honestly with the
// unchanged restart message.
func TestCriticRestoreNoGenIDHonestFail(t *testing.T) {
	// Pre-persist a job that completed attempt 1 but never captured GenID
	// (died before first progress poll surfaced it). CriticPending=2, no GenID.
	dir := t.TempDir()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	attempt1ID := "1111111111111111"
	attempt1PNG := tinyPNG()
	if _, err := s.SaveImage(attempt1ID, attempt1PNG); err != nil {
		t.Fatal(err)
	}

	cl := critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: "must have cat"}},
		Budget: 2,
		OnFail: "deliver",
	}
	clBytes, _ := json.Marshal(cl)

	job := storage.Job{
		ID:               "restore-nogen",
		Genre:            "landscape",
		Mode:             "generate",
		Prompt:           "A landscape: a valley.",
		Size:             "512x512",
		Seed:             int64Ptr(42),
		Status:           "generating",
		CreatedAt:        time.Now().Add(-time.Minute),
		GenID:            "", // NO GenID — never learned it
		CriticSpec:       string(clBytes),
		CriticPending:    intPtr(2),
		CriticNextPrompt: "A landscape: a valley., add cat",
		Attempts: []storage.CriticAttempt{
			{Image: attempt1ID, Prompt: "A landscape: a valley.", Seed: int64Ptr(42), Checks: []storage.CriticCheck{{Op: "vlm_check", Spec: "must have cat", Status: "fail", Reason: "no cat"}}},
		},
	}
	if err := s.AppendHistory(job); err != nil {
		t.Fatal(err)
	}

	opts := testOptsAt(t, dir)
	// Result should never be called (no GenID to recover)
	resultCalled := false
	opts.Ops.Result = func(ctx context.Context, mode, genID, sidecar string) ([]byte, error) {
		resultCalled = true
		return nil, lattice.ErrResultNotFound
	}
	opts.Critic = criticEnvForTest(true)

	m := New(opts)

	// Wait for terminal state
	j := waitForCriticDone(t, m, "restore-nogen")

	// Assert: Result was never called
	if resultCalled {
		t.Fatal("Result must NOT be called when no GenID exists")
	}

	// Assert: honest failure with unchanged message
	if j.Status != "failed" {
		t.Fatalf("status = %q, want failed", j.Status)
	}
	if !strings.Contains(j.Error, "interrupted by a service restart") {
		t.Fatalf("error = %q, want the restart message", j.Error)
	}
	if !strings.Contains(j.Error, "submit the job again") {
		t.Fatalf("error = %q, want resubmission hint", j.Error)
	}

	// Assert: CriticPending and CriticNextPrompt cleared even on failure
	if j.CriticPending != nil {
		t.Fatal("CriticPending must be cleared on terminal failure")
	}
	if j.CriticNextPrompt != "" {
		t.Fatal("CriticNextPrompt must be cleared on terminal failure")
	}
}

// TestCriticRestoreClearsPendingOnAllTerminals verifies that after any
// restart recovery path (success or failure), CriticPending and
// CriticNextPrompt are always cleared — no ghost pending state.
func TestCriticRestoreClearsPendingOnAllTerminals(t *testing.T) {
	// Test the kill-during-checks path (already covered by TestCriticRestoreKillDuringChecks)
	// and add a dedicated check that the fields are cleared in ALL cases.
	// We reuse the kill-during-checks setup.
	dir := t.TempDir()
	s, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	attempt1ID := "1111111111111111"
	if _, err := s.SaveImage(attempt1ID, tinyPNG()); err != nil {
		t.Fatal(err)
	}

	cl := critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: "must have cat"}},
		Budget: 2,
		OnFail: "deliver",
	}
	clBytes, _ := json.Marshal(cl)

	job := storage.Job{
		ID:               "restore-clear",
		Genre:            "landscape",
		Mode:             "generate",
		Prompt:           "A landscape: a valley.",
		Size:             "512x512",
		Seed:             int64Ptr(42),
		Status:           "validating",
		CreatedAt:        time.Now().Add(-time.Minute),
		CriticSpec:       string(clBytes),
		CriticPending:    intPtr(1),
		CriticNextPrompt: "A landscape: a valley., add cat",
		Attempts: []storage.CriticAttempt{
			{Image: attempt1ID, Prompt: "A landscape: a valley.", Seed: int64Ptr(42), Checks: []storage.CriticCheck{{Op: "vlm_check", Spec: "must have cat", Status: "fail", Reason: "no cat"}}},
		},
	}
	if err := s.AppendHistory(job); err != nil {
		t.Fatal(err)
	}

	opts := testOptsAt(t, dir)
	opts.Ops.Result = func(ctx context.Context, mode, genID, sidecar string) ([]byte, error) {
		return nil, lattice.ErrResultNotFound
	}
	opts.Critic = criticEnvForTest(true)

	m := New(opts)
	j := waitForCriticDone(t, m, "restore-clear")

	// The critical assertion: both fields MUST be cleared on terminal
	if j.CriticPending != nil {
		t.Fatal("CriticPending must be nil on terminal")
	}
	if j.CriticNextPrompt != "" {
		t.Fatal("CriticNextPrompt must be empty on terminal")
	}
}

// Helper functions
func int64Ptr(v int64) *int64 { return &v }
func intPtr(v int) *int       { return &v }
