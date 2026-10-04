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
	JobID  string    `json:"job_id"`
	Status string    `json:"status"`
	Error  string    `json:"error,omitempty"`
	Ts     time.Time `json:"ts"`
	Log    string    `json:"log,omitempty"`
}

type LogLine struct {
	Ts     time.Time
	Status string
	Msg    string
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
	logs  map[string][]LogLine
	order []string
	subs  map[string][]chan Event
	work  chan string
	opts  Options
}

func New(opts Options) *Manager {
	m := &Manager{
		jobs: map[string]*storage.Job{},
		logs: map[string][]LogLine{},
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
	m.log(id, "start: genre=%s size=%s enhance=%t", job.Genre, job.Size, job.Enhance)

	g, _ := m.opts.Genres.Genre(job.Genre)
	resolved := prompting.Resolve(g.Fields, job.Fields)
	m.log(id, "resolved %d field value(s)", len(resolved))

	var prompt string
	var err error
	if job.Enhance {
		m.setStatus(id, "enhancing", "")
		m.log(id, "sending enhance request to lattice chat")
		start := time.Now()
		prompt, err = prompting.Enhance(context.Background(), m.opts.Chat, m.opts.EnhanceSystem, resolved)
		if err != nil {
			m.log(id, "enhance failed after %s: %v", roundDur(time.Since(start)), err)
			m.finish(id, "failed", err.Error())
			return
		}
		m.log(id, "enhanced prompt ready in %s: %q", roundDur(time.Since(start)), prompt)
	} else {
		prompt = prompting.Direct(g.PromptTemplate, resolved)
		m.log(id, "assembled direct prompt: %q", prompt)
	}

	m.mu.Lock()
	job.Prompt = prompt
	m.mu.Unlock()

	m.setStatus(id, "generating", "")
	m.log(id, "requesting image from lattice (size=%s)", job.Size)
	start := time.Now()
	png, err := m.opts.Generate(context.Background(), prompt, job.Size)
	if err != nil {
		m.log(id, "generation failed after %s: %v", roundDur(time.Since(start)), err)
		m.finish(id, "failed", err.Error())
		return
	}
	m.log(id, "lattice returned %d bytes in %s", len(png), roundDur(time.Since(start)))

	rel, err := m.opts.Store.SaveImage(id, png)
	if err != nil {
		m.log(id, "save failed: %v", err)
		m.finish(id, "failed", err.Error())
		return
	}
	m.mu.Lock()
	job.ImagePath = rel
	m.mu.Unlock()
	m.log(id, "saved image to %s", rel)

	m.finish(id, "done", "")
}

// roundDur trims a duration to milliseconds for readable log lines.
func roundDur(d time.Duration) time.Duration {
	return d.Round(time.Millisecond)
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
	m.broadcast(Event{JobID: id, Status: status, Error: errMsg, Ts: time.Now()})
}

// log appends a timestamped line to the job's in-memory buffer and streams it
// to subscribers, tagging it with the job's current phase.
func (m *Manager) log(id, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now()
	m.mu.Lock()
	status := ""
	if j := m.jobs[id]; j != nil {
		status = j.Status
	}
	m.logs[id] = append(m.logs[id], LogLine{Ts: now, Status: status, Msg: msg})
	m.mu.Unlock()
	m.broadcast(Event{JobID: id, Status: status, Ts: now, Log: msg})
}

func (m *Manager) broadcast(ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.subs[ev.JobID] {
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
	ch := make(chan Event, 64)
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		// Replay any accumulated log lines first so a late subscriber (e.g. a
		// page reload mid-generation) catches up, then the current status.
		for _, l := range m.logs[id] {
			ch <- Event{JobID: id, Status: l.Status, Ts: l.Ts, Log: l.Msg}
		}
		ch <- Event{JobID: id, Status: j.Status, Ts: time.Now()}
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
