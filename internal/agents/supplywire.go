// supplywire.go — daemon-side production wiring for the SUPPLY-*
// manifest-gated rule set (D5 fix-loop iter 1 slice α).
//
// Why this file exists:
//
//   The strict verifier's Static shard caught two production-wiring
//   gaps that no test caught (because every test registers rules
//   manually via isb.RegisterManifestGated and sets up its own
//   SupplyRecheckDeps):
//
//     Gap 1: isb.RegisterManifestGated(...) was NEVER called for any
//            SUPPLY rule in production. runISBReviewTask called
//            DispatchManifestGated, which looped over an empty
//            registry. Zero SUPPLY findings ever fired.
//
//     Gap 2: agents.RegisterSupplyRecheckDeps(...) was NEVER called
//            in production. Dog logged "deps not registered —
//            skipping" on every tick. ConvoyReview gate fell back to
//            the read-only DB check, but Gap 1 ensured there was
//            nothing to find.
//
//   WireSupplyRules below is the single production-side entry point
//   that closes both gaps. The daemon (cmd/force/fleet_cmds.go)
//   calls it once at startup right after the codeartifact + osv
//   clients are constructed.
//
// Why this lives in internal/agents (not internal/isb/rules):
//
//   internal/agents/isb.go blank-imports internal/isb/rules to trigger
//   the rule init()s. If the wiring helper itself lived in
//   internal/isb/rules, importing internal/agents from there would
//   close an import cycle. The helper depends on both packages, so it
//   has to sit in the package that already imports the other.
//
//   It deliberately lives next to dogs_supply_token_recheck.go (which
//   owns SupplyRecheckDeps + RegisterSupplyRecheckDeps) so the wiring
//   surface is co-located with the consumer.
//
// Anti-cheat boundary:
//
//   - Only this file knows the cross-package adapter shape that bridges
//     isb.ManifestGatedRule into supplydeferral.ReplayableRule. The
//     adapter previously lived in test code (d5_p4_e2e_test.go);
//     production code now owns it so future SUPPLY rules can be wired
//     by adding three lines here, not by reproducing the adapter shape
//     in every caller.
//
//   - WireSupplyRules returns an error on nil osvClient. The daemon
//     constructs osv.NewInProcess() unconditionally (no AWS coupling),
//     so a nil osvClient at startup is a programmer error and we fail
//     closed (per CLAUDE.md "no silent failures").
//
//   - A nil codeartifact.Client is tolerated: that environment (CI,
//     non-AWS dev) cannot exercise SUPPLY-001/003/004's CodeArtifact
//     lookups anyway. Each rule already returns a per-call error when
//     the client is nil, which the dispatcher records in its per-rule
//     errs map and surfaces as a warn-level signal — but the daemon
//     still boots and SUPPLY-002 (allowlist-only) and SUPPLY-005
//     (osv-scanner) keep functioning.

package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/rules"
	"force-orchestrator/internal/isb/scanners/osv"
	"force-orchestrator/internal/isb/supplydeferral"
)

// WireSupplyRules registers all five SUPPLY rules (SUPPLY-001..005)
// into the manifest-gated dispatcher AND wires the supply-token-recheck
// dog's deps (codeartifact client + ReplayableRule adapter map + the
// production RepoResolver).
//
// Called once at daemon startup AFTER caClient and osvClient are
// constructed. caClient may be nil in environments without AWS config
// (CI, non-AWS dev); osvClient must be non-nil — if it is, this
// function returns an error and the caller MUST surface that failure
// via the daemon's standard escalation path (no silent failure).
//
// Returns nil on the happy path. Idempotency note: this function is
// NOT idempotent. Calling it twice will panic via
// isb.RegisterManifestGated's duplicate-ID guard. The daemon calls it
// exactly once at boot; tests that exercise it more than once must
// reset the registry between calls (isb.ResetManifestGatedForTest +
// agents.RegisterSupplyRecheckDeps(nil)).
func WireSupplyRules(db *sql.DB, caClient codeartifact.Client, osvClient osv.Client) error {
	if osvClient == nil {
		return errors.New("WireSupplyRules: osvClient required (osv.NewInProcess() never returns nil; check daemon startup wiring)")
	}

	// Construct each rule. caClient may be nil — see file comment.
	s001 := rules.NewSUPPLY001(caClient)
	s002 := rules.NewSUPPLY002()
	s003 := rules.NewSUPPLY003(caClient)
	s004, err := rules.NewSUPPLY004(caClient)
	if err != nil {
		return fmt.Errorf("WireSupplyRules: NewSUPPLY004: %w", err)
	}
	s005 := rules.NewSUPPLY005(osvClient)

	// Register into the manifest-gated dispatcher. Order matches the
	// rule-ID numbering for human-readability when AllManifestGated()
	// is dumped (e.g. by debug tooling).
	isb.RegisterManifestGated(s001)
	isb.RegisterManifestGated(s002)
	isb.RegisterManifestGated(s003)
	isb.RegisterManifestGated(s004)
	isb.RegisterManifestGated(s005)

	// Wire the supply-token-recheck dog deps. The replay map only
	// includes rules that actually go through the codeartifact deferral
	// path: SUPPLY-001 (publisher pin), SUPPLY-003 (recency), SUPPLY-004
	// (license matrix). SUPPLY-002 is allowlist-only (no codeartifact
	// dependency); SUPPLY-005 uses osv-scanner against OSV.dev (also no
	// codeartifact dependency). Neither defers on token expiry, so
	// neither needs a replay adapter.
	replayRules := map[string]supplydeferral.ReplayableRule{
		"SUPPLY-001": NewReplayAdapter(s001),
		"SUPPLY-003": NewReplayAdapter(s003),
		"SUPPLY-004": NewReplayAdapter(s004),
	}
	RegisterSupplyRecheckDeps(&SupplyRecheckDeps{
		CA:           caClient,
		Rules:        replayRules,
		RepoResolver: DefaultRepoResolver(db),
	})
	return nil
}

// ── ReplayableRule adapter (production owner) ──────────────────────────────

// ReplayAdapter wraps an isb.ManifestGatedRule into the loosely-typed
// supplydeferral.ReplayableRule shape. The supplydeferral package
// deliberately defines its own ReplayInput / ReplayFinding types to
// avoid importing internal/isb (which would create an import cycle:
// rules → supplydeferral via RecordDeferral; if supplydeferral imported
// isb, the cycle would close). This adapter lives in internal/agents
// because it's the package that owns the dog deps + cmd/force already
// imports it for daemon wiring.
//
// The adapter is stateless — it only forwards calls — so a single
// concrete adapter type wraps every rule. Callers construct one
// adapter per rule via NewReplayAdapter.
type ReplayAdapter struct {
	rule isb.ManifestGatedRule
}

// NewReplayAdapter constructs a ReplayAdapter wrapping r.
func NewReplayAdapter(r isb.ManifestGatedRule) *ReplayAdapter {
	return &ReplayAdapter{rule: r}
}

// ID implements supplydeferral.ReplayableRule.
func (a *ReplayAdapter) ID() string { return a.rule.ID() }

// Run implements supplydeferral.ReplayableRule. It translates a
// ReplayInput → isb.ManifestGatedInput, invokes the wrapped rule, and
// translates the resulting []isb.Finding back into []ReplayFinding.
//
// Field mapping (ReplayInput → ManifestGatedInput):
//
//	in.SourceTaskID         → mgIn.SourceTaskID
//	in.TargetRepo           → mgIn.TargetRepo
//	in.Branch               → mgIn.Branch
//	in.CommitSHA            → mgIn.CommitSHA
//	in.ChangedManifests[i]  → mgIn.ChangedManifests[i] with:
//	    .Path               → .Path
//	    .Ecosystem          → .Ecosystem
//	    .DepsAdded          → .DepsAdded
//	    .After              → .AfterBytes
//	    (ReplayInput has no DepsRemoved or BeforeBytes — replay reads
//	     the branch tip's CURRENT state only; rules that key off "what
//	     was added this commit" become "what is in the manifest now"
//	     during replay, which matches the replay semantics: the
//	     question is "is the current tip clean?" not "what changed
//	     between commits?")
func (a *ReplayAdapter) Run(ctx context.Context, db *sql.DB, in supplydeferral.ReplayInput) ([]supplydeferral.ReplayFinding, error) {
	mgIn := isb.ManifestGatedInput{
		SourceTaskID: in.SourceTaskID,
		TargetRepo:   in.TargetRepo,
		Branch:       in.Branch,
		CommitSHA:    in.CommitSHA,
	}
	for _, cm := range in.ChangedManifests {
		mgIn.ChangedManifests = append(mgIn.ChangedManifests, isb.ChangedManifest{
			Path:       cm.Path,
			Ecosystem:  cm.Ecosystem,
			DepsAdded:  cm.DepsAdded,
			AfterBytes: cm.After,
		})
	}
	findings, err := a.rule.Run(ctx, db, mgIn)
	if err != nil {
		return nil, err
	}
	out := make([]supplydeferral.ReplayFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, supplydeferral.ReplayFinding{
			RuleID:   f.RuleID,
			Severity: string(f.Severity),
			Path:     f.Path,
			Line:     f.Line,
			Message:  f.Message,
		})
	}
	return out, nil
}

// Compile-time assertion: ReplayAdapter implements supplydeferral.ReplayableRule.
var _ supplydeferral.ReplayableRule = (*ReplayAdapter)(nil)
