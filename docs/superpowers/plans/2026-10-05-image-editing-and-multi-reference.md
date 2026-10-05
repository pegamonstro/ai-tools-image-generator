# Image Editing & Multi-Reference Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add three image operations — edit (img2img), inpaint (masked region edit), and blend (multi-reference) — to the img-gen web tool, running on the existing FLUX.1-dev + adapter stack.

**Architecture:** The img-gen Go backend calls the mflux sidecar directly (`IMAGE_URL`). Three new sidecar endpoints (`/fill`, `/redux`) wrap `mflux-generate-fill` and `mflux-generate-redux`; `/edit` already exists. The Go backend gains mode-aware client methods and a mode-aware queue; the vanilla-JS frontend gains a mode selector plus upload/mask/strength/multi-upload controls.

**Tech Stack:** Go 1.27 stdlib (`net/http`, `encoding/json`, `//go:embed`); vanilla JS; Python 3 sidecar wrapping the mflux CLI.

**Spec:** `docs/superpowers/specs/2026-10-05-img-edit-and-multi-reference-design.md`

## Global Constraints

- Go stdlib only — no framework, no ORM, no third-party deps.
- One static binary; UI embedded via `//go:embed static`.
- **Minimise SSD writes** — hot state (including input images) in RAM; durable writes happen once, at completion (output PNG + one history line). Input images are **never** persisted.
- Mode strings (exact): `"generate"` (default when absent), `"edit"`, `"inpaint"`, `"blend"`.
- Edit strength default `0.4`, range `[0,1]`.
- Fill defaults: `--steps 25`, `--guidance 30`, `-q 8` (quantize), no width/height (size derived from image+mask).
- Redux defaults: `--steps 20`, `-q 8`, explicit `--width`/`--height`.
- Sidecar endpoints: `/generate`, `/edit`, `/fill`, `/redux`. Request bodies carry base64 images (`init_image`, `image`/`mask`, `images`); responses are `{image: <base64 png>}`.
- Repos: sidecar lives in **`/Users/<you>/Projects/LLM-router`** (`deploy/mflux-sidecar.py`); everything else in **`/Users/<you>/Projects/img-gen`**.

### mflux CLI flags (verified against current mflux docs)

- `mflux-generate-fill --prompt P --image-path IMG --masked-image-path MASK --output OUT --seed S --steps 25 --guidance 30 -q 8`
- `mflux-generate-redux --prompt P --redux-image-paths IMG1 IMG2 --redux-image-strengths 0.8 0.5 --output OUT --seed S --width W --height H --steps 20 -q 8`

Both are invoked as fresh subprocesses (same as `mflux-generate` today). `--model` is **not** passed (the command implies dev-fill / dev-redux respectively).

---

## Task 1: Sidecar — add `/fill` and `/redux` endpoints

**Repo:** `/Users/<you>/Projects/LLM-router`

**Files:**
- Modify: `deploy/mflux-sidecar.py`

**Interfaces:**
- Produces: HTTP endpoints `POST /fill` (body `{prompt, image, mask, steps?, guidance?, seed?}`) and `POST /redux` (body `{prompt, images:[b64], strengths:[f], width, height, steps?, seed?}`), both returning `{image: b64, seed, width, height, seconds}` or `{error, error_type}`. The img-gen lattice client (Task 2) POSTs to these.

- [ ] **Step 1: Add the two CLI env vars** after the existing `EXTRA`/`GEN_TIMEOUT` block (near line 47-51):

```python
FILL_BIN = os.environ.get("MFLUX_FILL_BIN", os.path.expanduser("~/.local/bin/mflux-generate-fill"))
REDUX_BIN = os.environ.get("MFLUX_REDUX_BIN", os.path.expanduser("~/.local/bin/mflux-generate-redux"))
FILL_QUANTIZE = os.environ.get("MFLUX_FILL_QUANTIZE", "8")
REDUX_QUANTIZE = os.environ.get("MFLUX_REDUX_QUANTIZE", "8")
```

- [ ] **Step 2: Add the `run_fill` and `run_redux` functions** directly after `run_generation` (after line 88):

```python
def run_fill(params: dict) -> bytes:
    """Run one mflux-generate-fill invocation (masked inpainting)."""
    with tempfile.TemporaryDirectory(prefix="mflux-") as td:
        img_path = os.path.join(td, "image.png")
        mask_path = os.path.join(td, "mask.png")
        out = os.path.join(td, "out.png")
        with open(img_path, "wb") as fh:
            fh.write(base64.b64decode(params["image"]))
        with open(mask_path, "wb") as fh:
            fh.write(base64.b64decode(params["mask"]))

        cmd = [
            FILL_BIN,
            "--prompt", params["prompt"],
            "--image-path", img_path,
            "--masked-image-path", mask_path,
            "--output", out,
            "--seed", str(params["seed"]),
            "--steps", str(params.get("steps", 25)),
            "--guidance", str(params.get("guidance", 30.0)),
            "-q", FILL_QUANTIZE,
        ]
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=GEN_TIMEOUT)
        if proc.returncode != 0:
            detail = (proc.stderr or proc.stdout or "mflux-generate-fill failed").strip()
            raise RuntimeError(detail[-800:])
        with open(out, "rb") as fh:
            return fh.read()


def run_redux(params: dict) -> bytes:
    """Run one mflux-generate-redux invocation (multi-reference)."""
    with tempfile.TemporaryDirectory(prefix="mflux-") as td:
        paths = []
        for i, b64 in enumerate(params["images"]):
            p = os.path.join(td, f"ref{i}.png")
            with open(p, "wb") as fh:
                fh.write(base64.b64decode(b64))
            paths.append(p)
        out = os.path.join(td, "out.png")

        cmd = [
            REDUX_BIN,
            "--prompt", params["prompt"],
            "--redux-image-paths", *paths,
            "--redux-image-strengths", *[str(s) for s in params["strengths"]],
            "--output", out,
            "--seed", str(params["seed"]),
            "--width", str(params.get("width", 1024)),
            "--height", str(params.get("height", 1024)),
            "--steps", str(params.get("steps", 20)),
            "-q", REDUX_QUANTIZE,
        ]
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=GEN_TIMEOUT)
        if proc.returncode != 0:
            detail = (proc.stderr or proc.stdout or "mflux-generate-redux failed").strip()
            raise RuntimeError(detail[-800:])
        with open(out, "rb") as fh:
            return fh.read()
```

- [ ] **Step 3: Rewrite `do_POST`** (lines 114-167) to dispatch the four paths. Replace the existing `do_POST` method body with:

```python
    def do_POST(self) -> None:
        if not self._authorized():
            self._send_json(401, {"error": "unauthorized", "error_type": "auth"}); return

        path = self.path.rstrip("/")
        if path not in ("/generate", "/edit", "/fill", "/redux"):
            self._send_json(404, {"error": "not found"}); return

        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length) or b"{}")
        except (ValueError, json.JSONDecodeError):
            self._send_json(400, {"error": "invalid JSON body", "error_type": "invalid_argument"}); return

        if not isinstance(body.get("prompt"), str) or not body["prompt"].strip():
            self._send_json(400, {"error": "prompt is required", "error_type": "invalid_argument"}); return

        try:
            params = self._build_params(path, body)
        except (ValueError, TypeError):
            self._send_json(400, {"error": "width/height/steps/guidance/seed/strength must be numeric and image inputs present", "error_type": "invalid_argument"}); return

        if not _lock.acquire(blocking=False):
            self._send_json(409, {"error": "generation already in progress", "error_type": "busy"}); return

        try:
            t0 = time.time()
            try:
                if path in ("/generate", "/edit"):
                    img = run_generation(params)
                elif path == "/fill":
                    img = run_fill(params)
                else:
                    img = run_redux(params)
            except subprocess.TimeoutExpired:
                self._send_json(504, {"error": "generation timed out", "error_type": "timeout"}); return
            except Exception as exc:  # noqa: BLE001 — surface the subprocess's own error text
                self._send_json(500, {"error": str(exc), "error_type": "provider_error"}); return
            self._send_json(200, {
                "image": base64.b64encode(img).decode("ascii"),
                "seed": params.get("seed"),
                "width": params.get("width"),
                "height": params.get("height"),
                "seconds": round(time.time() - t0, 1),
            })
        finally:
            _lock.release()

    def _build_params(self, path: str, body: dict) -> dict:
        common = {
            "prompt": body["prompt"].strip(),
            "seed": int(body["seed"]) if body.get("seed") is not None else random.randrange(0, 1_000_000_000),
        }
        if path in ("/generate", "/edit"):
            p = dict(common,
                     negative_prompt=body.get("negative_prompt"),
                     width=int(body.get("width", 1024)),
                     height=int(body.get("height", 1024)),
                     steps=int(body["steps"]) if body.get("steps") else None,
                     guidance=float(body["guidance"]) if body.get("guidance") else None)
            if path == "/edit":
                if not body.get("init_image"):
                    raise TypeError("init_image required")
                p["init_image"] = body["init_image"]
                p["strength"] = float(body.get("strength", 0.4))
            return p
        if path == "/fill":
            if not body.get("image") or not body.get("mask"):
                raise TypeError("image and mask required")
            return dict(common,
                        image=body["image"], mask=body["mask"],
                        steps=int(body["steps"]) if body.get("steps") else 25,
                        guidance=float(body["guidance"]) if body.get("guidance") else 30.0)
        # /redux
        images = body.get("images")
        if not isinstance(images, list) or not images:
            raise TypeError("images required")
        strengths = body.get("strengths") or [1.0] * len(images)
        if len(strengths) < len(images):
            strengths = list(strengths) + [1.0] * (len(images) - len(strengths))
        return dict(common,
                    images=images,
                    strengths=[float(s) for s in strengths[: len(images)]],
                    width=int(body.get("width", 1024)),
                    height=int(body.get("height", 1024)),
                    steps=int(body["steps"]) if body.get("steps") else 20)
```

- [ ] **Step 4: Update the module docstring** endpoint list (lines 20-25) to mention `/fill` and `/redux`:

```
  POST /fill     -> {prompt, image: b64, mask: b64, steps?, guidance?, seed?}
  POST /redux    -> {prompt, images: [b64...], strengths: [f...], width?, height?,
                     steps?, seed?}
```

- [ ] **Step 5: Syntax check**

Run: `python3 -m py_compile deploy/mflux-sidecar.py`
Expected: exit 0, no output.

- [ ] **Step 6: Commit**

```bash
cd /Users/<you>/Projects/LLM-router
git add deploy/mflux-sidecar.py
git commit -m "feat(mflux): add /fill (inpainting) and /redux (multi-reference) endpoints"
```

**Note (not a blocker):** a live smoke test of `/fill` and `/redux` requires the `mflux-generate-fill` and `mflux-generate-redux` binaries plus the FLUX.1-Fill-dev (~34 GB) and FLUX.1-Redux-dev (~1.1 GB) checkpoints on the image host. That is covered in Task 8. This task only wires the code; flag set is per the verified docs above but confirm with `mflux-generate-fill --help` on the host during Task 8.

---

## Task 2: `internal/lattice` — add Edit/Inpaint/Blend client methods

**Files:**
- Modify: `internal/lattice/lattice.go`
- Test: `internal/lattice/lattice_test.go`

**Interfaces:**
- Produces (used by Task 5's `queue.ImageOps`): 
  ```go
  func (c *Client) Edit(ctx context.Context, prompt, size, imageB64 string, strength float64) ([]byte, error)
  func (c *Client) Inpaint(ctx context.Context, prompt, imageB64, maskB64 string) ([]byte, error)
  func (c *Client) Blend(ctx context.Context, prompt, size string, imagesB64 []string, strengths []float64) ([]byte, error)
  ```
  Inputs are base64 strings (pass through to the sidecar unchanged); outputs are decoded PNG bytes (same as `Generate`).

- [ ] **Step 1: Write the failing tests** — append to `internal/lattice/lattice_test.go`:

```go
func TestEdit(t *testing.T) {
	img := []byte("fake-png")
	var got struct {
		Prompt    string  `json:"prompt"`
		Width     int     `json:"width"`
		Height    int     `json:"height"`
		InitImage string  `json:"init_image"`
		Strength  float64 `json:"strength"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/edit" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	out, err := c.Edit(context.Background(), "make it snow", "512x512", "aW1n", 0.6)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if string(out) != string(img) {
		t.Fatalf("got %q", out)
	}
	if got.InitImage != "aW1n" || got.Strength != 0.6 || got.Width != 512 || got.Height != 512 {
		t.Fatalf("body = %+v", got)
	}
}

func TestInpaint(t *testing.T) {
	img := []byte("fake-png")
	var got struct {
		Image string `json:"image"`
		Mask  string `json:"mask"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fill" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	if _, err := c.Inpaint(context.Background(), "add a cat", "aW1n", "bWFzaw=="); err != nil {
		t.Fatalf("Inpaint: %v", err)
	}
	if got.Image != "aW1n" || got.Mask != "bWFzaw==" {
		t.Fatalf("body = %+v", got)
	}
}

func TestBlend(t *testing.T) {
	img := []byte("fake-png")
	var got struct {
		Images    []string  `json:"images"`
		Strengths []float64 `json:"strengths"`
		Width     int       `json:"width"`
		Height    int       `json:"height"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/redux" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer srv.Close()

	c := New("http://unused")
	c.ImageURL = srv.URL
	_, err := c.Blend(context.Background(), "a statue", "1024x1024", []string{"aW1n"}, []float64{0.8})
	if err != nil {
		t.Fatalf("Blend: %v", err)
	}
	if len(got.Images) != 1 || got.Images[0] != "aW1n" || len(got.Strengths) != 1 || got.Strengths[0] != 0.8 {
		t.Fatalf("body = %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/lattice/`
Expected: FAIL — `c.Edit undefined`, `c.Inpaint undefined`, `c.Blend undefined`.

- [ ] **Step 3: Implement** — in `lattice.go`, refactor `Generate` to a shared `postImage` helper and add the three methods. Replace the body of `Generate` (lines 30-67) with:

```go
// Generate requests one image from the mflux sidecar and returns its raw PNG
// bytes. The sidecar takes explicit width/height rather than an OpenAI "size"
// string, so "1024x576" is split into its dimensions.
func (c *Client) Generate(ctx context.Context, prompt, size string) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"prompt": prompt, "width": w, "height": h})
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/generate", body)
}

// Edit requests an image-to-image edit: it keeps the source image's content
// while applying the prompt. imageB64 is the source image (base64); strength is
// the denoise strength in [0,1] (higher departs further from the source).
func (c *Client) Edit(ctx context.Context, prompt, size, imageB64 string, strength float64) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"prompt": prompt, "width": w, "height": h,
		"init_image": imageB64, "strength": strength,
	})
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/edit", body)
}

// Inpaint repaints only the masked region of imageB64. maskB64 is a same-sized
// mask (white = regenerate, black = keep); output size matches the source.
func (c *Client) Inpaint(ctx context.Context, prompt, imageB64, maskB64 string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"prompt": prompt, "image": imageB64, "mask": maskB64})
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/fill", body)
}

// Blend generates a new image from a prompt plus reference images (base64) and
// per-reference strengths.
func (c *Client) Blend(ctx context.Context, prompt, size string, imagesB64 []string, strengths []float64) ([]byte, error) {
	w, h, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"prompt": prompt, "width": w, "height": h,
		"images": imagesB64, "strengths": strengths,
	})
	if err != nil {
		return nil, err
	}
	return c.postImage(ctx, "/redux", body)
}

// postImage sends one sidecar request and decodes the returned base64 PNG.
func (c *Client) postImage(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ImageURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("mflux %s: %s: %s", path, resp.Status, msg)
	}
	var gr struct {
		Image string `json:"image"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, err
	}
	if gr.Image == "" {
		if gr.Error != "" {
			return nil, fmt.Errorf("mflux %s: %s", path, gr.Error)
		}
		return nil, fmt.Errorf("mflux %s: empty image", path)
	}
	return base64.StdEncoding.DecodeString(gr.Image)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lattice/`
Expected: PASS (including the pre-existing `TestGenerate`, `TestGenerateNon200`, `TestChat`).

- [ ] **Step 5: Commit**

```bash
git add internal/lattice/lattice.go internal/lattice/lattice_test.go
git commit -m "feat(lattice): add Edit/Inpaint/Blend client methods for the mflux sidecar"
```

---

## Task 3: `internal/prompting` — add EnhancePrompt

**Files:**
- Modify: `internal/prompting/prompting.go`
- Test: `internal/prompting/prompting_test.go`

**Interfaces:**
- Produces (used by Task 5): `func EnhancePrompt(ctx context.Context, chat ChatFunc, system, prompt string) (string, error)` — sends a free-text prompt to the chat model for polishing (used when a non-generate mode has `enhance` on).

- [ ] **Step 1: Write the failing test** — append to `internal/prompting/prompting_test.go`:

```go
func TestEnhancePrompt(t *testing.T) {
	var gotSystem, gotUser string
	chat := func(ctx context.Context, msgs []Message) (string, error) {
		gotSystem = msgs[0].Content
		gotUser = msgs[1].Content
		return "polished prompt", nil
	}
	got, err := EnhancePrompt(context.Background(), chat, "SYSTEM", "make it snow")
	if err != nil {
		t.Fatalf("EnhancePrompt: %v", err)
	}
	if got != "polished prompt" {
		t.Fatalf("got %q", got)
	}
	if gotSystem != "SYSTEM" || gotUser != "make it snow" {
		t.Fatalf("system=%q user=%q", gotSystem, gotUser)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/prompting/`
Expected: FAIL — `undefined: EnhancePrompt`.

- [ ] **Step 3: Implement** — append to `prompting.go` (after `Enhance`, line 86):

```go
// EnhancePrompt asks the chat model to polish a free-text image prompt (used by
// the edit/inpaint/blend modes, which take a raw prompt rather than structured
// genre fields).
func EnhancePrompt(ctx context.Context, chat ChatFunc, system, prompt string) (string, error) {
	return chat(ctx, []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	})
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/prompting/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/prompting/prompting.go internal/prompting/prompting_test.go
git commit -m "feat(prompting): add EnhancePrompt for free-text prompts"
```

---

## Task 4: `internal/storage` — add Mode to Job

**Files:**
- Modify: `internal/storage/storage.go`
- Test: `internal/storage/storage_test.go`

**Interfaces:**
- Produces: `storage.Job.Mode string` with JSON tag `json:"mode,omitempty"` (empty for legacy history lines, which the frontend treats as generate). Used by Tasks 5 and 7.

- [ ] **Step 1: Write the failing test** — append to `internal/storage/storage_test.go`:

```go
func TestJobModeRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = s.AppendHistory(Job{ID: "abc", Mode: "edit", Prompt: "make it snow", Status: "done"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Mode != "edit" {
		t.Fatalf("jobs = %+v", jobs)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/storage/`
Expected: FAIL — `unknown field Mode in struct literal`.

- [ ] **Step 3: Implement** — add the field to the `Job` struct (after `Genre`, line 13):

```go
	Mode       string            `json:"mode,omitempty"`
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/storage/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/storage.go internal/storage/storage_test.go
git commit -m "feat(storage): record job mode"
```

---

## Task 5: `internal/queue` — mode handling, ImageOps, in-RAM inputs

**Files:**
- Modify: `internal/queue/queue.go`
- Test: `internal/queue/queue_test.go`

**Interfaces:**
- Consumes: `lattice` client methods (Task 2), `prompting.EnhancePrompt` (Task 3), `storage.Job.Mode` (Task 4).
- Produces: `SubmitRequest` with `Mode`, `Prompt`, `Image`, `Mask`, `Strength`, `Images`, `Strengths`; `Options.Ops ImageOps`; mode-aware `Submit` validation and `run` dispatch. `cmd/img-gen` (Task 6) wires `Ops`; the frontend (Task 7) POSTs the extended body.

**Note:** this task *breaks* the existing `Options.Generate` field and therefore the existing `queue_test.go` `testOpts` helper and two tests — update them as specified.

- [ ] **Step 1: Update the `SubmitRequest` struct** (lines 17-22) to:

```go
type SubmitRequest struct {
	Genre   string            `json:"genre"`
	Fields  map[string]string `json:"fields"`
	Size    string            `json:"size"`
	Enhance bool              `json:"enhance"`

	Mode      string    `json:"mode"`       // "generate"(default) | "edit" | "inpaint" | "blend"
	Prompt    string    `json:"prompt"`     // free-text prompt (non-generate modes)
	Image     string    `json:"image"`      // base64: edit/inpaint source
	Mask      string    `json:"mask"`       // base64: inpaint mask
	Strength  float64   `json:"strength"`   // edit denoise strength (0..1)
	Images    []string  `json:"images"`     // base64: blend references
	Strengths []float64 `json:"strengths"`  // blend per-reference weights
}
```

- [ ] **Step 2: Add the `ImageOps` type and swap `Options.Generate` for `Options.Ops`** — add `ImageOps` after `LogLine`, and change the `Options` struct (lines 38-44):

```go
type ImageOps struct {
	Generate func(ctx context.Context, prompt, size string) ([]byte, error)
	Edit     func(ctx context.Context, prompt, size, imageB64 string, strength float64) ([]byte, error)
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
```

- [ ] **Step 3: Add the in-RAM inputs store** — add `imageInputs` type and an `inputs` field on `Manager` (lines 5, 46-54):

```go
type imageInputs struct {
	Image     string   // base64 (edit/inpaint source)
	Mask      string   // base64 (inpaint mask)
	Images    []string // base64 (blend refs)
	Strengths []float64
	Strength  float64
}
```

In the `Manager` struct add `inputs map[string]imageInputs`, and in `New` initialise `inputs: map[string]imageInputs{}`.

- [ ] **Step 4: Rewrite `Submit`** (lines 68-99) to validate per-mode and store inputs:

```go
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
			return "", fmt.Errorf("size %q not allowed for genre %q", req.Genre, req.Size)
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

	id := newID()
	job := &storage.Job{
		ID:        id,
		Mode:      mode,
		Genre:     req.Genre,
		Prompt:    req.Prompt,
		Fields:    req.Fields,
		Size:      req.Size,
		Enhance:   req.Enhance,
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
```

Add `"strconv"` to the imports.

- [ ] **Step 5: Rewrite `run`** (lines 107-165) to branch on mode. Replace with:

```go
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
	m.mu.Lock()
	job.Prompt = prompt
	m.mu.Unlock()

	m.setStatus(id, "generating", "")
	start := time.Now()
	var png []byte
	switch job.Mode {
	case "edit":
		m.log(id, "requesting edit (strength=%.2f)", inputs.Strength)
		png, err = m.opts.Ops.Edit(context.Background(), prompt, job.Size, inputs.Image, inputs.Strength)
	case "inpaint":
		m.log(id, "requesting inpaint")
		png, err = m.opts.Ops.Inpaint(context.Background(), prompt, inputs.Image, inputs.Mask)
	case "blend":
		m.log(id, "requesting blend (%d reference image(s))", len(inputs.Images))
		png, err = m.opts.Ops.Blend(context.Background(), prompt, job.Size, inputs.Images, inputs.Strengths)
	default:
		m.log(id, "requesting image from lattice (size=%s)", job.Size)
		png, err = m.opts.Ops.Generate(context.Background(), prompt, job.Size)
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
```

- [ ] **Step 6: Update the existing tests** in `queue_test.go`:

  - In `testOpts` (line 37-43), replace the `Generate:` field with:
    ```go
    Ops: ImageOps{
        Generate: func(ctx context.Context, prompt, size string) ([]byte, error) { return []byte("PNG"), nil },
    },
    ```
  - In `TestGenerateErrorFailsJob` (line 114), change `opts.Generate = ...` to `opts.Ops.Generate = ...`.
  - In `TestSingleSlotSerializes` (line 129), change `opts.Generate = ...` to `opts.Ops.Generate = ...`.

- [ ] **Step 7: Add new tests** — append to `queue_test.go`:

```go
func editOps() ImageOps {
	return ImageOps{
		Generate: func(ctx context.Context, prompt, size string) ([]byte, error) { return []byte("PNG"), nil },
		Edit:     func(ctx context.Context, prompt, size, img string, strength float64) ([]byte, error) { return []byte("PNG"), nil },
		Inpaint:  func(ctx context.Context, prompt, img, mask string) ([]byte, error) { return []byte("PNG"), nil },
		Blend:    func(ctx context.Context, prompt, size string, imgs []string, ws []float64) ([]byte, error) { return []byte("PNG"), nil },
	}
}

func TestSubmitUnknownMode(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "nope"}); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestSubmitEditMissingImage(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "edit", Prompt: "x", Size: "512x512"}); err == nil {
		t.Fatal("expected error for edit without image")
	}
}

func TestSubmitInpaintMissingMask(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "inpaint", Prompt: "x", Image: "aW1n"}); err == nil {
		t.Fatal("expected error for inpaint without mask")
	}
}

func TestSubmitBlendNoImages(t *testing.T) {
	m := New(testOpts(t))
	if _, err := m.Submit(SubmitRequest{Mode: "blend", Prompt: "x", Size: "512x512"}); err == nil {
		t.Fatal("expected error for blend without reference images")
	}
}

func TestEditJobCompletes(t *testing.T) {
	opts := testOpts(t)
	opts.Ops = editOps()
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Mode: "edit", Prompt: "make it snow", Size: "512x512", Image: "aW1n"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Mode != "edit" || j.Prompt != "make it snow" {
		t.Fatalf("mode=%q prompt=%q", j.Mode, j.Prompt)
	}
}

func TestEditJobDispatchCallsEdit(t *testing.T) {
	var called bool
	opts := testOpts(t)
	opts.Ops = editOps()
	opts.Ops.Edit = func(ctx context.Context, prompt, size, img string, strength float64) ([]byte, error) {
		called = true
		if strength != 0.4 {
			t.Fatalf("default strength = %v, want 0.4", strength)
		}
		return []byte("PNG"), nil
	}
	m := New(opts)
	id, _ := m.Submit(SubmitRequest{Mode: "edit", Prompt: "p", Size: "512x512", Image: "aW1n"})
	waitFor(t, m, id, "done")
	if !called {
		t.Fatal("Edit op was not called")
	}
}

func TestInpaintJobCompletes(t *testing.T) {
	opts := testOpts(t)
	opts.Ops = editOps()
	m := New(opts)
	id, err := m.Submit(SubmitRequest{Mode: "inpaint", Prompt: "add a cat", Image: "aW1n", Mask: "bWFzaw=="})
	if err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, m, id, "done")
	if j.Mode != "inpaint" {
		t.Fatalf("mode = %q", j.Mode)
	}
}
```

- [ ] **Step 8: Run the full package test**

Run: `go test ./internal/queue/`
Expected: PASS (all pre-existing and new tests).

- [ ] **Step 9: Commit**

```bash
git add internal/queue/queue.go internal/queue/queue_test.go
git commit -m "feat(queue): mode-aware submission and dispatch for edit/inpaint/blend"
```

---

## Task 6: `cmd/img-gen` — wire ImageOps + integration test

**Files:**
- Modify: `cmd/img-gen/main.go`
- Test: `cmd/img-gen/main_test.go`

**Interfaces:**
- Consumes: `queue.ImageOps` (Task 5) and `lattice` methods (Task 2).
- Produces: the running server now accepts the extended `SubmitRequest` body on `POST /api/jobs`. The frontend (Task 7) depends on this.

- [ ] **Step 1: Wire `Ops` in `newHandler`** — replace the `queue.New(queue.Options{...})` call (lines 68-74) with:

```go
	mgr := queue.New(queue.Options{
		Genres:        catalog,
		Store:         store,
		Ops: queue.ImageOps{
			Generate: lat.Generate,
			Edit:     lat.Edit,
			Inpaint:  lat.Inpaint,
			Blend:    lat.Blend,
		},
		Chat:          chatFn,
		EnhanceSystem: cfg.EnhanceSystem,
	})
```

- [ ] **Step 2: Run the build to verify compilation**

Run: `go build ./...`
Expected: exit 0.

- [ ] **Step 3: Add an integration test** — append to `main_test.go` (mirroring whatever fake-`Generate` wiring the existing test uses; if the existing test builds `newHandler` with a fake sidecar, extend it). If `main_test.go` does not yet stub the sidecar, add a test that stands up an `httptest` sidecar with a `/edit` handler and asserts a full `POST /api/jobs` → poll `GET /api/jobs/{id}` → `done` round-trip:

```go
func TestEditRoundTrip(t *testing.T) {
	img := []byte("fake-png")
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/edit" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(img) + `"}`))
	}))
	defer sidecar.Close()

	cfg := testConfig()
	cfg.ImageURL = sidecar.URL
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"mode":"edit","prompt":"make it snow","size":"512x512","image":"aW1n"}`
	resp, err := http.Post(srv.URL+"/api/jobs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// The job runs on a background worker; poll until it completes.
	var job struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/jobs")
		if err != nil {
			t.Fatal(err)
		}
		var jobs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&jobs)
		r.Body.Close()
		if len(jobs) == 1 && jobs[0].Status == "done" {
			job = jobs[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.Status != "done" {
		t.Fatalf("job never completed: %+v", job)
	}
}
```

(Adapt the `testConfig()` helper and import list to match whatever the existing `main_test.go` already uses; if the existing test wires `Generate` via a different mechanism, follow that mechanism. The key assertion is that an `edit` submission completes against a fake `/edit` sidecar.)

- [ ] **Step 4: Run the integration test**

Run: `go test ./cmd/img-gen/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/img-gen/main.go cmd/img-gen/main_test.go
git commit -m "feat(img-gen): wire edit/inpaint/blend ops into the job queue"
```

---

## Task 7: Frontend — mode selector + edit/inpaint/blend controls

**Files:**
- Modify: `cmd/img-gen/static/index.html`
- Modify: `cmd/img-gen/static/app.js`
- Modify: `cmd/img-gen/static/style.css` (minor, for the new controls)

**Interfaces:**
- Consumes: `POST /api/jobs` with the extended body (Task 6), plus the existing `/api/genres`, `/api/jobs` GET, `/api/jobs/{id}/events`, `/api/images/{id}.png`.

**Design:** a `#mode` `<select>` at the top of the composer switches which control group is visible. Generate keeps the existing genre/fields/size flow. Edit/Inpaint/Blend share a free-text `#prompt` textarea and an image-picker; Edit adds a strength slider; Inpaint adds a mask canvas; Blend adds a multi-image list with per-image weights. Uploads read via `FileReader.readAsDataURL` and are base64-stripped. Inpaint does not send a `size` (derived from the source). A fixed size list is used for edit/blend.

- [ ] **Step 1: Restructure the composer in `index.html`** — replace the `<section class="panel" id="composer">...</section>` (lines 24-51) with:

```html
      <section class="panel" id="composer">
        <div class="panel-head">
          <h2>Composer</h2>
          <p class="panel-hint">Generate, edit, inpaint, or blend an image.</p>
        </div>

        <label class="ctrl">Mode
          <select id="mode">
            <option value="generate">Generate</option>
            <option value="edit">Edit (img2img)</option>
            <option value="inpaint">Inpaint (mask)</option>
            <option value="blend">Blend (multi-reference)</option>
          </select>
        </label>

        <div id="gen-controls">
          <div class="row">
            <label class="ctrl">Genre
              <select id="genre"></select>
            </label>
            <label class="ctrl">Size
              <select id="size"></select>
            </label>
          </div>
          <div id="fields" class="fields"></div>
        </div>

        <div id="edit-controls" hidden>
          <label class="ctrl">Prompt
            <textarea id="prompt" rows="3" placeholder="describe the change, e.g. 'make it snow at night'"></textarea>
          </label>
          <label class="ctrl">Size
            <select id="edit-size"></select>
          </label>
          <div id="image-picker" class="picker">
            <input type="file" id="edit-image" accept="image/*">
            <small>Upload the image to edit.</small>
          </div>
          <label class="ctrl">Strength <span id="strength-val">0.4</span>
            <input type="range" id="strength" min="0" max="1" step="0.05" value="0.4">
          </label>
        </div>

        <div id="inpaint-controls" hidden>
          <label class="ctrl">Prompt
            <textarea id="inpaint-prompt" rows="3" placeholder="describe what should appear in the painted region"></textarea>
          </label>
          <div class="picker">
            <input type="file" id="inpaint-image" accept="image/*">
            <small>Upload the image to edit.</small>
          </div>
          <div id="mask-wrap" hidden>
            <canvas id="mask-canvas"></canvas>
            <div class="mask-tools">
              <label>Brush <input type="range" id="brush" min="4" max="120" value="30"></label>
              <button id="mask-eraser" type="button">Eraser</button>
              <button id="mask-clear" type="button">Clear</button>
            </div>
            <small>Paint white over the region to change; leave the rest black (kept).</small>
          </div>
        </div>

        <div id="blend-controls" hidden>
          <label class="ctrl">Prompt
            <textarea id="blend-prompt" rows="3" placeholder="describe the result, e.g. 'a portrait in the style of these images'"></textarea>
          </label>
          <label class="ctrl">Size
            <select id="blend-size"></select>
          </label>
          <div class="picker">
            <input type="file" id="blend-image" accept="image/*" multiple>
            <small>Upload one or more reference images.</small>
          </div>
          <div id="refs"></div>
        </div>

        <label class="toggle">
          <input type="checkbox" id="enhance">
          <span class="toggle-track" aria-hidden="true"></span>
          Enhance prompt with LLM
        </label>

        <div class="actions">
          <button id="generate" type="button">Generate</button>
          <span id="status" class="status" hidden></span>
        </div>
      </section>
```

- [ ] **Step 2: Add the mode-switching and image-reading helpers in `app.js`** — add near the top (after `let activeJobId = null;`):

```js
const EDIT_SIZES = ['512x512', '768x512', '1024x576', '1024x1024'];
let uploaded = { edit: null, inpaint: null, blend: [] }; // base64 strings
let brushErase = false;

function currentMode() { return $('mode').value; }

function renderMode() {
  const m = currentMode();
  $('gen-controls').hidden = m !== 'generate';
  $('edit-controls').hidden = m !== 'edit';
  $('inpaint-controls').hidden = m !== 'inpaint';
  $('blend-controls').hidden = m !== 'blend';
  $('enhance').closest('.toggle').hidden = false;
}

function populateSizes(sel, sizes) {
  sel.innerHTML = '';
  for (const s of sizes) {
    const o = document.createElement('option');
    o.value = s; o.textContent = s;
    sel.appendChild(o);
  }
}

function readFileAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(r.result);
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}
function stripDataURL(d) { return d.split(',')[1]; }
```

- [ ] **Step 3: Wire the mode selector and pickers** — in `init()`, after `renderGenreSelect()`, add:

```js
  $('mode').onchange = () => { renderMode(); };
  populateSizes($('edit-size'), EDIT_SIZES);
  populateSizes($('blend-size'), EDIT_SIZES);
  $('edit-image').onchange = async (e) => {
    if (e.target.files[0]) uploaded.edit = stripDataURL(await readFileAsDataURL(e.target.files[0]));
  };
  $('inpaint-image').onchange = async (e) => {
    if (e.target.files[0]) {
      uploaded.inpaint = stripDataURL(await readFileAsDataURL(e.target.files[0]));
      await setupMask(e.target.files[0]);
    }
  };
  $('blend-image').onchange = async (e) => {
    uploaded.blend = [];
    for (const f of e.target.files) uploaded.blend.push(stripDataURL(await readFileAsDataURL(f)));
    renderRefs();
  };
  renderMode();
```

- [ ] **Step 4: Implement the mask painter** — add the `setupMask`, `renderRefs`, and `drawMask` functions. The mask canvas is sized to the source image's natural dimensions; it draws the source at 40% opacity underneath a black mask layer, and mouse painting draws white (or black when erasing):

```js
let maskCtx = null;

async function setupMask(file) {
  const url = URL.createObjectURL(file);
  const img = new Image();
  img.onload = () => {
    const cv = $('mask-canvas');
    cv.width = img.naturalWidth;
    cv.height = img.naturalHeight;
    maskCtx = cv.getContext('2d');
    maskCtx.fillStyle = '#000';
    maskCtx.fillRect(0, 0, cv.width, cv.height);
    maskCtx.globalAlpha = 0.4;
    maskCtx.drawImage(img, 0, 0);
    maskCtx.globalAlpha = 1.0;
    $('mask-wrap').hidden = false;
    URL.revokeObjectURL(url);
  };
  img.src = url;

  $('mask-eraser').onclick = () => { brushErase = !brushErase; $('mask-eraser').textContent = brushErase ? 'Brush' : 'Eraser'; };
  $('mask-clear').onclick = () => { if (maskCtx) { maskCtx.globalCompositeOperation = 'source-over'; maskCtx.fillStyle = '#000'; maskCtx.fillRect(0, 0, maskCtx.canvas.width, maskCtx.canvas.height); } };
  const cv = $('mask-canvas');
  cv.onmousedown = (e) => { cv._drawing = true; paintMask(e); };
  cv.onmousemove = (e) => { if (cv._drawing) paintMask(e); };
  cv.onmouseup = cv.onmouseleave = () => { cv._drawing = false; };
}

function paintMask(e) {
  const cv = $('mask-canvas');
  const r = cv.getBoundingClientRect();
  const x = (e.clientX - r.left) * (cv.width / r.width);
  const y = (e.clientY - r.top) * (cv.height / r.height);
  maskCtx.fillStyle = brushErase ? '#000' : '#fff';
  maskCtx.beginPath();
  maskCtx.arc(x, y, $('brush').value, 0, Math.PI * 2);
  maskCtx.fill();
}
```

Note: the source is drawn at 40% opacity *only as a visual guide*, then painting happens with full opacity. To ensure the guide doesn't corrupt the exported mask, `renderRefs`/export must composite a fresh black canvas with only the painted strokes. Because the guide is painted *under* strokes with `globalAlpha` and never cleared from the strokes themselves, the exported mask would include the 40%-grey guide. **Fix:** track the mask on a separate offscreen canvas. Replace the `setupMask` maskCtx logic with a two-canvas scheme:

```js
let maskStroke = null; // offscreen canvas holding only black bg + painted strokes

async function setupMask(file) {
  const url = URL.createObjectURL(file);
  const img = new Image();
  img.onload = () => {
    const cv = $('mask-canvas');
    cv.width = img.naturalWidth;
    cv.height = img.naturalHeight;

    maskStroke = document.createElement('canvas');
    maskStroke.width = cv.width;
    maskStroke.height = cv.height;
    const sctx = maskStroke.getContext('2d');
    sctx.fillStyle = '#000';
    sctx.fillRect(0, 0, cv.width, cv.height);

    redrawMask(img);
    $('mask-wrap').hidden = false;
    URL.revokeObjectURL(url);
  };
  img.src = url;

  $('mask-eraser').onclick = () => { brushErase = !brushErase; $('mask-eraser').textContent = brushErase ? 'Brush' : 'Eraser'; };
  $('mask-clear').onclick = () => {
    const sctx = maskStroke.getContext('2d');
    sctx.fillStyle = '#000';
    sctx.fillRect(0, 0, maskStroke.width, maskStroke.height);
    redrawMask(null);
  };
  const cv = $('mask-canvas');
  cv.onmousedown = (e) => { cv._drawing = true; paintMask(e); };
  cv.onmousemove = (e) => { if (cv._drawing) paintMask(e); };
  cv.onmouseup = cv.onmouseleave = () => { cv._drawing = false; };
}

let guideImg = null;
function redrawMask(img) {
  guideImg = img;
  const cv = $('mask-canvas');
  const ctx = cv.getContext('2d');
  ctx.clearRect(0, 0, cv.width, cv.height);
  if (img) { ctx.globalAlpha = 0.4; ctx.drawImage(img, 0, 0); ctx.globalAlpha = 1.0; }
  ctx.drawImage(maskStroke, 0, 0);
}

function paintMask(e) {
  const cv = $('mask-canvas');
  const r = cv.getBoundingClientRect();
  const x = (e.clientX - r.left) * (cv.width / r.width);
  const y = (e.clientY - r.top) * (cv.height / r.height);
  const sctx = maskStroke.getContext('2d');
  sctx.fillStyle = brushErase ? '#000' : '#fff';
  sctx.beginPath();
  sctx.arc(x, y, $('brush').value, 0, Math.PI * 2);
  sctx.fill();
  redrawMask(guideImg);
}

function maskAsBase64() {
  return maskStroke.toDataURL('image/png').split(',')[1];
}
```

- [ ] **Step 5: Implement the blend reference list** — add:

```js
function renderRefs() {
  const el = $('refs');
  el.innerHTML = '';
  uploaded.blend.forEach((b64, i) => {
    const row = document.createElement('div');
    row.className = 'ref';
    const label = document.createElement('span');
    label.textContent = 'Ref ' + (i + 1);
    const w = document.createElement('input');
    w.type = 'range'; w.min = '0'; w.max = '1'; w.step = '0.05'; w.value = '1';
    w.dataset.idx = i;
    const rm = document.createElement('button');
    rm.type = 'button'; rm.textContent = '×';
    rm.onclick = () => { uploaded.blend.splice(i, 1); renderRefs(); };
    row.appendChild(label); row.appendChild(w); row.appendChild(rm);
    el.appendChild(row);
  });
}
```

- [ ] **Step 6: Rewrite `generate()` to build the mode-specific body** — replace the existing `generate()` (lines 157-177) with:

```js
async function generate() {
  const mode = currentMode();
  const body = { mode, enhance: $('enhance').checked };
  if (mode === 'generate') {
    body.genre = $('genre').value;
    body.fields = collectFields();
    body.size = $('size').value;
  } else if (mode === 'edit') {
    body.prompt = $('prompt').value;
    body.size = $('edit-size').value;
    body.image = uploaded.edit;
    body.strength = parseFloat($('strength').value);
    if (!body.image) { setStatus('please upload an image', 'err'); return; }
  } else if (mode === 'inpaint') {
    body.prompt = $('inpaint-prompt').value;
    body.image = uploaded.inpaint;
    body.mask = maskAsBase64();
    if (!body.image || !body.mask) { setStatus('please upload an image and paint a mask', 'err'); return; }
  } else if (mode === 'blend') {
    body.prompt = $('blend-prompt').value;
    body.size = $('blend-size').value;
    body.images = uploaded.blend;
    body.strengths = Array.from(document.querySelectorAll('#refs input[type=range]')).map(w => parseFloat(w.value));
    if (!body.images.length) { setStatus('please upload at least one reference image', 'err'); return; }
  }
  $('generate').disabled = true;
  setStatus('Submitting…', 'pending');
  try {
    const { job_id } = await jsonFetch('/api/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    subscribe(job_id);
  } catch (e) {
    setStatus('error: ' + e.message, 'err');
    $('generate').disabled = false;
  }
}
```

- [ ] **Step 7: Update the history card title** in `loadHistory()` (line 239) so non-generate jobs show their mode instead of an empty genre:

```js
    title.textContent = j.mode && j.mode !== 'generate' ? j.mode : j.genre;
```

- [ ] **Step 8: Minor CSS** — add to `style.css` a `.picker` and `.ref`/`.mask-tools` rule so the new controls lay out sanely (e.g. `#mask-canvas { max-width: 100%; border: 1px solid #444; }` and `.ref { display: flex; gap: 8px; align-items: center; margin: 4px 0; }`).

- [ ] **Step 9: Build and smoke-check the frontend**

Run: `go build ./...` then start the server against a fake sidecar and load the page in a browser (or via the preview tools) to confirm the mode selector switches panels, an image upload populates state, and an edit submission reaches `POST /api/jobs` with the correct body. If a full browser pass is not possible, at minimum confirm `go build ./...` passes and the static files are syntactically loadable.

- [ ] **Step 10: Commit**

```bash
git add cmd/img-gen/static/index.html cmd/img-gen/static/app.js cmd/img-gen/static/style.css
git commit -m "feat(frontend): mode selector and edit/inpaint/blend controls"
```

---

## Task 8: Deploy and verify end-to-end

**Files:** none (runtime only).

**Prerequisites to check on the image host** (`IMAGE_URL` host — likely this M1, where the sidecar already runs for img-gen):
- `mflux-generate-fill` and `mflux-generate-redux` binaries are installed (same `~/.local/bin` as `mflux-generate`).
- The FLUX.1-Redux-dev (~1.1 GB) and FLUX.1-Fill-dev (~34 GB) checkpoints are downloaded (mflux auto-downloads on first use, but the Fill download is large).

- [ ] **Step 1: Confirm the new sidecar endpoints exist**

Run on the host: `curl -s http://127.0.0.1:8899/health` and confirm the sidecar has been restarted with the new code (the `/fill` and `/redux` paths are accepted — a `POST /fill` with a bad body returns 400, not 404).

- [ ] **Step 2: Smoke-test `mflux-generate-redux` directly** (small size, real checkpoint)

Run: `mflux-generate-redux --prompt "a grey cat statue" --redux-image-paths <a-small-reference.png> --steps 4 --width 256 --height 256 --seed 1 -q 8 --output /tmp/redux-smoke.png`
Expected: exits 0 and writes a PNG. This confirms the redux flags match the installed version.

- [ ] **Step 3: Smoke-test `mflux-generate-fill` directly** (small size, real checkpoint)

Run: `mflux-generate-fill --prompt "a red circle" --image-path <small.png> --masked-image-path <small-mask.png> --steps 4 --guidance 30 --seed 1 -q 8 --output /tmp/fill-smoke.png`
Expected: exits 0 and writes a PNG. Confirm `--image-path`/`--masked-image-path` are the correct flag names for the installed version; adjust the sidecar (Task 1) if they differ.

- [ ] **Step 4: End-to-end edit** — build and restart img-gen, then submit an `edit` job through the UI (or `curl POST /api/jobs` with `{"mode":"edit",...}`) and confirm a restyled PNG is returned. Edit needs no new checkpoint (uses the already-deployed dev model), so it is the fastest full check.

- [ ] **Step 5: End-to-end blend and inpaint** — repeat through the UI for `blend` (Redux) and `inpaint` (Fill). These are slow (minutes at 512², longer at 1024²) and the Fill checkpoint is large; run at a small size.

- [ ] **Step 6: Commit any flag fixes** that Steps 2-3 revealed, back in Task 1's file if needed.

---

## Self-review notes

- **Spec coverage:** Edit (Task 1 `/edit` already existed + Task 2/5/7), Inpaint (Task 1 `/fill` + Task 2/5/7 + mask painter), Blend (Task 1 `/redux` + Task 2/5/7 + multi-upload) — all four modes covered. Storage discipline (inputs in RAM, never persisted) enforced by Task 5's `inputs` map that is deleted in `run`. Non-goals (no FLUX.2, no ControlNet, no gateway routing) respected.
- **Refinement vs spec:** the spec §8 showed `Edit/Inpaint/Blend` taking `[]byte`; the plan refines inputs to base64 **strings** (pass-through, since the sidecar consumes base64 and only outputs need decoding). No user-visible change. Also, inpaint drops the `size` requirement (fill derives size from image+mask) — a justified correction to the spec §6 table.
- **Type consistency:** `queue.ImageOps` signatures match `lattice` method signatures exactly; `SubmitRequest` field names match the frontend JSON body keys; `storage.Job.Mode` flows through to the history card title.
