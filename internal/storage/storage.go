package storage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	Model string    // mflux --model value; "" = sidecar default
	Loras []LoraRef // mflux --lora list; nil = none
}

// SamplingParams are optional generation knobs forwarded to the mflux sidecar.
// A nil pointer means "leave to the sidecar default".
type SamplingParams struct {
	Seed           *int64   `json:"seed,omitempty"`            // fixed seed; nil = random
	Steps          *int     `json:"steps,omitempty"`           // diffusion steps; nil = sidecar default
	Guidance       *float64 `json:"guidance,omitempty"`        // classifier-free guidance; nil = sidecar default
	NegativePrompt string   `json:"negative_prompt,omitempty"` // things to avoid; "" = none
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
	Loras          []LoraRef         `json:"loras,omitempty"`
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
