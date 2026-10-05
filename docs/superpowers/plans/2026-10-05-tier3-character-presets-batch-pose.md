# Tier 3: Character Presets + Batch + Pose — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add three capabilities to img-gen — one-click character presets, N-image batch generation, and ControlNet-style pose (edge-guided) composition.

**Architecture:** Presets and batch are pure img-gen additions (a new `presets` catalog resolved server-side in the queue, plus a `Batch` field fanning out to N jobs). Pose is a new mflux ControlNet endpoint on the existing sidecar, surfaced as a new "Pose" mode. All three follow the established pattern: catalog → `internal/*` package → `queue.Submit` → `/api/*` endpoint → frontend mode/control.

**Tech Stack:** Go (stdlib net/http), vanilla JS/HTML/CSS, the mflux sidecar (stdlib Python shelling out to `mflux-*` CLI binaries on M6).

**Spec:** `docs/superpowers/specs/2026-10-05-img-edit-and-multi-reference-design.md` (mode + ImageOps conventions) and `docs/superpowers/specs/2026-10-05-model-lora-selection-and-history-design.md` (catalog + ModelSpec + history conventions). Tier-2 negative-prompt/outpaint work established the mode/endpoint pattern this plan extends.

## Global Constraints

- Go stdlib only (no external deps beyond `img-gen/internal/*`); module `img-gen`, build with `go build -o bin/img-gen ./cmd/img-gen`, test with `go test ./...`.
- **No real usernames/hostnames/IPs in tracked files.** Catalog templates ship as `*.json.example` with `igor`/paths; real `models.json`/`presets.json` are gitignored. The M6 sidecar IP is passed at install time (`IMAGE_URL=... ./deploy/install-macos.sh`), never committed.
- Modes are validated server-side in `queue.Submit`; every new mode gets a `case` in that switch and an `ImageOps` func.
- The sidecar is **stdlib-only Python** (system `/usr/bin/python3`); it shells out to mflux CLIs. New endpoints follow `/generate`'s single-flight + b64-in/b64-out shape.
- mflux LoRA/ControlNet format constraint: BFL/Diffusers/LoKR OK, **XLabs not**; ControlNet built-ins are Canny/Depth (see pose spike, Task 6).

---

### Task 1: Presets catalog package

**Files:**
- Create: `internal/presets/presets.go`
- Create: `internal/presets/presets_test.go`
- Create: `presets.json.example`
- Modify: `.gitignore`

**Interfaces:**
- Produces: `presets.Catalog` (with `Load(path)` and `ByKey(key)`), `presets.Preset` — consumed by Task 2.

- [ ] **Step 1: Write the failing test**

```go
// internal/presets/presets_test.go
package presets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndByKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "presets.json")
	raw := `{"presets":[{"key":"aria","label":"Aria","trigger":"aria, silver hair","model":"/m/persephone","loras":[{"name":"alvdansen/illustration-1.0-flux-dev","scale":0.8}],"negative_prompt":"blurry","steps":24,"guidance":3.5}]}`
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := c.ByKey("aria")
	if !ok {
		t.Fatal("aria not found")
	}
	if a.Trigger != "aria, silver hair" || a.Model != "/m/persephone" || len(a.Loras) != 1 {
		t.Fatalf("aria = %+v", a)
	}
	if a.Steps == nil || *a.Steps != 24 || a.Guidance == nil || *a.Guidance != 3.5 {
		t.Fatalf("aria sampling = %+v / %+v", a.Steps, a.Guidance)
	}
	if _, ok := c.ByKey("missing"); ok {
		t.Fatal("missing should not resolve")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("want error for missing file")
	}
}
```

- [ ] **Step 2: Run it, confirm it fails** — `go test ./internal/presets/` → FAIL (package not defined).

- [ ] **Step 3: Implement**

```go
// internal/presets/presets.go
// Package presets loads the character-preset catalog (presets.json).
package presets

import (
	"encoding/json"
	"os"

	"img-gen/internal/storage"
)

type Preset struct {
	Key            string            `json:"key"`
	Label          string            `json:"label"`
	Trigger        string            `json:"trigger"`         // prepended to the prompt (character identity)
	Model          string            `json:"model,omitempty"` // mflux --model; "" = sidecar default
	Loras          []storage.LoraRef `json:"loras,omitempty"`
	Style          string            `json:"style,omitempty"` // global style key; "" = none
	NegativePrompt string            `json:"negative_prompt,omitempty"`
	Steps          *int              `json:"steps,omitempty"`
	Guidance       *float64          `json:"guidance,omitempty"`
}

type Catalog struct {
	Presets []Preset `json:"presets"`
}

func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Catalog) ByKey(key string) (Preset, bool) {
	for _, p := range c.Presets {
		if p.Key == key {
			return p, true
		}
	}
	return Preset{}, false
}
```

- [ ] **Step 4: Run tests** — `go test ./internal/presets/` → PASS.

- [ ] **Step 5: Add the example + gitignore, commit**

`presets.json.example` (tracked template, placeholder paths only):
```json
{
  "presets": [
    {
      "key": "aria",
      "label": "Aria",
      "trigger": "aria, long silver hair, violet eyes",
      "model": "/Users/<you>/mflux-models/persephone-4bit",
      "loras": [
        { "name": "alvdansen/illustration-1.0-flux-dev", "scale": 0.8 }
      ],
      "negative_prompt": "blurry, deformed hands",
      "steps": 24,
      "guidance": 3.5
    }
  ]
}
```
`.gitignore`: append `/presets.json` (keep `models.json` already there).

```bash
git add internal/presets/presets.go internal/presets/presets_test.go presets.json.example .gitignore
git commit -m "feat(presets): character-preset catalog package"
```

---

### Task 2: Resolve presets in the queue

**Files:**
- Modify: `internal/queue/queue.go` (`SubmitRequest`, `Options`, `Submit`, `run`)
- Modify: `internal/storage/storage.go` (`Job`)
- Modify: `cmd/img-gen/main.go` (wire catalog into `Options`)
- Test: `internal/queue/queue_test.go`

**Interfaces:**
- Consumes: `presets.Catalog` / `presets.Preset` from Task 1.
- Produces: `SubmitRequest.Preset string`, `storage.Job.Preset string`, `Options.Presets *presets.Catalog` — consumed by Task 3.

- [ ] **Step 1: Extend storage** — add `Preset string \`json:"preset,omitempty"\`` to `storage.Job` (next to `Style`).

- [ ] **Step 2: Extend SubmitRequest + Options** — in `queue.go` add `Preset string \`json:"preset"\` // character-preset key; "" = none` to `SubmitRequest`, and `Presets *presets.Catalog` to `Options`.

- [ ] **Step 3: Write the failing test**

```go
// internal/queue/queue_test.go — append
func TestSubmitResolvesPresetDefaults(t *testing.T) {
	// build a catalog with one preset "aria" (trigger + model + lora + negative)
	pc := &presets.Catalog{Presets: []presets.Preset{{
		Key: "aria", Trigger: "aria, silver hair", Model: "/m/persephone",
		Loras: []storage.LoraRef{{Name: "alvdansen/illustration-1.0-flux-dev", Scale: 0.8}},
		NegativePrompt: "blurry",
	}}}
	m := newTestManager(t) // reuse existing helper; set m.opts.Presets = pc
	m.opts.Presets = pc

	id, err := m.Submit(SubmitRequest{
		Genre: "portrait", Fields: map[string]string{"subject": "standing"},
		Size: "512x512", Preset: "aria",
	})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := m.Get(id)
	if j.Model != "/m/persephone" || len(j.Loras) != 1 || j.NegativePrompt != "blurry" {
		t.Fatalf("preset defaults not applied: %+v", j)
	}
	if j.Preset != "aria" {
		t.Fatalf("preset not recorded: %+v", j.Preset)
	}
}

func TestSubmitRejectsUnknownPreset(t *testing.T) {
	m := newTestManager(t)
	m.opts.Presets = &presets.Catalog{}
	if _, err := m.Submit(SubmitRequest{Genre: "portrait", Fields: map[string]string{"subject": "x"}, Size: "512x512", Preset: "nope"}); err == nil {
		t.Fatal("want error for unknown preset")
	}
}
```

- [ ] **Step 4: Run it** — `go test ./internal/queue/ -run Preset` → FAIL (Preset unresolved).

- [ ] **Step 5: Implement resolution** — in `queue.Submit`, after the mode switch and style check, add:

```go
var preset presets.Preset
if req.Preset != "" {
	var ok bool
	preset, ok = m.opts.Presets.ByKey(req.Preset)
	if !ok {
		return "", fmt.Errorf("unknown preset %q", req.Preset)
	}
	// Preset supplies defaults; an explicit request field wins.
	if req.Model == "" {
		req.Model = preset.Model
	}
	if len(req.Loras) == 0 {
		req.Loras = preset.Loras
	}
	if req.NegativePrompt == "" {
		req.NegativePrompt = preset.NegativePrompt
	}
	if req.Steps == nil {
		req.Steps = preset.Steps
	}
	if req.Guidance == nil {
		req.Guidance = preset.Guidance
	}
	if req.Style == "" {
		req.Style = preset.Style
	}
}
```

Then in the `job := &storage.Job{...}` literal add `Preset: req.Preset,`.

- [ ] **Step 6: Apply the trigger in `run`** — directly after the existing `if job.Style != "" { ... }` block in `run()`:

```go
if job.Preset != "" {
	if p, ok := m.opts.Presets.ByKey(job.Preset); ok && p.Trigger != "" {
		prompt = p.Trigger + ", " + prompt
		m.log(id, "applied preset %q trigger: %q", job.Preset, prompt)
	}
}
```

- [ ] **Step 7: Wire the catalog in main.go** — after `catalog, err := genres.Load(...)` add a parallel `presetCatalog, err := presets.Load(cfg.PresetsFile)` (add `PresetsFile` to `config` + `envOr("PRESETS_FILE", "./presets.json")`), and pass `Presets: presetCatalog` into `queue.Options`.

- [ ] **Step 8: Run all tests + build** — `go test ./...` and `go build -o bin/img-gen ./cmd/img-gen` → PASS/builds.

- [ ] **Step 9: Commit**

```bash
git add internal/queue/queue.go internal/storage/storage.go internal/queue/queue_test.go cmd/img-gen/main.go
git commit -m "feat(presets): resolve character presets to model/loras/trigger defaults"
```

---

### Task 3: /api/presets endpoint + frontend preset dropdown

**Files:**
- Modify: `cmd/img-gen/main.go` (`/api/presets` handler)
- Modify: `cmd/img-gen/main_test.go`
- Modify: `cmd/img-gen/static/index.html` (preset `<select>`)
- Modify: `cmd/img-gen/static/app.js` (load presets, render, collect)

**Interfaces:**
- Consumes: `Options.Presets` (Task 2), the catalog from `models.Load` (existing `/api/models`).
- Produces: `GET /api/presets` → `{presets:[...]}`; `SubmitRequest.Preset` filled by the frontend.

- [ ] **Step 1: Endpoint** — in `newHandler`, add (mirroring `/api/models`):

```go
mux.HandleFunc("/api/presets", func(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, presetCatalog)
})
```

Add a handler test in `main_test.go` asserting `GET /api/presets` returns the preset list (mirror the existing `/api/models` test at `main_test.go:225`).

- [ ] **Step 2: Frontend select** — in `index.html`, add a "Preset" `<select id="preset">` beside the Style selector. In `app.js`, add `loadPresets()` (fetch `/api/presets`, populate `#preset` with a "None" option), call it from `init()`, and add `preset: $('preset').value` to the `body` in `generate()`.

- [ ] **Step 3: Build + manual smoke** — `go build -o bin/img-gen ./cmd/img-gen`, `go test ./...`. Run the binary, confirm the preset dropdown populates and a submit carries `preset`.

- [ ] **Step 4: Commit**

```bash
git add cmd/img-gen/main.go cmd/img-gen/main_test.go cmd/img-gen/static/index.html cmd/img-gen/static/app.js
git commit -m "feat(presets): /api/presets endpoint + composer preset dropdown"
```

---

### Task 4: Batch — backend

**Files:**
- Modify: `internal/queue/queue.go` (`SubmitRequest.Batch`, `Manager.SubmitBatch`, refactor `Submit` to a shared helper)
- Modify: `internal/storage/storage.go` (`Job.BatchID`)
- Modify: `cmd/img-gen/main.go` (`/api/jobs` returns `job_ids` for batch)
- Test: `internal/queue/queue_test.go`

**Interfaces:**
- Consumes: `Manager.Submit` (existing single-job contract).
- Produces: `SubmitRequest.Batch int`, `Manager.SubmitBatch(req) ([]string, error)`, `Job.BatchID string` — consumed by Task 5.

- [ ] **Step 1: Refactor `Submit` to a shared constructor** — extract the validation + job-creation body of `Submit` into:

```go
func (m *Manager) submitOne(req SubmitRequest, batchID string) (string, error) {
	// ... the existing body of Submit, unchanged, except:
	//   job.BatchID = batchID
	//   return id, nil  (the m.work <- id push stays)
}

func (m *Manager) Submit(req SubmitRequest) (string, error) {
	return m.submitOne(req, "")
}
```

- [ ] **Step 2: Add `Batch` + `SubmitBatch`**

```go
// SubmitRequest gains: Batch int `json:"batch"` // >1 fans out to N jobs
func (m *Manager) SubmitBatch(req SubmitRequest) ([]string, error) {
	n := req.Batch
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8 // cap: the sidecar is single-flight and serial
	}
	batchID := newID()
	base := req.Seed
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		r := req
		if base != nil {
			s := *base + int64(i) // vary the locked seed across the batch
			r.Seed = &s
		}
		id, err := m.submitOne(r, batchID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
```

- [ ] **Step 3: Add `BatchID` to `storage.Job`** — `BatchID string \`json:"batch_id,omitempty"\``.

- [ ] **Step 4: Write the failing test**

```go
// queue_test.go — append
func TestSubmitBatchFansOut(t *testing.T) {
	m := newTestManager(t)
	seed := int64(42)
	ids, err := m.SubmitBatch(SubmitRequest{
		Genre: "portrait", Fields: map[string]string{"subject": "x"},
		Size: "512x512", Batch: 3, Seed: &seed,
	})
	if err != nil || len(ids) != 3 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatal("batch ids must be distinct")
	}
	j0, _ := m.Get(ids[0])
	j1, _ := m.Get(ids[1])
	if j0.BatchID == "" || j0.BatchID != j1.BatchID {
		t.Fatalf("batch ids must match: %q vs %q", j0.BatchID, j1.BatchID)
	}
	if *j1.Seed != 43 {
		t.Fatalf("seed should vary across batch: %v", *j1.Seed)
	}
}
```

- [ ] **Step 5: Run it** — `go test ./internal/queue/ -run Batch` → FAIL.

- [ ] **Step 6: Wire `/api/jobs`** — in `main.go`'s `case http.MethodPost`, after decoding, add:

```go
if req.Batch > 1 {
	ids, err := mgr.SubmitBatch(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string][]string{"job_ids": ids})
	return
}
```

- [ ] **Step 7: Run all tests + build** — `go test ./...`, `go build -o bin/img-gen ./cmd/img-gen`.

- [ ] **Step 8: Commit**

```bash
git add internal/queue/queue.go internal/storage/storage.go internal/queue/queue_test.go cmd/img-gen/main.go
git commit -m "feat(batch): N-image batch fan-out via SubmitBatch"
```

---

### Task 5: Batch — frontend

**Files:**
- Modify: `cmd/img-gen/static/index.html` (batch count input)
- Modify: `cmd/img-gen/static/app.js` (`generate()` sends `batch`; handle `job_ids` response)

**Interfaces:**
- Consumes: `POST /api/jobs` returning `{job_id}` (batch≤1) or `{job_ids:[...]}` (batch>1) from Task 4.

- [ ] **Step 1: Add the input** — in `index.html`'s Advanced block, add `<label class="ctrl">Batch count <input type="number" id="batch" min="1" max="8" value="1"></label>`.

- [ ] **Step 2: Send + handle batch** — in `app.js`:

```js
// in generate(), after building `body`:
const batch = Math.max(1, Math.min(8, parseInt($('batch').value) || 1));
body.batch = batch;
// ...
const res = await jsonFetch('/api/jobs', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(body) });
if (res.job_ids) {
  res.job_ids.forEach(subscribe);
  setStatus(`queued ${res.job_ids.length} images`, 'pending');
} else {
  subscribe(res.job_id);
}
```

- [ ] **Step 3: Manual smoke** — build, run, submit a 3-image batch, confirm three jobs appear in history with distinct seeds and a shared batch grouping (the gallery lists them).

- [ ] **Step 4: Commit**

```bash
git add cmd/img-gen/static/index.html cmd/img-gen/static/app.js
git commit -m "feat(batch): frontend batch count + multi-subscribe"
```

---

### Task 6: Pose feasibility spike (M6)

**Files:** none committed — throwaway probe on M6.

**Interfaces:**
- Produces a decision recorded in the ledger: which mflux ControlNet invocation backs the "Pose" mode (canny on `dev-controlnet-canny`, or canny on the uncensored Persephone path if mflux supports `--base-model` combination).

- [ ] **Step 1: Probe canny on the built-in model**

On M6, run (SFW prompt):
```bash
~/.local/bin/mflux-generate-controlnet --model dev-controlnet-canny \
  --controlnet-image-path /tmp/ref.png --controlnet-strength 0.7 \
  --prompt "a woman in a chair, photorealistic" --steps 20 --output /tmp/canny.png
```
Record: does it succeed, download the model, and produce an edge-following image?

- [ ] **Step 2: Probe canny on the uncensored model**

```bash
~/.local/bin/mflux-generate-controlnet --model /Users/<you>/mflux-models/persephone-4bit \
  --base-model dev-controlnet-canny --controlnet-image-path /tmp/ref.png \
  --controlnet-strength 0.7 --prompt "a woman in a chair, photorealistic" \
  --steps 20 --output /tmp/canny-uncensored.png
```
Record: does mflux accept a custom model + `--base-model dev-controlnet-canny` and apply canny conditioning, or does it error / ignore the conditioning?

- [ ] **Step 3: Record the ruling** — in the plan ledger, note the chosen invocation and the censored-vs-uncensored outcome. The sidecar `CONTROLNET_MODEL` default and the `--base-model` flag (if needed) in Task 7 follow this ruling.

---

### Task 7: Sidecar /pose endpoint + deploy

**Files:**
- Modify: `../LLM-router/deploy/mflux-sidecar.py` (`/pose` handler, `CONTROLNET_*` env)
- Modify: `../LLM-router/deploy/com.lattice.mflux.plist.in` (or `install-macos.sh`) if `CONTROLNET_BIN`/`CONTROLNET_MODEL` need plumbing
- Deploy: targeted `sed` + `launchctl bootstrap` on M6 (do NOT re-run the full install script — M6's copy is stale).

**Interfaces:**
- Consumes: Task 6's ruling (model + base-model choice).
- Produces: `POST /pose` → `{image, ...}` on the mflux sidecar — consumed by Task 8.

- [ ] **Step 1: Add env + handler** — in `mflux-sidecar.py`:

```python
CONTROLNET_BIN = os.environ.get("MFLUX_CONTROLNET_BIN", "mflux-generate-controlnet")
CONTROLNET_MODEL = os.environ.get("MFLUX_CONTROLNET_MODEL", "dev-controlnet-canny")
CONTROLNET_BASE = os.environ.get("MFLUX_CONTROLNET_BASE", "")  # --base-model when using a custom model
```

Add a `/pose` branch in `do_POST` (mirror `/generate`): decode `{prompt, image, strength?, steps?, guidance?, negative_prompt?, width?, height?, model?, loras?}`, write the ref image to a temp PNG, run:

```python
cmd = [CONTROLNET_BIN, "--model", model, "--controlnet-image-path", img_path,
       "--controlnet-strength", str(strength), "--output", out]
if CONTROLNET_BASE:
    cmd += ["--base-model", CONTROLNET_BASE]
# + prompt/steps/guidance/negative_prompt/width/height/--lora as in /generate
```

Return the output image the same b64-in/b64-out way `/generate` does.

- [ ] **Step 2: Deploy** — copy `mflux-sidecar.py` to M6, `launchctl bootout` + `bootstrap` `com.lattice.mflux`, then `curl 127.0.0.1:8899/health` and a `/pose` smoke test.

- [ ] **Step 3: Commit (LLM-router repo)**

```bash
git -C ../LLM-router add deploy/mflux-sidecar.py
git -C ../LLM-router commit -m "feat(mflux): /pose ControlNet endpoint"
```

---

### Task 8: img-gen Pose mode (backend + frontend)

**Files:**
- Modify: `internal/lattice/lattice.go` (`Controlnet` client method)
- Modify: `internal/queue/queue.go` (`SubmitRequest` pose fields, `Submit` pose case, `ImageOps.Controlnet`, `dispatch`)
- Modify: `cmd/img-gen/main.go` (wire `ImageOps.Controlnet`)
- Modify: `cmd/img-gen/static/index.html` (Pose option + controls)
- Modify: `cmd/img-gen/static/app.js` (pose mode handling)

**Interfaces:**
- Consumes: `POST /pose` on the mflux sidecar (Task 7); the existing `ImageOps`/`dispatch` shape.
- Produces: `ImageOps.Controlnet(ctx, prompt, size, imageB64 string, strength float64, spec, sp) ([]byte, *int64, error)`; mode `"pose"`.

- [ ] **Step 1: lattice client** — add `Controlnet` mirroring `Generate`/`Edit`: POST to `ImageURL + "/pose"` with `{prompt, image, strength, width, height, steps, guidance, negative_prompt, model, loras}`, decode `{image, seed}`.

- [ ] **Step 2: queue wiring** — add to `SubmitRequest` (reuse `Image` for the ref, `Strength` for controlnet strength): no new fields needed, but add a `case "pose"` to `Submit`'s switch:

```go
case "pose":
	if strings.TrimSpace(req.Prompt) == "" {
		return "", fmt.Errorf("prompt is required for pose")
	}
	if strings.TrimSpace(req.Image) == "" {
		return "", fmt.Errorf("a reference image is required for pose")
	}
	if !validSize(req.Size) {
		return "", fmt.Errorf("invalid size %q", req.Size)
	}
	inputs.Image = req.Image
	inputs.Strength = req.Strength
	if inputs.Strength == 0 {
		inputs.Strength = 0.7
	}
```

Add `Controlnet` to `ImageOps` and a `case "pose"` in `dispatch` calling it. Store pose inputs via the existing `m.inputs[id]` map (set for non-generate modes, already wired).

- [ ] **Step 3: main.go** — pass `Controlnet: lat.Controlnet` in the `queue.ImageOps` literal.

- [ ] **Step 4: frontend** — add `<option value="pose">Pose (ControlNet)</option>` to the mode `<select>`, a pose image-upload + preview + strength slider control (mirror the `edit` controls), and a `case 'pose'` in `generate()` that sets `body.prompt`, `body.size`, `body.image`, `body.strength`.

- [ ] **Step 5: Tests + build** — add a queue test for the pose validation (missing ref image → error); `go test ./...`, `go build -o bin/img-gen ./cmd/img-gen`.

- [ ] **Step 6: Deploy img-gen to <host> + end-to-end verify** — rebuild, `./deploy/install-macos.sh` (with `IMAGE_URL`/`UPSCALE_URL`), submit a pose job, confirm an edge-guided image returns.

- [ ] **Step 7: Commit**

```bash
git add internal/lattice/lattice.go internal/queue/queue.go cmd/img-gen/main.go cmd/img-gen/static/index.html cmd/img-gen/static/app.js
git commit -m "feat(pose): ControlNet pose mode end-to-end"
```

---

## Self-Review

- **Spec coverage:** presets (model+LoRA+trigger+negative+sampling defaults) → Tasks 1–3; batch (N images, distinct seeds, grouped) → Tasks 4–5; pose (ControlNet edge-guided) → Tasks 6–8. Each maps to an existing convention (catalog → queue → endpoint → frontend).
- **Placeholder scan:** no TBD/TODO; the pose spike (Task 6) is the one intentionally-open item, and its ruling feeds Tasks 7–8 explicitly.
- **Type consistency:** `presets.Preset` fields match the JSON keys used in Task 1's test and Task 2's resolution; `SubmitBatch` returns `[]string` and `/api/jobs` emits `job_ids` to match Task 5's frontend; `ImageOps.Controlnet` signature is spelled identically in Tasks 8's steps.

## Execution Handoff

Plan complete and saved. Two execution options:
1. **Subagent-Driven (recommended)** — fresh subagent per task, review between tasks.
2. **Inline Execution** — execute tasks in this session with checkpoints.
