package queue

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"img-gen/internal/genres"
	"img-gen/internal/presets"
	"img-gen/internal/prompting"
	"img-gen/internal/storage"
)

func testCatalog() *genres.Catalog {
	return &genres.Catalog{
		Version: 1,
		Genres: map[string]genres.Genre{
			"landscape": {
				Label: "Landscape",
				Fields: []genres.Field{
					{Key: "setting", Label: "Setting", Type: genres.FieldText, Required: true},
				},
				PromptTemplate: "A landscape: {setting}.",
				Sizes:          []string{"512x512"},
			},
		},
		Styles: []genres.Style{
			{Key: "watercolor", Label: "Watercolor", Prompt: "watercolor painting, soft washes"},
		},
	}
}

func testOpts(t *testing.T) Options {
	t.Helper()
	s, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Genres: testCatalog(),
		Store:  s,
		Ops: ImageOps{
			Generate: func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
				return []byte("PNG"), nil, nil
			},
		},
		Chat: func(ctx context.Context, msgs []prompting.Message) (string, error) { return "enhanced", nil },
	}
}

func waitFor(t *testing.T, m *Manager, id string, want string) storage.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := m.Get(id)
		if ok && (j.Status == want) {
			return j
		}
		if ok && (j.Status == "failed") {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %q", id, want)
	return storage.Job{}
}

func TestSubmitUnknownGenre(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Genre: "nope", Fields: map[string]string{}, Size: "512x512"}); err == nil {
		t.Fatal("expected error for unknown genre")
	}
}

func TestSubmitMissingRequired(t *testing.T) {
	m := New(testOpts(t))
	_, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": ""}, Size: "512x512"})
	if err == nil {
		t.Fatal("expected error for missing required field")
	}
}

func TestSubmitBadSize(t *testing.T) {
	m := New(testOpts(t))
	_, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "999x999"})
	if err == nil {
		t.Fatal("expected error for bad size")
	}
}

func TestDirectJobCompletes(t *testing.T) {
	m := New(testOpts(t))
	id, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a valley"}, Size: "512x512"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Prompt != "A landscape: a valley." {
		t.Fatalf("prompt = %q", j.Prompt)
	}
	if j.ImagePath == "" {
		t.Fatal("image path empty")
	}
}

func TestEnhanceJobCompletes(t *testing.T) {
	m := New(testOpts(t))
	id, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a valley"}, Size: "512x512", Enhance: true})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Prompt != "enhanced" {
		t.Fatalf("prompt = %q", j.Prompt)
	}
}

func TestGenerateErrorFailsJob(t *testing.T) {
	opts := testOpts(t)
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		return nil, nil, fmt.Errorf("lattice down")
	}
	m := New(opts)
	id, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512"})
	j := waitFor(t, m, id, "failed")
	if j.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestEnhanceRefusalFallsBackToDirect(t *testing.T) {
	opts := testOpts(t)
	opts.Chat = func(ctx context.Context, msgs []prompting.Message) (string, error) {
		return "I cannot fulfill this request. I am prohibited from generating explicit content.", nil
	}
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a valley"}, Size: "512x512", Enhance: true})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Prompt != "A landscape: a valley." {
		t.Fatalf("prompt = %q, want the direct prompt (refusal must be bypassed)", j.Prompt)
	}
}

func TestEnhanceErrorFallsBackToDirect(t *testing.T) {
	opts := testOpts(t)
	opts.Chat = func(ctx context.Context, msgs []prompting.Message) (string, error) {
		return "", fmt.Errorf("chat down")
	}
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a valley"}, Size: "512x512", Enhance: true})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Prompt != "A landscape: a valley." {
		t.Fatalf("prompt = %q, want the direct prompt (enhance error must not fail the job)", j.Prompt)
	}
}

func TestSingleSlotSerializes(t *testing.T) {
	var active int32
	release := make(chan struct{})
	opts := testOpts(t)
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		n := atomic.AddInt32(&active, 1)
		if n > 1 {
			t.Errorf("two generations ran concurrently")
		}
		<-release
		atomic.AddInt32(&active, -1)
		return []byte("PNG"), nil, nil
	}
	m := New(opts)
	id1, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a"}, Size: "512x512"})
	id2, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "b"}, Size: "512x512"})

	// Give job1 a moment to start, then assert job2 is still queued.
	time.Sleep(50 * time.Millisecond)
	if j, _ := m.Get(id2); j.Status != "queued" {
		t.Fatalf("job2 status = %q, want queued while job1 runs", j.Status)
	}
	close(release)
	waitFor(t, m, id1, "done")
	waitFor(t, m, id2, "done")
}

func TestCancelStopsRunningJob(t *testing.T) {
	started := make(chan struct{})
	opts := testOpts(t)
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	opts.Ops.Cancel = func(ctx context.Context, mode string) error { return nil }
	m := New(opts)
	id, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a"}, Size: "512x512"})

	<-started
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "cancelled")
	if j.Error != "" {
		t.Fatalf("cancelled job should have empty error, got %q", j.Error)
	}
}

func TestCancelTerminalJobFails(t *testing.T) {
	m := New(testOpts(t))
	id, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a"}, Size: "512x512"})
	waitFor(t, m, id, "done")
	if err := m.Cancel(id); err == nil {
		t.Fatal("expected error cancelling a done job")
	}
}

func TestProgressEventsStream(t *testing.T) {
	release := make(chan struct{})
	opts := testOpts(t)
	opts.Ops.Generate = func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		<-release
		return []byte("PNG"), nil, nil
	}
	opts.Ops.Progress = func(ctx context.Context, mode string) (int, int, error) {
		return 7, 25, nil
	}
	m := New(opts)
	id, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a"}, Size: "512x512"})

	ch, cancel := m.Subscribe(id)
	defer cancel()

	// The 500ms poller should emit a progress event while generation blocks.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Total > 0 && ev.Step > 0 {
				goto sawProgress
			}
		case <-deadline:
			close(release)
			t.Fatal("no progress event within 2s")
		}
	}
sawProgress:
	close(release)
	waitFor(t, m, id, "done")
}

func TestSubscribeGetsInitialStatus(t *testing.T) {
	m := New(testOpts(t))
	id, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512"})
	ch, cancel := m.Subscribe(id)
	defer cancel()
	ev := <-ch
	if ev.JobID != id {
		t.Fatalf("event job id = %q", ev.JobID)
	}
	// Wait for the worker to finish writing to the store so the TempDir
	// cleanup at test end does not race the worker goroutine.
	waitFor(t, m, id, "done")
}

func TestJobLogsStreamAndReplay(t *testing.T) {
	m := New(testOpts(t))
	id, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a valley"}, Size: "512x512"})

	// Collect events until the terminal status, counting log lines.
	var logs []string
	ch, cancel := m.Subscribe(id)
	for ev := range ch {
		if ev.Log != "" {
			logs = append(logs, ev.Log)
		}
		if ev.Status == "done" || ev.Status == "failed" {
			cancel()
			break
		}
	}
	if len(logs) == 0 {
		t.Fatal("expected at least one log line during generation")
	}
	if logs[0] == "" {
		t.Fatal("first log line empty")
	}

	// A fresh subscriber must replay the same accumulated log lines.
	ch2, cancel2 := m.Subscribe(id)
	defer cancel2()
	var replayed []string
	for ev := range ch2 {
		if ev.Log != "" {
			replayed = append(replayed, ev.Log)
		}
		if ev.Status == "done" {
			break
		}
	}
	if len(replayed) < len(logs) {
		t.Fatalf("replay got %d logs, want >= %d", len(replayed), len(logs))
	}
}

func editOps() ImageOps {
	return ImageOps{
		Generate: func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
			return []byte("PNG"), nil, nil
		},
		Edit: func(ctx context.Context, prompt, size, img string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
			return []byte("PNG"), nil, nil
		},
		Inpaint: func(ctx context.Context, prompt, img, mask string) ([]byte, error) { return []byte("PNG"), nil },
		Blend: func(ctx context.Context, prompt, size string, imgs []string, ws []float64) ([]byte, error) {
			return []byte("PNG"), nil
		},
		Upscale: func(ctx context.Context, img string) ([]byte, error) { return []byte("PNG"), nil },
	}
}

func TestSubmitUnknownStyle(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512", Style: "nope"}); err == nil {
		t.Fatal("expected error for unknown style")
	}
}

func TestStylePrefixApplied(t *testing.T) {
	m := New(testOpts(t))
	id, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "a valley"}, Size: "512x512", Style: "watercolor"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Prompt != "watercolor painting, soft washes, A landscape: a valley." {
		t.Fatalf("prompt = %q", j.Prompt)
	}
	if j.Style != "watercolor" {
		t.Fatalf("style = %q", j.Style)
	}
}

func TestSubmitRecordsModelAndLoras(t *testing.T) {
	m := New(testOpts(t))
	id, err := m.Submit(SubmitRequest{
		Genre: "landscape", Fields: map[string]string{"setting": "a valley"}, Size: "512x512",
		Model: "/m/dev",
		Loras: []storage.LoraRef{{Name: "shauray/flux-uncensored-lora"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Model != "/m/dev" {
		t.Fatalf("model = %q", j.Model)
	}
	if len(j.Loras) != 1 || j.Loras[0].Name != "shauray/flux-uncensored-lora" || j.Loras[0].Scale != 1.0 {
		t.Fatalf("loras = %+v (want default scale 1.0)", j.Loras)
	}
}

func TestSubmitUnknownMode(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "nope"}); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestSubmitEditMissingImage(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "edit", Prompt: "x", Size: "512x512"}); err == nil {
		t.Fatal("expected error for edit without image")
	}
}

func TestSubmitInpaintMissingMask(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "inpaint", Prompt: "x", Image: "aW1n"}); err == nil {
		t.Fatal("expected error for inpaint without mask")
	}
}

func TestSubmitBlendNoImages(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "blend", Prompt: "x", Size: "512x512"}); err == nil {
		t.Fatal("expected error for blend without reference images")
	}
}

func TestEditJobCompletes(t *testing.T) {
	opts := testOpts(t)
	opts.Ops = editOps()
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Mode: "edit", Prompt: "make it snow", Size: "512x512", Image: "aW1n"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Mode != "edit" || j.Prompt != "make it snow" {
		t.Fatalf("mode=%q prompt=%q", j.Mode, j.Prompt)
	}
}

func TestEditJobDispatchCallsEdit(t *testing.T) {
	var called bool
	var gotStrength float64
	opts := testOpts(t)
	opts.Ops = editOps()
	opts.Ops.Edit = func(ctx context.Context, prompt, size, img string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		called = true
		gotStrength = strength
		return []byte("PNG"), nil, nil
	}
	m := New(opts)
	id, _ := m.Submit(SubmitRequest{Mode: "edit", Prompt: "p", Size: "512x512", Image: "aW1n"})
	waitFor(t, m, id, "done")
	if !called {
		t.Fatal("Edit op was not called")
	}
	if gotStrength != 0.4 {
		t.Fatalf("default strength = %v, want 0.4", gotStrength)
	}
}

func TestInpaintJobCompletes(t *testing.T) {
	opts := testOpts(t)
	opts.Ops = editOps()
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Mode: "inpaint", Prompt: "add a cat", Image: "aW1n", Mask: "bWFzaw=="})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Mode != "inpaint" {
		t.Fatalf("mode = %q", j.Mode)
	}
}

func TestOutpaintJobCompletes(t *testing.T) {
	opts := testOpts(t)
	opts.Ops = editOps()
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Mode: "outpaint", Prompt: "extend the scene", Image: "aW1n", Mask: "bWFzaw=="})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Mode != "outpaint" {
		t.Fatalf("mode = %q", j.Mode)
	}
}

func TestSubmitOutpaintMissingMask(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "outpaint", Prompt: "x", Image: "aW1n"}); err == nil {
		t.Fatal("expected error for outpaint without mask")
	}
}

func TestUpscaleJobCompletes(t *testing.T) {
	opts := testOpts(t)
	opts.Ops = editOps()
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Mode: "upscale", Image: "aW1n"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Mode != "upscale" {
		t.Fatalf("mode = %q", j.Mode)
	}
}

func TestSubmitUpscaleMissingImage(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "upscale"}); err == nil {
		t.Fatal("expected error for upscale without image")
	}
}

func TestSubmitPoseMissingImage(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "pose", Prompt: "a woman", Size: "512x512"}); err == nil {
		t.Fatal("expected error for pose without a reference image")
	}
}

func TestSubmitPoseMissingPrompt(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "pose", Image: "aW1n", Size: "512x512"}); err == nil {
		t.Fatal("expected error for pose without a prompt")
	}
}

func TestPoseJobCompletes(t *testing.T) {
	var called bool
	var gotStrength float64
	opts := testOpts(t)
	opts.Ops = editOps()
	opts.Ops.Controlnet = func(ctx context.Context, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		called = true
		gotStrength = strength
		return []byte("PNG"), nil, nil
	}
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Mode: "pose", Prompt: "a woman", Image: "aW1n", Size: "512x512"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Mode != "pose" {
		t.Fatalf("mode = %q", j.Mode)
	}
	if !called {
		t.Fatal("Controlnet op was not called")
	}
	if gotStrength != 0.7 {
		t.Fatalf("default strength = %v, want 0.7", gotStrength)
	}
}

func TestSubmitResolvesPresetDefaults(t *testing.T) {
	pc := &presets.Catalog{Presets: []presets.Preset{{
		Key:            "aria",
		Trigger:        "aria, silver hair",
		Model:          "/m/persephone",
		Loras:          []storage.LoraRef{{Name: "alvdansen/illustration-1.0-flux-dev", Scale: 0.8}},
		NegativePrompt: "blurry",
	}}}
	m := New(testOpts(t))
	m.opts.Presets = pc

	id, err := m.Submit(SubmitRequest{
		Genre: "landscape", Fields: map[string]string{"setting": "a valley"},
		Size: "512x512", Preset: "aria",
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Preset != "aria" {
		t.Fatalf("preset not recorded: %q", j.Preset)
	}
	if j.Model != "/m/persephone" {
		t.Fatalf("model default not applied: %q", j.Model)
	}
	if len(j.Loras) != 1 || j.Loras[0].Name != "alvdansen/illustration-1.0-flux-dev" || j.Loras[0].Scale != 0.8 {
		t.Fatalf("lora defaults not applied: %+v", j.Loras)
	}
	if j.NegativePrompt != "blurry" {
		t.Fatalf("negative default not applied: %q", j.NegativePrompt)
	}
}

func TestSubmitExplicitFieldsOverridePreset(t *testing.T) {
	pc := &presets.Catalog{Presets: []presets.Preset{{
		Key:            "aria",
		Model:          "/m/persephone",
		Loras:          []storage.LoraRef{{Name: "preset/lora", Scale: 0.8}},
		NegativePrompt: "blurry",
	}}}
	m := New(testOpts(t))
	m.opts.Presets = pc

	id, err := m.Submit(SubmitRequest{
		Genre: "landscape", Fields: map[string]string{"setting": "a valley"},
		Size: "512x512", Preset: "aria",
		Model: "/m/explicit", Loras: []storage.LoraRef{{Name: "explicit/lora", Scale: 0.5}},
		NegativePrompt: "sharp",
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Model != "/m/explicit" || j.NegativePrompt != "sharp" {
		t.Fatalf("explicit fields must win: %+v", j)
	}
	if len(j.Loras) != 1 || j.Loras[0].Name != "explicit/lora" {
		t.Fatalf("explicit loras must win: %+v", j.Loras)
	}
}

func TestSubmitBatchFansOut(t *testing.T) {
	m := New(testOpts(t))
	seed := int64(42)
	ids, err := m.SubmitBatch(SubmitRequest{
		Genre: "landscape", Fields: map[string]string{"setting": "a valley"},
		Size: "512x512", Batch: 3, Seed: &seed,
	})
	if err != nil || len(ids) != 3 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatal("batch ids must be distinct")
	}
	j0, _ := m.Get(ids[0])
	j1, _ := m.Get(ids[1])
	if j0.BatchID == "" || j0.BatchID != j1.BatchID {
		t.Fatalf("batch ids must match: %q vs %q", j0.BatchID, j1.BatchID)
	}
	if j1.Seed == nil || *j1.Seed != 43 {
		t.Fatalf("seed should vary across batch: %v", j1.Seed)
	}
	// A single submit leaves BatchID empty.
	id, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512"})
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := m.Get(id); j.BatchID != "" {
		t.Fatalf("single submit must have empty batch id, got %q", j.BatchID)
	}
	// Drain the workers so the TempDir cleanup at test end doesn't race.
	for _, bid := range ids {
		waitFor(t, m, bid, "done")
	}
	waitFor(t, m, id, "done")
}

func TestSubmitBatchCapsAtEight(t *testing.T) {
	m := New(testOpts(t))
	ids, err := m.SubmitBatch(SubmitRequest{
		Genre: "landscape", Fields: map[string]string{"setting": "a valley"},
		Size: "512x512", Batch: 20,
	})
	if err != nil || len(ids) != 8 {
		t.Fatalf("want capped to 8, got %d (err=%v)", len(ids), err)
	}
	for _, id := range ids {
		waitFor(t, m, id, "done")
	}
}

func TestSubmitRejectsUnknownPreset(t *testing.T) {
	m := New(testOpts(t))
	m.opts.Presets = &presets.Catalog{}
	if _, err := m.Submit(SubmitRequest{
		Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512", Preset: "nope",
	}); err == nil {
		t.Fatal("expected error for unknown preset")
	}
}

func TestPresetTriggerPrefixApplied(t *testing.T) {
	pc := &presets.Catalog{Presets: []presets.Preset{{
		Key: "aria", Trigger: "aria, silver hair",
	}}}
	m := New(testOpts(t))
	m.opts.Presets = pc

	id, err := m.Submit(SubmitRequest{
		Genre: "landscape", Fields: map[string]string{"setting": "a valley"},
		Size: "512x512", Preset: "aria",
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Prompt == "" || !strings.HasPrefix(j.Prompt, "aria, silver hair") {
		t.Fatalf("trigger not prepended: %q", j.Prompt)
	}
}
