# Phase D — Critic / Vision-Pairing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give img-gen the validation limb — an opt-in checklist judge (VLM via the lattice, WD14 tagger sidecar, deterministic palette/tile metrics) wired into a `validating` job stage with bounded mutate-then-resample retries and full attempt provenance.

**Architecture:** A new `internal/critic` op registry (pure-Go deterministic ops + two remote legs), two new `internal/lattice` clients (`ChatVision`, `Tagger`), a `validating` stage inside the existing single-slot worker (`run()` becomes a bounded attempt loop), a synchronous `POST /api/v1/validate` endpoint (the `/api/v1/repair` precedent), and additive job-record/manifest provenance. Attempt images are separately-addressable stored images; the chosen attempt becomes `ImagePath`.

**Tech Stack:** Go 1.24 stdlib only (zero third-party deps — the tagger/VLM legs are remote HTTP). Remote prerequisites (Plan A, LLM-router repo): gateway image-content translation + `deploy/tagger-sidecar.py` on M6:8904.

**Spec:** `docs/superpowers/specs/2026-10-28-critic-vision-pairing-design.md` (rulings, thresholds, lifecycle, phasing D2–D4).

## Global Constraints

- Go 1.24, stdlib only — no new Go dependencies anywhere in this plan.
- Opt-in only: behavior with `validate` absent is byte-identical (new job fields are `omitempty`; history readers ignore unknown fields).
- Envelope codes stay `validation_error` for all submit-time rejections via `&queue.ValidationError{Code: "validation_error", Msg: ...}`; legacy `/api/*` messages must not change.
- Verification renders never touch the live `:8099` instance; use a scratch instance on `:9111` with a temp `DATA_DIR` (house ruling; `:9106` is user-owned).
- No push to origin without explicit user confirmation ([public-repo] rule). Spec-side refinements made while planning (flagged in the handoff): (1) `palette_conform` resolves its target from the explicit `validate.palette` only — a `palette_lock` set's palette is a *post-compose* property (C3 computes it from the children's renders), so per-child checks cannot know it; set-level coherence remains `palette_lock`'s unchanged; (2) post-restart, a recovered validated job completes at its first resolvable attempt — further retries are skipped honestly (documented + tested), because the attach goroutine holds no re-render inputs.
- Threshold constants are pinned by this plan and changed only via recorded rulings with golden updates (Phase C detector precedent).

**File structure map (new/modified):**

| File | Responsibility |
|---|---|
| `internal/critic/critic.go` | types, op enum, `CompileSpec`, availability gate, constants |
| `internal/critic/judge.go` | judge prompt build, strict reply parse, `ChatJudge` adapter |
| `internal/critic/ops.go` | tag drift, palette containment, tile metric, mutation, scoring, aggregation, `Run` |
| `internal/lattice/vision.go` | `Client.ChatVision` (content-parts chat) |
| `internal/lattice/tagger.go` | `Client.Tagger`, `Client.TaggerVocab` (cached) |
| `internal/storage/storage.go` | `CriticCheck`/`CriticAttempt`/`ValidateState` + Job fields |
| `internal/queue/queue.go` | submit validation + `validating` stage, attempt loop, retries, attach continuation |
| `internal/queue/compose.go` | per-asset `validate` field, manifest summary, GIF provided-frame checks |
| `cmd/img-gen/v1.go` | `POST /api/v1/validate` |
| `cmd/img-gen/main.go` | env wiring (`CRITIC_*`, `TAGGER_*`) |
| `cmd/img-gen-mcp/main.go` | tool-description additions (bodies pass through) |
| `deploy/com.img-gen.plist.in`, `deploy/install-macos.sh` | new env plumbing |
| `skills/img-gen-validation/SKILL.md` + 2 skill edits + `README.md` + `docs/openapi.yaml` | docs |

---

### Task 1: critic package — types, constants, compiler

**Files:**
- Create: `internal/critic/critic.go`
- Test: `internal/critic/critic_test.go`

**Interfaces:**
- Consumes: nothing yet (leaf package; later tasks add the remote legs).
- Produces (used by every later task — exact names):
```go
type CheckSpec struct { Op string `json:"op"`; Arg json.RawMessage `json:"arg,omitempty"` }
type ValidateSpec struct {
    Checks  []CheckSpec `json:"checks"`
    Budget  *int        `json:"budget,omitempty"` // nil = default 2; explicit 0 = evaluate-only
    OnFail  string      `json:"on_fail,omitempty"`
    Palette []string    `json:"palette,omitempty"`
}
type Check struct { Op string; Spec string; MaxRatio float64 } // MaxRatio 0 = TileMaxRatio default
type Checklist struct {
    Checks []Check  `json:"checks"`
    Budget int      `json:"budget"`
    OnFail string   `json:"on_fail"` // "" | "deliver" | "fail"
    Target []string `json:"target,omitempty"`
}
type Availability struct{ Tagger bool }
const (
    BudgetDefault = 2
    MaxChecks, MaxVLMCriteria = 8, 4
    TagFloor    = 0.35
    PaletteDeltaE = 8.0
    PaletteCoverage = 0.98
    TileMaxRatio  = 1.3
    MutationMaxChars = 1200
)
func CompileSpec(spec ValidateSpec, av Availability) (Checklist, error)
```

- [ ] **Step 1: Write the failing tests**

```go
package critic

import (
	"encoding/json"
	"strings"
	"testing"
)

func ip[T any](v T) *T { return &v }

func mustCompile(t *testing.T, spec ValidateSpec, av Availability) Checklist {
	t.Helper()
	cl, err := CompileSpec(spec, av)
	if err != nil {
		t.Fatalf("CompileSpec: %v", err)
	}
	return cl
}

func TestCompileDefaultsAndClamps(t *testing.T) {
	cl := mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tag_roundtrip"}}}, Availability{Tagger: true})
	if cl.Budget != BudgetDefault || cl.OnFail != "deliver" {
		t.Fatalf("defaults: %+v", cl)
	}
	cl = mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tile_conform"}}, Budget: ip(9), OnFail: "fail"}, Availability{})
	if cl.Budget != 3 || cl.OnFail != "fail" {
		t.Fatalf("clamp: %+v", cl)
	}
	if cl := mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tile_conform"}}, Budget: ip(0)}, Availability{}); cl.Budget != 0 {
		t.Fatalf("explicit zero must stay evaluate-only: %+v", cl)
	}
}

func TestCompileErrors(t *testing.T) {
	cases := []struct {
		spec ValidateSpec
		av   Availability
		want string
	}{
		{ValidateSpec{Checks: []CheckSpec{{Op: "wat"}}}, Availability{}, "unknown check op"},
		{ValidateSpec{Checks: []CheckSpec{{Op: "tag_roundtrip"}}}, Availability{}, "tag_roundtrip requires the WD14 tagger sidecar (TAGGER_URL)"},
		{ValidateSpec{Checks: make([]CheckSpec, 9)}, Availability{}, "at most 8 check"},
		{ValidateSpec{Checks: []CheckSpec{
			{Op: "vlm_check", Arg: json.RawMessage(`"a"`)}, {Op: "vlm_check", Arg: json.RawMessage(`"b"`)},
			{Op: "vlm_check", Arg: json.RawMessage(`"c"`)}, {Op: "vlm_check", Arg: json.RawMessage(`"d"`)},
			{Op: "vlm_check", Arg: json.RawMessage(`"e"`)}}}, Availability{}, "at most 4 vlm_check"},
		{ValidateSpec{OnFail: "explode"}, Availability{}, "on_fail must be"},
		{ValidateSpec{Palette: []string{"zzz"}, Checks: []CheckSpec{{Op: "palette_conform"}}}, Availability{}, "not a hex color"},
		{ValidateSpec{Checks: []CheckSpec{{Op: "tile_conform", Arg: json.RawMessage(`{"max_ratio":9}`)}}}, Availability{}, "max_ratio must be"},
	}
	for _, c := range cases {
		_, err := CompileSpec(c.spec, c.av)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("CompileSpec(%+v) err=%v want ~%q", c.spec, err, c.want)
		}
	}
}

func TestCompileTileArgAndVLMText(t *testing.T) {
	cl := mustCompile(t, ValidateSpec{Checks: []CheckSpec{
		{Op: "tile_conform", Arg: json.RawMessage(`{"max_ratio":1.1}`)},
		{Op: "vlm_check", Arg: json.RawMessage(`"subject matches"`)}},
	}, Availability{})
	if cl.Checks[0].MaxRatio != 1.1 {
		t.Fatalf("tile arg lost: %+v", cl.Checks[0])
	}
	if cl.Checks[1].Spec != "subject matches" {
		t.Fatalf("vlm spec lost: %+v", cl.Checks[1])
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/critic/` → FAIL (no package).

- [ ] **Step 3: Write `internal/critic/critic.go`**

```go
// Package critic implements Phase D validation: an op registry of checklist
// checks (VLM judge via the lattice, WD14 tagger roundtrip, deterministic
// palette/tile metrics), a compiler for client-submitted specs, and the
// mutate-then-resample retry policy's pure functions (mutation, scoring).
package critic

import (
	"encoding/json"
	"fmt"

	"img-gen/internal/compose"
)

// Threshold constants are pinned by the plan; recalibration is a recorded
// ruling with golden updates (Phase C detector precedent), never silent edits.
const (
	BudgetDefault    = 2
	MaxChecks        = 8
	MaxVLMCriteria   = 4
	TagFloor         = 0.35
	PaletteDeltaE    = 8.0
	PaletteCoverage  = 0.98
	TileMaxRatio     = 1.3
	MutationMaxChars = 1200
)

// CheckSpec is one checklist entry from a client. Arg is per-op JSON:
// vlm_check takes a JSON string (the binary criterion), tile_conform an
// optional {"max_ratio": 1..2}.
type CheckSpec struct {
	Op  string          `json:"op"`
	Arg json.RawMessage `json:"arg,omitempty"`
}

// ValidateSpec is the submit-time opt-in validation config.
type ValidateSpec struct {
	Checks  []CheckSpec `json:"checks"`
	Budget  int         `json:"budget,omitempty"`
	OnFail  string      `json:"on_fail,omitempty"`
	Palette []string    `json:"palette,omitempty"` // palette_conform target (explicit; see spec refinement)
}

// Check is one compiled check.
type Check struct {
	Op       string
	Spec     string  // vlm_check criterion
	MaxRatio float64 // tile_conform override; 0 = TileMaxRatio
}

// Checklist is the compiled, persisted form (storage.Job.CriticSpec holds its
// JSON so restarts re-run it idempotently).
type Checklist struct {
	Checks []Check  `json:"checks"`
	Budget int      `json:"budget"`
	OnFail string   `json:"on_fail"` // "" | "deliver" | "fail"
	Target []string `json:"target,omitempty"`
}

// Availability reports which remote legs are wired (nil Ops/Env functions).
type Availability struct{ Tagger bool }

func CompileSpec(spec ValidateSpec, av Availability) (Checklist, error) {
	if len(spec.Checks) == 0 || len(spec.Checks) > MaxChecks {
		return Checklist{}, fmt.Errorf("validate checks takes 1..%d entries", MaxChecks)
	}
	vlm := 0
	cl := Checklist{OnFail: spec.OnFail}
	if spec.Budget == nil {
		cl.Budget = BudgetDefault
	} else {
		cl.Budget = *spec.Budget
		if cl.Budget < 0 || cl.Budget > 3 {
			return Checklist{}, fmt.Errorf("validate budget must be 0..3")
		}
	}
	switch cl.OnFail {
	case "", "deliver", "fail":
	default:
		return Checklist{}, fmt.Errorf("on_fail must be \"deliver\" or \"fail\" (got %q)", cl.OnFail)
	}
	for _, cs := range spec.Checks {
		c := Check{Op: cs.Op}
		switch cs.Op {
		case "vlm_check":
			var s string
			if err := json.Unmarshal(cs.Arg, &s); err != nil || s == "" {
				return Checklist{}, fmt.Errorf("vlm_check takes a non-empty string criterion")
			}
			c.Spec = s
			vlm++
			if vlm > MaxVLMCriteria {
				return Checklist{}, fmt.Errorf("at most %d vlm_check criteria per checklist", MaxVLMCriteria)
			}
		case "tag_roundtrip":
		case "palette_conform":
		case "tile_conform":
			if len(cs.Arg) > 0 {
				var arg struct {
					MaxRatio float64 `json:"max_ratio"`
				}
				if err := json.Unmarshal(cs.Arg, &arg); err != nil {
					return Checklist{}, fmt.Errorf("tile_conform arg: %v", err)
				}
				if arg.MaxRatio != 0 && (arg.MaxRatio < 1.0 || arg.MaxRatio > 2.0) {
					return Checklist{}, fmt.Errorf("tile_conform max_ratio must be 1.0..2.0")
				}
				c.MaxRatio = arg.MaxRatio
			}
		default:
			return Checklist{}, fmt.Errorf("unknown check op %q (want vlm_check, tag_roundtrip, palette_conform, tile_conform)", cs.Op)
		}
		cl.Checks = append(cl.Checks, c)
	}
	for _, h := range spec.Palette {
		if _, err := compose.ParseHexColor(h); err != nil {
			return Checklist{}, fmt.Errorf("validate palette: %v", err)
		}
		cl.Target = append(cl.Target, h)
	}
	return cl, nil
}

// budget semantics: nil (key omitted) = BudgetDefault; an explicit 0 means
// evaluate-only — the *int on ValidateSpec carries the distinction, so no
// decode-side wrapper is needed.
```

**Note on the explicit-0 case:** to keep `evaluate-only` (budget 0) expressible from JSON, the queue-side decode (Task 11) uses `Budget *int` semantics via a thin `ValidateSpecIn` wrapper — see that task. Inside this package the rule is: `ValidateSpec.Budget` omitted ⇒ default 2; explicit 0 ⇒ 0. Add to `ValidateSpec`:

```go
	// BudgetSet marks a submit where the client explicitly sent budget (=0 is
	// then honored as evaluate-only). Queue decode sets it when the key was
	// present.
	BudgetSet bool `json:"-"`
```
and replace the `cl.Budget` normalization with:
```go
	cl := Checklist{Budget: spec.Budget, OnFail: spec.OnFail}
	if !spec.BudgetSet && spec.Budget == 0 {
		cl.Budget = BudgetDefault
	}
	if cl.Budget < 0 || cl.Budget > 3 {
		return Checklist{}, fmt.Errorf("validate budget must be 0..3")
	}
```
(deleting `specHasNoExplicitZero` and its test-irrelevant call).

- [ ] **Step 4: Run tests → PASS**, then `gofmt -w internal/critic/` and `go vet ./internal/critic/`.

- [ ] **Step 5: Commit**

```bash
git add internal/critic/critic.go internal/critic/critic_test.go
git commit -m "feat(critic): validate spec types, constants, and checklist compiler

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: `ChatVision` lattice client

**Files:**
- Create: `internal/lattice/vision.go`
- Test: `internal/lattice/vision_test.go`

**Interfaces:**
- Produces: `func (c *Client) ChatVision(ctx context.Context, model, system, userText, imageB64 string) (string, error)` — non-streaming OpenAI-shaped chat whose user message carries text + one `image_url` data-URL part. A pre-upgrade gateway's real signature is **HTTP 500 with `ollama returned status` in the body** (the parts body passes through to ollama, which 400s; the gateway's Execute collapses it to 500) — or a direct 400. Both are surfaced as an error naming the upgrade (corrected per the D1 final-review ruling; a 400-only check never fires against the live pre-upgrade gateway).

- [ ] **Step 1: Failing test**

```go
package lattice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatVisionSendsContentParts(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["stream"] != false {
			t.Errorf("stream must be explicit false")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `[{"id":0,"pass":true}]`}}},
		})
	}))
	defer ts.Close()
	c := New(ts.URL)
	out, err := c.ChatVision(context.Background(), "judge:7b", "be strict", "criteria: shape", "QUJD")
	if err != nil {
		t.Fatal(err)
	}
	if out != `[{"id":0,"pass":true}]` {
		t.Fatalf("reply = %q", out)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be strict" {
		t.Fatalf("system = %#v", sys)
	}
	user := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatal("user role")
	}
	parts := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	if tp := parts[0].(map[string]any); tp["type"] != "text" || tp["text"] != "criteria: shape" {
		t.Fatalf("text part = %#v", tp)
	}
	ip := parts[1].(map[string]any)
	if ip["type"] != "image_url" {
		t.Fatal("image part type")
	}
	url := ip["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,QUJD") {
		t.Fatalf("data url = %q", url)
	}
	if got["model"] != "judge:7b" {
		t.Fatalf("model = %#v", got["model"])
	}
}

func TestChatVisionNamesUpgradeOnPreUpgradeGateway(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"direct 400", http.StatusBadRequest, `{"error":{"message":"unknown field content"}}`},
		{"collapsed 500 (the real pre-upgrade shape)", http.StatusInternalServerError, `{"error":"ollama returned status 400"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.code)
				_, _ = w.Write([]byte(c.body))
			}))
			defer ts.Close()
			_, err := New(ts.URL).ChatVision(context.Background(), "m", "s", "u", "QUJD")
			if err == nil || !strings.Contains(err.Error(), "image content") {
				t.Fatalf("want named upgrade error, got %v", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run → FAIL** (`go test ./internal/lattice/ -run TestChatVision`).

- [ ] **Step 3: Implement `internal/lattice/vision.go`**

```go
package lattice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type visionTextPart struct {
	Type string `json:"type"` // "text"
	Text string `json:"text,omitempty"`
}
type visionImagePart struct {
	Type     string             `json:"type"` // "image_url"
	ImageURL visionImageURLData `json:"image_url"`
}
type visionImageURLData struct {
	URL string `json:"url"`
}

// ChatVision sends one non-streaming chat completion whose user message
// carries the text prompt and ONE image (PNG base64 → data URL). It is the
// Phase D critic judge transport: the lattice gateway translates content
// parts into Ollama's native images array (see the lattice gateway plan).
// A gateway that predates the translation rejects the body with 400 — that
// error is surfaced with a named upgrade note, never silently downgraded.
func (c *Client) ChatVision(ctx context.Context, model, system, userText, imageB64 string) (string, error) {
	if c.BaseURL == "" {
		return "", fmt.Errorf("chat vision: no lattice base URL configured")
	}
	body := map[string]any{
		"model": model,
		"stream": false,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": []any{
				visionTextPart{Type: "text", Text: userText},
				visionImagePart{Type: "image_url", ImageURL: visionImageURLData{
					URL: "data:image/png;base64," + imageB64,
				}},
			}},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Pre-upgrade gateway: the parts body reaches ollama's native endpoint,
		// ollama rejects it, and the gateway's Execute collapses upstream
		// non-200s into "ollama returned status N" written as HTTP 500. The
		// named-upgrade error must fire on that real signature (and on any
		// directly-migrated 400) — never on the generic path.
		upgrade := resp.StatusCode == http.StatusBadRequest ||
			(resp.StatusCode == http.StatusInternalServerError &&
				strings.Contains(string(msg), "ollama returned status"))
		if upgrade {
			return "", fmt.Errorf("lattice chat vision: gateway rejected the image content body (%s) — upgrade the lattice gateway to one that translates chat image content parts, or unset the critic's checklist ops that need the judge", strings.TrimSpace(string(msg)))
		}
		return "", fmt.Errorf("lattice chat vision: %s: %s", resp.Status, msg)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("lattice chat vision: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}
```

- [ ] **Step 4: Run → PASS**; `gofmt`; full lattice suite `go test ./internal/lattice/` green.

- [ ] **Step 5: Commit**

```bash
git add internal/lattice/vision.go internal/lattice/vision_test.go
git commit -m "feat(lattice): ChatVision — content-part chat transport for the critic judge

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: `Tagger` lattice client (WD14 sidecar)

**Files:**
- Create: `internal/lattice/tagger.go`
- Test: `internal/lattice/tagger_test.go`

**Interfaces:**
- Consumes: the sidecar contract from the LLM-router plan (`/health`, `/meta`, `/tag`).
- Produces: `Client.TaggerURL string` field (add to the `Client` struct in `internal/lattice/lattice.go`); `func (c *Client) Tagger(ctx, imageB64) ([]TagScore, error)`; `func (c *Client) TaggerVocab(ctx) ([]string, error)` — vocabulary fetched once, cached under a mutex, with a failed fetch retried on the next call.

```go
package lattice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// TagScore is one WD14 tag and its model score.
type TagScore struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
}

var (
	tagVocabMu     sync.Mutex
	tagVocabCached []string
)

// Tagger sends one image to the WD14 sidecar, returning tags ≥ its server
// threshold with scores. Errors carry the "tagger sidecar:" prefix like the
// rembg/rife clients.
func (c *Client) Tagger(ctx context.Context, imageB64 string) ([]TagScore, error) {
	body, err := json.Marshal(map[string]any{"image": imageB64})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.TaggerURL, "/")+"/tag", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tagger sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("tagger sidecar: HTTP %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Tags []TagScore `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("tagger sidecar body: %w", err)
	}
	return out.Tags, nil
}

// TaggerVocab returns the WD14 tag vocabulary (GET /meta), fetched once and
// cached; a failed fetch is retried on the next call. A nil TaggerURL must
// never reach here (submit validation gates tag ops on the client being wired).
func (c *Client) TaggerVocab(ctx context.Context) ([]string, error) {
	tagVocabMu.Lock()
	defer tagVocabMu.Unlock()
	if tagVocabCached != nil {
		return tagVocabCached, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.TaggerURL, "/")+"/meta", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tagger sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tagger sidecar meta: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tagger sidecar meta body: %w", err)
	}
	tagVocabCached = out.Tags
	return tagVocabCached, nil
}
```

Steps mirror Task 2: fake `/tag` + `/meta` httptest asserting body `{image}` and tags decode; vocab cached (second call hits the server once — count handler invocations); commit `feat(lattice): Tagger + TaggerVocab clients for the WD14 sidecar`.

---

### Task 4: the judge (VLM checklist op)

**Files:**
- Create: `internal/critic/judge.go`
- Test: `internal/critic/judge_test.go`

**Interfaces:**
- Consumes: `lattice.ChatVision`.
- Produces:
```go
type VLMScore struct {
	ID     int    `json:"id"`
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"`
	Fix    string `json:"fix,omitempty"`
}
type Judge func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error)
func ChatJudge(c *lattice.Client, model string) Judge   // wraps ChatVision; caller owns the model (CRITIC_MODEL)
func (j Judge) isNil() bool  // helper: reflect-free nil check via j == nil
```
Judge errors: malformed reply → `ErrJudgeUnreadable` wrapping a 4096-byte-truncated reply head.

- [ ] **Step 1: Failing tests** (httptest server answering valid JSON array; criteria order preserved by id; malformed reply → `ErrJudgeUnreadable` with head truncation; model threading asserted server-side; one call per criteria batch).

- [ ] **Step 2: Implement** — prompt contract (verbatim in code):

```go
// judgeSystem is the history-free judge preamble: binary per-criterion
// verdicts, strict JSON, nothing else. The judge never converses.
const judgeSystem = "You are an image QA judge. For each numbered criterion, " +
	"look at the image and decide pass or fail. Answer ONLY with a JSON array, " +
	"one object per criterion, in the same order: " +
	`[{"id": <criterion index starting at 0>, "pass": <true|false>, ` +
	`"reason": "<one short sentence>", "fix": "<how to fix the prompt when it failed, one short sentence, omit when pass>"}]. ` +
	"No prose, no markdown fences."
```

`ChatJudge` builds `userText` = `"Criteria:\n0. <first>\n1. <second>..."` and calls `c.ChatVision(ctx, model, judgeSystem, userText, imageB64)`; parses with `json.Unmarshal([]byte(reply), &scores)`; on parse error returns `fmt.Errorf("%w: reply head: %.4096s", ErrJudgeUnreadable, reply)`. Criteria >0 missing from the reply array → those criteria default to `{Pass:false, Reason:"judge omitted criterion"}` only when the array is well-formed but incomplete — document in the reply-parse comment: incomplete arrays are judge errors too, so map missing ids to `Pass:false` NOT pass (never fail-open). Actually follow the spec exactly: malformed → the whole call errors (caller marks `undetermined`); well-formed-but-incomplete → error as well (call it malformed): if `len(scores) < len(criteria)` → ErrJudgeUnreadable. Strictness beats cleverness.

```go
var ErrJudgeUnreadable = errors.New("critic: judge reply unreadable")
```

- [ ] **Step 3: Tests pass; full critic suite green; commit** `feat(critic): VLM judge — history-free criteria batch with strict JSON replies`.

---

### Task 5: tag drift, palette containment, tile metric

**Files:**
- Modify: `internal/compose/remap.go` (two exported wrappers at the end — the ONE palette math implementation stays there)
- Create: `internal/critic/ops.go` (the deterministic ops + mutation/scoring/aggregation/Run arrive in Tasks 5–6; this task adds the three pure ops)
- Test: `internal/critic/ops_test.go`, `internal/compose/remap_test.go` (extend)

**Interfaces:**
- Produces (`critic`):
```go
func TagDrift(prompt string, vocab map[string]bool, tags []TagScore, floor float64) (missing []string, known bool)
   // known=false when the vocab is unavailable (upstream op error, not drift)
func PromptTags(prompt string) []string  // danbooru tokenization: comma-split, trim, lowercase, spaces→underscores
func PaletteContain(img image.Image, targetHexes []string, pixelFixed bool) (ok bool, detail string)
func TileMetric(img image.Image) (ratio float64, ok bool)  // pass iff ratio ≤ maxRatio passed by caller
```
- Produces (`compose`): `func LAB(r, g, b float64) (l, a, bb float64)` and `func CIEDE2000(l1, a1, b1, l2, a2, b2 float64) float64` — exported wrappers over the existing unexported `rgbToLAB`/`cie2000` (one implementation, two faces).

- [ ] **Step 1: compose wrapper tests** — `TestCIEDE2000ExportedAgrees`: `compose.CIEDE2000(compose.LAB(...))` equals in-package `cie2000`/`rgbToLAB` results on a fixed color pair (e.g. pure red vs pure blue; assert same value to 1e-9 and >20).

- [ ] **Step 2: palette op tests** — synthetic NRGBA: (a) pixelFixed=true, all pixels exactly from a 3-color target → pass with containment detail "pixel_fix: exact containment"; one off-palette pixel → fail; (b) pixelFixed=false: ≥98% within ΔE≤8 → pass; a 5% red-patch on a green field (ΔE huge) → fail with `detail` naming the coverage fraction `fmt.Sprintf("%.3f", cov)`.
- [ ] **Step 3: tile op tests** — build a checkerboard-in-cell image where wrapped edges continue the pattern (seamless synthetic): ratio ≤ 1.0 → pass; a synthetic image with a deliberate 1-pixel border band of a different color: wrap-edge mean error >> interior variation → ratio > 2 → fail. The metric:

```go
// TileMetric returns the normalized wrap-edge error: the mean per-channel
// absolute difference across wrapped boundaries divided by the interior
// variation baseline (mean absolute neighbor difference away from the
// edges). ≈1 means the edges carry no more signal than the interior —
// the TexTile-lite property. Non-square or trivially-flat images pass on
// the flatness bail-out (interior baseline 0 and edge error 0).
```
implementation sketch (pure loops over an NRGBA): edgeErr = mean over all four boundary pairs |V(x,y) − V(x±w,y±h wrapped)|; interiorErr = mean |V − V(neighbors)| sampled over the interior rows/cols only (skip the outermost row/col); ratio = edgeErr / max(interiorErr, 1e-6).

- [ ] **Step 4: tag drift tests** — prompt `"1girl, red gloves, long hair"` vs vocab {1girl, gloves, long_hair}: missing ["gloves"] when tags score below floor; `known=false` when vocab nil; multi-word normalization (`long hair`→`long_hair`).

- [ ] **Step 5: All pass; commit** `feat(critic): deterministic ops — tag drift, palette containment, tile metric (+ compose CIEDE2000/LAB exports)`.

---

### Task 6: Runner — `Run`, `Aggregate`, `Score`, `MutatedPrompt`

**Files:**
- Modify: `internal/critic/ops.go` (append) and `internal/critic/critic.go` (Env types)
- Test: `internal/critic/run_test.go`

**Interfaces:**
- Produces:
```go
type Env struct {
    Judge  Judge       // nil-checked at Run time
    Tagger func(ctx context.Context, imageB64 string) ([]TagScore, error)
    Vocab  func(ctx context.Context) ([]string, error)
}
type TagScore struct{ Name string; Score float64 } // local alias? NO — reuse lattice.TagScore: critic imports lattice already via judge.go
func Run(ctx context.Context, cl Checklist, imageB64 string, e Env) []storage.CriticCheck
func Aggregate(rs []storage.CriticCheck) string // "pass" | "failed" | "indeterminate"
func Score(rs []storage.CriticCheck) float64    // pass/(pass+fail); 0 when both zero
func Fails(rs []storage.CriticCheck) int
func Relevant(rs []storage.CriticCheck) int     // pass+fail count
func MutatedPrompt(base string, rs []storage.CriticCheck) string
```
Semantics (each pinned by a test):
- All `vlm_check` criteria batch into ONE Judge call per Run; judge error or unreadable reply → every VLM check `undetermined` with `Reason` = trimmed error (not fail-open).
- `tag_roundtrip`: vocab fetch error → `error` status (inert); else missing required tags → `fail` with `Suggestion: "strengthen: a, b"`; no vocab-known tokens → `pass` with Reason "no known tags to verify". Tagger call error → `error` (inert, never consumes budget).
- `palette_conform`: no Target → `error` + Reason "no target palette"; else pass/fail from `PaletteContain` (Suggestion "palette drift" on fail).
- `tile_conform`: ratio > MaxRatio(default or override) → `fail` with `Suggestion: fmt.Sprintf("wrap-edge ratio %.2f > %.2f", ratio, max)`; else pass with the ratio in Reason.
- `MutatedPrompt`: vlm fails' `Fix` + tag drift suggestions appended `", "`-joined after dedupe (case-insensitive, also against base substrings), total ≤ `MutationMaxChars` (truncate at comma boundary); returns base unchanged when nothing to add.
- `Aggregate`: ≥1 `fail` → "failed"; all zero pass/fail (all inert) → "indeterminate"; else "pass". (The sync endpoint uses this; job-side verdict derivation lives in Task 8.)

- [ ] Steps: failing table tests per semantic bullet (fake Judge/Tagger/Vocab closures with call counters) → implement → pass → `gofmt` → **commit** `feat(critic): checklist runner — batching, statuses, mutation, scoring`.

---

### Task 7: storage record shapes

**Files:**
- Modify: `internal/storage/storage.go` (Job struct + three new types)
- Test: `internal/storage/storage_test.go` (extend)

**Interfaces:**
- Produces:

```go
// --- critic (Phase D) bookkeeping ---
// CriticCheck is one evaluated validation op; Status ∈ pass|fail|undetermined|error.
type CriticCheck struct {
	Op         string `json:"op"`
	Spec       string `json:"spec,omitempty"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// CriticAttempt is one render attempt of a validated job (attempt 1 first).
// Image is the stored image id (16-hex) whose PNG lives at images/<id>.png —
// attempts are separately-addressable so the chosen one becomes ImagePath.
type CriticAttempt struct {
	Image  string        `json:"image"`
	Prompt string        `json:"prompt"`
	Seed   *int64        `json:"seed,omitempty"`
	Checks []CriticCheck `json:"checks,omitempty"`
	Chosen bool          `json:"chosen,omitempty"`
}

// ValidateState is a job's validation summary.
type ValidateState struct {
	Verdict  string `json:"verdict"` // pass | best_of_n | failed | indeterminate
	Attempts int    `json:"attempts"`
}
```

Add to `Job` (after the compose block, same comment style):

```go
	// --- critic (Phase D) bookkeeping ---
	// CriticSpec is the compiled checklist JSON (critic.Checklist) so a
	// restart re-runs validation idempotently; absent = the job never opted in.
	CriticSpec string `json:"critic_spec,omitempty"`
	// CriticPending is the 1-based attempt currently rendering or being
	// validated (set before each dispatch); CriticNextPrompt is the prompt
	// it renders. Cleared on terminal.
	CriticPending   *int    `json:"critic_pending,omitempty"`
	CriticNextPrompt string `json:"critic_next_prompt,omitempty"`
	// Critic is the summary once the chain resolved; Attempts lists renders.
	Critic   *ValidateState  `json:"validate,omitempty"`
	Attempts []CriticAttempt `json:"attempts,omitempty"`
```

- [ ] Steps: JSON round-trip test (a Job with all fields marshals + unmarshals identically; an OLD record without them unmarshals with zero values — backward compat); **commit** `feat(storage): critic record shapes on the job`.
- [ ] **Also this task:** attempt images need independent deletion. Extend `Manager.Delete` (queue.go) to remove every attempt image: after `RemoveImage(job.ID)`, `for _, a := range job.Attempts { _ = m.opts.Store.RemoveImage(a.Image) }` (RemoveImage already tolerates missing files — verify with the same test file's Delete case + new assertion). Include.

---

### Task 8: sync endpoint `POST /api/v1/validate`

**Files:**
- Modify: `cmd/img-gen/v1.go` (new handler + deps fields), `cmd/img-gen/main.go` (wiring lands in Task 12 — this task uses test-local deps)
- Modify: `docs/openapi.yaml` (new path + schemas)
- Test: `cmd/img-gen/v1_validate_test.go` (new)

**Interfaces:**
- Consumes: `critic.CompileSpec`, `critic.Run`, `critic.Aggregate`, `compose.RepairMaxDim`.
- v1Deps gains `critic critic.Env` + `criticTimeout time.Duration`.

Handler (verbatim contract):

```go
type criticRequest struct {
	Image   string             `json:"image"`
	Checks  []critic.CheckSpec `json:"checks"`
	Palette []string           `json:"palette,omitempty"`
}

// POST /api/v1/validate — sync checklist evaluation of one image (the
// /api/v1/repair precedent: bounded size, bounded time, no retry logic).
func (d *v1Deps) handleV1Validate(w http.ResponseWriter, r *http.Request) {
	var req criticRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body: "+err.Error(), "")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Image)
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "image is not valid base64", "")
		return
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "image is not a PNG", "")
		return
	}
	if cfg.Width > compose.RepairMaxDim || cfg.Height > compose.RepairMaxDim {
		writeV1Err(w, http.StatusBadRequest, "validation_error",
			fmt.Sprintf("image is %dx%d; validate supports up to %dx%d", cfg.Width, cfg.Height, compose.RepairMaxDim, compose.RepairMaxDim), "")
		return
	}
	cl, err := critic.CompileSpec(critic.ValidateSpec{
		Checks: req.Checks, Palette: req.Palette, Budget: ip(0),
	}, critic.Availability{Tagger: d.critic.Tagger != nil})
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", err.Error(), "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), d.criticTimeout)
	defer cancel()
	checks := critic.Run(ctx, cl, req.Image, d.critic)
	writeJSON(w, map[string]any{"verdict": critic.Aggregate(checks), "checks": checks})
}
```
(Wire `mux.HandleFunc("/api/v1/validate", ...)` with the POST-only guard beside `/api/v1/repair`.)

**openapi.yaml:** new `/api/v1/validate` path (200 `{"verdict","checks"}`, 400 ErrorResponse) + schemas `CriticCheckSpec` (`op` enum of the four, `arg`), `ValidateResponse`, reuse `ErrorResponse`. Hand-maintained header note stays.

**Tests:** fake judge/tagger/vocab into `v1Deps.critic`; verdict pass / failed / indeterminate; 400 matrix (bad base64, non-PNG, oversize, unknown op, tag op with nil Tagger → message names `TAGGER_URL`); a docs-sync assertion:

```go
func TestOpenAPIDocumentsValidate(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil { t.Fatal(err) }
	for _, want := range []string{"/api/v1/validate", "CriticCheckSpec"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("openapi.yaml missing %q", want)
		}
	}
}
```

- [ ] **Commit** `feat(v1): sync /api/v1/validate — checklist evaluation of one image`.

---

### Task 9: submit-time validation (queue)

**Files:**
- Modify: `internal/queue/queue.go` (SubmitRequest field + submitOne gate) and `internal/queue/compose.go` (`AssetSpecIn.Validate`, `GifRequest.Validate`, per-asset compile, gif payload persistence)
- Test: `internal/queue/queue_test.go` (extend), `internal/queue/compose_test.go` (extend)

**Interfaces:**
- Consumes: `critic.CompileSpec/Checklist`.
- Produces:
  - `SubmitRequest.Validate *critic.ValidateSpec json:"validate,omitempty"`.
  - `AssetSpecIn.Validate *critic.ValidateSpec json:"validate,omitempty"`; persisted `assetSpec.Validate` (same JSON shape, so recompose re-applies).
  - `GifRequest.Validate *critic.ValidateSpec json:"validate,omitempty"`; `gifPayload.Validate *critic.ValidateSpec` (persisted verbatim; from_spec threads it onto every child SubmitRequest).

queue.go — after the SDXL mode gate in `submitOne` (behind the sdxl gate at queue.go:603):

```go
	// Phase D: validate is an opt-in checklist on jobs that render images.
	if req.Validate != nil {
		if mode != "generate" && mode != "asset" {
			return "", &ValidationError{Code: "validation_error",
				Msg: fmt.Sprintf("validate is supported on generate and asset jobs, not %q", mode)}
		}
		if _, err := critic.CompileSpec(*req.Validate, critic.Availability{Tagger: m.optsCritic()?.Tagger != nil}); err != nil {
			return "", &ValidationError{Code: "validation_error", Msg: err.Error()}
		}
	}
```
with a small accessor over `Options.Critic` (the compiled spec is re-marshaled by Task 10 when the job is built — compile once here for validation, store the spec as submitted; Task 10 compiles at job-build time and persists the compiled JSON).

`Options` gains `Critic critic.Env`.

compose.go — in `SubmitAssetSet`'s per-asset loop, after the Outline block:

```go
		// Phase D: per-asset validate reuses the job-level gate; the compiled
		// error names the asset like every other per-asset validation.
		if a.Validate != nil {
			if _, cerr := critic.CompileSpec(*a.Validate, critic.Availability{Tagger: m.opts.Critic.Tagger != nil}); cerr != nil {
				return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf("asset %q: %v", an, cerr)}
			}
		}
```
(stored onto `as.Validate`; children get it in Task 10.) GIF: in `SubmitGIF` before the source switch — compile once; for `from_spec`, set `frames.Validate = req.Validate` before `specFrames`; for from_frames/composite/keyframes, persist `spec.GIF.Validate = req.Validate` (evaluate-only at compose; Task 12).

**Tests:** generate job with unknown op → rejected with the op named; `tag_roundtrip` with no Tagger → "TAGGER_URL" message; asset spec validate error names `asset "logo"`; gif from_spec propagates; edit-mode + validate → mode message.

- [ ] **Commit** `feat(queue): validate field submit gating on jobs, assets, and gifs`.

---

### Task 10: the `validating` stage — attempt loop, mutation, best-of

**Files:**
- Modify: `internal/queue/queue.go` — refactor `run()`'s render tail into `renderAndFinish`, add `criticLoop`, `finishCritic`, `runChecks`, `persistCriticPending`, `persistJob`; `Cancel` accepts `validating`.
- Test: `internal/queue/critic_loop_test.go` (new)

**Interfaces:**
- Consumes: critic.Run/Score/MutatedPrompt/compile, storage attempt fields, the existing dispatch machinery.
- Produces: `type attemptRenderer func(ctx context.Context, prompt string, sp storage.SamplingParams) ([]byte, *int64, error)` — the seam run() and the restart path (Task 11) both use. Attempt images get fresh 16-hex ids (`newID()`), and tEXt gains `critic_attempt`.

The refactor, precisely:

```go
// run() keeps everything up to the style/preset prepend and job.Prompt set;
// then:
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancels[id] = cancel
	m.mu.Unlock()

	spec := storage.ModelSpec{Model: job.Model, LatticeModel: job.ModelKey, Loras: job.Loras, Sidecar: job.Sidecar}
	seed, steps, guidance, negative := job.Seed, job.Steps, job.Guidance, job.NegativePrompt
	if job.CriticSpec == "" {
		// Pre-existing behavior, verbatim: one render, save, finish.
		m.renderAndFinish(ctx, id, job, inputs, job.Prompt, spec,
			storage.SamplingParams{Seed: seed, Steps: steps, Guidance: guidance, NegativePrompt: negative})
		return
	}
	var cl critic.Checklist
	if err := json.Unmarshal([]byte(job.CriticSpec), &cl); err != nil {
		m.finish(id, "failed", "critic: persisted checklist unparsable: "+err.Error())
		return
	}
	renderer := func(pctx context.Context, prompt string, sp storage.SamplingParams) ([]byte, *int64, error) {
		return m.dispatch(pctx, id, job, inputs, prompt, spec, sp)
	}
	m.criticLoop(ctx, id, job, renderer, job.Prompt /* base */, cl, 1, nil)
```

```go
// criticLoop implements the Phase D retry policy: attempt 1 renders the base
// prompt; attempt 2 renders the mutated prompt at the SAME seed; attempt
// N ≥ 3 renders the mutated prompt at seed+(N−2). Budget B allows 1+B
// attempts. A render error never consumes critic budget. Runs on the worker
// slot; every attempt is persisted as it lands (restart-safe).
func (m *Manager) criticLoop(ctx context.Context, id string, job *storage.Job,
	renderer attemptRenderer, base string, cl critic.Checklist, start int, recs []storage.CriticAttempt) {

	onFail := cl.OnFail
	if onFail == "" {
		onFail = "deliver"
	}
	mutation := "" // set once, after attempt 1 fails
	for attempt := start; ; attempt++ {
		if m.isCancelled(id) {
			m.finish(id, "cancelled", "")
			return
		}
		prompt := base
		if attempt >= 2 {
			prompt = mutation
		}
		sp := storage.SamplingParams{Seed: job.Seed, Steps: job.Steps, Guidance: job.Guidance, NegativePrompt: job.NegativePrompt}
		if attempt >= 3 && job.Seed != nil {
			s := *job.Seed + int64(attempt-2)
			sp.Seed = &s
		}
		m.persistCriticPending(job, attempt, prompt)
		m.setStatus(id, "generating", "")
		png, seed, err := renderer(ctx, prompt, sp)
		if err != nil {
			if m.isCancelled(id) {
				m.finish(id, "cancelled", "")
				return
			}
			m.log(id, "attempt %d render failed: %v", attempt, err)
			m.finish(id, "failed", err.Error())
			return
		}
		if m.isCancelled(id) {
			m.finish(id, "cancelled", "")
			return
		}
		if seed != nil {
			m.mu.Lock()
			job.Seed = seed
			m.mu.Unlock()
			m.log(id, "seed=%d", *seed)
		}
		attID := newID()
		kv := renderParamsKV(*job, imageInputs{})
		kv["critic_attempt"] = strconv.Itoa(attempt)
		rel, serr := m.opts.Store.SaveImage(attID, m.withRenderParams(png, kv))
		if serr != nil {
			m.finish(id, "failed", serr.Error())
			return
		}
		m.setStatus(id, "validating", "")
		checks := m.runChecks(cl, png)
		rec := storage.CriticAttempt{Image: attID, Prompt: prompt, Seed: seed, Checks: checks}
		recs = append(recs, rec)
		m.mu.Lock()
		job.Attempts = recs
		job.CriticPending = nil
		m.mu.Unlock()
		m.persistJob(*job)
		for _, c := range checks {
			m.log(id, "check %s(%s): %s %s", c.Op, c.Spec, c.Status, c.Reason)
		}
		if fails := critic.Fails(checks); fails == 0 || attempt >= 1+cl.Budget {
			m.finishCritic(id, job, recs, onFail)
			return
		}
		if attempt == 1 {
			mutation = critic.MutatedPrompt(base, checks)
			if mutation != base {
				m.log(id, "retry candidate prompt: %q", mutation)
			}
		}
	}
}
```

`persistCriticPending` sets `CriticPending`+`CriticNextPrompt` under lock and appends history; `persistJob` appends history under lock with the same log-on-error convention as `captureGenID`. `finishCritic`:

```go
// finishCritic resolves the attempt chain: best attempt by score (tie: lower
// attempt index), verdict per the spec taxonomy, chosen PNG re-embedded with
// critic_* tEXt chunks (best-effort), then the terminal transition.
func (m *Manager) finishCritic(id string, job *storage.Job, recs []storage.CriticAttempt, onFail string) {
	best := 0
	bestScore := -1.0
	for i, r := range recs {
		if s := critic.Score(r.Checks); s > bestScore {
			bestScore, best = s, i
		}
	}
	recs[best].Chosen = true
	ch := recs[best]
	verdict := "pass"
	switch {
	case critic.Relevant(ch.Checks) == 0:
		verdict = "indeterminate"
	case critic.Fails(ch.Checks) > 0:
		verdict = "best_of_n"
	}
	if onFail == "fail" && critic.Fails(ch.Checks) > 0 {
		verdict = "failed"
	}
	attID := ch.Image
	m.mu.Lock()
	job.ImagePath = "images/" + attID + ".png"
	job.Critic = &storage.ValidateState{Verdict: verdict, Attempts: len(recs)}
	job.CriticPending = nil
	j := *job
	m.mu.Unlock()
	m.persistJob(j)
	// Verdict provenance on the chosen artifact (best-effort, A3 convention).
	if data, err := m.opts.Store.ReadImage(attID); err == nil {
		kv := map[string]string{"critic_verdict": verdict, "critic_attempt": strconv.Itoa(best + 1)}
		for _, c := range ch.Checks {
			kv["critic_"+c.Op] = c.Status
		}
		if nb, err := compose.EmbedTextPNG(data, kv); err == nil {
			_ = m.opts.Store.SaveImage(attID, nb)
		}
	}
	if verdict == "failed" {
		var fails []string
		for _, c := range ch.Checks {
			if c.Status == "fail" {
				fails = append(fails, c.Op+": "+c.Reason)
			}
		}
		m.finish(id, "failed", "critic failed: "+strings.Join(fails, "; "))
		return
	}
	m.log(id, "critic verdict %s after %d attempt(s), attempt %d chosen", verdict, len(recs), best+1)
	m.finish(id, "done", "")
}
```

Also in `run()` the pre-existing tail becomes `renderAndFinish` **verbatim** (dispatch → save with `renderParamsKV` → ImagePath → finish done) so the non-validated path is provably unchanged.

Cancel gains `"validating"` in its status switch (job cancel during checks/retries aborts via `m.isCancelled` checks in the loop).

**Tests (the retry state machine, fake renderer):**
- pass first attempt → verdict `pass`, attempts=1, chosen flag set, ImagePath = attempt id.
- attempt-1 fail + budget 2 → exactly 3 renderer calls; prompts: [base, mutation, mutation]; seeds: [s, s, s+1] (assert via captured SamplingParams); verdict from attempt-3's checks if it passes → `pass` with attempts=3.
- budget 0 + fail → one render, verdict `best_of_n` (`on_fail=deliver`) / job failed with "critic failed:" (on_fail=fail).
- all checks undetermined → verdict `indeterminate`, delivered.
- cancel mid-validating → status `cancelled` (Cancel's switch), renderer ctx cancelled.
- attempt images: two distinct ids exist on disk; chosen equals ImagePath.
- SSE: a subscriber sees `validating` events (Subscribe replays + live broadcasts).

- [ ] **Commit** `feat(queue): validating stage — bounded mutate-then-resample retries with best-of-N provenance`.

---

### Task 11: restart recovery of validated jobs

**Files:**
- Modify: `internal/queue/queue.go` (`attach`, `saveRecovered`, `restore` untouched-invariant check)
- Test: `internal/queue/critic_restore_test.go`

**Interfaces:**
- Consumes: `CriticPending`/`CriticNextPrompt`/`Attempts` persistence from Task 10.
- Produces: the documented post-restart limitation — a recovered chain resolves at its first resolvable attempt; later retries are skipped.

Logic (in `attach()`, before the poll loop, after locking job fields at the top):

```go
	// Phase D: when the pending attempt's image is already stored (the
	// process died while running checks), the chain resumes from validation —
	// no sidecar re-attach is needed. Otherwise the recovered render (below,
	// saveRecovered) IS that attempt. Either way the chain then FINALIZES
	// from the attempts recorded so far: further retries are skipped honestly
	// (the attach goroutine holds no re-render inputs), a documented Phase D
	// limitation. Kill-during-checks recovers the verdict; kill-during-render
	// recovers via the sidecar result first.
	m.mu.Lock()
	pend := job.CriticPending
	m.mu.Unlock()
```
and `saveRecovered` (after storing + ImagePath) branches:

```go
	if j.CriticSpec != "" && j.CriticPending != nil {
		// The recovered render completes the pending attempt: record it, run
		// its checks, and finalize the chain (no further retries post-restart).
		pending := *j.CriticPending
		attID := newID()
		rel := saved with kv["critic_attempt"] = strconv.Itoa(pending)
		checks := m.runChecks(cl, recoveredPNG)      // cl unmarshaled from CriticSpec
		recs := append(copy of j.Attempts, CriticAttempt{Image: attID, Prompt: j.CriticNextPrompt, Seed: j.Seed, Checks: checks})
		m.finishCritic(id, job, recs, onFail-from-cl)
		return
	}
	m.finish(id, "done", "")  // existing path, unchanged for non-critic jobs
```
The kill-during-checks path (attempts already ≥ pending) resumes without the sidecar: skip the Result poll when `len(job.Attempts) >= *job.CriticPending`, evaluate the stored attempt's bytes (idempotent — checks are stateless), then `finishCritic`.

`restore()` needs **no change**: a job with GenID attaches; one without fails honestly — but the honest-fail message for a never-rendered validated job stays the existing wording (the resubmit hint holds; params include validate).

**Tests:** fake Ops with controllable Result; (a) kill while checks run (persisted Attempts ≥ pending) → restart → verdict computed from stored bytes, no Result call (assert via counter); (b) kill during attempt-2 render (GenID set) → restart → Result returns the PNG → recovered attempt recorded with `CriticNextPrompt` → chain finalizes (no attempt 3); (c) no GenID mid-chain → honest failure unchanged.

- [ ] **Commit** `feat(queue): restart recovery of validated jobs — idempotent check resumption, no-retry finalize`.

---

### Task 12: compose integration — assets, manifest, GIF frames

**Files:**
- Modify: `internal/queue/compose.go` (child build-through of `Validate`, manifest summary, GIF provided-frame checks)
- Test: `internal/queue/compose_test.go` (extend)

**Interfaces:**
- Consumes: the compiled `Checklist` JSON flow (asset → children; gif payload for provided frames).
- Produces: manifest extensions — `manifestAsset.Validate *storage.ValidateState json:"validate,omitempty"` + `manifestAsset.Attempts []storage.CriticAttempt json:"attempts,omitempty"`; `manifestGIF.FrameChecks []manifestFrameChecks json:"frame_checks,omitempty"` where `type manifestFrameChecks struct { File string; Checks []storage.CriticCheck }`.

1. **Asset children** (`SubmitAssetSet` child loop): `for i := range built { built[i].Validate = a.Validate }` — every child of an asset validates (dual-pair siblings included; palette/tile checks on white/black siblings are harmless and honest).
2. **Manifest summary** (`composeAssetSet`): when building each `manifestAsset`, read each child's `job.Critic`/`job.Attempts` (a small `m.childCritic(childID)` accessor under `m.mu` copying the job pointer's fields) and set `ma.Attempts` from the FIRST child of the asset (representative; dual siblings share the story) + `ma.Validate` from that job's summary. Keep the existing per-asset error semantics untouched — validation verdicts ride IN the manifest, they don't change composition.
3. **GIF provided frames** (`composeGIF`, in the `len(gp.Frames) > 0 && gp.Validate != nil` case): for each persisted source frame, decode bytes and run `critic.Run` with the compiled checklist (persisted inside `gifPayload.Validate` as a submitted spec — compile at compose time with `Availability{Tagger: m.opts.Critic.Tagger != nil}`; a compile error here fails the parent with `compose_error:` prefix). Record every frame's checks into `man.GIF.FrameChecks`. Under `on_fail == "fail"`, any `fail` fails the parent naming the frame file (`compose_error: frame N: <op> failed: <reason>`); under `deliver` (default), a failed frame is recorded and the GIF still assembles (the composite-overhang honesty rule).
   Note: composite statics and keyframes are NOT frame-checked (only `gp.Frames`).
4. **from_spec children** need no compose-side work — their `Validate` rode through Task 9 and the per-child loop of Task 10 owns retries; but `composeGIF`'s children branch adds the same manifest recording as (2) where trivially readable.

**Tests:** (a) 2-frame from_frames with fake tagger: one frame fails tag drift, default on_fail → GIF still delivered, manifest carries frame checks; on_fail=fail → parent failed naming the frame; (b) asset child with validated fail→retry→pass: parent composes from the child's chosen attempt (childResult reads the child's ImagePath); manifest shows `validate.attempts == 2`; (c) palette_lock + validated children: the set remap still runs after attempts (assert final PNG palette ⊆ set palette).

- [ ] **Commit** `feat(queue): compose integration — validated children, manifest summaries, provided-frame checks`.

---

### Task 13: env wiring, plists, install script, MCP descriptions, main.go

**Files:**
- Modify: `cmd/img-gen/main.go` (config + wiring), `deploy/com.img-gen.plist.in`, `deploy/install-macos.sh`
- Modify: `cmd/img-gen-mcp/main.go` (tool descriptions only — the `request` objects pass `validate` through as raw maps)
- Test: `cmd/img-gen/main_test.go` (extend: env-derived config fields)

**Interfaces:**
- config gains:
```go
	CriticModel   string        // CRITIC_MODEL (default "huihui_ai/qwen2.5-vl-abliterated:latest" — full tag, control-plane passthrough rule)
	CriticTimeout time.Duration // CRITIC_TIMEOUT_S (default 120)
	TaggerURL     string        // TAGGER_URL ("" = the tag ops fail submission honestly)
	TaggerTimeout time.Duration // TAGGER_TIMEOUT_S (default 60)
```
- wiring in `newHandler`:
```go
	lat.TaggerURL = cfg.TaggerURL
	lat.HTTP = &http.Client{Timeout: cfg.ImageTimeout} // unchanged; critic calls context their own deadlines

	criticEnv := critic.Env{Judge: critic.ChatJudge(lat, cfg.CriticModel)}
	if cfg.TaggerURL != "" {
		criticEnv.Tagger = lat.Tagger
		criticEnv.Vocab = lat.TaggerVocab
	}
	mgr := queue.New(queue.Options{ ..., Critic: criticEnv })
	// v1Deps gains critic: criticEnv, criticTimeout: cfg.CriticTimeout
```
- plist template env block, next to the REMBG_URL placeholder:

```xml
        <key>TAGGER_URL</key>
        <string>__TAGGER_URL__</string>
        <key>CRITIC_MODEL</key>
        <string>__CRITIC_MODEL__</string>
```
- install-macos.sh: `TAGGER_URL="${TAGGER_URL:-}"` + `CRITIC_MODEL="${CRITIC_MODEL:-huihui_ai/qwen2.5-vl-abliterated:latest}"` config lines beside the RIFE/REMBG ones (lines ~34–38), and `-e "s|__TAGGER_URL__|$TAGGER_URL|g"` + `-e "s|__CRITIC_MODEL__|$CRITIC_MODEL|g"` added to the existing sed chain (lines ~74–75), with a header comment documenting the two envs like the RIFE/REMBG ones. Verify every `__PLACEHOLDER__` in the plist template has exactly one sed substitution and vice versa.
- MCP tool descriptions: `submit_job`/`generate_and_wait` gain one sentence: "validate (optional): Phase D checklist {checks:[{op: vlm_check(str criterion)|tag_roundtrip|palette_conform|tile_conform}], budget 0-3 (default 2), on_fail deliver|fail, palette [#hex]} — the job renders, checks run, failed checks trigger bounded mutated-prompt/seed retries, best attempt delivered with verdicts in the job record."; `request_asset_set` gains "per-asset validate (same shape) — children validate independently; set palette_lock coherence still applies after attempts."; `make_gif` gains "validate (from_spec: children; provided frames: evaluate-only at compose)."

- [ ] **Commit** `feat(server): critic env wiring — CRITIC_MODEL/TAGGER_URL plumbing + plist/install/MCP docs`.

---

### Task 14: skill + README + README openapi sync

- Create `skills/img-gen-validation/SKILL.md` (frontmatter: trigger on "image validation/QA, checklist, critic, anatomy/tag drift, palette conformance, tileability"):
  - body: when to attach `validate` at submit vs call `/api/v1/validate` standalone; the four ops with arg shapes; budget/on_fail guidance (default 2/deliver; 0 for report-only; "fail" when pipelines must not ship imperfect frames); verdict taxonomy incl. `undetermined` (judge refusal/malformed — never fail-open, never budget-consuming) and `error` (infra — inert); attempt/provenance reading (attempts[], chosen, critic_* tEXt keys, mutation reproduction via submit_job of the recorded attempt prompt); judge model + cold-start note (~20–40 s after idle under `OLLAMA_KEEP_ALIVE=1m`); the WD14 env prerequisite.
- Amend `skills/img-gen-assets/SKILL.md` (one paragraph: pairing `palette_conform` with `palette_lock` — target must be explicit; post-op `pixel_fix` makes containment exact) and `skills/img-gen-animation/SKILL.md` (one paragraph: from_spec frames + validate for state-set QA; provided-frame evaluate-only semantics).
- `README.md`: new "Validation" section (endpoint + field + envs + taxonomy table) + update the models/LoRA prose only where `validate` intersects.
- Deploy-script check: `deploy/install-skills.sh` copies `skills/*` — verify the new skill dir is picked up by the existing glob (no change expected; add a test assertion only if the script enumerates dirs explicitly).

- [ ] **Commit** `docs(skills): img-gen-validation skill + assets/animation cross-references + README validation section`.

---

### Task 15: live verification smoke (execution-time; requires Plan A deployed)

**Files:** none (scratch `DATA_DIR` + `:9111`).

- [ ] **Step 1: full suite first** — `go test ./... -count=1` green.
- [ ] **Step 2: scratch instance**

```bash
cd /Users/archcore/Projects/img-gen && go build -o bin/img-gen-phaseD-smoke ./cmd/img-gen
DATA_DIR=$(mktemp -d) bin/img-gen-phaseD-smoke &
# env inherited from the terminal; explicit overrides:
#   DATA_DIR=<temp> PORT... — use the port flag/env the server already supports (see main.go); bind 127.0.0.1
BASE=http://127.0.0.1:9111
curl -sf $BASE/api/v1/catalogs >/dev/null && echo catalog-ok
```
(If the server has no port env, run it with a small wrapper copying the launchd env but overriding `DATA_DIR`; never touch `com.img-gen` on `:8099`.)

- [ ] **Step 3: sync validate against the live tagger + judge** (requires the Plan A gateway upgrade LIVE on M6 and the tagger sidecar at 8904, with `TAGGER_URL` pointing there):
  - submit a cheap 512px generate with `validate:{checks:[{op:"tag_roundtrip"},{op:"vlm_check",arg:"the image shows a single subject"}], budget:1}` → observe `validating` in SSE → terminal job shows `validate.verdict` + `attempts` and, when the first attempt failed a check, an attempt-2 record whose prompt carries the mechanical mutation.
  - `POST /api/v1/validate` on the delivered PNG with `[{op:"tile_conform"}]` → verdict JSON.
  - record the two curl transcripts (they are the D4 showcase acceptance) in the task's summary.

**Rollback:** none needed (additive). If the gateway prereq is missing live, only the judge-dependent ops fail with the named-upgrade error — tag/sync checks still smoke-deployable.

---

## Self-review notes (already applied)

1. **Spec coverage:** D1 → LLM-router plan; D2 → Tasks 1–8 (+ Plan A sidecar); D3 → Tasks 9–13; D4 → Task 14 + Plan A Task 3 docs; thresholds → constants in Task 1; lifecycle/retry → Task 10; restart → Task 11; provenance → Tasks 7/10/12; sync endpoint → Task 8; env/plist/deploy → Tasks 13/LP-Task 2; non-goals untouched.
2. **Type consistency:** `storage.CriticCheck` shared by critic/queue/compose (defined once, Task 7 — critic imports storage); `attemptRenderer` is the single render seam (Tasks 10/11); `critic.Env`/`Options.Critic` wired once (Task 13); job JSON key for the summary is `validate` (matches the spec's example).
3. **Placeholders:** none — every code block is complete against the read source (queue.go, compose.go, lattice.go, v1.go, rembg contract).