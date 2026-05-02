package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/clients/librarian"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// dogCooldowns defines how often each built-in dog may run.
var dogCooldowns = map[string]time.Duration{
	"git-hygiene":           30 * time.Minute,
	"db-vacuum":             6 * time.Hour,
	"holonet-rotate":        24 * time.Hour,
	"mail-cleanup":          12 * time.Hour,
	"memory-hygiene":        24 * time.Hour,
	"stalled-reviews":       6 * time.Hour,
	"priority-aging":        6 * time.Hour,
	"daily-digest":          24 * time.Hour,
	"stale-convoys-report":  12 * time.Hour,
	// sub-pr-ci-watch polls open sub-PRs for CI state + external closure.
	// Cooldown is 0 so it runs every inquisitor tick (5 min) — the underlying
	// `gh pr view` calls are cheap enough that this is the right cadence to
	// avoid blocking tasks waiting on auto-merge for more than 5 minutes.
	"sub-pr-ci-watch":       0,
	// main-drift-watch polls `git ls-remote` per ask-branch. The detection is
	// cheap (~50ms per repo), so 15 min is a generous cadence — rebases fire
	// only when main has actually moved.
	"main-drift-watch":      15 * time.Minute,
	// draft-pr-watch polls `gh pr view` per DraftPROpen convoy. Merged/Closed
	// transitions are cheap to detect; 5 min cadence is a reasonable balance
	// between timeliness for the operator and API rate-limit hygiene.
	"draft-pr-watch":        0, // every inquisitor tick
	// ship-it-nag sends operator reminders for aged draft PRs. Once per 6h is
	// plenty — the internal per-threshold dedupe prevents spam.
	"ship-it-nag":           6 * time.Hour,
	// repo-config-check revalidates remote URL, default branch, and PR template
	// path for every registered repo once a day.
	"repo-config-check":     24 * time.Hour,
	// pr-review-poll fetches bot/human review comments on each DraftPROpen
	// convoy's draft PR and queues PRReviewTriage tasks when new comments land.
	// 5-min cadence mirrors draft-pr-watch — bot comments typically arrive in
	// bursts minutes after the draft PR opens, so this matches their arrival pattern.
	"pr-review-poll":        5 * time.Minute,
	"convoy-review-watch":   5 * time.Minute,
	// pr-review-resolve sweeps in_scope_fix comments whose spawned CodeEdit has
	// Completed, calling the GraphQL resolveReviewThread mutation. Runs on every
	// inquisitor tick — batches are small and cost is two gh calls per resolve.
	"pr-review-resolve":     0,
	// escalation-sweeper auto-resolves Open escalations whose referenced sub-PR
	// is now terminal (Merged/Closed). Cheap — one JOIN query — and stops stale
	// operator burden when our own terminal-task-exit or an external merge has
	// already made the problem moot.
	"escalation-sweeper":    10 * time.Minute,
	// spend-burn-watch polls TaskHistory for trailing-hour spend. 5-min cadence
	// so the operator sees a cost spike within one inquisitor tick of it
	// landing. Auto-e-stop at $200/h makes the fleet self-contain any runaway
	// loop regardless of whether anyone is watching the dashboard.
	"spend-burn-watch":      5 * time.Minute,
	// task-spend-watch (D2 T1-1) polls TaskHistory per-task for trailing-10-min
	// spend; mails the operator on a soft alert and suspends the task on a
	// hard escalate. 5-min cadence matches spend-burn-watch — both cost
	// signals share the same operator-facing dashboard refresh granularity.
	"task-spend-watch": 5 * time.Minute,
	// D2 T1-4: quarantined-repo-watch surfaces operator mail when claim
	// loops have skipped tasks against quarantined repos. Daily cadence —
	// internal per-repo dedup prevents spam (one mail per repo per day).
	"quarantined-repo-watch": 24 * time.Hour,
	// D3 P3: disagreement-tracker computes cross-layer disagreement rates
	// (Captain→Council, Council→CI, ConvoyReview→astromech, operator-revert)
	// over rolling 7d/30d/90d windows. Hourly cadence — fast enough for
	// the operator to see new signal within the working hour, slow enough
	// that the SQL aggregations don't dominate the daemon's CPU budget.
	"disagreement-tracker":  1 * time.Hour,
	// D3 P6B.12 — weekly auto-render of the fleet learning panel.
	// 7-day cadence; re-running mid-week via the dashboard "Refresh
	// now" trigger is safe (the deterministic synth is cheap).
	"learning-panel-render": 7 * 24 * time.Hour,
	// D3 P6B.9 — daily transcript archival sweep. Bounded
	// (maxArchivesPerRun=1000); rows >30d OR for closed convoys >7d
	// get summarised + offloaded to ~/.force/transcripts/.
	"transcript-archive": 24 * time.Hour,
	// D3 fix-loop-1 (slice δ) — model-availability watch. Probes
	// distinct model_identifier values from TreatmentSpecs and
	// upserts ModelAvailability rows. 30-min cadence matches the
	// holdout-monitor's deferral note: "full availability watch
	// deferred to Phase 5/6". Default mode is record-only;
	// LIVE_HAIKU_DISABLED keeps tests deterministic.
	"model-availability-watch": 30 * time.Minute,
	// D3 fix-loop-1 β2 — proposed-features-decay decays stale
	// value_score from high → medium → low so old features don't
	// permanently rank highest in the ProposedFeatures queue
	// (concern #10 / exit criterion 14). 12-hour cadence — slow
	// enough to not churn live prioritisation; fast enough that a
	// 30-day-old high-value row demotes within a day after the
	// cutoff fires.
	"proposed-features-decay": 12 * time.Hour,
	// D4 Phase 0 — Librarian evolution dogs.
	// librarian-dedup-watch folds near-identical FleetMemory rows.
	// 12h is slow enough that the curator's outputs settle before
	// the next pass, fast enough that an operator-observed
	// duplicate doesn't sit in the index for a full day.
	"librarian-dedup-watch": 12 * time.Hour,
	// librarian-quality-recompute decays freshness_score so older
	// memories rank below newer ones. 24h matches the natural
	// freshness-half-life cadence (default 30 days; the per-run
	// effect is small, the cumulative effect is what matters).
	"librarian-quality-recompute": 24 * time.Hour,
	// librarian-conflict-watch surfaces contradictory memories as
	// operator-actionable tickets. 24h matches the dedup cycle.
	"librarian-conflict-watch": 24 * time.Hour,
	// librarian-hypothesis-emit walks high-signal memories and
	// emits candidate PromotionProposals. 24h aligns with EC's
	// natural ratification cadence.
	"librarian-hypothesis-emit": 24 * time.Hour,
	// claude-md-drift-watch scans CLAUDE.md invariants vs FleetRules
	// and emits drift candidates. Weekly — invariants don't change
	// daily and the scan reads CLAUDE.md + walks FleetRules.
	"claude-md-drift-watch": 7 * 24 * time.Hour,
	// D4 Phase 3 — senate-refresh. For every active Senator, call
	// librarian.RefreshSenatorMemoryDigest, append fresh
	// SenateMemory entries, and bump SenateChambers.last_refreshed_at.
	// Weekly cadence per docs/next-gen-agents.md § "Bootstrap +
	// refresh" — invariants don't drift daily, so the digest cost
	// amortises well at one pass per week.
	"senate-refresh": 7 * 24 * time.Hour,
	// D5 Phase 4 (slice α) — supply-allowlist-refresh. Walks the
	// per-ecosystem CodeArtifact repository (npm-prod / pypi-prod /
	// rubygems-prod / maven-prod), pulls the org's own pull-history
	// via aws codeartifact list-packages, and writes the sorted
	// newline-joined name set into SystemConfig.supply_allowlist_<eco>
	// for SUPPLY-002 typosquat detection. Daily cadence — the org's
	// pull history is slow-moving and ListPackages cost scales with
	// the org footprint. Token-expired per ecosystem is non-fatal
	// (the next tick retries); other errors are aggregated via
	// errors.Join so a single broken ecosystem doesn't sink the rest.
	"supply-allowlist-refresh": 24 * time.Hour,
}

// dogOrder determines the execution order of dogs within each inquisitor cycle.
// spend-burn-watch runs FIRST so that if it flips e-stop, subsequent dogs and
// any subsequent cycle's agent claims all see the halt immediately — no
// grace period for the fleet to continue burning tokens past the cap.
// task-spend-watch (D2 T1-1) runs IMMEDIATELY AFTER spend-burn-watch so a
// per-task escalate (which suspends one task) is visible to the same cycle's
// claim loops as a fleet-wide e-stop would be — both cost defenses share the
// same "land before the next claim cycle starts" constraint.
var dogOrder = []string{
	"spend-burn-watch",
	"task-spend-watch",
	"git-hygiene", "db-vacuum", "holonet-rotate", "mail-cleanup", "memory-hygiene",
	"stalled-reviews", "priority-aging", "daily-digest", "stale-convoys-report",
	"sub-pr-ci-watch", "main-drift-watch", "draft-pr-watch", "ship-it-nag",
	"repo-config-check", "pr-review-poll", "pr-review-resolve", "convoy-review-watch",
	"escalation-sweeper",
	// D2 T1-4 — surfaces stuck quarantined repos to the operator.
	"quarantined-repo-watch",
	// D3 P3 — runs after task-spend-watch (per the spec's ordering
	// requirement); computes per-pair cross-layer disagreement rates.
	"disagreement-tracker",
	// D3 P6B.12 — Sunday-night fleet learning panel re-render.
	"learning-panel-render",
	// D3 P6B.9 — daily transcript archival.
	"transcript-archive",
	// D3 fix-loop-1 (slice δ) — model-availability watch.
	"model-availability-watch",
	// D3 fix-loop-1 β2 — score-decay for stale ProposedFeatures.
	"proposed-features-decay",
	// D4 Phase 0 — Librarian-evolution dogs. Order:
	//  1. dedup-watch first (folds duplicates into canonicals so
	//     the conflict / hypothesis passes operate on the post-merge
	//     view).
	//  2. quality-recompute next (freshness decay informs the
	//     hypothesis emission threshold).
	//  3. conflict-watch surfaces operator-actionable contradictions.
	//  4. hypothesis-emit picks up high-quality memories now that
	//     dedup + quality have run.
	//  5. claude-md-drift-watch is independent of the FleetMemory
	//     pipeline; ordered last for tidiness.
	"librarian-dedup-watch",
	"librarian-quality-recompute",
	"librarian-conflict-watch",
	"librarian-hypothesis-emit",
	"claude-md-drift-watch",
	// D4 Phase 3 — senate-refresh runs after the librarian-evolution
	// dogs so the digest reads from the post-dedup, post-quality view
	// of FleetMemory. Independent of the per-Senator review path.
	"senate-refresh",
	// D5 Phase 4 (slice α) — supply-allowlist-refresh populates the
	// SUPPLY-002 typosquat allowlist from CodeArtifact's per-ecosystem
	// repositories. Ordered last because it has no dependency on any
	// other dog and its only persistence target is SystemConfig (no
	// FleetMemory / convoy / task interaction).
	"supply-allowlist-refresh",
}

// RunDogs checks each built-in dog against its cooldown and runs any that are due.
// Called by SpawnInquisitor on every inquisitor cycle.
//
// E-stop gate (AUDIT-106 / Fix #1): if the operator has flipped e-stop, NO
// dogs run — not even observational ones. Dogs issue `gh` API calls, push
// empty commits via TriggerCIRerun, queue PRReviewTriage tasks, and mutate
// AskBranchPR rows; every one of these continues burning tokens or quota
// during an emergency halt. The whole point of e-stop is to stop activity
// that costs money, so we short-circuit before the cooldown loop.
//
// The spend-burn-watch dog itself is skipped when estopped — its job is
// to auto-flip e-stop, which is already done. Re-running it mid-estop
// would just emit another no-op log line.
//
// Fix #8e: ctx threads from SpawnInquisitor (the per-tick ctx, ultimately
// the daemon ctx) so per-dog timeouts derive from a cancellable parent;
// pre-fix the per-dog 5-min context used a fabricated context.Background
// root, leaving in-flight dogs deaf to daemon shutdown.
// D0-B: lib threads through to the dogs whose handlers enqueue
// WriteMemory bounties (sub-pr-ci-watch, draft-pr-watch). The Librarian
// is constructed once at daemon startup and passed via SpawnInquisitor's
// config struct; tests pass a librarian.NewMock() instance.
//
// D5 Phase 4 (slice α): ca threads through to supply-allowlist-refresh
// (and forthcoming supply-token-recheck in slice β). The CodeArtifact
// client is constructed once at daemon startup via codeartifact.NewInProcess
// and passed via InquisitorConfig; tests pass a stub Client (or nil, which
// the dog will detect and skip with a log line).
func RunDogs(ctx context.Context, db *sql.DB, lib librarian.Client, ca codeartifact.Client, logger interface{ Printf(string, ...any) }) {
	if IsEstopped(db) {
		logger.Printf("RunDogs: e-stop active — skipping all dogs this cycle")
		return
	}
	for _, dogName := range dogOrder {
		cooldown := dogCooldowns[dogName]
		last := store.DogLastRun(db, dogName)
		if last != "" {
			// AUDIT-131 (Fix #8d): parse directly with the known SQLite
			// datetime shape ("YYYY-MM-DD HH:MM:SS", no TZ), in UTC. Pre-
			// fix this chained off an RFC3339-binary-probe branch that
			// always failed on the SQLite shape; post-fix we try the
			// SQLite shape first and fall back to RFC3339 via time.Parse
			// for legacy rows that happen to carry TZ-bearing timestamps
			// (from older fleet versions).
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", last, time.UTC); err == nil && time.Since(t) < cooldown {
				continue
			}
			if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < cooldown {
				continue
			}
		}
		logger.Printf("Dog %s: running (cooldown %v)", dogName, cooldown)
		store.DogMarkRun(db, dogName)
		// AUDIT-047 (Fix #8d): write heartbeat_at so /healthz can spot a
		// wedged dog — a dog whose heartbeat_at is stale relative to its
		// cooldown is stuck in a long-running op.
		store.DogMarkHeartbeat(db, dogName)
		// AUDIT-047 (Fix #8d): per-dog context.WithTimeout. A hung `gh
		// pr view` inside Inquisitor would previously wedge the whole
		// watchdog. Each dog now gets a 5-minute budget; past that we log
		// and move on so the cycle keeps running.
		// Fix #8e: derive the dog ctx from the inquisitor tick ctx so a
		// daemon SIGINT cancels in-flight dogs at their next ctx-aware
		// subprocess invocation.
		dogCtx, dogCancel := context.WithTimeout(ctx, 5*time.Minute)
		errCh := make(chan error, 1)
		go func(name string) { errCh <- runDog(dogCtx, db, name, lib, ca, logger) }(dogName)
		select {
		case err := <-errCh:
			if err != nil {
				logger.Printf("Dog %s: error — %v", dogName, err)
				// P27 burn-down: budget-gate the operator emit before SendMail.
				// On allowed=false the helper has already drop/digested per the
				// configured budget. Fail-open on err so a transient SQLite
				// glitch never silences a high-stakes alert.
				if allowed, _ := store.RespectNotificationBudget(
					context.Background(), db, "operator", "inquisitor", "email", "{}",
					store.StakesHigh,
				); !allowed {
					// budget exhausted (StakesHigh always punches through, so
					// this branch only fires on a real config-set 0-cap row).
				} else {
					_ = allowed
				}
				store.SendMail(db, "inquisitor", "operator",
					fmt.Sprintf("[DOG FAILURE] %s", dogName),
					fmt.Sprintf("Watchdog '%s' failed during its scheduled run.\n\nError: %v\n\nThis may indicate a system health issue requiring attention.", dogName, err),
					0, store.MailTypeAlert)
			} else {
				logger.Printf("Dog %s: done", dogName)
			}
		case <-dogCtx.Done():
			logger.Printf("Dog %s: timed out after 5m — moving on; the dog goroutine may still finish but won't block the cycle", dogName)
		}
		dogCancel()
	}
}

// RunDogByName force-runs the named dog exactly once, bypassing the cooldown.
// Used by CLI ("force dogs run <name>") and dashboard ("Run now") buttons.
// Returns an error with the list of valid dog names if the name is unknown.
// Fix #8e: ctx threads from the caller (CLI command ctx) so SIGINT cancels
// the manually-triggered dog.
// D0-B: lib is the librarian.Client used by sub-pr-ci-watch / draft-pr-watch
// for their WriteMemory enqueues; CLI/dashboard callers construct it via
// librarian.NewInProcess(db) at the entry point.
// D5 Phase 4 (slice α): ca is the codeartifact.Client used by
// supply-allowlist-refresh; CLI/dashboard callers construct it via
// codeartifact.NewInProcess(ctx, db) at the entry point. ca may be nil
// when the AWS SDK config can't load (e.g. in CI without AWS env); the
// dog itself detects nil and logs a skip rather than panicking.
func RunDogByName(ctx context.Context, db *sql.DB, name string, lib librarian.Client, ca codeartifact.Client, logger interface{ Printf(string, ...any) }) error {
	// Mark the run before executing so a crashed dog still shows up as attempted.
	store.DogMarkRun(db, name)
	return runDog(ctx, db, name, lib, ca, logger)
}

// DogNames returns the canonical order of registered dogs. Used for CLI
// completion and validation.
func DogNames() []string {
	out := make([]string, len(dogOrder))
	copy(out, dogOrder)
	return out
}

func runDog(ctx context.Context, db *sql.DB, name string, lib librarian.Client, ca codeartifact.Client, logger interface{ Printf(string, ...any) }) error {
	switch name {
	case "git-hygiene":
		return dogGitHygiene(ctx, db, logger)
	case "db-vacuum":
		return dogDBVacuum(db, logger)
	case "holonet-rotate":
		return dogHolonetRotate(logger)
	case "mail-cleanup":
		return dogMailCleanup(db, logger)
	case "memory-hygiene":
		return dogMemoryHygiene(db, logger)
	case "stalled-reviews":
		return dogStalledReviews(db, logger)
	case "priority-aging":
		return dogPriorityAging(db, logger)
	case "daily-digest":
		return runDailyDigest(db, logger)
	case "stale-convoys-report":
		return runStaleConvoysReport(db, logger)
	case "sub-pr-ci-watch":
		return dogSubPRCIWatch(ctx, db, lib, logger)
	case "main-drift-watch":
		return dogMainDriftWatch(ctx, db, logger)
	case "draft-pr-watch":
		return dogDraftPRWatch(ctx, db, lib, logger)
	case "ship-it-nag":
		return dogShipItNag(db, logger)
	case "repo-config-check":
		return dogRepoConfigCheck(ctx, db, logger)
	case "pr-review-poll":
		return dogPRReviewPoll(db, logger)
	case "pr-review-resolve":
		return dogPRReviewResolve(db, logger)
	case "convoy-review-watch":
		return dogConvoyReviewWatch(ctx, db, logger)
	case "escalation-sweeper":
		return dogEscalationSweeper(db, logger)
	case "spend-burn-watch":
		return dogSpendBurnWatch(db, logger)
	case "task-spend-watch":
		return dogTaskSpendWatch(db, logger)
	case "quarantined-repo-watch":
		return dogQuarantinedRepoWatch(db, logger)
	case "disagreement-tracker":
		return dogDisagreementTracker(ctx, db, logger)
	case "learning-panel-render":
		return dogLearningPanelRender(ctx, db, logger)
	case "transcript-archive":
		return dogTranscriptArchive(ctx, db, logger)
	case "model-availability-watch":
		return dogModelAvailabilityWatch(ctx, db, logger)
	case "proposed-features-decay":
		return dogProposedFeaturesDecay(db, logger)
	case "librarian-dedup-watch":
		return dogLibrarianDedup(ctx, db, logger)
	case "librarian-quality-recompute":
		return dogLibrarianQualityRecompute(ctx, db, logger)
	case "librarian-conflict-watch":
		return dogLibrarianConflictWatch(ctx, db, logger)
	case "librarian-hypothesis-emit":
		return dogLibrarianHypothesisEmit(ctx, db, logger)
	case "claude-md-drift-watch":
		return dogClaudeMDDriftWatch(ctx, db, lib, logger)
	case "senate-refresh":
		return dogSenateRefresh(ctx, db, lib, logger)
	case "supply-allowlist-refresh":
		return dogSupplyAllowlistRefresh(ctx, db, ca, logger)
	default:
		return fmt.Errorf("unknown dog: %s", name)
	}
}

// dogSupplyAllowlistRefresh walks each supported ecosystem (npm / pypi /
// rubygems / maven), calls codeartifact.Client.ListPackages, and writes
// the sorted, newline-joined name set into
// SystemConfig.supply_allowlist_<ecosystem>. Used by SUPPLY-002
// typosquat detection ("the set of packages the org has ever pulled —
// better signal than external popularity"). D5 Phase 4 (slice α).
//
// Per-ecosystem error handling:
//   - ErrTokenExpired: log and skip; the next 24h tick retries (the
//     operator will refresh the SSO session via `umt artifacts`
//     out-of-band). Non-fatal so a single ecosystem's auth blip doesn't
//     wipe the whole allowlist.
//   - any other error: aggregate via errors.Join and continue to the
//     next ecosystem; the joined error returns at the end so the
//     standard dog-mail path surfaces it.
//
// Go is intentionally NOT in the ecosystem list — CodeArtifact does not
// expose a Go format and SUPPLY-002 silent-skips Go entirely.
//
// On nil ca (e.g. when the daemon couldn't construct an in-process
// CodeArtifact client because LoadDefaultConfig failed), log and exit
// nil — the dog reschedules normally and the operator can re-attempt
// once AWS config is fixed.
func dogSupplyAllowlistRefresh(ctx context.Context, db *sql.DB, ca codeartifact.Client, logger interface{ Printf(string, ...any) }) error {
	if ca == nil {
		logger.Printf("Dog supply-allowlist-refresh: codeartifact client unavailable; skipping (operator must fix AWS config)")
		return nil
	}
	ecosystems := []codeartifact.Ecosystem{
		codeartifact.EcosystemNPM,
		codeartifact.EcosystemPyPI,
		codeartifact.EcosystemRubyGems,
		codeartifact.EcosystemMaven,
	}
	var errs []error
	for _, eco := range ecosystems {
		pkgs, err := ca.ListPackages(ctx, eco)
		if errors.Is(err, codeartifact.ErrTokenExpired) {
			logger.Printf("supply-allowlist-refresh: token expired for %s; skipping (will retry on next tick)", eco)
			continue
		}
		if err != nil {
			logger.Printf("supply-allowlist-refresh: %s ListPackages error: %v", eco, err)
			errs = append(errs, fmt.Errorf("%s: %w", eco, err))
			continue
		}
		names := make([]string, 0, len(pkgs))
		for _, p := range pkgs {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		store.SetConfig(db, fmt.Sprintf("supply_allowlist_%s", eco), strings.Join(names, "\n"))
		store.SetConfig(db, fmt.Sprintf("supply_allowlist_%s_last_refresh", eco), store.NowSQLite())
		logger.Printf("Dog supply-allowlist-refresh: %s — %d package(s)", eco, len(names))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// dogSenateRefresh iterates every active Senator chamber, calls
// librarian.RefreshSenatorMemoryDigest, appends new SenateMemory
// entries derived from the digest, and bumps last_refreshed_at on the
// chamber row. D4 Phase 3.
//
// Errors per-Senator are logged and the loop continues — a single
// unreadable repo doesn't sink the whole sweep. Returns nil unless
// the chamber-list query itself fails (which is a structural rather
// than transient problem and surfaces via the standard dog mail path).
func dogSenateRefresh(ctx context.Context, db *sql.DB, lib librarian.Client, logger interface{ Printf(string, ...any) }) error {
	chambers, err := store.ListActiveSenateChambers(db)
	if err != nil {
		return fmt.Errorf("senate-refresh: ListActiveSenateChambers: %w", err)
	}
	if len(chambers) == 0 {
		logger.Printf("Dog senate-refresh: no active Senators — nothing to refresh")
		return nil
	}
	for _, c := range chambers {
		digest, dErr := lib.RefreshSenatorMemoryDigest(ctx, c.SenatorName)
		if dErr != nil {
			logger.Printf("Dog senate-refresh: digest for %s failed (%v) — skipping", c.SenatorName, dErr)
			continue
		}
		if digest.APISurfaceSummary != "" {
			if _, mErr := store.InsertSenateMemory(db, store.SenateMemoryEntry{
				Senator: c.SenatorName,
				Topic:   "weekly-refresh-api",
				Summary: digest.APISurfaceSummary,
				Source:  "commit",
				Weight:  0.9,
			}); mErr != nil {
				logger.Printf("Dog senate-refresh: insert api memory for %s failed: %v", c.SenatorName, mErr)
			}
		}
		if digest.NotesForOperator != "" {
			if _, mErr := store.InsertSenateMemory(db, store.SenateMemoryEntry{
				Senator: c.SenatorName,
				Topic:   "weekly-refresh-notes",
				Summary: digest.NotesForOperator,
				Source:  "commit",
				Weight:  0.7,
			}); mErr != nil {
				logger.Printf("Dog senate-refresh: insert notes memory for %s failed: %v", c.SenatorName, mErr)
			}
		}
		if mErr := store.MarkSenateChamberRefreshed(db, c.SenatorName); mErr != nil {
			logger.Printf("Dog senate-refresh: MarkSenateChamberRefreshed(%s): %v", c.SenatorName, mErr)
		}
		logger.Printf("Dog senate-refresh: refreshed %s (commits=%d rules_active=%d)",
			c.SenatorName, len(digest.RecentCommits.Commits), digest.OutstandingRulesK)
	}
	return nil
}

// dogProposedFeaturesDecay decays stale ProposedFeatures value_score
// (high → medium → low) so old rows don't permanently rank highest in
// the queue (concern #10 / exit criterion 14). The store helper writes
// a paired ProposedFeatureScoreOverrides audit row for every demotion
// so the distribution-shift signal stays auditable (Pattern P24
// composes naturally — proposer-emitted scores aren't mutated; the
// dog's writes are tagged scored_by='system:decay-dog').
//
// `staleAfter` is hard-coded to 30 days here. Slice α/γ may extend
// this with operator-tunable thresholds in a later iteration.
func dogProposedFeaturesDecay(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	const staleAfter = 30 * 24 * time.Hour
	n, err := store.DecayProposedFeatureScores(db, staleAfter)
	if err != nil {
		return fmt.Errorf("proposed-features-decay: %w", err)
	}
	if n > 0 {
		logger.Printf("Dog proposed-features-decay: decayed %d rows (stale > %v)", n, staleAfter)
	}
	return nil
}

// dogLearningPanelRender renders one FleetLearningPanels row for the
// trailing 7 days. Idempotent: re-running mid-window inserts a new row
// (the dashboard always reads the most recent). Errors propagate so
// the inquisitor cycle's per-dog error path mails the operator.
func dogLearningPanelRender(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	id, err := RenderFleetLearningPanel(ctx, db, time.Now())
	if err != nil {
		return fmt.Errorf("learning-panel-render: %w", err)
	}
	logger.Printf("Dog learning-panel-render: rendered FleetLearningPanels/%d", id)
	return nil
}


func dogGitHygiene(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Collect repos first, then close rows before doing any further DB work.
	// AUDIT-159: defer the close so a scan-error exit doesn't leak the FD
	// (manual `rows.Close()` on the tail only ran on the happy path).
	rows, err := db.Query(`SELECT name, local_path FROM Repositories`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type repo struct{ name, path string }
	var repos []repo
	for rows.Next() {
		var r repo
		if sErr := rows.Scan(&r.name, &r.path); sErr != nil {
			logger.Printf("Dog git-hygiene: repos scan failed: %v", sErr)
			continue
		}
		repos = append(repos, r)
	}
	if rErr := rows.Err(); rErr != nil {
		logger.Printf("Dog git-hygiene: repos query iter error: %v", rErr)
	}

	for _, r := range repos {
		if _, statErr := os.Stat(r.path); statErr != nil {
			logger.Printf("ERROR: Dog git-hygiene: repo '%s' path not accessible (%s) — check registration with: force repos", r.name, r.path)
			continue
		}
		if out, gitErr := igit.RunCmd(ctx, r.path, "fetch", "--prune", "--quiet"); gitErr != nil {
			logger.Printf("Dog git-hygiene: fetch failed for %s: %s", r.name, out)
		} else {
			logger.Printf("Dog git-hygiene: fetched %s", r.name)
		}
		igit.RunCmd(ctx, r.path, "gc", "--auto", "--quiet")
		igit.RunCmd(ctx, r.path, "worktree", "prune")
	}

	// Detach agent worktrees that are on branches no longer referenced by any live task,
	// and delete those orphaned branches. This happens when a task retries with a different
	// agent — the new agent creates a fresh branch, updating branch_name in the DB, but
	// the old agent's worktree stays on the old branch until we clean it up here.
	agentRows, agentErr := db.Query(`SELECT agent_name, repo, worktree_path FROM Agents`)
	if agentErr != nil {
		// AUDIT-091 (Fix #8d): propagate the query error to the caller so
		// RunDogs routes it to the operator-mail path. Pre-fix a broken
		// schema or locked DB was swallowed and the orphan-cleanup silently
		// skipped with no operator signal.
		return fmt.Errorf("dog git-hygiene: Agents query failed: %w", agentErr)
	}
	defer agentRows.Close()
	type agentEntry struct{ name, repo, path string }
	var agents []agentEntry
	for agentRows.Next() {
		var a agentEntry
		if sErr := agentRows.Scan(&a.name, &a.repo, &a.path); sErr != nil {
			logger.Printf("Dog git-hygiene: agents scan failed: %v", sErr)
			continue
		}
		agents = append(agents, a)
	}
	if rErr := agentRows.Err(); rErr != nil {
		logger.Printf("Dog git-hygiene: agents query iter error: %v", rErr)
	}

	var detached int
	for _, a := range agents {
		if _, statErr := os.Stat(a.path); statErr != nil {
			continue
		}
		// D3 polish-pass iteration 2 (B4r): route through igit.LogAndRun
		// so the dog's git-hygiene ops are recorded in GitOperationLog
		// (Pattern P32). The OpContext carries the repo path; task id
		// is 0 because this is fleet-wide hygiene, not task-scoped.
		out, gitErr := igit.LogAndRun(ctx, igit.OpContext{Repo: a.repo}, "dog-git-hygiene-rev-parse",
			"git", "-C", a.path, "rev-parse", "--abbrev-ref", "HEAD")
		if gitErr != nil {
			continue
		}
		branch := strings.TrimSpace(string(out))
		if branch == "HEAD" {
			continue // already detached
		}
		// Keep the branch if any non-terminal task still references it.
		// Failed/Escalated are included as "live" so operators can inspect the branch.
		var count int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE branch_name = ? AND status NOT IN ('Completed','Cancelled')`, branch).Scan(&count)
		if count > 0 {
			continue
		}
		_, _ = igit.LogAndRun(ctx, igit.OpContext{Repo: a.repo}, "dog-git-hygiene-detach",
			"git", "-C", a.path, "checkout", "--detach", "HEAD")
		_, _ = igit.LogAndRun(ctx, igit.OpContext{Repo: a.repo}, "dog-git-hygiene-branch-D",
			"git", "-C", a.repo, "branch", "-D", branch)
		detached++
		logger.Printf("Dog git-hygiene: detached worktree %s from orphaned branch %s", a.name, branch)
	}
	if detached > 0 {
		logger.Printf("Dog git-hygiene: cleaned up %d orphaned worktree branch(es)", detached)
	}
	return nil
}

const vacuumThresholdBytes = 100 * 1024 * 1024 // 100 MB

func dogDBVacuum(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// WAL checkpointing is handled by SpawnInquisitor every 5 minutes — no need to repeat here.
	db.Exec(`ANALYZE`)

	// Only VACUUM when the database file exceeds the threshold; VACUUM holds an
	// exclusive lock for its entire duration, so we avoid it during normal operation.
	var seq int
	var dbName, dbFile string
	db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbFile)
	if dbFile != "" {
		info, err := os.Stat(dbFile)
		if err == nil && info.Size() < vacuumThresholdBytes {
			logger.Printf("Dog db-vacuum: skipping VACUUM (%.1f MB < threshold)", float64(info.Size())/1024/1024)
			return nil
		}
	}

	_, err := db.Exec(`VACUUM`)
	return err
}

const holonetMaxBytes = 50 * 1024 * 1024 // 50 MB

func dogHolonetRotate(logger interface{ Printf(string, ...any) }) error {
	const path = "holonet.jsonl"
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Size() < holonetMaxBytes {
		return nil
	}
	// Use millisecond precision to prevent same-second filename collisions.
	stamp := time.Now().UTC().Format("20060102-150405.000")
	archivePath := filepath.Join("holonet-" + stamp + ".jsonl")
	if renErr := os.Rename(path, archivePath); renErr != nil {
		return renErr
	}
	logger.Printf("Dog holonet-rotate: rotated %s → %s (%.1f MB)", path, archivePath, float64(info.Size())/1024/1024)
	return nil
}

func dogMailCleanup(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	res, err := db.Exec(`
		DELETE FROM Fleet_Mail
		WHERE consumed_at = ''
		  AND task_id != 0
		  AND created_at < datetime('now', '-48 hours')
		  AND task_id IN (
		      SELECT id FROM BountyBoard WHERE status IN ('Completed', 'Failed', 'Escalated')
		  )`)
	if err != nil {
		return err
	}
	stale, _ := res.RowsAffected()

	res2, err := db.Exec(`DELETE FROM Fleet_Mail WHERE read_at != '' AND created_at < datetime('now', '-30 days')`)
	if err != nil {
		return err
	}
	old, _ := res2.RowsAffected()

	if stale > 0 || old > 0 {
		logger.Printf("Dog mail-cleanup: removed %d stale unread + %d old read messages", stale, old)
	}
	return nil
}

func dogMemoryHygiene(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Pass 1: delete failure memories where a success memory exists for the same task_id.
	res1, err := db.Exec(`
		DELETE FROM FleetMemory
		WHERE outcome = 'failure'
		  AND task_id IN (
		      SELECT task_id FROM FleetMemory WHERE outcome = 'success' AND task_id != 0
		  )`)
	if err != nil {
		return fmt.Errorf("memory-hygiene pass 1: %w", err)
	}
	removed1, _ := res1.RowsAffected()
	if removed1 > 0 {
		if _, err := db.Exec(`DELETE FROM FleetMemory_fts WHERE rowid NOT IN (SELECT id FROM FleetMemory)`); err != nil {
			return fmt.Errorf("memory-hygiene pass 1 fts sync: %w", err)
		}
		logger.Printf("Dog memory-hygiene: pass 1 removed %d stale failure memories", removed1)
	}

	// Pass 2: delete stale audit-finding memories whose underlying task is Completed.
	res2, err := db.Exec(`
		DELETE FROM FleetMemory
		WHERE summary LIKE '[AUDIT FINDING%'
		  AND task_id IN (
		      SELECT id FROM BountyBoard WHERE status = 'Completed'
		  )`)
	if err != nil {
		return fmt.Errorf("memory-hygiene pass 2: %w", err)
	}
	removed2, _ := res2.RowsAffected()
	if removed2 > 0 {
		if _, err := db.Exec(`DELETE FROM FleetMemory_fts WHERE rowid NOT IN (SELECT id FROM FleetMemory)`); err != nil {
			return fmt.Errorf("memory-hygiene pass 2 fts sync: %w", err)
		}
		logger.Printf("Dog memory-hygiene: pass 2 removed %d stale audit-finding memories", removed2)
	}

	if removed1 == 0 && removed2 == 0 {
		logger.Printf("Dog memory-hygiene: nothing to clean up")
	}
	return nil
}

func dogStalledReviews(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Traditional review-stall: tasks stuck in Captain/Council queues. locked_at
	// is set when they enter review so we can measure age directly.
	rows, err := db.Query(`
		SELECT id, status,
		       ROUND((julianday('now') - julianday(locked_at)) * 24, 1) AS hours_waiting
		FROM BountyBoard
		WHERE status IN ('AwaitingCaptainReview', 'AwaitingCouncilReview')
		  AND locked_at < datetime('now', '-4 hours')
		ORDER BY locked_at ASC`)
	if err != nil {
		return err
	}
	type stalledTask struct {
		id           int64
		status       string
		hoursWaiting float64
	}
	var tasks []stalledTask
	for rows.Next() {
		var t stalledTask
		if err := rows.Scan(&t.id, &t.status, &t.hoursWaiting); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, t)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("dogs.go:dogStalledReviews: rows iter error: %v", rErr)
	}
	rows.Close()

	// Sub-PR-CI stall: AwaitingSubPRCI tasks have owner='' (Jedi clears it when
	// handing off to sub-pr-ci-watch). Use the AskBranchPR's created_at as the
	// age yardstick. Threshold is longer (12h) because CI legitimately takes
	// minutes-to-hours on some repos and Medic has up to 3 attempts to fix.
	subPRRows, sErr := db.Query(`
		SELECT b.id,
		       ROUND((julianday('now') - julianday(abp.created_at)) * 24, 1) AS hours_open
		FROM BountyBoard b
		JOIN AskBranchPRs abp ON abp.task_id = b.id
		WHERE b.status = 'AwaitingSubPRCI'
		  AND abp.state = 'Open'
		  AND abp.created_at < datetime('now', '-12 hours')
		ORDER BY abp.created_at ASC`)
	if sErr != nil {
		logger.Printf("Dog stalled-reviews: sub-PR query failed: %v", sErr)
	} else {
		for subPRRows.Next() {
			var id int64
			var hours float64
			// AUDIT-090 (Fix #8d): log scan errors — pre-fix the err==nil
			// path silently dropped rows, so a legitimate 12h+ AwaitingSubPRCI
			// stall that happened to scan-fail never raised an alarm.
			if err := subPRRows.Scan(&id, &hours); err != nil {
				logger.Printf("Dog stalled-reviews: sub-PR scan failed for next row: %v", err)
				continue
			}
			tasks = append(tasks, stalledTask{id: id, status: "AwaitingSubPRCI", hoursWaiting: hours})
		}
		if iterErr := subPRRows.Err(); iterErr != nil {
			logger.Printf("Dog stalled-reviews: sub-PR iteration error: %v", iterErr)
		}
		subPRRows.Close()
	}

	if len(tasks) == 0 {
		return nil
	}

	logger.Printf("Dog stalled-reviews: %d task(s) stuck in review queue", len(tasks))

	var body strings.Builder
	fmt.Fprintf(&body, "%d task(s) have been waiting in the review queue for extended periods:\n\n", len(tasks))
	for _, t := range tasks {
		fmt.Fprintf(&body, "  Task #%d  status=%-24s  waiting=%.1fh\n", t.id, t.status, t.hoursWaiting)
	}

	store.SendMail(db, "inquisitor", "operator",
		fmt.Sprintf("[STALLED REVIEWS] %d tasks stuck in review", len(tasks)),
		body.String(), 0, store.MailTypeAlert)
	return nil
}

func dogPriorityAging(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	res1, err := db.Exec(`
		UPDATE BountyBoard
		SET priority = 1
		WHERE status = 'Pending'
		  AND priority = 0
		  AND created_at < datetime('now', '-12 hours')`)
	if err != nil {
		return err
	}
	bumped1, _ := res1.RowsAffected()

	res2, err := db.Exec(`
		UPDATE BountyBoard
		SET priority = 2
		WHERE status = 'Pending'
		  AND priority < 2
		  AND created_at < datetime('now', '-24 hours')`)
	if err != nil {
		return err
	}
	bumped2, _ := res2.RowsAffected()

	logger.Printf("Dog priority-aging: bumped %d task(s) to priority 1, %d task(s) to priority 2", bumped1, bumped2)
	return nil
}

func runDailyDigest(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	stats, err := store.FetchDigestStats(db)
	if err != nil {
		return fmt.Errorf("daily-digest: fetch stats: %w", err)
	}

	var body strings.Builder

	fmt.Fprintf(&body, "## Tasks (Last 24h)\n\n")
	fmt.Fprintf(&body, "- Completed: %d\n", stats.Completed)
	fmt.Fprintf(&body, "- Failed: %d\n", stats.Failed)
	fmt.Fprintf(&body, "- Escalated: %d\n\n", stats.Escalated)

	fmt.Fprintf(&body, "## Current Queue\n\n")
	fmt.Fprintf(&body, "- Pending: %d\n", stats.Pending)
	fmt.Fprintf(&body, "- Locked: %d\n\n", stats.Locked)

	fmt.Fprintf(&body, "## Top Agents\n\n")
	if len(stats.TopAgents) == 0 {
		fmt.Fprintf(&body, "None\n\n")
	} else {
		for _, a := range stats.TopAgents {
			fmt.Fprintf(&body, "- %s: %d\n", a.Agent, a.Count)
		}
		fmt.Fprintf(&body, "\n")
	}

	fmt.Fprintf(&body, "## Stale Convoys\n\n")
	if len(stats.StaleConvoys) == 0 {
		fmt.Fprintf(&body, "None\n")
	} else {
		for _, c := range stats.StaleConvoys {
			fmt.Fprintf(&body, "- #%d %s\n", c.ID, c.Name)
		}
	}

	store.SendMail(db, "inquisitor", "operator", "[DAILY DIGEST] Fleet Health Summary", body.String(), 0, store.MailTypeInfo)
	logger.Printf("Dog daily-digest: digest sent (completed=%d failed=%d escalated=%d pending=%d locked=%d stale_convoys=%d)",
		stats.Completed, stats.Failed, stats.Escalated, stats.Pending, stats.Locked, len(stats.StaleConvoys))
	return nil
}

// runStaleConvoysReport scans Active convoys and transitions those whose tasks
// have all reached a terminal status. The terminal set is
// ('Completed','Cancelled','Failed','Escalated') — NOT just the first two —
// so a convoy full of Failed or Escalated tasks no longer silently flips to
// Completed (AUDIT-012, Fix #5). When any child task is Failed/Escalated the
// convoy transitions to 'Failed' and the operator receives an alert; only
// convoys whose children are all Completed/Cancelled auto-complete.
func runStaleConvoysReport(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	rows, err := db.Query(`SELECT id, name FROM Convoys WHERE status = 'Active'`)
	if err != nil {
		return err
	}
	type convoy struct {
		id   int64
		name string
	}
	var convoys []convoy
	for rows.Next() {
		var c convoy
		if err := rows.Scan(&c.id, &c.name); err != nil {
			logger.Printf("dog convoy-reconcile: scan failed: %v", err)
			continue
		}
		convoys = append(convoys, c)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("dogs.go:runStaleConvoysReport: rows iter error: %v", rErr)
	}
	rows.Close()

	var fixedEmpty, fixedCompleted, fixedFailed int
	for _, c := range convoys {
		var total int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ?`, c.id).Scan(&total)

		// Decide whether this convoy is ready for a terminal transition.
		//   total == 0          → empty convoy → Completed (no-tasks).
		//   nonTerminal == 0    → every task has reached a terminal status
		//                         (Completed, Cancelled, Failed, or Escalated).
		//                         If any child is Failed/Escalated, the convoy
		//                         is Failed (not Completed) — a convoy full of
		//                         failed tasks is a stall, not a success.
		var (
			shouldAct  bool
			wantFailed bool
			reason     string
		)
		if total == 0 {
			shouldAct = true
			reason = "no tasks"
		} else {
			var nonTerminal int
			db.QueryRow(`
				SELECT COUNT(*) FROM BountyBoard
				WHERE convoy_id = ?
				  AND status NOT IN ('Completed', 'Cancelled', 'Failed', 'Escalated')`, c.id).Scan(&nonTerminal)
			if nonTerminal == 0 {
				shouldAct = true

				// If any child task is Failed/Escalated, the convoy is failed —
				// not completed. Masking failures as "success" is exactly the
				// bug AUDIT-012 called out.
				var problemCount int
				db.QueryRow(`
					SELECT COUNT(*) FROM BountyBoard
					WHERE convoy_id = ?
					  AND status IN ('Failed','Escalated')`, c.id).Scan(&problemCount)
				if problemCount > 0 {
					wantFailed = true
					reason = fmt.Sprintf("%d task(s) Failed/Escalated, remainder terminal", problemCount)
				} else {
					reason = "all tasks terminal"
				}
			}
		}

		if !shouldAct {
			continue
		}

		if wantFailed {
			db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ? AND status = 'Active'`, c.id)
			// Operator mail — modeled on CheckConvoyCompletions's STALLED alert so
			// dashboards and inbox filters treat the two paths identically.
			subject := fmt.Sprintf("[CONVOY FAILED] %s", c.name)
			var existing int
			db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject = ? AND read_at = ''`, subject).Scan(&existing)
			if existing == 0 {
				var taskErr string
				db.QueryRow(`SELECT error_log FROM BountyBoard WHERE convoy_id = ? AND status IN ('Failed','Escalated') ORDER BY id ASC LIMIT 1`, c.id).Scan(&taskErr)
				body := fmt.Sprintf(
					"Convoy '%s' was transitioned to Failed by the stale-convoys-report dog (%s).\n\nInspect: force convoy show %d\nRetry failed tasks: force convoy reset %d",
					c.name, reason, c.id, c.id)
				if taskErr != "" {
					body += "\n\nFirst failure:\n" + taskErr
				}
				store.SendMail(db, "inquisitor", "operator", subject, body, 0, store.MailTypeAlert)
			}
			fixedFailed++
			continue
		}

		db.Exec(`UPDATE Convoys SET status = 'Completed' WHERE id = ? AND status = 'Active'`, c.id)
		store.SendMail(db, "inquisitor", "operator",
			fmt.Sprintf("[CONVOY COMPLETE] %s", c.name),
			fmt.Sprintf("Convoy '%s' was auto-completed by the stale-convoys-report dog (%s).", c.name, reason),
			0, store.MailTypeInfo)

		if total == 0 {
			fixedEmpty++
		} else {
			fixedCompleted++
		}
	}

	switch {
	case fixedEmpty == 0 && fixedCompleted == 0 && fixedFailed == 0:
		logger.Printf("Dog stale-convoys-report: no stale convoys found")
	default:
		logger.Printf("Dog stale-convoys-report: completed %d empty convoy(s), %d stale convoy(s) with all-success children, %d stale convoy(s) transitioned to Failed", fixedEmpty, fixedCompleted, fixedFailed)
	}
	return nil
}

// DogStatus holds the status of a single watchdog.
type DogStatus struct {
	Name     string
	Cooldown time.Duration
	LastRun  string
	NextRun  string
	RunCount int
}

// ListDogs returns the name, last-run time, and run count for all known dogs.
func ListDogs(db *sql.DB) []DogStatus {
	var result []DogStatus
	for _, name := range dogOrder {
		cooldown := dogCooldowns[name]
		var lastRun string
		var count int
		db.QueryRow(`SELECT last_run_at, run_count FROM Dogs WHERE name = ?`, name).Scan(&lastRun, &count)
		var nextRun string
		if lastRun != "" {
			// Fix #8c (AUDIT-146): SQLite-UTC column on the DB side compared
			// to a UTC `time.Now().UTC()` on the Go side — apples-to-apples.
			// The prior code used raw `time.Now()` (local) against a
			// ParseInLocation-UTC'd value; it worked by coincidence because
			// time.Time values carry their own Location, but was fragile to
			// any future refactor of the parse.
			t, err := store.ParseSQLiteTime(lastRun)
			if err == nil {
				next := t.Add(cooldown)
				now := time.Now().UTC()
				if now.Before(next) {
					nextRun = fmt.Sprintf("in %v", next.Sub(now).Round(time.Minute))
				} else {
					nextRun = "overdue"
				}
			}
		} else {
			nextRun = "never run"
		}
		result = append(result, DogStatus{
			Name:     name,
			Cooldown: cooldown,
			LastRun:  lastRun,
			NextRun:  nextRun,
			RunCount: count,
		})
	}
	return result
}
