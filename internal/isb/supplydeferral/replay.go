// supplydeferral.replay — re-execute deferred SUPPLY-* findings once
// CodeArtifact recovers (D5 Phase 4 Slice β).
//
// The dog `supply-token-recheck` (every 30 min) calls
// ReplayPendingDeferrals after a successful Health probe. The
// ConvoyReview gate (Slice γ) calls ReplayPendingDeferralsForBranch
// inline at DraftPROpen so a recovered token immediately resolves the
// gate.
//
// Per docs/roadmap.md § D5 "Layer 1 — supply-token-recheck dog":
//   - For each branch: read the **current tip's** manifest files (latest
//     state, not historical commit — the current tip subsumes intermediate
//     churn).
//   - Re-run SUPPLY-* against the resolved dep set.
//   - **Now clean** → flip original row to disposition='resolved_late'.
//   - **Now flagged** → insert new disposition='block' row + flip
//     original to disposition='superseded'.
//   - **Branch missing** (deleted/rebased) → flip original to
//     disposition='branch_gone'.
//
// Design choice — ReplayableRule interface lives HERE (not inside
// internal/isb) so this package stays free of import cycles. The dog
// (in internal/agents) wraps each registered SUPPLY-* rule in a thin
// adapter that translates ReplayInput → isb.ManifestGatedInput. Rule
// authors don't have to know about replay; the adapter is owned by the
// caller.
//
// Cycle-avoidance recap (mirrors deferral.go's note): the rules
// package imports supplydeferral for RecordDeferral; supplydeferral
// must NOT import internal/isb or internal/isb/rules. Reading manifest
// content + branch tips happens through internal/git (which does not
// import isb), and parsing happens through internal/isb/scanners/manifests
// (a sibling sub-package outside internal/isb itself).

package supplydeferral

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/store"
)

// ── Public types ──────────────────────────────────────────────────────────

// ReplayResult is the outcome per deferred row. One DeferralRow → one
// ReplayResult, regardless of outcome.
type ReplayResult struct {
	OriginalFindingID int    // SecurityFindings row that was deferred
	Branch            string
	RuleKey           string
	ManifestPath      string
	Outcome           string // ReplayOutcome*
	NewFindingID      int    // populated when Outcome == ReplayOutcomeStillFlagged
	Reason            string // human-readable summary for the operator ping
}

// Outcome constants. Strings rather than an iota enum so the values
// round-trip through dashboards / logs unchanged.
const (
	ReplayOutcomeResolvedLate  = "resolved_late"
	ReplayOutcomeStillFlagged  = "still_flagged"
	ReplayOutcomeBranchMissing = "branch_missing"
)

// Disposition values written by the replay path.
const (
	DispositionResolvedLate = "resolved_late"
	DispositionSuperseded   = "superseded"
	DispositionBranchGone   = "branch_gone"
	DispositionBlock        = "block"
)

// Logger is the minimal log interface every dog already conforms to.
// Defined locally so this package stays decoupled from internal/agents.
type Logger interface {
	Printf(format string, args ...any)
}

// ReplayableRule is the contract a SUPPLY-* rule must satisfy to be
// replayed. It is loosely-typed (ReplayInput / ReplayFinding) so this
// package does not import internal/isb. The dog/gate wraps real rules
// in a tiny adapter at the call site.
type ReplayableRule interface {
	// ID returns the canonical rule key, e.g. "SUPPLY-001".
	ID() string

	// Run re-executes the rule against the supplied input. Returns
	// ([]ReplayFinding, error). A non-nil error is propagated as a
	// per-row error in the result aggregation; partial findings still
	// flow back.
	Run(ctx context.Context, db *sql.DB, in ReplayInput) ([]ReplayFinding, error)
}

// ReplayInput is the subset of isb.ManifestGatedInput needed for replay.
// Fields are populated from the current branch-tip state (not the
// historical commit that originally deferred).
type ReplayInput struct {
	ChangedManifests []ReplayChangedManifest
	Branch           string
	CommitSHA        string
	TargetRepo       string
	SourceTaskID     int
}

// ReplayChangedManifest mirrors isb.ChangedManifest minus the BeforeBytes
// (replay reads the current state only — no diff is needed).
type ReplayChangedManifest struct {
	Path      string
	Ecosystem manifests.Ecosystem
	DepsAdded []manifests.Dependency
	After     []byte
}

// ReplayFinding mirrors isb.Finding without dragging in the isb package.
type ReplayFinding struct {
	RuleID   string
	Severity string // "advise" | "block"
	Path     string
	Line     int
	Message  string
}

// RepoResolver maps a deferred SecurityFinding's task_id to the on-disk
// repository path used to read the branch tip. The dog provides the
// production resolver (BountyBoard.target_repo → Repositories.local_path);
// tests pass a closure pointing at a fixture tempdir.
//
// Returning ("", nil) is treated as "branch is unresolvable for this
// row, skip" (the row is logged but not flipped — the operator can
// inspect the reference). Returning a non-nil error is fatal for that
// row — the iteration continues, and the error is collected via
// errors.Join in the aggregate return.
type RepoResolver func(taskID int) (repoPath string, err error)

// ── Public entry points ──────────────────────────────────────────────────

// ReplayPendingDeferrals walks every SecurityFindings row matching
// disposition='token_expired' AND bureau='isb' AND rule_id LIKE 'SUPPLY-%'
// and re-runs the originating rule against the branch tip's current
// manifest. See package comment for the per-outcome flip semantics.
//
// rules is the dispatch table the caller assembles at startup. Missing
// rule IDs in the map cause the row to be skipped (with a log line) —
// they don't fail the whole sweep.
//
// Returns one ReplayResult per processed row plus errors.Join over any
// per-row failures. Caller-level (db nil, etc.) failures return
// (nil, err) directly.
func ReplayPendingDeferrals(ctx context.Context, db *sql.DB, repos RepoResolver, rules map[string]ReplayableRule, logger Logger) ([]ReplayResult, error) {
	return replay(ctx, db, "", repos, rules, logger)
}

// ReplayPendingDeferralsForBranch is the branch-scoped form used by the
// ConvoyReview gate. It only processes rows whose payload.Branch matches
// branch. branch must be non-empty.
func ReplayPendingDeferralsForBranch(ctx context.Context, db *sql.DB, branch string, repos RepoResolver, rules map[string]ReplayableRule, logger Logger) ([]ReplayResult, error) {
	if branch == "" {
		return nil, errors.New("ReplayPendingDeferralsForBranch: branch required")
	}
	return replay(ctx, db, branch, repos, rules, logger)
}

// ── Implementation ───────────────────────────────────────────────────────

func replay(ctx context.Context, db *sql.DB, branchFilter string, repos RepoResolver, rules map[string]ReplayableRule, logger Logger) ([]ReplayResult, error) {
	if db == nil {
		return nil, errors.New("supplydeferral.replay: nil db")
	}
	if repos == nil {
		return nil, errors.New("supplydeferral.replay: nil RepoResolver")
	}
	if logger == nil {
		// Logger is optional; provide a no-op to avoid nil derefs.
		logger = noopLogger{}
	}

	rows, err := ListPendingDeferrals(db, branchFilter)
	if err != nil {
		return nil, fmt.Errorf("supplydeferral.replay: list: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	// Group by branch so we read each branch tip's manifests once even
	// when multiple deferrals land on the same branch. Per-row outcomes
	// are still emitted 1:1 — the grouping is purely an I/O optimisation.
	byBranch := map[string][]DeferralRow{}
	for _, r := range rows {
		byBranch[r.Payload.Branch] = append(byBranch[r.Payload.Branch], r)
	}
	// Stable iteration so the ReplayResult slice is deterministic in tests.
	branches := make([]string, 0, len(byBranch))
	for b := range byBranch {
		branches = append(branches, b)
	}
	sort.Strings(branches)

	var (
		out  []ReplayResult
		errs []error
	)

	for _, branch := range branches {
		select {
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("supplydeferral.replay: ctx cancelled at branch %s: %w", branch, ctx.Err()))
			break
		default:
		}
		group := byBranch[branch]

		// All rows in a group share a branch; pick the first row's
		// task_id to resolve the repo path. If different rows in the
		// group resolve to different repos (shouldn't happen in
		// practice — a branch lives in one repo) we still process the
		// group against the first repo's tip, which is the right
		// behaviour: a stale row pointing at a deleted task simply
		// surfaces as branch_missing.
		repoPath, repoErr := repos(group[0].TaskID)
		if repoErr != nil {
			errs = append(errs, fmt.Errorf("supplydeferral.replay: branch %s: resolve repo for task %d: %w", branch, group[0].TaskID, repoErr))
			continue
		}
		if repoPath == "" {
			logger.Printf("supplydeferral.replay: branch %s: empty repo path for task %d — skipping", branch, group[0].TaskID)
			continue
		}

		// Branch-existence probe. ReadFileAtRef returns (nil, nil)
		// both for "branch missing" AND "file missing on branch", so
		// we use rev-parse first to disambiguate.
		if !branchExists(ctx, repoPath, branch) {
			for _, row := range group {
				if flipErr := store.SetDisposition(db, row.FindingID, DispositionBranchGone, "", ""); flipErr != nil {
					errs = append(errs, fmt.Errorf("supplydeferral.replay: flip branch_gone for finding %d: %w", row.FindingID, flipErr))
					continue
				}
				out = append(out, ReplayResult{
					OriginalFindingID: row.FindingID,
					Branch:            branch,
					RuleKey:           row.Payload.RuleKey,
					ManifestPath:      row.Payload.ManifestPath,
					Outcome:           ReplayOutcomeBranchMissing,
					Reason:            "branch deleted or rebased away — original deferral closed as branch_gone",
				})
			}
			logger.Printf("supplydeferral.replay: branch %s missing from %s — flipped %d row(s) to branch_gone", branch, repoPath, len(group))
			continue
		}

		// Resolve the branch-tip SHA so the new finding (still_flagged
		// path) can record the correct commit. Best-effort — empty
		// fallback is acceptable.
		tipSHA := branchTipSHA(ctx, repoPath, branch)

		// One row at a time so a parser error on file A doesn't sink
		// row B. The cost of re-reading the same manifest path twice
		// in a group is negligible (cached by the OS) compared with
		// the simplicity of per-row error scoping.
		for _, row := range group {
			result, perRowErr := replayOneRow(ctx, db, repoPath, branch, tipSHA, row, rules, logger)
			if perRowErr != nil {
				errs = append(errs, perRowErr)
				// Continue on partial failure (per spec: "partial
				// failures are collected and returned via
				// errors.Join along with the results slice").
				continue
			}
			out = append(out, result)
		}
	}

	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// replayOneRow performs the per-row replay. Returns the outcome row to
// append to results. A non-nil error indicates the row failed to
// process (different from "rule fired" — that's a normal
// still_flagged outcome).
func replayOneRow(ctx context.Context, db *sql.DB, repoPath, branch, tipSHA string, row DeferralRow, rules map[string]ReplayableRule, logger Logger) (ReplayResult, error) {
	rule, ok := rules[row.Payload.RuleKey]
	if !ok || rule == nil {
		// Missing replay adapter for this rule — log and skip
		// (no flip — operator must register the adapter).
		logger.Printf("supplydeferral.replay: no ReplayableRule registered for %s — leaving finding %d as token_expired",
			row.Payload.RuleKey, row.FindingID)
		return ReplayResult{}, fmt.Errorf("no ReplayableRule registered for %s (finding %d)", row.Payload.RuleKey, row.FindingID)
	}

	// Read the manifest at the branch tip. ReadFileAtRef returns
	// (nil, nil) when the file no longer exists at tip — that's the
	// "deps removed in a follow-up commit" case which we treat as
	// resolved_late (rule has nothing to flag).
	content, _ := igit.ReadFileAtRef(ctx, repoPath, branch, row.Payload.ManifestPath)

	parser, parserOK := manifests.Default().Detect(row.Payload.ManifestPath)
	if !parserOK {
		// Manifest path no longer recognised — should never happen
		// for a SUPPLY-* deferral that originally fired through the
		// dispatcher, but defend anyway.
		return ReplayResult{}, fmt.Errorf("supplydeferral.replay: no parser for manifest %q (finding %d)", row.Payload.ManifestPath, row.FindingID)
	}

	var depsAtTip []manifests.Dependency
	if len(content) > 0 {
		parsed, parseErr := parser.Parse(row.Payload.ManifestPath, content)
		if parseErr != nil {
			return ReplayResult{}, fmt.Errorf("supplydeferral.replay: parse %q at branch %s: %w (finding %d)", row.Payload.ManifestPath, branch, parseErr, row.FindingID)
		}
		depsAtTip = parsed
	}

	eco, _ := manifests.Default().EcosystemFor(row.Payload.ManifestPath)

	in := ReplayInput{
		Branch:       branch,
		CommitSHA:    tipSHA,
		TargetRepo:   "", // not needed by current SUPPLY-* rules; dog can populate via adapter if a rule needs it
		SourceTaskID: row.TaskID,
		ChangedManifests: []ReplayChangedManifest{
			{
				Path:      row.Payload.ManifestPath,
				Ecosystem: eco,
				DepsAdded: depsAtTip,
				After:     content,
			},
		},
	}

	findings, runErr := rule.Run(ctx, db, in)
	if runErr != nil {
		return ReplayResult{}, fmt.Errorf("supplydeferral.replay: rule %s on finding %d: %w", row.Payload.RuleKey, row.FindingID, runErr)
	}

	// Filter findings to only those for THIS rule + this manifest path.
	// A rule may emit a Finding with a different path (rare); the
	// replay's contract is "did THIS deferred row's situation get fixed?",
	// which corresponds to findings on the same manifest.
	var relevant []ReplayFinding
	for _, f := range findings {
		if f.RuleID != row.Payload.RuleKey {
			continue
		}
		if f.Path != "" && f.Path != row.Payload.ManifestPath {
			continue
		}
		relevant = append(relevant, f)
	}

	if len(relevant) == 0 {
		// Rule no longer fires → resolved_late.
		if flipErr := store.SetDisposition(db, row.FindingID, DispositionResolvedLate, "", ""); flipErr != nil {
			return ReplayResult{}, fmt.Errorf("supplydeferral.replay: flip resolved_late for finding %d: %w", row.FindingID, flipErr)
		}
		logger.Printf("supplydeferral.replay: %s on %s@%s now clean — finding %d → resolved_late",
			row.Payload.RuleKey, row.Payload.ManifestPath, branch, row.FindingID)
		return ReplayResult{
			OriginalFindingID: row.FindingID,
			Branch:            branch,
			RuleKey:           row.Payload.RuleKey,
			ManifestPath:      row.Payload.ManifestPath,
			Outcome:           ReplayOutcomeResolvedLate,
			Reason:            "rule no longer fires against branch tip",
		}, nil
	}

	// Rule still fires → insert a fresh block row that records the
	// (rule, manifest, message) at the new tip, then flip the original
	// to superseded. The new row carries a structured payload (JSON
	// list of fired-finding messages) in `message` so the operator
	// pings can render the precise reason.
	primary := relevant[0]
	body := encodeReplayMessage(row.Payload.RuleKey, branch, row.Payload.ManifestPath, relevant)
	newID, insErr := store.InsertSecurityFinding(db, store.SecurityFinding{
		TaskID:    row.TaskID,
		Bureau:    "isb",
		RuleID:    row.Payload.RuleKey,
		Severity:  "block",
		FilePath:  row.Payload.ManifestPath,
		Message:   body,
		CommitSHA: tipSHA,
		// Disposition '' (no-op default) → the new row enters the
		// 'open' bucket so ConvoyReview's normal block-eval picks it up.
	})
	if insErr != nil {
		return ReplayResult{}, fmt.Errorf("supplydeferral.replay: insert block for finding %d: %w", row.FindingID, insErr)
	}
	if flipErr := store.SetDisposition(db, row.FindingID, DispositionSuperseded, "", ""); flipErr != nil {
		// Roll back the freshly-inserted block row so we don't leave
		// a duplicate with no superseded link. Best-effort.
		_, _ = db.Exec(`DELETE FROM SecurityFindings WHERE id = ?`, newID)
		return ReplayResult{}, fmt.Errorf("supplydeferral.replay: flip superseded for finding %d: %w", row.FindingID, flipErr)
	}
	logger.Printf("supplydeferral.replay: %s on %s@%s still firing — finding %d → superseded; new block %d",
		row.Payload.RuleKey, row.Payload.ManifestPath, branch, row.FindingID, newID)
	return ReplayResult{
		OriginalFindingID: row.FindingID,
		Branch:            branch,
		RuleKey:           row.Payload.RuleKey,
		ManifestPath:      row.Payload.ManifestPath,
		Outcome:           ReplayOutcomeStillFlagged,
		NewFindingID:      newID,
		Reason:            primary.Message,
	}, nil
}

// branchExists returns true when `git rev-parse --verify <branch>` succeeds.
func branchExists(ctx context.Context, repoPath, branch string) bool {
	if repoPath == "" || branch == "" {
		return false
	}
	out, err := igit.RunCmd(ctx, repoPath, "rev-parse", "--verify", "--quiet", branch+"^{commit}")
	if err != nil {
		return false
	}
	return len(out) > 0
}

// branchTipSHA returns the branch's current commit SHA, or "" on error.
func branchTipSHA(ctx context.Context, repoPath, branch string) string {
	if repoPath == "" || branch == "" {
		return ""
	}
	out, err := igit.RunCmd(ctx, repoPath, "rev-parse", branch)
	if err != nil {
		return ""
	}
	// strip trailing newline
	if n := len(out); n > 0 && out[n-1] == '\n' {
		return out[:n-1]
	}
	return out
}

// encodeReplayMessage serialises the still_flagged findings into a
// stable JSON payload. The first finding's Message is the primary
// reason; the full slice is preserved for forensic inspection from
// the dashboard.
func encodeReplayMessage(ruleKey, branch, manifestPath string, findings []ReplayFinding) string {
	type item struct {
		Message string `json:"message"`
		Line    int    `json:"line,omitempty"`
	}
	body := struct {
		RuleKey      string    `json:"rule_key"`
		Branch       string    `json:"branch"`
		ManifestPath string    `json:"manifest_path"`
		ReplayedAt   time.Time `json:"replayed_at"`
		Findings     []item    `json:"findings"`
	}{
		RuleKey:      ruleKey,
		Branch:       branch,
		ManifestPath: manifestPath,
		ReplayedAt:   time.Now().UTC(),
		Findings:     make([]item, 0, len(findings)),
	}
	for _, f := range findings {
		body.Findings = append(body.Findings, item{Message: f.Message, Line: f.Line})
	}
	out, err := json.Marshal(body)
	if err != nil {
		// Fallback: just the primary message. Should never happen.
		return findings[0].Message
	}
	return string(out)
}

// noopLogger silences the per-call log output when the caller passes nil.
type noopLogger struct{}

func (noopLogger) Printf(string, ...any) {}
