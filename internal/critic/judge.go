package critic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"img-gen/internal/lattice"
)

// VLMScore is one criterion's binary verdict from the VLM judge.
type VLMScore struct {
	ID     int       `json:"id"`
	Pass   bool      `json:"pass"`
	Reason string    `json:"reason,omitempty"`
	Fix    string    `json:"fix,omitempty"`
	Bbox   []float64 `json:"bbox,omitempty"` // optional: requested region [x,y,w,h], normalized 0..1
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
	`"reason": "<one short sentence>", "fix": "<how to fix the prompt when it failed, one short sentence, omit when pass>", ` +
	`"bbox": "[x,y,w,h] numbers 0..1, only when the criterion asks for an image region, omit otherwise"}]. ` +
	"When a criterion asks for a bounding box or JSON region, put the numbers in the bbox field, " +
	"never inside any string value: braces and quotes are forbidden inside all string values. " +
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
		scores, perr := parseJudgeScores(reply, len(criteria))
		if perr != nil {
			return nil, fmt.Errorf("%w: reply head: %.4096s", ErrJudgeUnreadable, reply)
		}
		// Strict: incomplete array (len < criteria) is also an error — never fail-open.
		if len(scores) < len(criteria) {
			return nil, fmt.Errorf("%w: reply head: %.4096s", ErrJudgeUnreadable, reply)
		}
		// Clients locate requested regions by scanning the fix text; when the judge
		// put the numbers in the bbox field, surface them there in the contract shape.
		for i := range scores {
			b := scores[i].Bbox
			fix := scores[i].Fix
			// Append unless the fix text already carries numeric bbox info.
			if len(b) == 4 && !(strings.Contains(fix, "bbox") && reFixHasNum.MatchString(fix)) {
				scores[i].Fix = strings.TrimRight(fix, " ") + fmt.Sprintf(` {"bbox":[%v,%v,%v,%v]}`, b[0], b[1], b[2], b[3])
			}
		}
		return scores, nil
	}
}

// extractObjects brace-walks reply and returns every top-level {...} object
// that contains an "id" and "pass" key.
func extractObjects(reply string) []string {
	var out []string
	depth := 0
	start := -1
	for i := 0; i < len(reply); i++ {
		c := reply[i]
		switch {
		case c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case c == '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && start >= 0 && i-start > 8 {
				cand := reply[start : i+1]
				if strings.Contains(cand, `"id"`) && strings.Contains(cand, `"pass"`) {
					out = append(out, cand)
				}
				start = -1
			}
		}
	}
	return out
}

var (
	reID        = regexp.MustCompile(`"id"\s*:\s*(-?\d+)`)
	rePass      = regexp.MustCompile(`"pass"\s*:\s*(true|false)`)
	reFix       = regexp.MustCompile(`"fix"\s*:`)
	reBbox      = regexp.MustCompile(`"bbox"\s*:\s*\[([^\]]*)\]`)
	reNums      = regexp.MustCompile(`[\d.]+`)
	reFixHasNum = regexp.MustCompile(`[\d.]`)
)

// salvageObject reads one (possibly corrupted) score object by field-regexes:
// values may contain raw unescaped JSON (like {"bbox":[...]}) which breaks
// plain Unmarshal, but the field boundaries stay readable. fix runs from the
// first quote after the fix key to the last quote in the object (the closing
// quote before the final brace).
func salvageObject(text string) (VLMScore, bool) {
	var s VLMScore
	m := reID.FindStringSubmatch(text)
	if m == nil {
		return s, false
	}
	fmt.Sscanf(m[1], "%d", &s.ID)
	m = rePass.FindStringSubmatch(text)
	if m == nil {
		return s, false
	}
	s.Pass = m[1] == "true"
	if loc := reFix.FindStringIndex(text); loc != nil {
		rest := text[loc[1]:]
		q := strings.Index(rest, `"`)
		if q >= 0 {
			li := strings.LastIndexByte(rest, '"')
			if li > q {
				s.Fix = rest[q+1 : li]
				if idx := strings.IndexByte(s.Fix, 'n'); idx == 0 {
					// fix text cannot start with a bare 'n...' unless it is prose; keep as-is
				}
			} else {
				s.Fix = ""
			}
		}
	}
	if m := reReason.FindStringSubmatch(text); m != nil {
		s.Reason = m[1]
	} else if loc := reReasonKey.FindStringIndex(text); loc != nil {
		rest := text[loc[1]:]
		q := strings.Index(rest, `"`)
		comma := strings.Index(rest[q+1:], `",`)
		if q >= 0 && comma > 0 {
			s.Reason = rest[q+1 : q+1+comma]
		}
	}
	if m := reBbox.FindStringSubmatch(text); m != nil {
		for _, tok := range reNums.FindAllString(m[1], 8) {
			var f float64
			if n, _ := fmt.Sscanf(tok, "%g", &f); n == 1 {
				s.Bbox = append(s.Bbox, f)
			}
		}
	}
	return s, true
}

var (
	reReason    = regexp.MustCompile(`"reason"\s*:\s*"((?:[^"\\]|\\.)*?)"\s*,\s*"fix"`)
	reReasonKey = regexp.MustCompile(`"reason"\s*:`)
)

// parseJudgeScores decodes the judge reply into scores. Strict path: JSON
// array (or single object) Unmarshal. Salvage path: field-regex extraction
// per brace-balanced object, tolerant of raw JSON braces embedded inside
// string values. Salvage never fails open: it must cover the criteria count.
func parseJudgeScores(reply string, want int) ([]VLMScore, error) {
	trimmed := strings.TrimSpace(reply)
	if strings.HasPrefix(trimmed, "{") {
		var one VLMScore
		if err := json.Unmarshal([]byte(trimmed), &one); err == nil {
			return []VLMScore{one}, nil
		}
	} else if strings.HasPrefix(trimmed, "[") {
		var scores []VLMScore
		if err := json.Unmarshal([]byte(trimmed), &scores); err == nil {
			return scores, nil
		}
	}
	var scores []VLMScore
	for _, cand := range extractObjects(reply) {
		if s, ok := salvageObject(cand); ok {
			scores = append(scores, s)
		}
	}
	if len(scores) < want {
		return nil, errors.New("not enough readable scores")
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].ID < scores[j].ID })
	return scores, nil
}

// isNil returns true if the Judge is nil (reflect-free nil check).
func (j Judge) isNil() bool {
	return j == nil
}
