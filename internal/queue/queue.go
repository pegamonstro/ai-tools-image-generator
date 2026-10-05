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

	Seed           *int64   `json:"seed,omitempty"`            // fixed seed; nil = random
	Steps          *int     `json:"steps,omitempty"`           // diffusion steps; nil = sidecar default
	Guidance       *float64 `json:"guidance,omitempty"`        // guidance; nil = sidecar default
	NegativePrompt string   `json:"negative_prompt,omitempty"` // things to avoid; "" = none
}

type Event struct {
	JobID  string    `json:"job_id"`
	Status string    `json:"status"`
	Error  string    `json:"error,omitempty"`
	Ts     time.Time `json:"ts"`
	Log    string    `json:"log,omitempty"`
	Step   int       `json:"step,omitempty"`
	Total  int       `json:"total,omitempty"`
	Seed   *int64    `json:"seed,omitempty"` // seed used, on the terminal "done" event
}

type LogLine struct {
	Ts     time.Time
	Status string
	Msg    string
}

type ImageOps struct {
	Generate func(ctx context.Context, prompt, size string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error)
	Edit     func(ctx context.Context, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error)
	Inpaint  func(ctx context.Context, prompt, imageB64, maskB64 string) ([]byte, error)
	Blend    func(ctx context.Context, prompt, size string, imagesB64 []string, strengths []float64) ([]byte, error)
	Progress func(ctx context.Context, mode string) (step, total int, err error)
	Cancel   func(ctx context.Context, mode string) error
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
	mu        sync.Mutex
	jobs      map[string]*storage.Job
	logs      map[string][]LogLine
	order     []string
	subs      map[string][]chan Event
	work      chan string
	inputs    map[string]imageInputs
	cancels   map[string]context.CancelFunc
	cancelled map[string]bool
	opts      Options
}

func New(opts Options) *Manager {
	m := &Manager{
		jobs:      map[string]*storage.Job{},
		logs:      map[string][]LogLine{},
		subs:      map[string][]chan Event{},
		work:      make(chan string, 64),
		inputs:    map[string]imageInputs{},
		cancels:   map[string]context.CancelFunc{},
		cancelled: map[string]bool{},
		opts:      opts,
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
	case "inpaint", "outpaint":
		if strings.TrimSpace(req.Prompt) == "" {
			return "", fmt.Errorf("prompt is required for %s", mode)
		}
		if strings.TrimSpace(req.Image) == "" || strings.TrimSpace(req.Mask) == "" {
			return "", fmt.Errorf("image and mask are required for %s", mode)
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
		ID:             id,
		Mode:           mode,
		Genre:          req.Genre,
		Style:          req.Style,
		Prompt:         req.Prompt,
		Fields:         req.Fields,
		Size:           req.Size,
		Enhance:        req.Enhance,
		Model:          req.Model,
		Loras:          normalizeLoras(req.Loras),
		Seed:           req.Seed,
		Steps:          req.Steps,
		Guidance:       req.Guidance,
		NegativePrompt: req.NegativePrompt,
		Status:         "queued",
		CreatedAt:      time.Now(),
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

	if m.isCancelled(id) {
		m.finish(id, "cancelled", "")
		return
	}

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
	if m.isCancelled(id) {
		m.finish(id, "cancelled", "")
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

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancels[id] = cancel
	m.mu.Unlock()

	m.setStatus(id, "generating", "")
	start := time.Now()
	spec := storage.ModelSpec{Model: job.Model, Loras: job.Loras}
	sp := storage.SamplingParams{Seed: job.Seed, Steps: job.Steps, Guidance: job.Guidance, NegativePrompt: job.NegativePrompt}

	png, seed, err := m.dispatch(ctx, id, job, inputs, prompt, spec, sp)
	cancel()
	m.mu.Lock()
	delete(m.cancels, id)
	m.mu.Unlock()

	if err != nil {
		if m.isCancelled(id) {
			m.log(id, "generation cancelled after %s", roundDur(time.Since(start)))
			m.finish(id, "cancelled", "")
			return
		}
		m.log(id, "generation failed after %s: %v", roundDur(time.Since(start)), err)
		m.finish(id, "failed", err.Error())
		return
	}
	if seed != nil {
		m.mu.Lock()
		job.Seed = seed
		m.mu.Unlock()
		m.log(id, "seed=%d", *seed)
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

// dispatch runs the mode-specific op in a goroutine so per-step progress can
// be polled from the sidecar while the op blocks. It returns the PNG bytes or
// the op error. The op receives ctx, so cancelling it aborts the HTTP request.
func (m *Manager) dispatch(ctx context.Context, id string, job *storage.Job, inputs imageInputs, prompt string, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
	type result struct {
		png  []byte
		seed *int64
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var png []byte
		var seed *int64
		var err error
		switch job.Mode {
		case "edit":
			m.log(id, "requesting edit (strength=%.2f)", inputs.Strength)
			png, seed, err = m.opts.Ops.Edit(ctx, prompt, job.Size, inputs.Image, inputs.Strength, spec, sp)
		case "inpaint":
			m.log(id, "requesting inpaint")
			png, err = m.opts.Ops.Inpaint(ctx, prompt, inputs.Image, inputs.Mask)
		case "blend":
			m.log(id, "requesting blend (%d reference image(s))", len(inputs.Images))
			png, err = m.opts.Ops.Blend(ctx, prompt, job.Size, inputs.Images, inputs.Strengths)
		default:
			m.log(id, "requesting image from lattice (size=%s)", job.Size)
			png, seed, err = m.opts.Ops.Generate(ctx, prompt, job.Size, spec, sp)
		}
		done <- result{png, seed, err}
	}()

	if m.opts.Ops.Progress == nil {
		r := <-done
		return r.png, r.seed, r.err
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case r := <-done:
			return r.png, r.seed, r.err
		case <-ticker.C:
			step, total, perr := m.opts.Ops.Progress(context.Background(), job.Mode)
			if perr == nil && total > 0 {
				m.setProgress(id, step, total)
			}
		}
	}
}

// setProgress records the latest step/total on the job and streams it to
// subscribers as a progress event.
func (m *Manager) setProgress(id string, step, total int) {
	m.mu.Lock()
	if j := m.jobs[id]; j != nil {
		j.Step = step
		j.Total = total
	}
	m.mu.Unlock()
	m.broadcast(Event{JobID: id, Status: "generating", Ts: time.Now(), Step: step, Total: total})
}

func (m *Manager) isCancelled(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelled[id]
}

// Cancel stops a queued or in-flight job: it aborts the job's HTTP request via
// its context, and asks the sidecar to kill its mflux subprocess so the
// single-flight slot is freed for the next job. It is a no-op for terminal jobs.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	job := m.jobs[id]
	cfn := m.cancels[id]
	status := ""
	if job != nil {
		status = job.Status
	}
	m.mu.Unlock()
	if job == nil {
		return fmt.Errorf("job not found")
	}
	switch status {
	case "queued", "enhancing", "generating":
	default:
		return fmt.Errorf("job is %s", status)
	}
	m.mu.Lock()
	m.cancelled[id] = true
	m.mu.Unlock()
	if cfn != nil {
		cfn()
	}
	if m.opts.Ops.Cancel != nil {
		_ = m.opts.Ops.Cancel(context.Background(), job.Mode)
	}
	return nil
}

// resolveGeneratePrompt builds the prompt for a genre/fields job (direct or
// enhanced). It is the pre-existing prompt path, factored out of run.
func (m *Manager) resolveGeneratePrompt(id string, job *storage.Job) (string, error) {
	g, _ := m.opts.Genres.Genre(job.Genre)
	resolved := prompting.Resolve(g.Fields, job.Fields)
	m.log(id, "resolved %d field value(s)", len(resolved))
	direct := prompting.Direct(g.PromptTemplate, resolved)
	if job.Enhance {
		m.setStatus(id, "enhancing", "")
		m.log(id, "sending enhance request to lattice chat")
		start := time.Now()
		p, err := prompting.Enhance(context.Background(), m.opts.Chat, m.opts.EnhanceSystem, resolved)
		switch {
		case err != nil:
			m.log(id, "enhance failed after %s: %v (using direct prompt)", roundDur(time.Since(start)), err)
			return direct, nil
		case prompting.Refused(p):
			m.log(id, "enhancement refused after %s (using direct prompt)", roundDur(time.Since(start)))
			return direct, nil
		default:
			m.log(id, "enhanced prompt ready in %s: %q", roundDur(time.Since(start)), p)
			return p, nil
		}
	}
	m.log(id, "assembled direct prompt: %q", direct)
	return direct, nil
}

// resolveTextPrompt builds the prompt for a free-text (non-generate) job.
func (m *Manager) resolveTextPrompt(id string, job *storage.Job) (string, error) {
	if job.Enhance {
		m.setStatus(id, "enhancing", "")
		m.log(id, "sending free-text prompt to lattice chat for enhancement")
		start := time.Now()
		p, err := prompting.EnhancePrompt(context.Background(), m.opts.Chat, m.opts.EnhanceSystem, job.Prompt)
		switch {
		case err != nil:
			m.log(id, "enhance failed after %s: %v (using raw prompt)", roundDur(time.Since(start)), err)
			return job.Prompt, nil
		case prompting.Refused(p):
			m.log(id, "enhancement refused after %s (using raw prompt)", roundDur(time.Since(start)))
			return job.Prompt, nil
		default:
			m.log(id, "enhanced prompt ready in %s: %q", roundDur(time.Since(start)), p)
			return p, nil
		}
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
	m.broadcast(Event{JobID: id, Status: status, Error: errMsg, Ts: now, Seed: j.Seed})
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
		ch <- Event{JobID: id, Status: j.Status, Ts: time.Now(), Step: j.Step, Total: j.Total}
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
