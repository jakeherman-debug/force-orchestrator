package store

import (
	"database/sql"
	"testing"
)

// ── Forward-compat migration (D5.5 P0) ───────────────────────────────────────
//
// On startup (InitHolocronDSN → runMigrations) every existing convoy is
// retro-fitted with a stage 1 row in status='Open' and gate_type=NULL, and
// every existing ConvoyAskBranches row is pointed at that stage. These tests
// simulate "old-shaped" data by clearing the auto-created stage rows and
// then re-running runMigrations to verify the backfill logic works on its
// own — and that re-running it is a no-op (idempotent).

// resetToPreD55Shape strips the rows the forward-compat migration added so
// we can replay the migration on data that "looks like" pre-D5.5.
func resetToPreD55Shape(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`UPDATE ConvoyAskBranches SET stage_id = NULL`); err != nil {
		t.Fatalf("reset stage_id: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM ConvoyStages`); err != nil {
		t.Fatalf("delete stages: %v", err)
	}
}

func TestMigration_ExistingConvoy_GetsSingleStage(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed: a "pre-D5.5" convoy with a ConvoyAskBranch row.
	convoyID, err := CreateConvoy(db, "convoy-existing-pre-d55")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	AddRepo(db, "api", "/tmp/api", "")
	if err := UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-1-feat", "deadbeef"); err != nil {
		t.Fatalf("UpsertConvoyAskBranch: %v", err)
	}

	// Drop the stage rows the migration auto-created at init so we're
	// truly simulating pre-D5.5 shape.
	resetToPreD55Shape(t, db)

	// Sanity: stage 1 absent before migration runs.
	if _, err := GetStageByNum(db, convoyID, 1); err == nil {
		t.Fatalf("pre-migration: stage 1 unexpectedly present")
	}
	// Sanity: ConvoyAskBranches row exists with NULL stage_id.
	var stageIDBefore sql.NullInt64
	if err := db.QueryRow(`SELECT stage_id FROM ConvoyAskBranches WHERE convoy_id = ? AND repo = ?`,
		convoyID, "api").Scan(&stageIDBefore); err != nil {
		t.Fatalf("read stage_id pre-migration: %v", err)
	}
	if stageIDBefore.Valid {
		t.Fatalf("pre-migration: stage_id = %d, want NULL", stageIDBefore.Int64)
	}

	// Run the migration.
	runMigrations(db)

	// Stage 1 must now exist with the correct shape.
	got, err := GetStageByNum(db, convoyID, 1)
	if err != nil {
		t.Fatalf("post-migration GetStageByNum(1): %v", err)
	}
	if got.StageNum != 1 {
		t.Errorf("stage_num = %d, want 1", got.StageNum)
	}
	if got.Status != StageStatusOpen {
		t.Errorf("status = %q, want Open", got.Status)
	}
	if !got.GateTypeIsNull {
		t.Errorf("gate_type = %q, want NULL", got.GateType)
	}
	if got.GateConfigJSON != "{}" {
		t.Errorf("gate_config_json = %q, want '{}'", got.GateConfigJSON)
	}
	if got.OpenedAt == "" {
		t.Errorf("opened_at empty, want populated from convoy.created_at")
	}

	// ConvoyAskBranches.stage_id must point at the new row.
	var stageIDAfter sql.NullInt64
	if err := db.QueryRow(`SELECT stage_id FROM ConvoyAskBranches WHERE convoy_id = ? AND repo = ?`,
		convoyID, "api").Scan(&stageIDAfter); err != nil {
		t.Fatalf("read stage_id post-migration: %v", err)
	}
	if !stageIDAfter.Valid {
		t.Fatalf("post-migration: stage_id is NULL, want populated")
	}
	if int(stageIDAfter.Int64) != got.ID {
		t.Errorf("stage_id = %d, want %d (the migration-created stage)", stageIDAfter.Int64, got.ID)
	}
}

func TestMigration_StagingMode_DefaultsToSingle(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-staging-defaults")

	c := GetConvoy(db, convoyID)
	if c == nil {
		t.Fatalf("GetConvoy: nil")
	}
	if c.StagingMode != StagingModeSingle {
		t.Errorf("staging_mode = %q, want %q", c.StagingMode, StagingModeSingle)
	}
	if c.StagingStrategy != StagingStrategyStrict {
		t.Errorf("staging_strategy = %q, want %q", c.StagingStrategy, StagingStrategyStrict)
	}
}

func TestMigration_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-idempotent")
	AddRepo(db, "api", "/tmp/api", "")
	if err := UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-1", "deadbeef"); err != nil {
		t.Fatalf("UpsertConvoyAskBranch: %v", err)
	}

	// Run migrations 3 times — must remain a no-op after the first.
	runMigrations(db)
	runMigrations(db)
	runMigrations(db)

	// Exactly one stage row for the convoy.
	stages, err := ListStages(db, convoyID)
	if err != nil {
		t.Fatalf("ListStages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("after 3 migrations: %d stage rows, want 1", len(stages))
	}
	if stages[0].StageNum != 1 || stages[0].Status != StageStatusOpen {
		t.Errorf("stage row = %+v, want stage_num=1 status=Open", stages[0])
	}

	// And stage_id is still populated correctly on ConvoyAskBranches.
	var stageID sql.NullInt64
	db.QueryRow(`SELECT stage_id FROM ConvoyAskBranches WHERE convoy_id = ? AND repo = ?`,
		convoyID, "api").Scan(&stageID)
	if !stageID.Valid || int(stageID.Int64) != stages[0].ID {
		t.Errorf("stage_id = %v, want %d", stageID, stages[0].ID)
	}
}

func TestMigration_FreshDB_NoOps(t *testing.T) {
	// A fresh DB has no convoys; runMigrations must be a no-op (no panics,
	// no errors, no rows materialised).
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Verify the freshly-initialised DB has no convoy rows.
	var convoyCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Convoys`).Scan(&convoyCount); err != nil {
		t.Fatalf("count convoys: %v", err)
	}
	if convoyCount != 0 {
		t.Fatalf("fresh DB has %d convoys, want 0", convoyCount)
	}

	// Re-run migrations — must not panic, must remain a no-op.
	runMigrations(db)

	var stageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ConvoyStages`).Scan(&stageCount); err != nil {
		t.Fatalf("count stages: %v", err)
	}
	if stageCount != 0 {
		t.Errorf("fresh DB after migration has %d stages, want 0", stageCount)
	}
}

func TestMigration_NewConvoyAfterMigration_GetsStageOnReinit(t *testing.T) {
	// A convoy created at runtime (after the daemon's startup migration ran)
	// will not yet have a stage row — the migration runs again on next
	// startup and backfills it. This test verifies that re-running runMigrations
	// catches up newly-created convoys idempotently.
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-runtime")
	AddRepo(db, "monolith", "/tmp/monolith", "")
	UpsertConvoyAskBranch(db, convoyID, "monolith", "force/ask-X", "cafef00d")

	// Strip the auto-created stage to simulate "convoy created mid-runtime
	// before migration knows about it."
	resetToPreD55Shape(t, db)
	if _, err := GetStageByNum(db, convoyID, 1); err == nil {
		t.Fatalf("pre-migration: stage 1 unexpectedly present")
	}

	// Daemon restart → runMigrations runs again → backfill kicks in.
	runMigrations(db)

	got, err := GetStageByNum(db, convoyID, 1)
	if err != nil {
		t.Fatalf("post-restart GetStageByNum: %v", err)
	}
	if got.Status != StageStatusOpen {
		t.Errorf("status = %q, want Open", got.Status)
	}
}

func TestMigration_ExistingNonNullStageId_NotOverwritten(t *testing.T) {
	// If a ConvoyAskBranches row already has stage_id set (e.g. it's a real
	// staged-convoy row from D5.5+), the migration must NOT overwrite it.
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-explicit-stage")
	AddRepo(db, "api", "/tmp/api", "")
	UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-N", "abc123")

	// Migration has populated stage 1 and pointed the ask-branch at it.
	stageOne, err := GetStageByNum(db, convoyID, 1)
	if err != nil {
		t.Fatalf("GetStageByNum(1): %v", err)
	}

	// Now create stage 2 explicitly and re-point the ask-branch at it
	// (simulating a multi-stage convoy where some repo's ask-branch lives
	// in stage 2). This is contrived but exercises the WHERE stage_id IS NULL
	// guard in the backfill UPDATE.
	stageTwoID, err := CreateStage(db, convoyID, 2, "stage 2", "operator_confirm", "{}")
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	if _, err := db.Exec(`UPDATE ConvoyAskBranches SET stage_id = ? WHERE convoy_id = ? AND repo = ?`,
		stageTwoID, convoyID, "api"); err != nil {
		t.Fatalf("set stage_id to stage 2: %v", err)
	}

	// Re-run migration. The ask-branch row should still point at stage 2,
	// not be reset to stage 1.
	runMigrations(db)

	var stageID sql.NullInt64
	db.QueryRow(`SELECT stage_id FROM ConvoyAskBranches WHERE convoy_id = ? AND repo = ?`,
		convoyID, "api").Scan(&stageID)
	if !stageID.Valid {
		t.Fatalf("stage_id is NULL after migration, want preserved")
	}
	if int(stageID.Int64) != stageTwoID {
		t.Errorf("stage_id = %d, want %d (stage 2)", stageID.Int64, stageTwoID)
	}
	if int(stageID.Int64) == stageOne.ID {
		t.Errorf("migration overwrote stage_id back to stage 1")
	}
}
