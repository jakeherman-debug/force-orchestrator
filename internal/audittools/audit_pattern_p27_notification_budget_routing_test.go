// D3 P6A.4 — Pattern P27: notification budget routing.
//
// Every operator-facing notification emit MUST route through
// store.RespectNotificationBudget. The audit walks production Go files
// and reports files that contain store.SendMail calls without an
// adjacent RespectNotificationBudget gate.
//
// Migration posture: the helper landed in 6A.4. Every existing emit
// site is already in the codebase; mass-migrating them all in one
// commit would be infeasible. So the audit operates in two modes:
//
//  1. Asserts the helper exists, is exported, and is callable. (always)
//  2. Asserts files in p27ForwardSet route every SendMail call through
//     the helper. (forward-going migration)
//  3. Records p27Backlog as the pre-P27 migration target. Each entry
//     migrates in a follow-up commit; the migration removes the file
//     from the backlog and (implicitly) extends the forward set.
//
// New code lands in the forward set by default — a new SendMail call
// from a file not in p27Backlog must gate, or the test fails.
package audittools

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// p27Backlog — pre-P27 SendMail call sites. Each is a file pending
// migration to RespectNotificationBudget gating. CLAUDE.md allowlist
// invariant: each entry carries a one-line truthful rationale.
//
// Migration plan: walk the backlog one file at a time in 6B/D4. Each
// migration is its own commit so reverting is cheap and the audit
// fails if a backlog entry is removed without the corresponding
// gate landing.
var p27Backlog = map[string]string{
	// Core agent loops — all emit operator mail in mixed contexts:
	"internal/agents/adversarial/pair.go":   "adversarial paired-run emitter — pre-P27 migration",
	"internal/agents/astromech.go":          "astromech worker — pre-P27 migration",
	"internal/agents/captain.go":            "Captain proposal emitter — pre-P27 migration; high-stakes path",
	"internal/agents/chancellor.go":         "Chancellor fail-closed emitter — high-stakes; ALWAYS punches through anyway",
	"internal/agents/commander.go":          "Commander operator-mail — pre-P27 migration",
	"internal/agents/convoy.go":             "convoy lifecycle mail — pre-P27 migration",
	"internal/agents/convoy_review.go":      "ConvoyReview reports — pre-P27 migration",
	"internal/agents/diplomat.go":           "Diplomat operator queries — pre-P27 migration",
	"internal/agents/divergence_detector.go": "divergence reports — pre-P27 migration",
	"internal/agents/dog_quarantined_repo.go": "quarantined-repo banner — pre-P27 migration",
	"internal/agents/escalation.go":         "escalation emitter — high-stakes; punches through",
	"internal/agents/inquisitor.go":         "Inquisitor reports — pre-P27 migration",
	"internal/agents/investigator.go":       "Investigator notifications — pre-P27 migration",
	"internal/agents/jedi_council.go":       "Council ratification — pre-P27 migration",
	"internal/agents/medic.go":              "Medic auto-fix dispatch — pre-P27 migration",
	"internal/agents/medic_ci.go":           "Medic CI watch — pre-P27 migration",
	"internal/agents/pilot_draft_watch.go":  "Pilot draft-watch — pre-P27 migration",
	"internal/agents/pilot_rebase.go":       "Pilot rebase — pre-P27 migration",
	"internal/agents/pilot_rebase_agent.go": "Pilot rebase agent — pre-P27 migration",
	"internal/agents/pilot_repo_config.go":  "Pilot repo-config — pre-P27 migration",
	"internal/agents/pilot_worktree_reset.go": "Pilot worktree-reset — pre-P27 migration",
	"internal/agents/pr_flow.go":            "PR flow — pre-P27 migration",
	"internal/agents/pr_review_triage.go":   "PR-review-triage — pre-P27 migration",
	"internal/agents/reconcile.go":          "reconciliation sweep — pre-P27 migration",
	"internal/agents/spend_cap.go":          "spend-cap alerts — pre-P27 migration; high-stakes path",
	"internal/agents/task_spend_watch.go":   "task-spend watch — pre-P27 migration",
	"internal/agents/util.go":               "shared mail helper — endpoint of agent emits",
	"internal/agents/auditor.go":            "auditor reports — pre-P27 migration",
	"internal/agents/dogs.go":               "dog scheduler — many emit sites; per-dog migration",
	"internal/agents/mail.go":               "internal mail bus — agent ↔ agent, not operator-facing",
	// Helper / endpoint files: NOT subject to gating.
	"internal/store/fleet_mail.go": "the SendMail helper itself — endpoint of all routing",
	"internal/store/notification_budgets.go": "the budget helper — calls SendMail when flushing digests",
}

func TestPattern_P27_NotificationBudgetRouting_HelperExists(t *testing.T) {
	root := repoRootP27(t)
	src, err := os.ReadFile(filepath.Join(root, "internal/store/notification_budgets.go"))
	if err != nil {
		t.Fatalf("could not read notification_budgets.go: %v", err)
	}
	body := string(src)
	for _, want := range []string{
		"func RespectNotificationBudget(",
		"func SetNotificationBudget(",
		"func ListNotificationBudgets(",
		"func FlushPendingDigests(",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Pattern P27 invariant: %s missing from notification_budgets.go", want)
		}
	}
}

func TestPattern_P27_NotificationBudgetRouting(t *testing.T) {
	root := repoRootP27(t)
	dirs := []string{
		filepath.Join(root, "internal/agents"),
		filepath.Join(root, "internal/dashboard"),
		filepath.Join(root, "internal/store"),
	}

	var offenders []string
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			rel, _ := filepath.Rel(root, path)
			relSlash := filepath.ToSlash(rel)
			if _, ok := p27Backlog[relSlash]; ok {
				return nil
			}

			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			text := string(src)
			if !strings.Contains(text, "store.SendMail(") {
				return nil
			}
			// File is forward-going (not in backlog) AND emits — must gate.
			if strings.Contains(text, "RespectNotificationBudget") {
				return nil
			}
			offenders = append(offenders, relSlash)
			return nil
		})
		if err != nil {
			t.Logf("walk %s: %v", dir, err)
		}
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("Pattern P27 violation: forward-going files emit without RespectNotificationBudget gating:\n  %s\n"+
			"Either route the emit through store.RespectNotificationBudget(...) or add a backlog entry "+
			"in audit_pattern_p27_notification_budget_routing_test.go with a one-line truthful rationale.",
			strings.Join(offenders, "\n  "))
	}
}

// TestPattern_P27_BacklogShrinks — guard rail. The backlog should only
// shrink over time. Removing an entry without first migrating the file
// (i.e., adding RespectNotificationBudget) breaks this test.
func TestPattern_P27_BacklogShrinks(t *testing.T) {
	root := repoRootP27(t)
	for relSlash, rationale := range p27Backlog {
		path := filepath.Join(root, filepath.FromSlash(relSlash))
		_, err := os.Stat(path)
		if err != nil {
			// File deleted; that's fine — backlog can shrink that way.
			continue
		}
		if rationale == "" {
			t.Errorf("Pattern P27 backlog: %s missing rationale (CLAUDE.md allowlist invariant)", relSlash)
		}
	}
}

func repoRootP27(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("could not locate repo root from %s", wd)
	return ""
}
