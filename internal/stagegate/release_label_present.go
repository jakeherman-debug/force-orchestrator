package stagegate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// PRLabelFetcher is the narrow capability the release_label_present gate
// needs from a GitHub client: fetch the label-name list for a single PR.
// Pulling out a tiny interface (rather than depending on *gh.Client
// directly) lets tests inject a stub without spinning up a real gh
// runner, and keeps this file's dependency surface honest about what it
// actually consumes — the rest of *gh.Client is irrelevant to label
// polling.
//
// *gh.Client satisfies this interface via its PRLabels method.
type PRLabelFetcher interface {
	PRLabels(cwd, repo string, number int) ([]string, error)
}

// Compile-time check: *gh.Client must satisfy PRLabelFetcher so wiring
// the production daemon (D5.5 P3 W3 ζ) is a one-liner that doesn't drift.
var _ PRLabelFetcher = (*gh.Client)(nil)

// ReleaseLabelPresent is the D5.5 P3 advanced leaf gate that polls each
// merged PR in a stage and asserts that at least one of its labels matches
// the per-repo release_label_pattern regex stored in Repositories. The
// gate only passes when EVERY merged PR in the stage carries a matching
// label. PRs without a match keep the gate in pending — they're presumed
// "not yet released."
//
// Why per-repo and not gate-config: the release-label naming convention
// is a property of the repo, not the convoy stage. monorepo "release/v1.2"
// vs microservice "released-prod" vs library "v0.5.0" — same gate type,
// different patterns. Storing the pattern on Repositories lets a single
// release_label_present gate spec apply uniformly across multi-repo
// stages without the planner having to hand-roll a pattern per repo in
// the gate config.
//
// Config shape (JSON object stored in ConvoyStages.gate_config_json):
//
//	{
//	  "polling_interval_minutes": 5
//	}
//
// The polling_interval_minutes field is informational/documentary; the
// convoy-stage-watch dog already runs on its own tick cadence and the gate
// itself is stateless. Per the roadmap spec, the pattern lives on
// Repositories.release_label_pattern, not the gate config.
//
// Evaluation contract:
//   - Any merged PR's repo has empty release_label_pattern → structural
//     error (planner-time validation should have caught it; runtime is the
//     defensive backstop).
//   - Any merged PR has no label matching its repo's pattern → ErrPending.
//     A release rollout is in flight; check again next tick.
//   - gh fetch error → ErrPending. Network blips shouldn't fail the stage.
//   - Every merged PR has at least one matching label → passed=true.
type ReleaseLabelPresent struct {
	ghClient PRLabelFetcher
}

// NewReleaseLabelPresent constructs the gate with an injected gh client.
// Production wires a *gh.Client; tests pass a stub satisfying
// PRLabelFetcher. Panics on nil — a misconfigured registry should fail at
// startup, not silently no-op every release.
func NewReleaseLabelPresent(client PRLabelFetcher) ReleaseLabelPresent {
	if client == nil {
		panic("stagegate: NewReleaseLabelPresent requires a non-nil PRLabelFetcher")
	}
	return ReleaseLabelPresent{ghClient: client}
}

// Type implements Gate.
func (ReleaseLabelPresent) Type() string { return "release_label_present" }

// Evaluate implements Gate.
func (r ReleaseLabelPresent) Evaluate(_ context.Context, db *sql.DB, stage StageContext) (bool, string, error) {
	if r.ghClient == nil {
		return false, "", errors.New("release_label_present: gh client not wired (use NewReleaseLabelPresent)")
	}
	branches := store.ListConvoyAskBranchesByStage(db, stage.ConvoyID, stage.StageID)
	if len(branches) == 0 {
		// No ask-branches for this stage — either the stage hasn't yet
		// produced any PRs (still pending) or it's a misconfigured stage.
		// The convoy-stage-watch dog only enters AwaitingGate after
		// AllPRsMerged, so reaching here with zero branches is a wiring
		// bug; surface as an error so it gets fixed.
		return false, "", fmt.Errorf("release_label_present: no ask-branches found for convoy=%d stage_id=%d (gate cannot evaluate empty stage)", stage.ConvoyID, stage.StageID)
	}

	// Pre-compile patterns per repo so we don't recompile on every iteration
	// (and so a bad pattern surfaces with the offending repo's name).
	type repoCheck struct {
		branch  store.ConvoyAskBranch
		pattern string
		re      *regexp.Regexp
	}
	checks := make([]repoCheck, 0, len(branches))
	for _, b := range branches {
		if b.DraftPRState != "Merged" {
			// Defensive: the dog should not enter AwaitingGate until
			// every PR in the stage is merged. If we somehow get here
			// with an unmerged PR, stay pending so the dog re-checks.
			return false, fmt.Sprintf("release_label_present: PR for repo %s not yet merged (state=%q)", b.Repo, b.DraftPRState), ErrPending
		}
		pattern, err := store.GetRepositoryReleaseLabelPattern(db, b.Repo)
		if err != nil {
			return false, "", fmt.Errorf("release_label_present: get pattern for %s: %w", b.Repo, err)
		}
		if pattern == "" {
			// Planner should have rejected this gate at convoy-creation;
			// runtime treats it as a structural misconfiguration error,
			// NOT pending — operator must fix it (set the pattern or
			// pick a different gate).
			return false, "", fmt.Errorf("release_label_present: repo %s has no release_label_pattern configured (gate misconfigured — set the pattern or choose a different gate)", b.Repo)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return false, "", fmt.Errorf("release_label_present: invalid pattern %q for repo %s: %w", pattern, b.Repo, err)
		}
		checks = append(checks, repoCheck{branch: b, pattern: pattern, re: re})
	}

	var pendingReasons []string
	for _, c := range checks {
		labels, err := r.ghClient.PRLabels("", c.branch.Repo, c.branch.DraftPRNumber)
		if err != nil {
			// gh errors are transient — retry next tick. The dog's
			// timeout-escalation path catches the case where the error
			// persists indefinitely.
			pendingReasons = append(pendingReasons, fmt.Sprintf("%s#%d: gh error: %v", c.branch.Repo, c.branch.DraftPRNumber, err))
			continue
		}
		matched := false
		for _, l := range labels {
			if c.re.MatchString(l) {
				matched = true
				break
			}
		}
		if !matched {
			pendingReasons = append(pendingReasons, fmt.Sprintf("%s#%d: no label matches /%s/", c.branch.Repo, c.branch.DraftPRNumber, c.pattern))
		}
	}
	if len(pendingReasons) > 0 {
		return false, fmt.Sprintf("release_label_present pending: %s", strings.Join(pendingReasons, "; ")), ErrPending
	}
	return true, fmt.Sprintf("release_label_present: all %d merged PRs carry matching labels", len(checks)), nil
}
