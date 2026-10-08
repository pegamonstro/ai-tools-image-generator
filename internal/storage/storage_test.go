package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveImage(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel, err := s.SaveImage("abc", []byte("PNG"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != filepath.Join("images", "abc.png") {
		t.Fatalf("rel = %q", rel)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "PNG" {
		t.Fatalf("got %q", b)
	}
}

func TestAppendAndLoadHistory(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "1", Genre: "landscape", Status: "done", CreatedAt: time.Now()}
	if err := s.AppendHistory(j); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHistory(Job{ID: "2", Genre: "landscape", Status: "done", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
}

func TestLoadHistoryDedupsByIDLastWins(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := Job{ID: "a", Prompt: "first", Status: "queued"}
	aDone := Job{ID: "a", Prompt: "first", Status: "done"}
	b := Job{ID: "b", Prompt: "second", Status: "done"}
	for _, j := range []Job{a, aDone, b} {
		if err := s.AppendHistory(j); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 deduped records, got %d: %+v", len(got), got)
	}
	if got[0].ID != "a" || got[0].Status != "done" {
		t.Fatalf("want a at first position with last content, got %+v", got[0])
	}
	if got[1].ID != "b" {
		t.Fatalf("want b second, got %+v", got[1])
	}
}

func TestLoadHistoryMalformedLineErrors(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "history.jsonl"), []byte("{not json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadHistory(); err == nil {
		t.Fatal("want error for malformed line")
	}
}

func TestLoadHistoryMissingFile(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("want nil for missing file, got %v", got)
	}
}

func TestJobModeRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = s.AppendHistory(Job{ID: "abc", Mode: "edit", Prompt: "make it snow", Status: "done"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Mode != "edit" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestJobSidecarRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHistory(Job{ID: "sdxljob", Mode: "generate", Sidecar: "sdxl", Status: "generating"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Sidecar != "sdxl" {
		t.Fatalf("sidecar not preserved: %+v", jobs)
	}
	raw, err := json.Marshal(Job{ID: "x", Sidecar: "sdxl", Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sidecar":"sdxl"`) {
		t.Fatalf("sidecar key missing from job JSON: %s", raw)
	}
}

func TestCopyImage(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef" // 16 hex chars
	if _, err := s.SaveImage(id, []byte{0x89, 'P', 'N', 'G'}); err != nil {
		t.Fatal(err)
	}
	dst, err := s.CopyImage(id, filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatalf("CopyImage: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("copied file missing: %v", err)
	}
}

func TestCopyImageRejectsBadID(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyImage("../../etc/passwd", t.TempDir()); err == nil {
		t.Fatal("want error for invalid id")
	}
}

func TestJobProgressJSON(t *testing.T) {
	inFlight, _ := json.Marshal(Job{ID: "1", Status: "generating", Step: 2, Total: 8})
	var in map[string]any
	if err := json.Unmarshal(inFlight, &in); err != nil {
		t.Fatal(err)
	}
	if in["step"] != float64(2) || in["total"] != float64(8) {
		t.Fatalf("in-flight job JSON = %s", inFlight)
	}

	terminal, _ := json.Marshal(Job{ID: "1", Status: "done"})
	var term map[string]any
	if err := json.Unmarshal(terminal, &term); err != nil {
		t.Fatal(err)
	}
	if _, ok := term["step"]; ok {
		t.Fatalf("terminal job JSON has step: %s", terminal)
	}
	if _, ok := term["total"]; ok {
		t.Fatalf("terminal job JSON has total: %s", terminal)
	}
}
