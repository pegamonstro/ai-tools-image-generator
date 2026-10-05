package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
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
	Style   string            `json:"style"` // global style preset key (empty = none)

	Mode      string    `json:"mode"`      // "generate"(default) | "edit" | "inpaint" | "blend"
	Prompt    string    `json:"prompt"`    // free-text prompt (non-generate modes)
	Image     string    `json:"image"`     // base64: edit/inpaint source
	Mask      string    `json:"mask"`      // base64: inpaint mask
	Strength  float64   `json:"strength"`  // edit denoise strength (0..1)
	Images    []string  `json:"images"`    // base64: blend references
	Strengths []float64 `json:"strengths"` // blend per-reference weights

	Model string            `json:"model"` // mflux --model value (path or HF id); "" = sidecar default
	Loras []storage.LoraRef `json:"loras"` // mflux --lora list; nil = none
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

type ImageOps struct {
	Generate func(ctx context.Context, prompt, size string, spec storage.ModelSpec) ([]byte, error)
	Edit     func(ctx context.Context, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec) ([]byte, error)
	Inpaint  func(ctx context.Context, prompt, imageB64, maskB64 string) ([]byte, error)
	Blend    func(ctx context.Context, prompt, size string, imagesB64 []string, strengths []float64) ([]byte, error)
}

type Options struct {
	Genres        *genres.Catalog
	Store         *storage.Store
	Ops           ImageOps
	Chat          prompting.ChatFunc
	EnhanceSystem string
}

type imageInputs struct {
	Image     string   // base64 (edit/inpaint source)
	Mask      string   // base64 (inpaint mask)
	Images    []string // base64 (blend refs)
	Strengths []float64
	Strength  float64
}

type Manager struct {
	mu     sync.Mutex
	jobs   map[string]*storage.Job
	logs   map[string][]LogLine
	order  []string
	subs   map[string][]chan Event
	work   chan string
	inputs map[string]imageInputs
	opts   Options
}

func New(opts Options) *Manager {
	m := &Manager{
		jobs:   map[string]*storage.Job{},
		logs:   map[string][]LogLine{},
		subs:   map[string][]chan Event{},
		work:   make(chan string, 64),
		inputs: map[string]imageInputs{},
		opts:   opts,
	}
	go m.worker()
	return m
}

func (m *Manager) Submit(req SubmitRequest) (string, error) {
	mode := req.Mode
	if mode == "" {
		mode = "generate"
	}

	var inputs imageInputs
	switch mode {
	case "generate":
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
			return "", fmt.Errorf("size %q not allowed for genre %q", req.Size, req.Genre)
		}
	case "edit":
		if strings.TrimSpace(req.Prompt) == "" {
			return "", fmt.Errorf("prompt is required for edit")
		}
		if strings.TrimSpace(req.Image) == "" {
			return "", fmt.Errorf("image is required for edit")
		}
		if !validSize(req.Size) {
			return "", fmt.Errorf("invalid size %q", req.Size)
		}
		inputs.Image = req.Image
		inputs.Strength = req.Strength
		if inputs.Strength == 0 {
			inputs.Strength = 0.4
		}
	case "inpaint":
		if strings.TrimSpace(req.Prompt) == "" {
			return "", fmt.Errorf("prompt is required for inpaint")
		}
		if strings.TrimSpace(req.Image) == "" || strings.TrimSpace(req.Mask) == "" {
			return "", fmt.Errorf("image and mask are required for inpaint")
		}
		inputs.Image = req.Image
		inputs.Mask = req.Mask
	case "blend":
		if strings.TrimSpace(req.Prompt) == "" {
			return "", fmt.Errorf("prompt is required for blend")
		}
		if len(req.Images) == 0 {
			return "", fmt.Errorf("at least one reference image is required for blend")
		}
		if !validSize(req.Size) {
			return "", fmt.Errorf("invalid size %q", req.Size)
		}
		inputs.Images = req.Images
		inputs.Strengths = normalizeStrengths(req.Strengths, len(req.Images))
	default:
		return "", fmt.Errorf("unknown mode %q", mode)
	}

	if req.Style != "" {
		if _, ok := m.opts.Genres.Style(req.Style); !ok {
			return "", fmt.Errorf("unknown style %q", req.Style)
		}
	}

	id := newID()
	job := &storage.Job{
		ID:        id,
		Mode:      mode,
		Genre:     req.Genre,
		Style:     req.Style,
		Prompt:    req.Prompt,
		Fields:    req.Fields,
		Size:      req.Size,
		Enhance:   req.Enhance,
		Model:     req.Model,
		Loras:     normalizeLoras(req.Loras),
		Status:    "queued",
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.jobs[id] = job
	if mode != "generate" {
		m.inputs[id] = inputs
	}
	m.order = append(m.order, id)
	m.mu.Unlock()

	m.work <- id
	return id, nil
}

func validSize(size string) bool {
	w, h, ok := strings.Cut(size, "x")
	if !ok {
		return false
	}
	wi, err1 := strconv.Atoi(w)
	hi, err2 := strconv.Atoi(h)
	return err1 == nil && err2 == nil && wi > 0 && hi > 0
}

func normalizeStrengths(ws []float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		if i < len(ws) {
			out[i] = ws[i]
		} else {
			out[i] = 1.0
		}
	}
	return out
}

// normalizeLoras defaults a missing scale to 1.0 and clamps to [0,1].
func normalizeLoras(refs []storage.LoraRef) []storage.LoraRef {
	out := make([]storage.LoraRef, len(refs))
	for i, r := range refs {
		if r.Scale == 0 {
			r.Scale = 1.0
		}
		if r.Scale < 0 {
			r.Scale = 0
		}
		if r.Scale > 1 {
			r.Scale = 1
		}
		out[i] = r
	}
	return out
}

func (m *Manager) worker() {
	for id := range m.work {
		m.run(id)
	}
}

func (m *Manager) run(id string) {
	m.mu.Lock()
	job := m.jobs[id]
	inputs := m.inputs[id]
	delete(m.inputs, id)
	m.mu.Unlock()
	if job == nil {
		return
	}
	m.log(id, "start: mode=%s size=%s enhance=%t", job.Mode, job.Size, job.Enhance)

	var prompt string
	var err error
	if job.Mode == "generate" {
		prompt, err = m.resolveGeneratePrompt(id, job)
	} else {
		prompt, err = m.resolveTextPrompt(id, job)
	}
	if err != nil {
		m.finish(id, "failed", err.Error())
		return
	}
	if job.Style != "" {
		if st, ok := m.opts.Genres.Style(job.Style); ok {
			prompt = st.Prompt + ", " + prompt
			m.log(id, "applied style %q: %q", job.Style, prompt)
		}
	}
	m.mu.Lock()
	job.Prompt = prompt
	m.mu.Unlock()

	m.setStatus(id, "generating", "")
	start := time.Now()
	spec := storage.ModelSpec{Model: job.Model, Loras: job.Loras}
	var png []byte
	switch job.Mode {
	case "edit":
		m.log(id, "requesting edit (strength=%.2f)", inputs.Strength)
		png, err = m.opts.Ops.Edit(context.Background(), prompt, job.Size, inputs.Image, inputs.Strength, spec)
	case "inpaint":
		m.log(id, "requesting inpaint")
		png, err = m.opts.Ops.Inpaint(context.Background(), prompt, inputs.Image, inputs.Mask)
	case "blend":
		m.log(id, "requesting blend (%d reference image(s))", len(inputs.Images))
		png, err = m.opts.Ops.Blend(context.Background(), prompt, job.Size, inputs.Images, inputs.Strengths)
	default:
		m.log(id, "requesting image from lattice (size=%s)", job.Size)
		png, err = m.opts.Ops.Generate(context.Background(), prompt, job.Size, spec)
	}
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

// resolveGeneratePrompt builds the prompt for a genre/fields job (direct or
// enhanced). It is the pre-existing prompt path, factored out of run.
func (m *Manager) resolveGeneratePrompt(id string, job *storage.Job) (string, error) {
	g, _ := m.opts.Genres.Genre(job.Genre)
	resolved := prompting.Resolve(g.Fields, job.Fields)
	m.log(id, "resolved %d field value(s)", len(resolved))
	if job.Enhance {
		m.setStatus(id, "enhancing", "")
		m.log(id, "sending enhance request to lattice chat")
		start := time.Now()
		p, err := prompting.Enhance(context.Background(), m.opts.Chat, m.opts.EnhanceSystem, resolved)
		if err != nil {
			m.log(id, "enhance failed after %s: %v", roundDur(time.Since(start)), err)
			return "", err
		}
		m.log(id, "enhanced prompt ready in %s: %q", roundDur(time.Since(start)), p)
		return p, nil
	}
	p := prompting.Direct(g.PromptTemplate, resolved)
	m.log(id, "assembled direct prompt: %q", p)
	return p, nil
}

// resolveTextPrompt builds the prompt for a free-text (non-generate) job.
func (m *Manager) resolveTextPrompt(id string, job *storage.Job) (string, error) {
	if job.Enhance {
		m.setStatus(id, "enhancing", "")
		m.log(id, "sending free-text prompt to lattice chat for enhancement")
		start := time.Now()
		p, err := prompting.EnhancePrompt(context.Background(), m.opts.Chat, m.opts.EnhanceSystem, job.Prompt)
		if err != nil {
			m.log(id, "enhance failed after %s: %v", roundDur(time.Since(start)), err)
			return "", err
		}
		m.log(id, "enhanced prompt ready in %s: %q", roundDur(time.Since(start)), p)
		return p, nil
	}
	return job.Prompt, nil
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
