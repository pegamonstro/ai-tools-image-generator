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
	got, err := s.DeliverSet(set, "panel", dest)
	if err != nil || len(got) != 1 {
		t.Fatalf("deliver: %v %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "panel.png")); string(b) != "A" {
		t.Fatal("delivered content mismatch")
	}
	if _, err := s.DeliverSet(set, "panel", dest); err == nil {
		t.Fatal("want collision error on second deliver")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error text: %v", err)
	}
}

func TestDeliverSetNamespacesManifest(t *testing.T) {
	s := newTestStore(t)
	set := "0123456789abcdef"
	if err := s.SaveSetFile(set, "manifest.json", []byte("m")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSetFile(set, "panel.png", []byte("A")); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	got, err := s.DeliverSet(set, "ui-kit", dest)
	if err != nil || len(got) != 2 {
		t.Fatalf("deliver: %v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("bare manifest.json must not be delivered")
	}
	if b, err := os.ReadFile(filepath.Join(dest, "ui-kit.manifest.json")); err != nil || string(b) != "m" {
		t.Fatalf("namespaced manifest: %v, %q", err, b)
	}
	// The set-dir copy keeps its canonical name.
	if b, err := s.ReadSetFile(set, "manifest.json"); err != nil || string(b) != "m" {
		t.Fatalf("set-dir manifest: %v, %q", err, b)
	}
}

func TestDeliverSetTwoSetsOneDest(t *testing.T) {
	s := newTestStore(t)
	names := map[string]string{
		"0123456789abcdef": "ui-kit",
		"1234567890abcdef": "gif-load",
	}
	for id := range names {
		if err := s.SaveSetFile(id, "manifest.json", []byte("m:"+names[id])); err != nil {
			t.Fatal(err)
		}
	}
	dest := t.TempDir()
	for id, name := range names {
		got, err := s.DeliverSet(id, name, dest)
		if err != nil || len(got) != 1 {
			t.Fatalf("deliver %s: %v %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("bare manifest.json must not be delivered")
	}
	for _, name := range names {
		if b, err := os.ReadFile(filepath.Join(dest, name+".manifest.json")); err != nil || string(b) != "m:"+name {
			t.Fatalf("manifest %s: %q %v", name, b, err)
		}
	}
}

func TestDeliverSetRejectsBadName(t *testing.T) {
	s := newTestStore(t)
	set := "0123456789abcdef"
	if err := s.SaveSetFile(set, "manifest.json", []byte("m")); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	for _, bad := range []string{"", "../evil", "a/b", ".."} {
		if _, err := s.DeliverSet(set, bad, dest); err == nil {
			t.Fatalf("want rejection for name %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "..", "evil.manifest.json")); err == nil {
		t.Fatal("traversal name escaped dest")
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

func TestCriticCheckRoundTrip(t *testing.T) {
	s := newTestStore(t)
	j := Job{
		ID:               "0123456789abcdef",
		Status:           "done",
		CriticSpec:       `{"checks":[{"op":"wd14","spec":"general"}],"attempts":3}`,
		CriticPending:    func() *int { i := 2; return &i }(),
		CriticNextPrompt: "a cat in space",
		Critic:           &ValidateState{Verdict: "pass", Attempts: 2},
		Attempts: []CriticAttempt{
			{Image: "1111111111111111", Prompt: "first", Seed: func() *int64 { v := int64(42); return &v }(), Checks: []CriticCheck{{Op: "wd14", Spec: "general", Status: "pass", Reason: "ok"}}, Chosen: true},
			{Image: "2222222222222222", Prompt: "second", Checks: []CriticCheck{{Op: "wd14", Spec: "general", Status: "fail", Reason: "missing cat", Suggestion: "add cat"}}},
		},
	}
	if err := s.AppendHistory(j); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 job, got %d", len(got))
	}
	gotJob := got[0]
	if gotJob.CriticSpec != j.CriticSpec {
		t.Fatalf("CriticSpec mismatch: %q vs %q", gotJob.CriticSpec, j.CriticSpec)
	}
	if gotJob.CriticPending == nil || *gotJob.CriticPending != *j.CriticPending {
		t.Fatalf("CriticPending mismatch: %v vs %v", gotJob.CriticPending, j.CriticPending)
	}
	if gotJob.CriticNextPrompt != j.CriticNextPrompt {
		t.Fatalf("CriticNextPrompt mismatch: %q vs %q", gotJob.CriticNextPrompt, j.CriticNextPrompt)
	}
	if gotJob.Critic == nil || gotJob.Critic.Verdict != j.Critic.Verdict || gotJob.Critic.Attempts != j.Critic.Attempts {
		t.Fatalf("Critic mismatch: %+v vs %+v", gotJob.Critic, j.Critic)
	}
	if len(gotJob.Attempts) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(gotJob.Attempts))
	}
	if gotJob.Attempts[0].Image != "1111111111111111" || !gotJob.Attempts[0].Chosen {
		t.Fatalf("attempt 0 mismatch: %+v", gotJob.Attempts[0])
	}
	if gotJob.Attempts[1].Image != "2222222222222222" || gotJob.Attempts[1].Chosen {
		t.Fatalf("attempt 1 mismatch: %+v", gotJob.Attempts[1])
	}
	if gotJob.Attempts[0].Seed == nil || *gotJob.Attempts[0].Seed != 42 {
		t.Fatalf("attempt 0 seed mismatch: %v", gotJob.Attempts[0].Seed)
	}
	if gotJob.Attempts[0].Checks[0].Status != "pass" || gotJob.Attempts[1].Checks[0].Status != "fail" {
		t.Fatalf("checks mismatch: %+v", gotJob.Attempts)
	}
}

func TestCriticOldRecordBackwardCompat(t *testing.T) {
	s := newTestStore(t)
	// Old record without critic fields (simulated by marshaling a Job with zero critic fields)
	oldJSON := `{"id":"abcdef0123456789","genre":"landscape","prompt":"a view","size":"512x512","enhance":false,"status":"done","created_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(s.dir, "history.jsonl"), []byte(oldJSON+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 job, got %d", len(got))
	}
	j := got[0]
	if j.ID != "abcdef0123456789" || j.Prompt != "a view" {
		t.Fatalf("basic fields lost: %+v", j)
	}
	// Critic fields should be zero values
	if j.CriticSpec != "" {
		t.Fatalf("CriticSpec should be empty, got %q", j.CriticSpec)
	}
	if j.CriticPending != nil {
		t.Fatalf("CriticPending should be nil, got %v", j.CriticPending)
	}
	if j.CriticNextPrompt != "" {
		t.Fatalf("CriticNextPrompt should be empty, got %q", j.CriticNextPrompt)
	}
	if j.Critic != nil {
		t.Fatalf("Critic should be nil, got %+v", j.Critic)
	}
	if j.Attempts != nil {
		t.Fatalf("Attempts should be nil, got %+v", j.Attempts)
	}
}
