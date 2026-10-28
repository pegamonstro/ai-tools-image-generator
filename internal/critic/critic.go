// Package critic implements Phase D validation: an op registry of checklist
// checks (VLM judge via the lattice, WD14 tagger roundtrip, deterministic
// palette/tile metrics), a compiler for client-submitted specs, and the
// mutate-then-resample retry policy's pure functions (mutation, scoring).
package critic

import (
	"context"
	"encoding/json"
	"fmt"

	"img-gen/internal/compose"
	"img-gen/internal/lattice"
)

// Threshold constants are pinned by the plan; recalibration is a recorded
// ruling with golden updates (Phase C detector precedent), never silent edits.
const (
	BudgetDefault    = 2
	BudgetMax        = 3
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
	Budget  *int        `json:"budget,omitempty"` // nil = default 2; explicit 0 = evaluate-only
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
	Prompt string   `json:"prompt,omitempty"` // source prompt for tag_roundtrip
}

// Availability reports which remote legs are wired (nil Ops/Env functions).
type Availability struct{ Tagger bool }

// Env provides the external dependencies for Run.
type Env struct {
	Judge  Judge
	Tagger func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error)
	Vocab  func(ctx context.Context) ([]string, error)
}

func CompileSpec(spec ValidateSpec, av Availability) (Checklist, error) {
	if len(spec.Checks) == 0 || len(spec.Checks) > MaxChecks {
		return Checklist{}, fmt.Errorf("validate checks take at most %d check entries", MaxChecks)
	}
	vlm := 0
	cl := Checklist{OnFail: spec.OnFail}
	if spec.Budget == nil {
		cl.Budget = BudgetDefault
	} else {
		cl.Budget = *spec.Budget
		if cl.Budget < 0 {
			cl.Budget = 0
		} else if cl.Budget > BudgetMax {
			cl.Budget = BudgetMax
		}
	}
	// Normalize empty OnFail to "deliver"
	if cl.OnFail == "" {
		cl.OnFail = "deliver"
	}
	switch cl.OnFail {
	case "deliver", "fail":
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
			if !av.Tagger {
				return Checklist{}, fmt.Errorf("tag_roundtrip requires the WD14 tagger sidecar (TAGGER_URL)")
			}
		case "palette_conform":
			if len(spec.Palette) == 0 {
				return Checklist{}, fmt.Errorf("palette_conform requires a palette")
			}
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
