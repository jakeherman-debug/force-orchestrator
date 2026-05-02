// Package store: per-rule precision metrics for BoS / ISB.
// D4 fix-loop-1 α2 — backs the dashboard "Rule Metrics" view.
//
// A rule's precision is the share of its firings that actually caught a
// real violation. Two failure modes are distinguishable:
//
//   - True-positive (TP): the rule fired AND the operator/system kept
//     the finding (disposition is empty, 'resolved', 'closed', or
//     'escalated' — anything that does NOT downgrade the verdict).
//   - False-positive (FP): the rule fired AND the verdict was downgraded
//     via // BOS-BYPASS / // ISB-BYPASS comment ('overridden') or
//     explicit operator suppression ('suppressed').
//
// The anti-cheat ramp gate (rule must accrue 30 clean firings before it
// graduates from advise → block) is the second derived signal. We
// surface the lifetime-firings count so the dashboard can render
// "X / 30 firings until block-eligible" for an advise-severity rule.
//
// Schema lives in security_findings.go (table SecurityFindings).

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RuleMetrics is the per-rule rollup. Bureau + RuleID identify the rule;
// counts and derived signals describe the firing history.
type RuleMetrics struct {
	Bureau              string  `json:"bureau"`
	RuleID              string  `json:"rule_id"`
	Severity            string  `json:"severity"` // 'advise' | 'block' (most-recent firing's severity)
	TotalFirings        int     `json:"total_firings"`
	TruePositives       int     `json:"true_positives"`
	FalsePositives      int     `json:"false_positives"`
	OverriddenCount     int     `json:"overridden_count"`
	SuppressedCount     int     `json:"suppressed_count"`
	OpenCount           int     `json:"open_count"`
	ResolvedCount       int     `json:"resolved_count"`
	Precision           float64 `json:"precision"` // TP / (TP + FP); 0 when (TP+FP)==0
	Last30DayFirings    int     `json:"last_30_day_firings"`
	FirstFiredAt        string  `json:"first_fired_at"`
	LastFiredAt         string  `json:"last_fired_at"`
	RampStatus          string  `json:"ramp_status"` // 'advise' | 'eligible-for-block' | 'block'
	FiringsToBlockReady int     `json:"firings_to_block_ready"` // 30 - TotalFirings; 0 if already eligible
}

// ComputeRuleMetrics rolls up SecurityFindings for one (bureau, ruleID)
// pair. Returns (nil, nil) if no rows exist for that pair.
//
// Bureau may be empty to query across all bureaus; the returned Bureau
// will be empty in that case (the rule_id collapses across bureaus).
func ComputeRuleMetrics(db *sql.DB, bureau, ruleID string) (*RuleMetrics, error) {
	if ruleID == "" {
		return nil, errors.New("ComputeRuleMetrics: ruleID required")
	}
	q := `
		SELECT IFNULL(severity,'advise') AS severity,
		       COUNT(*) AS total,
		       SUM(CASE WHEN disposition = 'overridden'  THEN 1 ELSE 0 END) AS overridden_n,
		       SUM(CASE WHEN disposition = 'suppressed'  THEN 1 ELSE 0 END) AS suppressed_n,
		       SUM(CASE WHEN disposition = ''            THEN 1 ELSE 0 END) AS open_n,
		       SUM(CASE WHEN disposition = 'resolved'    THEN 1 ELSE 0 END) AS resolved_n,
		       MIN(IFNULL(created_at,'')) AS first_fired,
		       MAX(IFNULL(created_at,'')) AS last_fired
		  FROM SecurityFindings
		 WHERE rule_id = ?`
	args := []any{ruleID}
	if bureau != "" {
		q += " AND bureau = ?"
		args = append(args, bureau)
	}

	var (
		severity, firstFired, lastFired                    string
		total, overriddenN, suppressedN, openN, resolvedN  int
	)
	row := db.QueryRow(q, args...)
	// Use sql.NullInt64 for SUMs (NULL when 0 rows match).
	var (
		nTotal, nOver, nSupp, nOpen, nRes sql.NullInt64
		nSev, nFirst, nLast               sql.NullString
	)
	if err := row.Scan(&nSev, &nTotal, &nOver, &nSupp, &nOpen, &nRes, &nFirst, &nLast); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ComputeRuleMetrics(%s/%s): %w", bureau, ruleID, err)
	}
	total = int(nTotal.Int64)
	if total == 0 {
		return nil, nil
	}
	severity = nSev.String
	overriddenN = int(nOver.Int64)
	suppressedN = int(nSupp.Int64)
	openN = int(nOpen.Int64)
	resolvedN = int(nRes.Int64)
	firstFired = nFirst.String
	lastFired = nLast.String

	// TP/FP definition above:
	//   FP = overridden + suppressed
	//   TP = total - FP
	fp := overriddenN + suppressedN
	tp := total - fp
	precision := 0.0
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}

	// Last-30-day firings — separate query so the rollup stays cheap.
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	var last30 int
	args30 := []any{ruleID, cutoff}
	q30 := `SELECT COUNT(*) FROM SecurityFindings WHERE rule_id = ? AND created_at >= ?`
	if bureau != "" {
		q30 += " AND bureau = ?"
		args30 = append(args30, bureau)
	}
	if err := db.QueryRow(q30, args30...).Scan(&last30); err != nil {
		return nil, fmt.Errorf("ComputeRuleMetrics(%s/%s): last30: %w", bureau, ruleID, err)
	}

	// Ramp status: a rule's anti-cheat gate is "30 clean firings → eligible
	// to graduate from advise → block". A rule already at severity='block'
	// is past the ramp; an advise rule with >=30 TPs is eligible; otherwise
	// the count toward the gate.
	rampStatus := "advise"
	firingsToReady := 30 - tp
	if firingsToReady < 0 {
		firingsToReady = 0
	}
	if severity == "block" {
		rampStatus = "block"
		firingsToReady = 0
	} else if tp >= 30 {
		rampStatus = "eligible-for-block"
		firingsToReady = 0
	}

	return &RuleMetrics{
		Bureau:              bureau,
		RuleID:              ruleID,
		Severity:            severity,
		TotalFirings:        total,
		TruePositives:       tp,
		FalsePositives:      fp,
		OverriddenCount:     overriddenN,
		SuppressedCount:     suppressedN,
		OpenCount:           openN,
		ResolvedCount:       resolvedN,
		Precision:           precision,
		Last30DayFirings:    last30,
		FirstFiredAt:        firstFired,
		LastFiredAt:         lastFired,
		RampStatus:          rampStatus,
		FiringsToBlockReady: firingsToReady,
	}, nil
}

// ListAllRuleMetrics rolls up metrics for every (bureau, rule_id) pair
// that has at least one SecurityFindings row. The dashboard's rule list
// view enumerates this; per-rule drilldowns use ComputeRuleMetrics directly.
//
// Bureau may be empty to span all bureaus.
func ListAllRuleMetrics(db *sql.DB, bureau string) ([]RuleMetrics, error) {
	q := `SELECT DISTINCT bureau, rule_id FROM SecurityFindings WHERE 1=1`
	args := []any{}
	if bureau != "" {
		q += " AND bureau = ?"
		args = append(args, bureau)
	}
	q += " ORDER BY bureau ASC, rule_id ASC"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListAllRuleMetrics: %w", err)
	}
	defer rows.Close()
	type pair struct{ bureau, ruleID string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if scanErr := rows.Scan(&p.bureau, &p.ruleID); scanErr != nil {
			return nil, fmt.Errorf("ListAllRuleMetrics: scan: %w", scanErr)
		}
		pairs = append(pairs, p)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListAllRuleMetrics: rows.Err: %w", rErr)
	}
	out := make([]RuleMetrics, 0, len(pairs))
	for _, p := range pairs {
		m, mErr := ComputeRuleMetrics(db, p.bureau, p.ruleID)
		if mErr != nil {
			return nil, mErr
		}
		if m != nil {
			out = append(out, *m)
		}
	}
	return out, nil
}
