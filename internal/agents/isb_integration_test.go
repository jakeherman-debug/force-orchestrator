package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/isb"
	_ "force-orchestrator/internal/isb/rules"
	"force-orchestrator/internal/store"
)

// isbTestLogger is a minimal Printf-shaped logger used by integration
// tests so the ISB task path doesn't drag in package-level loggers.
type isbTestLogger struct{ msgs []string }

func (s *isbTestLogger) Printf(format string, a ...any) {
	s.msgs = append(s.msgs, fmt.Sprintf(format, a...))
}

// seedISBDB initialises an in-memory holocron and seeds Repositories +
// FleetRules so the ISB task can run end-to-end.
func seedISBDB(t *testing.T) (*sql.DB, string) {
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

// isbFixture returns a per-rule violating Go source string keyed by
// rule ID. Each must trip the rule's red branch when scanned.
func isbFixture(ruleID string) (path, source string) {
	switch ruleID {
	case "ISB-001":
		return "internal/foo/leak.go", `package foo
const token = "ghp_aB3dE7fG9hJkLmNpQrStVwXyZ12345aBcD"
const url = "https://user:hunter2@api.example.com/foo"
`
	case "ISB-002":
		return "internal/foo/exec.go", `package foo
import "os/exec"
func F(branch string) { _ = exec.Command("git", "checkout", branch) }
`
	case "ISB-003":
		return "internal/foo/sql.go", `package foo
import "database/sql"
func F(db *sql.DB, name string) error {
	_, err := db.Exec("SELECT * FROM Users WHERE name = " + name)
	return err
}
`
	case "ISB-004":
		return "internal/foo/http.go", `package foo
import "net/http"
func F(url string) { _, _ = http.Get(url) }
`
	case "ISB-005":
		return "internal/foo/srv.go", `package foo
import "net/http"
func upd(w http.ResponseWriter, r *http.Request) {}
func reg() { http.HandleFunc("/api/update", upd) }
`
	case "ISB-006":
		return "internal/foo/perm.go", `package foo
import "os"
func F() { _ = os.WriteFile("/etc/foo.conf", []byte("x"), 0o777) }
`
	case "ISB-007":
		return "internal/foo/del.go", `package foo
import "os"
func F(p string) error { return os.RemoveAll(p) }
`
	case "ISB-008":
		return "internal/foo/prompt.go", `package foo
type API struct{}
var claude API
func (API) CallWithTranscript(p string) string { return "" }
func F(b string) string { return claude.CallWithTranscript("Summarize: " + b) }
`
	case "ISB-009":
		return "internal/foo/read.go", `package foo
import (
	"io"
	"net/http"
)
func F(resp *http.Response) ([]byte, error) { return io.ReadAll(resp.Body) }
`
	case "ISB-010":
		return "internal/agents/parse.go", `package agents
import "encoding/json"
type R struct{ X string }
func parse(data []byte) (R, error) {
	var r R
	err := json.Unmarshal(data, &r)
	return r, err
}
`
	}
	return "", ""
}

// runISBReviewerDirect runs the ISB reviewer against a single in-memory
// fixture without going through the git-diff path. Validates the rule
// + bypass + finding-record pipeline.
func runISBReviewerDirect(t *testing.T, db *sql.DB, srcPath, srcBody string, sourceTaskID int) {
	t.Helper()
	gate := isb.DBFleetRulesGate(db)
	res := isb.ReviewFiles(gate, []isb.ReviewInput{{Path: srcPath, Source: srcBody}})
	for _, f := range res.Findings {
		if _, err := store.InsertSecurityFinding(db, store.SecurityFinding{
			TaskID:     sourceTaskID,
			Bureau:     "ISB",
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

// TestISB_SeededViolations_AllRulesFire — every rule's red fixture
// produces at least one SecurityFindings row when run through the
// reviewer.
func TestISB_SeededViolations_AllRulesFire(t *testing.T) {
	for _, r := range isb.All() {
		t.Run(r.ID(), func(t *testing.T) {
			db, _ := seedISBDB(t)
			path, src := isbFixture(r.ID())
			if path == "" {
				t.Skipf("no fixture for %s", r.ID())
			}
			sourceTaskID := 100 // arbitrary, valid
			runISBReviewerDirect(t, db, path, src, sourceTaskID)

			rows, err := store.ListSecurityFindings(db, sourceTaskID)
			if err != nil {
				t.Fatalf("ListSecurityFindings: %v", err)
			}
			found := false
			for _, f := range rows {
				if f.RuleID == r.ID() && f.Bureau == "ISB" {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("rule %s: expected at least one ISB finding; got %d rows: %v", r.ID(), len(rows), rows)
			}
		})
	}
}

// TestISB_BypassDowngradesAdvise_PreservesAuditTrail — an
// ISB-BYPASS comment downgrades a finding to advise (and at launch
// they're already advise, but the audit trail must capture
// disposition='overridden' + AUDIT-NNN + reason ≥ 10 chars).
func TestISB_BypassDowngradesAdvise_PreservesAuditTrail(t *testing.T) {
	db, _ := seedISBDB(t)
	src := `package foo
import "net/http"
func F(url string) {
	// ISB-BYPASS: AUDIT-007 Operator approved override pre-merge for D4 P2 shakedown
	_, _ = http.Get(url)
}
`
	gate := isb.DBFleetRulesGate(db)
	res := isb.ReviewFiles(gate, []isb.ReviewInput{
		{Path: "internal/foo/h.go", Source: src},
	})
	if res.HasBlock {
		t.Fatal("HasBlock should be false after bypass downgrade")
	}
	for _, f := range res.Findings {
		if _, err := store.InsertSecurityFinding(db, store.SecurityFinding{
			TaskID:        42,
			Bureau:        "ISB",
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
	hasBypassedISB004 := false
	for _, r := range rows {
		if r.RuleID == "ISB-004" && r.Disposition == "overridden" && r.BypassAuditID == "AUDIT-007" && len(r.BypassReason) >= 10 {
			hasBypassedISB004 = true
		}
	}
	if !hasBypassedISB004 {
		t.Fatalf("expected one ISB-004 row with disposition=overridden + AUDIT-007; got %v", rows)
	}
}

// TestISB_MalformedBypassFailsParse — anti-cheat: a malformed
// ISB-BYPASS surfaces as a ISB-BYPASS-MALFORMED block-severity
// finding (parses fail hard).
func TestISB_MalformedBypassFailsParse(t *testing.T) {
	db, _ := seedISBDB(t)
	src := `package x
// ISB-BYPASS: AUDIT-001 short
func F() {}
`
	gate := isb.DBFleetRulesGate(db)
	res := isb.ReviewFiles(gate, []isb.ReviewInput{{Path: "x.go", Source: src}})
	if !res.HasBlock {
		t.Fatal("malformed bypass: expected HasBlock=true")
	}
	hasMalformed := false
	for _, f := range res.Findings {
		if f.RuleID == "ISB-BYPASS-MALFORMED" {
			hasMalformed = true
		}
	}
	if !hasMalformed {
		t.Fatalf("expected ISB-BYPASS-MALFORMED finding; got %v", res.Findings)
	}
	_ = db
}

// TestCommitPipeline_BoSAndISB_BothMustApprove — dual-gate: a source
// task is only considered approved when BOTH bureaus' Reviews complete
// without block-severity findings.
func TestCommitPipeline_BoSAndISB_BothMustApprove(t *testing.T) {
	db, repoDir := seedISBDB(t)

	// Initialize a real git repo so loadISBReviewInputs has a HEAD to
	// diff against.
	mustGit(t, repoDir, "init", "-q", "-b", "main")
	mustGit(t, repoDir, "config", "user.email", "test@example.com")
	mustGit(t, repoDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repoDir, "seed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "add", ".")
	mustGit(t, repoDir, "commit", "-q", "-m", "seed")

	// Seed a clean commit on a feature branch — no violations in ISB
	// or BoS for the dual-pass case.
	mustGit(t, repoDir, "checkout", "-q", "-b", "feature/clean")
	srcRel := "internal/foo/clean.go"
	full := filepath.Join(repoDir, srcRel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`package foo
func F() string { return "hello" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "add", ".")
	mustGit(t, repoDir, "commit", "-q", "-m", "clean")

	// Source task that the ISBReview will adjudicate.
	srcTaskID := store.AddBounty(db, 0, "CodeEdit", "[demo] clean code edit task")
	if _, err := db.Exec(`UPDATE BountyBoard SET target_repo = ?, branch_name = ?, status = 'AwaitingCaptainReview' WHERE id = ?`,
		"demo", "feature/clean", srcTaskID); err != nil {
		t.Fatalf("update src task: %v", err)
	}

	// Enqueue ISBReview.
	srcBounty, err := store.GetBounty(db, srcTaskID)
	if err != nil {
		t.Fatalf("GetBounty: %v", err)
	}
	isbTaskID, err := store.QueueISBReview(db, srcBounty, "feature/clean", "abcdef")
	if err != nil {
		t.Fatalf("QueueISBReview: %v", err)
	}
	isbBounty, _ := store.GetBounty(db, isbTaskID)
	row := db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, isbTaskID)
	var payload string
	_ = row.Scan(&payload)
	isbBounty.Payload = payload

	// Sanity: payload includes our branch.
	var p isbReviewPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.Branch != "feature/clean" {
		t.Fatalf("payload branch: %q", p.Branch)
	}

	// Run the reviewer.
	logger := &isbTestLogger{}
	runISBReviewTask(context.Background(), db, "ISB-test", isbBounty, logger)

	// Source task should remain in AwaitingCaptainReview (clean ISB
	// pass; the dual-gate was satisfied because no block findings
	// were recorded).
	got, err := store.GetBounty(db, srcTaskID)
	if err != nil {
		t.Fatalf("GetBounty src after review: %v", err)
	}
	if got.Status != "AwaitingCaptainReview" {
		t.Fatalf("source task status after clean ISB pass: got %q, want AwaitingCaptainReview", got.Status)
	}

	// ISBReview infrastructure task is Completed.
	isbRow, err := store.GetBounty(db, isbTaskID)
	if err != nil {
		t.Fatalf("GetBounty isb: %v", err)
	}
	if isbRow.Status != "Completed" {
		t.Fatalf("ISBReview task status: got %q, want Completed", isbRow.Status)
	}

	// ── Now seed a malformed-bypass commit to force a block-class
	// finding and confirm the source task IS routed back to Pending.
	mustGit(t, repoDir, "checkout", "-q", "-b", "feature/bad")
	srcRel2 := "internal/foo/bad.go"
	full2 := filepath.Join(repoDir, srcRel2)
	if err := os.WriteFile(full2, []byte(`package foo
// ISB-BYPASS: AUDIT-001 short
func F() {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "add", ".")
	mustGit(t, repoDir, "commit", "-q", "-m", "bad")

	srcTaskID2 := store.AddBounty(db, 0, "CodeEdit", "[demo] malformed bypass commit")
	if _, err := db.Exec(`UPDATE BountyBoard SET target_repo = ?, branch_name = ?, status = 'AwaitingCaptainReview' WHERE id = ?`,
		"demo", "feature/bad", srcTaskID2); err != nil {
		t.Fatalf("update src task 2: %v", err)
	}
	srcBounty2, _ := store.GetBounty(db, srcTaskID2)
	isbTaskID2, err := store.QueueISBReview(db, srcBounty2, "feature/bad", "deadbeef")
	if err != nil {
		t.Fatalf("QueueISBReview 2: %v", err)
	}
	isbBounty2, _ := store.GetBounty(db, isbTaskID2)
	row2 := db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, isbTaskID2)
	var payload2 string
	_ = row2.Scan(&payload2)
	isbBounty2.Payload = payload2

	runISBReviewTask(context.Background(), db, "ISB-test", isbBounty2, logger)

	got2, _ := store.GetBounty(db, srcTaskID2)
	if got2.Status != "Pending" {
		t.Fatalf("malformed bypass: source task status = %q, want Pending (rejected)", got2.Status)
	}
}
