package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"img-gen/internal/genres"
	"img-gen/internal/prompting"
	"img-gen/internal/storage"
)

type SubmitRequest struct {
	Genre   string            `json:"genre"`
	Fields  map[string]string `json:"fields"`
	Size    string            `json:"size"`
	Enhance bool              `json:"enhance"`
}

type Event struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type Options struct {
	Genres        *genres.Catalog
	Store         *storage.Store
	Generate      func(ctx context.Context, prompt, size string) ([]byte, error)
	Chat          prompting.ChatFunc
	EnhanceSystem string
}

type Manager struct {
	mu    sync.Mutex
	jobs  map[string]*storage.Job
	order []string
	subs  map[string][]chan Event
	work  chan string
	opts  Options
}

func New(opts Options) *Manager {
	m := &Manager{
		jobs: map[string]*storage.Job{},
		subs: map[string][]chan Event{},
		work: make(chan string, 64),
		opts: opts,
	}
	go m.worker()
	return m
}

func (m *Manager) Submit(req SubmitRequest) (string, error) {
	g, ok := m.opts.Genres.Genre(req.Genre)
	if !ok {
		return "", fmt.Errorf("unknown genre %q", req.Genre)
	}
	for _, f := range g.Fields {
		if f.Required && strings.TrimSpace(req.Fields[f.Key]) == "" {
			return "", fmt.Errorf("field %q is required", f.Key)
		}
	}
	if !contains(g.Sizes, req.Size) {
		return "", fmt.Errorf("size %q not allowed for genre %q", req.Genre, req.Size)
	}

	id := newID()
	job := &storage.Job{
		ID:        id,
		Genre:     req.Genre,
		Fields:    req.Fields,
		Size:      req.Size,
		Enhance:   req.Enhance,
		Status:    "queued",
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.jobs[id] = job
	m.order = append(m.order, id)
	m.mu.Unlock()

	m.work <- id
	return id, nil
}

func (m *Manager) worker() {
	for id := range m.work {
		m.run(id)
	}
}

func (m *Manager) run(id string) {
	m.mu.Lock()
	job := m.jobs[id]
	m.mu.Unlock()
	if job == nil {
		return
	}
	g, _ := m.opts.Genres.Genre(job.Genre)
	resolved := prompting.Resolve(g.Fields, job.Fields)

	var prompt string
	var err error
	if job.Enhance {
		m.setStatus(id, "enhancing", "")
		prompt, err = prompting.Enhance(context.Background(), m.opts.Chat, m.opts.EnhanceSystem, resolved)
		if err != nil {
			m.finish(id, "failed", err.Error())
			return
		}
	} else {
		prompt = prompting.Direct(g.PromptTemplate, resolved)
	}

	m.mu.Lock()
	job.Prompt = prompt
	m.mu.Unlock()

	m.setStatus(id, "generating", "")
	png, err := m.opts.Generate(context.Background(), prompt, job.Size)
	if err != nil {
		m.finish(id, "failed", err.Error())
		return
	}
	rel, err := m.opts.Store.SaveImage(id, png)
	if err != nil {
		m.finish(id, "failed", err.Error())
		return
	}
	m.mu.Lock()
	job.ImagePath = rel
	m.mu.Unlock()

	m.finish(id, "done", "")
}

// finish records the terminal state, persists it once, then notifies.
func (m *Manager) finish(id, status, errMsg string) {
	now := time.Now()
	m.mu.Lock()
	job := m.jobs[id]
	job.Status = status
	job.Error = errMsg
	job.FinishedAt = &now
	j := *job
	m.mu.Unlock()

	_ = m.opts.Store.AppendHistory(j)
	m.notify(id, status, errMsg)
}

// setStatus updates an in-progress status and notifies subscribers.
func (m *Manager) setStatus(id, status, errMsg string) {
	m.mu.Lock()
	if j := m.jobs[id]; j != nil {
		j.Status = status
	}
	m.mu.Unlock()
	m.notify(id, status, errMsg)
}

func (m *Manager) notify(id, status, errMsg string) {
	ev := Event{JobID: id, Status: status, Error: errMsg}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.subs[id] {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (m *Manager) Get(id string) (storage.Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		return *j, true
	}
	return storage.Job{}, false
}

func (m *Manager) List() []storage.Job {
	m.mu.Lock()
	out := make([]storage.Job, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if j, ok := m.jobs[m.order[i]]; ok {
			out = append(out, *j)
		}
	}
	m.mu.Unlock()

	hist, _ := m.opts.Store.LoadHistory()
	m.mu.Lock()
	for _, j := range hist {
		if _, ok := m.jobs[j.ID]; !ok {
			out = append(out, j)
		}
	}
	m.mu.Unlock()
	return out
}

func (m *Manager) Subscribe(id string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		ch <- Event{JobID: id, Status: j.Status}
	}
	m.subs[id] = append(m.subs[id], ch)
	m.mu.Unlock()

	cancel := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, c := range m.subs[id] {
			if c == ch {
				m.subs[id] = append(m.subs[id][:i], m.subs[id][i+1:]...)
				break
			}
		}
	}
	return ch, cancel
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
