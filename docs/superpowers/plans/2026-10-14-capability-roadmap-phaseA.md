# Phase A — Capability Roadmap Round 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Phase A of the roadmap spec: RIFE keyframes interpolation end-to-end, engine-consumable metadata sidecars, PNG tEXt param embedding, and persisted idempotency keys.

**Architecture:** Four independent feature tracks in the existing layered shape — `internal/compose` (pure image/post-process work), `internal/lattice` (sidecar HTTP clients), `internal/queue` (compose orchestration), `cmd/img-gen` (HTTP surface + persistence). Every new field is opt-in with defaults empty; existing callers see zero behavior change. The only removed code is the `not_implemented` keyframes branch, replaced by real validation + a `Rife` sidecar call.

**Tech Stack:** Go stdlib only (no new dependencies). Tests follow the existing per-package `_test.go` files. Sidecar calls re-use `lattice.Client`'s HTTP plumbing.

**Spec:** `[docs/superpowers/specs/2026-10-14-capability-roadmap-round2-design.md](docs/superpowers/specs/2026-10-14-capability-roadmap-round2-design.md)` — executors read both. Spec sections A1–A4 and "Phase A testing" are the authority; where this plan concretizes them further (output file names, supported-kind table for `meta_formats`), this plan is more specific and wins.

## Global Constraints

- Go stdlib only — no new module dependencies anywhere in this plan.
- Every task ends with `go vet ./... && go test ./...` green plus its own committed diff. Commit messages end with `Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>`.
- All new API fields are opt-in; defaults preserve existing behavior byte-for-byte (empty `meta_formats` → no new files; keyframes only reachable via `source.type: "keyframes"`).
- Honest failure: unset URLs / unreachable sidecars produce `validation_error` or failed parents with named reasons — never silent ignore, never panic.
- Never touch the live :8099 instance or the :9106 render instance; tests must bind random ports (httptest), never fixed ports.
- File names emitted into set directories must pass `storage.ValidImageID`-style naming already enforced by `SaveSetFile` — no new escaping rules invented here.
- Private env values (real sidecar IPs) never enter tracked files; docs use placeholders like `RIFE_URL=http://<rife-host>:<port>`.

### Concretized decisions this plan locks (spec A1/A2 refinements)

1. **Keyframes projection formula:** total frames = `(len(anchors)-1)*multiplier + 1`, capped at 64 (the existing gif frame cap). Spec's "≤ 64 interpolated frames" is enforced with this exact formula.
2. **`meta_formats` supported-kind table** (single-file formats):

   | format | file emitted | supported kinds |
   |---|---|---|
   | phaser | `<name>.phaser.json` | atlas, npc |
   | unity  | `<name>.frames.json` | atlas, npc |
   | sparrow | `<name>.sparrow.xml` | atlas, npc |
   | tiled  | `<name>.tsx` + `<name>-wangset.png` | kit |

   Rationale: phaser/unity/sparrow JSONs reference ONE texture image each (the existing `-sheet.png` / `-walk.png`); kit renders 20 separate class PNGs with no sheet, so only the tileset format (which ships its own assembled wangset sheet) fits it. The spec's Phase A testing already treats the table as indicative; a one-line spec-table amendment is part of Task 8's commit so spec and code agree.
3. **Keyframes anchor files** are saved into the gif set dir as `keyframe-%04d.png` (mirroring from_frames' `source-frame-%04d.png`, compose.go:986) so restart recovery re-reads them like every other gif source.

---

### Task 1: `compose.EmbedTextPNG` — tEXt chunk writer

**Files:**
- Create: `internal/compose/pngtext.go`
- Test: `internal/compose/pngtext_test.go`

**Interfaces:**
- Produces: `func EmbedTextPNG(p []byte, fields map[string]string) ([]byte, error)` and `func ScanTextChunks(p []byte) (map[string]string, error)` — pure functions, no I/O. Later tasks call these at delivery chokepoints and in delivery tests.

- [ ] **Step 1: Write the failing test**

```go
package compose

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
)

// scanText walks every PNG chunk of p and returns the tEXt key/value pairs
// (order preserved) after verifying each chunk's CRC.
func scanText(t *testing.T, p []byte) map[string]string {
	t.Helper()
	if bytes.Equal(p[12-4:12-4+4], []byte("IHDR")) == false {
		t.Fatalf("not a png chunk stream: % x", p[:8])
	}
	off := 8
	out := map[string]string{}
	for off < len(p) {
		l := int(binary.BigEndian.Uint32(p[off : off+4]))
		typ := string(p[off+4 : off+8])
		data := p[off+8 : off+8+l]
		sum := binary.BigEndian.Uint32(p[off+8+l : off+12+l])
		if crc32.ChecksumIEEE(p[off+4:off+8+l]) != sum {
			t.Fatalf("chunk %s crc mismatch", typ)
		}
		if typ == "tEXt" {
			i := bytes.IndexByte(data, 0)
			out[string(data[:i])] = string(data[i+1:])
		}
		off += 12 + l
		if typ == "IEND" {
			break
		}
	}
	return out
}

func TestEmbedTextPNG(t *testing.T) {
	src := renderTestPNG(t, 16, 16) // tiny png via png.Encode
	out, err := EmbedTextPNG(src, map[string]string{"software": "img-gen", "seed": "42"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(out) <= len(src) {
		t.Fatal("expected output larger than input")
	}
	// Output must still decode as a PNG (stdlib ignores tEXt).
	if _, err := pngDecode(out); err != nil {
		t.Fatalf("embedded png undecodable: %v", err)
	}
	got := scanText(t, out)
	if got["software"] != "img-gen" || got["seed"] != "42" {
		t.Fatalf("tEXt mismatch: %v", got)
	}
}

func TestEmbedTextPNGRejectsAndTruncates(t *testing.T) {
	if _, err := EmbedTextPNG([]byte("not a png"), map[string]string{"a": "b"}); err == nil {
		t.Fatal("want error for non-png input")
	}
	// IEND tamper: truncate past last chunk
	if _, err := EmbedTextPNG(renderTestPNG(t, 16, 16)[:8], nil); err == nil {
		t.Fatal("want error for missing IEND")
	}
	long := strings.Repeat("x", 5000)
	out, err := EmbedTextPNG(renderTestPNG(t, 16, 16), map[string]string{"parameters": long})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if got := scanText(t, out)["parameters"]; len(got) != 4096 {
		t.Fatalf("want value truncated to 4096, got %d", len(got))
	}
}

func TestScanTextChunksRejectsMangledCRC(t *testing.T) {
	src, err := EmbedTextPNG(renderTestPNG(t, 16, 16), map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	bad := append([]byte(nil), src...)
	// the embedded tEXt chunk's CRC is the 4 bytes just before IEND's
	// 12-byte trailer — flip one to prove verification is real (the spec's
	// "hand-mangled chunk is rejected" test)
	bad[len(bad)-16] ^= 0xFF
	if _, err := ScanTextChunks(bad); err == nil {
		t.Fatal("want CRC mismatch error")
	}
}
```

`renderTestPNG` / `pngDecode`: use whatever tiny helpers already exist in `internal/compose/compose_test.go`; if none exist, write them (5 lines — `png.Encode` of an `image.NewNRGBA` and the inverse using `image/png`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run EmbedTextPNG -v`
Expected: FAIL — `EmbedTextPNG` undefined.

- [ ] **Step 3: Write the implementation**

```go
package compose

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
)

// pngTextMax caps one tEXt value; keeps history.jsonl and PNG params alike
// in size. PNG allows far more, this keeps embeds practical.
const pngTextMax = 4096

// EmbedTextPNG splices ASCII tEXt chunks (one per key, sorted by key for
// determinism) before the IEND chunk of a PNG. Returns the input unchanged
// when fields is empty. Errors on non-PNG input or a missing IEND trailer;
// it never "fixes" anything else about the stream.
func EmbedTextPNG(p []byte, fields map[string]string) ([]byte, error) {
	if len(fields) == 0 {
		return p, nil
	}
	if len(p) < 45 || !bytes.HasPrefix(p, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return nil, errors.New("pngtext: not a png")
	}
	const iendLen = 12
	tail := p[len(p)-iendLen:]
	if binary.BigEndian.Uint32(tail[0:4]) != 0 || string(tail[4:8]) != "IEND" {
		return nil, errors.New("pngtext: missing IEND trailer")
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "" || len(k) > 79 || bytes.ContainsAny([]byte(k), "\x00 ") {
			return nil, errors.New("pngtext: invalid keyword")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ins := make([]byte, 0, 64)
	for _, k := range keys {
		v := fields[k]
		if len(v) > pngTextMax {
			v = v[:pngTextMax]
		}
		ins = appendChunk(ins, "tEXt", append(append([]byte(k), 0), v...))
	}
	out := make([]byte, 0, len(p)+len(ins))
	out = append(out, p[:len(p)-iendLen]...)
	out = append(out, ins...)
	return append(out, tail...), nil
}

func appendChunk(dst []byte, typ string, data []byte) []byte {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(data)))
	copy(hdr[4:8], typ)
	dst = append(dst, hdr[:]...)
	dst = append(dst, data...)
	binary.BigEndian.PutUint32(hdr[0:4], crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return append(dst, hdr[0:4]...)
}

// ScanTextChunks walks every PNG chunk, verifies each CRC, and returns the
// tEXt key/value pairs. Errors on non-PNG input, truncated chunk framing, or
// a CRC mismatch — it is the reader half of EmbedTextPNG and the delivery
// path tests' verifier.
func ScanTextChunks(p []byte) (map[string]string, error) {
	if len(p) < 8 || !bytes.HasPrefix(p, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return nil, errors.New("pngtext: not a png")
	}
	out := map[string]string{}
	off := 8
	for off < len(p) {
		if off+12 > len(p) {
			return nil, errors.New("pngtext: truncated chunk header")
		}
		l := int(binary.BigEndian.Uint32(p[off : off+4]))
		typ := string(p[off+4 : off+8])
		if off+12+l > len(p) {
			return nil, errors.New("pngtext: truncated chunk body")
		}
		data := p[off+8 : off+8+l]
		if crc32.ChecksumIEEE(p[off+4:off+8+l]) != binary.BigEndian.Uint32(p[off+8+l:off+12+l]) {
			return nil, fmt.Errorf("pngtext: chunk %s crc mismatch", typ)
		}
		if typ == "tEXt" {
			i := bytes.IndexByte(data, 0)
			if i < 0 {
				return nil, errors.New("pngtext: tEXt missing keyword separator")
			}
			out[string(data[:i])] = string(data[i+1:])
		}
		off += 12 + l
		if typ == "IEND" {
			break
		}
	}
	return out, nil
}
```

(Note for the implementer: `appendChunk` mutates `hdr` bytes reused after CRC write — that is intentional and correct because `hdr[0:4]` is re-encoded as the CRC of type+data per the chunk spec.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compose/ -run 'EmbedText|ScanText' -v`
Expected: PASS. Then `go vet ./... && go test ./...` green.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/pngtext.go internal/compose/pngtext_test.go
git commit -m "feat(compose): EmbedTextPNG — stdlib tEXt chunk splicing"
```

---

### Task 2: Embed render params at delivery chokepoints

**Files:**
- Modify: `internal/queue/queue.go` (both `m.opts.Store.SaveImage` call sites, current lines 322 and 712)
- Modify: `internal/queue/compose.go` (composeAssetSet save loop, the `for ai := range outsByAsset` block before `SaveSetFile`, current line ~763)
- Test: `internal/queue/compose_test.go` (extend an existing asset-set test or add one)

**Interfaces:**
- Consumes: `compose.EmbedTextPNG(p []byte, fields map[string]string) ([]byte, error)` from Task 1.
- Produces: `func (m *Manager) withRenderParams(png []byte, kv map[string]string) []byte` — best-effort embed used by compose paths; job paths embed inline the same way.

- [ ] **Step 1: Write the failing test** (in `internal/queue/compose_test.go`, using the file's existing fake-Ops manager harness — the constructor used by the existing asset-set tests)

```go
func TestComposedOutputsCarryTextParams(t *testing.T) {
	m, ids := submitDoneAssetSet(t, AssetSetRequest{
		Name: "txt-elem", PaletteLock: false,
		Seed: int64p(7),
		Assets: []AssetSpecIn{{Kind: "element", Name: "b", Prompt: "a button", Size: "16x16"}},
	})
	raw := readStoredSetFileBytes(t, m, ids[0], "b.png")
	// ScanTextChunks comes from Task 1 (compose package).
	got, err := compose.ScanTextChunks(raw)
	if err != nil { t.Fatalf("scan: %v", err) }
	if got["software"] != "img-gen" || got["kind"] != "element" || got["name"] != "b" || got["set"] != ids[0] {
		t.Fatalf("unexpected params: %v", got)
	}
}
```

The `submitDoneAssetSet`/`readStoredSetFileBytes` helpers: follow the existing patterns in this test file for running a set to completion with fake renders and reading set files via `m.opts.Store.ReadSetFile` (the same way current tests assert deliverables). If the exact helper names differ, adapt names — the assertions above are the contract.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run ComposedOutputs -v`
Expected: FAIL — composed `b.png` carries no `kind`/`name`/`set` tEXt chunks yet (no `software` either).

- [ ] **Step 3: Implement the embed hooks**

In `internal/queue/queue.go`, add:

```go
// withRenderParams best-effort embeds generation params as tEXt chunks.
// Embed failures never fail a render — the PNG is delivered unmodified.
func (m *Manager) withRenderParams(pngBytes []byte, kv map[string]string) []byte {
	if kv == nil {
		kv = map[string]string{}
	}
	kv["software"] = "img-gen"
	out, err := compose.EmbedTextPNG(pngBytes, kv)
	if err != nil {
		m.log("params", "tEXt embed skipped: %v", err)
		return pngBytes
	}
	return out
}
```

(`m.log` — match whatever the Manager's logging method is named in queue.go; the existing logf pattern. If `m.log` has an id parameter, pass `"job"` context where available.)

At each `SaveImage` call site (queue.go:322 and queue.go:712), wrap the png with the params available in that scope — prompt, negative (if present), model, size, seed, mode, loras (if present), routing target, and the job id (the spec's A3 key list: prompt, negative, seed, model, size, loras, routing target; compose assets additionally get kind/name/set from the compose-side hook):

```go
kv := map[string]string{
	"prompt": job.Prompt, "model": job.Model, "size": job.Size,
	"seed": strconv.FormatInt(seed, 10), "mode": job.Mode, "job": id,
}
if job.Negative != "" {
	kv["negative"] = job.Negative
}
if len(job.Loras) > 0 {
	if b, jerr := json.Marshal(job.Loras); jerr == nil {
		kv["loras"] = string(b)
	}
}
if tgt := routingTargetOf(job); tgt != "" {
	kv["routing"] = tgt
}
png = m.withRenderParams(png, kv)
```

`routingTargetOf` is NOT a new helper — it stands for whichever in-scope variable at that site records what ran the render (the lattice catalog key, or `direct`); if no such value is in scope, omit the `routing` key rather than invent one. Adapt exact field/variable names to the locals at each site (`seed` may be a pointer — dereference as done nearby; the loras field is the slice the render already consumed — serialize once, compactly, with `encoding/json`). Add `strconv`/`encoding/json`/`img-gen/internal/compose` imports if missing.

In `internal/queue/compose.go` composeAssetSet, directly before the save loop (`for ai := range outsByAsset`), embed compose-level params into every PNG output (children carry their own render params on the child jobs; the composed deliverables get set identity):

```go
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
```

`composeGIF` outputs are GIF/WebP — no PNG path, skip it.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/queue/ ./internal/compose/ -v`
Expected: PASS (new test + no regressions). Then `go vet ./... && go test ./...` green.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/queue.go internal/queue/compose.go internal/queue/compose_test.go internal/compose/pngtext.go
git commit -m "feat(queue): embed generation params as PNG tEXt at delivery"
```

---

### Task 3: `lattice.Client.Rife` + `RIFE_URL` wiring

**Files:**
- Modify: `internal/lattice/lattice.go` (Client struct near line 22; new method after `Upscale`, line ~232)
- Modify: `cmd/img-gen/main.go` (cfg struct ~line 35-36, cfg builder ~line 55, `lat.*` assignments ~line 90, `queue.ImageOps` literal at line 104)
- Modify: `deploy/com.img-gen.plist.in`, `deploy/install-macos.sh` (add `RIFE_URL` beside the existing `UPSCALE_URL` wiring)
- Test: `internal/lattice/lattice_test.go` (extend)

**Interfaces:**
- Consumes: the `postImage`-style POST pattern used by `Upscale` (lattice.go:232).
- Produces: `func (c *Client) Rife(ctx context.Context, anchors [][]byte, multiplier int) ([][]byte, error)` and `queue.ImageOps.Rife func(ctx context.Context, anchors [][]byte, multiplier int) ([][]byte, error)` — Task 5 consumes both.

- [ ] **Step 1: Write the failing test** (style of the existing lattice_test.go tests)

```go
func TestRife(t *testing.T) {
	anchor := renderTinyPNG(t) // helper pattern from existing tests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/interpolate" {
			t.Fatalf("path %s", r.URL.Path)
		}
		var req struct {
			Images     []string `json:"images"`
			Multiplier int      `json:"multiplier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("body: %v", err)
		}
		if len(req.Images) != 2 || req.Multiplier != 3 {
			t.Fatalf("req mismatch: %d imgs, mult %d", len(req.Images), req.Multiplier)
		}
		// echo back 5 frames (2 anchors + 3 in-betweens) as png b64
		resp := map[string]any{"frames": []string{b64(anchor), b64(anchor), b64(anchor), b64(anchor), b64(anchor)}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	c := New(""); c.RifeURL = srv.URL; c.HTTP = srv.Client()
	frames, err := c.Rife(context.Background(), [][]byte{anchor, anchor}, 3)
	if err != nil { t.Fatalf("rife: %v", err) }
	if len(frames) != 5 { t.Fatalf("want 5 frames, got %d", len(frames)) }
}

func TestRifeSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(""); c.RifeURL = srv.URL; c.HTTP = srv.Client()
	if _, err := c.Rife(context.Background(), [][]byte{[]byte("x")}, 3); err == nil {
		t.Fatal("want error on 503")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lattice/ -run Rife -v`
Expected: FAIL — `Rife` undefined / `RifeURL` unknown field.

- [ ] **Step 3: Implement**

In `lattice.Client` struct (next to `UpscaleURL`), add:

```go
RifeURL     string // optional Practical-RIFE sidecar for POST /interpolate keyframes interpolation
```

Method (after `Upscale`):

```go
// Rife interpolates between anchor frames on the RIFE sidecar. anchors are
// raw PNG bytes; the returned slice holds one raw PNG per interpolated
// frame, count (n-1)*multiplier+1, anchors included in order.
func (c *Client) Rife(ctx context.Context, anchors [][]byte, multiplier int) ([][]byte, error) {
	imgs := make([]string, len(anchors))
	for i, a := range anchors {
		imgs[i] = base64.StdEncoding.EncodeToString(a)
	}
	body, err := json.Marshal(map[string]any{"images": imgs, "multiplier": multiplier})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.RifeURL, "/")+"/interpolate",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rife sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rife sidecar: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Frames []string `json:"frames"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("rife sidecar body: %w", err)
	}
	frames := make([][]byte, 0, len(out.Frames))
	for i, f := range out.Frames {
		raw, err := base64.StdEncoding.DecodeString(f)
		if err != nil {
			return nil, fmt.Errorf("rife frame %d: %w", i, err)
		}
		frames = append(frames, raw)
	}
	return frames, nil
}
```

(The limit of 64 MiB matches the response ceiling for ≤64 small-to-medium sprite PNGs; mirror whatever cap `postImage` already uses if it has one.)

In `cmd/img-gen/main.go`:
1. cfg struct (beside `UpscaleURL string` at line ~35): `RifeURL string`
2. cfg builder (beside line 55): `RifeURL: envOr("RIFE_URL", ""),`
3. `lat.RifeURL = cfg.RifeURL` (beside line ~90)
4. `queue.ImageOps` literal (line 104): `Rife` is wired CONDITIONALLY so `Ops.Rife == nil` unambiguously means "sidecar not configured" — submit-time validation (Task 4) leans on that to give the spec's honest error:

```go
var rifeOp func(ctx context.Context, anchors [][]byte, multiplier int) ([][]byte, error)
if cfg.RifeURL != "" {
	rifeOp = lat.Rife
}
// ... in the Ops literal: Rife: rifeOp,
```

In `deploy/com.img-gen.plist.in` and `deploy/install-macos.sh`: add `RIFE_URL` exactly where `UPSCALE_URL` appears (plist env dict key + the script's launchctl/plist generation), with the same empty-default handling.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/lattice/ -v && go vet ./...` green.
Sanity: `grep -rn "RIFE_URL" deploy/` shows both files wired; `grep -n "RifeURL" cmd/img-gen/main.go` shows env, lat, and conditional Ops wiring all present.

- [ ] **Step 5: Commit**

```bash
git add internal/lattice/lattice.go internal/lattice/lattice_test.go cmd/img-gen/main.go deploy/com.img-gen.plist.in deploy/install-macos.sh
git commit -m "feat(lattice): RIFE sidecar client + RIFE_URL env wiring"
```

---

### Task 4: Keyframes submit path (validation + anchor persistence)

**Files:**
- Modify: `internal/queue/compose.go` — `GifSource` (~line 855), new types near it, `gifPayload` (~line 59), `SubmitGIF` keyframes case (the `not_implemented` branch at line ~973)
- Test: `internal/queue/compose_test.go` (extend)

**Interfaces:**
- Consumes: `FrameRef` (compose.go:880), `resolveFrameRef` (890), the from_frames submit tail (what SubmitGIF does after saving `source-frame-%04d.png` files — mirror it exactly).
- Produces (Task 5 consumes): `type keyframesPayload struct { Files []string; Multiplier int }` on `gifPayload.Keyframes`; anchors stored as `keyframe-%04d.png` set files.

- [ ] **Step 1: Write the failing tests**

```go
func TestSubmitGIFKeyframesValidation(t *testing.T) {
	// FrameRef B64 sources need no setup — decoding happens inside
	// resolveFrameRef at submit. Two harness variants prove both error
	// families: a manager WITHOUT Ops.Rife (unconfigured sidecar) and the
	// harness's usual fake-ops manager WITH a stub Rife wired (never called
	// during submit-path validation).
	bad := func(m *Manager, req GifRequest, want string) {
		_, err := m.SubmitGIF(req)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q err, got %v", want, err)
		}
	}
	anchor := func(b64 string) FrameRef { return FrameRef{B64: b64} }
	a := anchor(tinyPNGB64(t)); b := anchor(tinyPNGB64(t))

	bad(harnessWithoutRife(t),
		GifRequest{Name: "k0", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: []FrameRef{a, b}}}},
		"requires the RIFE sidecar (RIFE_URL)")

	mgr := harnessWithFakeRife(t)
	bad(mgr, GifRequest{Name: "k1", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: []FrameRef{a}}}},
		"anchors takes 2..16")
	bad(GifRequest{Name: "k2", Source: GifSource{Type: "keyframes"}},
		"keyframes source requires anchors")
	bad(GifRequest{Name: "k3", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: []FrameRef{a, b}, Multiplier: 11}}},
		"multiplier must be 2..10")
	for i := 0; i < 33; i++ { extra := tinyPNGB64; ... } // build 17 anchors -> "anchors takes 2..16"

	// 16 anchors x multiplier 5 => (16-1)*5+1 = 76 > 64
	anchors := make([]FrameRef, 16)
	for i := range anchors { anchors[i] = anchor(tinyPNGB64(t)) }
	bad(GifRequest{Name: "k4", Source: GifSource{Type: "keyframes", Keyframes: &KeyframesSource{Anchors: anchors, Multiplier: 5}}},
		"would produce 76 frames, cap is 64")
	// The same anchors with unset multiplier (default 3 => 45*1? (16-1)*3+1=46) must PASS validation
}

func TestSubmitGIFKeyframesStoresAnchors(t *testing.T) {
	// with the harness's fake Rife wired (the parent composeGIF calls it at
	// execution): submit with 2 b64 anchors, multiplier unset; run to done;
	// rebuild the spec from the store the way existing compose restart tests
	// do, assert spec.GIF.Keyframes.Files ==
	// ["keyframe-0000.png","keyframe-0001.png"] and Multiplier == 3.
}
```

Note: validation runs before any sidecar work — anchors resolve via `resolveFrameRef` exactly once at submit (decoding bad PNGs fails the submit, per the existing resolveFrameRef doc). When writing these tests, follow how existing from_frames tests manufacture anchors (B64 refs are simplest — no setup needed). `harnessWithoutRife`/`harnessWithFakeRife`: variants of the file's existing fake-ops constructor — the plain constructor already has `Ops.Rife == nil` (it was never wired); `harnessWithFakeRife` adds a stub returning two decoded-anchor PNGs.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run Keyframes -v`
Expected: FAIL — `KeyframesSource` undefined; the current code path returns `not_implemented`.

- [ ] **Step 3: Implement**

Types beside `GifSource` (compose.go ~855):

```go
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
```

`gifPayload` gains one field after `Composite`:

```go
Keyframes *keyframesPayload `json:"keyframes,omitempty"`
```

Replace the `not_implemented` branch in `SubmitGIF` (compose.go ~968-974) with:

```go
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
		return "", &ValidationError{Code: "validation_error", Msg: fmt.Sprintf(
			"keyframes would produce %d frames, cap is 64 — use multiplier ≤ %d or fewer anchors", total, 64/(n-1)+1)}
	}
```

Then, immediately after the shared validation, in the same switch arm resolve + persist the anchors by copying the from_frames block (compose.go ~976-1033, the arm starting `case "from_frames":`): `id := newID()`, loop `resolveFrameRef`, save each with `fname := fmt.Sprintf("keyframe-%04d.png", i)` via `m.opts.Store.SaveSetFile`; on any resolve/save failure run the same cleanup (`m.opts.Store.RemoveSet(id)`) and `ValidationError`; after the loop set

```go
spec.GIF.Keyframes = &keyframesPayload{Files: names, Multiplier: mult}
```

with `spec` carrying `DelayMS`/`Loop`/`Formats` exactly as from_frames does, and finish the arm with whatever enqueue/return the from_frames arm ends with (same child-free submission path — zero render children).

Delete the `not_implemented` code comment block above `GifRequest` at compose.go ~853-856 (update the mode list comment: `from_frames | from_spec | composite | keyframes` all real now).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/queue/ -run Keyframes -v` PASS, then `go vet ./... && go test ./...` green.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/compose.go internal/queue/compose_test.go
git commit -m "feat(queue): keyframes gif source submit path (validation + anchor persistence)"
```

---

### Task 5: Keyframes executor — composeGIF branch with the Rife call

**Files:**
- Modify: `internal/queue/compose.go` — `composeGIF` frames switch (compose.go ~1100, the `switch { case len(gp.Frames) > 0: ... }`)
- Test: `internal/queue/compose_test.go` (extend)

**Interfaces:**
- Consumes: Task 3's `Ops.Rife` and Task 4's `keyframesPayload`.

- [ ] **Step 1: Write the failing test**

Using the existing compose_test.go fake-Ops harness (the same one asset-set and from_frames tests use), add a `Rife` fake to the harness's `queue.ImageOps` literal if the harness centralizes it (it does — the same literal that supplies fake `Generate`):

```go
func TestComposeGIFKeyframesFakeSidecar(t *testing.T) {
	// GOLDEN PATH — the spec's testing section requires the full
	// keyframes→gif pipeline to run against an HTTP fake sidecar, so wire
	// Ops.Rife through lattice.Client rather than a hand-rolled Go func:
	//
	//   srv := httptest.NewServer(handler echoing (n-1)*mult+1 distinct
	//     16x16 NRGBA PNGs per the Task 3 contract {frames: [b64...]})
	//   lat := lattice.New(""); lat.RifeURL = srv.URL; lat.HTTP = srv.Client()
	//   mgr := the existing fake-ops harness but Ops.Rife: lat.Rife
	//
	// Submit a keyframes gif with 2 B64 anchors, multiplier 2 (=> 3 frames),
	// run to done with the harness's existing completion helper, then:
	//   - decode the produced <name>.gif with gif.Decoder: expect 3 frames;
	//   - expect set files keyframe-0000/0001.png present;
	//   - expect the webp deliverable or its status note per the existing
	//     from_frames tests' conventions.
}

func TestComposeGIFKeyframesSidecarFailure(t *testing.T) {
	// Ops.Rife (plain Go fake this time) returns an error: the parent must
	// surface the failure honestly with a "rife:" cause in the error text and
	// save no partial gif. Match the status/semantics of the nearest sidecar
	// precedent — the Ops.Upscale failure handling near queue.go:752 (if that
	// path classifies retryable sidecar errors — sidecar_unavailable/busy/
	// timeout — mirror the classification; otherwise the compose_error finish
	// applies). Write the assertion after reading that precedent; the
	// invariants are: error text carries the rife cause, no deliverables
	// saved, manager not left mid-flight.
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run ComposeGIFKeyframes -v`
Expected: FAIL — parent never produces frames ("neither frames nor children" path), since composeGIF has no keyframes branch.

- [ ] **Step 3: Implement**

Add the case as the FIRST clause of composeGIF's frames switch (before `case len(gp.Frames) > 0:`), since it uses `gp.Keyframes` not `gp.Frames`:

```go
case gp.Keyframes != nil:
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
```

The switch's own default failure clause ("neither frames nor children") stays; update the comment above `GifRequest`'s `Source` field if it still mentions phase-3 deferral. `context.Background()` here matches how compose executors run no other network calls — sidecar cancellation mid-compose is not modeled today (document that fact in the RIFE sidecar docs in Task 10). Error classification: mirror what the `Ops.Upscale` call does with sidecar failures (queue.go ~745-760) — if retryable sidecar errors get a distinct treatment there, apply it to the Rife error too; the `compose_error` finish above covers the flat-failure case only.

Note: `compose.BlitImage` composite statics (`gp.Composite`) run AFTER any frames branch in the existing code — keyframes gifs get composite support for free; keep that ordering untouched.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/queue/ -v` full package green; then `go vet ./... && go test ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/compose.go internal/queue/compose_test.go
git commit -m "feat(queue): keyframes compose executor — Rife call into gif pipeline"
```

---

### Task 6: Meta emitters — atlas/npc (phaser, unity, sparrow)

**Files:**
- Create: `internal/compose/meta.go`
- Test: `internal/compose/meta_test.go`

**Interfaces:**
- Consumes: `Rect` JSON shape `{"x","y","w","h"}` (atlas.go:14), the `-atlas.json` deliverable convention (`ComposeAtlas` compose.go:74-77, npc walk4 npc.go:50-52), sheet file naming (`-sheet.png`, `-walk.png`).
- Produces: `func EmitMeta(kind, name string, formats []string, outs []OutputFile) ([]OutputFile, error)` (Task 8 consumes). `EmitMeta` NEVER mutates `outs`; it appends new files.

- [ ] **Step 1: Write the failing tests**

```go
func atlasOuts(t *testing.T, name string, w, h int, frames int) []OutputFile {
	// build outs the way ComposeAtlas does: encode `frames` distinct 16x16
	// NRGBA pngs, call ComposeAtlas(name, pngs, 0). Returns the 2-file outs.
}

func TestEmitMetaAtlasPhaser(t *testing.T) {
	outs := atlasOuts(t, "chips", 16, 16, 4)
	meta, err := EmitMeta("atlas", "chips", []string{"phaser"}, outs)
	if err != nil { t.Fatalf("emit: %v", err) }
	if len(meta) != 1 || meta[0].Name != "chips.phaser.json" {
		t.Fatalf("files: %+v", meta)
	}
	var doc struct {
		Frames map[string]struct {
			Frame            Rect   `json:"frame"`
			Rotated          bool   `json:"rotated"`
			Trimmed          bool   `json:"trimmed"`
			SpriteSourceSize Rect   `json:"spriteSourceSize"`
			SourceSize       [2]int `json:"sourceSize"`
		} `json:"frames"`
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(meta[0].Data, &doc); err != nil { t.Fatal(err) }
	f := doc.Frames["chips-0000"]
	if f.Frame != (Rect{X: 0, Y: 0, W: 16, H: 16}) || f.Rotated || f.Trimmed {
		t.Fatalf("phaser frame: %+v", f)
	}
	if doc.Meta["image"] != "chips-sheet.png" {
		t.Fatalf("meta image: %v", doc.Meta)
	}
}

func TestEmitMetaAtlasUnityAndSparrow(t *testing.T) {
	outs := atlasOuts(t, "chips", 16, 16, 4)
	meta, err := EmitMeta("atlas", "chips", []string{"unity", "sparrow"}, outs)
	if err != nil { t.Fatalf("emit: %v", err) }
	// unity: {"sprites":[{"name":...,"rect":{"x","y","width","height"},"pivot":{"x":0.5,"y":0.5}}]}
	var unity struct {
		Sprites []struct {
			Name  string   `json:"name"`
			Rect  RectWH   `json:"rect"`
			Pivot [2]float64 `json:"pivot"`
		} `json:"sprites"`
	}
	// ... unmarshal "chips.frames.json", assert len(Sprites)==4, names
	// chips-0000..0003, pivot {0.5,0.5}, Rect fields x/y/width/height ints.
	// sparrow: parse "chips.sparrow.xml" with encoding/xml:
	var atlasXML struct {
		XMLName     xml.Name `xml:"TextureAtlas"`
		ImagePath   string   `xml:"imagePath,attr"`
		SubTextures []struct {
			Name string `xml:"name,attr"`
			X    int    `xml:"x,attr"`
			Y    int    `xml:"y,attr"`
			W    int    `xml:"width,attr"`
			H    int    `xml:"height,attr"`
		} `xml:"SubTexture"`
	}
	// assert ImagePath == "chips-sheet.png", 4 SubTextures in y-then-x order.
}

func TestEmitMetaNPCUsesWalkSheet(t *testing.T) {
	// out := composeNPCWalk4Outs(t, "runner") — encode three 16x16 pngs,
	// call ComposeNPCWalk4("runner", down, up, right). EmitMeta("npc",
	// "runner", []string{"phaser"}, outs) -> meta image "runner-walk.png".
}

func TestEmitMetaFrameRectsReadFromAtlasJSON(t *testing.T) {
	// 2-frame atlas with non-square columns=1: frames land at y=0 and y=16
	// (PackGrid rows) — assert the emitted rects reflect atlas.json, not a
	// recomputed grid (EmitMeta is forbidden from re-running PackGrid).
}

func TestEmitMetaRejectsAndIsStable(t *testing.T) {
	// - unknown format "phaza" error; - kind "element" error;
	// - empty formats returns nil outs, nil err;
	// - atlas outs with no -atlas.json file -> error "atlas metadata missing".
}

func TestEmitMetaGoldenByteExact(t *testing.T) {
	// fixed 4-frame atlas (16x16 frames, columns 2) × {phaser, unity,
	// sparrow} and one npc × {phaser}: output bytes must equal
	// testdata/golden/<file> byte-for-byte (Go's encoding/json sorts map
	// keys, emitters build XML in fixed order — both deterministic). Regen
	// convention: `go test ./internal/compose/ -run Golden -update` writes
	// the goldens via the standard CLI flag defined in the test file.
}
```

Type `RectWH` in the test is `{X int `json:"x"`; Y int `json:"y"`; Width int `json:"width"`; Height int `json:"height"`}` (test-local, distinct from compose.Rect which uses w/h).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run EmitMeta -v`
Expected: FAIL — `EmitMeta` undefined.

- [ ] **Step 3: Implement `meta.go`**

```go
package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MetaFormats is the closed set of engine-metadata formats EmitMeta emits.
// Each references exactly one texture image, which is why kit (20 separate
// class files, no sheet) supports tiled only — see the spec's Phase A table.
var MetaFormats = map[string][]string{
	"phaser":  {"atlas", "npc"},
	"unity":   {"atlas", "npc"},
	"sparrow": {"atlas", "npc"},
	"tiled":   {"kit"},
}

type atlasJSONDoc struct {
	Frames map[string]struct {
		Frame Rect `json:"frame"`
	} `json:"frames"`
	Meta map[string]string `json:"meta"`
}

// EmitMeta appends engine-consumable metadata files for an already-composed
// asset. It never mutates outs and never re-derives geometry: rects are read
// from the asset's own -atlas.json deliverable (atlas/npc) or assembled from
// the class files (kit, see emitKitTiled). formats must be a subset of
// MetaFormats entries valid for kind.
func EmitMeta(kind, name string, formats []string, outs []OutputFile) ([]OutputFile, error) {
	if len(formats) == 0 {
		return nil, nil
	}
	for _, f := range formats {
		kinds, ok := MetaFormats[f]
		if !ok {
			return nil, fmt.Errorf("meta format %q unknown (valid: phaser, unity, sparrow, tiled)", f)
		}
		supported := false
		for _, k := range kinds {
			if k == kind {
				supported = true
				break
			}
		}
		if !supported {
			return nil, fmt.Errorf("meta format %q does not apply to kind %q", f, kind)
		}
	}
	switch kind {
	case "atlas", "npc":
		return emitStripMeta(kind, name, formats, outs)
	case "kit":
		return emitKitMeta(name, formats, outs)
	}
	return nil, fmt.Errorf("meta formats apply to atlas, npc, kit — not %q", kind)
}

func findFile(outs []OutputFile, suffix string) (OutputFile, bool) {
	for _, of := range outs {
		if strings.HasSuffix(of.Name, suffix) {
			return of, true
		}
	}
	return OutputFile{}, false
}

// emitStripMeta reads <name>-atlas.json + locates the sheet (-sheet.png for
// atlas, -walk.png for npc) and emits one file per requested format.
func emitStripMeta(kind, name string, formats []string, outs []OutputFile) ([]OutputFile, error) {
	aj, ok := findFile(outs, "-atlas.json")
	if !ok {
		return nil, fmt.Errorf("atlas metadata missing for %q", name)
	}
	var doc atlasJSONDoc
	if err := json.Unmarshal(aj.Data, &doc); err != nil {
		return nil, fmt.Errorf("atlas metadata %s: %w", aj.Name, err)
	}
	sheetSuffix := "-sheet.png"
	if kind == "npc" {
		sheetSuffix = "-walk.png"
	}
	sheet, ok := findFile(outs, sheetSuffix)
	if !ok {
		return nil, fmt.Errorf("sheet file %s missing for %q", sheetSuffix, name)
	}
	sheetName := sheet.Name
	// Frames in stable y-then-x order (map iteration is unordered).
	type ent struct {
		name string
		r    Rect
	}
	list := make([]ent, 0, len(doc.Frames))
	for n, fr := range doc.Frames {
		list = append(list, ent{n, fr.Frame})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].r.Y != list[j].r.Y {
			return list[i].r.Y < list[j].r.Y
		}
		return list[i].r.X < list[j].r.X
	})
	var out []OutputFile
	for _, f := range formats {
		switch f {
		case "phaser":
			b, err := json.Marshal(buildPhaser(doc.Meta, sheetName, list))
			if err != nil {
				return nil, err
			}
			out = append(out, OutputFile{Name: name + ".phaser.json", Data: b})
		case "unity":
			b, err := json.Marshal(buildUnity(sheetName, list))
			if err != nil {
				return nil, err
			}
			out = append(out, OutputFile{Name: name + ".frames.json", Data: b})
		case "sparrow":
			b, err := buildSparrow(sheetName, list)
			if err != nil {
				return nil, err
			}
			out = append(out, OutputFile{Name: name + ".sparrow.xml", Data: b})
		}
	}
	return out, nil
}

func buildPhaser(meta map[string]string, sheet string, list ...) map[string]any {
	frames := map[string]any{}
	for _, e := range list {
		frames[e.name] = map[string]any{
			"frame":            e.r,
			"rotated":          false,
			"trimmed":          false,
			"spriteSourceSize": e.r,
			"sourceSize":       map[string]int{"w": e.r.W, "h": e.r.H},
		}
	}
	app := "img-gen compose"
	if meta != nil && meta["app"] != "" {
		app = meta["app"]
	}
	return map[string]any{"frames": frames, "meta": map[string]string{"app": app, "image": sheet}}
}

func buildUnity(sheet string, list ...) map[string]any {
	sprites := make([]map[string]any, 0, len(list))
	for _, e := range list {
		sprites = append(sprites, map[string]any{
			"name": e.name,
			"rect": map[string]int{"x": e.r.X, "y": e.r.Y, "width": e.r.W, "height": e.r.H},
			"pivot": map[string]float64{"x": 0.5, "y": 0.5},
		})
	}
	return map[string]any{"sprites": sprites}
}

func buildSparrow(sheet string, list ...) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&buf, `<TextureAtlas imagePath=%q>` + "\n", sheet)
	for _, e := range list {
		fmt.Fprintf(&buf, "  <SubTexture name=%q x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\"/>\n",
			e.name, e.r.X, e.r.Y, e.r.W, e.r.H)
	}
	buf.WriteString("</TextureAtlas>\n")
	return buf.Bytes(), nil
}
```

(The `list ...` placeholders above stand for the concrete `ent` slice param — in real code, pass `[]ent` explicitly; the struct `ent` must be defined at package level or passed as `[]struct{name string; r Rect}` — pick one and make signatures consistent: `buildPhaser(meta map[string]string, sheet string, ents []ent)` etc. Rename `ent` to `metaEntry` at package scope.)

- [ ] **Step 4: Run tests**

Run: `go test ./internal/compose/ -run EmitMeta -v` green, then full package + `go vet ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/meta.go internal/compose/meta_test.go
git commit -m "feat(compose): phaser/unity/sparrow meta emitters for atlas+npc"
```

---

### Task 7: Kit tiled emitter — tsx + wangset sheet

**Files:**
- Modify: `internal/compose/meta.go` (add `emitKitMeta`)
- Modify: `internal/compose/meta_test.go`
- Consumes: `KitClasses` (wang.go:13) with fixed 20-key order, `PackGrid` (atlas.go:31).

- [ ] **Step 1: Write the failing test**

```go
func TestEmitMetaKitTiled(t *testing.T) {
	images := make([][]byte, len(KitClasses)) // encode a 16x16 flat png per class
	outs, err := ComposeKit("circuit", images)
	if err != nil { t.Fatalf("kit: %v", err) }
	meta, err := EmitMeta("kit", "circuit", []string{"tiled"}, outs)
	if err != nil { t.Fatalf("emit: %v", err) }
	if len(meta) != 2 {
		t.Fatalf("want tsx + wangset png, got %d files", len(meta))
	}
	var pngName, tsxName string
	for _, of := range meta {
		switch {
		case strings.HasSuffix(of.Name, ".tsx"): tsxName = of.Name
		case strings.HasSuffix(of.Name, ".png"): pngName = of.Name
		}
	}
	if tsxName != "circuit.tsx" || pngName != "circuit-wangset.png" {
		t.Fatalf("names: %s %s", tsxName, pngName)
	}
	// The wangset png must be a real png of 4 cols x 5 rows of 16px = 64x80.
	im, err := png.Decode(bytes.NewReader(findData(meta, pngName)))
	if err != nil { t.Fatal(err) }
	if im.Bounds().Dx() != 64 || im.Bounds().Dy() != 80 {
		t.Fatalf("wangset png %v", im.Bounds())
	}
	// tsx parses: tilecount 20, columns 4, image source + dims, and one
	// <wangtile> per class whose tileid ordering matches KitClasses[0..19]
	// laid out row-major into the 4-col sheet.
	doc, err := tsxParse(findData(meta, tsxName)) // small encoding/xml helper in the test
	if err != nil { t.Fatal(err) }
	if doc.TileCount != 20 || doc.Columns != 4 { t.Fatalf("tiles: %+v", doc) }
	if doc.Image.Source != "circuit-wangset.png" || doc.Image.W != 64 || doc.Image.H != 80 {
		t.Fatalf("image: %+v", doc.Image)
	}
	if len(doc.Wangset.WangTiles) != 20 { t.Fatalf("wangtiles: %d", len(doc.Wangset.WangTiles)) }
	// spot-check edge-id table (see Step 3): line-h -> [0,1,0,1], line-v -> [1,0,1,0],
	// corner-out-nw -> [1,0,0,1], t-e -> [1,1,1,0]
}

func TestKitTiledGoldenByteExact(t *testing.T) {
	// circuit.tsx and circuit-wangset.png bytes equal testdata/golden/…
	// (regen via -update; the tsx is deterministic — fixed 20-class order,
	// fixed wangid derivation, fixed color hexes — and png.Encode of the
	// same PackGrid output is byte-stable).
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compose/ -run TestEmitMetaKitTiled -v`
Expected: FAIL — `emitKitMeta` returns "meta formats apply to atlas, npc, kit" stub error (from Task 6's switch), or tsx files missing.

- [ ] **Step 3: Implement**

Add to `meta.go`:

```go
// wangIDForClass derives a kit class's 4-element Tiled edge wangid
// [north, east, south, west] with colors 0=none 1=line-color, from the
// class's connectivity as documented in wang.go's Hint strings. NOTE
// (documented limitation, see spec): this is an EDGE-color-only wangset —
// inner and outer corner classes share connectivity, so Tiled's autotile
// may pick either. The full 20 classes remain individually selectable in
// the terrain brush. The single source of truth for the mapping is this
// function; reconcile it against the real KitClasses table (wang.go:13)
// when implementing — every key must be covered.
//
// Semantic reference (verified against wang.go Hints): line-h runs E–W;
// line-v runs N–S; corner-out-<q> enters one edge and exits the adjacent
// edge through quarter-circle <q>; corner-in-* joins the same edge pair;
// t-<dir> has its crossbar spanning the two edges perpendicular to <dir>
// with the stem exiting <dir> (t-e: crossbar N–S + stem E → [1,1,1,0];
// t-w: crossbar N–S + stem W → [1,0,1,1]; t-n: crossbar E–W + stem N →
// [1,1,0,1]; t-s: crossbar E–W + stem S → [0,1,1,1]); cap-<dir> carries
// line on the single <dir> edge only; cross joins all four; empty none.
func wangIDForClass(key string) ([4]int, error) {
	switch {
	case key == "line-h": return [4]int{0, 1, 0, 1}, nil
	case key == "line-v": return [4]int{1, 0, 1, 0}, nil
	case key == "cross":  return [4]int{1, 1, 1, 1}, nil
	case key == "empty":  return [4]int{0, 0, 0, 0}, nil
	case strings.HasPrefix(key, "t-"):
		switch key[2:] {
		case "e": return [4]int{1, 1, 1, 0}, nil // crossbar N–S, stem E
		case "w": return [4]int{1, 0, 1, 1}, nil // crossbar N–S, stem W
		case "n": return [4]int{1, 1, 0, 1}, nil // crossbar E–W, stem N
		case "s": return [4]int{0, 1, 1, 1}, nil // crossbar E–W, stem S
		}
		return [4]int{}, fmt.Errorf("kit class %q: bad t-stem", key)
	case strings.HasPrefix(key, "cap-"):
		if len(key) != 5 {
			return [4]int{}, fmt.Errorf("kit class %q: bad cap", key)
		}
		id := [4]int{0, 0, 0, 0}
		switch key[4:] {
		case "e": id[1] = 1
		case "w": id[3] = 1
		case "n": id[0] = 1
		case "s": id[2] = 1
		default: return [4]int{}, fmt.Errorf("kit class %q: bad cap", key)
		}
		return id, nil
	case strings.HasPrefix(key, "corner-out-"), strings.HasPrefix(key, "corner-in-"):
		id := [4]int{0, 0, 0, 0}
		switch key[len(key)-2:] {
		case "nw": id[0], id[3] = 1, 1
		case "ne": id[0], id[1] = 1, 1
		case "sw": id[2], id[3] = 1, 1
		case "se": id[1], id[2] = 1, 1
		}
		return id, nil
	}
	return [4]int{}, fmt.Errorf("kit class %q: no wang mapping", key)
}
```

`emitKitMeta`:

```go
// emitKitMeta assembles the 20 class PNGs (fixed KitClasses order, 4
// columns) into one wangset sheet via PackGrid and emits <name>.tsx
// (tileset + edge-color wangset).
func emitKitMeta(name string, formats []string, outs []OutputFile) ([]OutputFile, error) {
	framePNGs := make([][]byte, 0, len(KitClasses))
	for _, kc := range KitClasses {
		of, ok := findFile(outs, "-"+kc.Key+".png")
		if !ok {
			return nil, fmt.Errorf("kit class file %s-%s.png missing", name, kc.Key)
		}
		framePNGs = append(framePNGs, of.Data)
	}
	sheetPNG, metas, err := PackGrid(name+"-wangset", framePNGs, 4)
	if err != nil {
		return nil, fmt.Errorf("kit wangset sheet: %w", err)
	}
	b := img.Bounds... // decode not needed: use metas + the known cw/ch from PackGrid
	// dims: first tile W/H — metas[0].Frame.W/H
	tileW := metas[0].Frame.W
	tileH := metas[0].Frame.H
	var buf bytes.Buffer
	buf.WriteString("<?xml version=\"1.0\"?>\n")
	fmt.Fprintf(&buf, `<tileset version="1.10" name=%q tilewidth="%d" tileheight="%d" tilecount="%d" columns="4">`+"\n",
		name, tileW, tileH, len(KitClasses))
	fmt.Fprintf(&buf, ` <image source=%q width="%d" height="%d"/>`+"\n",
		name+"-wangset.png", tileW*4, tileH*5)
	fmt.Fprintf(&buf, ` <wangset name=%q type="edge" tile="-1">`+"\n", name)
	buf.WriteString(`  <wangedgecolors>` + "\n")
	buf.WriteString(`   <color color="#000000"/>` + "\n")
	buf.WriteString(`   <color color="#ff8800"/>` + "\n")
	buf.WriteString(`  </wangedgecolors>` + "\n")
	buf.WriteString(`  <wangtiles>` + "\n")
	for i, kc := range KitClasses {
		id, err := wangIDForClass(kc.Key)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, `   <wangtile tileid="%d" wangid="%d,%d,%d,%d"/>`+"\n", i, id[0], id[1], id[2], id[3])
	}
	buf.WriteString(`  </wangtiles>` + "\n")
	buf.WriteString(` </wangset>` + "\n")
	buf.WriteString("</tileset>\n")
	return []OutputFile{
		{Name: name + "-wangset.png", Data: sheetPNG},
		{Name: name + ".tsx", Data: buf.Bytes()},
	}, nil
}
```

Wire `"tiled"` in EmitMeta's switch to `emitKitMeta` (replacing the Task 6 stub error for kit).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/compose/ -v` full green; `go vet ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/compose/meta.go internal/compose/meta_test.go
git commit -m "feat(compose): kit tiled tsx emitter with edge-color wangset"
```

---

### Task 8: Queue plumbing — `meta_formats` validation, persistence, emission

**Files:**
- Modify: `internal/queue/compose.go` — `AssetSpecIn` (~line 339 area), `assetSpec` (~line 42), `SubmitAssetSet` kind switch (~line 366+), `composeAssetSet` (~line 620)
- Modify: `docs/superpowers/specs/2026-10-14-capability-roadmap-round2-design.md` (one-line table amendment)
- Test: `internal/queue/compose_test.go` (extend)

**Interfaces:**
- Consumes: Task 6/7's `compose.EmitMeta(kind, name string, formats []string, outs []OutputFile) ([]OutputFile, error)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestMetaFormatsValidation(t *testing.T) {
	mgr := testManager(t)
	bad := func(req AssetSetRequest, want string) {
		_, err := mgr.SubmitAssetSet(req)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	bad(setWithMeta("element", []string{"phaser"}), "meta formats apply to atlas, npc, kit")
	bad(setWithMeta("atlas", []string{"tiled"}), `"tiled" does not apply to kind "atlas"`)
	bad(setWithMeta("atlas", []string{"nope"}), "unknown")
	bad(setWithMeta("kit", []string{"phaser"}), `does not apply`)
}

func TestAssetSetEmitsMetaFiles(t *testing.T) {
	// run one atlas set (3 frames, columns 0) with meta_formats ["phaser"]
	// through the fake-ops harness to done. Assert set files include
	// "<name>.phaser.json" and that GET /api/v1/sets/<id> file list includes
	// it (set read endpoints list files from the manifest — whatever the
	// existing tests assert for atlas.json applies identically).
}
```

`setWithMeta(kind, formats)` builds a minimal one-asset request using the existing helpers in the test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queue/ -run MetaFormats -v`
Expected: FAIL — field unknown / no validation error.

- [ ] **Step 3: Implement**

1. `AssetSpecIn` gains: `MetaFormats []string `json:"meta_formats,omitempty"``
2. `assetSpec` gains the same field (same tag) — this is what makes a mid-render restart rebuild the same set.
3. `SubmitAssetSet`: before the per-kind switch, validate once:

```go
	if len(a.MetaFormats) > 0 {
		valid := map[string]bool{"phaser": true, "unity": true, "sparrow": true, "tiled": true}
		for _, f := range a.MetaFormats {
			if !valid[f] {
				return "", &ValidationError{Code: "validation_error",
					Msg: fmt.Sprintf("asset %q: meta_formats: format %q unknown (valid: phaser, unity, sparrow, tiled)", an, f)}
			}
		}
	}
```

(Kind-compat lives in `EmitMeta` — queue validation checks format names only; `EmitMeta` produces the friendlier does-not-apply error on kind mismatch, which surfaces as the asset's `broken` error in composeAssetSet, consistent with how compose errors already report per-asset.)

4. Every `as := assetSpec{...}` assignment in the switch gains `MetaFormats: a.MetaFormats,`.
5. In `composeAssetSet`, right after the per-kind compose switch succeeded (`if err == nil { outsByAsset[ai] = outs }`), insert emission so failures mark the asset broken like compose errors do:

```go
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
```

The palette-lock pass that follows already skips non-PNG outputs (`strings.HasSuffix(of.Name, ".png")` guard), so metadata files pass through unremapped — no change needed there.

6. Spec amendment — in the roadmap spec's A2 file-format table, replace the three rows with the concretized table from this plan's "Concretized decisions" (phaser/unity/sparrow → atlas, npc; tiled → kit with the wangset sheet) so spec and code agree. One explanatory sentence: single-texture formats target assets that ship one sheet.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/queue/ ./internal/compose/ -v` green; `go vet ./... && go test ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/queue/compose.go internal/queue/compose_test.go docs/superpowers/specs/2026-10-14-capability-roadmap-round2-design.md
git commit -m "feat(queue): meta_formats opt-in emission for asset sets"
```

---

### Task 9: Idempotency persistence

**Files:**
- Modify: `cmd/img-gen/v1.go` — `idemStore` (~line 513) + `complete` (~546) + `v1Deps` wiring point in `cmd/img-gen/main.go:249`
- Test: `cmd/img-gen/v1_test.go` (extend)

**Interfaces:**
- Produces: `func newIdemStore(path string) (*idemStore, error)` replacing the current zero-value map; `idemStore.complete` appends `{"key":..,"status":..,"body":"<b64>"}` to `path` synchronously.

- [ ] **Step 1: Write the failing test**

```go
func TestIdemStorePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idempotency.jsonl")
	s1 := newIdemStore(t, path) // helper: assert(path) + return store
	_, err := s1.reserve("k1") // fresh
	if err != nil { t.Fatal(err) }
	body := []byte(`{"id":"123"}`)
	s1.complete("k1", http.StatusCreated, body)

	s2 := newIdemStore(t, path) // "restart": fresh store, same file
	state, reply := s2.reserve("k1")
	if state != "replay" {
		t.Fatalf("want replay, got %q", state)
	}
	if !bytes.Equal(body, reply.raw) {
		t.Fatalf("body mismatch: %q vs %q", reply.raw, body)
	}
	// in-flight entries do NOT survive (a crashed process's job is dead):
	s1.reserve("k2")
	s2 := newIdemStore(t, path)
	if st, _ := s2.reserve("k2"); st != "fresh" {
		t.Fatalf("in-flight entry must not replay, got %q", st)
	}
}

func TestIdemStoreRotationCapsReplay(t *testing.T) {
	// write 20_005 entries directly to the file (one json line each, via
	// the same struct shape), open a store, assert: seen has last 10_000
	// keys only, and the file on disk now holds exactly 10_000 lines.
}
```

Test helpers: `newIdemStore(t, path)` wraps the constructor (no error expected under t.TempDir) — adapt constructor signature accordingly.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/img-gen/ -run TestIdemStore -v`
Expected: FAIL — constructor takes no path; nothing persists.

- [ ] **Step 3: Implement**

In `v1.go`, evolve `idemStore`:

```go
type idemStore struct {
	mu   sync.Mutex
	seen map[string]*idemEntry
	path string // DATA_DIR/idempotency.jsonl; empty = legacy in-memory behavior (tests)
}

// idemRecord is the JSONL persistence form of one COMPLETED key. In-flight
// entries are deliberately not persisted: a restart killed their jobs.
type idemRecord struct {
	Key    string `json:"key"`
	Status int    `json:"status"`
	Body   string `json:"body"` // base64 of the response body
}

const idemReplayLines = 10000 // entries replayed/kept on load
const idemRotateLines = 20000 // above this, the file compacts to idemReplayLines on load

func newIdemStore(path string) (*idemStore, error) {
	s := &idemStore{seen: map[string]*idemEntry{}, path: path}
	lines, err := readLastLines(path, idemReplayLines)
	if err != nil {
		return nil, err
	}
	for _, ln := range lines {
		var rec idemRecord
		if json.Unmarshal(ln, &rec) != nil || rec.Key == "" {
			continue // tolerate a torn tail line from a crash
		}
		raw, err := base64.StdEncoding.DecodeString(rec.Body)
		if err != nil {
			continue
		}
		s.seen[rec.Key] = &idemEntry{done: true, status: rec.Status, body: raw}
	}
	// rotation: compact when the file grows past 2x the replay window
	if len(lines) >= idemReplayLines {
		// readLastLines already fetched the tail; rewrite only when the
		// file exceeded idemRotateLines — count lines first via a quick scan.
	}
	return s, nil
}
```

`complete` gains the append (guarded: write errors are logged-and-ignored — in-memory semantics stay authoritative):

```go
func (s *idemStore) complete(key string, status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.seen[key]; ok && !e.done {
		e.done, e.status, e.body = true, status, body
		if s.path != "" {
			rec, _ := json.Marshal(idemRecord{Key: key, Status: status, Body: base64.StdEncoding.EncodeToString(body)})
			f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, werr := f.Write(append(rec, '\n'))
				f.Close()
				_ = werr // disk-loss of one replay entry is acceptable (spec)
			}
		}
	}
}
```

(`readLastLines(path, n)` — new small helper in v1.go: last n NEWLINE-split lines, handling a trailing newline and a torn final line; returns them in file order. Rotation: helper returns the total line count; `newIdemStore` rewrites the file with the replay tail when total > idemRotateLines — single atomic `os.WriteFile` to a temp then rename.)

In `main.go` at the `registerV1` call (line 249): build the store and pass it:

```go
	idem, idemErr := newIdemStore(filepath.Join(cfg.DataDir, "idempotency.jsonl"))
	if idemErr != nil {
		cfg-fatalf("idempotency store: %v", idemErr) // match main.go's existing fatal pattern
	}
	registerV1(mux, &v1Deps{ ...existing fields..., idem: *idem })
```

(v1Deps.idem changes from value to pointer or stays a value per the struct's ergonomics — pick pointer `*idemStore` to avoid a copy-with-slice semantics surprise and update `d.idem.reserve(...)` call sites accordingly.)

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/img-gen/ -v` green (existing v1 tests use the zero-value store — they now go through the `path: ""` legacy branch, unchanged behavior); `go vet ./... && go test ./...`.

- [ ] **Step 5: Commit**

```bash
git add cmd/img-gen/v1.go cmd/img-gen/main.go cmd/img-gen/v1_test.go
git commit -m "feat(v1): persist idempotency keys to DATA_DIR/idempotency.jsonl"
```

---

### Task 10: Docs, MCP passthrough check, final verification

**Files:**
- Modify: `README.md` (+ any skills/howto files that document keyframes and asset-set fields — grep `keyframes` and `fill_states` to find every doc that mentions gif source types or asset fields; README.md:182-183 currently says keyframes is phase-3-planned)
- Verify: `cmd/img-gen-mcp/main.go:109` (the MCP `make_gif` note that keyframes requires RIFE_URL — reword to real behavior: validation_error when RIFE_URL unset)
- No behavioral code changes in this task beyond those doc strings.

- [ ] **Step 1: grep-and-fix pass**

Run: `grep -rn "keyframes\|not_implemented" README.md docs/ skills/ cmd/ --exclude-dir=node_modules | grep -v "_test\|superpowers/specs\|superpowers/plans"`
Fix every statement that calls keyframes deferred/not implemented:
- README: document the keyframes source (`anchors` 2..16, `multiplier` 2..10 default 3, `(n-1)*m+1 ≤ 64` projection, `RIFE_URL` env) and the sidecar's `POST /interpolate {images:[b64], multiplier} → {frames:[b64]}` contract for sidecar operators.
- Document `meta_formats` (the four formats, the supported-kind table, "files appear in the set listing and download individually").
- One paragraph on PNG tEXt params (`software`/`parameters`... the exact keys: `software`, `prompt`, `model`, `size`, `seed`, `mode`, `job`, `kind`, `name`, `set`) and one on idempotency persistence (`DATA_DIR/idempotency.jsonl`, ≤10k replayed, restart returns replayed responses).
- `cmd/img-gen-mcp/main.go:109` tool description: replace the pending-sidecar wording with the validation-error wording.
- Spec A2's Kenney-style pack-layout section (docs-only per spec): add the README section describing how to lay a finished set's files into Kenney-convention folders (`images/`, `atlases/`, `spritesheets/`, `tilemaps/`) for sharing — no code.

- [ ] **Step 2: Full verification**

Run: `go vet ./... && go test ./... && go build ./...`
Expected: all green. Also `grep -rn "not_implemented" internal/ cmd/` returns NOTHING — the keyframes branch (compose.go:973) was the codebase's only live `not_implemented` path.

- [ ] **Step 3: Smoke (optional but recommended when a render instance is reachable)**

Against the :9106 render instance (its env already carries sidecar URLs; `RIFE_URL` may be unset — a keyframes submit then must return `validation_error` mentioning `RIFE_URL` — that is the honest-failure proof):

```bash
curl -s -X POST http://127.0.0.1:9106/api/v1/gifs -H 'Content-Type: application/json' -d '{"name":"kf-probe","source":{"type":"keyframes","keyframes":{"anchors":[{"b64":"<16x16 png b64>"},{"b64":"<16x16 png b64>"}],"multiplier":2}}}'
```

Expected: `{"error":{"code":"validation_error",...,"RIFE_URL"..."}}` with the :9106 instance restarted on a worktree build first (never the live :8099). If no instance is available, skip — the fake-sidecar tests already cover the golden path.

- [ ] **Step 4: Commit**

```bash
git add -A README.md docs/ skills/ cmd/img-gen-mcp/main.go
git commit -m "docs: phase A features — keyframes, meta_formats, tEXt params, idempotency persistence"
```

---

## Self-Review checklist (completed during plan writing)

- Spec coverage: A1 → Tasks 3/4/5; A2 → Tasks 6/7/8 (+ spec amendment); A3 → Tasks 1/2; A4 → Task 9; deploy wiring → Task 3; docs → Task 10. Phase A testing requirements from the spec (fake RIFE sidecar, golden metadata tests, tEXt round-trip, restart simulation) all land inside their owning tasks.
- Known deviations from spec text, deliberate and recorded: (1) the `meta_formats` supported-kind table — single-texture formats phaser/unity/sparrow target atlas+npc, tiled targets kit, written back into the spec via Task 8's amendment; (2) Wang v1 is edge-color-only (inner/outer corner classes share connectivity — documented limitation in Task 7); (3) the keyframes request field is `anchors` where the spec sketched `refs` (Task 4 — matches the sidecar contract's `images` naming); (4) the frame-cap formula is `(n-1)*m+1 ≤ 64`, precise where the spec's `ceil(n*k)` approximated; (5) idempotency records persist `{key, status, body}` where the spec sketched `{key, scope, id, ts}` — persisting the full response keeps restart replay byte-identical to the current in-memory behavior; (6) boot-time rotation compacts files >20k lines to the 10k replay window (spec specifies replay only; rotation bounds unbounded file growth).
- Type consistency: `KeyframesSource`/`keyframesPayload` (Tasks 4/5) distinct names; `EmitMeta(kind, name, formats, outs)` signature identical in Tasks 6/7/8; `Ops.Rife(ctx, [][]byte, int) ([][]byte, error)` identical in Tasks 3/5; `com.ScanTextChunks` used by Task 2 exported in Task 1.
- No placeholders: every code step carries the real code or a named existing pattern to adapt (`resolveFrameRef`, the from_frames submit tail, existing fake-Ops test harness).