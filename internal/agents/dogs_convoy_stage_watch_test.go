package agents

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/stagegate"
	"force-orchestrator/internal/store"
)

// dogStubGate is a Gate that returns a pre-decided outcome. Tests use
// it to drive Pass/Fail behaviour without time-based gates.
type dogStubGate struct {
	typeName string
	passed   bool
	reason   string
	err      error
}

func (s *dogStubGate) Type() string { return s.typeName }
func (s *dogStubGate) Evaluate(_ context.Context, _ *sql.DB, _ stagegate.StageContext) (bool, string, error) {
	return s.passed, s.reason, s.err
}

// ── DB seed helpers ──────────────────────────────────────────────────────

func cswInsertConvoy(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	res, err := db.Exec(`INSERT INTO Convoys (name, status, staging_mode, staging_strategy)
		VALUES (?, 'Active', 'staged', 'strict')`, name)
	if err != nil {
		t.Fatalf("insert convoy %q: %v", name, err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

func cswInsertStage(t *testing.T, db *sql.DB, convoyID, stageNum int, gateType, gateConfigJSON string) int {
	t.Helper()
	id, err := store.CreateStage(db, convoyID, stageNum, "test stage", gateType, gateConfigJSON)
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return id
}

func cswInsertConvoyAskBranch(t *testing.T, db *sql.DB, convoyID int, repo string, stageID int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO ConvoyAskBranches
		(convoy_id, repo, ask_branch, ask_branch_base_sha, stage_id)
		VALUES (?, ?, ?, ?, ?)`, convoyID, repo, "ask-"+repo, "deadbeef", stageID)
	if err != nil {
		t.Fatalf("insert ConvoyAskBranches: %v", err)
	}
}

func cswInsertAskBranchPR(t *testing.T, db *sql.DB, convoyID int, repo, state string, prNumber int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO AskBranchPRs
		(task_id, convoy_id, repo, pr_number, state)
		VALUES (?, ?, ?, ?, ?)`, prNumber, convoyID, repo, prNumber, state)
	if err != nil {
		t.Fatalf("insert AskBranchPRs: %v", err)
	}
}

func cswStageStatus(t *testing.T, db *sql.DB, stageID int) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT status FROM ConvoyStages WHERE id = ?`, stageID).Scan(&s); err != nil {
		t.Fatalf("read stage status: %v", err)
	}
	return s
}

func cswStageColumn(t *testing.T, db *sql.DB, stageID int, col string) string {
	t.Helper()
	var v string
	q := `SELECT IFNULL(` + col + `, '') FROM ConvoyStages WHERE id = ?`
	if err := db.QueryRow(q, stageID).Scan(&v); err != nil {
		t.Fatalf("read stage col %s: %v", col, err)
	}
	return v
}

// ── Tests ─────────────────────────────────────────────────────────────────

func TestConvoyStageWatch_NoActiveStages_NoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)

	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
}

func TestConvoyStageWatch_OpenWithMergedPRs_FlipsToAllPRsMerged(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-staged-1")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='Open', opened_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set Open: %v", err)
	}
	cswInsertConvoyAskBranch(t, db, convoyID, "repo-a", stageID)
	cswInsertAskBranchPR(t, db, convoyID, "repo-a", "Merged", 101)
	cswInsertConvoyAskBranch(t, db, convoyID, "repo-b", stageID)
	cswInsertAskBranchPR(t, db, convoyID, "repo-b", "Merged", 102)

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}

	if got := cswStageStatus(t, db, stageID); got != store.StageStatusAllPRsMerged {
		t.Errorf("status = %s, want AllPRsMerged", got)
	}
	if cswStageColumn(t, db, stageID, "all_prs_merged_at") == "" {
		t.Error("expected all_prs_merged_at to be stamped")
	}
}

func TestConvoyStageWatch_OpenWithUnmergedPRs_StaysOpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-staged-2")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='Open', opened_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set Open: %v", err)
	}
	cswInsertConvoyAskBranch(t, db, convoyID, "repo-a", stageID)
	cswInsertAskBranchPR(t, db, convoyID, "repo-a", "Open", 201)

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusOpen {
		t.Errorf("status = %s, want Open (PR not merged)", got)
	}
}

func TestConvoyStageWatch_OpenWithNoPRs_StaysOpen(t *testing.T) {
	// Open stage with zero PR rows must NOT be treated as
	// "all PRs merged" — astromechs haven't opened them yet.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-staged-empty")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='Open', opened_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set Open: %v", err)
	}

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusOpen {
		t.Errorf("status = %s, want Open (no PRs yet)", got)
	}
}

func TestConvoyStageWatch_AllPRsMerged_FlipsToAwaitingGate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-staged-3")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AllPRsMerged', opened_at=datetime('now'), all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AllPRsMerged: %v", err)
	}

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusAwaitingGate {
		t.Errorf("status = %s, want AwaitingGate", got)
	}
}

func TestConvoyStageWatch_AwaitingGate_PassedGate_FlipsToGatePassed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-staged-4")
	// null gate always passes.
	stageID := cswInsertStage(t, db, convoyID, 1, "null", `{}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusGatePassed {
		t.Errorf("status = %s, want GatePassed", got)
	}
	if cswStageColumn(t, db, stageID, "gate_passed_at") == "" {
		t.Error("expected gate_passed_at to be stamped")
	}
}

func TestConvoyStageWatch_AwaitingGate_FailedGate_FlipsToFailed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	reg.Register(&dogStubGate{typeName: "always_fail", passed: false, reason: "intentional fail"})

	convoyID := cswInsertConvoy(t, db, "test-staged-5")
	stageID := cswInsertStage(t, db, convoyID, 1, "always_fail", `{}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusFailed {
		t.Errorf("status = %s, want Failed", got)
	}
}

func TestConvoyStageWatch_AwaitingGate_PendingGate_NoChange(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-staged-6")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":60}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusAwaitingGate {
		t.Errorf("status = %s, want AwaitingGate (gate still pending)", got)
	}
}

func TestConvoyStageWatch_GateTimeoutExceeded_FlipsToFailed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-staged-timeout")
	// Configure a long soak (24h) but a tiny timeout (5m) plus an
	// all_prs_merged_at 2h in the past — the dog should sink to
	// Failed via the timeout path.
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1440}`)
	twoHoursAgo := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`UPDATE ConvoyStages
		SET status='AwaitingGate', all_prs_merged_at=?, gate_timeout_minutes=5
		WHERE id = ?`, twoHoursAgo, stageID); err != nil {
		t.Fatalf("set timeout fixture: %v", err)
	}

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusFailed {
		t.Errorf("status = %s, want Failed (gate timeout)", got)
	}

	var subject string
	_ = db.QueryRow(`SELECT subject FROM Fleet_Mail WHERE subject LIKE '[STAGE GATE TIMEOUT]%' LIMIT 1`).Scan(&subject)
	if subject == "" {
		t.Error("expected [STAGE GATE TIMEOUT] mail")
	}
}

func TestConvoyStageWatch_LegacySingleStageNullGate_NoOp(t *testing.T) {
	// A legacy single-stage convoy carries gate_type=NULL (the
	// forward-compat migration shape). The dog must NOT advance it —
	// stale-convoys-report owns that lifecycle.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "legacy-single")
	res, err := db.Exec(`INSERT INTO ConvoyStages
		(convoy_id, stage_num, intent_text, status, gate_type, gate_config_json, opened_at)
		VALUES (?, 1, '', 'Open', NULL, '{}', datetime('now'))`, convoyID)
	if err != nil {
		t.Fatalf("insert legacy stage: %v", err)
	}
	stageID, _ := res.LastInsertId()
	cswInsertConvoyAskBranch(t, db, convoyID, "repo-a", int(stageID))
	cswInsertAskBranchPR(t, db, convoyID, "repo-a", "Merged", 301)

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if got := cswStageStatus(t, db, int(stageID)); got != store.StageStatusOpen {
		t.Errorf("expected legacy stage to remain Open (gate_type=NULL), got %s", got)
	}
}

func TestConvoyStageWatch_OperatorConfirmAdvancesOnSystemConfigWrite(t *testing.T) {
	// End-to-end: AwaitingGate with operator_confirm gate, dashboard
	// "advance" key not set → stays pending. Set the key → GatePassed.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "operator-flow")
	stageID := cswInsertStage(t, db, convoyID, 1, "operator_confirm", `{"prompt":"deploy looks healthy?"}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)

	// Tick 1: no operator confirm → still AwaitingGate.
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusAwaitingGate {
		t.Errorf("tick 1 status = %s, want AwaitingGate", got)
	}

	// Operator clicks advance.
	store.SetConfig(db, "stage_advance_"+strconv.Itoa(convoyID)+"_1", "operator-x:2026-05-01")

	// Tick 2: should flip GatePassed.
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusGatePassed {
		t.Errorf("tick 2 status = %s, want GatePassed", got)
	}
}

// ── registration / cooldown ──────────────────────────────────────────────

func TestConvoyStageWatch_RegisteredAtCorrectCadence(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dogs := ListDogs(db)
	var found *DogStatus
	for i := range dogs {
		if dogs[i].Name == "convoy-stage-watch" {
			found = &dogs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("convoy-stage-watch not in ListDogs output")
	}
	if found.Cooldown.Minutes() != 5 {
		t.Errorf("cooldown: want 5m, got %v", found.Cooldown)
	}
	hit := false
	for _, name := range dogOrder {
		if name == "convoy-stage-watch" {
			hit = true
			break
		}
	}
	if !hit {
		t.Errorf("convoy-stage-watch missing from dogOrder")
	}
}

// ── dispatch when registry is unwired ────────────────────────────────────

func TestDogConvoyStageWatch_NilRegistry_LogsAndExits(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	prev := getStageGateRegistry()
	defer RegisterStageGateRegistry(prev)
	RegisterStageGateRegistry(nil)

	if err := dogConvoyStageWatch(context.Background(), db, testLogger{}); err != nil {
		t.Errorf("expected nil err on unwired registry, got %v", err)
	}
}

func TestDogConvoyStageWatch_RegistryWired_Dispatches(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	prev := getStageGateRegistry()
	defer RegisterStageGateRegistry(prev)

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	RegisterStageGateRegistry(reg)

	if err := dogConvoyStageWatch(context.Background(), db, testLogger{}); err != nil {
		t.Errorf("expected nil err on wired registry, got %v", err)
	}
}

// TestConvoyStageWatch_GateTimeout_RespectsNotificationBudget_NotAllowed —
// D5.5 P3 ζ: when RespectNotificationBudget returns allowed=false, the
// gate-timeout escalation MUST NOT call SendMail. StakesHigh always
// punches through in production today, so the only way to drive this
// branch is to override the budget seam — but the regression slot exists
// because a future budget-semantics change could silently re-enable
// emission via the dropped allowed return.
func TestConvoyStageWatch_GateTimeout_RespectsNotificationBudget_NotAllowed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-budget-suppress")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1440}`)
	twoHoursAgo := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`UPDATE ConvoyStages
		SET status='AwaitingGate', all_prs_merged_at=?, gate_timeout_minutes=5
		WHERE id = ?`, twoHoursAgo, stageID); err != nil {
		t.Fatalf("set timeout fixture: %v", err)
	}

	// Force allowed=false so the dog's escalation path must skip SendMail.
	restoreBudget := SetGateTimeoutBudgetCheckForTest(
		func(_ context.Context, _ *sql.DB, _, _, _, _ string, _ store.NotificationStakes) (bool, error) {
			return false, nil
		},
	)
	defer restoreBudget()

	var sendMailCalls int
	restoreSend := SetGateTimeoutSendMailForTest(
		func(_ *sql.DB, _, _, _, _ string, _ int, _ store.MailType) int64 {
			sendMailCalls++
			return 0
		},
	)
	defer restoreSend()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}

	// Stage must still flip to Failed — the budget gate only suppresses
	// the operator notification, not the state transition itself
	// (anti-cheat: timeout always sinks to Failed).
	if got := cswStageStatus(t, db, stageID); got != store.StageStatusFailed {
		t.Errorf("status = %s, want Failed (timeout still flips state regardless of budget)", got)
	}
	if sendMailCalls != 0 {
		t.Errorf("expected SendMail to be skipped when budget denies (allowed=false); got %d call(s)", sendMailCalls)
	}
}

// TestConvoyStageWatch_GateTimeout_RespectsNotificationBudget_Allowed —
// the inverse: when allowed=true, SendMail MUST be called. This is the
// production path (StakesHigh always punches through).
func TestConvoyStageWatch_GateTimeout_RespectsNotificationBudget_Allowed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-budget-allowed")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1440}`)
	twoHoursAgo := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`UPDATE ConvoyStages
		SET status='AwaitingGate', all_prs_merged_at=?, gate_timeout_minutes=5
		WHERE id = ?`, twoHoursAgo, stageID); err != nil {
		t.Fatalf("set timeout fixture: %v", err)
	}

	restoreBudget := SetGateTimeoutBudgetCheckForTest(
		func(_ context.Context, _ *sql.DB, _, _, _, _ string, _ store.NotificationStakes) (bool, error) {
			return true, nil
		},
	)
	defer restoreBudget()

	var capturedSubject, capturedBody string
	var sendMailCalls int
	restoreSend := SetGateTimeoutSendMailForTest(
		func(_ *sql.DB, _, _, subject, body string, _ int, _ store.MailType) int64 {
			sendMailCalls++
			capturedSubject = subject
			capturedBody = body
			return 1
		},
	)
	defer restoreSend()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}

	if sendMailCalls != 1 {
		t.Fatalf("expected exactly one SendMail call; got %d", sendMailCalls)
	}

	// Escalation message contract (D5.5 P3 ζ): subject + body must
	// include the load-bearing operator-actionable signals so the
	// inbox alone tells the operator what to do.
	if !strings.Contains(capturedSubject, "STAGE GATE TIMEOUT") {
		t.Errorf("subject missing tag, got %q", capturedSubject)
	}
	if !strings.Contains(capturedSubject, "soak_minutes") {
		t.Errorf("subject must name the gate type, got %q", capturedSubject)
	}
	for _, want := range []string{
		"convoy",                    // human-readable convoy reference
		"test-budget-allowed",       // the convoy name from cswInsertConvoy
		"Stage intent",              // intent line label
		"Gate type:",                // gate type line label
		"Gate config:",              // gate config line label
		"Awaiting for:",             // duration line label
		"timeout: 5 minutes",        // configured timeout
		"Recommendation",            // resolution-path label
		"operator-confirm",          // recommended fallback path
		"force convoy show",         // inspection command
	} {
		if !strings.Contains(capturedBody, want) {
			t.Errorf("escalation body missing %q; got:\n%s", want, capturedBody)
		}
	}
}

// TestConvoyStageWatch_GateTimeout_RespectsNotificationBudget_BudgetError_FailOpen —
// when the budget check returns an error, the dog must fail-open and
// emit the escalation. A SQLite glitch must never silence a high-stakes
// gate-timeout alert.
func TestConvoyStageWatch_GateTimeout_RespectsNotificationBudget_BudgetError_FailOpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "test-budget-error")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1440}`)
	twoHoursAgo := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`UPDATE ConvoyStages
		SET status='AwaitingGate', all_prs_merged_at=?, gate_timeout_minutes=5
		WHERE id = ?`, twoHoursAgo, stageID); err != nil {
		t.Fatalf("set timeout fixture: %v", err)
	}

	restoreBudget := SetGateTimeoutBudgetCheckForTest(
		func(_ context.Context, _ *sql.DB, _, _, _, _ string, _ store.NotificationStakes) (bool, error) {
			return false, errors.New("simulated sqlite glitch")
		},
	)
	defer restoreBudget()

	var sendMailCalls int
	restoreSend := SetGateTimeoutSendMailForTest(
		func(_ *sql.DB, _, _, _, _ string, _ int, _ store.MailType) int64 {
			sendMailCalls++
			return 1
		},
	)
	defer restoreSend()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if sendMailCalls != 1 {
		t.Errorf("expected SendMail to be called even on budget error (fail-open); got %d call(s)", sendMailCalls)
	}
}

// ── D5.5 P4 — stage-transition pings + audit trail ─────────────────────────
//
// The dog fires notify-after on each stage transition (Open→AllPRsMerged,
// AllPRsMerged→AwaitingGate, AwaitingGate→GatePassed, AwaitingGate→Failed)
// and appends a stage_auto_advance AuditLog row. Pings are debounced via
// SystemConfig.stage_transition_notified_<convoy>_<stage>_<status> so a
// re-tick that re-evaluates the same transition doesn't re-ping.

func cswCaptureStageTransitionNotifies(t *testing.T) (calls *[]string, restore func()) {
	t.Helper()
	captured := []string{}
	restore = SetStageTransitionNotifyForTest(func(_ context.Context, label string) error {
		captured = append(captured, label)
		return nil
	})
	return &captured, restore
}

func TestConvoyStageWatch_StageTransition_FiresNotifyAfter_Open(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "transition-open")
	stageID := cswInsertStage(t, db, convoyID, 1, "soak_minutes", `{"minutes":1}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='Open', opened_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set Open: %v", err)
	}
	cswInsertConvoyAskBranch(t, db, convoyID, "repo-a", stageID)
	cswInsertAskBranchPR(t, db, convoyID, "repo-a", "Merged", 401)

	calls, restore := cswCaptureStageTransitionNotifies(t)
	defer restore()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected 1 notify-after call after Open→AllPRsMerged, got %d (%v)", len(*calls), *calls)
	}
	want := []string{"Open", "AllPRsMerged", "soak_minutes", "transition-open"}
	for _, w := range want {
		if !strings.Contains((*calls)[0], w) {
			t.Errorf("ping label missing %q; got %q", w, (*calls)[0])
		}
	}

	// Audit row landed.
	logs, err := store.ListStageAuditLog(db, convoyID, 1)
	if err != nil {
		t.Fatalf("ListStageAuditLog: %v", err)
	}
	if len(logs) != 1 || logs[0].Action != store.AuditActionStageAutoAdvance ||
		logs[0].Actor != "convoy-stage-watch-dog" {
		t.Errorf("audit row not as expected: %+v", logs)
	}
}

func TestConvoyStageWatch_StageTransition_FiresNotifyAfter_GatePassed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "transition-gatepass")
	stageID := cswInsertStage(t, db, convoyID, 1, "null", `{}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	calls, restore := cswCaptureStageTransitionNotifies(t)
	defer restore()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 ping for AwaitingGate→GatePassed, got %d", len(*calls))
	}
	if !strings.Contains((*calls)[0], "GatePassed") {
		t.Errorf("ping label missing GatePassed marker; got %q", (*calls)[0])
	}
}

func TestConvoyStageWatch_StageTransition_FiresNotifyAfter_Failed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	convoyID := cswInsertConvoy(t, db, "transition-fail")
	stageID := cswInsertStage(t, db, convoyID, 1, "always_fail", `{}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	calls, restore := cswCaptureStageTransitionNotifies(t)
	defer restore()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	reg.Register(&dogStubGate{typeName: "always_fail", passed: false, reason: "intentional fail"})

	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 ping for AwaitingGate→Failed, got %d", len(*calls))
	}
	if !strings.Contains((*calls)[0], "Failed") {
		t.Errorf("ping label missing Failed marker; got %q", (*calls)[0])
	}
}

func TestConvoyStageWatch_StageTransition_DebouncedPerTransition(t *testing.T) {
	// Re-running the dog after a transition fired must NOT re-ping the
	// same (convoy, stage, new_status) tuple. The debounce flag lives in
	// SystemConfig.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "transition-debounce")
	stageID := cswInsertStage(t, db, convoyID, 1, "null", `{}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	calls, restore := cswCaptureStageTransitionNotifies(t)
	defer restore()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)

	// Tick 1: real transition AwaitingGate→GatePassed → 1 ping.
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("tick 1: expected 1 ping, got %d", len(*calls))
	}

	// Force the stage back to AwaitingGate so the dog re-fires the same
	// transition. Without the debounce, this would emit a second ping.
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', gate_passed_at=NULL WHERE id = ?`, stageID); err != nil {
		t.Fatalf("reset to AwaitingGate: %v", err)
	}
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("tick 2: debounce failed — expected 1 ping total, got %d", len(*calls))
	}
}

func TestConvoyStageWatch_DogTransition_AppendsAuditLog(t *testing.T) {
	// Cross-cuts the dog flow + the AuditLog write. The audit row's
	// actor MUST be "convoy-stage-watch-dog" so the dashboard's
	// per-stage history pane can distinguish dog vs operator actions.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := cswInsertConvoy(t, db, "transition-audit")
	stageID := cswInsertStage(t, db, convoyID, 1, "null", `{}`)
	if _, err := db.Exec(`UPDATE ConvoyStages SET status='AwaitingGate', all_prs_merged_at=datetime('now') WHERE id = ?`, stageID); err != nil {
		t.Fatalf("set AwaitingGate: %v", err)
	}

	_, restore := cswCaptureStageTransitionNotifies(t)
	defer restore()

	reg := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(reg)
	if err := runConvoyStageWatch(context.Background(), db, reg, testLogger{}); err != nil {
		t.Fatalf("dog: %v", err)
	}

	logs, err := store.ListStageAuditLog(db, convoyID, 1)
	if err != nil {
		t.Fatalf("ListStageAuditLog: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(logs))
	}
	if logs[0].Actor != "convoy-stage-watch-dog" {
		t.Errorf("actor = %q, want convoy-stage-watch-dog", logs[0].Actor)
	}
	if logs[0].Action != store.AuditActionStageAutoAdvance {
		t.Errorf("action = %q, want %q", logs[0].Action, store.AuditActionStageAutoAdvance)
	}
	if !strings.Contains(logs[0].Detail, `"new_status":"GatePassed"`) {
		t.Errorf("detail missing new_status: %q", logs[0].Detail)
	}
}
