// Package store: D8 Track 3 — ConsumerIntegrationResults read/write helpers.
//
// One row per (feature_id, consumer_repo_name) pair, written by the Diplomat
// runConsumerIntegrationCheck handler after testing a consumer repo against
// the producer's ask-branch. Schema is declared in createSchema +
// runMigrations + schema/schema.sql; TestSchemaParity enforces three-way
// agreement.
//
// Per CLAUDE.md "no silent failures": every mutator returns error.
package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// ConsumerIntegrationStatus is the status enum stored in
// ConsumerIntegrationResults.status. Defined as TEXT (not CHECK-constrained)
// so future statuses don't require a destructive migration; the helper
// constants below are the canonical vocabulary the handler should use.
const (
	// CIStatusGreen — consumer's tests passed against producer's change.
	CIStatusGreen = "green"
	// CIStatusRed — consumer's tests failed against producer's change AND
	// pre-existing-red baseline showed the same suite was green on consumer's
	// main. This blocks the ship gate.
	CIStatusRed = "red"
	// CIStatusPreExistingRed — consumer's tests were already red on its main
	// before the producer change was applied. NOT blocking.
	CIStatusPreExistingRed = "pre_existing_red"
	// CIStatusSkippedReadOnly — consumer repo is in read_only or quarantined
	// mode (D2 T1-4). NOT blocking.
	CIStatusSkippedReadOnly = "skipped_read_only"
	// CIStatusSkippedUnsupportedLang — consumer repo's primary language is
	// not Go. v1 stub; an operator-mail fires once per new-language-encountered.
	CIStatusSkippedUnsupportedLang = "skipped_unsupported_lang"
	// CIStatusSkippedNoLocalPath — consumer repo has no local_path on disk
	// (e.g. registered but never cloned). NOT blocking; the operator sees a
	// distinct status so they can fix the registration.
	CIStatusSkippedNoLocalPath = "skipped_no_local_path"
	// CIStatusTimeout — per-consumer test budget exceeded. NOT blocking
	// (operator interprets, per roadmap line 2216).
	CIStatusTimeout = "timeout"
	// CIStatusError — handler hit an error before/while running the consumer
	// tests (e.g. worktree-add failed). NOT blocking; the operator sees the
	// stderr_tail for diagnosis.
	CIStatusError = "error"
)

// ConsumerIntegrationResult is one row in ConsumerIntegrationResults.
type ConsumerIntegrationResult struct {
	ID               int
	FeatureID        int
	ConsumerRepoName string
	TestCommand      string
	ExitCode         int
	Status           string
	StdoutTail       string
	StderrTail       string
	DurationSeconds  int
	RanAt            string
}

// IsBlockingCIStatus reports whether the given status should block the
// ship gate. Only CIStatusRed blocks; everything else (green, skipped_*,
// pre_existing_red, timeout, error) is non-blocking by roadmap definition
// (line 2214-2222).
func IsBlockingCIStatus(status string) bool {
	return status == CIStatusRed
}

// UpsertConsumerIntegrationResult inserts a new row or replaces an existing
// (feature_id, consumer_repo_name) row. The UNIQUE constraint on the table
// makes INSERT OR REPLACE the natural shape for "run once per Feature in
// DraftPROpen, re-queue is a no-op rewrite".
//
// Stdout/stderr tails are caller-truncated to keep the row reasonable; we
// defensively cap at 16 KiB each to bound the worst-case row size if the
// caller forgets.
func UpsertConsumerIntegrationResult(db *sql.DB, r ConsumerIntegrationResult) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("UpsertConsumerIntegrationResult: db is nil")
	}
	if r.FeatureID <= 0 {
		return 0, fmt.Errorf("UpsertConsumerIntegrationResult: feature_id required")
	}
	if r.ConsumerRepoName == "" {
		return 0, fmt.Errorf("UpsertConsumerIntegrationResult: consumer_repo_name required")
	}
	if r.Status == "" {
		return 0, fmt.Errorf("UpsertConsumerIntegrationResult: status required")
	}
	if r.RanAt == "" {
		r.RanAt = NowSQLite()
	}
	const tailCap = 16 * 1024
	if len(r.StdoutTail) > tailCap {
		r.StdoutTail = r.StdoutTail[len(r.StdoutTail)-tailCap:]
	}
	if len(r.StderrTail) > tailCap {
		r.StderrTail = r.StderrTail[len(r.StderrTail)-tailCap:]
	}
	res, err := db.Exec(`INSERT INTO ConsumerIntegrationResults
		(feature_id, consumer_repo_name, test_command, exit_code, status,
		 stdout_tail, stderr_tail, duration_seconds, ran_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(feature_id, consumer_repo_name) DO UPDATE SET
			test_command     = excluded.test_command,
			exit_code        = excluded.exit_code,
			status           = excluded.status,
			stdout_tail      = excluded.stdout_tail,
			stderr_tail      = excluded.stderr_tail,
			duration_seconds = excluded.duration_seconds,
			ran_at           = excluded.ran_at`,
		r.FeatureID, r.ConsumerRepoName, r.TestCommand, r.ExitCode, r.Status,
		r.StdoutTail, r.StderrTail, r.DurationSeconds, r.RanAt)
	if err != nil {
		return 0, fmt.Errorf("UpsertConsumerIntegrationResult(feature=%d, repo=%s): %w",
			r.FeatureID, r.ConsumerRepoName, err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// ListConsumerIntegrationResultsByFeature returns every row for a Feature,
// ordered by consumer_repo_name for deterministic dashboard rendering.
func ListConsumerIntegrationResultsByFeature(db *sql.DB, featureID int) ([]ConsumerIntegrationResult, error) {
	if db == nil {
		return nil, fmt.Errorf("ListConsumerIntegrationResultsByFeature: db is nil")
	}
	rows, err := db.Query(`SELECT id, feature_id, consumer_repo_name, test_command,
		exit_code, status, stdout_tail, stderr_tail, duration_seconds, ran_at
		FROM ConsumerIntegrationResults
		WHERE feature_id = ?
		ORDER BY consumer_repo_name ASC`, featureID)
	if err != nil {
		return nil, fmt.Errorf("ListConsumerIntegrationResultsByFeature(feature=%d): %w", featureID, err)
	}
	defer rows.Close()
	var out []ConsumerIntegrationResult
	for rows.Next() {
		var r ConsumerIntegrationResult
		if sErr := rows.Scan(&r.ID, &r.FeatureID, &r.ConsumerRepoName, &r.TestCommand,
			&r.ExitCode, &r.Status, &r.StdoutTail, &r.StderrTail,
			&r.DurationSeconds, &r.RanAt); sErr != nil {
			log.Printf("ListConsumerIntegrationResultsByFeature: scan: %v", sErr)
			continue
		}
		out = append(out, r)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListConsumerIntegrationResultsByFeature(feature=%d): rows: %w", featureID, rErr)
	}
	return out, nil
}

// FeatureHasBlockingConsumerBreakage reports whether the Feature has any
// row whose status blocks the ship gate (currently only CIStatusRed). Used
// by the aggregation step in runConsumerIntegrationCheck to decide whether
// to emit the [CONSUMER BREAKAGE] mail and block ship-it.
func FeatureHasBlockingConsumerBreakage(db *sql.DB, featureID int) (bool, []string, error) {
	if db == nil {
		return false, nil, fmt.Errorf("FeatureHasBlockingConsumerBreakage: db is nil")
	}
	rows, err := db.Query(`SELECT consumer_repo_name FROM ConsumerIntegrationResults
		WHERE feature_id = ? AND status = ?
		ORDER BY consumer_repo_name ASC`, featureID, CIStatusRed)
	if err != nil {
		return false, nil, fmt.Errorf("FeatureHasBlockingConsumerBreakage(feature=%d): %w", featureID, err)
	}
	defer rows.Close()
	var failed []string
	for rows.Next() {
		var name string
		if sErr := rows.Scan(&name); sErr != nil {
			log.Printf("FeatureHasBlockingConsumerBreakage: scan: %v", sErr)
			continue
		}
		failed = append(failed, name)
	}
	if rErr := rows.Err(); rErr != nil {
		return false, nil, fmt.Errorf("FeatureHasBlockingConsumerBreakage(feature=%d): rows: %w", featureID, rErr)
	}
	return len(failed) > 0, failed, nil
}

// HasConsumerIntegrationResult reports whether the (feature_id, repo) pair
// already has a recorded result. Used by the dispatcher to skip queuing a
// duplicate task for a Feature that's already had one ConsumerIntegrationCheck
// pass run.
func HasConsumerIntegrationResult(db *sql.DB, featureID int, consumerRepo string) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("HasConsumerIntegrationResult: db is nil")
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM ConsumerIntegrationResults
		WHERE feature_id = ? AND consumer_repo_name = ?`,
		featureID, consumerRepo).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("HasConsumerIntegrationResult(feature=%d, repo=%s): %w",
			featureID, consumerRepo, err)
	}
	return n > 0, nil
}

// FormatConsumerBreakageMailBody composes the human-readable body of the
// [CONSUMER BREAKAGE] operator mail emitted when one or more
// ConsumerIntegrationResults rows for a Feature land in CIStatusRed.
func FormatConsumerBreakageMailBody(featureID int, failedRepos []string, results []ConsumerIntegrationResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Feature #%d's consumer integration checks found breakage in %d repo(s):\n\n",
		featureID, len(failedRepos))
	for _, name := range failedRepos {
		fmt.Fprintf(&sb, "  - %s\n", name)
	}
	sb.WriteString("\nPer-consumer detail (latest run):\n\n")
	for _, r := range results {
		if r.Status != CIStatusRed {
			continue
		}
		fmt.Fprintf(&sb, "── %s ── status=%s exit=%d duration=%ds cmd=%q\n",
			r.ConsumerRepoName, r.Status, r.ExitCode, r.DurationSeconds, r.TestCommand)
		if r.StderrTail != "" {
			fmt.Fprintf(&sb, "stderr tail:\n%s\n\n", r.StderrTail)
		} else if r.StdoutTail != "" {
			fmt.Fprintf(&sb, "stdout tail:\n%s\n\n", r.StdoutTail)
		}
	}
	sb.WriteString("\nThe ship gate is BLOCKED until either (a) the producer's ask-branch is updated to keep the consumer green, (b) the operator acknowledges the breakage and proceeds anyway, or (c) the consumer's tests are amended on its main and the result transitions to pre_existing_red on re-run.\n")
	return sb.String()
}
