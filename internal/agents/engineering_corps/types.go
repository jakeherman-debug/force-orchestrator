// Package engineering_corps implements the Engineering Corps claim-loop
// agent for Deliverable 3 Phase 3.
//
// EC is the experimentation orchestrator: it consumes Librarian-emitted
// candidate PromotionProposals, drafts experiment YAML, monitors running
// experiments, declares winners and nulls, assembles ratifiable
// PromotionProposals, and manages the global holdout. See
// docs/paired-runs.md § Engineering Corps and docs/next-gen-agents.md
// § Engineering Corps for the spec.
//
// Phase 1 (this commit) ships the SpawnEngineeringCorps loop, the
// dispatcher, and stub handlers returning ErrNotImplemented. Phase 3
// sub-agent A fills in the six task type handlers behind the dispatcher
// switch.
package engineering_corps

// ── Authoritative task type inventory ────────────────────────────────
//
// The const block below is the canonical list of EC claimable task
// types. Sub-agents discover the inventory from this file (Phase 3
// sub-agent A's discovery phase greps for "TaskType" here). Adding a
// new task type means: add a const here, add the dispatcher switch
// case in engineering_corps.go, add a handler file, add tests.
//
// Names use the "EC" prefix so a stray BountyBoard.type value collision
// with a non-EC task type is impossible. The string values are the
// BountyBoard.type column values the claim loop matches against.
const (
	// TaskTypeExperimentAuthor — turn a Librarian candidate
	// PromotionProposal into a complete experiment YAML.
	TaskTypeExperimentAuthor = "ECExperimentAuthor"

	// TaskTypeExperimentMonitor — watch a running experiment's
	// posterior, budget, duration; declare winner/null/inconclusive;
	// emergency-stop on degradation.
	TaskTypeExperimentMonitor = "ECExperimentMonitor"

	// TaskTypePromotionAuthor — on winner + confirm, assemble a
	// ratifiable PromotionProposals row with full evidence trail.
	TaskTypePromotionAuthor = "ECPromotionAuthor"

	// TaskTypeDemotionAuthor — on retention-report signal, assemble a
	// demotion PromotionProposals row.
	TaskTypeDemotionAuthor = "ECDemotionAuthor"

	// TaskTypeMetricAuthor — when a hypothesis needs an unregistered
	// metric, generate the metric SQL + test + manifest.
	TaskTypeMetricAuthor = "ECMetricAuthor"

	// TaskTypeHoldoutMonitor — run the holdout refresh lifecycle;
	// detect model-deprecation threats; emit operator mail.
	TaskTypeHoldoutMonitor = "ECHoldoutMonitor"
)

// AllTaskTypes is the canonical iteration order — claim attempts are
// made in this order, and tests iterate this slice to assert the
// dispatcher routes every type.
var AllTaskTypes = []string{
	TaskTypeExperimentAuthor,
	TaskTypeExperimentMonitor,
	TaskTypePromotionAuthor,
	TaskTypeDemotionAuthor,
	TaskTypeMetricAuthor,
	TaskTypeHoldoutMonitor,
}

// ── Shared types used by handlers ────────────────────────────────────

// Candidate is the librarian-emitted hypothesis that EC turns into an
// experiment via the ExperimentAuthor handler. Mirrors the shape of a
// PromotionProposals row with kind='candidate' (authored_by='librarian'
// per Phase 3 sub-agent B's handoff). Sub-agent B may extend this type.
type Candidate struct {
	ProposalID    int    // PromotionProposals.id
	HypothesisKey string // identifies the rule under hypothesis
	HypothesisRaw string // raw natural-language hypothesis text
	EvidenceJSON  string // librarian's evidence summary
}

// ProposedExperiment is the YAML-shaped output of the ExperimentAuthor
// handler — what EC writes to disk for the operator to ratify before
// the experiment enters running state. Contents are sub-agent A's to
// freeze; the type is declared here so the handler signatures compile.
type ProposedExperiment struct {
	ManifestPath string // disk path to the authored YAML
	Hypothesis   string
	StakesTier   string // 'low' | 'medium' | 'high' | 'safety_critical'
}

// Outcome is the post-termination rollup the ExperimentMonitor handler
// produces. The Bayesian posterior + decision string drive whether
// PromotionAuthor is queued.
type Outcome struct {
	ExperimentID    int
	WinnerArm       string // '' if null/inconclusive
	Decision        string // 'winner' | 'null' | 'inconclusive' | 'emergency_stop'
	PosteriorProb   float64
	TerminationKind string // 'completed' | 'over_budget' | 'emergency_stop'
}

// PromotionProposal is the EC-internal view of a ratifiable
// PromotionProposals row. Distinct from store.PromotionProposal (the
// DB row shape) so handlers can assemble the proposal incrementally
// before persisting it. The PromotionAuthor handler writes this into
// the PromotionProposals table.
type PromotionProposal struct {
	ExperimentID         int
	WinnerArm            string
	RuleKey              string
	ProposedContent      string
	EvidenceSummaryJSON  string
}
