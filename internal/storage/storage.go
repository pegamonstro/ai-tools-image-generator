package storage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type Job struct {
	ID         string            `json:"id"`
	Genre      string            `json:"genre"`
	Mode       string            `json:"mode,omitempty"`
	Prompt     string            `json:"prompt"`
	Fields     map[string]string `json:"fields"`
	Size       string            `json:"size"`
	Enhance    bool              `json:"enhance"`
	Status     string            `json:"status"`
	CreatedAt  time.Time         `json:"created_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	Error      string            `json:"error,omitempty"`
	ImagePath  string            `json:"image_path,omitempty"`
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

// LoadHistory reads history.jsonl (nil if the file does not exist yet).
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
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var j Job
		if err := json.Unmarshal(sc.Bytes(), &j); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, sc.Err()
}
