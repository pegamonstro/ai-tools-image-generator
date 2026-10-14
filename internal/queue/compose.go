package queue

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strconv"
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
	Kind        string      `json:"kind"` // "asset-set" | "gif"
	Name        string      `json:"name"`
	Dest        string      `json:"dest,omitempty"`
	PaletteLock bool        `json:"palette_lock,omitempty"`
	Assets      []assetSpec `json:"assets,omitempty"`
	GIF         *gifPayload `json:"gif,omitempty"`
}

type assetSpec struct {
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	Prompt       string   `json:"prompt"`
	Size         string   `json:"size"`
	Frames       int      `json:"frames,omitempty"`
	Columns      int      `json:"columns,omitempty"`
	Transparent  bool     `json:"transparent,omitempty"`
	FrameMargins int      `json:"frame_margins,omitempty"`
	BarFill      string   `json:"bar_fill,omitempty"`
	FillStates   []int    `json:"fill_states,omitempty"`
	Shape        string   `json:"shape,omitempty"`    // npc: "variants" | "walk4"
	Variants     []string `json:"variants,omitempty"` // npc variants: per-variant prompt suffixes
	MetaFormats  []string `json:"meta_formats,omitempty"`
	Children     int      `json:"children"` // generation jobs this asset owns, in order
}

// compositeLayerSpec is one persisted static layer: its set file (saved at
// submit time) and the position to blit it at each frame during a recompose.
type compositeLayerSpec struct {
	File string `json:"file"`
	Pos  [2]int `json:"pos"`
}

type gifPayload struct {
	Frames    []string             `json:"frames,omitempty"` // set-relative source files (from_frames/composite anim)
	Composite []compositeLayerSpec `json:"composite,omitempty"`
	Keyframes *keyframesPayload    `json:"keyframes,omitempty"` // keyframes: resolved anchors as keyframe-%04d.png
	DelayMS   int                  `json:"delay_ms"`
	Loop      int                  `json:"loop"`
	Formats   []string             `json:"formats"`
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
// so later child finishes no-op (childFinished finds no parent job), drop the
// parent's persisted history too — the parent went down as "queued" in
// history.jsonl before any child was submitted (submitComposeParent), so
// leaving it would strand a queued ghost that Cancel/Delete reject and a
// restart would recompose — and cancel whatever children already went out.
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
	if err := m.opts.Store.DeleteHistory(parentID); err != nil {
		log.Printf("queue: drop abandoned compose parent %s history: %v", parentID, err)
	}
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
	maxComposeStatics  = 8
)

const (
	whiteBgSuffix = ", isolated on plain white background"
	blackBgSuffix = ", isolated on plain black background"
)

// AssetSetRequest is the public asset-set submission. Seed is required:
// a set's parts must be reproducible for iteration on a design.
type AssetSetRequest struct {
	Name        string        `json:"name"`           // file stem for the set (sanitized)
	Dest        string        `json:"dest,omitempty"` // absolute dir to deliver into; empty = EXPORT_DIR fallback chain
	Seed        *int64        `json:"seed"`           // required
	PaletteLock bool          `json:"palette_lock,omitempty"`
	Assets      []AssetSpecIn `json:"assets"` // 1..8
}

type AssetSpecIn struct {
	Kind         string   `json:"kind"` // "element" | "kit" | "atlas" | "frame" | "bar" | "npc"
	Name         string   `json:"name"`
	Prompt       string   `json:"prompt"`
	Negative     string   `json:"negative,omitempty"`
	Size         string   `json:"size"`                    // WxH, multiples of 16
	Frames       int      `json:"frames,omitempty"`        // atlas: 2..64
	Columns      int      `json:"columns,omitempty"`       // atlas: 0 = single row
	Transparent  bool     `json:"transparent,omitempty"`   // element: white+black double pass
	FrameMargins int      `json:"frame_margins,omitempty"` // frame|bar: 1..size/2-1
	BarFill      string   `json:"bar_fill,omitempty"`      // bar: fill art prompt
	FillStates   []int    `json:"fill_states,omitempty"`   // bar: 1..100, <=16, ascending
	Shape        string   `json:"shape,omitempty"`         // npc: "variants" | "walk4"
	Variants     []string `json:"variants,omitempty"`      // npc variants: per-variant prompt suffixes
	MetaFormats  []string `json:"meta_formats,omitempty"`  // engine metadata: subset of {phaser, unity, sparrow, tiled}
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
	spec := composeSpec{Kind: "asset-set", Name: setName, Dest: req.Dest, PaletteLock: req.PaletteLock}
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
		// meta_formats (A2): only the format-name subset is queue-side
		// validation. Kind-compat lives in compose.EmitMeta — a valid name on
		// an unserving kind submits and surfaces as the asset's compose-time
		// broken error, mirroring every other compose asset failure.
		if len(a.MetaFormats) > 0 {
			valid := map[string]bool{"phaser": true, "unity": true, "sparrow": true, "tiled": true}
			for _, f := range a.MetaFormats {
				if !valid[f] {
					return "", &ValidationError{Code: "validation_error",
						Msg: fmt.Sprintf("asset %q: meta_formats: format %q unknown (valid: phaser, unity, sparrow, tiled)", an, f)}
				}
			}
		}
		var built []SubmitRequest
		var as assetSpec
		switch a.Kind {
		case "element":
			built = elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Transparent: a.Transparent, MetaFormats: a.MetaFormats}
		case "kit":
			built = kitChildren(prompt, a.Negative, a.Size, *req.Seed)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, MetaFormats: a.MetaFormats}
		case "atlas":
			if a.Frames < 2 || a.Frames > 64 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frames must be 2..64", an)}
			}
			built = atlasChildren(prompt, a.Negative, a.Size, *req.Seed, a.Frames)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Frames: a.Frames, Columns: a.Columns, MetaFormats: a.MetaFormats}
		case "frame":
			if a.FrameMargins < 1 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins is required for frames", an)}
			}
			w, h, serr := parseAssetSize(a.Size)
			if serr != nil || 2*a.FrameMargins >= w || 2*a.FrameMargins >= h {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins must be 1..size/2-1", an)}
			}
			built = elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Transparent: a.Transparent, FrameMargins: a.FrameMargins, MetaFormats: a.MetaFormats}
		case "bar":
			if a.BarFill == "" {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: bar_fill is required for bars", an)}
			}
			if a.FrameMargins < 1 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins is required for bars", an)}
			}
			w, h, serr := parseAssetSize(a.Size)
			if serr != nil || 2*a.FrameMargins >= w || 2*a.FrameMargins >= h {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: frame_margins must be 1..size/2-1", an)}
			}
			states := a.FillStates
			if len(states) == 0 {
				states = []int{25, 50, 75, 100}
			}
			seenSt := map[int]bool{}
			uniq := []int{}
			for _, st := range states {
				if st < 1 || st > 100 {
					return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: fill_states values must be 1..100", an)}
				}
				if seenSt[st] {
					continue
				}
				seenSt[st] = true
				uniq = append(uniq, st)
			}
			if len(uniq) > 16 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: fill_states takes at most 16 states", an)}
			}
			for i := 1; i < len(uniq); i++ {
				if uniq[i] <= uniq[i-1] {
					return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: fill_states must be ascending", an)}
				}
			}
			built = append(elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent),
				elementChildren(a.BarFill, a.Negative, a.Size, *req.Seed, false)...)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Transparent: a.Transparent, FrameMargins: a.FrameMargins, BarFill: a.BarFill, FillStates: uniq, MetaFormats: a.MetaFormats}
		case "npc":
			switch a.Shape {
			case "variants":
				if len(a.Variants) < 2 || len(a.Variants) > 64 {
					return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: npc variants takes 2..64 suffixes", an)}
				}
				for i, v := range a.Variants {
					if strings.TrimSpace(v) == "" {
						return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: npc variant %d is empty", an, i)}
					}
				}
			case "walk4":
				// Fixed direction suffixes; any provided variants list is ignored.
			default:
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: npc shape must be variants or walk4", an)}
			}
			built = npcChildren(prompt, a.Negative, a.Size, *req.Seed, a.Shape, a.Variants)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Shape: a.Shape, Variants: a.Variants, Columns: a.Columns, MetaFormats: a.MetaFormats}
		default:
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: unknown kind %q (want element, kit, atlas, frame, bar, npc)", an, a.Kind)}
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

// npcChildren builds one job per npc variant, ALL at the pinned set seed —
// no +i increment. Same composition scaffold, per-variant content: the
// variant-family mechanic (walk4 gets the three fixed direction suffixes;
// left is mirrored downstream, never rendered).
func npcChildren(prompt, negative, size string, seed int64, shape string, variants []string) []SubmitRequest {
	suffixes := variants
	if shape == "walk4" {
		suffixes = []string{
			"full front view, facing the viewer",
			"rear view, facing away from the viewer",
			"side profile view, facing right",
		}
	}
	kids := make([]SubmitRequest, 0, len(suffixes))
	for _, v := range suffixes {
		s := seed
		kids = append(kids, SubmitRequest{Mode: "asset", Prompt: prompt + ", " + v, NegativePrompt: negative, Size: size, Seed: &s})
	}
	return kids
}

// parseAssetSize parses a size that already passed validAssetSize ("WxH",
// multiples of 16) into pixel ints, for post-processing bounds checks.
func parseAssetSize(size string) (w, h int, err error) {
	ws, hs, ok := strings.Cut(size, "x")
	if !ok {
		return 0, 0, fmt.Errorf("invalid size %q", size)
	}
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("invalid size %q", size)
	}
	return w, h, nil
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
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Prompt   string   `json:"prompt,omitempty"`
	Size     string   `json:"size,omitempty"`
	Variants []string `json:"variants,omitempty"`
	Files    []string `json:"files,omitempty"`
	Error    string   `json:"error,omitempty"`
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
	outsByAsset := make([][]compose.OutputFile, len(spec.Assets))
	for ai, a := range spec.Assets {
		ma := manifestAsset{Kind: a.Kind, Name: a.Name, Prompt: a.Prompt, Size: a.Size, Variants: a.Variants}
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
			case "frame":
				black := []byte(nil)
				if len(imgs) == 2 {
					black = imgs[1]
				}
				outs, err = compose.ComposeFrame(a.Name, imgs[0], black, a.Transparent && len(imgs) == 2, a.FrameMargins)
			case "bar":
				fill := imgs[len(imgs)-1]
				trackBlack := []byte(nil)
				if len(imgs) == 3 {
					trackBlack = imgs[1]
				}
				outs, err = compose.ComposeBar(a.Name, imgs[0], trackBlack, a.Transparent && len(imgs) == 3, a.FrameMargins, fill, a.FillStates)
			case "npc":
				if a.Shape == "walk4" {
					outs, err = compose.ComposeNPCWalk4(a.Name, imgs[0], imgs[1], imgs[2])
				} else {
					outs, err = compose.ComposeAtlas(a.Name, imgs, a.Columns)
				}
			default:
				err = fmt.Errorf("unknown asset kind %q", a.Kind)
			}
			// Engine-metadata emission (A2): opt-in via meta_formats, running
			// right after per-kind composition so failures read exactly like
			// compose failures — the reason breaks the asset (its own composed
			// outputs are dropped with it, whole-asset delivery), lands in the
			// manifest, and the set's partial-success rule applies unchanged.
			if err == nil && len(a.MetaFormats) > 0 {
				metas, merr := compose.EmitMeta(a.Kind, a.Name, a.MetaFormats, outs)
				if merr == nil {
					outs = append(outs, metas...)
				} else {
					err = merr
				}
			}
			if err == nil {
				outsByAsset[ai] = outs
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

	// Palette lock (F1): remap every PNG output of the set onto one shared
	// palette before anything is saved, then ship the palette file. Runs
	// after per-kind composition (the outputs exist as PNGs) and before
	// SaveSetFile, so every deliverable inherits the set palette. Non-image
	// outputs (atlas/frame/bar JSON) pass through untouched.
	var palJSON []byte
	if spec.PaletteLock && ok > 0 {
		var imgs []image.Image
		for ai := range outsByAsset {
			for _, of := range outsByAsset[ai] {
				if !strings.HasSuffix(of.Name, ".png") {
					continue
				}
				im, derr := png.Decode(bytes.NewReader(of.Data))
				if derr != nil {
					// Composed outputs are our own fresh encodes; this cannot
					// happen outside corrupted pipeline state. Leave the file
					// unremapped and log — remap never fails the set.
					m.log(id, "palette lock: %s undecodable, left unremapped: %v", of.Name, derr)
					continue
				}
				imgs = append(imgs, im)
			}
		}
		pal := compose.SetPalette(imgs, 255)
		if len(pal) > 0 {
			for ai := range outsByAsset {
				for j := range outsByAsset[ai] {
					of := &outsByAsset[ai][j]
					if !strings.HasSuffix(of.Name, ".png") {
						continue
					}
					nb, rerr := compose.RemapPNG(of.Data, pal)
					if rerr != nil {
						m.log(id, "palette lock: %s left unremapped: %v", of.Name, rerr)
						continue
					}
					of.Data = nb
				}
			}
			if pj, jerr := compose.MarshalPalette(pal); jerr != nil {
				m.log(id, "palette lock: palette file: %v", jerr)
			} else {
				palJSON = pj
			}
		}
	}

	// Compose-level params (A3): every composed PNG deliverable carries the
	// set identity (kind/name/set); the child jobs' own renders already carry
	// their render params on the child images. Runs after the palette pass
	// (RemapPNG re-encodes and would strip tEXt chunks) and best-effort: an
	// embed failure leaves the bytes unchanged. GIF/WebP outputs are not
	// PNG — no embed path for them.
	for ai := range outsByAsset {
		a := spec.Assets[ai]
		for j := range outsByAsset[ai] {
			of := &outsByAsset[ai][j]
			if strings.HasSuffix(of.Name, ".png") {
				of.Data = m.withRenderParams(of.Data, map[string]string{
					"kind": a.Kind, "name": a.Name, "set": id,
				})
			}
		}
	}

	// Save phase: every composed output lands here, after the palette pass.
	// A save failure fails the parent immediately (the composeGIF precedent)
	// instead of the old per-asset break-marking — storage save failures were
	// always catastrophic disk-level events.
	for ai := range outsByAsset {
		for _, of := range outsByAsset[ai] {
			if serr := m.opts.Store.SaveSetFile(id, of.Name, of.Data); serr != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: %v", serr))
				return
			}
			files = append(files, storage.FileRef{Name: of.Name, Primary: of.Primary})
			man.Assets[ai].Files = append(man.Assets[ai].Files, of.Name)
		}
	}
	if len(palJSON) > 0 {
		pname := spec.Name + ".palette.json"
		if serr := m.opts.Store.SaveSetFile(id, pname, palJSON); serr != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: %v", serr))
			return
		}
		files = append(files, storage.FileRef{Name: pname})
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
	// Deliver on every success. Explicit dest keeps the flat documented
	// contract: delivered names are semantic project files (the manifest
	// ships as <name>.manifest.json, so per-name manifests coexist in one
	// dest). An unset dest inherits the export fallback chain (dest →
	// EXPORT_DIR → ~/Downloads/img-gen), and that root is SHARED by every
	// fallback set — identically named deliverables would still collide, so
	// fallback deliveries namespace by set id; the resolved root never comes
	// up empty, so there is no "no delivery" case.
	dest := m.resolveExportDest(spec.Dest)
	if spec.Dest == "" {
		dest = filepath.Join(dest, id)
	}
	served, err := m.opts.Store.DeliverSet(id, spec.Name, dest)
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

// --- public gif submission -------------------------------------------------

const (
	defaultGifDelayMS = 100
	minGifDelayMS     = 10
	maxGifDelayMS     = 5000
)

// GifRequest builds an animated GIF. Four source modes, all real:
// from_frames (assemble given images), from_spec (generate the frames here),
// composite (statics blitted over every frame of an animation), keyframes
// (interpolate between 2..16 anchors on the RIFE sidecar — RIFE_URL).
type GifRequest struct {
	Name    string    `json:"name"`
	Dest    string    `json:"dest,omitempty"`
	DelayMS int       `json:"delay_ms,omitempty"` // 10..5000; 0 = default 100
	Loop    int       `json:"loop,omitempty"`     // >= 0; 0 = infinite loop
	Formats []string  `json:"formats,omitempty"`  // subset of {gif,webp}; default ["gif"]
	Source  GifSource `json:"source"`
}

type GifSource struct {
	Type      string           `json:"type"`              // from_frames | from_spec | composite | keyframes
	Frames    []FrameRef       `json:"frames,omitempty"`  // from_frames: 2..64 sources
	Request   *SubmitRequest   `json:"request,omitempty"` // from_spec: base asset request
	Count     int              `json:"count,omitempty"`   // from_spec: 2..64 frames
	Composite *CompositeSource `json:"composite,omitempty"`
	Keyframes *KeyframesSource `json:"keyframes,omitempty"` // 2..16 anchors on the RIFE sidecar
}

// KeyframesSource is the RIFE interpolation source: 2..16 anchor frame refs
// with an optional multiplier (default 3, allowed 2..10).
type KeyframesSource struct {
	Anchors    []FrameRef `json:"anchors"`
	Multiplier int        `json:"multiplier,omitempty"`
}

// keyframesPayload persists a keyframes gif source for restart recovery:
// the resolved anchors live as set-relative files, mirroring gp.Frames.
type keyframesPayload struct {
	Files      []string `json:"files"`
	Multiplier int      `json:"multiplier"`
}

// CompositeSource layers one animated frame list (the base canvas) under
// static RGBA elements composited in list order, pre-quantization.
type CompositeSource struct {
	Anim    []FrameRef    `json:"anim"`              // 2..64 frame refs — the base canvas
	Statics []StaticLayer `json:"statics,omitempty"` // 0..8
}

// StaticLayer places one static element: From is any frame ref form, Pos is
// the x,y origin on the anim canvas — the overlay must fit inside it.
type StaticLayer struct {
	From FrameRef `json:"from"`
	Pos  [2]int   `json:"pos"`
}

// FrameRef is one source frame: exactly one of Job (an img-gen job id),
// Path (data-dir-relative, no "..", no absolute paths), B64 (inline PNG),
// or the pair Set (storage set id) + File (set file name).
type FrameRef struct {
	Job  string `json:"job,omitempty"`
	Path string `json:"path,omitempty"`
	B64  string `json:"b64,omitempty"`
	Set  string `json:"set,omitempty"`
	File string `json:"file,omitempty"`
}

// resolveFrameRef fetches the PNG bytes for a source frame. Decodability is
// checked here so bad sources fail the submit, not the compose.
func (m *Manager) resolveFrameRef(ref FrameRef) ([]byte, error) {
	set := 0
	for _, s := range []string{ref.Job, ref.Path, ref.B64} {
		if s != "" {
			set++
		}
	}
	switch {
	case ref.Set != "" || ref.File != "":
		if ref.Set == "" || ref.File == "" {
			return nil, fmt.Errorf("set frame source needs both set and file")
		}
		if set != 0 {
			return nil, fmt.Errorf("frame source needs exactly one of job, path, b64, set+file")
		}
		data, serr := m.opts.Store.ReadSetFile(ref.Set, ref.File)
		if serr != nil {
			return nil, serr
		}
		if _, derr := png.Decode(bytes.NewReader(data)); derr != nil {
			return nil, fmt.Errorf("frame is not a decodable PNG: %v", derr)
		}
		return data, nil
	case set != 1:
		return nil, fmt.Errorf("frame source needs exactly one of job, path, b64, set+file")
	}
	var data []byte
	var err error
	switch {
	case ref.Job != "":
		data, err = m.opts.Store.ReadImage(ref.Job)
	case ref.Path != "":
		if filepath.IsAbs(ref.Path) || strings.Contains(ref.Path, "..") {
			return nil, fmt.Errorf("path source must be relative to the data directory without '..'")
		}
		data, err = m.opts.Store.ReadDataFile(ref.Path)
	default:
		data, err = base64.StdEncoding.DecodeString(ref.B64)
	}
	if err != nil {
		return nil, err
	}
	if _, derr := png.Decode(bytes.NewReader(data)); derr != nil {
		return nil, fmt.Errorf("frame is not a decodable PNG: %v", derr)
	}
	return data, nil
}

// SubmitGIF registers the compose parent and queues frame generation when
// the source is from_spec. from_frames, composite, and keyframes resolve and
// persist every source into the set at submit time (source-frame-%04d.png;
// keyframes anchors as keyframe-%04d.png; composite statics additionally as
// static-layer-%02d.png with their positions kept in the persisted spec) so a
// restart can recompose without re-fetching.
func (m *Manager) SubmitGIF(req GifRequest) (string, error) {
	name := validComposeAssetName(req.Name)
	if name == "" {
		return "", &ValidationError{Code: "validation_error", Msg: "name is required for gifs"}
	}
	delay := req.DelayMS
	if delay == 0 {
		delay = defaultGifDelayMS
	}
	if delay < minGifDelayMS || delay > maxGifDelayMS {
		return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("delay_ms must be %d..%d (0 = default %d)", minGifDelayMS, maxGifDelayMS, defaultGifDelayMS)}
	}
	if req.Loop < 0 {
		return "", &ValidationError{Code: "validation_error", Msg: "loop must be >= 0 (0 = infinite)"}
	}
	formats := req.Formats
	if len(formats) == 0 {
		formats = []string{"gif"}
	}
	for _, f := range formats {
		if f != "gif" && f != "webp" {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("formats must be a subset of [gif, webp], got %q", f)}
		}
	}
	if req.Dest != "" && !filepath.IsAbs(req.Dest) {
		return "", &ValidationError{Code: "validation_error", Msg: "dest must be an absolute path"}
	}
	spec := composeSpec{Kind: "gif", Name: name, Dest: req.Dest, GIF: &gifPayload{DelayMS: delay, Loop: req.Loop, Formats: formats}}
	switch req.Source.Type {
	case "keyframes":
		if m.opts.Ops.Rife == nil {
			return "", &ValidationError{Code: "validation_error", Msg: "keyframes requires the RIFE sidecar (RIFE_URL)"}
		}
		if req.Source.Keyframes == nil {
			return "", &ValidationError{Code: "validation_error", Msg: "keyframes source requires anchors"}
		}
		kfs := req.Source.Keyframes
		n := len(kfs.Anchors)
		if n < 2 || n > 16 {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("keyframes anchors takes 2..16, got %d", n)}
		}
		mult := kfs.Multiplier
		if mult == 0 {
			mult = 3
		}
		if mult < 2 || mult > 10 {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("keyframes multiplier must be 2..10, got %d", mult)}
		}
		if total := (n-1)*mult + 1; total > 64 {
			// Largest multiplier satisfying (n-1)*m+1 <= 64 is 63/(n-1)
			// (n=16 → 4), clamped to the allowed 2..10 range (small n).
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf(
				"keyframes would produce %d frames, cap is 64 — use multiplier ≤ %d or fewer anchors", total, min(10, 63/(n-1)))}
		}
		id := newID()
		names := make([]string, 0, len(kfs.Anchors))
		for i, ref := range kfs.Anchors {
			data, err := m.resolveFrameRef(ref)
			if err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("frame %d: %v", i, err)}
			}
			fname := fmt.Sprintf("keyframe-%04d.png", i)
			if err := m.opts.Store.SaveSetFile(id, fname, data); err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("frame %d: %v", i, err)}
			}
			names = append(names, fname)
		}
		spec.GIF.Keyframes = &keyframesPayload{Files: names, Multiplier: mult}
		return m.submitComposeParent(id, spec, nil)
	case "from_frames":
		if len(req.Source.Frames) < 2 || len(req.Source.Frames) > 64 {
			return "", &ValidationError{Code: "validation_error", Msg: "from_frames takes 2..64 frames"}
		}
		id := newID()
		names := make([]string, 0, len(req.Source.Frames))
		for i, ref := range req.Source.Frames {
			data, err := m.resolveFrameRef(ref)
			if err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("frame %d: %v", i, err)}
			}
			fname := fmt.Sprintf("source-frame-%04d.png", i)
			if err := m.opts.Store.SaveSetFile(id, fname, data); err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("frame %d: %v", i, err)}
			}
			names = append(names, fname)
		}
		spec.GIF.Frames = names
		return m.submitComposeParent(id, spec, nil)
	case "from_spec":
		base := req.Source.Request
		if base == nil {
			return "", &ValidationError{Code: "validation_error", Msg: "from_spec needs source.request"}
		}
		if base.Seed == nil {
			return "", &ValidationError{Code: "validation_error", Msg: "seed is required for generated gif frames"}
		}
		if strings.TrimSpace(base.Prompt) == "" {
			return "", &ValidationError{Code: "validation_error", Msg: "prompt is required for generated gif frames"}
		}
		if !validAssetSize(base.Size) {
			return "", &ValidationError{Code: "invalid_size", Msg: fmt.Sprintf("invalid size %q for gif frames (want WxH, multiples of 16)", base.Size)}
		}
		if req.Source.Count < 2 || req.Source.Count > 64 {
			return "", &ValidationError{Code: "validation_error", Msg: "gif frames must be 2..64"}
		}
		return m.submitComposeParent(newID(), spec, specFrames(*base, req.Source.Count))
	case "composite":
		cs := req.Source.Composite
		if cs == nil {
			return "", &ValidationError{Code: "validation_error", Msg: "composite source needs source.composite"}
		}
		if len(cs.Anim) < 2 || len(cs.Anim) > 64 {
			return "", &ValidationError{Code: "validation_error", Msg: "composite anim takes 2..64 frames"}
		}
		if len(cs.Statics) > maxComposeStatics {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("composite takes at most %d static layers", maxComposeStatics)}
		}
		id := newID()
		names := make([]string, 0, len(cs.Anim))
		for i, ref := range cs.Anim {
			data, err := m.resolveFrameRef(ref)
			if err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("anim frame %d: %v", i, err)}
			}
			fname := fmt.Sprintf("source-frame-%04d.png", i)
			if err := m.opts.Store.SaveSetFile(id, fname, data); err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("anim frame %d: %v", i, err)}
			}
			names = append(names, fname)
		}
		statics := make([]compositeLayerSpec, 0, len(cs.Statics))
		for i, l := range cs.Statics {
			if l.Pos[0] < 0 || l.Pos[1] < 0 {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("static layer %d: pos must be non-negative", i)}
			}
			data, err := m.resolveFrameRef(l.From)
			if err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("static layer %d: %v", i, err)}
			}
			sname := fmt.Sprintf("static-layer-%02d.png", i)
			if err := m.opts.Store.SaveSetFile(id, sname, data); err != nil {
				m.opts.Store.RemoveSet(id)
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("static layer %d: %v", i, err)}
			}
			statics = append(statics, compositeLayerSpec{File: sname, Pos: l.Pos})
		}
		spec.GIF.Frames = names
		spec.GIF.Composite = statics
		return m.submitComposeParent(id, spec, nil)
	default:
		return "", &ValidationError{Code: "validation_error", Msg: "source.type must be from_frames, from_spec, composite, or keyframes"}
	}
}

// specFrames mirrors atlasChildren over an arbitrary base request: one
// asset-mode job per frame, seed+i, frame index appended for consistency.
func specFrames(base SubmitRequest, count int) []SubmitRequest {
	kids := make([]SubmitRequest, 0, count)
	for i := 0; i < count; i++ {
		c := base
		s := *base.Seed + int64(i)
		c.Seed = &s
		c.Mode = "asset"
		c.Prompt = fmt.Sprintf("%s, animation frame %d of %d, consistent style and lighting", strings.TrimSpace(c.Prompt), i+1, count)
		kids = append(kids, c)
	}
	return kids
}

// composeGIF assembles the set's GIF. Unlike asset sets a GIF is one
// artifact: every frame must be present, so any failed child fails the
// parent. webp unavailability is recorded, not fatal.
func (m *Manager) composeGIF(id string, spec *composeSpec, children []string) {
	gp := spec.GIF
	if gp == nil {
		// A corrupted history record can rehydrate a gif parent whose spec
		// parsed without a gif block; failing here is the "neither frames nor
		// children" precedent, not a panic inside the worker goroutine.
		m.finish(id, "failed", "compose_error: gif spec missing gif block")
		return
	}
	man := setManifest{SetID: id, Kind: "gif", Name: spec.Name, CreatedAt: time.Now(),
		GIF: &manifestGIF{Name: spec.Name, DelayMS: gp.DelayMS, Loop: gp.Loop, Formats: gp.Formats}}
	var frames []image.Image
	switch {
	case gp.Keyframes != nil:
		// Keyframes interpolation (Task 5): the persisted anchors go to the
		// RIFE sidecar, whose returned frames replace the anchor list
		// wholesale before the shared encoder. ctx is Background because
		// composition runs no other cancellable calls — cancelling a
		// mid-compose keyframes parent is not modeled today.
		if m.opts.Ops.Rife == nil {
			// A restart can rehydrate a keyframes spec into a manager built
			// without RIFE_URL: fail the parent honestly, never panic.
			m.finish(id, "failed", "compose_error: rife: sidecar not wired")
			return
		}
		ctx := context.Background()
		raws := make([][]byte, len(gp.Keyframes.Files))
		for i, name := range gp.Keyframes.Files {
			data, err := m.opts.Store.ReadSetFile(id, name)
			if err != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: keyframe %d: %v", i, err))
				return
			}
			raws[i] = data
		}
		interp, rerr := m.opts.Ops.Rife(ctx, raws, gp.Keyframes.Multiplier)
		if rerr != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: rife: %v", rerr))
			return
		}
		if len(interp) == 0 {
			m.finish(id, "failed", "compose_error: rife returned no frames")
			return
		}
		for i, raw := range interp {
			im, derr := png.Decode(bytes.NewReader(raw))
			if derr != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: rife frame %d: %v", i, derr))
				return
			}
			frames = append(frames, im)
		}
	case len(gp.Frames) > 0:
		for i, name := range gp.Frames {
			data, err := m.opts.Store.ReadSetFile(id, name)
			if err != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: source frame %d: %v", i, err))
				return
			}
			im, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: source frame %d: %v", i, err))
				return
			}
			frames = append(frames, im)
		}
	case len(children) > 0:
		imgs, errs := m.childrenResults(len(children), children, func(f string, a ...any) { m.log(id, f, a...) })
		if len(imgs) == 0 {
			m.finish(id, "failed", "compose_error: gif frames: "+errs[0])
			return
		}
		for i, img := range imgs {
			if img == nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: gif frame %d incomplete: %s", i, strings.Join(errs, "; ")))
				return
			}
			im, err := png.Decode(bytes.NewReader(img))
			if err != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: gif frame %d: %v", i, err))
				return
			}
			frames = append(frames, im)
		}
	default:
		m.finish(id, "failed", "compose_error: gif spec has neither frames nor children")
		return
	}
	// Composite statics (F5): blit each persisted static layer over every
	// frame BEFORE quantization — 1-bit GIF alpha stays halo-free and the
	// palette covers overlay colors too. Each layer is one PNG read plus one
	// decode; the blit itself runs per frame. An overhang (or any layer
	// failure) fails the parent naming the file — never a silent crop.
	for _, l := range gp.Composite {
		data, err := m.opts.Store.ReadSetFile(id, l.File)
		if err != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: static layer %s: %v", l.File, err))
			return
		}
		ov, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: static layer %s: %v", l.File, err))
			return
		}
		for i := range frames {
			out, berr := compose.BlitImage(frames[i], ov, l.Pos[0], l.Pos[1])
			if berr != nil {
				m.finish(id, "failed", fmt.Sprintf("compose_error: static layer %s: %v", l.File, berr))
				return
			}
			frames[i] = out
		}
	}
	outs, _, webpStatus, err := compose.ComposeGIF(spec.Name, frames, gp.DelayMS, gp.Loop, gp.Formats)
	if err != nil {
		m.finish(id, "failed", fmt.Sprintf("compose_error: %v", err))
		return
	}
	man.GIF.WebP = webpStatus
	var files []storage.FileRef
	for _, of := range outs {
		if err := m.opts.Store.SaveSetFile(id, of.Name, of.Data); err != nil {
			m.finish(id, "failed", fmt.Sprintf("compose_error: %v", err))
			return
		}
		files = append(files, storage.FileRef{Name: of.Name, Primary: of.Primary})
		man.GIF.Files = append(man.GIF.Files, of.Name)
	}
	m.mu.Lock()
	job := m.jobs[id]
	if job != nil {
		job.Files = files
		job.ImagePath = "sets/" + id + "/" + primaryName(files)
	}
	m.mu.Unlock()
	m.saveManifest(id, &man)
	// Deliver on every success. Explicit dest keeps the flat documented
	// contract: delivered names are semantic project files (the manifest
	// ships as <name>.manifest.json, so per-name manifests coexist in one
	// dest). An unset dest inherits the export fallback chain (dest →
	// EXPORT_DIR → ~/Downloads/img-gen), and that root is SHARED by every
	// fallback set — identically named deliverables would still collide, so
	// fallback deliveries namespace by set id; the resolved root never comes
	// up empty, so there is no "no delivery" case.
	dest := m.resolveExportDest(spec.Dest)
	if spec.Dest == "" {
		dest = filepath.Join(dest, id)
	}
	served, err := m.opts.Store.DeliverSet(id, spec.Name, dest)
	if err != nil {
		m.finish(id, "failed", fmt.Sprintf("compose_error: %v", err))
		return
	}
	man.Delivered = served
	man.Dest = dest
	m.saveManifest(id, &man)
	m.log(id, "compose done: gif %s (%d frames)", spec.Name, len(frames))
	m.finish(id, "done", "")
}
