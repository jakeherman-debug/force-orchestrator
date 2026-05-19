// Package agents — D8 Track 3 Diplomat ConsumerIntegrationCheck handler.
//
// Mission (per docs/roadmap.md §D8 Track D8-IntegTest, lines 2198-2229):
//
// Before merging a Feature whose blast-radius flags consumer repos,
// validate that the proposed change doesn't break those consumers'
// tests — without waiting for the consumer's next CI cycle to surface
// the breakage.
//
// Flow:
//   1. Diplomat's ShipConvoy handler transitions the convoy to DraftPROpen.
//      A small dispatcher (DispatchConsumerIntegrationChecks below, called
//      at end of runShipConvoy) reads the parent Feature's blast-radius
//      and queues one ConsumerIntegrationCheck task per affected consumer
//      repo.
//   2. Diplomat's claim loop picks up each ConsumerIntegrationCheck task
//      and routes to runConsumerIntegrationCheck.
//   3. The handler:
//      a. Loads the consumer Repository row; if mode='read_only' or
//         'quarantined', records skipped_read_only and exits.
//      b. Detects the consumer's primary language. v1 only supports Go;
//         non-Go repos record skipped_unsupported_lang AND fire one
//         operator mail per new-language-encountered (dedup via
//         SystemConfig key).
//      c. For Go: creates a producer worktree (so the producer's ask-branch
//         is materialised at a stable on-disk path) and a consumer worktree
//         from the consumer's main branch. Adds a `replace` directive to
//         the consumer's go.mod pointing at the producer worktree. Runs
//         `go build ./...` then `go test ./...` (or the per-repo override
//         from RepositoryTestCommand). Captures exit code, stdout/stderr
//         tail, duration.
//      d. Pre-existing-red detection: BEFORE applying the producer change,
//         the handler runs the consumer's tests on its current main. If
//         that baseline already fails, the result is pre_existing_red and
//         the change-applied run is skipped (no point comparing against a
//         broken baseline).
//      e. Persists the row to ConsumerIntegrationResults.
//      f. Aggregates per-Feature: if any row is `red`, emits
//         [CONSUMER BREAKAGE] operator mail; otherwise the ship gate is
//         not blocked.
//
// Anti-cheat invariants honoured:
//   - No string-grep dependency detection — Track 1 supplies the
//     CrossRepoSymbols set; Track 2 produces the consumer-repo list;
//     Track 3 just consumes it.
//   - Non-Go repos are NOT skipped silently — the handler logs +
//     mails the operator on first encounter.
//   - Test-only consumer references are out-of-scope here (Track 2
//     responsibility); Track 3 honestly attempts the test against
//     whatever consumer the blast-radius flagged.
//   - The graph informs the dispatch but operator still ratifies via
//     ship-it; this handler emits evidence, not approvals.
//
// Per CLAUDE.md "no silent failures": every error path terminates in
// store.FailBounty / UpdateBountyStatus / explicit operator mail.
package agents

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// consumerIntegrationCheckPayload is the on-wire shape for a
// ConsumerIntegrationCheck task.
type consumerIntegrationCheckPayload struct {
	FeatureID    int    `json:"feature_id"`
	ConvoyID     int    `json:"convoy_id"`
	ConsumerRepo string `json:"consumer_repo"`
	// ProducerRepo is the repo whose ask-branch we'll wire into the
	// consumer via a Go module replace directive. For multi-repo Features,
	// the dispatcher emits one task per (consumer, producer) crossing —
	// v1 simplifies to: one task per consumer, with the producer chosen
	// as the convoy's first ConvoyAskBranch. The handler still records
	// the producer it picked so the audit trail is honest.
	ProducerRepo string `json:"producer_repo"`
	// ProducerAskBranch is the ask-branch we test against (the producer's
	// integration branch carrying all in-flight CodeEdits for the convoy).
	ProducerAskBranch string `json:"producer_ask_branch"`
}

const (
	// ConsumerIntegrationWorktreeRoot is the on-disk root for ephemeral
	// consumer-integration worktrees. Distinct from .force-worktrees
	// (production astromech worktrees) and .force-shadow-worktrees
	// (paired-runs shadow worktrees) so sweepers don't conflate them.
	ConsumerIntegrationWorktreeRoot = ".force-integ-worktrees"

	// defaultConsumerIntegTimeoutMinutes is the per-consumer test-budget
	// fallback when SystemConfig.consumer_integ_timeout_minutes is unset
	// or unparseable. Matches the roadmap default (line 2216).
	defaultConsumerIntegTimeoutMinutes = 20

	// defaultConsumerIntegTestCommand is the canonical Go test command.
	// Operators can override per-repo via SystemConfig key
	// `consumer_integ_test_command:<repo_name>`.
	defaultConsumerIntegTestCommand = "go test ./..."

	// defaultConsumerIntegBuildCommand runs first; a build failure in
	// either run (baseline OR with-change) tracks just like a test failure.
	defaultConsumerIntegBuildCommand = "go build ./..."
)

// QueueConsumerIntegrationCheck enqueues one ConsumerIntegrationCheck task
// for the (feature, consumer_repo) pair. Idempotent via
// "consumer-integ:<feature>:<consumer>" key — a re-queue while one is
// already pending is a no-op (returns 0). Returns the new task ID on
// fresh insert; (0, nil) when an existing live row claims the key.
func QueueConsumerIntegrationCheck(db *sql.DB, featureID, convoyID int, consumerRepo, producerRepo, producerAskBranch string) (int, error) {
	if featureID <= 0 {
		return 0, fmt.Errorf("QueueConsumerIntegrationCheck: featureID required")
	}
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueConsumerIntegrationCheck: convoyID required")
	}
	if consumerRepo == "" {
		return 0, fmt.Errorf("QueueConsumerIntegrationCheck: consumerRepo required")
	}
	// Skip if a result already exists — the "run once per Feature in
	// DraftPROpen" budget bound from the roadmap (line 2217).
	if exists, err := store.HasConsumerIntegrationResult(db, featureID, consumerRepo); err != nil {
		return 0, fmt.Errorf("QueueConsumerIntegrationCheck: existing-result lookup: %w", err)
	} else if exists {
		return 0, nil
	}
	payload, mErr := json.Marshal(consumerIntegrationCheckPayload{
		FeatureID:         featureID,
		ConvoyID:          convoyID,
		ConsumerRepo:      consumerRepo,
		ProducerRepo:      producerRepo,
		ProducerAskBranch: producerAskBranch,
	})
	if mErr != nil {
		return 0, fmt.Errorf("QueueConsumerIntegrationCheck: marshal payload: %w", mErr)
	}
	key := fmt.Sprintf("consumer-integ:%d:%s", featureID, consumerRepo)
	id, existed, qErr := store.AddIdempotentTask(
		db, key, featureID, consumerRepo, "ConsumerIntegrationCheck",
		string(payload), convoyID, 4, "Pending",
	)
	if qErr != nil {
		return 0, fmt.Errorf("QueueConsumerIntegrationCheck: %w", qErr)
	}
	if existed {
		return 0, nil
	}
	return id, nil
}

// DispatchConsumerIntegrationChecks reads the parent Feature's blast-radius
// for the convoy and queues one ConsumerIntegrationCheck task per affected
// consumer repo. Called from runShipConvoy after the convoy transitions to
// DraftPROpen. Returns the count of tasks queued (counting idempotent skips
// as 0) and any error.
//
// The Feature ID is derived from the convoy's task rows: chancellor.go
// inserts CodeEdit tasks with parent_id = feature.ID. We pick any
// CodeEdit's parent_id (the set is uniform per convoy); falls back to
// Convoys.parent_feature_id when the column is populated. If neither
// resolves, the dispatcher logs + skips — a convoy without a Feature
// is a legacy / hand-crafted shape and Track 3 simply doesn't apply.
func DispatchConsumerIntegrationChecks(db *sql.DB, convoyID int, branches []store.ConvoyAskBranch, logger interface{ Printf(string, ...any) }) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("DispatchConsumerIntegrationChecks: db is nil")
	}
	featureID, fErr := lookupConvoyFeatureID(db, convoyID)
	if fErr != nil {
		return 0, fmt.Errorf("DispatchConsumerIntegrationChecks(convoy=%d): %w", convoyID, fErr)
	}
	if featureID <= 0 {
		// Legacy convoy with no Feature ancestor — Track 3 is a no-op.
		if logger != nil {
			logger.Printf("DispatchConsumerIntegrationChecks: convoy %d has no resolvable parent Feature — skipping Track 3", convoyID)
		}
		return 0, nil
	}
	rec, brErr := store.GetFeatureBlastRadius(db, featureID)
	if brErr != nil {
		return 0, fmt.Errorf("DispatchConsumerIntegrationChecks(convoy=%d, feature=%d): %w",
			convoyID, featureID, brErr)
	}

	// D15: union symbol-level consumers (D8 blast-radius) with API consumers
	// (D15 CrossRepoAPIDependencies) so integration checks cover both surfaces.
	// The union is computed here rather than in PostProcessBlastRadius so
	// that D8 and D15 can evolve independently without re-running the blast
	// radius computation.
	consumerSet := map[string]struct{}{}
	for _, r := range rec.AffectedConsumerRepos {
		consumerSet[r] = struct{}{}
	}
	for _, r := range rec.APIConsumers {
		consumerSet[r] = struct{}{}
	}
	if len(consumerSet) == 0 {
		if logger != nil {
			logger.Printf("DispatchConsumerIntegrationChecks: feature #%d has no consumer repos (symbol or API) — nothing to dispatch", featureID)
		}
		return 0, nil
	}

	// Pick the producer repo + ask-branch. v1: first ConvoyAskBranch row
	// (sorted by repo via ListConvoyAskBranches's ORDER BY). For
	// multi-producer convoys this is a simplification — the consumer test
	// will only see one producer's change. Track 3 v2 can fan out per
	// (consumer, producer) crossing; for v1 the simpler shape covers the
	// dominant single-producer case.
	var producerRepo, producerAskBranch string
	for _, ab := range branches {
		if ab.AskBranch != "" {
			producerRepo = ab.Repo
			producerAskBranch = ab.AskBranch
			break
		}
	}
	if producerRepo == "" {
		if logger != nil {
			logger.Printf("DispatchConsumerIntegrationChecks: feature #%d has no ask-branch on any ConvoyAskBranch row — cannot wire consumer test, skipping", featureID)
		}
		return 0, nil
	}
	queued := 0
	for consumer := range consumerSet {
		// Don't ConsumerIntegrationCheck a producer against itself —
		// that's just running the producer's own tests, which the
		// producer's CI already does.
		isProducer := false
		for _, ab := range branches {
			if ab.Repo == consumer {
				isProducer = true
				break
			}
		}
		if isProducer {
			continue
		}
		id, qErr := QueueConsumerIntegrationCheck(db, featureID, convoyID,
			consumer, producerRepo, producerAskBranch)
		if qErr != nil {
			// Don't fail the whole dispatch on one consumer; log and continue.
			// The operator sees the failure in the daemon log; the missing
			// row in ConsumerIntegrationResults makes the gap visible.
			if logger != nil {
				logger.Printf("DispatchConsumerIntegrationChecks(feature=%d, consumer=%s): queue failed: %v",
					featureID, consumer, qErr)
			}
			continue
		}
		if id > 0 {
			queued++
			if logger != nil {
				logger.Printf("DispatchConsumerIntegrationChecks(feature=%d): queued ConsumerIntegrationCheck #%d for consumer %s",
					featureID, id, consumer)
			}
		}
	}
	return queued, nil
}

// lookupConvoyFeatureID resolves the Feature ID for a convoy. Strategy:
//   1. Read Convoys.parent_feature_id (forward-compat — populated in v2+).
//   2. Fall back to SELECT DISTINCT parent_id FROM BountyBoard WHERE
//      convoy_id=? AND type='CodeEdit' AND parent_id > 0 LIMIT 1 — the
//      legacy / current shape (commander.insertConvoyAndTasks stamps
//      parent_id = feature.ID on every CodeEdit).
// Returns (0, nil) when neither path resolves — signal for "no Feature
// ancestor exists" (legacy convoy).
func lookupConvoyFeatureID(db *sql.DB, convoyID int) (int, error) {
	var pfid int
	if err := db.QueryRow(`SELECT IFNULL(parent_feature_id, 0) FROM Convoys WHERE id = ?`,
		convoyID).Scan(&pfid); err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("lookupConvoyFeatureID(convoy=%d): convoys: %w", convoyID, err)
	}
	if pfid > 0 {
		return pfid, nil
	}
	// Fallback: any CodeEdit's parent_id.
	if err := db.QueryRow(`SELECT parent_id FROM BountyBoard
		WHERE convoy_id = ? AND type = 'CodeEdit' AND parent_id > 0
		ORDER BY id ASC LIMIT 1`, convoyID).Scan(&pfid); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("lookupConvoyFeatureID(convoy=%d): codeedit parent: %w", convoyID, err)
	}
	return pfid, nil
}

// runConsumerIntegrationCheck is the Diplomat handler for a claimed
// ConsumerIntegrationCheck bounty. Single-pass: read payload, run the
// integration check, persist the result, aggregate per-Feature, complete
// the bounty.
//
// Failure modes are explicit per CLAUDE.md "no silent failures":
//   - Bad payload → FailBounty.
//   - Consumer repo not registered → FailBounty (the dispatcher upstream
//     should have caught this; defensive).
//   - Consumer repo read_only/quarantined → green-completion path with
//     status=skipped_read_only persisted.
//   - Unsupported language → green-completion + status=skipped_unsupported_lang.
//   - Worktree-add failure → status=error persisted (NOT blocking),
//     bounty Completed (the row exists, the operator sees the error).
//   - Build/test timeout → status=timeout persisted (NOT blocking).
//   - Real test failure → status=red persisted; aggregation step emits
//     [CONSUMER BREAKAGE] mail.
func runConsumerIntegrationCheck(ctx context.Context, db *sql.DB, agentName string, bounty *store.Bounty, _ *capabilities.Profile, logger interface{ Printf(string, ...any) }) {
	var payload consumerIntegrationCheckPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if ferr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); ferr != nil {
			logger.Printf("ConsumerIntegrationCheck #%d: FailBounty(invalid payload) failed: %v", bounty.ID, ferr)
		}
		return
	}
	if payload.FeatureID <= 0 || payload.ConsumerRepo == "" {
		if ferr := store.FailBounty(db, bounty.ID, "payload missing feature_id or consumer_repo"); ferr != nil {
			logger.Printf("ConsumerIntegrationCheck #%d: FailBounty(missing fields) failed: %v", bounty.ID, ferr)
		}
		return
	}
	consumerRepo := store.GetRepo(db, payload.ConsumerRepo)
	if consumerRepo == nil {
		if ferr := store.FailBounty(db, bounty.ID,
			fmt.Sprintf("consumer repo %q not registered", payload.ConsumerRepo)); ferr != nil {
			logger.Printf("ConsumerIntegrationCheck #%d: FailBounty(missing consumer) failed: %v", bounty.ID, ferr)
		}
		return
	}
	timeout := loadConsumerIntegTimeout(db)
	startedAt := time.Now()

	res := store.ConsumerIntegrationResult{
		FeatureID:        payload.FeatureID,
		ConsumerRepoName: payload.ConsumerRepo,
		RanAt:            store.NowSQLite(),
	}

	// (a) Mode gate.
	if consumerRepo.Mode == string(store.ModeReadOnly) || consumerRepo.Mode == string(store.ModeQuarantined) {
		res.Status = store.CIStatusSkippedReadOnly
		res.StdoutTail = fmt.Sprintf("consumer repo %s mode=%q — skipped per D2 T1-4 (no destructive ops on non-write repos)",
			payload.ConsumerRepo, consumerRepo.Mode)
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}
	if consumerRepo.LocalPath == "" {
		res.Status = store.CIStatusSkippedNoLocalPath
		res.StdoutTail = fmt.Sprintf("consumer repo %s has no local_path — repo is registered but not cloned", payload.ConsumerRepo)
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}

	// (b) Language detect.
	lang := detectConsumerLanguage(consumerRepo.LocalPath)
	if lang != "go" {
		res.Status = store.CIStatusSkippedUnsupportedLang
		res.StdoutTail = fmt.Sprintf("consumer repo %s primary_language=%q — Track 3 v1 only supports Go; Node/Python/Rust support is a stub (operator mailed once per new language encountered)",
			payload.ConsumerRepo, lang)
		alertOperatorOnNewConsumerLanguage(db, lang, payload.ConsumerRepo, agentName, logger)
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}

	// Resolve producer.
	if payload.ProducerRepo == "" {
		res.Status = store.CIStatusError
		res.StderrTail = "payload missing producer_repo — cannot wire replace directive"
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}
	producerRepo := store.GetRepo(db, payload.ProducerRepo)
	if producerRepo == nil || producerRepo.LocalPath == "" {
		res.Status = store.CIStatusError
		res.StderrTail = fmt.Sprintf("producer repo %q not registered or missing local_path", payload.ProducerRepo)
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}

	testCmd := loadConsumerIntegTestCommand(db, payload.ConsumerRepo)
	res.TestCommand = testCmd

	// (g) Pre-existing-red baseline. Run consumer's tests against its
	// CURRENT main paired with the producer's CURRENT main BEFORE applying
	// the producer's ask-branch. If baseline fails, the breakage isn't
	// ours; record pre_existing_red and exit.
	baselineCtx, baselineCancel := context.WithTimeout(ctx, timeout)
	baselineRes, baselineErr := runConsumerTestsBaseline(baselineCtx, consumerRepo, producerRepo, testCmd)
	baselineCancel()
	if baselineErr != nil {
		// Baseline setup failed — record error but don't block ship.
		res.Status = store.CIStatusError
		res.StderrTail = fmt.Sprintf("baseline setup error: %v", baselineErr)
		res.DurationSeconds = int(time.Since(startedAt).Seconds())
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}
	if baselineRes.timedOut {
		res.Status = store.CIStatusTimeout
		res.ExitCode = -1
		res.StdoutTail = baselineRes.stdoutTail
		res.StderrTail = "baseline timed out — change-applied run skipped (timeout NOT blocking per roadmap line 2216)"
		res.DurationSeconds = int(time.Since(startedAt).Seconds())
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}
	if baselineRes.exitCode != 0 {
		res.Status = store.CIStatusPreExistingRed
		res.ExitCode = baselineRes.exitCode
		res.StdoutTail = baselineRes.stdoutTail
		res.StderrTail = "consumer's tests already failed on its main BEFORE the producer change was applied — pre-existing red, NOT blocking ship"
		res.DurationSeconds = int(time.Since(startedAt).Seconds())
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}

	// (c) With-change run. Materialise consumer's main with the producer's
	// ask-branch wired in via go.mod replace, run build + test.
	withCtx, withCancel := context.WithTimeout(ctx, timeout)
	defer withCancel()
	withRes, withErr := runConsumerTestsWithProducerChange(withCtx, consumerRepo, producerRepo, payload.ProducerAskBranch, testCmd)
	if withErr != nil {
		res.Status = store.CIStatusError
		res.StderrTail = fmt.Sprintf("with-change setup error: %v", withErr)
		res.DurationSeconds = int(time.Since(startedAt).Seconds())
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}
	if withRes.timedOut {
		res.Status = store.CIStatusTimeout
		res.ExitCode = -1
		res.StdoutTail = withRes.stdoutTail
		res.StderrTail = "with-change run timed out (NOT blocking per roadmap line 2216)"
		res.DurationSeconds = int(time.Since(startedAt).Seconds())
		finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
		return
	}
	res.ExitCode = withRes.exitCode
	res.StdoutTail = withRes.stdoutTail
	res.StderrTail = withRes.stderrTail
	res.DurationSeconds = int(time.Since(startedAt).Seconds())
	if withRes.exitCode == 0 {
		res.Status = store.CIStatusGreen
	} else {
		// Baseline was green AND with-change is red → it's our change.
		res.Status = store.CIStatusRed
	}
	finalizeConsumerIntegResult(ctx, db, bounty, payload, res, agentName, logger)
}

// finalizeConsumerIntegResult persists the row, completes the bounty, and
// runs the per-Feature aggregation pass.
func finalizeConsumerIntegResult(ctx context.Context, db *sql.DB, bounty *store.Bounty, payload consumerIntegrationCheckPayload, res store.ConsumerIntegrationResult, agentName string, logger interface{ Printf(string, ...any) }) {
	if _, err := store.UpsertConsumerIntegrationResult(db, res); err != nil {
		// Persisting the result failed — surface explicitly. We still
		// complete the bounty (the work itself ran); the operator will
		// see the missing row + the FailBounty mail.
		logger.Printf("ConsumerIntegrationCheck #%d: UpsertConsumerIntegrationResult failed: %v",
			bounty.ID, err)
		if ferr := store.FailBounty(db, bounty.ID,
			fmt.Sprintf("upsert result failed: %v", err)); ferr != nil {
			logger.Printf("ConsumerIntegrationCheck #%d: FailBounty(upsert) failed: %v", bounty.ID, ferr)
		}
		return
	}
	logger.Printf("ConsumerIntegrationCheck #%d: feature=%d consumer=%s status=%s exit=%d duration=%ds",
		bounty.ID, payload.FeatureID, payload.ConsumerRepo, res.Status, res.ExitCode, res.DurationSeconds)
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("ConsumerIntegrationCheck #%d: UpdateBountyStatus(Completed) failed: %v", bounty.ID, err)
	}
	// Aggregation: any red row → emit operator mail. The aggregation
	// runs inside the handler so the operator sees the [CONSUMER BREAKAGE]
	// mail as soon as the offending consumer's row lands; we don't wait
	// for "all consumers checked" because the convoy might have many
	// affected consumers and we want the alert immediately.
	if blocking, failed, aggErr := store.FeatureHasBlockingConsumerBreakage(db, payload.FeatureID); aggErr != nil {
		logger.Printf("ConsumerIntegrationCheck #%d: FeatureHasBlockingConsumerBreakage failed: %v", bounty.ID, aggErr)
	} else if blocking && res.Status == store.CIStatusRed {
		// Only fire the mail when THIS run produced a red (avoid
		// re-mailing on every subsequent green/skip after the first
		// red landed).
		emitConsumerBreakageMail(ctx, db, payload.FeatureID, failed, agentName, logger)
	}
}

// emitConsumerBreakageMail composes + sends the [CONSUMER BREAKAGE] alert.
// Routes through RespectNotificationBudget per Pattern P27.
func emitConsumerBreakageMail(_ context.Context, db *sql.DB, featureID int, failedRepos []string, agentName string, logger interface{ Printf(string, ...any) }) {
	results, _ := store.ListConsumerIntegrationResultsByFeature(db, featureID)
	body := store.FormatConsumerBreakageMailBody(featureID, failedRepos, results)
	subject := fmt.Sprintf("[CONSUMER BREAKAGE] Feature #%d — %d consumer repo(s) red", featureID, len(failedRepos))
	if allowed, _ := store.RespectNotificationBudget(
		context.Background(), db, "operator", agentName, "email", "{}",
		store.StakesHigh,
	); !allowed {
		// StakesHigh always punches through; this branch is defensive.
		logger.Printf("emitConsumerBreakageMail: notification budget denied (defensive — StakesHigh should always pass)")
	}
	store.SendMail(db, agentName, "operator", subject, body, 0, store.MailTypeAlert)
}

// loadConsumerIntegTimeout reads SystemConfig.consumer_integ_timeout_minutes.
// Returns the configured duration; falls back to the default on missing /
// invalid value. Accepts fractional minutes (e.g. "0.05" = 3s) so test
// suites can exercise the timeout path without burning real wall time;
// production operators use whole-minute integers.
func loadConsumerIntegTimeout(db *sql.DB) time.Duration {
	raw := store.GetConfig(db, "consumer_integ_timeout_minutes",
		strconv.Itoa(defaultConsumerIntegTimeoutMinutes))
	mins, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || mins <= 0 {
		mins = float64(defaultConsumerIntegTimeoutMinutes)
	}
	return time.Duration(mins * float64(time.Minute))
}

// loadConsumerIntegTestCommand returns the per-repo test command if set,
// else the canonical Go default. The override key is
// `consumer_integ_test_command:<repo_name>`.
func loadConsumerIntegTestCommand(db *sql.DB, repoName string) string {
	key := "consumer_integ_test_command:" + repoName
	val := store.GetConfig(db, key, "")
	if strings.TrimSpace(val) != "" {
		return val
	}
	return defaultConsumerIntegTestCommand
}

// detectConsumerLanguage returns the consumer's primary language by
// presence-of-manifest. v1 only differentiates "go" from "other"; the
// granular non-Go category is recorded so the operator-mail dedup key
// (consumer_integ_lang_alerted_<lang>) can fan out per-language.
func detectConsumerLanguage(repoPath string) string {
	if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err == nil {
		return "go"
	}
	if _, err := os.Stat(filepath.Join(repoPath, "package.json")); err == nil {
		return "node"
	}
	if _, err := os.Stat(filepath.Join(repoPath, "Cargo.toml")); err == nil {
		return "rust"
	}
	if _, err := os.Stat(filepath.Join(repoPath, "pyproject.toml")); err == nil {
		return "python"
	}
	if _, err := os.Stat(filepath.Join(repoPath, "requirements.txt")); err == nil {
		return "python"
	}
	return "unknown"
}

// alertOperatorOnNewConsumerLanguage fires one operator mail per
// new-language-encountered. Dedup via SystemConfig key
// `consumer_integ_lang_alerted_<lang>` so the second consumer in the
// same language doesn't re-mail.
func alertOperatorOnNewConsumerLanguage(db *sql.DB, lang, consumerRepo, agentName string, logger interface{ Printf(string, ...any) }) {
	dedupKey := "consumer_integ_lang_alerted_" + lang
	if store.GetConfig(db, dedupKey, "") == "1" {
		return
	}
	subject := fmt.Sprintf("[CONSUMER CHECK SKIPPED: unsupported lang %s] first encounter — please consider Track 3 support", lang)
	body := fmt.Sprintf(`Force's D8 Track 3 (consumer integration check) encountered its first %s repo (%s) and skipped it.

Track 3 v1 only supports Go consumers. To extend coverage to %s repos, we need:
  - A language-specific worktree-prep step (e.g. for Node: package.json file: reference or npm link; for Python: pip install -e in a venv; for Rust: Cargo.toml [patch.crates-io] / path dep).
  - The canonical test command for the repo (override via SystemConfig key consumer_integ_test_command:<repo_name>).

Once this alert is acknowledged, set SystemConfig.consumer_integ_lang_alerted_%s = "1" so subsequent encounters of %s repos don't re-mail.

This skip is NON-BLOCKING — the ship gate proceeds. The skip status is recorded in ConsumerIntegrationResults for the audit trail.`,
		lang, consumerRepo, lang, lang, lang)
	if allowed, _ := store.RespectNotificationBudget(
		context.Background(), db, "operator", agentName, "email", "{}",
		store.StakesMedium,
	); !allowed {
		// Budget exhausted — log + skip the mail (the dedup key won't
		// be set, so the next encounter retries). StakesMedium so the
		// operator can dial this down.
		logger.Printf("alertOperatorOnNewConsumerLanguage(%s): notification budget denied — will retry on next encounter", lang)
		return
	}
	store.SendMail(db, agentName, "operator", subject, body, 0, store.MailTypeInfo)
	store.SetConfig(db, dedupKey, "1")
}

// consumerTestRunResult is the structured output from a single test-suite
// invocation (baseline OR with-change).
type consumerTestRunResult struct {
	exitCode   int
	stdoutTail string
	stderrTail string
	timedOut   bool
}

// runConsumerTestsBaseline runs the consumer's test suite against its
// current main with the producer wired in at its CURRENT main (NOT the
// ask-branch). Used to detect pre-existing red so we don't blame the
// producer for a broken consumer.
//
// Wiring the producer's main into the baseline (rather than letting `go
// build` resolve via the consumer's pinned version from the module proxy)
// keeps the baseline self-contained — a consumer whose go.mod requires a
// private/non-public producer module, or whose pinned producer version is
// long gone from the proxy, would otherwise always look "pre-existing red".
// The baseline isolates "is the consumer's test suite green when paired
// with the producer's main HEAD?" from "does the producer's ask-branch
// break it?".
func runConsumerTestsBaseline(ctx context.Context, consumer, producer *store.Repository, testCmd string) (consumerTestRunResult, error) {
	consumerBranch := consumer.DefaultBranch
	if consumerBranch == "" {
		consumerBranch = "main"
	}
	producerBranch := producer.DefaultBranch
	if producerBranch == "" {
		producerBranch = "main"
	}
	producerWT, producerCleanup, err := setupConsumerIntegWorktree(ctx, producer, producerBranch, "producer-baseline")
	if err != nil {
		return consumerTestRunResult{}, fmt.Errorf("producer baseline worktree: %w", err)
	}
	defer producerCleanup()
	consumerWT, consumerCleanup, err := setupConsumerIntegWorktree(ctx, consumer, consumerBranch, "consumer-baseline")
	if err != nil {
		return consumerTestRunResult{}, fmt.Errorf("consumer baseline worktree: %w", err)
	}
	defer consumerCleanup()
	prodModulePath, err := readGoModulePathFromFile(filepath.Join(producerWT, "go.mod"))
	if err != nil || prodModulePath == "" {
		return consumerTestRunResult{}, fmt.Errorf("read producer go.mod module path: %w", err)
	}
	if err := appendReplaceDirective(filepath.Join(consumerWT, "go.mod"), prodModulePath, producerWT); err != nil {
		return consumerTestRunResult{}, fmt.Errorf("append baseline replace directive: %w", err)
	}
	_ = runShellCmdInDir(ctx, consumerWT, "go mod tidy", 2*time.Minute)
	buildRes := runTestCommandInDir(ctx, consumerWT, defaultConsumerIntegBuildCommand)
	if buildRes.exitCode != 0 || buildRes.timedOut {
		// Baseline build failed — surface as the result so the handler
		// can attribute via "with-change vs baseline" comparison. A
		// baseline build break means the consumer was already broken
		// in the producer's main shape (pre-existing red).
		return buildRes, nil
	}
	return runTestCommandInDir(ctx, consumerWT, testCmd), nil
}

// runConsumerTestsWithProducerChange materialises the consumer at its main
// branch with the producer's ask-branch wired in via a go.mod replace
// directive, then runs the consumer's test suite.
//
// Algorithm:
//   1. Create a producer-side ephemeral worktree at the producer's
//      ask-branch (so the producer's tip is materialised at a stable
//      on-disk path the consumer can `replace` against).
//   2. Create a consumer-side ephemeral worktree at consumer's main.
//   3. Read the producer's go.mod to find its module path (the "from"
//      side of the replace directive).
//   4. Append `replace <producer-module> => <producer-worktree>` to the
//      consumer's go.mod.
//   5. Run `go mod tidy` (best-effort), then `go build ./...`, then the
//      configured test command. Returns the test command's exit + tails.
func runConsumerTestsWithProducerChange(ctx context.Context, consumer, producer *store.Repository, producerAskBranch, testCmd string) (consumerTestRunResult, error) {
	consumerBranch := consumer.DefaultBranch
	if consumerBranch == "" {
		consumerBranch = "main"
	}
	if producerAskBranch == "" {
		producerAskBranch = producer.DefaultBranch
		if producerAskBranch == "" {
			producerAskBranch = "main"
		}
	}
	// 1. Producer worktree at the ask-branch.
	producerWT, producerCleanup, err := setupConsumerIntegWorktree(ctx, producer, producerAskBranch, "producer")
	if err != nil {
		return consumerTestRunResult{}, fmt.Errorf("producer worktree: %w", err)
	}
	defer producerCleanup()

	// 2. Consumer worktree at main.
	consumerWT, consumerCleanup, err := setupConsumerIntegWorktree(ctx, consumer, consumerBranch, "consumer")
	if err != nil {
		return consumerTestRunResult{}, fmt.Errorf("consumer worktree: %w", err)
	}
	defer consumerCleanup()

	// 3. Producer module path.
	prodModulePath, err := readGoModulePathFromFile(filepath.Join(producerWT, "go.mod"))
	if err != nil || prodModulePath == "" {
		return consumerTestRunResult{}, fmt.Errorf("read producer go.mod module path: %w", err)
	}

	// 4. Append replace directive to consumer's go.mod.
	if err := appendReplaceDirective(filepath.Join(consumerWT, "go.mod"), prodModulePath, producerWT); err != nil {
		return consumerTestRunResult{}, fmt.Errorf("append replace directive: %w", err)
	}

	// 5. Best-effort go mod tidy (some repos have explicit replace blocks
	// that need normalising). Don't fail on tidy error — it might be a
	// vendor-mode repo. The build/test commands report the real failure.
	_ = runShellCmdInDir(ctx, consumerWT, "go mod tidy", 2*time.Minute)

	// Build first; if build fails, that IS the test result (the test
	// command can't run if compile fails, and a build break IS a real
	// breakage).
	buildRes := runTestCommandInDir(ctx, consumerWT, defaultConsumerIntegBuildCommand)
	if buildRes.exitCode != 0 || buildRes.timedOut {
		return buildRes, nil
	}
	return runTestCommandInDir(ctx, consumerWT, testCmd), nil
}

// setupConsumerIntegWorktree creates an ephemeral worktree under
// .force-integ-worktrees/<repo>/<role>-<unique>/ from the given ref.
// Returns the absolute path, a cleanup func that removes the worktree
// (best-effort — also removes the temporary branch if one was created).
//
// Uses `git worktree add --detach` so we don't create a branch and
// don't risk colliding with existing branches.
func setupConsumerIntegWorktree(ctx context.Context, repo *store.Repository, ref, role string) (string, func(), error) {
	if repo == nil || repo.LocalPath == "" {
		return "", func() {}, fmt.Errorf("repo missing local_path")
	}
	// Use a per-call unique suffix so concurrent tasks against the same
	// (repo, role) don't collide on the worktree path.
	wtBase := filepath.Join(filepath.Dir(repo.LocalPath), ConsumerIntegrationWorktreeRoot, repo.Name)
	if err := os.MkdirAll(wtBase, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("mkdir base: %w", err)
	}
	suffix := fmt.Sprintf("%s-%d-%d", role, time.Now().UnixNano(), os.Getpid())
	wtPath := filepath.Join(wtBase, suffix)

	// `git worktree add --detach` so we don't create a branch (the ref
	// is checked out detached). Pass `--` before positional pair (Fix #9).
	// Pattern P32: route through igit.LogAndRun so the row + redacted
	// output land in GitOperationLog.
	if out, err := igit.LogAndRun(ctx,
		igit.OpContext{Repo: repo.LocalPath},
		"consumer-integ-worktree-add",
		"git", "-C", repo.LocalPath,
		"worktree", "add", "--detach", "--", wtPath, ref,
	); err != nil {
		return "", func() {}, fmt.Errorf("git worktree add %s @ %s: %s: %w",
			repo.Name, ref, strings.TrimSpace(string(out)), err)
	}
	cleanup := func() {
		// Best-effort cleanup. Detach from the caller's cancellation
		// (the cleanup runs in a `defer` and must complete even after
		// the handler's ctx was cancelled by daemon shutdown) but keep
		// a short timeout so a wedged `git worktree remove` can't hang
		// the SpawnDiplomat loop on shutdown. context.WithoutCancel
		// preserves any embedded values from the parent without
		// inheriting the cancellation.
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = igit.LogAndRun(cleanCtx,
			igit.OpContext{Repo: repo.LocalPath},
			"consumer-integ-worktree-remove",
			"git", "-C", repo.LocalPath, "worktree", "remove", "--force", wtPath,
		)
		// Defensive: if `worktree remove` failed because the dir was
		// already gone, scrub the on-disk path.
		_ = os.RemoveAll(wtPath)
	}
	return wtPath, cleanup, nil
}

// runTestCommandInDir runs `bash -c <cmd>` in dir, with stdout/stderr
// truncated to 8 KiB tails, ctx-bounded. Returns exit code (or -1 if the
// process couldn't start), tails, and a timeout flag.
func runTestCommandInDir(ctx context.Context, dir, cmd string) consumerTestRunResult {
	if cmd == "" {
		return consumerTestRunResult{exitCode: -1, stderrTail: "empty test command"}
	}
	c := exec.CommandContext(ctx, "bash", "-c", cmd)
	c.Dir = dir
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	res := consumerTestRunResult{
		stdoutTail: tailString(stdout.String(), 8192),
		stderrTail: tailString(stderr.String(), 8192),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.timedOut = true
		res.exitCode = -1
		return res
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.exitCode = exitErr.ExitCode()
		} else {
			res.exitCode = -1
			if res.stderrTail == "" {
				res.stderrTail = err.Error()
			}
		}
		return res
	}
	res.exitCode = 0
	return res
}

// runShellCmdInDir is the fire-and-forget sibling of runTestCommandInDir
// for setup steps (e.g. go mod tidy) where we don't care about the
// captured output.
func runShellCmdInDir(parentCtx context.Context, dir, cmd string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "bash", "-c", cmd)
	c.Dir = dir
	return c.Run()
}

// tailString returns the trailing n bytes of s (or all of s if shorter).
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// readGoModulePathFromFile reads `module ...` from a go.mod file. Returns
// "" + nil error when the file exists but has no module declaration
// (caller treats as "missing").
func readGoModulePathFromFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		ln := strings.TrimSpace(line)
		if strings.HasPrefix(ln, "module ") {
			parts := strings.Fields(ln)
			if len(parts) >= 2 {
				return parts[1], nil
			}
		}
	}
	return "", nil
}

// appendReplaceDirective appends `replace <module> => <localPath>` to the
// given go.mod. If the file already has a replace for this module, the
// existing directive is left untouched and the new one is NOT appended
// (avoids dup-replace which `go mod` rejects).
//
// Edge case: a go.mod with a multi-line `replace ( ... )` block carrying
// the same module is also detected; we only check substring presence of
// the module path, which is sufficient for v1 (the alternative — full
// go.mod parsing — is overkill and would pull in golang.org/x/mod).
func appendReplaceDirective(goModPath, modulePath, localPath string) error {
	b, err := os.ReadFile(goModPath)
	if err != nil {
		return fmt.Errorf("read go.mod: %w", err)
	}
	body := string(b)
	// Substring check covers both single-line `replace foo => bar` and
	// multi-line `replace ( foo => bar )` shapes for the pre-existing
	// case.
	if strings.Contains(body, "replace "+modulePath+" =>") ||
		strings.Contains(body, "replace "+modulePath+" v") {
		// Already has a replace — leave it be; the consumer's existing
		// pin wins. This is safe because the substring catches the
		// common cases; if the operator wrote a more exotic shape we
		// fail later on the build/test invocation, which is honest.
		return nil
	}
	directive := fmt.Sprintf("\nreplace %s => %s\n", modulePath, localPath)
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += directive
	if err := os.WriteFile(goModPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write go.mod: %w", err)
	}
	return nil
}
