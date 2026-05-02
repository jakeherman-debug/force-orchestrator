package store

import (
	"database/sql"
	"strings"
	"testing"
)

// TestCreateStagedConvoy_BasicHappyPath proves the multi-stage convoy
// constructor lands rows in Convoys (staging_mode='staged'), and one
// ConvoyStages row per spec with the expected status/gate metadata.
func TestCreateStagedConvoy_BasicHappyPath(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()

	specs := []StagedStageSpec{
		{StageNum: 1, Intent: "add column", GateType: "soak_minutes", GateConfigJSON: `{"type":"soak_minutes","config":{"minutes":60}}`},
		{StageNum: 2, Intent: "dual-write", GateType: "operator_confirm", GateConfigJSON: `{"type":"operator_confirm"}`},
		{StageNum: 3, Intent: "read-only new", GateType: "", GateConfigJSON: ""},
	}
	convoyID, stageIDs, err := CreateStagedConvoy(db, "[42] migrate user_account_status", StagingStrategyStrict, specs)
	if err != nil {
		t.Fatalf("CreateStagedConvoy: %v", err)
	}
	if convoyID <= 0 {
		t.Fatalf("expected positive convoyID, got %d", convoyID)
	}
	if len(stageIDs) != 3 {
		t.Fatalf("expected 3 stage IDs, got %d", len(stageIDs))
	}

	// Convoy row staging_mode + staging_strategy.
	c := GetConvoy(db, convoyID)
	if c == nil {
		t.Fatal("GetConvoy returned nil for freshly-created staged convoy")
	}
	if c.StagingMode != StagingModeStaged {
		t.Errorf("staging_mode = %q, want %q", c.StagingMode, StagingModeStaged)
	}
	if c.StagingStrategy != StagingStrategyStrict {
		t.Errorf("staging_strategy = %q, want %q", c.StagingStrategy, StagingStrategyStrict)
	}

	// Three ConvoyStages rows; stage 1 Open, stages 2/3 Pending.
	stages, lerr := ListStages(db, convoyID)
	if lerr != nil {
		t.Fatalf("ListStages: %v", lerr)
	}
	if len(stages) != 3 {
		t.Fatalf("expected 3 ConvoyStages rows, got %d", len(stages))
	}
	if stages[0].Status != StageStatusOpen {
		t.Errorf("stage 1 status = %q, want %q", stages[0].Status, StageStatusOpen)
	}
	if stages[0].OpenedAt == "" {
		t.Errorf("stage 1 opened_at must be stamped at creation")
	}
	for i, s := range stages[1:] {
		if s.Status != StageStatusPending {
			t.Errorf("stage %d status = %q, want %q", i+2, s.Status, StageStatusPending)
		}
		if s.OpenedAt != "" {
			t.Errorf("stage %d should not have opened_at set yet (got %q)", i+2, s.OpenedAt)
		}
	}
	// Stage 3 has gate_type=NULL (terminal); stages 1 + 2 carry gate_type.
	if !stages[2].GateTypeIsNull {
		t.Errorf("stage 3 gate_type should be NULL (terminal); got %q", stages[2].GateType)
	}
	if stages[0].GateType != "soak_minutes" {
		t.Errorf("stage 1 gate_type = %q, want soak_minutes", stages[0].GateType)
	}
}

// TestCreateStagedConvoy_NonContiguousStages_Errors — the constructor
// refuses non-1-indexed-contiguous stage_num values (the planner already
// enforces this; the constructor is the belt-and-suspenders defense).
func TestCreateStagedConvoy_NonContiguousStages_Errors(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()

	specs := []StagedStageSpec{
		{StageNum: 1, Intent: "x"},
		{StageNum: 3, Intent: "y"}, // skipped 2
	}
	_, _, err := CreateStagedConvoy(db, "name", StagingStrategyStrict, specs)
	if err == nil {
		t.Fatal("expected error for non-contiguous stages; got nil")
	}
	if !strings.Contains(err.Error(), "1-indexed contiguous") {
		t.Fatalf("error must name '1-indexed contiguous'; got %q", err.Error())
	}
}

// TestCreateStagedConvoy_EmptyStages_Errors — at least one stage required.
func TestCreateStagedConvoy_EmptyStages_Errors(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()
	_, _, err := CreateStagedConvoy(db, "x", StagingStrategyStrict, nil)
	if err == nil {
		t.Fatal("expected error for empty stages; got nil")
	}
}

// TestCreateStagedConvoy_DuplicateName_Errors — Convoys.name is UNIQUE;
// the second insert under the same name fails (and rolls back so no
// partial state lands).
func TestCreateStagedConvoy_DuplicateName_Errors(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()
	specs := []StagedStageSpec{{StageNum: 1, Intent: "x"}}
	if _, _, err := CreateStagedConvoy(db, "dup", StagingStrategyStrict, specs); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, _, err := CreateStagedConvoy(db, "dup", StagingStrategyStrict, specs); err == nil {
		t.Fatal("expected duplicate-name error; got nil")
	}
	// Verify only one Convoys row exists with name='dup'.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE name = 'dup'`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected exactly 1 Convoys row named 'dup', got %d", n)
	}
}

// TestAddConvoyTaskWithStageTx_StampsStageID proves a task created via
// the staged path lands BountyBoard.stage_id at the requested value, and
// that querying back returns the same id.
func TestAddConvoyTaskWithStageTx_StampsStageID(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()

	// Register a repo so the FK chain is realistic.
	_, err := db.Exec(`INSERT INTO Repositories (name, local_path, description) VALUES ('api','/tmp/api','x')`)
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	specs := []StagedStageSpec{
		{StageNum: 1, Intent: "stage 1"},
		{StageNum: 2, Intent: "stage 2"},
	}
	convoyID, stageIDs, err := CreateStagedConvoy(db, "stagey", StagingStrategyStrict, specs)
	if err != nil {
		t.Fatalf("CreateStagedConvoy: %v", err)
	}
	tx, _ := db.Begin()
	id1, err := AddConvoyTaskWithStageTx(tx, 0, "api", "do work A", convoyID, 0, stageIDs[0], "Pending")
	if err != nil {
		tx.Rollback()
		t.Fatalf("AddConvoyTaskWithStageTx (stage 1): %v", err)
	}
	id2, err := AddConvoyTaskWithStageTx(tx, 0, "api", "do work B", convoyID, 0, stageIDs[1], "Pending")
	if err != nil {
		tx.Rollback()
		t.Fatalf("AddConvoyTaskWithStageTx (stage 2): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Query back stage_id on each row.
	var s1, s2 sql.NullInt64
	db.QueryRow(`SELECT stage_id FROM BountyBoard WHERE id = ?`, id1).Scan(&s1)
	db.QueryRow(`SELECT stage_id FROM BountyBoard WHERE id = ?`, id2).Scan(&s2)
	if !s1.Valid || int(s1.Int64) != stageIDs[0] {
		t.Errorf("task 1 stage_id = %v, want %d", s1, stageIDs[0])
	}
	if !s2.Valid || int(s2.Int64) != stageIDs[1] {
		t.Errorf("task 2 stage_id = %v, want %d", s2, stageIDs[1])
	}
}

// TestAddConvoyTaskWithStageTx_RejectsZeroStage — passing stageID=0 is
// caller error (the stage-less path uses AddConvoyTaskTx instead).
func TestAddConvoyTaskWithStageTx_RejectsZeroStage(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()
	tx, _ := db.Begin()
	defer tx.Rollback()
	if _, err := AddConvoyTaskWithStageTx(tx, 0, "api", "x", 1, 0, 0, "Pending"); err == nil {
		t.Fatal("expected error for stageID=0; got nil")
	}
}

// TestAddConvoyTaskTx_LeavesStageIDNull proves the legacy single-mode
// task-creation path leaves BountyBoard.stage_id as NULL — preserving
// the contract that NULL == "no stage assignment / legacy convoy."
func TestAddConvoyTaskTx_LeavesStageIDNull(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, description) VALUES ('api','/tmp/api','x')`); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	convoyID, err := CreateConvoy(db, "single-mode")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	tx, _ := db.Begin()
	id, err := AddConvoyTaskTx(tx, 0, "api", "do work", convoyID, 0, "Pending")
	if err != nil {
		tx.Rollback()
		t.Fatalf("AddConvoyTaskTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var s sql.NullInt64
	db.QueryRow(`SELECT stage_id FROM BountyBoard WHERE id = ?`, id).Scan(&s)
	if s.Valid {
		t.Errorf("legacy single-mode task should have NULL stage_id; got %d", s.Int64)
	}
}

// TestSetConvoyStaging_HappyPath — flipping staging_mode from single to
// staged on an existing convoy via SetConvoyStaging persists.
func TestSetConvoyStaging_HappyPath(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()
	convoyID, _ := CreateConvoy(db, "x")
	if err := SetConvoyStaging(db, convoyID, StagingModeStaged, StagingStrategyStrict); err != nil {
		t.Fatalf("SetConvoyStaging: %v", err)
	}
	c := GetConvoy(db, convoyID)
	if c.StagingMode != StagingModeStaged {
		t.Errorf("staging_mode = %q, want %q", c.StagingMode, StagingModeStaged)
	}
}

// TestSetConvoyStaging_RejectsBadMode covers input validation.
func TestSetConvoyStaging_RejectsBadMode(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()
	convoyID, _ := CreateConvoy(db, "x")
	if err := SetConvoyStaging(db, convoyID, "garbage", StagingStrategyStrict); err == nil {
		t.Fatal("expected error for bad mode; got nil")
	}
}

// TestCreateConvoy_StagedPlan_RowsLanded — end-to-end: build a 2-stage
// plan via store helpers, create the convoy, populate tasks with stage
// stamps, and verify everything queries back coherently. This is the
// integration test the task spec calls out as "TestCreateConvoy_StagedPlan_RowsLanded."
func TestCreateConvoy_StagedPlan_RowsLanded(t *testing.T) {
	db := mustInitDB(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, description) VALUES ('api','/tmp/api','x'), ('frontend','/tmp/fe','y')`); err != nil {
		t.Fatalf("insert repos: %v", err)
	}

	specs := []StagedStageSpec{
		{StageNum: 1, Intent: "Add nullable column", GateType: "soak_minutes", GateConfigJSON: `{"type":"soak_minutes","config":{"minutes":60}}`},
		{StageNum: 2, Intent: "Backfill", GateType: "operator_confirm", GateConfigJSON: `{"type":"operator_confirm"}`},
		{StageNum: 3, Intent: "Read from new column", GateType: "", GateConfigJSON: ""},
	}
	convoyID, stageIDs, err := CreateStagedConvoy(db, "[100] zdm-rollout", StagingStrategyStrict, specs)
	if err != nil {
		t.Fatalf("CreateStagedConvoy: %v", err)
	}

	// Insert tasks: 2 in stage 1, 1 in stage 2, 1 in stage 3.
	tx, _ := db.Begin()
	tasksByStage := map[int][]int{}
	plans := []struct {
		repo, payload string
		stageIdx      int
	}{
		{"api", "add column migration", 0},
		{"api", "add column code path", 0},
		{"api", "backfill script", 1},
		{"frontend", "switch reads", 2},
	}
	for _, p := range plans {
		tid, terr := AddConvoyTaskWithStageTx(tx, 0, p.repo, p.payload, convoyID, 0, stageIDs[p.stageIdx], "Pending")
		if terr != nil {
			tx.Rollback()
			t.Fatalf("AddConvoyTaskWithStageTx stage_idx=%d: %v", p.stageIdx, terr)
		}
		tasksByStage[stageIDs[p.stageIdx]] = append(tasksByStage[stageIDs[p.stageIdx]], tid)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Convoy assertions.
	c := GetConvoy(db, convoyID)
	if c.StagingMode != StagingModeStaged || c.StagingStrategy != StagingStrategyStrict {
		t.Errorf("convoy staging_mode/staging_strategy = %q/%q, want %q/%q",
			c.StagingMode, c.StagingStrategy, StagingModeStaged, StagingStrategyStrict)
	}

	// 3 ConvoyStages rows in expected order + status mix.
	stages, _ := ListStages(db, convoyID)
	if len(stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(stages))
	}
	if stages[0].Status != StageStatusOpen || stages[1].Status != StageStatusPending || stages[2].Status != StageStatusPending {
		t.Errorf("stage statuses = [%s, %s, %s]; want [Open, Pending, Pending]",
			stages[0].Status, stages[1].Status, stages[2].Status)
	}
	if stages[0].IntentText != "Add nullable column" {
		t.Errorf("stage 1 intent_text = %q", stages[0].IntentText)
	}

	// 4 BountyBoard rows tagged with the right stage_id each.
	for stageID, taskIDs := range tasksByStage {
		for _, tid := range taskIDs {
			var s sql.NullInt64
			db.QueryRow(`SELECT stage_id FROM BountyBoard WHERE id = ?`, tid).Scan(&s)
			if !s.Valid || int(s.Int64) != stageID {
				t.Errorf("task %d stage_id = %v, want %d", tid, s, stageID)
			}
		}
	}

	// Per-stage scan via the new partial index — every populated row is
	// reachable via WHERE stage_id = ?.
	var n1, n2, n3 int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE stage_id = ?`, stageIDs[0]).Scan(&n1)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE stage_id = ?`, stageIDs[1]).Scan(&n2)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE stage_id = ?`, stageIDs[2]).Scan(&n3)
	if n1 != 2 || n2 != 1 || n3 != 1 {
		t.Errorf("per-stage task counts = %d/%d/%d, want 2/1/1", n1, n2, n3)
	}
}

// mustInitDB returns a fresh in-memory holocron DB for tests.
func mustInitDB(t *testing.T) *sql.DB {
	t.Helper()
	db := InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	return db
}
