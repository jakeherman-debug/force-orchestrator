// dogs_supply_token_recheck.go — D5 Phase 4 Slice β.
//
// Probes CodeArtifact every 30 min via codeartifact.Client.Health.
// On a 401 (errors.Is(err, codeartifact.ErrTokenExpired)) the dog
// debounces a single notify-after Slack ping per session — repeat
// failures during a long outage don't spam the operator. On a 200,
// the dog calls supplydeferral.ReplayPendingDeferrals against the
// production rule registry and emits a per-branch Slack ping
// summarising the outcomes.
//
// State management:
//
//   SystemConfig.supply_token_expired_notified — '1' while we're
//   waiting on the operator. Set on first detection of an expired
//   token, cleared on the next successful Health probe. The
//   recovery transition (→ Health OK with the flag set) doubles as
//   the trigger for the replay sweep — even if the operator hasn't
//   manually flipped the flag, the next 30-min tick that sees a
//   healthy CodeArtifact will run the replay and clear the flag.
//
// Dispatch wiring:
//
//   The codeartifact.Client + the per-rule replay adapter map are
//   constructed at daemon startup and registered via
//   RegisterSupplyRecheckDeps. RunDogByName / the inquisitor's
//   RunDogs both go through this single entry point. Tests inject
//   their own deps via the same setter — the package var is the
//   test-seam, not magic.
//
//   When deps are NOT registered (e.g. a daemon spun up without the
//   D5 Slice δ wiring landed yet), the dog logs a one-line warning
//   and returns nil — it does NOT escalate to the operator. The
//   point of the dog is to recover from token expiry; if the dog
//   itself is unwired, that's a config bug for the operator's
//   regular dashboard, not a fleet-mail-worthy alert.
//
// notifyAfterFn is a package-var indirection so tests can stub the
// notify-after side effect. Default fires the long-running-notifier
// shell script with `--` `true` (a no-op command) so the wrapper just
// posts the label as the Slack message and exits.

package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/store"
)

// ── Dependency injection ─────────────────────────────────────────────────

// SupplyRecheckDeps bundles the inputs the supply-token-recheck dog
// needs. The daemon constructs this once at startup and registers it
// via RegisterSupplyRecheckDeps; the dog reads it via getSupplyRecheckDeps.
type SupplyRecheckDeps struct {
	// CA is the CodeArtifact client used for the health probe.
	CA codeartifact.Client

	// Rules is the per-rule replay adapter map. Keys are rule IDs
	// ("SUPPLY-001" .. "SUPPLY-005"); values are adapters that wrap
	// the live ManifestGatedRule into the supplydeferral.ReplayableRule
	// shape.
	Rules map[string]supplydeferral.ReplayableRule

	// RepoResolver maps a deferred row's task_id → repo path on disk.
	// The default production resolver looks up
	// BountyBoard.target_repo → Repositories.local_path. Tests inject
	// a tempdir-backed closure.
	RepoResolver supplydeferral.RepoResolver
}

var (
	supplyDepsMu sync.RWMutex
	supplyDeps   *SupplyRecheckDeps
)

// RegisterSupplyRecheckDeps is the daemon-side setter. Call once at
// startup with the live codeartifact.Client + rule registry. Passing
// nil clears the registration (used by tests for cleanup).
func RegisterSupplyRecheckDeps(deps *SupplyRecheckDeps) {
	supplyDepsMu.Lock()
	defer supplyDepsMu.Unlock()
	supplyDeps = deps
}

// getSupplyRecheckDeps returns the registered deps, or nil when none.
func getSupplyRecheckDeps() *SupplyRecheckDeps {
	supplyDepsMu.RLock()
	defer supplyDepsMu.RUnlock()
	return supplyDeps
}

// GetSupplyRecheckDeps is the exported read-only accessor used by the
// daemon-side wiring regression tests (D5 fix-loop iter 1 slice α). It
// returns the same value as the internal getSupplyRecheckDeps; tests
// that only need to assert "the daemon registered the deps" use this
// instead of reaching into the package var directly.
func GetSupplyRecheckDeps() *SupplyRecheckDeps {
	return getSupplyRecheckDeps()
}

// DefaultRepoResolver is the production RepoResolver: BountyBoard
// row → target_repo → Repositories.local_path. Returns ("", nil) when
// the task or repo isn't registered (treated as "skip" by the replay
// helper rather than a hard error).
func DefaultRepoResolver(db *sql.DB) supplydeferral.RepoResolver {
	return func(taskID int) (string, error) {
		var targetRepo string
		if err := db.QueryRow(`SELECT IFNULL(target_repo, '') FROM BountyBoard WHERE id = ?`, taskID).Scan(&targetRepo); err != nil {
			// Task may have been deleted. Treat as soft-skip: ""
			// triggers the replay helper's "skip with log" path.
			return "", nil
		}
		if targetRepo == "" {
			return "", nil
		}
		return store.GetRepoPath(db, targetRepo), nil
	}
}

// ── Notify-after seam ────────────────────────────────────────────────────

// notifyAfterFn is the test seam for the operator-ping side effect.
// The default implementation shells out to the long-running-notifier
// binary; tests override this var to capture invocations without
// touching the real Slack webhook.
var notifyAfterFn = realNotifyAfter

// realNotifyAfter shells `notify-after "<label>" -- true`. Best-effort:
// if the binary isn't on PATH (e.g. on CI without the plugin), the
// call is a no-op and the dog logs a warning. We do NOT propagate
// notify-after's exit code — the dog's primary purpose is the replay
// sweep; webhook delivery is a UX bonus.
func realNotifyAfter(ctx context.Context, label string) error {
	bin, err := exec.LookPath("notify-after")
	if err != nil {
		// Helper not installed — silently no-op. Log line below is
		// emitted by the dog itself so this stays out of the env's
		// noisy "binary missing" tracks.
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, label, "--", "true")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify-after: %w", err)
	}
	return nil
}

// ── State helpers (debounce flag) ────────────────────────────────────────

// supplyTokenNotifiedKey is the SystemConfig key that records "we
// already pinged the operator about token expiry this session." Set on
// first ErrTokenExpired detection, cleared on the next successful
// Health probe.
const supplyTokenNotifiedKey = "supply_token_expired_notified"

func supplyTokenAlreadyNotified(db *sql.DB) bool {
	return store.GetConfig(db, supplyTokenNotifiedKey, "") == "1"
}

func markSupplyTokenNotified(db *sql.DB) {
	store.SetConfig(db, supplyTokenNotifiedKey, "1")
}

func clearSupplyTokenNotified(db *sql.DB) {
	store.SetConfig(db, supplyTokenNotifiedKey, "")
}

// ── Dog body ─────────────────────────────────────────────────────────────

// dogSupplyTokenRecheck is the cooldown-driven entry. Reads deps from
// the package-level registry; behavioural sub-helpers are factored
// out for testability.
func dogSupplyTokenRecheck(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	deps := getSupplyRecheckDeps()
	if deps == nil {
		logger.Printf("Dog supply-token-recheck: deps not registered (RegisterSupplyRecheckDeps never called) — skipping")
		return nil
	}
	return runSupplyTokenRecheck(ctx, db, deps, logger)
}

// runSupplyTokenRecheck is the testable core. Tests pass a stub
// SupplyRecheckDeps directly without touching the package var.
func runSupplyTokenRecheck(ctx context.Context, db *sql.DB, deps *SupplyRecheckDeps, logger interface{ Printf(string, ...any) }) error {
	if deps == nil || deps.CA == nil {
		return errors.New("supply-token-recheck: nil deps or codeartifact client")
	}

	// 1. Health probe.
	if err := deps.CA.Health(ctx); err != nil {
		if errors.Is(err, codeartifact.ErrTokenExpired) {
			// Debounced operator ping — only on the FIRST detection
			// per session. Subsequent ticks during the same outage
			// are silent.
			if supplyTokenAlreadyNotified(db) {
				logger.Printf("Dog supply-token-recheck: token still expired (already notified) — skipping replay")
				return nil
			}
			label := "[SUPPLY] umt artifacts token expired — re-auth needed for SUPPLY-* deferral replay"
			if notifyErr := notifyAfterFn(ctx, label); notifyErr != nil {
				logger.Printf("Dog supply-token-recheck: notify-after failed (continuing): %v", notifyErr)
			}
			markSupplyTokenNotified(db)
			logger.Printf("Dog supply-token-recheck: token expired — operator pinged via notify-after")
			return nil
		}
		// Other health-probe error class — surface to the operator
		// via the standard dog-failure mail path.
		return fmt.Errorf("supply-token-recheck: health probe failed: %w", err)
	}

	// 2. Health OK. Clear the debounce flag (so the next expiry
	// re-pings) before the replay so a replay-helper crash doesn't
	// leave the flag stuck.
	if supplyTokenAlreadyNotified(db) {
		clearSupplyTokenNotified(db)
		logger.Printf("Dog supply-token-recheck: codeartifact recovered — replay starting")
	}

	// 3. Replay deferred findings.
	if deps.RepoResolver == nil {
		return errors.New("supply-token-recheck: nil RepoResolver")
	}
	results, replayErr := supplydeferral.ReplayPendingDeferrals(ctx, db, deps.RepoResolver, deps.Rules, supplydeferralLogger{logger})

	// 4. Per-branch summary ping (best-effort; webhook failures don't
	// surface as dog errors). Group by branch so the operator gets
	// one ping per branch even when multiple rules + manifests are
	// involved.
	if len(results) > 0 {
		summarised := summariseReplayResults(results)
		for _, line := range summarised {
			label := fmt.Sprintf("[SUPPLY] %s", line)
			if err := notifyAfterFn(ctx, label); err != nil {
				logger.Printf("Dog supply-token-recheck: per-branch notify-after failed: %v", err)
			}
		}
	}

	// Surface the replay's joined errors (if any) as the dog's exit
	// status — the standard dog-mail path handles the operator alert.
	if replayErr != nil {
		return fmt.Errorf("supply-token-recheck: replay had partial failures: %w", replayErr)
	}
	if len(results) == 0 {
		logger.Printf("Dog supply-token-recheck: no pending deferrals — nothing to replay")
	} else {
		logger.Printf("Dog supply-token-recheck: replayed %d deferral(s)", len(results))
	}
	return nil
}

// summariseReplayResults groups results by (branch, outcome) so the
// per-branch operator ping reads naturally. The format is one line
// per (branch, outcome) tuple.
//
// Example output:
//   "feature/x: 2 resolved_late, 1 still_flagged (SUPPLY-001 evilpkg@0.0.1)"
//   "feature/old: 1 branch_missing"
func summariseReplayResults(results []supplydeferral.ReplayResult) []string {
	type key struct {
		branch  string
		outcome string
	}
	bucket := map[key][]supplydeferral.ReplayResult{}
	branchOrder := []string{}
	seenBranch := map[string]bool{}
	for _, r := range results {
		k := key{branch: r.Branch, outcome: r.Outcome}
		bucket[k] = append(bucket[k], r)
		if !seenBranch[r.Branch] {
			seenBranch[r.Branch] = true
			branchOrder = append(branchOrder, r.Branch)
		}
	}

	var out []string
	for _, branch := range branchOrder {
		// Re-bucket to a per-branch tally line.
		var (
			resolved []supplydeferral.ReplayResult
			flagged  []supplydeferral.ReplayResult
			missing  []supplydeferral.ReplayResult
		)
		for _, r := range results {
			if r.Branch != branch {
				continue
			}
			switch r.Outcome {
			case supplydeferral.ReplayOutcomeResolvedLate:
				resolved = append(resolved, r)
			case supplydeferral.ReplayOutcomeStillFlagged:
				flagged = append(flagged, r)
			case supplydeferral.ReplayOutcomeBranchMissing:
				missing = append(missing, r)
			}
		}
		var line string
		line = branch + ":"
		first := true
		appendPart := func(part string) {
			if first {
				line += " " + part
				first = false
			} else {
				line += ", " + part
			}
		}
		if len(resolved) > 0 {
			appendPart(fmt.Sprintf("%d resolved_late", len(resolved)))
		}
		if len(flagged) > 0 {
			// Embed the first still_flagged finding's rule + manifest
			// + reason for operator readability.
			primary := flagged[0]
			appendPart(fmt.Sprintf("%d still_flagged (%s %s — %s)", len(flagged), primary.RuleKey, primary.ManifestPath, primary.Reason))
		}
		if len(missing) > 0 {
			appendPart(fmt.Sprintf("%d branch_missing", len(missing)))
		}
		out = append(out, line)
	}
	return out
}

// supplydeferralLogger adapts the dog's logger interface (which uses
// `interface{ Printf(string, ...any) }`) to the supplydeferral
// package's Logger interface (same shape, separately declared to
// avoid the cross-package interface coupling).
type supplydeferralLogger struct {
	inner interface{ Printf(string, ...any) }
}

func (l supplydeferralLogger) Printf(format string, args ...any) {
	l.inner.Printf(format, args...)
}
