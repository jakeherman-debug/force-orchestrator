package agents

import (
	"context"
	"database/sql"
	"strconv"
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
