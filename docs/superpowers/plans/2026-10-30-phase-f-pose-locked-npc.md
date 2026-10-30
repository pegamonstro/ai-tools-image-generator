# Phase F — Pose-Locked NPC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add pose-locked NPC generation by wiring B1 ControlNet (already implemented) with a DWPose sidecar for pose extraction, plus identity-conditioned character sets. This enables consistent character appearance across multiple poses using the existing ControlNet + IPAdapter infrastructure.

**Architecture:** 
- New DWPose sidecar (uv tool from git, like RIFE/ESRGAN) on a new port
- New asset kind "character" that builds identity-conditioned pose sets
- Wire DWPose through the lattice gateway to the queue
- Submit-time validation for character kind assets
- Compose pipeline generates pose variations with consistent identity

**Tech Stack:** Go stdlib; uv-managed Python sidecar for DWPose (like RIFE); existing ControlNet/IPAdapter plumbing from Phase B; httptest-based tests.

**Spec:** Based on round-3 research §5: "pose-locked npc via B1 ControlNet+DWPose sidecar+identity-conditioned sets". The ControlNet wiring from Phase B (control, ip_adapter fields in SubmitRequest) is the foundation.

## Global Constraints

- Go stdlib only — no new module dependencies, no go.mod edits (sidecar managed by uv)
- Every task ends green: `go vet ./... && go test ./...` must pass before the commit
- Byte-for-byte defaults: every new field is opt-in; omitting all new fields produces byte-identical request payloads, job records, and deliverables
- Honest failure: unset sidecar URLs and unreachable sidecars produce validation errors or failed jobs with named reasons — never silent field drops, never panics
- Tests bind random ports (httptest). No secrets in tracked files
- Every git commit message ends with the attribution trailer:
  `Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>`

---

### Task 1: DWPose sidecar deployment (uv tool, git install)

**Files:**
- Create: Deployment script/docs for DWPose sidecar
- Verify: Sidecar responds to pose extraction requests

**Interfaces:**
- Input: base64 PNG image
- Output: JSON with pose keypoints (COCO format 17 keypoints) + optional rendered pose image
- Port: 8904 (new, alongside RIFE 8900, ESRGAN 8901, REMBG 8903)

- [ ] **Step 1: Install DWPose as uv tool from git**
```bash
uv tool install git+https://github.com/IDEA-Research/DWPose.git
# or if on PyPI: uv tool install dwpose
```

- [ ] **Step 2: Create systemd service / launch config**
- Run on port 8904
- Health endpoint: GET /health returns 200 OK
- Main endpoint: POST /pose {image: base64} → {keypoints: [[x,y,conf]...], pose_image: base64?}

- [ ] **Step 3: Verify deployment**
```bash
curl -X POST http://localhost:8904/pose -d '{"image":"<base64>"}' -H "Content-Type: application/json"
```

- [ ] **Step 4: Document in memory and deployment docs**

---

### Task 2: Add DWPose client to lattice gateway

**Files:**
- Modify: `ai-tools-inference-lattice` (separate repo) — add DWPose client
- Wire: lattice.Client.DWPose method

**Interfaces:**
- Consumes: sidecar URL from config (DWPOSE_URL)
- Produces: lattice.DWPose(ctx, imageB64) ([]float32, error) — flattened keypoints [x1,y1,conf1,x2,y2,conf2...]

- [ ] **Step 1: Add DWPOSE_URL config to lattice**
- [ ] **Step 2: Implement DWPose method with timeout**
- [ ] **Step 3: Add health check wiring**

---

### Task 3: Wire DWPose through img-gen queue

**Files:**
- Modify: `cmd/img-gen/main.go` — add DWPOSE_URL config
- Modify: `internal/queue/queue.go` — add DWPose to Options and ImageOps
- Modify: `internal/queue/compose.go` — use DWPose for character kind

**Interfaces:**
- Config: DWPOSE_URL (default ""), DWPOSE_TIMEOUT_S (default 60)
- Queue: Ops.DWPose func(ctx, imageB64) ([]float32, error)

- [ ] **Step 1: Add config fields to main.go**
- [ ] **Step 2: Add to queue.Options and wire in newHandler**
- [ ] **Step 3: Add DWPose to ImageOps struct**

---

### Task 4: Add "character" asset kind

**Files:**
- Modify: `internal/queue/compose.go` — AssetSpecIn validation, assetSpec, kind switch
- Add: Character-specific compose logic

**Interfaces:**
- AssetSpecIn fields: 
  - Kind: "character"
  - CharacterIdentity: base64 reference image for IPAdapter
  - Poses: []string (pose names or DWPose keypoints)
  - ControlNet: existing control params (for pose conditioning)
  - IPAdapter: existing IPAdapter params (for identity conditioning)

- [ ] **Step 1: Add validation for character kind in SubmitAssetSet**
- [ ] **Step 2: Add character case to composeAssetSet kind switch**
- [ ] **Step 3: Implement character compose logic:**
  1. Extract pose keypoints from Poses using DWPose (if pose images provided)
  2. For each pose: generate with ControlNet (pose) + IPAdapter (identity)
  3. Compose as atlas or sheet

---

### Task 5: Submit-time validation for character kind

**Files:**
- Modify: `internal/queue/compose.go` — validation in SubmitAssetSet

**Validation rules:**
- CharacterIdentity required (base64 or catalog reference)
- Poses array required (min 1)
- ControlNet and/or IPAdapter must be configured
- DWPose sidecar must be wired (DWPOSE_URL) if poses are images not keypoints

---

### Task 6: Tests

**Files:**
- Create: `internal/queue/compose_character_test.go`

**Tests:**
- Character asset with pose images → DWPose extraction → ControlNet+IPAdapter generation
- Character asset with explicit keypoints → direct ControlNet generation
- Validation errors for missing identity/poses/sidecar

---

### Task 7: Docs sweep

**Files:**
- Modify: `docs/openapi.yaml` — character kind schema
- Modify: `README.md` — character kind documentation
- Modify: `cmd/img-gen-mcp/main.go` — request_asset_set schema

---

### Task 8: Full verification + smoke

- [ ] Run: `go vet ./... && go test ./... && go build ./...`
- [ ] Deploy DWPose sidecar to test environment
- [ ] Smoke test character generation end-to-end
- [ ] Commit with proper attribution