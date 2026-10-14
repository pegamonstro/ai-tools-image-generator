# Capability Roadmap Round 2 — Phase B Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Phase B of the round-2 capability roadmap: B1 ControlNet/IP-Adapter generate conditioning via the SDXL sidecar, B2 rembg alpha extraction for compose assets, B3 lattice `extension` serialization with honest unknown-field detection, and B4 the `sheet` compose kind (multi-view character turnaround on one grid).

**Architecture:** All four features are additive and opt-in. B1/B3 extend the request→queue→lattice plumbing without new endpoints; B2 adds a sidecar client wired exactly like RIFE plus a tri-state `transparent` compose field with full persistence compatibility (JSON `true` still parses); B4 reuses the npc pinned-seed variant mechanic and the PackGrid atlas packer through a new `ComposeSheet` pure function. `control`/`ip_adapter` ride a new `extension` object when lattice-routed, with explicit rejection detection so pre-upgrade gateways fail honestly instead of silently dropping fields.

**Tech Stack:** Go stdlib only (`net/http/httptest`, `image`, `image/png`, `encoding/json`, `encoding/base64`). No new module dependencies.

**Spec:** `docs/superpowers/specs/2026-10-14-capability-roadmap-round2-design.md` (Phase B, lines 225–348). The spec is the binding authority; this plan argues from it.

## Global Constraints

- **Go stdlib only** — no new module dependencies, no go.mod edits.
- **Every task ends green:** `go vet ./... && go test ./...` must pass before each task's commit.
- **Byte-for-byte defaults:** every new field is opt-in; omitting all new fields produces byte-identical request payloads, job records, and deliverables.
- **Honest failure:** unset sidecar URLs and unreachable sidecars produce `validation_error` at submit or failed jobs/parents with named reasons — never silent field drops, never panics.
- **Never touch live instances:** the :8099 service instance and the :9106 render instance are untouchable. Tests bind random ports (httptest). Smoke uses only fresh scratch instances on unassigned ports (e.g. :9110) with scratch DATA_DIR.
- **No secrets in tracked files:** real env values (SDXL host, lattice URL, etc.) never enter tracked files. Docs use placeholders like `REMBG_URL=http://<rembg-host>:<port>`.
- **Legacy stability:** `ValidationError.Msg` strings for existing paths must not change; the v1 envelope and /api/* echo behavior is frozen.
- **Persistence compatibility:** old persisted `Job.ComposeSpec` JSON (with `"transparent": true`) must keep parsing and producing the same dual-render behavior after this phase.

---

### Task 1: B1 storage types + lattice direct-path payload and SDXL gate

**Files:**
- Modify: `internal/storage/storage.go` (append after the `SamplingParams` struct — grep `type SamplingParams` for the anchor, around line 35)
- Modify: `internal/lattice/lattice.go:42-149` (Generate + latticeImage signatures)
- Test: `internal/lattice/lattice_test.go` (append new tests)

**Interfaces:**
- Consumes: existing `lattice.Client.Generate(ctx, prompt, size string, spec, sp)` and `latticeImage(...)` internals.
- Produces: `storage.ControlParams{Net, Image string; Strength float64}`; `storage.IPAdapterParams{Image string; Strength float64}`; `lattice.Client.Generate(ctx, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error)` — **all later tasks and main.go method-value wiring rely on this exact signature**; `latticeImage` gains ctrl/ipa params (ignored until Task 3).

- [ ] **Step 1: Write the failing tests**

Append to `internal/lattice/lattice_test.go`:

```go
func TestGenerateControlNetSDXL(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString([]byte("fake-png")) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.SDXLImageURL = srv.URL
	ctrl := &storage.ControlParams{Net: "canny", Image: "Q2FubnlNYXA=", Strength: 0.55}
	if _, _, err := c.Generate(context.Background(), "a cat", "512x512", ctrl, nil, storage.ModelSpec{Sidecar: "sdxl"}, storage.SamplingParams{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cn, ok := got["control_net"].(map[string]any)
	if !ok {
		t.Fatalf("control_net missing from body: %v", got)
	}
	if cn["net"] != "canny" || cn["image"] != "Q2FubnlNYXA=" || cn["strength"] != 0.55 {
		t.Fatalf("control_net = %v", cn)
	}
	if _, ok := got["ip_adapter"]; ok {
		t.Fatalf("ip_adapter must be absent when nil")
	}
}

func TestGenerateIPAdapterOmitsZeroStrength(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString([]byte("fake-png")) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.SDXLImageURL = srv.URL
	ipa := &storage.IPAdapterParams{Image: "SWRlbnRpdHk="}
	if _, _, err := c.Generate(context.Background(), "a cat", "512x512", nil, ipa, storage.ModelSpec{Sidecar: "sdxl"}, storage.SamplingParams{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	ia, ok := got["ip_adapter"].(map[string]any)
	if !ok {
		t.Fatalf("ip_adapter missing from body: %v", got)
	}
	if ia["image"] != "SWRlbnRpdHk=" {
		t.Fatalf("ip_adapter.image = %v", ia)
	}
	if _, ok := ia["strength"]; ok {
		t.Fatalf("strength must be omitted when 0 (sidecar applies its 0.6 default)")
	}
}

func TestGenerateControlOmittedByDefault(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString([]byte("fake-png")) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	if _, _, err := c.Generate(context.Background(), "a cat", "512x512", nil, nil, storage.ModelSpec{}, storage.SamplingParams{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, key := range []string{"control_net", "ip_adapter"} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s must be absent without conditioning (byte-identical legacy body)", key)
		}
	}
}

func TestGenerateConditioningGate(t *testing.T) {
	c := New("http://unused")
	c.ImageURL = "http://127.0.0.1:1" // unreachable; the gate must fire before any HTTP
	ctrl := &storage.ControlParams{Net: "pose", Image: "x", Strength: 0.7}
	_, _, err := c.Generate(context.Background(), "p", "512x512", ctrl, nil, storage.ModelSpec{}, storage.SamplingParams{})
	if err == nil {
		t.Fatal("expected gate error without sdxl routing")
	}
	if !strings.Contains(err.Error(), "SDXL sidecar") {
		t.Fatalf("gate error = %v", err)
	}
	ipa := &storage.IPAdapterParams{Image: "x", Strength: 0.6}
	if _, _, err := c.Generate(context.Background(), "p", "512x512", nil, ipa, storage.ModelSpec{}, storage.SamplingParams{}); err == nil {
		t.Fatal("expected gate error for ip_adapter without sdxl routing")
	}
}
```

Add `"strings"` to the test file's imports if not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/lattice/ -run 'TestGenerateControl|TestGenerateIPAdapter|TestGenerateConditioning' -v`
Expected: FAIL (compile error — `Generate` has no ctrl/ipa params yet).

- [ ] **Step 3: Add the storage types**

In `internal/storage/storage.go`, immediately after the `SamplingParams` struct definition, add:

```go
// ControlParams conditions a generate run on a reference image through the
// SDXL sidecar's ControlNet (pose / depth / canny map). Users supply
// processed maps for pose and depth; canny may be preprocessed sidecar-side
// (--canny).
type ControlParams struct {
	Net      string  `json:"net"`      // "pose" | "depth" | "canny"
	Image    string  `json:"image"`    // base64: conditioning reference
	Strength float64 `json:"strength"` // 0 = sidecar default (0.7); else 0.1..1.0
}

// IPAdapterParams conditions a generate run on an identity reference through
// the SDXL sidecar's IP-Adapter.
type IPAdapterParams struct {
	Image    string  `json:"image"`    // base64: identity reference
	Strength float64 `json:"strength"` // 0 = sidecar default (0.6); else 0.1..1.0
}
```

- [ ] **Step 4: Grow the lattice Generate signature (direct payload + gate)**

In `internal/lattice/lattice.go`, replace the `Generate` function (lines 42-62) with:

```go
// Generate requests one image from the image sidecar and returns its raw PNG
// bytes plus the seed the sidecar used (nil when the sidecar does not report
// one). The sidecar takes explicit width/height rather than an OpenAI "size"
// string, so "1024x576" is split into its dimensions. ctrl/ipa are the B1
// SDXL-sidecar conditioning references: nil on both produces byte-for-byte
// the same request as before, and either set on a non-SDXL route is rejected
// honestly instead of silently dropped.
func (c *Client) Generate(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
	if c.ImagesViaLattice {
		return c.latticeImage(ctx, "/v1/images/generations", prompt, size, "", 0, spec, sp, ctrl, ipa)
	}
	w, h, err := parseSize(size)
	if err != nil {
		return nil, nil, err
	}
	if ctrl != nil || ipa != nil {
		// Conditioning needs the SDXL engine's ControlNet/IP-Adapter flags;
		// any other target would drop the references on the floor.
		if spec.Sidecar != "sdxl" || c.SDXLImageURL == "" {
			return nil, nil, errors.New("control/ip-adapter requires the SDXL sidecar (model sidecar \"sdxl\" with SDXL_URL configured)")
		}
	}
	body := map[string]any{"prompt": prompt, "width": w, "height": h}
	if ctrl != nil {
		cn := map[string]any{"net": ctrl.Net, "image": ctrl.Image}
		if ctrl.Strength != 0 {
			cn["strength"] = ctrl.Strength
		}
		body["control_net"] = cn
	}
	if ipa != nil {
		ia := map[string]any{"image": ipa.Image}
		if ipa.Strength != 0 {
			ia["strength"] = ipa.Strength
		}
		body["ip_adapter"] = ia
	}
	applyModelSpec(body, spec)
	applyGenParams(body, sp)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return c.postImage(ctx, "/generate", b, spec.Sidecar)
}
```

- [ ] **Step 5: Update every latticeImage call site (signature grows; params unused until Task 3)**

Replace the `latticeImage` comment + signature (lines 88-94) with:

```go
// latticeImage runs one generate or edit through the frontend's OpenAI Images
// routes: the model field carries the lattice registry name (the submitted
// catalog key, falling back to the raw value), the size stays the "WxH" string,
// and the Lattice extensions (loras + sampling knobs, plus edit's image/strength)
// pass through so settings survive the proxy to the gateway. data[0] returns the
// PNG and the seed the engine's run actually used. ctrl/ipa carry the B1
// conditioning references; a later task serializes them onto the request's
// "extension" object — until then they are accepted but not sent.
func (c *Client) latticeImage(ctx context.Context, path, prompt, size, imageB64 string, strength float64, spec storage.ModelSpec, sp storage.SamplingParams, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams) ([]byte, *int64, error) {
```

Keep the body byte-identical (no `ctrl`/`ipa` references inside yet). Update the `Edit` call site (line 69) to:

```go
		return c.latticeImage(ctx, "/v1/images/edits", prompt, size, imageB64, strength, spec, sp, nil, nil)
```

(`storage` is already imported in lattice.go.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS everywhere (main.go's `Generate: lat.Generate` method value compiles unchanged against the grown signature; queue fakes still compile because queue.go hasn't changed).

- [ ] **Step 7: Commit**

```bash
git add internal/storage/storage.go internal/lattice/lattice.go internal/lattice/lattice_test.go
git commit -m "feat(b1): storage ControlParams/IPAdapterParams + lattice direct conditioning payload with honest SDXL gate

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 2: queue B1 — SubmitRequest fields, validation matrix, and dispatch plumbing

**Files:**
- Modify: `internal/queue/queue.go` (SubmitRequest ~line 40, imageInputs ~line 140, submitOne 441-599, normalizeConditioning after normalizeLoras ~686, dispatch 826-828, renderParamsKV 349-374, saveRecovered call 387)
- Modify: `internal/queue/queue_test.go:56,324` and `internal/queue/compose_test.go:70,88-90` (fake Generate signature updates)
- Test: `internal/queue/queue_test.go` (append new tests)

**Interfaces:**
- Consumes: Task 1's grown `Ops.Generate`-shaped signature — the queue's `ImageOps.Generate` field now takes `(ctx, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec, sp)`.
- Produces: `SubmitRequest.Control *storage.ControlParams` + `SubmitRequest.IPAdapter *storage.IPAdapterParams` (JSON `control`/`ip_adapter`); `imageInputs{Control, IPAdapter}` fields (runtime-only); `renderParamsKV(job storage.Job, inputs imageInputs) map[string]string` with `control` = `"net/strength"` and `ip_adapter` = `"strength"` keys; the tEXt params from Task T2 travel via those keys (A3 embed covers them with no new embed code).

- [ ] **Step 1: Write the failing tests**

Append to `internal/queue/queue_test.go`:

```go
// sdxlCatalog offers one SDXL-sidecar model and one default-sidecar model,
// so tests can drive both engine-gate branches through resolveModelValues.
func sdxlCatalog() *models.Catalog {
	return &models.Catalog{Models: []models.Entry{
		{Key: "pony", Label: "Pony V6 XL", Value: "/m/sdxl/pony.safetensors", Sidecar: "sdxl"},
		{Key: "persephone", Label: "Persephone 2.0", Value: "/m/persephone-4bit"},
	}}
}

func TestSubmitConditioningValidationMatrix(t *testing.T) {
	m := New(testOpts(t))
	cases := []struct {
		name     string
		req      SubmitRequest
		wantMsg  string
	}{
		{"mode conflict: edit", SubmitRequest{Mode: "edit", Prompt: "p", Image: "b64",
			Control: &storage.ControlParams{Net: "pose", Image: "x"}},
			`control and ip_adapter require mode "generate"`},
		{"mode conflict: inpaint", SubmitRequest{Mode: "inpaint", Prompt: "p", Image: "b64", Mask: "m64",
			IPAdapter: &storage.IPAdapterParams{Image: "x"}},
			`control and ip_adapter require mode "generate"`},
		{"bad net", SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512",
			Control: &storage.ControlParams{Net: "edges", Image: "x"}},
			"control.net must be one of pose, depth, canny"},
		{"missing control.image", SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512",
			Control: &storage.ControlParams{Net: "pose"}},
			"control.image is required"},
		{"control.strength range", SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512",
			Control: &storage.ControlParams{Net: "pose", Image: "x", Strength: 5}},
			"out of range 0.1..1.0"},
		{"missing ip_adapter.image", SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512",
			IPAdapter: &storage.IPAdapterParams{}},
			"ip_adapter.image is required"},
		{"ip_adapter.strength range", SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512",
			IPAdapter: &storage.IPAdapterParams{Image: "x", Strength: 1.5}},
			"out of range 0.1..1.0"},
	}
	for _, tc := range cases {
		_, err := m.Submit(tc.req)
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("%s: want ValidationError, got %v", tc.name, err)
		}
		if verr.Code != "validation_error" || verr.Msg != tc.wantMsg {
			t.Fatalf("%s: msg = %q, want %q", tc.name, verr.Msg, tc.wantMsg)
		}
	}
}

func TestSubmitConditioningEngineGate(t *testing.T) {
	o := testOpts(t)
	o.Models = sdxlCatalog()
	o.Ops.Generate = func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		return []byte("PNG"), nil, nil
	}
	m := New(o)
	req := func(model string) SubmitRequest {
		return SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512",
			Model:   model,
			Control: &storage.ControlParams{Net: "pose", Image: "x", Strength: 0.7}}
	}
	if _, err := m.Submit(req("persephone")); err == nil {
		t.Fatal("non-SDXL model must be rejected")
	} else if verr, ok := err.(*ValidationError); !ok || !strings.Contains(verr.Msg, "engine unsupported for control") {
		t.Fatalf("gate error = %v", err)
	}
	id, err := m.Submit(req("pony"))
	if err != nil {
		t.Fatalf("SDXL model accepted: %v", err)
	}
	waitFor(t, m, id, "done")
}

func TestSubmitConditioningDispatch(t *testing.T) {
	o := testOpts(t)
	o.Models = sdxlCatalog()
	var mu sync.Mutex
	var gotCtrl *storage.ControlParams
	var gotIPA *storage.IPAdapterParams
	o.Ops.Generate = func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
		mu.Lock()
		gotCtrl, gotIPA = ctrl, ipa
		mu.Unlock()
		return []byte("PNG"), nil, nil
	}
	m := New(o)
	s := int64(11)
	id, err := m.Submit(SubmitRequest{Genre: "landscape", Fields: map[string]string{"setting": "x"}, Size: "512x512",
		Model:   "pony",
		Control: &storage.ControlParams{Net: "canny", Image: "b64can", Strength: 0.5},
		IPAdapter: &storage.IPAdapterParams{Image: "b64ipa"},
		Seed:    &s})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	mu.Lock()
	if gotCtrl == nil || gotCtrl.Net != "canny" || gotCtrl.Image != "b64can" || gotCtrl.Strength != 0.5 {
		t.Fatalf("control reached dispatch = %+v", gotCtrl)
	}
	if gotIPA == nil || gotIPA.Image != "b64ipa" || gotIPA.Strength != 0.6 {
		t.Fatalf("ip_adapter reached dispatch with defaulted strength = %+v", gotIPA)
	}
	mu.Unlock()
	// The strength 0 → 0.6 default must be visible in the A3 tEXt params.
	if kv := renderParamsKV(storage.Job{ID: id, Mode: "generate"}, imageInputs{IPAdapter: gotIPA}); kv["ip_adapter"] != "0.60" {
		t.Fatalf("ip_adapter param = %q", kv["ip_adapter"])
	}
}

func TestRenderParamsKVConditioning(t *testing.T) {
	kv := renderParamsKV(storage.Job{ID: "j1", Mode: "generate"}, imageInputs{
		Control:   &storage.ControlParams{Net: "pose", Strength: 0.7},
		IPAdapter: &storage.IPAdapterParams{Image: "x", Strength: 0.6},
	})
	if kv["control"] != "pose/0.70" || kv["ip_adapter"] != "0.60" {
		t.Fatalf("conditioning params = %q / %q", kv["control"], kv["ip_adapter"])
	}
	// No b64 ever lands in tEXt: the control key carries net/strength only.
	for k, v := range kv {
		if strings.Contains(v, "b64can") {
			t.Fatalf("key %q leaked b64: %q", k, v)
		}
	}
	zero := renderParamsKV(storage.Job{ID: "j1", Mode: "generate"}, imageInputs{})
	if _, ok := zero["control"]; ok {
		t.Fatal("control key must be omit-when-empty")
	}
	if _, ok := zero["ip_adapter"]; ok {
		t.Fatal("ip_adapter key must be omit-when-empty")
	}
}
```

Update `internal/queue/queue_test.go` `editOps()` (line 324) and the fixture fake (line 56 in `testOpts`/`testOptsAt`) to the new signature — add `ctrl *storage.ControlParams, ipa *storage.IPAdapterParams` after `size string` in each; the bodies are unchanged.

Update `internal/queue/compose_test.go` the same way: the fake in `composeTestOpts` (line 70) and `sizedGen` (line 88-90) — its type and function signature both grow the two pointer params after `size string`:

```go
func sizedGen(t *testing.T, cap *genCapture) func(context.Context, string, string, *storage.ControlParams, *storage.IPAdapterParams, storage.ModelSpec, storage.SamplingParams) ([]byte, *int64, error) {
	t.Helper()
	return func(ctx context.Context, prompt, size string, ctrl *storage.ControlParams, ipa *storage.IPAdapterParams, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
```

Check `compose_test.go` imports include `"sync"` and `"encoding/json"` (they do) and `"errors"` (add if missing).

For `queue_test.go`, ensure its imports include `"errors"`, `"sync"`, and the models package (`img-gen/internal/models` — same import path style as the file's existing internal imports; add if missing).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/queue/ -run 'TestSubmitConditioning|TestRenderParamsKVConditioning'`
Expected: FAIL (validation missing; engine gate absent; renderParamsKV signature mismatch).

- [ ] **Step 3: SubmitRequest + imageInputs fields**

In `internal/queue/queue.go`, after line 40 (`Strengths []float64 \`json:"strengths"\``) insert:

```go
	// Control/IPAdapter condition generate-mode runs on reference images
	// (B1): SDXL-sidecar models only, mutually exclusive with the
	// edit-family modes. Both are opt-in; nil on both preserves the prior
	// request shape byte for byte.
	Control   *storage.ControlParams   `json:"control,omitempty"`
	IPAdapter *storage.IPAdapterParams `json:"ip_adapter,omitempty"`
```

In `imageInputs` (line 140-146), after the `Strength` field add:

```go
	// Control/IPAdapter carry the B1 generate-mode conditioning references.
	// Runtime-only like the other image inputs — never persisted on the job.
	Control   *storage.ControlParams
	IPAdapter *storage.IPAdapterParams
```

- [ ] **Step 4: generate-mode input capture + mutual exclusion + normalization**

In `submitOne`'s `case "generate"` (lines 442-454), after the `contains(g.Sizes, req.Size)` check, add:

```go
		inputs.Control = req.Control
		inputs.IPAdapter = req.IPAdapter
```

Between the mode switch's closing brace (line 521) and `if req.Style != ""` (line 523), insert:

```go
	// B1: control/ip-adapter only ever steer generate runs. The edit-family
	// modes own their init-image semantics, so the pair is mutually
	// exclusive with them — a request carrying both is rejected, never
	// silently deprioritized.
	if req.Control != nil || req.IPAdapter != nil {
		if mode != "generate" {
			return "", &ValidationError{Code: "validation_error", Msg: "control and ip_adapter require mode \"generate\""}
		}
		if verr := normalizeConditioning(req.Control, req.IPAdapter); verr != nil {
			return "", verr
		}
	}
```

After `normalizeLoras` (line 686), add:

```go
// normalizeConditioning validates the B1 conditioning structs and folds an
// omitted strength (0) onto the sd.cpp sidecar defaults (0.7 / 0.6), so every
// layer downstream — dispatch, tEXt params, sidecar payload — sees the
// effective value.
func normalizeConditioning(ctrl *storage.ControlParams, ipa *storage.IPAdapterParams) *ValidationError {
	if ctrl != nil {
		switch ctrl.Net {
		case "pose", "depth", "canny":
		default:
			return &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("control.net must be one of pose, depth, canny (got %q)", ctrl.Net)}
		}
		if ctrl.Image == "" {
			return &ValidationError{Code: "validation_error", Msg: "control.image is required"}
		}
		if ctrl.Strength == 0 {
			ctrl.Strength = 0.7
		} else if ctrl.Strength < 0.1 || ctrl.Strength > 1.0 {
			return &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("control.strength %v out of range 0.1..1.0", ctrl.Strength)}
		}
	}
	if ipa != nil {
		if ipa.Image == "" {
			return &ValidationError{Code: "validation_error", Msg: "ip_adapter.image is required"}
		}
		if ipa.Strength == 0 {
			ipa.Strength = 0.6
		} else if ipa.Strength < 0.1 || ipa.Strength > 1.0 {
			return &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("ip_adapter.strength %v out of range 0.1..1.0", ipa.Strength)}
		}
	}
	return nil
}
```

- [ ] **Step 5: Engine gate after resolveModelValues**

Immediately after the existing SDXL-mode gate (lines 565-567), insert:

```go
	// Conditioning is an SDXL-sidecar engine feature (B1): a non-SDXL model
	// with conditioning present is a validation error, never a silent drop.
	if (req.Control != nil || req.IPAdapter != nil) && req.Sidecar != "sdxl" {
		return "", &ValidationError{Code: "validation_error", Msg: "engine unsupported for control: control and ip_adapter apply to SDXL-sidecar models only"}
	}
```

- [ ] **Step 6: Dispatch + inputs persistence + renderParamsKV**

Update the inputs persistence block (lines 593-599) to:

```go
	m.mu.Lock()
	m.jobs[id] = job
	if mode != "generate" && mode != "asset" {
		m.inputs[id] = inputs
	} else if inputs.Control != nil || inputs.IPAdapter != nil {
		// B1: generate with conditioning — the references are runtime-only
		// request fields stored beside the other image inputs so dispatch
		// has them; every other generate input stays empty as before.
		m.inputs[id] = inputs
	}
	m.order = append(m.order, id)
	m.mu.Unlock()
```

Update `dispatch`'s `default` branch (lines 826-828) to:

```go
		default:
			if inputs.Control != nil {
				m.log(id, "control %s (strength=%.2f)", inputs.Control.Net, inputs.Control.Strength)
			}
			if inputs.IPAdapter != nil {
				m.log(id, "ip-adapter (strength=%.2f)", inputs.IPAdapter.Strength)
			}
			m.log(id, "requesting image from lattice (size=%s)", job.Size)
			png, seed, err = m.opts.Ops.Generate(ctx, prompt, job.Size, inputs.Control, inputs.IPAdapter, spec, sp)
```

Update `renderParamsKV` (lines 349-374): signature becomes `func renderParamsKV(job storage.Job, inputs imageInputs) map[string]string`, its doc comment gains "…and the B1 conditioning pair when present", and before `return kv` add:

```go
	if inputs.Control != nil {
		kv["control"] = fmt.Sprintf("%s/%.2f", inputs.Control.Net, inputs.Control.Strength)
	}
	if inputs.IPAdapter != nil {
		kv["ip_adapter"] = fmt.Sprintf("%.2f", inputs.IPAdapter.Strength)
	}
```

Update the two call sites: line 782 → `m.withRenderParams(png, renderParamsKV(*job, inputs))`; line 387 (in `saveRecovered`) → `m.withRenderParams(png, renderParamsKV(j, imageInputs{}))`.

- [ ] **Step 7: Run tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS everywhere.

- [ ] **Step 8: Commit**

```bash
git add internal/queue/queue.go internal/queue/queue_test.go internal/queue/compose_test.go
git commit -m "feat(b1): queue generates-with-conditioning — validation matrix, SDXL engine gate, dispatch + tEXt params

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 3: B3 — lattice `extension` serialization + unknown-field detection

**Files:**
- Modify: `internal/lattice/lattice.go` (latticeImage body)
- Test: `internal/lattice/lattice_test.go` (append new tests)

**Interfaces:**
- Consumes: Task 1's `latticeImage(..., ctrl, ipa)` signature.
- Produces: when lattice-routing and either reference is present, the request body gains a top-level `extension` object — `{"control": {"net","image","strength"}, "ip_adapter": {"image","strength"}}` with only the non-nil half present; a 400 response whose body mentions "extension" yields an error containing `upgrade the lattice gateway`. Top-level `control_net`/`ip_adapter` keys never appear on the lattice path.

- [ ] **Step 1: Write the failing tests**

Append to `internal/lattice/lattice_test.go`:

```go
func TestGenerateLatticeExtension(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("fake-png")) + `","seed":1}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.ImagesViaLattice = true
	ctrl := &storage.ControlParams{Net: "pose", Image: "UG9zZQ==", Strength: 0.7}
	ipa := &storage.IPAdapterParams{Image: "SWQ=", Strength: 0.6}
	if _, _, err := c.Generate(context.Background(), "p", "512x512", ctrl, ipa, storage.ModelSpec{Sidecar: "sdxl"}, storage.SamplingParams{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	ext, ok := got["extension"].(map[string]any)
	if !ok {
		t.Fatalf("extension missing from body: %v", got)
	}
	cn := ext["control"].(map[string]any)
	if cn["net"] != "pose" || cn["image"] != "UG9zZQ==" || cn["strength"] != 0.7 {
		t.Fatalf("extension.control = %v", cn)
	}
	ia := ext["ip_adapter"].(map[string]any)
	if ia["image"] != "SWQ=" || ia["strength"] != 0.6 {
		t.Fatalf("extension.ip_adapter = %v", ia)
	}
	for _, key := range []string{"control_net", "ip_adapter"} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s must ride extension on the lattice path, not top level", key)
		}
	}
}

func TestGenerateLatticeRejestsExtension(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"unknown field \"extension\""}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.ImagesViaLattice = true
	ctrl := &storage.ControlParams{Net: "pose", Image: "UG9zZQ==", Strength: 0.7}
	_, _, err := c.Generate(context.Background(), "p", "512x512", ctrl, nil, storage.ModelSpec{Sidecar: "sdxl"}, storage.SamplingParams{})
	if err == nil {
		t.Fatal("expected rejection error")
	}
	if !strings.Contains(err.Error(), "upgrade the lattice gateway") {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateLatticeBadRequestUnrelated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"unknown field \"loras\""}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.ImagesViaLattice = true
	_, _, err := c.Generate(context.Background(), "p", "512x512", nil, nil, storage.ModelSpec{Sidecar: "sdxl"}, storage.SamplingParams{})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "upgrade the lattice gateway") {
		t.Fatalf("unrelated 400 must keep the plain error, got: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/lattice/ -run 'TestGenerateLattice' -v`
Expected: FAIL (no extension written; no rejection detection).

- [ ] **Step 3: Write the extension block + detection**

In `latticeImage`, replace the comment line `// accepted but not sent.` from Task 1 and after the `if len(spec.Loras) > 0 {` block (keeping it), add (after the loras block, before `applyGenParams(body, sp)`):

```go
	if ctrl != nil || ipa != nil {
		// B3: the engine-optional fields ride one extension object so the
		// gateway gains a single relay contract. A gateway that predates the
		// field rejects the whole request with 400 — detect that and name the
		// upgrade instead of letting the fields drop silently.
		ext := map[string]any{}
		if ctrl != nil {
			cn := map[string]any{"net": ctrl.Net, "image": ctrl.Image}
			if ctrl.Strength != 0 {
				cn["strength"] = ctrl.Strength
			}
			ext["control"] = cn
		}
		if ipa != nil {
			ia := map[string]any{"image": ipa.Image}
			if ipa.Strength != 0 {
				ia["strength"] = ipa.Strength
			}
			ext["ip_adapter"] = ia
		}
		body["extension"] = ext
	}
```

And update the latticeImage doc comment's last sentence to: `ctrl/ipa carry the B1 conditioning references; they serialize onto the request's "extension" object when present.`

Replace the non-OK response branch (lines 128-131) with:

```go
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(msg)), "extension") {
			return nil, nil, fmt.Errorf("lattice %s: gateway rejected the extension field (%s) — upgrade the lattice gateway to one that relays engine extensions, or set IMAGE_ROUTING=direct", strings.TrimPrefix(path, "/v1/images/"), strings.TrimSpace(string(msg)))
		}
		return nil, nil, fmt.Errorf("lattice %s: %s: %s", strings.TrimPrefix(path, "/v1/images/"), resp.Status, msg)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS everywhere.

- [ ] **Step 5: Commit**

```bash
git add internal/lattice/lattice.go internal/lattice/lattice_test.go
git commit -m "feat(b3): serialize B1 references onto the lattice extension object with honest unknown-field detection

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 4: B2 — rembg sidecar client + REMBG_URL wiring

**Files:**
- Modify: `internal/lattice/lattice.go` (Client field + Rembg method after the Rife method, ~line 283)
- Modify: `internal/queue/queue.go` (ImageOps struct ~lines 95-116 — add the Rembg field)
- Modify: `cmd/img-gen/main.go:30-111` (config struct, loadConfig, newHandler nil-gate + Ops wiring)
- Modify: `deploy/com.img-gen.plist.in` (env block after the RIFE_URL block at lines 33-38)
- Modify: `deploy/install-macos.sh` (RIFE_URL block at 31-35 + sed chain at ~71)
- Test: `internal/lattice/lattice_test.go` (append)

**Interfaces:**
- Consumes: the Rife method's URL/error-shape conventions.
- Produces: `lattice.Client.RembgURL string` field; `func (c *Client) Rembg(ctx context.Context, imageB64 string) ([]byte, error)` — POST `{RembgURL}/segment` with `{"image": "<b64>"}`, decodes `{"png": "<b64>"}`, returns raw PNG bytes, errors wrapped `rembg sidecar: …`; `ImageOps.Rembg` field (nil = sidecar unconfigured — the gate Task 5 consumes); `REMBG_URL` env plumbed through config → lat → ImageOps → plist/installer.

- [ ] **Step 1: Write the failing test**

Append to `internal/lattice/lattice_test.go`:

```go
func TestRembg(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/segment" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"png":"` + base64.StdEncoding.EncodeToString([]byte("rgba-png")) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.RembgURL = srv.URL
	raw, err := c.Rembg(context.Background(), "SU1H")
	if err != nil {
		t.Fatalf("Rembg: %v", err)
	}
	if string(raw) != "rgba-png" {
		t.Fatalf("raw = %q", raw)
	}
	if got["image"] != "SU1H" {
		t.Fatalf("body = %v", got)
	}
}

func TestRembgSidecarError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model warmup", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New("http://unused")
	c.RembgURL = srv.URL
	if _, err := c.Rembg(context.Background(), "SU1H"); err == nil || !strings.Contains(err.Error(), "rembg sidecar") {
		t.Fatalf("err = %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/lattice/ -run 'TestRembg' -v`
Expected: FAIL (`c.RembgURL`/`c.Rembg` do not exist).

- [ ] **Step 3: Client field + method**

In `internal/lattice/lattice.go`, after the `RifeURL` field (line 27), add:

```go
	RembgURL     string // optional rembg u2netp sidecar for POST /segment background removal
```

(Assert alignment gofmt will fix — run gofmt after.)

After the `Rife` method's closing brace (line 283), add:

```go
// Rembg removes the background of imageB64 (PNG) on the rembg sidecar and
// returns the resulting RGBA PNG bytes. Errors carry the "rembg sidecar"
// prefix so they read as sidecar failures, matching the Rife convention.
func (c *Client) Rembg(ctx context.Context, imageB64 string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"image": imageB64})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.RembgURL, "/")+"/segment",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rembg sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rembg sidecar: HTTP %d", resp.StatusCode)
	}
	var out struct {
		PNG string `json:"png"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("rembg sidecar body: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(out.PNG)
	if err != nil {
		return nil, fmt.Errorf("rembg sidecar png: %w", err)
	}
	return raw, nil
}
```

- [ ] **Step 4: ImageOps.Rembg field**

In `internal/queue/queue.go`'s `ImageOps` struct (lines 95-116), after the `Rife` field, add:

```go
	// Rembg extracts an RGBA subject from one PNG via the rembg sidecar
	// (B2). nil = not configured — SubmitAssetSet refuses transparent
	// "rembg" assets with the honest validation error.
	Rembg func(ctx context.Context, imageB64 string) ([]byte, error)
```

- [ ] **Step 5: main.go wiring**

In `cmd/img-gen/main.go`:

1. In the `config` struct (line 30-53), after `RifeURL string` add `RembgURL string`.
2. In `loadConfig` (after line 64's `RifeURL:`), add `RembgURL: envOr("REMBG_URL", ""),`.
3. In `newHandler`, after line 100 (`lat.RifeURL = cfg.RifeURL`), add `lat.RembgURL = cfg.RembgURL`.
4. After the rifeOp nil-gate block (lines 105-111), add:

```go
	// Rembg is wired only when the sidecar is configured: nil Ops.Rembg must
	// unambiguously mean "rembg sidecar not configured" so asset-set submit
	// validation can refuse transparent "rembg" assets with the honest error.
	var rembgOp func(ctx context.Context, imageB64 string) ([]byte, error)
	if cfg.RembgURL != "" {
		rembgOp = lat.Rembg
	}
```

5. In the `Ops: queue.ImageOps{...}` literal, after `Rife:       rifeOp,` add `Rembg:      rembgOp,`.

- [ ] **Step 6: Deploy plumbing**

In `deploy/com.img-gen.plist.in`, after the RIFE_URL `<string>__RIFE_URL__</string>` (line 38), add:

```xml
        <!-- Route background-subject extraction to the rembg sidecar (u2netp
             weights). transparent "rembg" compose assets need it; leaving it
             unset makes those submissions fail validation honestly. Placeholder,
             substituted by install-macos.sh, for the same reason as __RIFE_URL__. -->
        <key>REMBG_URL</key>
        <string>__REMBG_URL__</string>
```

In `deploy/install-macos.sh`, after line 35 (`RIFE_URL="${RIFE_URL:-}"`), add:

```bash
# Optional: the rembg sidecar for background removal (u2netp alpha path).
#   REMBG_URL=http://<rembg-sidecar>:<port> ./deploy/install-macos.sh
REMBG_URL="${REMBG_URL:-}"
```

And in the sed chain (after the `__RIFE_URL__` substitution at line 71), add:

```bash
    -e "s|__REMBG_URL__|$REMBG_URL|g" \
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go vet ./... && go test ./... && go build ./...`
Expected: PASS everywhere.

- [ ] **Step 8: Commit**

```bash
git add internal/lattice/lattice.go internal/lattice/lattice_test.go internal/queue/queue.go cmd/img-gen/main.go deploy/com.img-gen.plist.in deploy/install-macos.sh
git commit -m "feat(b2): rembg sidecar client + REMBG_URL env wiring (config, plist, installer)

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 5: B2 — TransparentSpec tri-state + rembg submit gates + executor branch

**Files:**
- Modify: `internal/queue/compose.go` (TransparentSpec near assetSpec ~line 37; AssetSpecIn ~line 359; SubmitAssetSet ~lines 426-519; elementChildren 527-540; composeAssetSet composition switch 685-722)
- Modify: `internal/queue/compose_test.go` (TestSubmitAssetSetTransparent literal fix + new tests)

**Interfaces:**
- Consumes: Task 4's `ImageOps.Rembg` nil-gate.
- Produces: `type TransparentSpec string` with `TransparentDual = "dual"`, `TransparentRembg = "rembg"`; `assetSpec.Transparent` and `AssetSpecIn.Transparent` switch to `TransparentSpec` (JSON `true` → "dual", `false`/`null`/absent/`""` → "", any JSON string stored verbatim for submit validation); `assetSpec.Transparent == TransparentRembg` in the executor = rembg branch reusing `ComposeElement/ComposeFrame/ComposeBar` with `transparent=false` and the extracted RGBA as the white arg. Submission rule: rembg allowed on element, frame, bar only; requires `Ops.Rembg` wired. elementChildren gains no bg suffix for rembg (plain prompt — rembg expects natural scenes).

- [ ] **Step 1: Write the failing tests**

In `internal/queue/compose_test.go`:

Patch the existing `TestSubmitAssetSetTransparent` — its literal `Transparent: true` becomes `Transparent: TransparentDual`.

Append:

```go
func TestTransparentSpecParse(t *testing.T) {
	cases := []struct {
		in   string
		want TransparentSpec
	}{
		{`{"transparent":true}`, TransparentDual},
		{`{"transparent":false}`, ""},
		{`{"transparent":null}`, ""},
		{`{}`, ""},
		{`{"transparent":""}`, ""},
		{`{"transparent":"rembg"}`, TransparentRembg},
		{`{"transparent":"dual"}`, TransparentDual},
		// Unknown strings survive decode verbatim so submit validation can
		// return a proper error message instead of a 400 decode failure.
		{`{"transparent":"wat"}`, "wat"},
	}
	for _, tc := range cases {
		var in AssetSpecIn
		if err := json.Unmarshal([]byte(tc.in), &in); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if in.Transparent != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.in, in.Transparent, tc.want)
		}
	}
}

func TestSubmitAssetSetRembgValidation(t *testing.T) {
	seed := int64(7)
	base := func() AssetSetRequest {
		return AssetSetRequest{Name: "star", Seed: &seed, Assets: []AssetSpecIn{{
			Kind: "element", Name: "star", Prompt: "star icon", Size: "32x32", Transparent: TransparentRembg,
		}}}
	}
	// No Ops.Rembg wired → honest validation error naming the env.
	m := New(composeTestOpts(t, t.TempDir(), &genCapture{}))
	_, err := m.SubmitAssetSet(base())
	verr, ok := err.(*ValidationError)
	if !ok || !strings.Contains(verr.Msg, "REMBG_URL") {
		t.Fatalf("err = %v", err)
	}
	// rembg is element/frame/bar only. kit/atlas fixtures pass their own kind
	// validations (kit has none; atlas needs Frames=2, npc needs a Shape) and
	// reach the rembg kind gate; bogus never leaves the kind switch.
	o := composeTestOpts(t, t.TempDir(), &genCapture{})
	o.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) { return nil, nil }
	m2 := New(o)
	kinds := map[string]string{
		"kit":   `transparent "rembg" is supported for element, frame and bar`,
		"atlas": `transparent "rembg" is supported for element, frame and bar`,
		"npc":   `transparent "rembg" is supported for element, frame and bar`,
		"bogus": `unknown kind "bogus"`,
	}
	for kind, want := range kinds {
		req := base()
		req.Assets[0].Kind = kind
		if kind == "atlas" {
			req.Assets[0].Frames = 2
		}
		if kind == "npc" {
			req.Assets[0].Shape = "walk4"
		}
		_, err := m2.SubmitAssetSet(req)
		verr, ok := err.(*ValidationError)
		if !ok || !strings.Contains(verr.Msg, want) {
			t.Fatalf("kind %s: err = %v, want substring %q", kind, err, want)
		}
	}
	// Unknown transparent value → validation error, not decode failure.
	req := base()
	req.Assets[0].Transparent = "wat"
	_, err = m2.SubmitAssetSet(req)
	verr, ok = err.(*ValidationError)
	if !ok || !strings.Contains(verr.Msg, `transparent must be true, false, or "rembg"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitAssetSetRembgSingleChild(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	rembgImg := image.NewNRGBA(image.Rect(0, 0, 8, 8)) // fully transparent
	var rb bytes.Buffer
	if err := png.Encode(&rb, rembgImg); err != nil {
		t.Fatal(err)
	}
	rembgCalled := 0
	o.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) {
		rembgCalled++
		return rb.Bytes(), nil
	}
	m := New(o)
	seed := int64(7)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "star", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "element", Name: "star", Prompt: "star icon", Size: "32x32", Transparent: TransparentRembg}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 1 {
		t.Fatalf("rembg renders once, got %q", cap.Prompts)
	}
	// The rembg child has a plain prompt — no "isolated on white" suffix;
	// U²-Net works best on natural backgrounds.
	if strings.Contains(cap.Prompts[0], "white") || strings.Contains(cap.Prompts[0], "black") {
		t.Fatalf("rembg prompt carries no bg suffix: %q", cap.Prompts[0])
	}
	cap.mu.Unlock()
	if rembgCalled != 1 {
		t.Fatalf("rembg calls = %d", rembgCalled)
	}
	data, err := m.opts.Store.ReadSetFile(id, "star.png")
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, a := im.At(0, 0).RGBA(); a != 0 {
		t.Fatalf("rembg output must keep alpha, got a=%d", a)
	}
}

func TestSubmitAssetSetRembgBarKeepsFill(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	rembgImg := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	var rb bytes.Buffer
	if err := png.Encode(&rb, rembgImg); err != nil {
		t.Fatal(err)
	}
	o.Ops.Rembg = func(ctx context.Context, imageB64 string) ([]byte, error) { return rb.Bytes(), nil }
	m := New(o)
	seed := int64(7)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hp", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "bar", Name: "hp", Prompt: "steel frame", Size: "32x32",
			Transparent: TransparentRembg, FrameMargins: 4, BarFill: "red potion fill"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.Prompts) != 2 {
		t.Fatalf("rembg bar = track + fill renders, got %q", cap.Prompts)
	}
	if !strings.Contains(cap.Prompts[0], "steel frame") || strings.Contains(cap.Prompts[0], "white") {
		t.Fatalf("track prompt plain: %q", cap.Prompts[0])
	}
	if !strings.Contains(cap.Prompts[1], "red potion fill") {
		t.Fatalf("fill prompt: %q", cap.Prompts[1])
	}
	if _, err := m.opts.Store.ReadSetFile(id, "hp-track.png"); err != nil {
		t.Fatalf("track file: %v", err)
	}
	if _, err := m.opts.Store.ReadSetFile(id, "hp-fill.png"); err != nil {
		t.Fatalf("fill file: %v", err)
	}
}
```

Check imports: compose_test.go already has bytes/context/image/png/strings.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/queue/ -run 'TestTransparentSpecParse|TestSubmitAssetSetRembg|TestSubmitAssetSetTransparent$'`
Expected: FAIL (TransparentSpec undefined; tests referencing rembg fail).

- [ ] **Step 3: Implement TransparentSpec**

In `internal/queue/compose.go`, immediately before `type assetSpec struct` (line 37), add:

```go
// TransparentSpec selects how an asset's transparency is produced. "" (the
// default) renders one opaque image; TransparentDual renders white and black
// background siblings and unmixes them (the classic double pass);
// TransparentRembg extracts the subject from the single plain render with the
// rembg sidecar. JSON true parses as "dual" so pre-B2 clients and persisted
// specs keep working; unknown JSON strings are stored verbatim and rejected
// by submit validation with a proper envelope message instead of a decode
// error.
type TransparentSpec string

const (
	TransparentDual  TransparentSpec = "dual"
	TransparentRembg TransparentSpec = "rembg"
)

func (t *TransparentSpec) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "null" || s == `""`:
		*t = ""
	case s == "true":
		*t = TransparentDual
	case s == "false":
		*t = ""
	default:
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			*t = TransparentSpec(s)
			return nil
		}
		*t = TransparentSpec(str)
	}
	return nil
}
```

Change `assetSpec.Transparent bool` (line 44) to:

```go
	Transparent  TransparentSpec `json:"transparent,omitempty"` // "" | "dual" | "rembg"
```

and `AssetSpecIn.Transparent bool` (line 359) to:

```go
	Transparent  TransparentSpec `json:"transparent,omitempty"` // true = dual pass (legacy spell); "dual"; "rembg" = u2netp sidecar
```

- [ ] **Step 4: Submit gates**

In `SubmitAssetSet`, immediately after the meta_formats validation block (after line 425's closing brace) and before `var built []SubmitRequest` (line 426), insert:

```go
		// B2: transparency production. Unknown values are rejected here so the
		// message lands in the v1 error envelope, not a 400 decode failure;
		// the rembg kind gate runs after the kind switch so unknown kinds
		// report the kind error.
		if a.Transparent != "" && a.Transparent != TransparentDual && a.Transparent != TransparentRembg {
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: transparent must be true, false, or %q", an, string(TransparentRembg))}
		}
```

Then at the END of the kind switch — i.e. restructure the `default:` case so it stays first-in-line for unknown kinds, and add the rembg kind gate right after the switch closes (after line 509's closing brace, before the model seeding loop at line 510):

```go
		if a.Transparent == TransparentRembg {
			switch a.Kind {
			case "element", "frame", "bar":
			default:
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: transparent %q is supported for element, frame and bar", an, string(TransparentRembg))}
			}
			if m.opts.Ops.Rembg == nil {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: transparent %q requires the rembg sidecar (REMBG_URL)", an, string(TransparentRembg))}
			}
		}
```

(The exact insertion: between line 509's `}` closing the switch and line 510's comment `// The request-level model seeds every child of the set;`.)

- [ ] **Step 5: elementChildren + executor**

Replace `elementChildren` (lines 527-540) with:

```go
// elementChildren builds the child jobs an element-like asset renders: one
// plain render (opaque, or rembg-extracted later at compose time), or the
// classic white and black background pair (dual) sharing the seed. rembg gets
// the plain prompt on purpose — U²-Net works best on natural backgrounds.
func elementChildren(prompt, negative, size string, seed int64, t TransparentSpec) []SubmitRequest {
	if t != TransparentDual {
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
```

In `SubmitAssetSet`'s `case "bar":` (line 486-487), the fill children call becomes:

```go
			built = append(elementChildren(prompt, a.Negative, a.Size, *req.Seed, a.Transparent),
				elementChildren(a.BarFill, a.Negative, a.Size, *req.Seed, "")...)
```

(the only change is `false` → `""`).

In `composeAssetSet`'s composition block, insert the rembg pass right after `if broken == "" {` (line 682), before `var outs []compose.OutputFile` (line 683):

```go
			// B2 rembg branch: extract the subject from the first render,
			// leaving any sibling render (the bar fill) untouched. Dual
			// assets never enter this path — their children arrive in pairs.
			if a.Transparent == TransparentRembg {
				if m.opts.Ops.Rembg == nil {
					broken = "rembg: sidecar not wired (REMBG_URL)"
				} else {
					rgba, rerr := m.opts.Ops.Rembg(context.Background(), imgs[0])
					if rerr != nil {
						broken = fmt.Sprintf("rembg: %v", rerr)
					} else {
						imgs[0] = rgba
					}
				}
			}
```

Then update the composition switch conditions (lines 689, 703, 706, 710, 713):

- `case "element":` condition `if a.Transparent && len(imgs) == 2 {` → `if a.Transparent == TransparentDual && len(imgs) == 2 {`
- `case "frame":` block becomes:

```go
			case "frame":
				black := []byte(nil)
				if a.Transparent == TransparentDual && len(imgs) == 2 {
					black = imgs[1]
				}
				outs, err = compose.ComposeFrame(a.Name, imgs[0], black, a.Transparent == TransparentDual && len(imgs) == 2, a.FrameMargins)
```

- `case "bar":` block becomes:

```go
			case "bar":
				fill := imgs[len(imgs)-1]
				trackBlack := []byte(nil)
				if a.Transparent == TransparentDual && len(imgs) == 3 {
					trackBlack = imgs[1]
				}
				outs, err = compose.ComposeBar(a.Name, imgs[0], trackBlack, a.Transparent == TransparentDual && len(imgs) == 3, a.FrameMargins, fill, a.FillStates)
```

(The extracted RGBA flows through the `imgs[0]`(=white)/`transparent=false` path, which preserves alpha — zero changes to the compose package.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS everywhere — including the untouched `TestSubmitAssetSetTransparent` (now on `TransparentDual`) and every other compose test.

- [ ] **Step 7: Commit**

```bash
git add internal/queue/compose.go internal/queue/compose_test.go
git commit -m "feat(b2): transparent tri-state — rembg pass with submit gates and executor branch, dual via legacy true kept parsing

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 6: B4 — compose sheet.go + EmitMeta sheet support

**Files:**
- Create: `internal/compose/sheet.go`
- Modify: `internal/compose/meta.go` (MetaFormats 16-21, EmitMeta switch 48-54 + final error, emitStripMeta 72-80)
- Test: `internal/compose/sheet_test.go` (create)

**Interfaces:**
- Consumes: `PackGrid`, `MarshalAtlas`, `OutputFile` (atlas.go / compose.go).
- Produces: `type sheetCell struct { View string; Img image.Image }`; `func ComposeSheet(name string, cells []sheetCell, columns int) ([]OutputFile, error)` → `[{"<name>-sheet.png", primary}, {"<name>-sheet.json"}]` with atlas-JSON frames keyed by view name; `MetaFormats` phaser/unity/sparrow lists gain `"sheet"`; `EmitMeta` routes kind `"sheet"` to `emitStripMeta`, which probes `-sheet.json` for sheet (`-atlas.json` otherwise; the walk-vs-sheet probe stays npc-only).

- [ ] **Step 1: Write the failing tests**

Create `internal/compose/sheet_test.go`:

```go
package compose

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"bytes"
	"strings"
	"testing"
)

func solid(w, h int, c color.Color) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBAModel.Convert(c).(color.NRGBA))
		}
	}
	return img
}

func TestComposeSheet(t *testing.T) {
	cells := []sheetCell{
		{View: "front", Img: solid(16, 16, color.RGBA{R: 255, A: 255})},
		{View: "side", Img: solid(16, 16, color.RGBA{G: 255, A: 255})},
		{View: "back", Img: solid(16, 16, color.RGBA{B: 255, A: 255})},
		{View: "casting", Img: solid(16, 16, color.RGBA{R: 255, B: 255, A: 255})},
	}
	files, err := ComposeSheet("hero", cells, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d", len(files))
	}
	if files[0].Name != "hero-sheet.png" || !files[0].Primary {
		t.Fatalf("png file = %+v", files[0])
	}
	if files[1].Name != "hero-sheet.json" || files[1].Primary {
		t.Fatalf("json file = %+v", files[1])
	}
	im, err := png.Decode(bytes.NewReader(files[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	if b := im.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
		t.Fatalf("sheet dims = %dx%d", b.Dx(), b.Dy())
	}
	var doc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(files[1].Data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range cells {
		fr, ok := doc.Frames[c.View]
		if !ok {
			t.Fatalf("view %q missing from %v", c.View, doc.Frames)
		}
		if fr.Frame.W != 16 || fr.Frame.H != 16 {
			t.Fatalf("view %q rect = %+v", c.View, fr.Frame)
		}
	}
	if doc.Frames["front"].Frame.X != 0 || doc.Frames["front"].Frame.Y != 0 {
		t.Fatalf("front rect = %+v", doc.Frames["front"].Frame)
	}
	if doc.Frames["side"].Frame.X != 16 || doc.Frames["side"].Frame.Y != 0 {
		t.Fatalf("side rect = %+v", doc.Frames["side"].Frame)
	}
	if doc.Frames["back"].Frame.X != 0 || doc.Frames["back"].Frame.Y != 16 {
		t.Fatalf("back rect = %+v", doc.Frames["back"].Frame)
	}
}

func TestComposeSheetOneRowDefault(t *testing.T) {
	cells := []sheetCell{
		{View: "front", Img: solid(16, 16, color.RGBA{A: 255})},
		{View: "side", Img: solid(16, 16, color.RGBA{A: 255})},
		{View: "back", Img: solid(16, 16, color.RGBA{A: 255})},
	}
	files, err := ComposeSheet("hero", cells, 0)
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(files[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	if b := im.Bounds(); b.Dx() != 48 || b.Dy() != 16 {
		t.Fatalf("one-row dims = %dx%d", b.Dx(), b.Dy())
	}
	var doc struct {
		Frames map[string]struct {
			Frame Rect `json:"frame"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(files[1].Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Frames["back"].Frame.Y != 0 {
		t.Fatalf("back row = %+v", doc.Frames["back"].Frame)
	}
}

func TestComposeSheetValidation(t *testing.T) {
	if _, err := ComposeSheet("hero", nil, 0); err == nil {
		t.Fatal("empty cells")
	}
	if _, err := ComposeSheet("hero", []sheetCell{{View: "", Img: solid(1, 1, color.Black)}}, 0); err == nil || !strings.Contains(err.Error(), "view") {
		t.Fatalf("empty view name: %v", err)
	}
	if _, err := ComposeSheet("hero", []sheetCell{{View: "front"}}, 0); err == nil || !strings.Contains(err.Error(), "render") {
		t.Fatalf("nil img: %v", err)
	}
}
```

And in `internal/compose/meta_test.go` (existing file — append):

```go
func TestEmitMetaSheet(t *testing.T) {
	cells := []sheetCell{
		{View: "front", Img: solid(16, 16, color.RGBA{R: 255, A: 255})},
		{View: "side", Img: solid(16, 16, color.RGBA{G: 255, A: 255})},
	}
	outs, err := ComposeSheet("hero", cells, 0)
	if err != nil {
		t.Fatal(err)
	}
	metas, err := EmitMeta("sheet", "hero", []string{"phaser", "unity", "sparrow"}, outs)
	if err != nil {
		t.Fatalf("sheet meta: %v", err)
	}
	got := map[string]bool{}
	for _, m := range metas {
		got[m.Name] = true
	}
	for _, want := range []string{"hero.phaser.json", "hero.frames.json", "hero.sparrow.xml"} {
		if !got[want] {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
	if _, err := EmitMeta("sheet", "hero", []string{"tiled"}, outs); err == nil {
		t.Fatal("tiled must be rejected for sheet")
	}
	if _, err := EmitMeta("sheet", "hero", []string{"phaser"}, []OutputFile{{Name: "hero-sheet.png", Data: []byte("not-a-real-png")}}); err == nil {
		t.Fatal("missing -sheet.json must error")
	}
	if _, ok := MetaFormats["phaser"]; !ok {
		t.Fatal("phaser missing")
	}
	supported := false
	for _, k := range MetaFormats["phaser"] {
		if k == "sheet" {
			supported = true
		}
	}
	if !supported {
		t.Fatalf("phaser list must include sheet: %v", MetaFormats["phaser"])
	}
}
```

(If `meta_test.go` lacks the `image/color` import, add it.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/compose/ -run 'TestComposeSheet|TestEmitMetaSheet' -v`
Expected: FAIL (`sheetCell`/`ComposeSheet` undefined; phaser list lacks "sheet").

- [ ] **Step 3: Implement sheet.go**

Create `internal/compose/sheet.go`:

```go
package compose

import (
	"errors"
	"fmt"
	"image"
)

// sheetCell is one named view render on a character sheet.
type sheetCell struct {
	View string
	Img  image.Image
}

// ComposeSheet packs named view renders onto one uniform grid sheet and emits
// sheet metadata JSON naming each cell's view (the atlas.json contract, keyed
// by view for its different consumers). columns <= 0 lays one row. The PNG is
// <name>-sheet.png, the metadata <name>-sheet.json.
func ComposeSheet(name string, cells []sheetCell, columns int) ([]OutputFile, error) {
	if len(cells) == 0 {
		return nil, errors.New("compose: no sheet cells")
	}
	for i, c := range cells {
		if c.View == "" {
			return nil, fmt.Errorf("compose: sheet %q cell %d has no view name", name, i)
		}
		if c.Img == nil {
			return nil, fmt.Errorf("compose: sheet %q view %q render missing", name, c.View)
		}
	}
	frames := make([]image.Image, len(cells))
	for i, c := range cells {
		frames[i] = c.Img
	}
	sheet, metas, err := PackGrid(name, frames, columns)
	if err != nil {
		return nil, err
	}
	for i, c := range cells {
		metas[i].Name = c.View
	}
	metaJSON, err := MarshalAtlas(metas)
	if err != nil {
		return nil, err
	}
	return []OutputFile{
		{Name: name + "-sheet.png", Data: sheet, Primary: true},
		{Name: name + "-sheet.json", Data: metaJSON},
	}, nil
}
```

- [ ] **Step 4: EmitMeta sheet support**

In `internal/compose/meta.go`:

1. `MetaFormats` (lines 16-21) becomes:

```go
var MetaFormats = map[string][]string{
	"phaser":  {"atlas", "npc", "sheet"},
	"unity":   {"atlas", "npc", "sheet"},
	"sparrow": {"atlas", "npc", "sheet"},
	"tiled":   {"kit"},
}
```

2. `EmitMeta`'s switch (lines 48-54): `case "atlas", "npc":` → `case "atlas", "npc", "sheet":`; the final error becomes `fmt.Errorf("meta formats apply to atlas, npc, sheet, kit — not %q", kind)`.

3. `emitStripMeta`'s probe (line 73) becomes:

```go
	metaSuffix := "-atlas.json"
	if kind == "sheet" {
		metaSuffix = "-sheet.json"
	}
	aj, ok := findFile(outs, metaSuffix)
	if !ok {
		return nil, fmt.Errorf("%s missing for %q", metaSuffix, name)
	}
```

and the decode error message (line 79) becomes `fmt.Errorf("%s: %w", metaSuffix, err)`. Update the function's doc comment: "reads <name>-atlas.json (sheet: -sheet.json) + locates the sheet and emits one file per requested format."

- [ ] **Step 5: Run tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS everywhere.

- [ ] **Step 6: Commit**

```bash
git add internal/compose/sheet.go internal/compose/sheet_test.go internal/compose/meta.go internal/compose/meta_test.go
git commit -m "feat(b4): compose sheet — named-view grid packer + EmitMeta sheet family

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 7: B4 — queue sheet kind (validation, children, executor, manifest)

**Files:**
- Modify: `internal/queue/compose.go` (AssetSpecIn after Variants line 364; assetSpec after Variants line 49; SubmitAssetSet kind switch before `default` line 507; unknown-kind message line 508; sheetChildren after npcChildren ~587; composeAssetSet manifest init line 660 + executor case after `case "npc":` at 714-719)
- Test: `internal/queue/compose_test.go` (append)

**Interfaces:**
- Consumes: Task 6's `compose.sheetCell` / `compose.ComposeSheet` (sheet kind composes through the atlas family → `meta_formats: ["phaser"|"unity"|"sparrow"]` works on sheet with zero extra code); `assetSpec.Views`.
- Produces: `AssetSpecIn.Views []string` + `assetSpec.Views []string` (JSON `views`); `sheetChildren(prompt, negative, size string, seed int64, views []string) []SubmitRequest` — one job per view, all at the pinned set seed, prompt `prompt + ", " + view + " view"`; executor `case "sheet":` decoding child renders into cells and calling `compose.ComposeSheet(a.Name, cells, a.Columns)`; `manifestAsset.Views []string`.
- **Excluded by ruling:** the spec's "plus whatever control tools Phase B provides" sentence does not add a control field to asset specs — B1's contract puts `control`/`ip_adapter` on generate/job submission only, and the B4 request contract (views/size/columns) is the complete sheet shape. Pose-locked views go through per-view generate calls until an asset-level control field ships.

- [ ] **Step 1: Write the failing tests**

Append to `internal/queue/compose_test.go`:

```go
func TestSubmitAssetSetSheet(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap) // real sizes for the grid packer
	m := New(o)
	seed := int64(42)
	views := []string{"front", "side", "back", "casting"}
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hero", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "sheet", Name: "hero", Prompt: "knight", Size: "16x16", Columns: 2, Views: views}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	cap.mu.Lock()
	if len(cap.Prompts) != 4 {
		t.Fatalf("4 children, got %q", cap.Prompts)
	}
	if cap.Seeds[0] != 42 || cap.Seeds[1] != 42 || cap.Seeds[2] != 42 || cap.Seeds[3] != 42 {
		t.Fatalf("every view shares the pinned seed: %v", cap.Seeds)
	}
	for i, want := range views {
		if !strings.Contains(cap.Prompts[i], ", "+want+" view") {
			t.Fatalf("prompt %d missing view suffix: %q", i, cap.Prompts[i])
		}
	}
	cap.mu.Unlock()
	if _, err := m.opts.Store.ReadSetFile(id, "hero-sheet.png"); err != nil {
		t.Fatalf("sheet png: %v", err)
	}
	sj, err := m.opts.Store.ReadSetFile(id, "hero-sheet.json")
	if err != nil {
		t.Fatalf("sheet json: %v", err)
	}
	for _, v := range views {
		if !strings.Contains(string(sj), `"`+v+`"`) {
			t.Fatalf("sheet json missing view %q: %s", v, sj)
		}
	}
	// manifest records the views
	mj, err := m.opts.Store.ReadSetFile(id, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mj), `"front"`) || !strings.Contains(string(mj), `"views"`) {
		t.Fatalf("manifest views: %s", mj)
	}
}

func TestSubmitAssetSetSheetValidation(t *testing.T) {
	seed := int64(1)
	mk := func(a AssetSpecIn) AssetSetRequest {
		return AssetSetRequest{Name: "hero", Seed: &seed, Assets: []AssetSpecIn{a}}
	}
	base := AssetSpecIn{Kind: "sheet", Name: "hero", Prompt: "knight", Size: "16x16"}
	m := New(composeTestOpts(t, t.TempDir(), nil))
	cases := []struct {
		name string
		set  func(*AssetSpecIn)
		want string
	}{
		{"one view", func(a *AssetSpecIn) { a.Views = []string{"front"} }, "sheet views takes 2..8"},
		{"nine views", func(a *AssetSpecIn) { a.Views = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} }, "sheet views takes 2..8"},
		{"empty view", func(a *AssetSpecIn) { a.Views = []string{"front", " "} }, "sheet view 1 is empty"},
		{"dup view", func(a *AssetSpecIn) { a.Views = []string{"front", "front"} }, `duplicate sheet view "front"`},
		{"columns too many", func(a *AssetSpecIn) { a.Views = []string{"front", "side"}; a.Columns = 3 }, "columns must be 0 (single row) or 1..2"},
		{"columns negative", func(a *AssetSpecIn) { a.Views = []string{"front", "side"}; a.Columns = -1 }, "columns must be 0 (single row) or 1..2"},
	}
	for _, tc := range cases {
		req := mk(base)
		tc.set(&req.Assets[0])
		_, err := m.SubmitAssetSet(req)
		verr, ok := err.(*ValidationError)
		if !ok || !strings.Contains(verr.Msg, tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if verr.Code != "validation_error" {
			t.Fatalf("%s: code = %q", tc.name, verr.Code)
		}
	}
}

func TestSubmitAssetSetSheetOneRowDefault(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(9)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hero", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "sheet", Name: "hero", Prompt: "knight", Size: "16x16", Views: []string{"front", "side", "back"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	data, err := m.opts.Store.ReadSetFile(id, "hero-sheet.png")
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := im.Bounds(); b.Dx() != 48 || b.Dy() != 16 {
		t.Fatalf("one-row sheet dims = %dx%d", b.Dx(), b.Dy())
	}
}

func TestSubmitAssetSetSheetMetaFormats(t *testing.T) {
	cap := &genCapture{}
	o := composeTestOpts(t, t.TempDir(), cap)
	o.Ops.Generate = sizedGen(t, cap)
	m := New(o)
	seed := int64(5)
	id, err := m.SubmitAssetSet(AssetSetRequest{
		Name: "hero", Seed: &seed,
		Assets: []AssetSpecIn{{Kind: "sheet", Name: "hero", Prompt: "knight", Size: "16x16",
			Views: []string{"front", "side"}, MetaFormats: []string{"phaser", "unity", "sparrow"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, id, "done")
	for _, f := range []string{"hero.phaser.json", "hero.frames.json", "hero.sparrow.xml"} {
		if _, err := m.opts.Store.ReadSetFile(id, f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/queue/ -run 'TestSubmitAssetSetSheet' -v`
Expected: FAIL (kind sheet unknown).

- [ ] **Step 3: Implement the kind**

In `internal/queue/compose.go`:

1. `assetSpec.Variaants` — after line 49 (`Variants []string ...`), add:

```go
	Views       []string `json:"views,omitempty"`    // sheet: 2..8 view names
```

(after `Variants`; gofmt alignment pass after).

2. `AssetSpecIn`: after line 364 (`Variants ...`), add:

```go
	Views       []string `json:"views,omitempty"`    // sheet: 2..8 view names
```

3. In the kind switch, before `default:` (line 507), add the case:

```go
		case "sheet":
			if len(a.Views) < 2 || len(a.Views) > 8 {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: sheet views takes 2..8 view names", an)}
			}
			seenV := map[string]bool{}
			for i, v := range a.Views {
				if strings.TrimSpace(v) == "" {
					return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: sheet view %d is empty", an, i)}
				}
				if seenV[v] {
					return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: duplicate sheet view %q", an, v)}
				}
				seenV[v] = true
			}
			if a.Columns < 0 || a.Columns > len(a.Views) {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: columns must be 0 (single row) or 1..%d", an, len(a.Views))}
			}
			built = sheetChildren(prompt, a.Negative, a.Size, *req.Seed, a.Views)
			as = assetSpec{Kind: a.Kind, Name: an, Prompt: prompt, Size: a.Size, Views: a.Views, Columns: a.Columns, MetaFormats: a.MetaFormats}
```

4. The unknown-kind message (line 508) becomes:

```go
			return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: unknown kind %q (want element, kit, atlas, frame, bar, npc, sheet)", an, a.Kind)}
```

5. After `npcChildren` (after line 587), add:

```go
// sheetChildren builds one job per character-sheet view, ALL at the pinned
// set seed — the npc-variants mechanic: same composition scaffold, per-view
// content.
func sheetChildren(prompt, negative, size string, seed int64, views []string) []SubmitRequest {
	kids := make([]SubmitRequest, 0, len(views))
	for _, v := range views {
		s := seed
		kids = append(kids, SubmitRequest{Mode: "asset", Prompt: prompt + ", " + v + " view", NegativePrompt: negative, Size: size, Seed: &s})
	}
	return kids
}
```

6. Manifest init (line 660): `ma := manifestAsset{Kind: a.Kind, Name: a.Name, Prompt: a.Prompt, Size: a.Size, Variants: a.Variants}` gains `Views: a.Views`:

```go
		ma := manifestAsset{Kind: a.Kind, Name: a.Name, Prompt: a.Prompt, Size: a.Size, Variants: a.Variants, Views: a.Views}
```

7. `manifestAsset` (lines 617-625): after `Variants`, add:

```go
	Views    []string `json:"views,omitempty"`
```

8. In `composeAssetSet`'s composition switch, after the `case "npc":` block (after line 719's closing brace), add:

```go
			case "sheet":
				if len(imgs) != len(a.Views) {
					err = fmt.Errorf("sheet %q: expected %d render(s), found %d", a.Name, len(a.Views), len(imgs))
					break
				}
				cells := make([]compose.sheetCell, len(imgs))
				for i, v := range a.Views {
					img, derr := png.Decode(bytes.NewReader(imgs[i]))
					if derr != nil {
						err = fmt.Errorf("sheet %q view %q is not a PNG: %v", a.Name, v, derr)
						break
					}
					cells[i] = compose.sheetCell{View: v, Img: img}
				}
				if err == nil {
					outs, err = compose.ComposeSheet(a.Name, cells, a.Columns)
				}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go vet ./... && go test ./...`
Expected: PASS everywhere.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/compose.go internal/queue/compose_test.go
git commit -m "feat(b4): queue sheet kind — pinned-seed view children, executor pack, manifest views

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```

---

### Task 8: docs sweep (openapi, MCP, README) + full verification + smoke

**Files:**
- Modify: `docs/openapi.yaml` (SubmitRequest schema; new ControlParams/IPAdapterParams schemas; AssetSpecIn transparent/views; kinds list)
- Modify: `cmd/img-gen-mcp/main.go` (submit_job schema string ~line 75; asset_sets description ~line 105)
- Modify: `README.md` (env list ~34-36; generate/modes section; asset kinds list ~165+; meta_formats table ~248-267)
- Test: none new — full suite + smoke

**Interfaces:**
- Consumes: everything shipped in Tasks 1-7.
- Produces: documented contracts only — no behavior change.

- [ ] **Step 1: openapi.yaml**

In `docs/openapi.yaml`:

1. Add two component schemas (alphabetical with the others):

```yaml
    ControlParams:
      type: object
      description: B1 ControlNet conditioning (SDXL-sidecar models only; mutually exclusive with edit/inpaint/blend).
      required: [net, image]
      properties:
        net:
          type: string
          enum: [pose, depth, canny]
        image:
          type: string
          description: base64 conditioning reference (pose/depth map, or photo for canny)
        strength:
          type: number
          minimum: 0.1
          maximum: 1.0
          description: defaults to 0.7 (sd.cpp --control-strength)
    IPAdapterParams:
      type: object
      description: B1 IP-Adapter identity conditioning (SDXL-sidecar models only).
      required: [image]
      properties:
        image:
          type: string
          description: base64 identity reference
        strength:
          type: number
          minimum: 0.1
          maximum: 1.0
          description: defaults to 0.6
```

2. In the SubmitRequest schema (`/api/v1/jobs` POST + `/api/v1/generate` both `$ref` it — one edit) add properties:

```yaml
        control:
          $ref: '#/components/schemas/ControlParams'
        ip_adapter:
          $ref: '#/components/schemas/IPAdapterParams'
```

3. In `AssetSpecIn`: replace the `transparent` property (currently boolean) with:

```yaml
        transparent:
          description: element/frame/bar. true = white+black dual pass (legacy spell); "rembg" = U²-Net extraction via the REMBG_URL sidecar.
          oneOf:
            - type: boolean
            - type: string
              enum: [rembg, dual]
```

and add:

```yaml
        views:
          type: array
          description: sheet kind — cell view names (2..8); every cell shares the set seed; columns bounds the grid (0 = single row).
          items:
            type: string
          minItems: 2
          maxItems: 8
        columns:
          type: integer
          description: atlas/sheet — 0 = single row, else 1..len(frames|views).
```

(where a `columns` doc already exists for atlas, extend its description instead). Update the kind enumeration text (wherever kinds are listed as an enum or description) to include `sheet`.

- [ ] **Step 2: MCP schemas**

In `cmd/img-gen-mcp/main.go`, in the `submit_job` tool schema string (line ~75) append lines mirroring the existing field description lines:

```
- control (object, optional): B1 ControlNet conditioning — {net: pose|depth|canny, image: base64, strength: 0.1..1 (default 0.7)}; SDXL-sidecar models only; exclusive with mode edit/inpaint/blend.
- ip_adapter (object, optional): B1 IP-Adapter identity — {image: base64, strength: 0.1..1 (default 0.6)}; same constraints.
```

In the `asset_sets` tool description (~line 105), extend the asset fields text with:

```
- transparent: true (white+black dual pass, default spell) or "rembg" (U²-Net extraction; REMBG_URL sidecar; element/frame/bar only).
- views (sheet kind): 2..8 view names; every cell shares the set seed; columns 0 = single row. Deliverable: <name>-sheet.png + <name>-sheet.json.
- kind sheet renders a multi-view character turnaround onto one grid.
```

- [ ] **Step 3: README**

In `README.md`:

1. Env list (~line 34-36): after the `RIFE_URL` line add:

```markdown
- `REMBG_URL` — optional rembg sidecar (POST /segment); transparent `"rembg"` assets need it, unset makes them fail validation honestly
```

2. In the generate/modes section, add a subsection:

```markdown
### Controlled generation (SDXL models, B1)

`/api/v1/generate` and job submission accept two optional conditioning objects:

- `control`: `{"net": "pose"|"depth"|"canny", "image": "<b64>", "strength": 0.1..1.0 (default 0.7)}` — the image is the conditioning reference; for `net: "canny"` the sidecar preprocesses (`--canny`), pose and depth expect ready-made maps.
- `ip_adapter`: `{"image": "<b64>", "strength": 0.1..1.0 (default 0.6)}` — identity pinning via IP-Adapter.

Both are exclusive with `mode: edit|inpaint|blend`, apply to SDXL-sidecar models only (`validation_error` otherwise — never silently ignored), and under lattice routing ride the request's `extension` object: a gateway that predates the field rejects it with an error telling you to upgrade the gateway or set `IMAGE_ROUTING=direct`.
```

3. In the asset kinds list (~line 165+): under `element` note `` `transparent` accepts `true` (white+black dual pass) or `"rembg"` (U²-Net extraction via REMBG_URL, single plain render) ``; add a `sheet` bullet:

```markdown
- `sheet` — multi-view character turnaround on one grid: `views`: 2..8 view names, `columns` 0..len(views) (0 = single row); every cell shares the set seed; deliverable `<name>-sheet.png` + `<name>-sheet.json` (view name per cell rect), and `meta_formats` phaser/unity/sparrow apply.
```

4. meta_formats table (~248-267): update the phaser/unity/sparrow rows' kind lists to `atlas, npc, sheet`.

- [ ] **Step 4: Full verification**

Run: `gofmt -l . && go vet ./... && go test ./... && go build ./...`
Expected: clean (no gofmt output), PASS, build OK.

- [ ] **Step 5: Smoke — scratch instance, real SDXL sidecar**

All env values resolved at runtime; nothing tracked. Do NOT touch :8099 or :9106.

1. Build: `go build -o /tmp/img-gen-phaseb ./cmd/img-gen`.
2. Probe the real SDXL sidecar (:8902, host value from the live environment) for B1 payload acceptance — POST `/generate` with a minimal `control_net` object; record accepted/rejected verbatim in the report. If rejected, record the honest finding (the sidecar-side flag work is a separate deliverable) and skip step 4's control renders rather than faking them.
3. Launch a scratch instance on :9110: env `IMAGE_ROUTING=direct`, `SDXL_URL=<same as :9106 render instance>`, `DATA_DIR=/tmp/img-gen-phaseb-data`, `EXPORT_DIR=/tmp/img-gen-phaseb-exports`, `GENRES_FILE=./genres.json`, `MODELS_FILE=./models.json`, `PRESETS_FILE=./presets.json`, `PORT=9110` (or equivalent flag). No REMBG_URL.
4. With the SDXL sidecar accepting control payloads: submit through `POST /api/v1/generate` on :9110 — (a) a pose/canny-locked single render (`control` with a small canny edge-map PNG), (b) an identity-pinned pair (two submits, same `ip_adapter` image + same `seed`) — and verify the pair's determinism (identical bytes expected from a deterministic engine + pinned seed) and view the PNGs. Record result.
5. rembg honest-failure proof: POST an asset-set with `transparent: "rembg"` against :9110 (no REMBG_URL) → expect `validation_error` naming `REMBG_URL`. Record.
6. Tear down :9110 and scratch dirs; kill the scratch process.
7. Record the smoke section in `task-8-report.md`, including the SDXL-sidecar probe verdict and any blocked items (e.g. no rembg sidecar deployed → the rembg happy-path smoke is blocked until that deployment exists; httptest fakes cover the contract meanwhile).

- [ ] **Step 6: Commit**

```bash
git add docs/openapi.yaml cmd/img-gen-mcp/main.go README.md
git commit -m "docs(b1-b4): openapi + MCP + README — control/ip_adapter, rembg transparent, sheet kind

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>"
```