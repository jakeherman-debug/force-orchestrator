// Package agents — D3 P6B.13 5-min retro generator.
//
// Friday button in Reflection: synthesises a markdown post — top win,
// top frustration (rejections + escalations + flagged annotations),
// suggested experiment for next week. Saves to docs/retros/<date>.md
// (operator commits manually).
//
// Pure-read on the source side; the only filesystem write is the
// markdown draft at docs/retros/<date>.md. No DB writes.

package agents

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// RetroPayload is the structured response surfaced to the dashboard.
type RetroPayload struct {
	Markdown      string `json:"markdown"`
	SuggestedPath string `json:"suggested_path"`
	GeneratedAt   string `json:"generated_at"`
}

// retrosDir is the on-disk root for saved retros. Repo-relative; the
// daemon's CWD is the orchestrator repo root for the dashboard
// process.
const retrosDir = "docs/retros"

// GenerateRetro collects week's signal and produces the markdown
// draft. Pure-read on DB; no live-state mutation.
func GenerateRetro(ctx context.Context, db *sql.DB, now time.Time) (RetroPayload, error) {
	if db == nil {
		return RetroPayload{}, fmt.Errorf("GenerateRetro: nil db")
	}

	weekStart := now.Add(-7 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05")

	// Top frustration: count rejections + escalations + flagged
	// annotations in the trailing 7 days.
	var rejections, escalations, problems int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM BriefingRenders
		  WHERE operator_decision = 'reject' AND rendered_at >= ?`, weekStart,
	).Scan(&rejections)
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM Escalations WHERE created_at >= ?`, weekStart,
	).Scan(&escalations)
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM OperatorEventAnnotations
		  WHERE flag = 'problem' AND noted_at >= ?`, weekStart,
	).Scan(&problems)

	// Top win: ratified PromotionProposals in window
	var ratified int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM PromotionProposals
		  WHERE ratified_at != '' AND ratified_at >= ?`, weekStart,
	).Scan(&ratified)

	// Markdown body — deterministic shape; live-Haiku swap
	// mechanical (replaces this with a CallWithTranscript call).
	var b strings.Builder
	dateLabel := now.UTC().Format("2006-01-02")
	fmt.Fprintf(&b, "# Fleet retro — week ending %s\n\n", dateLabel)

	fmt.Fprintln(&b, "## Top win")
	if ratified > 0 {
		fmt.Fprintf(&b, "%d PromotionProposal%s ratified this week. The fleet learned something.\n\n",
			ratified, plural(ratified))
	} else {
		fmt.Fprintln(&b, "No PromotionProposals ratified this week. Consider whether the fleet is shipping enough learnable signal.")
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "## Top frustration")
	switch {
	case rejections > 0:
		fmt.Fprintf(&b, "%d operator rejection%s in Briefing this week. Likely calibration drift — review the highest-stakes rejections in Drill.\n\n",
			rejections, plural(rejections))
	case escalations > 0:
		fmt.Fprintf(&b, "%d escalation%s opened this week. The fleet asked for help %d times — what could have made the answer obvious?\n\n",
			escalations, plural(escalations), escalations)
	case problems > 0:
		fmt.Fprintf(&b, "%d annotation%s flagged 'problem'. Review them in Reflection's flagged-events panel.\n\n",
			problems, plural(problems))
	default:
		fmt.Fprintln(&b, "No major frustrations recorded this week. (If that doesn't match the operator's experience, consider whether the signal channels are catching what matters.)")
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "## Suggested experiment for next week")
	switch {
	case rejections > 5:
		fmt.Fprintln(&b, "Run an experiment that tightens Captain's reject criteria — paired-run the new prompt against control for 50 decisions and watch the rejection-rate delta.")
	case escalations > 3:
		fmt.Fprintln(&b, "Pick the top escalation pattern and ship a FleetRule that catches the failure mode preemptively. Use the escalation's first-seen evidence as the test fixture.")
	default:
		fmt.Fprintln(&b, "If next week is quiet, use the slack to harden the diagnostic substrate — write 2-3 Drill view tests against real convoy histories.")
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Week's stats")
	fmt.Fprintf(&b, "- PromotionProposals ratified: %d\n", ratified)
	fmt.Fprintf(&b, "- Briefing rejections: %d\n", rejections)
	fmt.Fprintf(&b, "- Escalations opened: %d\n", escalations)
	fmt.Fprintf(&b, "- Annotations flagged 'problem': %d\n", problems)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "_Generated %s by force-orchestrator's 5-min retro helper. Edit then commit to docs/retros/._\n",
		now.UTC().Format("2006-01-02 15:04 UTC"))

	suggestedPath := filepath.Join(retrosDir, dateLabel+".md")
	return RetroPayload{
		Markdown:      b.String(),
		SuggestedPath: suggestedPath,
		GeneratedAt:   store.NowSQLite(),
	}, nil
}

// SaveRetroDraft writes the markdown to its suggested path. Returns
// the absolute path written. Operator commits manually.
//
// Anti-cheat: the path is constructed from suggestedPath only if it
// starts with the canonical retrosDir prefix; an attacker-controlled
// path is refused.
func SaveRetroDraft(suggestedPath, markdown string) (string, error) {
	if suggestedPath == "" {
		return "", fmt.Errorf("SaveRetroDraft: empty path")
	}
	cleaned := filepath.Clean(suggestedPath)
	if !strings.HasPrefix(cleaned, retrosDir+string(filepath.Separator)) && cleaned != retrosDir {
		return "", fmt.Errorf("SaveRetroDraft: path must live under %s/", retrosDir)
	}
	if err := os.MkdirAll(filepath.Dir(cleaned), 0755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(cleaned, []byte(markdown), 0644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	abs, _ := filepath.Abs(cleaned)
	return abs, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
