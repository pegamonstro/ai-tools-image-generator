package storage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// LoraRef is one LoRA in mflux terms: a --lora value (HF id or local path)
// plus its scale.
type LoraRef struct {
	Name  string  `json:"name"`
	Scale float64 `json:"scale"`
}

// ModelSpec is the per-request model + LoRA selection forwarded to the sidecar.
type ModelSpec struct {
	Model   string    // model value (checkpoint path / HF id); "" = sidecar default
	Loras   []LoraRef // lora value list; nil = none
	Sidecar string    // engine selection ("sdxl" = stable-diffusion.cpp sidecar; "" = default mflux sidecar)
	// LatticeModel is the submitted catalog key, forwarded as the lattice
	// model name when generation runs through the frontend (the lattice
	// routes by registry name, not by file path). Empty = fall back to Model.
	LatticeModel string
}

// SamplingParams are optional generation knobs forwarded to the mflux sidecar.
// A nil pointer means "leave to the sidecar default".
type SamplingParams struct {
	Seed           *int64   `json:"seed,omitempty"`            // fixed seed; nil = random
	Steps          *int     `json:"steps,omitempty"`           // diffusion steps; nil = sidecar default
	Guidance       *float64 `json:"guidance,omitempty"`        // classifier-free guidance; nil = sidecar default
	NegativePrompt string   `json:"negative_prompt,omitempty"` // things to avoid; "" = none
}

// FileRef is one named deliverable inside a job's set directory.
type FileRef struct {
	Name    string `json:"name"`
	Primary bool   `json:"primary,omitempty"`
}

type Job struct {
	ID             string            `json:"id"`
	Genre          string            `json:"genre"`
	Mode           string            `json:"mode,omitempty"`
	Style          string            `json:"style,omitempty"`
	Preset         string            `json:"preset,omitempty"` // character-preset key; "" = none
	BatchID        string            `json:"batch_id,omitempty"`
	Prompt         string            `json:"prompt"`
	Fields         map[string]string `json:"fields"`
	Size           string            `json:"size"`
	Enhance        bool              `json:"enhance"`
	Model          string            `json:"model,omitempty"`
	ModelKey       string            `json:"model_key,omitempty"` // submitted catalog key; the lattice routes by this name
	Loras          []LoraRef         `json:"loras,omitempty"`
	Sidecar        string            `json:"sidecar,omitempty"`         // engine selection ("sdxl"); lets a restart re-attach on the right sidecar
	Seed           *int64            `json:"seed,omitempty"`            // seed actually used (sidecar response); nil for inpaint/blend
	Steps          *int              `json:"steps,omitempty"`           // requested diffusion steps (nil = default)
	Guidance       *float64          `json:"guidance,omitempty"`        // requested guidance (nil = default)
	NegativePrompt string            `json:"negative_prompt,omitempty"` // requested negative prompt ("" = none)
	Status         string            `json:"status"`
	Step           int               `json:"step,omitempty"`  // in-flight diffusion step; zeroed at finish (transient)
	Total          int               `json:"total,omitempty"` // total diffusion steps; zeroed at finish (transient)
	CreatedAt      time.Time         `json:"created_at"`
	FinishedAt     *time.Time        `json:"finished_at,omitempty"`
	Error          string            `json:"error,omitempty"`
	ImagePath      string            `json:"image_path,omitempty"`
	GenID          string            `json:"gen_id,omitempty"` // sidecar-side generation id; lets a restart re-attach to a persisted result
	// --- compose (asset-set / gif) bookkeeping ---
	// SetID identifies the DATA_DIR/sets/<SetID>/ directory holding a parent's
	// deliverables. On compose parents SetID == ID (one id, one set).
	SetID string `json:"set_id,omitempty"`
	// ComposeParent marks a generation child; children re-enqueue their
	// parent when the last of them turns terminal.
	ComposeParent string `json:"compose_parent,omitempty"`
	// Children lists the child job ids of a compose parent (informational —
	// ordering is restored by scanning children's ComposeParent).
	Children []string `json:"children,omitempty"`
	// Files lists the named deliverables under SetDir (queue-set at compose
	// completion).
	Files []FileRef `json:"files,omitempty"`
	// ComposeSpec is the parent's request as JSON, so a restart can recompose.
	ComposeSpec string `json:"compose_spec,omitempty"`
}

type Store struct {
	dir string
}

// New creates the data directory and its images/ subdirectory.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

var idRe = regexp.MustCompile(`^[a-f0-9]{16}$`)

// ValidImageID reports whether id is a well-formed stored-image id (the same
// 16-hex shape /api/images/ accepts).
func ValidImageID(id string) bool { return idRe.MatchString(id) }

// CopyImage copies a stored image to destDir, returning the destination path.
// id is validated so only our own images/*.png are ever read.
func (s *Store) CopyImage(id, destDir string) (string, error) {
	if !ValidImageID(id) {
		return "", fmt.Errorf("invalid image id %q", id)
	}
	src := filepath.Join(s.dir, "images", id+".png")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(destDir, id+".png")
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// SaveImage writes the PNG once and returns its relative path under dir.
func (s *Store) SaveImage(id string, png []byte) (string, error) {
	rel := filepath.Join("images", id+".png")
	if err := os.WriteFile(filepath.Join(s.dir, rel), png, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

// AppendHistory appends one JSON line to history.jsonl.
func (s *Store) AppendHistory(j Job) error {
	f, err := os.OpenFile(filepath.Join(s.dir, "history.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// DeleteHistory rewrites history.jsonl without any line belonging to id — the
// inverse of AppendHistory, which can only append. Kept lines are preserved
// byte-for-byte; a missing file or an id with no matching lines is a no-op.
func (s *Store) DeleteHistory(id string) error {
	path := filepath.Join(s.dir, "history.jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var kept []byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var j Job
		if err := json.Unmarshal(sc.Bytes(), &j); err != nil {
			return err
		}
		if j.ID == id {
			continue
		}
		kept = append(kept, sc.Bytes()...)
		kept = append(kept, '\n')
	}
	if err := sc.Err(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, kept, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveImage deletes the stored PNG for id, mirroring SaveImage. A file that
// is already gone is success, so a retried delete converges.
func (s *Store) RemoveImage(id string) error {
	if !ValidImageID(id) {
		return fmt.Errorf("invalid image id %q", id)
	}
	err := os.Remove(filepath.Join(s.dir, "images", id+".png"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// LoadHistory reads history.jsonl (nil if the file does not exist yet). Jobs
// may be appended more than once (per persisted transition, e.g. queued at
// submit-time and terminal at finish); records are deduped by id, last record
// wins, and the result keeps first-appearance (submit-time) order.
func (s *Store) LoadHistory() ([]Job, error) {
	f, err := os.Open(filepath.Join(s.dir, "history.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var jobs []Job
	slot := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var j Job
		if err := json.Unmarshal(sc.Bytes(), &j); err != nil {
			return nil, err
		}
		if i, ok := slot[j.ID]; ok {
			jobs[i] = j
		} else {
			slot[j.ID] = len(jobs)
			jobs = append(jobs, j)
		}
	}
	return jobs, sc.Err()
}

// --- set directories (compose deliverables) ---

// SetDir is a compose parent's deliverable directory. SetID is always a job
// id, so it reuses ValidImageID's 16-hex shape as the traversal guard.
func (s *Store) SetDir(setID string) string {
	return filepath.Join(s.dir, "sets", setID)
}

var setFileRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// validSetFile enforces the set-file name rule: no separators, no leading
// dot, and no ".." anywhere, so traversal is structurally impossible.
func validSetFile(name string) bool {
	return setFileRe.MatchString(name) && !strings.Contains(name, "..")
}

func validSetID(setID string) bool { return ValidImageID(setID) }

// SaveSetFile writes one deliverable into the set dir, creating the dir and
// overwriting freely — a re-run after an interrupted compose must land on the
// same paths. Collision refusal applies to delivery (DeliverSet), not here.
func (s *Store) SaveSetFile(setID, name string, data []byte) error {
	if !validSetID(setID) {
		return fmt.Errorf("invalid set id %q", setID)
	}
	if !validSetFile(name) {
		return fmt.Errorf("invalid set file name %q", name)
	}
	if err := os.MkdirAll(s.SetDir(setID), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.SetDir(setID), name), data, 0o644)
}

// ReadSetFile reads a deliverable back. The name guard makes the read safe
// even though callers pass HTTP path segments.
func (s *Store) ReadSetFile(setID, name string) ([]byte, error) {
	if !validSetID(setID) {
		return nil, fmt.Errorf("invalid set id %q", setID)
	}
	if !validSetFile(name) {
		return nil, fmt.Errorf("invalid set file name %q", name)
	}
	return os.ReadFile(filepath.Join(s.SetDir(setID), name))
}

// ListSetFiles returns the set's file names, sorted.
func (s *Store) ListSetFiles(setID string) ([]string, error) {
	if !validSetID(setID) {
		return nil, fmt.Errorf("invalid set id %q", setID)
	}
	entries, err := os.ReadDir(s.SetDir(setID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// RemoveSet deletes the whole set directory. A missing dir is success so a
// retried delete converges (mirrors RemoveImage).
func (s *Store) RemoveSet(setID string) error {
	if !validSetID(setID) {
		return fmt.Errorf("invalid set id %q", setID)
	}
	if err := os.RemoveAll(s.SetDir(setID)); err != nil {
		return err
	}
	return nil
}

// DeliverSet copies every set file into dest under its own name, renaming
// the set's manifest.json to "<setName>.manifest.json" so two sets delivered
// into one dest do not collide on a fixed-name file. Dest is created when it
// does not exist. A file that already exists at the destination is a
// collision error: delivered names are semantic ("panel.png",
// "tilekit-cross.png") and silently overwriting a project file is worse than
// making the caller pick another dest. Returns the written destination paths.
//
// Traversal and symlink safety: set-file names match setFileRe (no
// separators, no ".." segment), the constructed manifest name is re-checked
// against the same rule (setName arrives from callers unvalidated), and an
// existing entry — including a planted symlink pointing anywhere — is
// refused before any write, never followed.
func (s *Store) DeliverSet(setID, setName, destDir string) ([]string, error) {
	names, err := s.ListSetFiles(setID)
	if err != nil {
		return nil, err
	}
	manifestFile := setName + ".manifest.json"
	if !validSetFile(manifestFile) {
		return nil, fmt.Errorf("invalid set name %q for manifest delivery", setName)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		b, err := s.ReadSetFile(setID, name)
		if err != nil {
			return nil, err
		}
		delivered := name
		if name == "manifest.json" {
			delivered = manifestFile
		}
		dst := filepath.Join(destDir, delivered)
		if _, err := os.Lstat(dst); err == nil {
			return out, fmt.Errorf("destination file %s already exists", dst)
		} else if !os.IsNotExist(err) {
			return out, err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return out, err
		}
		out = append(out, dst)
	}
	return out, nil
}

// ReadImage reads a stored generation result back by id.
func (s *Store) ReadImage(id string) ([]byte, error) {
	if !ValidImageID(id) {
		return nil, fmt.Errorf("invalid image id %q", id)
	}
	return os.ReadFile(filepath.Join(s.dir, "images", id+".png"))
}

// ReadDataFile reads a DATA_DIR-relative file. Clean relative paths only:
// absolute paths and any ".." are rejected, so a GIF's path frame source can
// only reference data the service already owns.
func (s *Store) ReadDataFile(rel string) ([]byte, error) {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		return nil, fmt.Errorf("invalid data path %q", rel)
	}
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") {
		return nil, fmt.Errorf("invalid data path %q", rel)
	}
	return os.ReadFile(filepath.Join(s.dir, clean))
}
