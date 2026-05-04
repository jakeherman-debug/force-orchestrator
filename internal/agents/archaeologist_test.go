package agents

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// TestArchaeologistSweep_HappyPath_PersistsHits seeds a synthetic
// repo with three ARCH-001 hits, queues one ArchaeologistSweep, runs
// the agent loop until the task is Completed, and asserts:
//   - the bounty completes (not Failed)
//   - three ArchaeologistFindings rows land for ARCH-001
//   - all three carry status='open'
func TestArchaeologistSweep_HappyPath_PersistsHits(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	writeArchTestFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nimport \"io/ioutil\"\n\nfunc A() { ioutil.ReadFile(\"x\") }\nfunc B() { ioutil.WriteFile(\"a\", nil, 0) }\nfunc C() { ioutil.WriteFile(\"b\", nil, 0) }\n")
	store.AddRepo(db, "synthetic-1", dir, "synthetic ARCH-001 repo")

	target := mustGetArchTarget(t, db, "synthetic-1")
	taskID, err := store.QueueArchaeologistSweep(db, target.ID, target.Name)
	if err != nil {
		t.Fatalf("QueueArchaeologistSweep: %v", err)
	}
	if taskID == 0 {
		t.Fatal("QueueArchaeologistSweep returned 0 id")
	}

	runArchaeologistOnceUntilComplete(t, db, taskID, librarian.NewMock(), "ArchaeologistSweep")

	hits, err := store.ListOpenArchaeologistFindings(db, "ARCH-001", target.ID)
	if err != nil {
		t.Fatalf("ListOpenArchaeologistFindings: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 ARCH-001 findings, got %d: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.Status != "open" {
			t.Errorf("finding %d: Status = %q, want open", h.ID, h.Status)
		}
	}
}

// TestArchaeologistSweep_DedupsAcrossRuns runs two sweeps back-to-back
// against the same repo and asserts the second pass detects zero new
// rows (UNIQUE constraint dedup'd them).
func TestArchaeologistSweep_DedupsAcrossRuns(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	writeArchTestFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nimport \"io/ioutil\"\n\nfunc A() { ioutil.ReadFile(\"x\") }\n")
	store.AddRepo(db, "synthetic-2", dir, "ARCH-001 dedup repo")
	target := mustGetArchTarget(t, db, "synthetic-2")

	for i := 0; i < 2; i++ {
		id, _ := store.QueueArchaeologistSweep(db, target.ID, target.Name)
		runArchaeologistOnceUntilComplete(t, db, id, librarian.NewMock(), "ArchaeologistSweep")
	}

	hits, _ := store.ListOpenArchaeologistFindings(db, "ARCH-001", target.ID)
	if len(hits) != 1 {
		t.Fatalf("idempotence: expected 1 finding after two sweeps, got %d", len(hits))
	}
}

// TestArchaeologistSweep_FiresProposeMigrationOnThreshold seeds a
// repo with > MinHitsForFeature ARCH-001 hits, spins up the agent
// loop, and asserts the chained pipeline lands its end-state:
//   - the sweep handler queued an ArchaeologistProposeMigration task.
//   - the agent loop claimed it and called EmitCandidate exactly once.
//   - all findings flipped to status='proposed'.
//
// The test asserts on the end-state (EmitCandidate count + findings
// status), not on the intermediate queued-Pending row, because the
// same goroutine consumes both task types in turn — by the time the
// sweep completes, the propose-migration task may already be Locked
// or Completed.
func TestArchaeologistSweep_FiresProposeMigrationOnThreshold(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	// 6 hits — well past ARCH-001's MinHitsForFeature=5. Use distinct
	// function shapes (different param/return types and varying body
	// kinds) so ARCH-003 (duplicate-abstractions) does NOT also fire
	// — the test is scoped to the ARCH-001 → propose-migration path.
	body := "package main\n\nimport (\n\t\"io/ioutil\"\n\t\"fmt\"\n)\n"
	body += "func F1() { ioutil.ReadFile(\"x\") }\n"
	body += "func F2(s string) error { _, err := ioutil.ReadFile(s); return err }\n"
	body += "func F3(s string) (int, error) {\n\tb, err := ioutil.ReadFile(s)\n\tif err != nil {\n\t\treturn 0, err\n\t}\n\treturn len(b), nil\n}\n"
	body += "func F4(paths []string) {\n\tfor _, p := range paths {\n\t\tioutil.ReadFile(p)\n\t}\n}\n"
	body += "func F5(p string) string {\n\tb, _ := ioutil.ReadFile(p)\n\treturn string(b)\n}\n"
	body += "func F6() {\n\tdata, _ := ioutil.ReadFile(\"y\")\n\tfmt.Println(data)\n}\n"
	writeArchTestFile(t, filepath.Join(dir, "main.go"), body)
	store.AddRepo(db, "synthetic-3", dir, "ARCH-001 threshold repo")
	target := mustGetArchTarget(t, db, "synthetic-3")

	mock := librarian.NewMock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SpawnArchaeologist(ctx, db, mock, "Archaeologist-test")

	sweepID, _ := store.QueueArchaeologistSweep(db, target.ID, target.Name)

	// Wait for the chained pipeline to settle:
	//   sweep claimed → completed → propose claimed → completed → EmitCandidate called.
	deadline := time.Now().Add(8 * time.Second)
	settled := false
	for time.Now().Before(deadline) {
		var sweepStatus string
		_ = db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, sweepID).Scan(&sweepStatus)
		if sweepStatus == "Failed" {
			t.Fatalf("sweep #%d Failed unexpectedly", sweepID)
		}
		var pending, locked, completed int
		_ = db.QueryRow(`SELECT
			SUM(CASE WHEN status='Pending' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='Locked' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='Completed' THEN 1 ELSE 0 END)
			FROM BountyBoard WHERE type='ArchaeologistProposeMigration'`).Scan(&pending, &locked, &completed)
		if sweepStatus == "Completed" && pending == 0 && locked == 0 && completed >= 1 && len(mock.EmitCalls) >= 1 {
			settled = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !settled {
		t.Fatalf("pipeline did not settle within 8s; emits=%d", len(mock.EmitCalls))
	}

	if len(mock.EmitCalls) != 1 {
		t.Fatalf("expected exactly 1 EmitCandidate call, got %d", len(mock.EmitCalls))
	}
	cand := mock.EmitCalls[0]
	if !strings.HasPrefix(cand.HypothesisKey, "archaeologist-arch-001-") {
		t.Errorf("HypothesisKey = %q, want prefix archaeologist-arch-001-", cand.HypothesisKey)
	}
	if !strings.Contains(cand.HypothesisRaw, "ARCH-001") {
		t.Errorf("HypothesisRaw missing ARCH-001 reference: %s", cand.HypothesisRaw)
	}

	// All open findings should now be flipped to 'proposed'.
	open, _ := store.ListOpenArchaeologistFindings(db, "ARCH-001", target.ID)
	if len(open) != 0 {
		t.Errorf("after EmitCandidate: %d findings still open, expected 0 (all flipped to proposed)", len(open))
	}
}

// TestDogArchaeologistSweep_FansOutPerRepo registers two repos (one
// opted-out) and asserts the sweep dog queues exactly one
// ArchaeologistSweep task — for the active repo only.
func TestDogArchaeologistSweep_FansOutPerRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dirA := t.TempDir()
	dirB := t.TempDir()
	store.AddRepo(db, "active-repo", dirA, "active")
	store.AddRepo(db, "opted-out-repo", dirB, "opted out")
	if err := store.SetArchaeologistSweepDisabled(db, "opted-out-repo", true); err != nil {
		t.Fatalf("SetArchaeologistSweepDisabled: %v", err)
	}

	if err := dogArchaeologistSweep(context.Background(), db, &silentArchLogger{}); err != nil {
		t.Fatalf("dogArchaeologistSweep: %v", err)
	}

	rows, err := db.Query(`SELECT target_repo FROM BountyBoard WHERE type = 'ArchaeologistSweep' AND status = 'Pending'`)
	if err != nil {
		t.Fatalf("query queued sweeps: %v", err)
	}
	defer rows.Close()
	var queued []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		queued = append(queued, r)
	}
	if len(queued) != 1 || queued[0] != "active-repo" {
		t.Fatalf("expected 1 queued sweep for active-repo, got %v", queued)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// mustGetArchTarget wraps store.ListArchaeologistSweepTargets to
// resolve the target by name. Fails the test on lookup error.
func mustGetArchTarget(t *testing.T, db *sql.DB, name string) store.ArchaeologistRepoTarget {
	t.Helper()
	targets, err := store.ListArchaeologistSweepTargets(db)
	if err != nil {
		t.Fatalf("ListArchaeologistSweepTargets: %v", err)
	}
	for _, target := range targets {
		if target.Name == name {
			return target
		}
	}
	t.Fatalf("repo %q not found in sweep targets", name)
	return store.ArchaeologistRepoTarget{}
}

// runArchaeologistOnceUntilComplete spins up a SpawnArchaeologist
// goroutine, waits up to 5s for the supplied taskID to flip to
// Completed (or Failed — surfaces as test failure), then cancels the
// goroutine and returns. taskType is used only for error messages.
func runArchaeologistOnceUntilComplete(t *testing.T, db *sql.DB, taskID int, lib librarian.Client, taskType string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SpawnArchaeologist(ctx, db, lib, "Archaeologist-test")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		_ = db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, taskID).Scan(&status)
		switch status {
		case "Completed":
			return
		case "Failed":
			var errLog string
			_ = db.QueryRow(`SELECT IFNULL(error_log, '') FROM BountyBoard WHERE id = ?`, taskID).Scan(&errLog)
			t.Fatalf("%s task #%d Failed: %s", taskType, taskID, errLog)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s task #%d did not complete within 5s", taskType, taskID)
}

// writeArchTestFile creates a file (with parent dirs) for use in
// archaeologist agent tests.
func writeArchTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// silentArchLogger is a test logger that drops every log line. We
// don't import testing.T into the dog (Pattern P14 / package
// boundaries), so use a tiny in-test stand-in.
type silentArchLogger struct{}

func (s *silentArchLogger) Printf(string, ...any) {}
