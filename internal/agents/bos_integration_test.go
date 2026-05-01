package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/bos"
	_ "force-orchestrator/internal/bos/rules"
	"force-orchestrator/internal/store"
)

// mustGit runs a git command in the given directory and fails the
// test on non-zero exit. For `init` we omit the -C flag because the
// directory already exists (t.TempDir) — git initializes in-place.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	var cmd *exec.Cmd
	if len(args) > 0 && args[0] == "init" {
		// `git init [flags] dir` form: append dir as last positional.
		cmd = exec.Command("git", append(args, dir)...)
	} else {
		cmd = exec.Command("git", append([]string{"-C", dir}, args...)...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// bosTestLogger is a minimal io.Writer-backed logger used by integration
// tests so the BoS task path doesn't drag in package-level loggers.
type bosTestLogger struct{ msgs []string }

func (s *bosTestLogger) Printf(format string, a ...any) {
	s.msgs = append(s.msgs, fmt.Sprintf(format, a...))
}

// seedDB initialises an in-memory holocron and seeds Repositories +
// FleetRules so the BoS task can run end-to-end.
func seedDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	t.Cleanup(func() { db.Close() })
	if _, err := store.BootstrapFleetRules(context.Background(), db, ""); err != nil {
		t.Fatalf("BootstrapFleetRules: %v", err)
	}
	repoDir := t.TempDir()
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode) VALUES (?, ?, 'write')`, "demo", repoDir); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return db, repoDir
}

// fixture returns a per-rule violating Go source string keyed by ruleID.
// The returned source must trip the rule's red branch when scanned.
func fixture(ruleID string) (path, source string) {
	switch ruleID {
	case "BOS-001":
		return "internal/store/fix1.go", `
package store
import "database/sql"
func UpdateThing(db *sql.DB, id int) {
	db.Exec("UPDATE T SET x=1 WHERE id=?", id)
}
`
	case "BOS-002":
		return "internal/agents/fix2.go", `
package agents
import "force-orchestrator/internal/store"
func DoIt() { _ = store.UpdateBountyStatus(nil, 1, "X") }
`
	case "BOS-003":
		return "internal/agents/fix3.go", `
package agents
import "force-orchestrator/internal/store"
func DoTwo(id int) error {
	if err := store.UpdateBountyStatus(nil, id, "X"); err != nil { return err }
	if err := store.InsertSecurityFinding(nil, store.SecurityFinding{}); err != nil { return err }
	return nil
}
`
	case "BOS-004":
		return "internal/agents/fix4.go", `
package agents
import "context"
import "database/sql"
func SpawnNew(ctx context.Context, db *sql.DB, name string) {
	for {}
}
`
	case "BOS-005":
		return "internal/git/fix5.go", `
package git
import "context"
func LogAndRun(ctx context.Context, args ...string) {}
func runIt(ctx context.Context) { LogAndRun(ctx, "push", "--force", "origin", "main") }
`
	case "BOS-006":
		return "internal/store/fix6.go", `
package store
import "database/sql"
func InsertChild(db *sql.DB, parentID int) error {
	_, err := db.Exec(` + "`INSERT INTO Children (parent_id) VALUES (?)`" + `, parentID)
	return err
}
`
	case "BOS-007":
		return "internal/store/fix7.go", `
package store
import "database/sql"
func ListByConvoy(db *sql.DB) error {
	_, err := db.Exec(` + "`SELECT * FROM BountyBoard WHERE payload LIKE '%\"convoy_id\":1,%'`" + `)
	return err
}
`
	case "BOS-008":
		return "internal/store/fix8.go", `
package store
import "database/sql"
func mk(db *sql.DB) {
	db.Exec(` + "`CREATE TABLE IF NOT EXISTS NewThing (id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL);`" + `)
}
`
	case "BOS-009":
		return "internal/agents/fix9.go", `
package agents
import "database/sql"
import "time"
func IsEstopped(db *sql.DB) bool { return false }
func loop(db *sql.DB) {
	for {
		if IsEstopped(db) { time.Sleep(time.Second); continue }
	}
}
`
	case "BOS-010":
		return "internal/agents/fix10.go", `
package agents
func SendMail(from, to, subject, body string, taskID int, t string) {}
func emit(body string) { SendMail("a", "b", "c", body, 1, "x") }
`
	case "BOS-011":
		return "internal/agents/fix11.go", `
package agents
import "force-orchestrator/internal/clients/librarian"
func setup() { _ = &librarian.InProcessClient{} }
`
	}
	return "", ""
}

// writeFixture writes the fixture to repoDir at its relative path,
// creating intermediate directories.
func writeFixture(t *testing.T, repoDir, rel, src string) string {
	t.Helper()
	full := filepath.Join(repoDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

// runReviewerDirect runs the BoS reviewer against a single in-memory
// fixture without going through the git-diff path. This validates the
// rule + bypass + finding-record pipeline; the git-diff bridge is
// exercised in TestBoS_PostCommitHookEnqueues.
func runReviewerDirect(t *testing.T, db *sql.DB, ruleID, srcPath, srcBody string, sourceTaskID int) {
	t.Helper()
	gate := bos.DBFleetRulesGate(db)
	res := bos.ReviewFiles(gate, []bos.ReviewInput{{Path: srcPath, Source: srcBody}})
	for _, f := range res.Findings {
		if _, err := store.InsertSecurityFinding(db, store.SecurityFinding{
			TaskID:     sourceTaskID,
			Bureau:     "BoS",
			RuleID:     f.RuleID,
			Severity:   string(f.Severity),
			FilePath:   f.Path,
			LineNumber: f.Line,
			Message:    f.Message,
		}); err != nil {
			t.Fatalf("InsertSecurityFinding: %v", err)
		}
	}
}

// TestBoS_SeededViolations_AllRulesFire — every rule's red fixture
// produces at least one SecurityFindings row when run through the
// reviewer.
func TestBoS_SeededViolations_AllRulesFire(t *testing.T) {
	for _, r := range bos.All() {
		t.Run(r.ID(), func(t *testing.T) {
			db, _ := seedDB(t)
			path, src := fixture(r.ID())
			if path == "" {
				t.Skipf("no fixture for %s", r.ID())
			}
			sourceTaskID := 100 // arbitrary, valid
			runReviewerDirect(t, db, r.ID(), path, src, sourceTaskID)

			rows, err := store.ListSecurityFindings(db, sourceTaskID)
			if err != nil {
				t.Fatalf("ListSecurityFindings: %v", err)
			}
			found := false
			for _, f := range rows {
				if f.RuleID == r.ID() {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("rule %s: expected at least one finding; got %d rows: %v", r.ID(), len(rows), rows)
			}
		})
	}
}

// TestBoS_BypassDowngradesBlock_PreservesAuditTrail — a // BOS-BYPASS
// comment downgrades a block-severity finding (BOS-011) to advise and
// captures the AUDIT-NNN + reason in the SecurityFindings row.
func TestBoS_BypassDowngradesBlock_PreservesAuditTrail(t *testing.T) {
	db, _ := seedDB(t)
	src := `
package agents
import "force-orchestrator/internal/clients/librarian"
func setup() {
	// BOS-BYPASS: AUDIT-007 Operator approved override pre-merge for D4 P1 shakedown
	_ = &librarian.InProcessClient{}
}
`
	gate := bos.DBFleetRulesGate(db)
	res := bos.ReviewFiles(gate, []bos.ReviewInput{
		{Path: "internal/agents/foo.go", Source: src},
	})
	if res.HasBlock {
		t.Fatal("HasBlock should be false after bypass downgrade")
	}
	// Persist findings (mimicking what runBoSReviewTask does).
	for _, f := range res.Findings {
		if _, err := store.InsertSecurityFinding(db, store.SecurityFinding{
			TaskID:        42,
			Bureau:        "BoS",
			RuleID:        f.RuleID,
			Severity:      string(f.Severity),
			FilePath:      f.Path,
			LineNumber:    f.Line,
			Message:       f.Message,
			Disposition:   dispositionFromMessage(f.Message),
			BypassAuditID: extractAuditFromBypassed(f.Message),
			BypassReason:  extractReasonFromBypassed(f.Message),
		}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	rows, err := store.ListSecurityFindings(db, 42)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	hasBypassedBOS011 := false
	for _, r := range rows {
		if r.RuleID == "BOS-011" && r.Disposition == "overridden" && r.BypassAuditID == "AUDIT-007" && len(r.BypassReason) >= 10 {
			hasBypassedBOS011 = true
		}
	}
	if !hasBypassedBOS011 {
		t.Fatalf("expected one BOS-011 row with disposition=overridden + AUDIT-007; got %v", rows)
	}
}

// TestBoS_MalformedBypassFailsParse — anti-cheat: a malformed bypass
// comment surfaces as a BOS-BYPASS-MALFORMED block-severity finding.
func TestBoS_MalformedBypassFailsParse(t *testing.T) {
	db, _ := seedDB(t)
	src := `
package x
// BOS-BYPASS: AUDIT-001 short
func F() {}
`
	gate := bos.DBFleetRulesGate(db)
	res := bos.ReviewFiles(gate, []bos.ReviewInput{{Path: "x.go", Source: src}})
	if !res.HasBlock {
		t.Fatal("malformed bypass: expected HasBlock=true")
	}
	hasMalformed := false
	for _, f := range res.Findings {
		if f.RuleID == "BOS-BYPASS-MALFORMED" {
			hasMalformed = true
		}
	}
	if !hasMalformed {
		t.Fatalf("expected BOS-BYPASS-MALFORMED finding; got %v", res.Findings)
	}
	_ = db
}

// TestBoS_runBoSReviewTask_RejectsOnBlock — full end-to-end:
// QueueBoSReview → runBoSReviewTask → source task returned to Pending
// when the diff carries a block-severity violation.
func TestBoS_runBoSReviewTask_RejectsOnBlock(t *testing.T) {
	db, repoDir := seedDB(t)

	// Initialize a real git repo so loadBoSReviewInputs has a HEAD to
	// diff against.
	mustGit(t, repoDir, "init", "-q", "-b", "main")
	mustGit(t, repoDir, "config", "user.email", "test@example.com")
	mustGit(t, repoDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repoDir, "seed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "add", ".")
	mustGit(t, repoDir, "commit", "-q", "-m", "seed")

	// Create the violating commit on a feature branch.
	mustGit(t, repoDir, "checkout", "-q", "-b", "feature/violate")
	srcRel := "internal/agents/violate.go"
	full := filepath.Join(repoDir, srcRel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`package agents
import "force-orchestrator/internal/clients/librarian"
func setup() { _ = &librarian.InProcessClient{} }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "add", ".")
	mustGit(t, repoDir, "commit", "-q", "-m", "violate")

	// Source task that the BoSReview will adjudicate.
	srcTaskID := store.AddBounty(db, 0, "CodeEdit", "[demo] code edit task")
	if _, err := db.Exec(`UPDATE BountyBoard SET target_repo = ?, branch_name = ?, status = 'AwaitingCaptainReview' WHERE id = ?`,
		"demo", "feature/violate", srcTaskID); err != nil {
		t.Fatalf("update src task: %v", err)
	}

	// Enqueue BoSReview.
	srcBounty, err := store.GetBounty(db, srcTaskID)
	if err != nil {
		t.Fatalf("GetBounty: %v", err)
	}
	bosTaskID, err := store.QueueBoSReview(db, srcBounty, "feature/violate", "abcdef")
	if err != nil {
		t.Fatalf("QueueBoSReview: %v", err)
	}
	bosBounty, _ := store.GetBounty(db, bosTaskID)

	// Patch payload onto bounty (GetBounty doesn't include it).
	row := db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, bosTaskID)
	var payload string
	_ = row.Scan(&payload)
	bosBounty.Payload = payload

	// Sanity: payload includes our branch.
	var p bosReviewPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.Branch != "feature/violate" {
		t.Fatalf("payload branch: %q", p.Branch)
	}

	// Run the reviewer.
	logger := &bosTestLogger{}
	runBoSReviewTask(context.Background(), db, "BoS-test", bosBounty, logger)

	// Source task should be in Pending (returned for rework).
	got, err := store.GetBounty(db, srcTaskID)
	if err != nil {
		t.Fatalf("GetBounty src after review: %v", err)
	}
	if got.Status != "Pending" {
		t.Fatalf("source task status after BoS reject: got %q, want Pending", got.Status)
	}

	// At least one BOS-011 SecurityFindings row.
	findings, err := store.ListSecurityFindings(db, srcTaskID)
	if err != nil {
		t.Fatalf("ListSecurityFindings: %v", err)
	}
	hasBOS011Block := false
	for _, f := range findings {
		if f.RuleID == "BOS-011" && f.Severity == "block" {
			hasBOS011Block = true
		}
	}
	if !hasBOS011Block {
		t.Fatalf("expected BOS-011 block-severity finding; got %v", findings)
	}

	// BoSReview infrastructure task is Completed.
	bosRow, err := store.GetBounty(db, bosTaskID)
	if err != nil {
		t.Fatalf("GetBounty bos: %v", err)
	}
	if bosRow.Status != "Completed" {
		t.Fatalf("BoSReview task status: got %q, want Completed", bosRow.Status)
	}
}
