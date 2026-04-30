// Package analytics implements EC's analysis layer over TaskHistory and
// outcome tables. D3 Phase 3 ships cross-layer disagreement-rate
// computation; later phases (D4+) extend it with promotion-quality
// retros and revert-rate trend lines.
//
// All exported functions take an explicit context.Context for daemon
// cancellation and an explicit *sql.DB so callers can supply the
// holocron handle directly (no global). Disagreement rates are
// computed over rolling windows by joining TaskHistory rows for the
// same task_id at different layers.
package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PairName constants — the canonical pair_name values written to
// DisagreementPairs. Tests assert against these so a typo in a SQL
// query is a build error, not a silent zero.
const (
	PairCaptainCouncilReject  = "captain-council-reject"
	PairCouncilCIFail         = "council-ci-fail"
	PairConvoyReviewCantFix   = "convoy-review-cant-fix"
	PairSenateChancellor      = "senate-chancellor-decline"
	PairOperatorRevert30d     = "operator-revert-30d"
)

// PairResult is the per-pair output of ComputeDisagreementRates.
// Sample count is the denominator (eligible decisions in the window);
// Disagreements is the numerator (decisions that disagreed at the next
// layer). Rate is Disagreements / max(SampleCount, 1).
type PairResult struct {
	PairName       string
	SampleCount    int
	Disagreements  int
	Rate           float64
	WindowStart    string
	WindowEnd      string
	Deferred       bool   // true → not yet supported (e.g. Senate before D4); rate is 0 by definition
	DeferredReason string // human-readable reason
}

// ComputeDisagreementRates walks all known cross-layer pairs and
// returns a map keyed by pair_name → rate. The window argument is the
// rolling-window length (e.g. 24h, 7d, 30d). The window ENDS at "now"
// and starts at now - window.
//
// Pairs whose downstream layer is not yet shipped (e.g. Senate before
// D4) return rate=0 with Deferred=true so the caller can persist the
// row without fabricating a non-zero rate.
//
// Every pair query is a self-contained SQL fragment over TaskHistory +
// the relevant outcome tables; a missing/empty table yields zero
// samples (the dog records that and moves on).
func ComputeDisagreementRates(ctx context.Context, db *sql.DB, window time.Duration) (map[string]PairResult, error) {
	if db == nil {
		return nil, fmt.Errorf("ComputeDisagreementRates: nil db")
	}
	if window <= 0 {
		return nil, fmt.Errorf("ComputeDisagreementRates: window must be positive, got %v", window)
	}

	now := time.Now().UTC()
	windowEnd := now.Format("2006-01-02 15:04:05")
	windowStart := now.Add(-window).Format("2006-01-02 15:04:05")

	out := map[string]PairResult{}

	// Captain → Council reject. Eligible: tasks where Captain recorded
	// outcome="Completed" inside the window. Disagreement: a later
	// TaskHistory row on the same task_id from the council with
	// outcome="Rejected".
	captainCouncil, err := computeCaptainCouncilReject(ctx, db, windowStart, windowEnd)
	if err != nil {
		return out, fmt.Errorf("captain-council-reject: %w", err)
	}
	out[PairCaptainCouncilReject] = captainCouncil

	// Council → CI fail. Eligible: tasks where Council recorded
	// outcome="Completed" or "AwaitingSubPRCI" inside the window.
	// Disagreement: a later TaskHistory row on the same task_id with
	// outcome="Failed" (CI subsequently reported failure).
	councilCI, err := computeCouncilCIFail(ctx, db, windowStart, windowEnd)
	if err != nil {
		return out, fmt.Errorf("council-ci-fail: %w", err)
	}
	out[PairCouncilCIFail] = councilCI

	// ConvoyReview → astromech "can't fix". Eligible: BountyBoard
	// tasks where parent_id points at a convoy-review decision and
	// the task is a fix-task (CodeEdit). Disagreement: the fix task
	// completed with outcome="Failed" (astromech reported it couldn't
	// close the gap). Approximated via TaskHistory.outcome="Failed"
	// over CodeEdit tasks whose creation-time falls in the window.
	convoyReview, err := computeConvoyReviewCantFix(ctx, db, windowStart, windowEnd)
	if err != nil {
		return out, fmt.Errorf("convoy-review-cant-fix: %w", err)
	}
	out[PairConvoyReviewCantFix] = convoyReview

	// Senate → Chancellor declines. Deferred until D4: the Senate
	// agent (per the roadmap) doesn't ship until D4, so there are no
	// rows to aggregate. Persist a deferred row so the dashboard can
	// distinguish "0 samples; awaiting D4" from "we ran the query and
	// nobody disagreed."
	out[PairSenateChancellor] = PairResult{
		PairName:       PairSenateChancellor,
		WindowStart:    windowStart,
		WindowEnd:      windowEnd,
		Deferred:       true,
		DeferredReason: "Senate agent ships in D4; pair will populate then",
	}

	// Operator approve → revert within 30d. Eligible: BountyBoard
	// tasks the operator approved (LogAudit action='operator-approve'
	// or task transitioned to Completed via operator) inside the
	// window. Disagreement: a revert task targeting the same task_id
	// landed within 30 days. Approximated here as: any BountyBoard
	// row with deferred_revert=1 OR revert_target_task_id pointing at
	// the original whose creation falls within 30d of the original.
	operatorRevert, err := computeOperatorRevert30d(ctx, db, windowStart, windowEnd)
	if err != nil {
		return out, fmt.Errorf("operator-revert-30d: %w", err)
	}
	out[PairOperatorRevert30d] = operatorRevert

	return out, nil
}

// PersistDisagreementRates writes the per-pair results to
// DisagreementPairs. UPSERT semantics: a re-tick over the same
// (pair_name, window_start, window_end) overwrites in place so the dog
// is idempotent. Returns error per CLAUDE.md "new mutator policy" — a
// silent failure here would leave the dashboard reading stale rows.
func PersistDisagreementRates(ctx context.Context, db *sql.DB, results map[string]PairResult) error {
	if db == nil {
		return fmt.Errorf("PersistDisagreementRates: nil db")
	}
	for _, r := range results {
		if r.PairName == "" {
			return fmt.Errorf("PersistDisagreementRates: empty pair_name in result")
		}
		_, err := db.ExecContext(ctx, `
			INSERT INTO DisagreementPairs
				(pair_name, window_start, window_end, sample_count, disagreement_count, rate, computed_at)
			VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
			ON CONFLICT(pair_name, window_start, window_end) DO UPDATE SET
				sample_count       = excluded.sample_count,
				disagreement_count = excluded.disagreement_count,
				rate               = excluded.rate,
				computed_at        = datetime('now')
		`,
			r.PairName, r.WindowStart, r.WindowEnd,
			r.SampleCount, r.Disagreements, r.Rate,
		)
		if err != nil {
			return fmt.Errorf("PersistDisagreementRates: upsert %s: %w", r.PairName, err)
		}
	}
	return nil
}

// computeCaptainCouncilReject — Captain approved (outcome Completed),
// then Council subsequently recorded Rejected on the same task_id.
//
// agent name match is prefix-based (`Captain%`/`Council%`) so the
// query works regardless of the per-agent suffix (Captain-1, Council-2,
// or operator-renamed rosters).
func computeCaptainCouncilReject(ctx context.Context, db *sql.DB, windowStart, windowEnd string) (PairResult, error) {
	r := PairResult{
		PairName:    PairCaptainCouncilReject,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
	}
	// Sample: distinct task_ids the captain marked Completed in the window.
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT task_id)
		FROM TaskHistory
		WHERE agent LIKE 'Captain%'
		  AND outcome = 'Completed'
		  AND created_at BETWEEN ? AND ?
	`, windowStart, windowEnd).Scan(&r.SampleCount)
	if err != nil {
		return r, fmt.Errorf("sample query: %w", err)
	}
	// Disagreement: of those tasks, how many had a later Council row
	// with outcome=Rejected.
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT cap.task_id)
		FROM TaskHistory cap
		JOIN TaskHistory cou
		  ON cou.task_id = cap.task_id
		 AND cou.agent LIKE 'Council%'
		 AND cou.outcome = 'Rejected'
		 AND cou.created_at >= cap.created_at
		WHERE cap.agent LIKE 'Captain%'
		  AND cap.outcome = 'Completed'
		  AND cap.created_at BETWEEN ? AND ?
	`, windowStart, windowEnd).Scan(&r.Disagreements)
	if err != nil {
		return r, fmt.Errorf("disagreement query: %w", err)
	}
	r.Rate = computeRate(r.Disagreements, r.SampleCount)
	return r, nil
}

// computeCouncilCIFail — Council approved a diff (Completed or
// AwaitingSubPRCI), then the same task subsequently saw outcome=Failed
// (CI returned red).
func computeCouncilCIFail(ctx context.Context, db *sql.DB, windowStart, windowEnd string) (PairResult, error) {
	r := PairResult{
		PairName:    PairCouncilCIFail,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
	}
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT task_id)
		FROM TaskHistory
		WHERE agent LIKE 'Council%'
		  AND outcome IN ('Completed', 'AwaitingSubPRCI')
		  AND created_at BETWEEN ? AND ?
	`, windowStart, windowEnd).Scan(&r.SampleCount)
	if err != nil {
		return r, fmt.Errorf("sample query: %w", err)
	}
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT cou.task_id)
		FROM TaskHistory cou
		JOIN TaskHistory ci
		  ON ci.task_id = cou.task_id
		 AND ci.outcome = 'Failed'
		 AND ci.created_at >= cou.created_at
		WHERE cou.agent LIKE 'Council%'
		  AND cou.outcome IN ('Completed', 'AwaitingSubPRCI')
		  AND cou.created_at BETWEEN ? AND ?
	`, windowStart, windowEnd).Scan(&r.Disagreements)
	if err != nil {
		return r, fmt.Errorf("disagreement query: %w", err)
	}
	r.Rate = computeRate(r.Disagreements, r.SampleCount)
	return r, nil
}

// computeConvoyReviewCantFix — ConvoyReview spawned a fix task and the
// astromech subsequently failed it.
//
// Approximation: BountyBoard rows of type='CodeEdit' whose parent_id
// points at a row recorded by ConvoyReview, created in the window.
// Disagreement: that fix task ended with TaskHistory.outcome='Failed'.
func computeConvoyReviewCantFix(ctx context.Context, db *sql.DB, windowStart, windowEnd string) (PairResult, error) {
	r := PairResult{
		PairName:    PairConvoyReviewCantFix,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
	}
	// Sample: distinct fix-tasks (CodeEdit child of a ConvoyReview parent)
	// created in the window. ConvoyReview parents are identified by their
	// own type/parent shape — we look for any TaskHistory row from the
	// convoy-review agent (agent LIKE 'ConvoyReview%') on a parent that
	// then spawned a CodeEdit child. Cheaper proxy: just count CodeEdit
	// children whose parent has a ConvoyReview-authored TaskHistory entry.
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT b.id)
		FROM BountyBoard b
		WHERE b.type = 'CodeEdit'
		  AND b.parent_id > 0
		  AND b.created_at BETWEEN ? AND ?
		  AND EXISTS (
		    SELECT 1 FROM TaskHistory th
		    WHERE th.task_id = b.parent_id
		      AND th.agent LIKE 'ConvoyReview%'
		  )
	`, windowStart, windowEnd).Scan(&r.SampleCount)
	if err != nil {
		return r, fmt.Errorf("sample query: %w", err)
	}
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT b.id)
		FROM BountyBoard b
		JOIN TaskHistory fail
		  ON fail.task_id = b.id
		 AND fail.outcome = 'Failed'
		WHERE b.type = 'CodeEdit'
		  AND b.parent_id > 0
		  AND b.created_at BETWEEN ? AND ?
		  AND EXISTS (
		    SELECT 1 FROM TaskHistory th
		    WHERE th.task_id = b.parent_id
		      AND th.agent LIKE 'ConvoyReview%'
		  )
	`, windowStart, windowEnd).Scan(&r.Disagreements)
	if err != nil {
		return r, fmt.Errorf("disagreement query: %w", err)
	}
	r.Rate = computeRate(r.Disagreements, r.SampleCount)
	return r, nil
}

// computeOperatorRevert30d — operator-approved tasks at DraftPROpen
// that were reverted within 30 days.
//
// Approximation: BountyBoard rows that completed in the window
// (status='Completed') and have an associated revert task pointing
// back at them via revert_target_task_id, where the revert task's
// creation falls within 30 days of the approved task's creation.
func computeOperatorRevert30d(ctx context.Context, db *sql.DB, windowStart, windowEnd string) (PairResult, error) {
	r := PairResult{
		PairName:    PairOperatorRevert30d,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
	}
	// Sample: tasks that completed in the window. Use BountyBoard.created_at
	// as the timestamp because all rows have it; status='Completed' is the
	// terminal state operator approval lands on.
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM BountyBoard
		WHERE status = 'Completed'
		  AND created_at BETWEEN ? AND ?
	`, windowStart, windowEnd).Scan(&r.SampleCount)
	if err != nil {
		return r, fmt.Errorf("sample query: %w", err)
	}
	// Disagreement: a revert task exists whose revert_target_task_id
	// matches and whose creation_at is within 30 days of the original.
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT orig.id)
		FROM BountyBoard orig
		JOIN BountyBoard rev
		  ON rev.revert_target_task_id = orig.id
		 AND julianday(rev.created_at) - julianday(orig.created_at) <= 30
		WHERE orig.status = 'Completed'
		  AND orig.created_at BETWEEN ? AND ?
	`, windowStart, windowEnd).Scan(&r.Disagreements)
	if err != nil {
		return r, fmt.Errorf("disagreement query: %w", err)
	}
	r.Rate = computeRate(r.Disagreements, r.SampleCount)
	return r, nil
}

func computeRate(numerator, denominator int) float64 {
	if denominator <= 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
