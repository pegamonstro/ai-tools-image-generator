package storage

import (
	"os"
	"path/filepath"
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
	if err := s.AppendHistory(j); err != nil {
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
