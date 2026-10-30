# Phase G — Agent Surface & Model Profiles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the legacy/v1 fork by building the agent surface — model profiles (Pony/NoobAI/Illustrious presets), MCP tool completion, and UI integration. This makes img-gen fully usable from coding agents and the MCP interface without manual v1 API calls.

**Architecture:** 
- Model profile catalog (YAML/JSON) with per-family defaults (v-pred, CFG rescale, score tags, prefixes)
- Complete MCP tool coverage (all v1 endpoints exposed)
- Agent-facing skills for common workflows
- UI updates to surface new capabilities

**Tech Stack:** Go stdlib; existing MCP server; YAML/JSON for model profiles; existing v1 handlers.

**Spec:** Based on round-3 research §5: "agent surface+model profiles+UI closes the legacy/v1 fork". Per-model-family profiles from research §2.2 (NoobAI v-pred needs `--prediction v`+CFG rescale; Pony V7=AuraFlow NOT supported by sd.cpp; SD1.5 NSFW family is a cheap add).

## Global Constraints

- Go stdlib only — no new module dependencies, no go.mod edits
- Every task ends green: `go vet ./... && go test ./...` must pass before the commit
- Byte-for-byte defaults: every new field is opt-in; omitting all new fields produces byte-identical request payloads
- Honest failure: unset configs produce validation errors with named reasons
- Tests bind random ports (httptest). No secrets in tracked files
- Every git commit message ends with the attribution trailer:
  `Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>`

---

### Task 1: Model Profile Catalog

**Files:**
- Create: `models_profiles.yaml` (or JSON) — per-family presets
- Modify: `internal/models/models.go` — load and resolve profiles
- Modify: `cmd/img-gen/main.go` — load profiles config

**Profile Schema:**
```yaml
profiles:
  - name: "pony"
    models: ["pony-v6", "pony-v7"]
    defaults:
      prediction_type: "v"
      cfg_rescale: 0.7
      score_tags: "score_9, score_8_up, score_7_up"
      negative: "score_6, score_5, score_4, worst quality"
      sampler: "euler_a"
      steps: 30
    unsupported: ["pony-v7"]  # AuraFlow not in sd.cpp
  - name: "noobai"
    models: ["noobai-vpred"]
    defaults:
      prediction_type: "v"
      cfg_rescale: 0.5
      positive_prefix: "masterpiece, best quality, "
      negative_prefix: "worst quality, low quality, "
  - name: "illustrious"
    models: ["illustrious-xl"]
    defaults:
      positive_prefix: "masterpiece, best quality, "
      negative: "worst quality, low quality, "
  - name: "sd15-nsfw"
    models: ["juggernaut-xl", "realistic-vision"]
    defaults:
      sampler: "dpmpp_2m"
      steps: 20
```

- [ ] **Step 1: Create models_profiles.yaml with researched profiles**
- [ ] **Step 2: Add Profile struct and loading to internal/models**
- [ ] **Step 3: Wire profile resolution in queue.Submit (model catalog key → profile defaults)**

---

### Task 2: Complete MCP Tool Coverage

**Files:**
- Modify: `cmd/img-gen-mcp/main.go` — add missing v1 endpoints

**Missing endpoints to expose:**
- [ ] POST /api/v1/repair (Phase C) — sync pixel-art repair
- [ ] POST /api/v1/validate (Phase D) — sync critic checklist evaluation
- [ ] POST /api/v1/assets (full) — asset set submission (already partial)
- [ ] GET /api/v1/assets/{id} — asset set status
- [ ] POST /api/v1/generate — single image (already exists)
- [ ] POST /api/v1/edit — edit mode (already exists)
- [ ] POST /api/v1/inpaint — inpaint mode (already exists)
- [ ] POST /api/v1/blend — blend mode (already exists)

- [ ] **Step 1: Add repair tool with full schema**
- [ ] **Step 2: Add validate tool with checklist schema**
- [ ] **Step 3: Add asset set get tool**
- [ ] **Step 4: Verify all tools return proper envelope responses**

---

### Task 3: Agent Skills

**Files:**
- Create: `skills/img-gen-character/SKILL.md` — character generation workflow
- Create: `skills/img-gen-pixel-art/SKILL.md` — pixel art repair workflow
- Create: `skills/img-gen-animation/SKILL.md` — GIF/keyframe workflow
- Update: `skills/img-gen-assets/SKILL.md` — add character kind
- Update: `skills/img-gen-validation/SKILL.md` — add validate tool usage

**Skill content pattern:**
- Trigger phrases
- Tool sequences
- Parameter guidance
- Common patterns/examples

---

### Task 4: UI Integration

**Files:**
- Modify: `static/` (embedded frontend) — add new asset kinds to UI
- Model profile selector in generate form
- Character kind form (identity + poses)
- Pixel-fix/outline checkboxes for element/atlas/frame
- Repair endpoint test button

---

### Task 5: Legacy v1 Fork Closure

**Files:**
- Verify: All v1 endpoints have MCP equivalents
- Deprecate: Direct v1 HTTP calls in docs (point to MCP)
- Test: Agent can complete full workflows via MCP only

---

### Task 6: Tests

**Files:**
- Create: `internal/models/profiles_test.go`
- Create: `cmd/img-gen-mcp/tools_test.go`

---

### Task 7: Full verification + smoke

- [ ] Run: `go vet ./... && go test ./... && go build ./...`
- [ ] Test model profile resolution end-to-end
- [ ] Test all MCP tools return proper envelopes
- [ ] Smoke test agent workflows via MCP
- [ ] Commit with proper attribution