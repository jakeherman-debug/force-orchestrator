package agents

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/bos"
	_ "force-orchestrator/internal/bos/rules"
	"force-orchestrator/internal/store"
)

// TestCommitPipeline_BoS_Captain_NoRegression — D4 Phase 1 regression
// per docs/roadmap.md § D4 exit criterion 4 ("a task that would have
// merged before D4 still merges after D4 when BoS [...] approves").
//
// Setup: a clean diff (no BoS violations) goes through the post-commit
// hook. After BoSReview runs and approves, the source task remains in
// AwaitingCaptainReview (not returned to Pending). This is the
// "no-regression" gate — BoS must NOT silently disrupt the
// Astromech → Captain → Council flow on clean diffs.
func TestCommitPipeline_BoS_Captain_NoRegression(t *testing.T) {
	db, repoDir := seedDB(t)

	mustGit(t, repoDir, "init", "-q", "-b", "main")
	mustGit(t, repoDir, "config", "user.email", "test@example.com")
	mustGit(t, repoDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repoDir, "seed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "add", ".")
	mustGit(t, repoDir, "commit", "-q", "-m", "seed")

	mustGit(t, repoDir, "checkout", "-q", "-b", "feature/clean")

	// CLEAN diff — no BoS violations.
	cleanRel := "internal/whatever/util.go"
	full := filepath.Join(repoDir, cleanRel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`package whatever

// PureFunctionAddingTwo returns a + b. No store calls, no spawn,
// no destructive git, no convoy_id LIKE — just clean code.
func PureFunctionAddingTwo(a, b int) int {
	return a + b
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "add", ".")
	mustGit(t, repoDir, "commit", "-q", "-m", "clean change")

	// Source task in AwaitingCaptainReview (post-commit, pre-BoS verdict).
	srcTaskID := store.AddBounty(db, 0, "CodeEdit", "[demo] add helper")
	if _, err := db.Exec(`UPDATE BountyBoard SET target_repo = ?, branch_name = ?, status = 'AwaitingCaptainReview' WHERE id = ?`,
		"demo", "feature/clean", srcTaskID); err != nil {
		t.Fatalf("update src task: %v", err)
	}

	srcBounty, err := store.GetBounty(db, srcTaskID)
	if err != nil {
		t.Fatalf("GetBounty: %v", err)
	}
	bosTaskID, err := store.QueueBoSReview(db, srcBounty, "feature/clean", "abcdef")
	if err != nil {
		t.Fatalf("QueueBoSReview: %v", err)
	}
	bosBounty, _ := store.GetBounty(db, bosTaskID)
	row := db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, bosTaskID)
	var payload string
	_ = row.Scan(&payload)
	bosBounty.Payload = payload

	// Run the reviewer.
	runBoSReviewTask(context.Background(), db, "BoS-test", bosBounty, &bosTestLogger{})

	// CRITICAL: source task must STILL be in AwaitingCaptainReview.
	got, err := store.GetBounty(db, srcTaskID)
	if err != nil {
		t.Fatalf("GetBounty src: %v", err)
	}
	if got.Status != "AwaitingCaptainReview" {
		t.Fatalf("regression: clean diff got rerouted to %q (expected AwaitingCaptainReview)", got.Status)
	}

	// No block-severity findings.
	rows, err := store.ListSecurityFindings(db, srcTaskID)
	if err != nil {
		t.Fatalf("ListSecurityFindings: %v", err)
	}
	for _, f := range rows {
		if f.Severity == "block" {
			t.Errorf("regression: clean diff produced block-severity finding: %+v", f)
		}
	}

	// Post-condition: BoSReview infrastructure task is Completed.
	bosRow, err := store.GetBounty(db, bosTaskID)
	if err != nil {
		t.Fatalf("GetBounty bos: %v", err)
	}
	if bosRow.Status != "Completed" {
		t.Fatalf("BoSReview status: got %q, want Completed", bosRow.Status)
	}
	_ = bos.All // keep import live
}
