package store

import (
	"strings"
	"testing"
)

// ── CreateStage ──────────────────────────────────────────────────────────────

func TestCreateStage_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := CreateConvoy(db, "convoy-create-stage-happy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	// The forward-compat migration auto-created stage 1 for this convoy at
	// daemon init. Use stage 2 here so we exercise the normal create path.
	stageID, err := CreateStage(db, convoyID, 2, "ship monolith change", "soak_minutes", `{"minutes":15}`)
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	if stageID <= 0 {
		t.Fatalf("CreateStage: returned non-positive id %d", stageID)
	}

	got, err := GetStage(db, stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.ConvoyID != convoyID {
		t.Errorf("convoy_id = %d, want %d", got.ConvoyID, convoyID)
	}
	if got.StageNum != 2 {
		t.Errorf("stage_num = %d, want 2", got.StageNum)
	}
	if got.IntentText != "ship monolith change" {
		t.Errorf("intent_text = %q, want %q", got.IntentText, "ship monolith change")
	}
	if got.Status != StageStatusPending {
		t.Errorf("status = %q, want Pending", got.Status)
	}
	if got.GateType != "soak_minutes" {
		t.Errorf("gate_type = %q, want soak_minutes", got.GateType)
	}
	if got.GateTypeIsNull {
		t.Errorf("gate_type was NULL, want non-NULL")
	}
	if got.GateConfigJSON != `{"minutes":15}` {
		t.Errorf("gate_config_json = %q, want %q", got.GateConfigJSON, `{"minutes":15}`)
	}
	// Default 7 days = 10080 minutes.
	if got.GateTimeoutMinutes != 10080 {
		t.Errorf("gate_timeout_minutes = %d, want 10080", got.GateTimeoutMinutes)
	}
}

func TestCreateStage_NullGate_TerminalShape(t *testing.T) {
	// gate_type='' (no gate) maps to SQL NULL — the schema requires this for
	// the null gate type used on terminal stages of staged convoys.
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-null-gate")

	stageID, err := CreateStage(db, convoyID, 2, "terminal stage", "", "")
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	got, err := GetStage(db, stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if !got.GateTypeIsNull {
		t.Errorf("gate_type IS NOT NULL, want NULL")
	}
	if got.GateConfigJSON != "{}" {
		t.Errorf("gate_config_json = %q, want %q", got.GateConfigJSON, "{}")
	}
}

func TestCreateStage_DuplicateNum_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-dup-stage")

	// Forward-compat migration already created stage 1. Inserting another
	// stage 1 must violate UNIQUE(convoy_id, stage_num).
	if _, err := CreateStage(db, convoyID, 1, "duplicate", "operator_confirm", "{}"); err == nil {
		t.Fatalf("CreateStage(stage_num=1): want UNIQUE violation, got nil")
	}

	// And a manually-created stage 2 cannot be duplicated either.
	if _, err := CreateStage(db, convoyID, 2, "first", "", ""); err != nil {
		t.Fatalf("CreateStage(stage_num=2 first): %v", err)
	}
	if _, err := CreateStage(db, convoyID, 2, "second", "", ""); err == nil {
		t.Fatalf("CreateStage(stage_num=2 second): want UNIQUE violation, got nil")
	}
}

func TestCreateStage_BadInput_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := CreateStage(db, 0, 1, "", "", ""); err == nil {
		t.Errorf("CreateStage(convoyID=0): want error, got nil")
	}
	if _, err := CreateStage(db, 1, 0, "", "", ""); err == nil {
		t.Errorf("CreateStage(stageNum=0): want error, got nil")
	}
}

// ── ListStages / GetStageByNum ───────────────────────────────────────────────

func TestListStages_OrderedByStageNum(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-list-ordered")

	// Insert in reverse order — list must come back ascending.
	// Stage 1 already exists from the forward-compat migration.
	if _, err := CreateStage(db, convoyID, 3, "third", "operator_confirm", "{}"); err != nil {
		t.Fatalf("CreateStage 3: %v", err)
	}
	if _, err := CreateStage(db, convoyID, 2, "second", "soak_minutes", `{"minutes":5}`); err != nil {
		t.Fatalf("CreateStage 2: %v", err)
	}

	got, err := ListStages(db, convoyID)
	if err != nil {
		t.Fatalf("ListStages: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListStages: len = %d, want 3", len(got))
	}
	for i, s := range got {
		if s.StageNum != i+1 {
			t.Errorf("got[%d].stage_num = %d, want %d", i, s.StageNum, i+1)
		}
	}
}

func TestGetStageByNum_HappyAndMiss(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-getbynum")

	// Stage 1 exists from migration.
	got, err := GetStageByNum(db, convoyID, 1)
	if err != nil {
		t.Fatalf("GetStageByNum(1): %v", err)
	}
	if got.StageNum != 1 {
		t.Errorf("stage_num = %d, want 1", got.StageNum)
	}
	if got.Status != StageStatusOpen {
		t.Errorf("migration-stage status = %q, want Open", got.Status)
	}

	// Stage 99 does not exist — must return error (sql.ErrNoRows).
	if _, err := GetStageByNum(db, convoyID, 99); err == nil {
		t.Errorf("GetStageByNum(99): want error, got nil")
	}
}

// ── AdvanceStage ─────────────────────────────────────────────────────────────

func TestAdvanceStage_ValidTransitions(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-advance-valid")
	// Use a freshly-created Pending stage for the full walk. Stage 1 from
	// the migration is already Open, so it's not a clean Pending starting point.
	stageID, err := CreateStage(db, convoyID, 2, "stage 2", "soak_minutes", `{"minutes":1}`)
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}

	walk := []string{
		StageStatusOpen,
		StageStatusAllPRsMerged,
		StageStatusAwaitingGate,
		StageStatusGatePassed,
		StageStatusVerified,
	}
	for _, target := range walk {
		if err := AdvanceStage(db, stageID, target); err != nil {
			t.Fatalf("AdvanceStage → %s: %v", target, err)
		}
		got, err := GetStage(db, stageID)
		if err != nil {
			t.Fatalf("GetStage after %s: %v", target, err)
		}
		if got.Status != target {
			t.Errorf("after AdvanceStage(%s): status = %q, want %q", target, got.Status, target)
		}
	}
}

func TestAdvanceStage_InvalidTransition_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-advance-invalid")
	stageID, _ := CreateStage(db, convoyID, 2, "skip-test", "operator_confirm", "{}")

	// Pending → GatePassed skips three intermediate states; must reject.
	err := AdvanceStage(db, stageID, StageStatusGatePassed)
	if err == nil {
		t.Fatalf("Pending → GatePassed: want error, got nil")
	}
	if !strings.Contains(err.Error(), "illegal transition") {
		t.Errorf("error message = %q, want contains 'illegal transition'", err.Error())
	}

	// Pending → AllPRsMerged also skips Open; must reject.
	if err := AdvanceStage(db, stageID, StageStatusAllPRsMerged); err == nil {
		t.Errorf("Pending → AllPRsMerged: want error, got nil")
	}

	// Unknown target status: reject.
	if err := AdvanceStage(db, stageID, "Bogus"); err == nil {
		t.Errorf("Pending → Bogus: want error, got nil")
	}

	// Status is unchanged after every rejected transition.
	got, _ := GetStage(db, stageID)
	if got.Status != StageStatusPending {
		t.Errorf("after rejected transitions: status = %q, want Pending", got.Status)
	}
}

func TestAdvanceStage_FailedFromAnyNonTerminal(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// fromStatus → ordered list of intermediate transitions to walk through
	// before invoking the Failed transition under test. (Empty list means
	// "stay in initial Pending status".)
	fullPath := []string{
		StageStatusOpen, StageStatusAllPRsMerged,
		StageStatusAwaitingGate, StageStatusGatePassed,
	}
	cases := map[string][]string{
		StageStatusPending:      {},
		StageStatusOpen:         fullPath[0:1],
		StageStatusAllPRsMerged: fullPath[0:2],
		StageStatusAwaitingGate: fullPath[0:3],
		StageStatusGatePassed:   fullPath[0:4],
	}

	for fromStatus, walk := range cases {
		t.Run(fromStatus, func(t *testing.T) {
			convoyID, _ := CreateConvoy(db, "convoy-fail-from-"+fromStatus)
			stageID, _ := CreateStage(db, convoyID, 2, "fail-test", "operator_confirm", "{}")

			for _, s := range walk {
				if err := AdvanceStage(db, stageID, s); err != nil {
					t.Fatalf("setup AdvanceStage → %s: %v", s, err)
				}
			}

			cur, _ := GetStage(db, stageID)
			if cur.Status != fromStatus {
				t.Fatalf("setup landed on %q, want %q", cur.Status, fromStatus)
			}

			if err := AdvanceStage(db, stageID, StageStatusFailed); err != nil {
				t.Fatalf("AdvanceStage(%s → Failed): %v", fromStatus, err)
			}
			got, _ := GetStage(db, stageID)
			if got.Status != StageStatusFailed {
				t.Errorf("status = %q, want Failed", got.Status)
			}
			if got.CompletedAt == "" {
				t.Errorf("completed_at not stamped on Failed transition")
			}
		})
	}
}

func TestAdvanceStage_TerminalStatesAreSticky(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for _, terminal := range []string{StageStatusVerified, StageStatusFailed} {
		convoyID, _ := CreateConvoy(db, "convoy-terminal-"+terminal)
		stageID, _ := CreateStage(db, convoyID, 2, "term-test", "operator_confirm", "{}")

		// Get into the terminal state through legal transitions.
		if terminal == StageStatusVerified {
			for _, s := range []string{
				StageStatusOpen, StageStatusAllPRsMerged,
				StageStatusAwaitingGate, StageStatusGatePassed, StageStatusVerified,
			} {
				if err := AdvanceStage(db, stageID, s); err != nil {
					t.Fatalf("setup → %s: %v", s, err)
				}
			}
		} else {
			if err := AdvanceStage(db, stageID, StageStatusFailed); err != nil {
				t.Fatalf("setup → Failed: %v", err)
			}
		}

		// Now any further transition should be rejected.
		if err := AdvanceStage(db, stageID, StageStatusFailed); err == nil {
			t.Errorf("from %s → Failed: want error (terminal), got nil", terminal)
		}
		if err := AdvanceStage(db, stageID, StageStatusOpen); err == nil {
			t.Errorf("from %s → Open: want error (terminal), got nil", terminal)
		}
	}
}

func TestAdvanceStage_StampsTimestamp(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := CreateConvoy(db, "convoy-stamp")
	stageID, _ := CreateStage(db, convoyID, 2, "stamp-test", "soak_minutes", `{"minutes":1}`)

	// opened_at populated only after Pending → Open.
	pre, _ := GetStage(db, stageID)
	if pre.OpenedAt != "" {
		t.Errorf("opened_at = %q on fresh Pending row, want empty", pre.OpenedAt)
	}
	if err := AdvanceStage(db, stageID, StageStatusOpen); err != nil {
		t.Fatalf("→ Open: %v", err)
	}
	got, _ := GetStage(db, stageID)
	if got.OpenedAt == "" {
		t.Errorf("opened_at empty after Open transition")
	}
	if got.AllPRsMergedAt != "" || got.GatePassedAt != "" || got.CompletedAt != "" {
		t.Errorf("non-Open stamps populated prematurely: %+v", got)
	}

	// AllPRsMerged stamps all_prs_merged_at only.
	if err := AdvanceStage(db, stageID, StageStatusAllPRsMerged); err != nil {
		t.Fatalf("→ AllPRsMerged: %v", err)
	}
	got, _ = GetStage(db, stageID)
	if got.AllPRsMergedAt == "" {
		t.Errorf("all_prs_merged_at empty after AllPRsMerged transition")
	}

	// AwaitingGate stamps nothing extra.
	if err := AdvanceStage(db, stageID, StageStatusAwaitingGate); err != nil {
		t.Fatalf("→ AwaitingGate: %v", err)
	}
	got, _ = GetStage(db, stageID)
	if got.GatePassedAt != "" {
		t.Errorf("gate_passed_at populated on AwaitingGate, want empty")
	}

	// GatePassed stamps gate_passed_at.
	if err := AdvanceStage(db, stageID, StageStatusGatePassed); err != nil {
		t.Fatalf("→ GatePassed: %v", err)
	}
	got, _ = GetStage(db, stageID)
	if got.GatePassedAt == "" {
		t.Errorf("gate_passed_at empty after GatePassed transition")
	}

	// Verified stamps completed_at.
	if err := AdvanceStage(db, stageID, StageStatusVerified); err != nil {
		t.Fatalf("→ Verified: %v", err)
	}
	got, _ = GetStage(db, stageID)
	if got.CompletedAt == "" {
		t.Errorf("completed_at empty after Verified transition")
	}
}

func TestAdvanceStage_BadInput_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := AdvanceStage(db, 0, StageStatusOpen); err == nil {
		t.Errorf("AdvanceStage(stageID=0): want error, got nil")
	}
	if err := AdvanceStage(db, 999999, StageStatusOpen); err == nil {
		t.Errorf("AdvanceStage(missing stage): want error, got nil")
	}
}

// ── BypassStage (D5.5 exit criterion #10) ───────────────────────────────────

// TestBypassStage_FromAwaitingGate — bypass mid-soak: stage transitions to
// GatePassed and gate_passed_at is stamped, even though no gate evaluator
// was consulted. Reuses the existing all_prs_merged_at stamp.
func TestBypassStage_FromAwaitingGate(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	cid, err := CreateConvoy(db, "convoy-bypass-1")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	sid, err := CreateStage(db, cid, 2, "soak phase", "soak_minutes", `{"minutes":1440}`)
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	if err := AdvanceStage(db, sid, StageStatusOpen); err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	if err := AdvanceStage(db, sid, StageStatusAllPRsMerged); err != nil {
		t.Fatalf("seed AllPRsMerged: %v", err)
	}
	if err := AdvanceStage(db, sid, StageStatusAwaitingGate); err != nil {
		t.Fatalf("seed AwaitingGate: %v", err)
	}
	prevMerged, _ := GetStage(db, sid)

	if err := BypassStage(db, sid); err != nil {
		t.Fatalf("BypassStage: %v", err)
	}
	got, _ := GetStage(db, sid)
	if got.Status != StageStatusGatePassed {
		t.Errorf("status = %q, want GatePassed", got.Status)
	}
	if got.GatePassedAt == "" {
		t.Errorf("gate_passed_at not stamped")
	}
	if got.AllPRsMergedAt != prevMerged.AllPRsMergedAt {
		t.Errorf("BypassStage clobbered an existing all_prs_merged_at: was=%q now=%q",
			prevMerged.AllPRsMergedAt, got.AllPRsMergedAt)
	}
}

// TestBypassStage_FromPending — bypass from Pending: skips Open, AllPRsMerged,
// AwaitingGate. all_prs_merged_at is backfilled because it was empty.
func TestBypassStage_FromPending(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	cid, err := CreateConvoy(db, "convoy-bypass-2")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	sid, err := CreateStage(db, cid, 2, "ad hoc", "operator_confirm", `{"prompt":"x"}`)
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}

	if err := BypassStage(db, sid); err != nil {
		t.Fatalf("BypassStage: %v", err)
	}
	got, _ := GetStage(db, sid)
	if got.Status != StageStatusGatePassed {
		t.Errorf("status = %q, want GatePassed", got.Status)
	}
	if got.AllPRsMergedAt == "" {
		t.Errorf("all_prs_merged_at should be backfilled when bypass skips the merge transition")
	}
	if got.GatePassedAt == "" {
		t.Errorf("gate_passed_at not stamped")
	}
}

// TestBypassStage_TerminalRejected — Verified and Failed stages reject bypass.
func TestBypassStage_TerminalRejected(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	cid, err := CreateConvoy(db, "convoy-bypass-3")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	sid, err := CreateStage(db, cid, 2, "x", "soak_minutes", `{"minutes":1}`)
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	if err := AdvanceStage(db, sid, StageStatusFailed); err != nil {
		t.Fatalf("seed Failed: %v", err)
	}
	if err := BypassStage(db, sid); err == nil {
		t.Errorf("BypassStage on Failed: want error, got nil")
	}
}

// TestBypassStage_BadInput_Errors — guard rails for nonsense ids.
func TestBypassStage_BadInput_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if err := BypassStage(db, 0); err == nil {
		t.Errorf("BypassStage(stageID=0): want error, got nil")
	}
	if err := BypassStage(db, 999999); err == nil {
		t.Errorf("BypassStage(missing stage): want error, got nil")
	}
}

// ── GetRepositoryReleaseLabelPattern / Set ───────────────────────────────────

func TestGetRepositoryReleaseLabelPattern_DefaultEmpty(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "test-repo", "/tmp/test-repo", "test")
	got, err := GetRepositoryReleaseLabelPattern(db, "test-repo")
	if err != nil {
		t.Fatalf("GetRepositoryReleaseLabelPattern: %v", err)
	}
	if got != "" {
		t.Errorf("default release_label_pattern = %q, want empty", got)
	}
}

func TestSetRepositoryReleaseLabelPattern_RoundTrip(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "monolith", "/tmp/monolith", "")

	pattern := `release-202\d+\.\d+`
	if err := SetRepositoryReleaseLabelPattern(db, "monolith", pattern); err != nil {
		t.Fatalf("SetRepositoryReleaseLabelPattern: %v", err)
	}
	got, err := GetRepositoryReleaseLabelPattern(db, "monolith")
	if err != nil {
		t.Fatalf("GetRepositoryReleaseLabelPattern: %v", err)
	}
	if got != pattern {
		t.Errorf("got %q, want %q", got, pattern)
	}

	// Clearing back to empty.
	if err := SetRepositoryReleaseLabelPattern(db, "monolith", ""); err != nil {
		t.Fatalf("SetRepositoryReleaseLabelPattern(clear): %v", err)
	}
	got, _ = GetRepositoryReleaseLabelPattern(db, "monolith")
	if got != "" {
		t.Errorf("after clear: got %q, want empty", got)
	}

	// Repository.ReleaseLabelPattern is also surfaced by GetRepo.
	if err := SetRepositoryReleaseLabelPattern(db, "monolith", "rel/.*"); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	r := GetRepo(db, "monolith")
	if r == nil {
		t.Fatalf("GetRepo: nil")
	}
	if r.ReleaseLabelPattern != "rel/.*" {
		t.Errorf("Repository.ReleaseLabelPattern = %q, want %q", r.ReleaseLabelPattern, "rel/.*")
	}
}

func TestSetRepositoryReleaseLabelPattern_PreservedAcrossAddRepo(t *testing.T) {
	// AddRepo's INSERT OR REPLACE must not clobber a previously-set
	// release_label_pattern when an operator re-registers the repo (e.g. to
	// change local_path).
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "api", "/tmp/api", "")
	if err := SetRepositoryReleaseLabelPattern(db, "api", "release/v\\d+"); err != nil {
		t.Fatalf("SetRepositoryReleaseLabelPattern: %v", err)
	}
	// Re-add (e.g. operator updates local_path or description).
	AddRepo(db, "api", "/different/path", "updated description")
	got, err := GetRepositoryReleaseLabelPattern(db, "api")
	if err != nil {
		t.Fatalf("GetRepositoryReleaseLabelPattern: %v", err)
	}
	if got != "release/v\\d+" {
		t.Errorf("after re-AddRepo: pattern = %q, want preserved", got)
	}
}

func TestRepositoryReleaseLabelPattern_UnknownRepo_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := GetRepositoryReleaseLabelPattern(db, "nonexistent"); err == nil {
		t.Errorf("GetRepositoryReleaseLabelPattern(missing): want error, got nil")
	}
	if err := SetRepositoryReleaseLabelPattern(db, "nonexistent", "anything"); err == nil {
		t.Errorf("SetRepositoryReleaseLabelPattern(missing): want error, got nil")
	}
	if _, err := GetRepositoryReleaseLabelPattern(db, ""); err == nil {
		t.Errorf("GetRepositoryReleaseLabelPattern(\"\"): want error, got nil")
	}
}
