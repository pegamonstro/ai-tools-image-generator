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

// --- public asset-set submission ------------------------------------------

const (
	maxAssetsPerSet    = 8
	maxComposeChildren = 64
)

const (
	whiteBgSuffix = ", isolated on plain white background"
	blackBgSuffix = ", isolated on plain black background"
)

// AssetSetRequest is the public asset-set submission. Seed is required:
// a set's parts must be reproducible for iteration on a design.
type AssetSetRequest struct {
	Name   string        `json:"name"`           // file stem for the set (sanitized)
	Dest   string        `json:"dest,omitempty"` // absolute dir to deliver into; empty = EXPORT_DIR fallback chain
	Seed   *int64        `json:"seed"`           // required
	Assets []AssetSpecIn `json:"assets"`         // 1..8
}

type AssetSpecIn struct {
	Kind        string `json:"kind"` // "element" | "kit" | "atlas"
	Name        string `json:"name"`
	Prompt      string `json:"prompt"`
	Negative    string `json:"negative,omitempty"`
	Size        string `json:"size"`                  // WxH, multiples of 16
	Frames      int    `json:"frames,omitempty"`      // atlas: 2..64
	Columns     int    `json:"columns,omitempty"`     // atlas: 0 = single row
	Transparent bool   `json:"transparent,omitempty"` // element: white+black double pass
}

// SubmitAssetSet validates, registers the compose parent (set id = job id),
// and submits the generation children. It returns as soon as children are
// queued — the caller polls the job or GETs the set.
func (m *Manager) SubmitAssetSet(req AssetSetRequest) (string, error) {
	setName := validComposeAssetName(req.Name)
	if setName == "" {
		return "", &ValidationError{Code: "validation_error", Msg: "name is required for asset sets"}
	}
	if req.Seed == nil {
		return "", &ValidationError{Code: "validation_error", Msg: "seed is required for asset sets"}
	}
	if req.Dest != "" && !filepath.IsAbs(req.Dest) {
		return "", &ValidationError{Code: "validation_error", Msg: "dest must be an absolute path"}
	}
	if len(req.Assets) == 0 || len(req.Assets) > maxAssetsPerSet {
		return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset sets take 1..%d assets", maxAssetsPerSet)}
	}
	spec := composeSpec{Kind: "asset-set", Name: setName, Dest: req.Dest}
	var kids []SubmitRequest
	seen := map[string]bool{}
	total := 0
	for _, a := range req.Assets {
		an := validComposeAssetName(a.Name)
		if an == "" {
			return "", &ValidationError{Code: "validation_error", Msg: "asset name is required"}
		}
		if seen[an] {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("duplicate asset name %q", an)}
		}
		seen[an] = true
		prompt := strings.TrimSpace(a.Prompt)
		if prompt == "" {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: prompt is required", an)}
		}
		if !validAssetSize(a.Size) {
			return "", &ValidationError{Code: "invalid_size", Msg: fmt.Sprintf("asset %q: invalid size %q (want WxH, multiples of 16)", an, a.Size)}
		}
		as := assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Frames: a.Frames, Columns: a.Columns, Transparent: a.Transparent}
		var built []SubmitRequest
		switch a.Kind {
		case "element":
			built = elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent)
		case "kit":
			built = kitChildren(prompt, a.Negative, a.Size, *req.Seed)
		case "atlas":
			if a.Frames < 2 || a.Frames > 64 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frames must be 2..64", an)}
			}
			built = atlasChildren(prompt, a.Negative, a.Size, *req.Seed, a.Frames)
		default:
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: unknown kind %q (want element, kit, atlas)", an, a.Kind)}
		}
		as.Children = len(built)
		total += as.Children
		spec.Assets = append(spec.Assets, as)
		kids = append(kids, built...)
	}
	if total > maxComposeChildren {
		return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset set needs %d generation jobs; limit is %d", total, maxComposeChildren)}
	}
	return m.submitComposeParent(newID(), spec, kids)
}

// elementChildren builds the 1 job (opaque) or 2 jobs (transparent: white
// and black pass, same seed — only the background prompt differs).
func elementChildren(prompt, negative, size string, seed int64, transparent bool) []SubmitRequest {
	if !transparent {
		s := seed
		return []SubmitRequest{{Mode: "asset", Prompt: prompt, NegativePrompt: negative, Size: size, Seed: &s}}
	}
	ws := seed
	bs := seed
	return []SubmitRequest{
		{Mode: "asset", Prompt: prompt + whiteBgSuffix, NegativePrompt: negative, Size: size, Seed: &ws},
		{Mode: "asset", Prompt: prompt + blackBgSuffix, NegativePrompt: negative, Size: size, Seed: &bs},
	}
}

// kitChildren builds one job per wang class, seeded seed+SeedOffset, with
// the class hint appended so each tile knows its edge role.
func kitChildren(prompt, negative, size string, seed int64) []SubmitRequest {
	kids := make([]SubmitRequest, 0, len(compose.KitClasses))
	for _, kc := range compose.KitClasses {
		s := seed + int64(kc.SeedOffset)
		p := prompt
		if kc.Hint != "" {
			p = prompt + ", " + kc.Hint
		}
		kids = append(kids, SubmitRequest{Mode: "asset", Prompt: p, NegativePrompt: negative, Size: size, Seed: &s})
	}
	return kids
}

// atlasChildren builds one job per animation frame, seeded seed+i.
func atlasChildren(prompt, negative, size string, seed int64, frames int) []SubmitRequest {
	kids := make([]SubmitRequest, 0, frames)
	for i := 0; i < frames; i++ {
		s := seed + int64(i)
		p := fmt.Sprintf("%s, animation frame %d of %d, consistent style and lighting", prompt, i+1, frames)
		kids = append(kids, SubmitRequest{Mode: "asset", Prompt: p, NegativePrompt: negative, Size: size, Seed: &s})
	}
	return kids
}

// --- manifest --------------------------------------------------------------

type setManifest struct {
	SetID     string          `json:"set_id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	CreatedAt time.Time       `json:"created_at"`
	Assets    []manifestAsset `json:"assets,omitempty"`
	GIF       *manifestGIF    `json:"gif,omitempty"`
	Delivered []string        `json:"delivered,omitempty"`
	Dest      string          `json:"dest,omitempty"`
}

type manifestAsset struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Prompt string   `json:"prompt,omitempty"`
	Size   string   `json:"size,omitempty"`
	Files  []string `json:"files,omitempty"`
	Error  string   `json:"error,omitempty"`
}

type manifestGIF struct {
	Name    string   `json:"name"`
	Files   []string `json:"files,omitempty"`
	DelayMS int      `json:"delay_ms"`
	Loop    int      `json:"loop"`
	Formats []string `json:"formats,omitempty"`
	WebP    string   `json:"webp,omitempty"` // "ok" | "unavailable: reason"
	Error   string   `json:"error,omitempty"`
}

func (m *Manager) saveManifest(id string, man *setManifest) {
	specJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		m.log(id, "manifest marshal: %v", err)
		return
	}
	if err := m.opts.Store.SaveSetFile(id, "manifest.json", specJSON); err != nil {
		m.log(id, "manifest save: %v", err)
	}
}

// composeAssetSet turns terminal children into set files, writes the
// manifest, delivers when a dest is set, and finishes the parent. Partial
// success: the set completes if ANY asset composed; per-asset errors are
// recorded in the manifest.
func (m *Manager) composeAssetSet(id string, spec *composeSpec, children []string) {
	man := setManifest{SetID: id, Kind: "asset-set", Name: spec.Name, CreatedAt: time.Now(), Assets: []manifestAsset{}}
	cursor := 0
	ok := 0
	var firstErr string
	var files []storage.FileRef
	for _, a := range spec.Assets {
		ma := manifestAsset{Kind: a.Kind, Name: a.Name, Prompt: a.Prompt, Size: a.Size}
		end := cursor + a.Children
		if cursor+a.Children > len(children) {
			ma.Error = fmt.Sprintf("expected %d child job(s), found %d", a.Children, len(children)-cursor)
			man.Assets = append(man.Assets, ma)
			if firstErr == "" {
				firstErr = ma.Error
			}
			continue
		}
		kids := children[cursor:end]
		cursor = end
		broken := ""
		imgs := make([][]byte, len(kids))
		for i, cid := range kids {
			st, em, png := m.childResult(cid)
			if st != "done" {
				broken = em
				break
			}
			imgs[i] = png
		}
		if broken == "" {
			var outs []compose.OutputFile
			var err error
			switch a.Kind {
			case "element":
				// ComposeElement yields one deliverable; kits and atlases yield many.
				var of compose.OutputFile
				if a.Transparent && len(imgs) == 2 {
					of, err = compose.ComposeElement(a.Name, imgs[0], imgs[1], true)
				} else {
					of, err = compose.ComposeElement(a.Name, imgs[0], nil, false)
				}
				if err == nil {
					outs = []compose.OutputFile{of}
				}
			case "kit":
				outs, err = compose.ComposeKit(a.Name, imgs)
			case "atlas":
				outs, err = compose.ComposeAtlas(a.Name, imgs, a.Columns)
			default:
				err = fmt.Errorf("unknown asset kind %q", a.Kind)
			}
			if err == nil {
				for _, of := range outs {
					if serr := m.opts.Store.SaveSetFile(id, of.Name, of.Data); serr != nil {
						err = serr
						break
					}
					files = append(files, storage.FileRef{Name: of.Name, Primary: of.Primary})
					ma.Files = append(ma.Files, of.Name)
				}
			}
			if err != nil {
				broken = err.Error()
			}
		}
		if broken != "" {
			ma.Error = broken
			if firstErr == "" {
				firstErr = broken
			}
		} else {
			ok++
		}
		man.Assets = append(man.Assets, ma)
	}
	m.mu.Lock()
	job := m.jobs[id]
	if job != nil {
		job.Files = files
		if ok > 0 {
			job.ImagePath = "sets/" + id + "/" + primaryName(files)
		}
		// Files are set on the in-memory job before finish() persists it, so
		// history and restore() carry the set outputs.
	}
	m.mu.Unlock()
	m.saveManifest(id, &man)
	if ok == 0 {
		m.finish(id, "failed", "compose_error: all assets failed: "+firstErr)
		return
	}
	// Deliver on every success: unset dest inherits the export fallback chain
	// (dest → EXPORT_DIR → ~/Downloads/img-gen) per spec; resolveExportDest
	// never returns empty, so there is no "no delivery" case.
	dest := m.resolveExportDest(spec.Dest)
	served, err := m.opts.Store.DeliverSet(id, dest)
	if err != nil {
		m.finish(id, "failed", fmt.Sprintf("compose_error: %v", err))
		return
	}
	man.Delivered = served
	man.Dest = dest
	m.saveManifest(id, &man)
	m.log(id, "compose done: %d/%d asset(s)", ok, len(spec.Assets))
	m.finish(id, "done", "")
}

// primaryName is the first Primary FileRef, falling back to the first file.
func primaryName(refs []storage.FileRef) string {
	for _, r := range refs {
		if r.Primary {
			return r.Name
		}
	}
	if len(refs) > 0 {
		return refs[0].Name
	}
	return ""
}

func (m *Manager) composeGIF(id string, spec *composeSpec, children []string) {
	m.finish(id, "failed", "compose_error: gif executor not implemented")
}
