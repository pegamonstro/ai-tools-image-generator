package queue

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"img-gen/internal/compose"
	"img-gen/internal/storage"
)

// --- persisted parent spec ------------------------------------------------

// composeSpec is what a parent job remembers (Job.ComposeSpec) so a restart
// can re-compose. Child job ids are deliberately absent: children are
// recovered by scanning for jobs whose ComposeParent == parent ID, so there
// is no registration race between spec and child list.
type composeSpec struct {
	Kind   string      `json:"kind"` // "asset-set" | "gif"
	Name   string      `json:"name"`
	Dest   string      `json:"dest,omitempty"`
	Assets []assetSpec `json:"assets,omitempty"`
	GIF    *gifPayload `json:"gif,omitempty"`
}

type assetSpec struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Prompt      string `json:"prompt"`
	Size        string `json:"size"`
	Frames      int    `json:"frames,omitempty"`
	Columns     int    `json:"columns,omitempty"`
	Transparent bool   `json:"transparent,omitempty"`
	Children    int    `json:"children"` // generation jobs this asset owns, in order
}

type gifPayload struct {
	Frames  []string `json:"frames,omitempty"` // set-relative source files (from_frames)
	DelayMS int      `json:"delay_ms"`
	Loop    int      `json:"loop"`
	Formats []string `json:"formats"`
}

// --- parent registration --------------------------------------------------

// submitComposeParent registers a compose parent and submits its generation
// children. The parent is registered (in-memory + persisted) BEFORE any child
// is submitted and its remaining-child count presets to len(children), so a
// child that finishes within microseconds of its submit can never outrun the
// parent's registration. Parents occupy no worker slot while children run —
// the last child to turn terminal re-enqueues the parent (childFinished).
func (m *Manager) submitComposeParent(parentID string, spec composeSpec, children []SubmitRequest) (string, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("compose spec: %v", err)
	}
	parent := &storage.Job{
		ID:          parentID,
		Mode:        "compose",
		Prompt:      strings.TrimSpace("compose " + spec.Kind),
		SetID:       parentID,
		ComposeSpec: string(specJSON),
		Status:      "queued",
		CreatedAt:   time.Now(),
	}
	m.mu.Lock()
	m.jobs[parentID] = parent
	m.order = append(m.order, parentID)
	m.childLeft[parentID] = len(children)
	m.mu.Unlock()
	if err := m.opts.Store.AppendHistory(*parent); err != nil {
		log.Printf("queue: persist queued compose parent %s: %v", parentID, err)
	}
	if len(children) == 0 {
		// No generation children (a from-frames GIF): compose immediately.
		m.enqueueCompose(parentID)
		m.mu.Lock()
		j := *parent
		m.mu.Unlock()
		_ = j
		return parentID, nil
	}
	var ids []string
	for i := range children {
		children[i].ComposeParent = parentID
		id, serr := m.submitOne(children[i], "")
		if serr != nil {
			m.abandonCompose(parentID, ids)
			return "", serr
		}
		ids = append(ids, id)
	}
	m.mu.Lock()
	parent.Children = ids
	j := *parent
	m.mu.Unlock()
	if err := m.opts.Store.AppendHistory(j); err != nil {
		log.Printf("queue: persist compose parent children %s: %v", parentID, err)
	}
	return parentID, nil
}

// abandonCompose unwinds a half-submitted parent: drop the in-memory parent
// so later child finishes no-op (childFinished finds no parent job), and
// cancel whatever children already went out.
func (m *Manager) abandonCompose(parentID string, submitted []string) {
	m.mu.Lock()
	delete(m.jobs, parentID)
	delete(m.childLeft, parentID)
	for i, o := range m.order {
		if o == parentID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	for _, cid := range submitted {
		_ = m.Cancel(cid) // best effort: terminal children were their own story
	}
}

// enqueueCompose pushes a parent onto the work channel. The channel is small
// (cap 64) and the worker may be blocked finishing the child that triggered
// this send, so a non-blocking send falls back to a goroutine that waits —
// never to a dropped enqueue.
func (m *Manager) enqueueCompose(parentID string) {
	select {
	case m.work <- parentID:
	default:
		go func() { m.work <- parentID }()
	}
}

// --- child -> parent handoff ----------------------------------------------

// childFinished is called from finish() for every terminal job. Compose
// children decrement their parent's remaining count; the child that drops it
// to zero re-enqueues the parent so the worker composes the finished parts.
// This is the single-slot coordination: the parent never occupies a slot
// while its children generate.
//
// A parent deleted mid-run (abandonCompose) is nil in jobs, so its children's
// finishes no-op here.
func (m *Manager) childFinished(childID string) {
	m.mu.Lock()
	j := m.jobs[childID]
	if j == nil || j.ComposeParent == "" {
		m.mu.Unlock()
		return
	}
	parent := j.ComposeParent
	if m.jobs[parent] == nil {
		m.mu.Unlock()
		return
	}
	left, ok := m.childLeft[parent]
	if !ok {
		// No pending handoff: the parent already composed, or restore's
		// first pass failed it — enqueueing would resurrect it past a
		// terminal record.
		m.mu.Unlock()
		return
	}
	if left > 1 {
		m.childLeft[parent] = left - 1
		m.mu.Unlock()
		return
	}
	delete(m.childLeft, parent)
	m.mu.Unlock()
	m.enqueueCompose(parent)
}

// --- compose run -----------------------------------------------------------

// runCompose executes a worker-dequeued compose parent. The job's deliverable
// work is CPU-local (no sidecar), so it runs synchronously inside the worker.
func (m *Manager) runCompose(id string) {
	m.mu.Lock()
	job := m.jobs[id]
	var spec composeSpec
	if job != nil && json.Unmarshal([]byte(job.ComposeSpec), &spec) != nil {
		job = nil // unparsable spec: handled below as a hard failure
	}
	m.mu.Unlock()
	if job == nil {
		m.finish(id, "failed", "compose_error: compose spec unparsable or parent gone")
		return
	}
	m.log(id, "compose start (kind=%s)", spec.Kind)
	if m.isCancelled(id) {
		m.finish(id, "cancelled", "")
		return
	}
	m.setStatus(id, "generating", "")

	// Children in submission order: m.order records submit order and every
	// child carries ComposeParent, so the reconstruction needs no persisted
	// child list.
	m.mu.Lock()
	var children []string
	for _, oid := range m.order {
		if c := m.jobs[oid]; c != nil && c.ComposeParent == id {
			children = append(children, c.ID)
		}
	}
	m.mu.Unlock()

	switch spec.Kind {
	case "asset-set":
		m.composeAssetSet(id, &spec, children)
	case "gif":
		m.composeGIF(id, &spec, children)
	default:
		m.finish(id, "failed", fmt.Sprintf("compose_error: unknown compose kind %q", spec.Kind))
	}
}

// childResult fetches a child's outcome: status, error text, and — when the
// child succeeded — its stored PNG bytes.
func (m *Manager) childResult(childID string) (status, errMsg string, png []byte) {
	m.mu.Lock()
	j := m.jobs[childID]
	m.mu.Unlock()
	if j == nil {
		return "failed", "child job record missing", nil
	}
	if j.Status != "done" {
		msg := j.Error
		if msg == "" {
			msg = "child " + j.Status
		}
		return j.Status, msg, nil
	}
	b, err := m.opts.Store.ReadImage(childID)
	if err != nil {
		return "failed", err.Error(), nil
	}
	return "done", "", b
}

// childrenResults loads a parent's child images in order. specChildren is the
// child count the spec expects; a child that is not done contributes nil with
// its error string recorded.
func (m *Manager) childrenResults(specChildren int, ids []string, logf func(f string, a ...any)) (imgs [][]byte, errs []string) {
	if len(ids) != specChildren {
		// Defensive: a drifted child set fails the parent loudly instead of
		// silently composing the wrong artifact.
		return nil, []string{fmt.Sprintf("expected %d child result(s), found %d", specChildren, len(ids))}
	}
	imgs = make([][]byte, len(ids))
	for i, cid := range ids {
		st, em, png := m.childResult(cid)
		if st != "done" {
			errs = append(errs, em)
			logf("child %s: %s", cid, em)
		}
		imgs[i] = png
	}
	return imgs, errs
}

// resolveExportDest applies the delivery fallback chain: explicit dest →
// EXPORT_DIR → ~/Downloads/img-gen (the same chain the v1 export endpoint
// uses). An empty dest after the chain means no delivery.
func (m *Manager) resolveExportDest(dest string) string {
	if dest == "" {
		dest = m.opts.ExportDir
	}
	if dest == "" {
		home, _ := os.UserHomeDir()
		dest = filepath.Join(home, "Downloads", "img-gen")
	}
	return filepath.Clean(dest)
}

// validComposeAssetName lowercases, squashes disallowed characters, and trims
// leading/trailing separators and dots. Any non-empty result passes storage's
// validSetFile rule (that helper is unexported, so queue mirrors its shape
// here — SaveSetFile re-checks the real rule as defense in depth). An empty
// return means nothing sane remained; callers reject with a validation error.
func validComposeAssetName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" || strings.Contains(out, "..") || len(out) > 128 {
		return ""
	}
	return out
}

var _ = compose.KitClasses // referenced by composeAssetSet (Task 8)

func (m *Manager) composeAssetSet(id string, spec *composeSpec, children []string) {
	// Filled by Task 8 (per-asset composition). Until then a parent composed
	// as done without files would lie, so fail honestly with the spec intact.
	m.finish(id, "failed", "compose_error: asset-set executor not implemented")
}

func (m *Manager) composeGIF(id string, spec *composeSpec, children []string) {
	m.finish(id, "failed", "compose_error: gif executor not implemented")
}
