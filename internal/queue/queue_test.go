package queue

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"img-gen/internal/genres"
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
	}
}

func testOpts(t *testing.T) Options {
	t.Helper()
	s, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Genres:   testCatalog(),
		Store:    s,
		Generate: func(ctx context.Context, prompt, size string) ([]byte, error) { return []byte("PNG"), nil },
		Chat:     func(ctx context.Context, msgs []prompting.Message) (string, error) { return "enhanced", nil },
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
	opts.Generate = func(ctx context.Context, prompt, size string) ([]byte, error) {
		return nil, fmt.Errorf("lattice down")
	}
	m := New(opts)
	id, _ := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512"})
	j := waitFor(t, m, id, "failed")
	if j.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestSingleSlotSerializes(t *testing.T) {
	var active int32
	release := make(chan struct{})
	opts := testOpts(t)
	opts.Generate = func(ctx context.Context, prompt, size string) ([]byte, error) {
		n := atomic.AddInt32(&active, 1)
		if n > 1 {
			t.Errorf("two generations ran concurrently")
		}
		<-release
		atomic.AddInt32(&active, -1)
		return []byte("PNG"), nil
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
