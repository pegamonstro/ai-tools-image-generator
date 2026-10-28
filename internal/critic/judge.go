package critic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"img-gen/internal/lattice"
)

// VLMScore is one criterion's binary verdict from the VLM judge.
type VLMScore struct {
	ID     int    `json:"id"`
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Judge evaluates a batch of criteria against an image via the lattice VLM.
type Judge func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error)

// ErrJudgeUnreadable is returned when the judge's reply cannot be parsed as a
// well-formed JSON array matching the criteria count.
var ErrJudgeUnreadable = errors.New("critic: judge reply unreadable")

// judgeSystem is the history-free judge preamble: binary per-criterion
// verdicts, strict JSON, nothing else. The judge never converses.
const judgeSystem = "You are an image QA judge. For each numbered criterion, " +
	"look at the image and decide pass or fail. Answer ONLY with a JSON array, " +
	"one object per criterion, in the same order: " +
	`[{"id": <criterion index starting at 0>, "pass": <true|false>, ` +
	`"reason": "<one short sentence>", "fix": "<how to fix the prompt when it failed, one short sentence, omit when pass>"}]. ` +
	"No prose, no markdown fences."

// ChatJudge builds a Judge that calls the lattice's ChatVision with the given model.
func ChatJudge(c *lattice.Client, model string) Judge {
	return func(ctx context.Context, criteria []string, imageB64 string) ([]VLMScore, error) {
		if len(criteria) == 0 {
			return []VLMScore{}, nil
		}
		// Build user text: "Criteria:\n0. <first>\n1. <second>..."
		userText := "Criteria:\n"
		for i, c := range criteria {
			userText += fmt.Sprintf("%d. %s\n", i, c)
		}
		reply, err := c.ChatVision(ctx, model, judgeSystem, userText, imageB64)
		if err != nil {
			return nil, err
		}
		var scores []VLMScore
		if err := json.Unmarshal([]byte(reply), &scores); err != nil {
			return nil, fmt.Errorf("%w: reply head: %.4096s", ErrJudgeUnreadable, reply)
		}
		// Strict: incomplete array (len < criteria) is also an error — never fail-open.
		if len(scores) < len(criteria) {
			return nil, fmt.Errorf("%w: reply head: %.4096s", ErrJudgeUnreadable, reply)
		}
		return scores, nil
	}
}

// IsNil returns true if the Judge is nil (reflect-free nil check).
func (j Judge) IsNil() bool {
	return j == nil
}
