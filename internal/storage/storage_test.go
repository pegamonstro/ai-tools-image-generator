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

func TestDeleteHistoryRemovesOnlyThatID(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Each job persists twice (submit + terminal lines); deletion must drop
	// both lines of the deleted job and keep its neighbour's.
	a, aDone := Job{ID: "a", Prompt: "first", Status: "queued"}, Job{ID: "a", Prompt: "first", Status: "done"}
	b, bDone := Job{ID: "b", Prompt: "second", Status: "queued"}, Job{ID: "b", Prompt: "second", Status: "done"}
	for _, j := range []Job{a, aDone, b, bDone} {
		if err := s.AppendHistory(j); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteHistory("a"); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("want only b after deleting a, got %+v", got)
	}
}

func TestDeleteHistoryUnknownIDIsANoOp(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHistory(Job{ID: "b", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHistory("missing"); err != nil {
		t.Fatalf("want no-op success for unknown id, got %v", err)
	}
	got, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("b must survive, got %+v", got)
	}
}

func TestDeleteHistoryMissingFileIsANoOp(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHistory("a"); err != nil {
		t.Fatalf("want no-op success for missing file, got %v", err)
	}
}

func TestRemoveImageDeletesFile(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef"
	if _, err := s.SaveImage(id, []byte("PNG")); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveImage(id); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "images", id+".png")); !os.IsNotExist(err) {
		t.Fatalf("want the PNG gone, got %v", err)
	}
}

func TestRemoveImageMissingFileSucceeds(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveImage("0123456789abcdef"); err != nil {
		t.Fatalf("want already-gone to be success, got %v", err)
	}
}

func TestRemoveImageRejectsBadID(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveImage("../../etc/passwd"); err == nil {
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

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSetFilesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveSetFile("0123456789abcdef", "panel.png", []byte("PNGDATA")); err != nil {
		t.Fatal(err)
	}
	b, err := s.ReadSetFile("0123456789abcdef", "panel.png")
	if err != nil || string(b) != "PNGDATA" {
		t.Fatalf("read back: %q %v", b, err)
	}
	names, err := s.ListSetFiles("0123456789abcdef")
	if err != nil || len(names) != 1 || names[0] != "panel.png" {
		t.Fatalf("list: %v %v", names, err)
	}
}

func TestSetFileTraversalRejected(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{
		"../evil.png", "sub/dir/x.png", "/abs.png", ".hidden", "", "x/y", "a..b",
	} {
		if err := s.SaveSetFile("0123456789abcdef", bad, []byte("x")); err == nil {
			t.Fatalf("want rejection for %q", bad)
		}
		if _, err := s.ReadSetFile("0123456789abcdef", bad); err == nil {
			t.Fatalf("read: want rejection for %q", bad)
		}
	}
}

func TestSetIDValidated(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveSetFile("../../etc", "x.png", []byte("x")); err == nil {
		t.Fatal("want rejection for non-hex set id")
	}
	if _, err := s.ReadSetFile("0123456789abcdeg", "x.png"); err == nil { // 'g' is not hex
		t.Fatal("want rejection for non-hex set id on read")
	}
}

func TestReadDataFileGuards(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "sub", "f.png"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := s.ReadDataFile("sub/f.png")
	if err != nil || string(b) != "ok" {
		t.Fatalf("rel read: %q %v", b, err)
	}
	for _, bad := range []string{"../f.png", "/etc/passwd", "sub/../sub/f.png"} {
		if _, err := s.ReadDataFile(bad); err == nil {
			t.Fatalf("want rejection for %q", bad)
		}
	}
}

func TestDeliverSetCollision(t *testing.T) {
	s := newTestStore(t)
	set := "0123456789abcdef"
	if err := s.SaveSetFile(set, "panel.png", []byte("A")); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	got, err := s.DeliverSet(set, dest)
	if err != nil || len(got) != 1 {
		t.Fatalf("deliver: %v %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "panel.png")); string(b) != "A" {
		t.Fatal("delivered content mismatch")
	}
	if _, err := s.DeliverSet(set, dest); err == nil {
		t.Fatal("want collision error on second deliver")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error text: %v", err)
	}
}

func TestReadImageAndRemoveSet(t *testing.T) {
	s := newTestStore(t)
	png := []byte("PNG")
	if _, err := s.SaveImage("0123456789abcdef", png); err != nil {
		t.Fatal(err)
	}
	b, err := s.ReadImage("0123456789abcdef")
	if err != nil || string(b) != "PNG" {
		t.Fatalf("ReadImage: %q %v", b, err)
	}
	if _, err := s.ReadImage("zz"); err == nil {
		t.Fatal("want rejection for invalid image id")
	}
	if err := s.RemoveSet("0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSetFile("0123456789abcdef", "panel.png"); err == nil {
		t.Fatal("set dir should be gone")
	}
}
