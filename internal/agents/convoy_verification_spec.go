package agents

// D3 fix-loop-1 / γ2 — verification_spec_json consumer at DraftPROpen
// (concern #6, exit criterion 6).
//
// At DraftPROpen, ConvoyReview evaluates each AT in the convoy's
// verification_spec_json against the ask-branch diff. AT failures merge
// with the existing LLM-review findings into a unified findings list and
// flow through the existing fix-task-spawn path.
//
// Spec shape (per roadmap line 1022; concern #9):
//
//	{
//	  "ats": [
//	    {"id": "AT-1", "description": "...", "evaluator": "..."}
//	  ],
//	  "exit_criteria": [
//	    {"id": "EC-1", "description": "..."}
//	  ],
//	  "anti_cheat": ["..."],
//	  "deprecated": [
//	    {"at_id": "AT-3", "removed_at": "...", "removed_by_email": "...",
//	     "rationale": "...", "removal_kind": "..."}
//	  ]
//	}
//
// AT evaluators are simple semantic markers ("substring:foo" /
// "regex:^bar" / "must_touch:path/to/file") — full LLM-driven AT
// evaluation lives in the existing LLM-review pass; this consumer
// provides the cheap mechanical pre-check that grounds the LLM's
// findings against the spec's named acceptance tests.
//
// Pattern-test wiring: AT lookups MUST use compound (convoy_id, at_id)
// keys (concern #8 — slice α's P20). The functions in this file accept
// a parsed spec already scoped to one convoy, so the compound-key
// invariant is satisfied at the call boundary in convoy_review.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

// VerificationSpec is the parsed shape of Convoys.verification_spec_json.
//
// All fields are optional: an empty spec is valid (a convoy with no ATs
// declared is evaluated only by the LLM pass). The deprecated list is
// the spec-deprecation flow's archive; deprecated AT IDs are skipped at
// evaluation time and surfaced as "[CONVOY REVIEW] Skipped deprecated"
// events instead of contributing fix tasks.
type VerificationSpec struct {
	ATs           []SpecAT          `json:"ats,omitempty"`
	ExitCriteria  []SpecEC          `json:"exit_criteria,omitempty"`
	AntiCheat     []string          `json:"anti_cheat,omitempty"`
	Deprecated    []SpecDeprecation `json:"deprecated,omitempty"`
}

// SpecAT — one acceptance test entry. Evaluator is an optional
// semantic marker; absent means "LLM-only evaluation."
type SpecAT struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Evaluator   string `json:"evaluator,omitempty"`
}

// SpecEC — one exit-criterion entry. Same evaluator shape as ATs.
type SpecEC struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Evaluator   string `json:"evaluator,omitempty"`
}

// SpecDeprecation — one row in the deprecated[] archive.
type SpecDeprecation struct {
	ATID           string `json:"at_id"`
	RemovedAt      string `json:"removed_at"`
	RemovedByEmail string `json:"removed_by_email"`
	Rationale      string `json:"rationale"`
	RemovalKind    string `json:"removal_kind"` // 'mistake'|'superseded'|'satisfied'|'out_of_scope'
	SupersededBy   any    `json:"superseded_by,omitempty"`
}

// ATResult is one AT evaluator's verdict for one cycle.
type ATResult struct {
	ATID        string // "AT-1"
	Status      string // "pass" | "fail" | "inconclusive" | "skipped_deprecated"
	Description string // copied from the AT for fix-task payloads
	Evidence    string // why the evaluator returned this status
}

// ParseVerificationSpec parses the raw JSON. Empty / "{}" / unset is
// valid and returns an empty spec. Malformed JSON is an error so
// callers can surface a "[SPEC PARSE ERROR]" event instead of silently
// skipping AT evaluation.
func ParseVerificationSpec(raw string) (*VerificationSpec, error) {
	out := &VerificationSpec{}
	t := strings.TrimSpace(raw)
	if t == "" || t == "{}" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(t), out); err != nil {
		return nil, fmt.Errorf("ParseVerificationSpec: %w", err)
	}
	return out, nil
}

// IsATDeprecated returns true if the spec has a deprecation entry for the
// given AT ID. Used at evaluation time to skip deprecated ATs and emit
// the skip event instead of a fix task. Cheap O(N) over deprecated[]
// (specs typically have <50 entries).
func IsATDeprecated(spec *VerificationSpec, atID string) bool {
	if spec == nil {
		return false
	}
	for _, d := range spec.Deprecated {
		if d.ATID == atID {
			return true
		}
	}
	return false
}

// EvaluateATs runs each non-deprecated AT against the diff text. Returns
// the per-AT results in spec order so the caller can interleave them
// with LLM findings.
//
// Evaluator grammar (kept intentionally narrow — full LLM-driven
// evaluation runs in the existing LLM pass):
//
//   - "" or absent       — skipped, status "inconclusive" (LLM-only)
//   - "substring:STR"    — diff must contain STR
//   - "regex:PATTERN"    — diff must match PATTERN (Go regexp syntax)
//   - "must_touch:PATH"  — diff must include the substring "+++ b/<PATH>"
//                          or "--- a/<PATH>" (added or removed file ref)
//
// Anything else returns inconclusive — the LLM's pass is authoritative
// for evaluators we don't recognize, so we don't false-fail.
//
// emitSkip is invoked for each deprecated AT — callers usually wire it
// to logger.Printf("[CONVOY REVIEW] Skipped deprecated %s", atID).
func EvaluateATs(ctx context.Context, _ *sql.DB, _ string, _ string, spec *VerificationSpec, diff string, emitSkip func(atID string)) ([]ATResult, error) {
	_ = ctx
	if spec == nil {
		return nil, nil
	}
	results := make([]ATResult, 0, len(spec.ATs))
	for _, at := range spec.ATs {
		if IsATDeprecated(spec, at.ID) {
			if emitSkip != nil {
				emitSkip(at.ID)
			}
			results = append(results, ATResult{
				ATID:        at.ID,
				Status:      "skipped_deprecated",
				Description: at.Description,
				Evidence:    "spec.deprecated[] contains this AT",
			})
			continue
		}
		results = append(results, evaluateOneAT(at, diff))
	}
	return results, nil
}

func evaluateOneAT(at SpecAT, diff string) ATResult {
	res := ATResult{
		ATID:        at.ID,
		Status:      "inconclusive",
		Description: at.Description,
	}
	ev := strings.TrimSpace(at.Evaluator)
	if ev == "" {
		res.Evidence = "no evaluator declared — LLM pass is authoritative"
		return res
	}

	switch {
	case strings.HasPrefix(ev, "substring:"):
		needle := strings.TrimPrefix(ev, "substring:")
		if strings.Contains(diff, needle) {
			res.Status = "pass"
			res.Evidence = fmt.Sprintf("diff contains %q", truncEvidence(needle))
		} else {
			res.Status = "fail"
			res.Evidence = fmt.Sprintf("diff missing %q", truncEvidence(needle))
		}
	case strings.HasPrefix(ev, "regex:"):
		pat := strings.TrimPrefix(ev, "regex:")
		re, err := regexp.Compile(pat)
		if err != nil {
			res.Status = "inconclusive"
			res.Evidence = fmt.Sprintf("regex compile failed: %v", err)
			return res
		}
		if re.MatchString(diff) {
			res.Status = "pass"
			res.Evidence = fmt.Sprintf("diff matches regex %q", truncEvidence(pat))
		} else {
			res.Status = "fail"
			res.Evidence = fmt.Sprintf("diff does not match regex %q", truncEvidence(pat))
		}
	case strings.HasPrefix(ev, "must_touch:"):
		path := strings.TrimPrefix(ev, "must_touch:")
		// Match unified-diff file headers "+++ b/<path>" or "--- a/<path>".
		if strings.Contains(diff, "+++ b/"+path) || strings.Contains(diff, "--- a/"+path) {
			res.Status = "pass"
			res.Evidence = fmt.Sprintf("diff touches %s", path)
		} else {
			res.Status = "fail"
			res.Evidence = fmt.Sprintf("diff does not touch %s", path)
		}
	default:
		res.Evidence = fmt.Sprintf("unknown evaluator %q — LLM pass is authoritative", ev)
	}
	return res
}

func truncEvidence(s string) string {
	const cap = 60
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "…"
}

// ATResultsToFindings converts AT failures into convoyReviewFinding
// entries so they can be fed through the existing fix-task spawn path.
// Pass results carry the convoy_id prefix in payloads (concern #8 — UI
// labeling discipline) so downstream displays can disambiguate AT IDs
// across convoys.
func ATResultsToFindings(convoyID int, results []ATResult) []convoyReviewFinding {
	var out []convoyReviewFinding
	for _, r := range results {
		if r.Status != "fail" {
			continue
		}
		out = append(out, convoyReviewFinding{
			Type:        "gap",
			Description: fmt.Sprintf("Convoy #%d / %s failed: %s (%s)", convoyID, r.ATID, r.Description, r.Evidence),
			Fix: fmt.Sprintf("Address acceptance test %s on convoy #%d:\n\n%s\n\nEvidence: %s",
				r.ATID, convoyID, r.Description, r.Evidence),
		})
	}
	return out
}

// LoadFrozenSpec parses the bytes returned by BeginConvoyReviewCycle. Wraps
// store-side concern leakage out of convoy_review.go and centralizes the
// "empty is OK" handling.
func LoadFrozenSpec(frozen string) (*VerificationSpec, error) {
	return ParseVerificationSpec(frozen)
}

// EvaluateConvoySpec is the convoy_review.go entry point: it loads the
// frozen spec, evaluates ATs against the diff, returns:
//
//   - the parsed spec so the caller can examine it (e.g., for Captain
//     re-justification)
//   - the AT results in spec order (skipped/pass/fail/inconclusive)
//   - findings derived from AT failures (ready to merge with LLM findings)
//
// Caller passes a logger to emit skip events.
func EvaluateConvoySpec(ctx context.Context, db *sql.DB, convoyID int, frozenSpec, diff string, logger interface{ Printf(string, ...any) }) (*VerificationSpec, []ATResult, []convoyReviewFinding, error) {
	spec, err := ParseVerificationSpec(frozenSpec)
	if err != nil {
		return nil, nil, nil, err
	}
	results, err := EvaluateATs(ctx, db, "", "", spec, diff, func(atID string) {
		if logger != nil {
			logger.Printf("[CONVOY REVIEW] Skipped deprecated %s on convoy #%d", atID, convoyID)
		}
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return spec, results, ATResultsToFindings(convoyID, results), nil
}

// SerializeATResults turns a slice of ATResult into a JSON object keyed
// by AT ID for storage in ConvoyReviewCycles.outcomes_json.
//
// Shape: {"AT-1":"pass","AT-2":"fail",...}
func SerializeATResults(results []ATResult) string {
	if len(results) == 0 {
		return "{}"
	}
	m := make(map[string]string, len(results))
	for _, r := range results {
		m[r.ATID] = r.Status
	}
	out, _ := json.Marshal(m)
	return string(out)
}

// Compile-time guard: ensure the package can reach the store helpers it
// needs. (Avoids "imported and not used" if a refactor moves the spec
// reader.)
var _ = store.GetConvoy
