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
func makeTestJob(t *testing.T, script []fakeRenderResult) *Manager {
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
	m.fakeCalls = &calls
	return m
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

// makeChecklist creates a checklist with a single vlm_check that passes or fails.
func makeChecklist(pass bool) critic.Checklist {
	spec := "test criterion"
	if pass {
		return critic.Checklist{
			Checks: []critic.Check{{Op: "vlm_check", Spec: spec}},
			Budget: critic.BudgetDefault,
			OnFail: "deliver",
		}
	}
	return critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: spec}},
		Budget: critic.BudgetDefault,
		OnFail: "deliver",
	}
}

// makeChecklistWithBudget creates a checklist with specified budget and on_fail.
func makeChecklistWithBudget(budget int, onFail string) critic.Checklist {
	return critic.Checklist{
		Checks: []critic.Check{{Op: "vlm_check", Spec: "test criterion"}},
		Budget: budget,
		OnFail: onFail,
	}
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
	cl := makeChecklist(true)
	_ = cl // used for documentation

	// Script: first (and only) render returns a valid PNG
	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m := makeTestJob(t, script)
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
	if len(*m.fakeCalls) != 1 {
		t.Fatalf("renderer called %d times, want 1", len(*m.fakeCalls))
	}
	if (*m.fakeCalls)[0].Prompt != "A landscape: a valley." {
		t.Fatalf("prompt = %q, want base prompt", (*m.fakeCalls)[0].Prompt)
	}
	if (*m.fakeCalls)[0].Seed == nil || *(*m.fakeCalls)[0].Seed != 42 {
		t.Fatalf("seed = %v, want 42", (*m.fakeCalls)[0].Seed)
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
	cl := makeFailingChecklist() // Budget=2, OnFail=deliver
	_ = cl                       // used for documentation

	// Script: 3 renders - attempt 1 fails (judge returns fail), attempt 2 fails, attempt 3 passes
	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)}, // attempt 1
		{PNG: tinyPNG(), Seed: int64Ptr(100)}, // attempt 2 (same seed)
		{PNG: tinyPNG(), Seed: int64Ptr(101)}, // attempt 3 (seed+1)
	}

	m := makeTestJob(t, script)
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
	if len(*m.fakeCalls) != 3 {
		t.Fatalf("renderer called %d times, want 3", len(*m.fakeCalls))
	}

	// Verify prompts: [base, mutation, mutation]
	basePrompt := "A landscape: a valley."
	expectedMutation := basePrompt + ", add cat" // deduped from "add cat"
	if (*m.fakeCalls)[0].Prompt != basePrompt {
		t.Fatalf("attempt 1 prompt = %q, want %q", (*m.fakeCalls)[0].Prompt, basePrompt)
	}
	if (*m.fakeCalls)[1].Prompt != expectedMutation {
		t.Fatalf("attempt 2 prompt = %q, want %q", (*m.fakeCalls)[1].Prompt, expectedMutation)
	}
	if (*m.fakeCalls)[2].Prompt != expectedMutation {
		t.Fatalf("attempt 3 prompt = %q, want %q", (*m.fakeCalls)[2].Prompt, expectedMutation)
	}

	// Verify seeds: [100, 100, 101]
	if (*m.fakeCalls)[0].Seed == nil || *(*m.fakeCalls)[0].Seed != 100 {
		t.Fatalf("attempt 1 seed = %v, want 100", (*m.fakeCalls)[0].Seed)
	}
	if (*m.fakeCalls)[1].Seed == nil || *(*m.fakeCalls)[1].Seed != 100 {
		t.Fatalf("attempt 2 seed = %v, want 100 (same seed)", (*m.fakeCalls)[1].Seed)
	}
	if (*m.fakeCalls)[2].Seed == nil || *(*m.fakeCalls)[2].Seed != 101 {
		t.Fatalf("attempt 3 seed = %v, want 101 (seed+1)", (*m.fakeCalls)[2].Seed)
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
	cl := makeZeroBudgetChecklist(false) // Budget=0, OnFail=deliver
	_ = cl                               // used for documentation

	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m := makeTestJob(t, script)
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
	if len(*m.fakeCalls) != 1 {
		t.Fatalf("renderer called %d times, want 1", len(*m.fakeCalls))
	}
}

func TestCriticLoopBudgetZeroFailOnFailFail(t *testing.T) {
	cl := makeOnFailFailChecklist() // Budget=0, OnFail=fail
	_ = cl                          // used for documentation

	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m := makeTestJob(t, script)
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
	if len(*m.fakeCalls) != 1 {
		t.Fatalf("renderer called %d times, want 1", len(*m.fakeCalls))
	}
}

func TestCriticLoopAllIndeterminate(t *testing.T) {
	cl := makeIndeterminateChecklist()
	_ = cl // used for documentation

	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m := makeTestJob(t, script)
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
	if len(*m.fakeCalls) != 1 {
		t.Fatalf("renderer called %d times, want 1", len(*m.fakeCalls))
	}
}

func TestCriticLoopCancelMidValidating(t *testing.T) {
	cl := makeFailingChecklist() // Budget=2
	_ = cl                       // used for documentation

	// Use a channel to block the Judge during validation
	judgeBlock := make(chan struct{})
	judgeUnblocked := make(chan struct{})

	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	}

	m := makeTestJob(t, script)
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
	cl := makeFailingChecklist() // Budget=2
	_ = cl                       // used for documentation

	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	}

	m := makeTestJob(t, script)
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
	cl := makeFailingChecklist()
	_ = cl // used for documentation

	script := []fakeRenderResult{
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(100)},
		{PNG: tinyPNG(), Seed: int64Ptr(101)},
	}

	m := makeTestJob(t, script)
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
	cl := makeChecklist(true)
	_ = cl // used for documentation

	script := []fakeRenderResult{{PNG: tinyPNG(), Seed: int64Ptr(42)}}

	m := makeTestJob(t, script)
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
	cl := critic.Checklist{
		Checks: []critic.Check{
			{Op: "vlm_check", Spec: "must have cat"},
			{Op: "vlm_check", Spec: "must have dog"},
		},
		Budget: 2,
		OnFail: "deliver",
	}
	_ = cl // used for documentation

	// First attempt fails both, second passes first but fails second, third passes
	callCount := 0
	m := makeTestJob(t, []fakeRenderResult{
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

	if len(*m.fakeCalls) != 3 {
		t.Fatalf("renderer called %d times, want 3", len(*m.fakeCalls))
	}
	if (*m.fakeCalls)[0].Prompt != basePrompt {
		t.Fatalf("attempt 1 prompt = %q, want %q", (*m.fakeCalls)[0].Prompt, basePrompt)
	}
	if (*m.fakeCalls)[1].Prompt != expectedMutation1 {
		t.Fatalf("attempt 2 prompt = %q, want %q", (*m.fakeCalls)[1].Prompt, expectedMutation1)
	}
	if (*m.fakeCalls)[2].Prompt != expectedMutation1 {
		t.Fatalf("attempt 3 prompt = %q, want %q", (*m.fakeCalls)[2].Prompt, expectedMutation1)
	}
}

// Helper functions
func int64Ptr(v int64) *int64 { return &v }
func intPtr(v int) *int       { return &v }
