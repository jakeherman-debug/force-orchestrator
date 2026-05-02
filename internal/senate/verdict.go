// Package senate — Senator verdict shape (D4 Phase 3).
//
// Authoritative spec: docs/next-gen-agents.md § "Senator verdict shape"
// (line ~304). The Senator's review of a proposed plan returns:
//
//	{
//	  "senator": "force-orchestrator",
//	  "position": "concur | dissent | amend",
//	  "rationale": "one paragraph",
//	  "concerns":   [{task_id, concern, severity: 'block' | 'warn'}],
//	  "amendments": [{task_id, new_task}]
//	}
//
// Position drives Chancellor's downstream decision:
//   - all Senators concur (no concerns) → auto-advance to AwaitingChancellorReview
//   - any dissent + any block-severity concern → block (return to Pending)
//   - amendments only → Chancellor applies and re-reviews (Phase 3 stub:
//                       advance with concerns recorded)
//
// "Teeth" gate: a dissent vote with confidence >= dissentTeethConfidence
// (0.8 per spec) blocks unconditionally. Below that threshold, the
// dissent is recorded but does not block, mirroring the spec's "warn"
// path.
package senate

// DissentTeethConfidence is the minimum confidence for a dissent verdict
// to actually block forwarding to AwaitingChancellorReview. Per spec
// teeth: only "high-confidence" dissent counts.
const DissentTeethConfidence = 0.8

// Position is the discrete verdict from one Senator on one Feature plan.
type Position string

const (
	PositionConcur  Position = "concur"
	PositionAmend   Position = "amend"
	PositionDissent Position = "dissent"
)

// Severity is the per-concern dial inside a verdict.
type Severity string

const (
	SeverityBlock Severity = "block"
	SeverityWarn  Severity = "warn"
)

// Concern is one item inside a Verdict's Concerns array. TaskID is the
// position-in-plan index of the task the concern targets (0-based,
// matches store.TaskPlan indexing); 0 with TaskID==0 also encodes a
// plan-wide concern.
type Concern struct {
	TaskID   int      `json:"task_id"`
	Concern  string   `json:"concern"`
	Severity Severity `json:"severity"`
}

// Amendment is one Senator-proposed change to a task in the plan.
// NewTask is the replacement task body (free text; Chancellor's plan-
// merge logic decides whether to splice or re-emit).
type Amendment struct {
	TaskID  int    `json:"task_id"`
	NewTask string `json:"new_task"`
}

// Verdict is one Senator's reply to a Feature plan review. JSON-tagged
// to round-trip with the spec's documented shape; the LLM's response is
// unmarshalled directly into this struct.
type Verdict struct {
	Senator        string      `json:"senator"`
	Position       Position    `json:"position"`
	Rationale      string      `json:"rationale"`
	Concerns       []Concern   `json:"concerns,omitempty"`
	Amendments     []Amendment `json:"amendments,omitempty"`
	Confidence     float64     `json:"confidence"`
	CitedMemoryIDs []int       `json:"cited_memory_ids,omitempty"`
	CitedRuleIDs   []string    `json:"cited_rule_ids,omitempty"`
}

// Approves reports whether this verdict permits the plan to advance to
// AwaitingChancellorReview given the spec's teeth: any non-concur
// position with confidence >= DissentTeethConfidence blocks; any
// concern at SeverityBlock with confidence >= teeth blocks.
func (v Verdict) Approves() bool {
	if v.Position == PositionDissent && v.Confidence >= DissentTeethConfidence {
		return false
	}
	for _, c := range v.Concerns {
		if c.Severity == SeverityBlock && v.Confidence >= DissentTeethConfidence {
			return false
		}
	}
	return true
}

// HasMaterialAmendment reports whether the verdict carries at least one
// amendment that materially changes the plan. Phase 3 ships the boolean;
// the "re-review if material" loop is a Phase-3-follow-up TODO.
func (v Verdict) HasMaterialAmendment() bool {
	return len(v.Amendments) > 0
}
