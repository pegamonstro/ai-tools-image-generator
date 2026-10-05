# Model & LoRA Selection + History Gallery — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-job model + LoRA selection and a history gallery (select / edit / download / save-to-dir) to the img-gen tool.

**Architecture:** A pass-through: img-gen resolves a local `models.json` catalog (or a raw "advanced" value) to concrete `--model`/`--lora` values and forwards them on the existing `/generate`/`/edit` request bodies; the mflux sidecar forwards them to `mflux-generate`. History images (already persisted) gain a gallery with reuse/download/export actions.

**Tech Stack:** Go stdlib (`net/http`, `encoding/json`), vanilla JS SPA (`//go:embed`), Python stdlib sidecar. Tests: `go test`, `httptest` fake sidecar.

**Spec:** `docs/superpowers/specs/2026-10-05-model-lora-selection-and-history-design.md`

## Global Constraints

- Go stdlib only; no framework, no ORM.
- One static binary; UI embedded via `//go:embed`.
- Minimise SSD writes: hot state in RAM; durable artifacts written once at completion.
- No speculative infrastructure; complexity must earn its existence.
- Shared types (`LoraRef`, `ModelSpec`) live in `internal/storage` so `queue` and `lattice` share one definition without an import cycle.

### Resolution (deviation from spec §6.3)

The spec placed `ModelSpec` in `internal/lattice`. At implementation, `ModelSpec` must be shared by `queue.ImageOps` (which does not import `lattice`) and `lattice.Client`. Both already import — or will import — `internal/storage`. Therefore **`ModelSpec` lives in `internal/storage` alongside `LoraRef`**, not in `lattice`. Cost if wrong: none; this is the only placement that avoids a `queue → lattice` import.

### Repo split

Tasks 1–5, 7–9 live in `/Users/<you>/Projects/img-gen`. Task 6 (the sidecar) lives in `/Users/<you>/Projects/LLM-router` (`deploy/mflux-sidecar.py`).

---

## Task 1: `internal/models` catalog package

**Files:**
- Create: `internal/models/models.go`
- Create: `internal/models/models_test.go`

**Interfaces:**
- Produces: `models.Entry{Key,Label,Value string}`, `models.Catalog{Models,Loras []Entry}`, `func Load(path string) (*Catalog, error)`.

- [ ] **Step 1: Write the failing test**

```go
package models

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.json")
	if err := os.WriteFile(p, []byte(`{"models":[{"key":"dev","label":"FLUX.1-dev","value":"/m/dev"}],"loras":[{"key":"uncensored","label":"Uncensored","value":"shauray/flux-uncensored-lora"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Models) != 1 || c.Models[0].Key != "dev" || c.Models[0].Value != "/m/dev" {
		t.Fatalf("models: %+v", c.Models)
	}
	if len(c.Loras) != 1 || c.Loras[0].Value != "shauray/flux-uncensored-lora" {
		t.Fatalf("loras: %+v", c.Loras)
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if c == nil || len(c.Models) != 0 || len(c.Loras) != 0 {
		t.Fatalf("want empty catalog, got %+v", c)
	}
}

func TestLoadBadJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("want error for bad JSON")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/models/`
Expected: FAIL — package does not exist / `undefined: Load`.

- [ ] **Step 3: Implement**

```go
// Package models loads the local model/LoRA catalog (models.json).
package models

import (
	"encoding/json"
	"os"
)

type Entry struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value string `json:"value"`
}

type Catalog struct {
	Models []Entry `json:"models"`
	Loras  []Entry `json:"loras"`
}

// Load reads a catalog JSON file. A missing file yields an empty catalog
// (generation still works on the sidecar's default model), not an error.
func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Catalog{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/models/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/models/models.go internal/models/models_test.go
git commit -m "feat(models): add local model/LoRA catalog loader"
```

---

## Task 2: `internal/storage` — `LoraRef`, `ModelSpec`, Job fields, `CopyImage`

**Files:**
- Modify: `internal/storage/storage.go`
- Modify: `internal/storage/storage_test.go`

**Interfaces:**
- Consumes: existing `Store`, `Job`, `SaveImage`, `AppendHistory`, `LoadHistory`.
- Produces: `LoraRef{Name string; Scale float64}`, `ModelSpec{Model string; Loras []LoraRef}`, `Job.Model string`, `Job.Loras []LoraRef`, `func (s *Store) CopyImage(id, destDir string) (string, error)`, `func ValidImageID(id string) bool`.

- [ ] **Step 1: Write the failing tests** (append to `storage_test.go`)

```go
func TestCopyImage(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef" // 16 hex chars
	if _, err := s.SaveImage(id, []byte{0x89, 'P', 'N', 'G'}); err != nil {
		t.Fatal(err)
	}
	dst, err := s.CopyImage(id, filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatalf("CopyImage: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("copied file missing: %v", err)
	}
}

func TestCopyImageRejectsBadID(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyImage("../../etc/passwd", t.TempDir()); err == nil {
		t.Fatal("want error for invalid id")
	}
}
```

(Add `"path/filepath"` to the test imports.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/storage/`
Expected: FAIL — `undefined: CopyImage`.

- [ ] **Step 3: Implement** (in `storage.go`)

Add after the `Job` type:

```go
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
```

Extend `Job` with two fields (before `Status`):

```go
	Model string    `json:"model,omitempty"`
	Loras []LoraRef `json:"loras,omitempty"`
```

Add `idRe` and the copy/validate helpers (near the top, after imports):

```go
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
```

Add `"fmt"` and `"regexp"` to `storage.go`'s imports (they are not present today).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/storage/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/storage.go internal/storage/storage_test.go
git commit -m "feat(storage): add LoraRef/ModelSpec, Job model+loras, CopyImage"
```

---

## Task 3: `internal/lattice` — `ModelSpec` on Generate/Edit

**Files:**
- Modify: `internal/lattice/lattice.go`
- Modify: `internal/lattice/lattice_test.go`

**Interfaces:**
- Consumes: `storage.ModelSpec`, `storage.LoraRef`.
- Produces: `func (c *Client) Generate(ctx, prompt, size string, spec storage.ModelSpec) ([]byte, error)`, `func (c *Client) Edit(ctx, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec) ([]byte, error)`.

- [ ] **Step 1: Change the signatures and body encoding** (in `lattice.go`)

Add the import `"img-gen/internal/storage"`.

Replace `Generate`:

```go
func (c *Client) Generate(ctx context.Context, prompt, size string, spec storage.ModelSpec) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"prompt": prompt, "width": w, "height": h}
	applyModelSpec(body, spec)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/generate", b)
}
```

Replace `Edit`:

```go
func (c *Client) Edit(ctx context.Context, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"prompt": prompt, "width": w, "height": h,
		"init_image": imageB64, "strength": strength,
	}
	applyModelSpec(body, spec)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/edit", b)
}
```

Add the helper (anywhere in the file):

```go
// applyModelSpec injects the optional model/LoRA selection into a request body,
// leaving the fields absent when unset so the sidecar uses its defaults.
func applyModelSpec(body map[string]any, spec storage.ModelSpec) {
	if spec.Model != "" {
		body["model"] = spec.Model
	}
	if len(spec.Loras) > 0 {
		body["loras"] = spec.Loras
	}
}
```

`Inpaint` and `Blend` are unchanged.

- [ ] **Step 2: Update the existing tests** (in `lattice_test.go`)

Every call to `c.Generate(ctx, prompt, size)` becomes `c.Generate(ctx, prompt, size, storage.ModelSpec{})`; `c.Edit(...)` similarly gains a trailing `storage.ModelSpec{}`. Add one assertion case that a non-empty spec appears in the body:

```go
// in the existing fake-sidecar test, add a variant that posts a spec:
spec := storage.ModelSpec{Model: "/m/dev", Loras: []storage.LoraRef{{Name: "shauray/flux-uncensored-lora", Scale: 0.8}}}
_, err := c.Generate(ctx, "a cat", "512x512", spec)
// the fake sidecar asserts body["model"] == "/m/dev" and body["loras"][0].Name == "shauray/flux-uncensored-lora"
```

(Read the existing `lattice_test.go` to match its fake-sidecar body-decoding style; it already decodes the posted JSON.)

- [ ] **Step 3: Run to verify it passes**

Run: `go test ./internal/lattice/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/lattice/lattice.go internal/lattice/lattice_test.go
git commit -m "feat(lattice): thread ModelSpec through Generate/Edit"
```

---

## Task 4: `internal/queue` — thread model/loras

**Files:**
- Modify: `internal/queue/queue.go`
- Modify: `internal/queue/queue_test.go`

**Interfaces:**
- Consumes: `storage.ModelSpec`, `storage.LoraRef`.
- Produces: `SubmitRequest.Model string`, `SubmitRequest.Loras []storage.LoraRef`, `ImageOps.Generate/Edit` gain `spec storage.ModelSpec`.

- [ ] **Step 1: Extend the structs** (in `queue.go`)

Add to `SubmitRequest` (after `Strengths`):

```go
	Model string            `json:"model"`  // mflux --model value (path or HF id); "" = sidecar default
	Loras []storage.LoraRef `json:"loras"`  // mflux --lora list; nil = none
```

Change `ImageOps.Generate`/`Edit` (leave `Inpaint`/`Blend` unchanged):

```go
type ImageOps struct {
	Generate func(ctx context.Context, prompt, size string, spec storage.ModelSpec) ([]byte, error)
	Edit     func(ctx context.Context, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec) ([]byte, error)
	Inpaint  func(ctx context.Context, prompt, imageB64, maskB64 string) ([]byte, error)
	Blend    func(ctx context.Context, prompt, size string, imagesB64 []string, strengths []float64) ([]byte, error)
}
```

- [ ] **Step 2: Copy fields onto the Job in `Submit`** (in `queue.go`)

In `Submit`, add `Model` and `Loras` to the `storage.Job{...}` literal:

```go
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
```

Add the helper (next to `normalizeStrengths`):

```go
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
```

- [ ] **Step 3: Pass the spec in `run`** (in `queue.go`)

In `run`, just before the `switch`, build the spec; use it in the `edit` and default (`generate`) cases:

```go
	spec := storage.ModelSpec{Model: job.Model, Loras: job.Loras}
	...
	case "edit":
		m.log(id, "requesting edit (strength=%.2f)", inputs.Strength)
		png, err = m.opts.Ops.Edit(context.Background(), prompt, job.Size, inputs.Image, inputs.Strength, spec)
	...
	default:
		m.log(id, "requesting image from lattice (size=%s)", job.Size)
		png, err = m.opts.Ops.Generate(context.Background(), prompt, job.Size, spec)
```

- [ ] **Step 4: Update the existing tests** (in `queue_test.go`)

Update the `ImageOps` literals in the tests to the new `Generate`/`Edit` signatures (add a trailing `spec storage.ModelSpec` parameter). Add one assertion that `Submit` records `Model`/`Loras` on the returned job (via `mgr.Get(id)`).

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/queue/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/queue/queue.go internal/queue/queue_test.go
git commit -m "feat(queue): thread model/loras through submit and run"
```

---

## Task 5: `cmd/img-gen` — catalog, `/api/models`, download, `/api/export`

**Files:**
- Modify: `cmd/img-gen/main.go`
- Modify: `cmd/img-gen/main_test.go`

**Interfaces:**
- Consumes: `models.Load`, `storage.ValidImageID`, `store.CopyImage`, the new `ImageOps` signatures.
- Produces: `GET /api/models`, `GET /api/images/{id}.png?download=1`, `POST /api/export`.

- [ ] **Step 1: Config + catalog load** (in `main.go`)

Add `ModelsFile` and `ExportDir` to `config`; populate in `loadConfig`:

```go
	ModelsFile: envOr("MODELS_FILE", "./models.json"),
	ExportDir:  envOr("EXPORT_DIR", ""), // defaulted to ~/Downloads/img-gen when empty
```

In `newHandler`, after loading genres, load the catalog and keep `store` in scope:

```go
	catalog, err := models.Load(cfg.ModelsFile)
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
```

Add the import `"img-gen/internal/models"`.

Wire the `ImageOps` (signatures now match `lat.Generate`/`lat.Edit` after Task 3, so the direct method-value assignment is unchanged):

```go
		Ops: queue.ImageOps{
			Generate: lat.Generate,
			Edit:     lat.Edit,
			Inpaint:  lat.Inpaint,
			Blend:    lat.Blend,
		},
```

- [ ] **Step 2: Add `/api/models`** (in `main.go`, after `/api/genres`)

```go
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, catalog)
	})
```

- [ ] **Step 3: `?download=1` on `/api/images/`** (in `main.go`)

Inside the existing `/api/images/` handler, before `http.ServeFile`:

```go
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+".png"))
		}
```

- [ ] **Step 4: `POST /api/export`** (in `main.go`, after `/api/images/`)

```go
	mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			IDs  []string `json:"ids"`
			Dest string   `json:"dest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad body")
			return
		}
		dest := req.Dest
		if dest == "" {
			dest = cfg.ExportDir
		}
		if dest == "" {
			home, _ := os.UserHomeDir()
			dest = filepath.Join(home, "Downloads", "img-gen")
		}
		if !filepath.IsAbs(dest) {
			writeErr(w, http.StatusBadRequest, "dest must be an absolute path")
			return
		}
		var copied, skipped []string
		for _, id := range req.IDs {
			dst, err := store.CopyImage(id, dest)
			if err != nil {
				skipped = append(skipped, id)
				continue
			}
			copied = append(copied, dst)
		}
		writeJSON(w, map[string]any{"copied": copied, "skipped": skipped})
	})
```

- [ ] **Step 5: Update the integration test** (in `main_test.go`)

Add a case that (a) `GET /api/models` returns the catalog, and (b) `POST /api/export` with `{ids:[...], dest:"<tmpdir>"}` returns `copied` containing a path that exists. Use a temp `DATA_DIR` and seed one image via a fake sidecar, or by calling the store directly.

- [ ] **Step 6: Build + run the full suite**

Run: `go build ./... && go test ./...`
Expected: build clean; all packages PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/img-gen/main.go cmd/img-gen/main_test.go
git commit -m "feat(img-gen): /api/models, image download, /api/export"
```

---

## Task 6: sidecar `model`/`loras` override (LLM-router repo)

**Files:**
- Modify: `/Users/<you>/Projects/LLM-router/deploy/mflux-sidecar.py`

**Interfaces:**
- Consumes: existing `run_generation`, `_build_params`, `MODEL`, `LORA`.
- Produces: `/generate` and `/edit` accept optional `model` (string) and `loras` (`[{name, scale}]`), overriding the env defaults.

- [ ] **Step 1: Thread `model`/`loras` through `_build_params`**

In `_build_params`, the `if path in ("/generate", "/edit")` branch already builds `p = dict(common, …)`. Add:

```python
        p["model"] = body.get("model")
        p["loras"] = body.get("loras")
```

- [ ] **Step 2: Use them in `run_generation`**

Replace the fixed model/LoRA wiring at the top of `run_generation`:

```python
        model = params.get("model") or MODEL
        cmd = [BIN, "--model", model, "--output", out]
        loras = params.get("loras")
        if loras:
            for ref in loras:
                cmd += ["--lora", ref["name"], str(ref.get("scale", 1.0))]
        elif LORA:
            cmd += ["--lora", LORA, LORA_SCALE]
```

(The rest — `QUANTIZE`, `EXTRA`, prompt/width/height/… — is unchanged.)

- [ ] **Step 3: Syntax check**

Run: `python3 -m py_compile deploy/mflux-sidecar.py`
Expected: no output (compiles clean).

- [ ] **Step 4: Commit** (in the LLM-router repo)

```bash
git add deploy/mflux-sidecar.py
git commit -m "feat(mflux): accept per-request model/loras override on /generate + /edit"
```

---

## Task 7: frontend — model/LoRA pickers

**Files:**
- Modify: `cmd/img-gen/static/index.html`
- Modify: `cmd/img-gen/static/app.js`
- Modify: `cmd/img-gen/static/style.css`

- [ ] **Step 1: Add the controls to `index.html`** (after the Style `<label>`, before `<div id="gen-controls">`)

```html
        <label class="ctrl">Model
          <select id="model"></select>
        </label>

        <div class="row">
          <label class="ctrl">LoRA
            <select id="lora"></select>
          </label>
          <label class="ctrl">LoRA scale <span id="lora-scale-val">1.0</span>
            <input type="range" id="lora-scale" min="0" max="1" step="0.05" value="1">
          </label>
        </div>

        <details class="advanced">
          <summary>Advanced — local path or HuggingFace id</summary>
          <label class="ctrl">Model override
            <input type="text" id="model-raw" placeholder="/path/to/model or org/model">
          </label>
          <label class="ctrl">LoRA override
            <input type="text" id="lora-raw" placeholder="/path/to/lora or org/lora">
          </label>
        </details>
```

- [ ] **Step 2: Load + render the catalog in `app.js`**

Add a module state and two render functions, called from `init()` (after `renderStyleSelect()`):

```js
let models = { models: [], loras: [] };

async function loadModels() {
  try { models = await jsonFetch('/api/models'); }
  catch (e) { appendLog(new Date(), 'failed', 'could not load models: ' + e.message); return; }
  renderModelSelect();
  renderLoraSelect();
}

function renderModelSelect() {
  const sel = $('model'); sel.innerHTML = '';
  const none = document.createElement('option'); none.value = ''; none.textContent = 'Sidecar default'; sel.appendChild(none);
  for (const m of models.models || []) {
    const o = document.createElement('option'); o.value = m.value; o.textContent = m.label; sel.appendChild(o);
  }
}

function renderLoraSelect() {
  const sel = $('lora'); sel.innerHTML = '';
  const none = document.createElement('option'); none.value = ''; none.textContent = 'None'; sel.appendChild(none);
  for (const l of models.loras || []) {
    const o = document.createElement('option'); o.value = l.value; o.textContent = l.label; sel.appendChild(o);
  }
}
```

Add `loadModels();` to `init()` and a `$('lora-scale').oninput` handler that updates `$('lora-scale-val').textContent`.

- [ ] **Step 3: Collect the spec and submit it in `generate()`**

Add a collector:

```js
function collectModelSpec() {
  const model = $('model-raw').value.trim() || $('model').value;
  const loraName = $('lora-raw').value.trim() || $('lora').value;
  const spec = {};
  if (model) spec.model = model;
  if (loraName) spec.loras = [{ name: loraName, scale: parseFloat($('lora-scale').value) }];
  return spec;
}
```

In `generate()`, after `const body = { mode, enhance, style };`, merge in the spec for generate/edit only:

```js
  if (mode === 'generate' || mode === 'edit') {
    Object.assign(body, collectModelSpec());
  }
```

- [ ] **Step 4: Gate the LoRA controls per mode**

In `renderMode()`, disable the LoRA select/scale/raw inputs when the mode is `inpaint` or `blend` (LoRA applies to generate/edit only):

```js
function renderMode() {
  const m = currentMode();
  // ...existing hidden toggles...
  const loraDisabled = (m === 'inpaint' || m === 'blend');
  $('lora').disabled = loraDisabled;
  $('lora-scale').disabled = loraDisabled;
  $('lora-raw').disabled = loraDisabled;
}
```

- [ ] **Step 5: Minimal styling** (in `style.css`)

Add a `.advanced` and `.row` rule if not already present (the `.row` class already exists for genre/size). Add spacing for the pickers to match `.ctrl`.

- [ ] **Step 6: Verify in a browser**

Run `go run ./cmd/img-gen`, open `http://127.0.0.1:8099`, and confirm the Model/LoRA pickers render (empty catalog is fine), the advanced fields appear, and submitting a generate job still works (no model selected → sidecar default).

- [ ] **Step 7: Commit**

```bash
git add cmd/img-gen/static/index.html cmd/img-gen/static/app.js cmd/img-gen/static/style.css
git commit -m "feat(frontend): model + LoRA pickers with advanced override"
```

---

## Task 8: frontend — history gallery actions

**Files:**
- Modify: `cmd/img-gen/static/app.js`
- Modify: `cmd/img-gen/static/index.html`
- Modify: `cmd/img-gen/static/style.css`

- [ ] **Step 1: Add a detail modal to `index.html`** (before the closing `</main>`)

```html
    <div id="detail" class="modal" hidden>
      <div class="modal-body">
        <button id="detail-close" class="ghost" type="button">×</button>
        <img id="detail-img" alt="image">
        <div id="detail-meta" class="detail-meta"></div>
        <div class="actions">
          <button id="detail-edit" type="button">Edit this image</button>
          <a id="detail-download" class="button" download>Download</a>
          <button id="detail-save" type="button">Save to folder</button>
        </div>
      </div>
    </div>
```

- [ ] **Step 2: Card metadata + actions in `loadHistory()`**

In the per-job loop in `loadHistory()`, after the image/error block, append a meta line and action buttons:

```js
    const meta = document.createElement('div');
    meta.className = 'card-meta';
    meta.textContent = [j.model, (j.loras && j.loras.length ? j.loras.map(l => l.name).join(', ') : ''), j.style].filter(Boolean).join(' · ');
    card.appendChild(meta);

    const acts = document.createElement('div');
    acts.className = 'card-actions';
    const view = document.createElement('button');
    view.type = 'button'; view.textContent = 'View';
    view.onclick = () => openDetail(j);
    acts.appendChild(view);
    if (j.image_path) {
      const editBtn = document.createElement('button');
      editBtn.type = 'button'; editBtn.textContent = 'Edit';
      editBtn.onclick = () => useHistoryAsEdit(j.id);
      acts.appendChild(editBtn);
    }
    card.appendChild(acts);
```

- [ ] **Step 3: Detail, reuse, download, export helpers** (append to `app.js`)

```js
let detailJob = null;

function openDetail(j) {
  detailJob = j;
  $('detail').hidden = false;
  $('detail-img').src = '/api/images/' + j.id + '.png';
  $('detail-meta').textContent = [j.prompt, j.model, (j.loras || []).map(l => l.name).join(','), j.style, j.size, j.created_at].filter(Boolean).join('\n');
  $('detail-download').href = '/api/images/' + j.id + '.png?download=1';
  $('detail-edit').hidden = !j.image_path;
  $('detail-save').hidden = !j.image_path;
}

$('detail-close').onclick = () => { $('detail').hidden = true; };

async function useHistoryAsEdit(id) {
  $('detail').hidden = true;
  const img = await fetch('/api/images/' + id + '.png');
  const blob = await img.blob();
  uploaded.edit = await blobToBase64(blob);
  $('mode').value = 'edit';
  renderMode();
  setStatus('loaded image ' + id + ' for editing', 'pending');
}

async function saveToFolder() {
  if (!detailJob) return;
  const r = await jsonFetch('/api/export', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids: [detailJob.id] }),
  });
  setStatus('saved: ' + (r.copied[0] || 'skipped'), r.copied.length ? 'ok' : 'err');
}

$('detail-edit').onclick = () => useHistoryAsEdit(detailJob.id);
$('detail-save').onclick = saveToFolder;
```

Add the base64 helper (the existing code uses `FileReader` for uploads; this converts a fetched blob):

```js
function blobToBase64(blob) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(r.result.split(',')[1]);
    r.onerror = () => reject(r.error);
    r.readAsDataURL(blob);
  });
}
```

- [ ] **Step 4: Minimal styling** (in `style.css`)

Add `.modal`, `.modal-body`, `.card-meta`, `.card-actions`, `.button`, `.detail-meta` rules — a simple centered overlay for the modal, small muted text for meta, a flex row of buttons.

- [ ] **Step 5: Verify in a browser**

Run `go run ./cmd/img-gen`, generate one image, then confirm: the history card shows model/loras metadata and View/Edit buttons; View opens the modal; Edit loads the image into edit mode; Download saves a PNG; Save to folder copies it to `~/Downloads/img-gen`.

- [ ] **Step 6: Commit**

```bash
git add cmd/img-gen/static/app.js cmd/img-gen/static/index.html cmd/img-gen/static/style.css
git commit -m "feat(frontend): history detail, edit-from-history, download, export"
```

---

## Task 9: catalog example, `.gitignore`, README

**Files:**
- Create: `models.json.example`
- Modify: `.gitignore`
- Modify: `README.md`

- [ ] **Step 1: `models.json.example`**

```json
{
  "models": [
    { "key": "persephone", "label": "Persephone 2.0 (NSFW)", "value": "/Users/<you>/mflux-models/persephone-4bit" },
    { "key": "dev", "label": "FLUX.1-dev", "value": "/Users/<you>/mflux-models/flux-dev-4bit" }
  ],
  "loras": [
    { "key": "uncensored", "label": "Uncensored (dev-trained)", "value": "shauray/flux-uncensored-lora" }
  ]
}
```

- [ ] **Step 2: `.gitignore`**

Append `models.json` (keep `data/` and `.playwright-mcp/`).

- [ ] **Step 3: README** — document `MODELS_FILE`, `EXPORT_DIR`, the `/api/models` and `/api/export` endpoints, and how to add a model (copy `models.json.example` → `models.json`, add entries).

- [ ] **Step 4: Commit**

```bash
git add models.json.example .gitignore README.md
git commit -m "docs: models catalog example, gitignore, README"
```

---

## Task 10: full build, test, end-to-end verify

**Files:** none (verification only).

- [ ] **Step 1:** `go build ./... && go test ./...` in the img-gen repo — all green.
- [ ] **Step 2:** `go vet ./...` — clean.
- [ ] **Step 3:** Manual end-to-end: run the server, generate one image with a catalog model (Persephone), one with a LoRA, one with an advanced HF id, one edit-from-history, one export-to-dir.
- [ ] **Step 4:** (If the sidecar was deployed to the M6) restart the mflux LaunchAgent to pick up the sidecar change, then re-run a generate with a `model` override.
- [ ] **Step 5:** Commit any fixes surfaced by verification.

---

## Self-review notes

- **Spec coverage:** model/LoRA selection (§6) → Tasks 1–7; history gallery (§7) → Task 8; catalog/gitignore/README (§6.1, §7.4) → Task 9; sidecar (§9) → Task 6. PNG `tEXt` metadata is an explicit non-goal (§12), correctly absent.
- **Type consistency:** `storage.LoraRef`/`storage.ModelSpec` are the single definitions used by Tasks 2–4; `ImageOps`/`Client` signatures match across Tasks 3–5.
- **Repo boundary:** Task 6 is the only cross-repo task; all others are in img-gen.
