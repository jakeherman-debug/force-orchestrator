package commander

import (
	"encoding/json"
	"strings"
	"testing"

	"force-orchestrator/internal/stagegate"
	"force-orchestrator/internal/store"
)

// newRegistry builds a stagegate.Registry pre-loaded with the 5 baseline
// gates (soak_minutes, operator_confirm, null, all_of, any_of). Validator
// tests use this so unknown-type rejection actually exercises the registry
// lookup instead of an empty map.
func newRegistry(t *testing.T) *stagegate.Registry {
	t.Helper()
	r := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(r)
	return r
}

// soakGate builds a leaf soak_minutes gate spec for use in tests.
func soakGate(minutes int) *StagingPlanGate {
	return &StagingPlanGate{
		Type:   "soak_minutes",
		Config: map[string]interface{}{"minutes": minutes},
	}
}

func operatorConfirmGate() *StagingPlanGate {
	return &StagingPlanGate{Type: "operator_confirm"}
}

func taskN(id int, repo string) store.TaskPlan {
	return store.TaskPlan{TempID: id, Repo: repo, Task: "do the thing", BlockedBy: []int{}}
}

// TestValidateStagingPlan_SingleStage_OK proves a `staging_mode: "single"`
// plan validates trivially (stages list ignored). This is the no-op path
// that lets the Commander emit the staging-mode wrapper without changing
// behavior for non-staged convoys.
func TestValidateStagingPlan_SingleStage_OK(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeSingle),
		StagingStrategy: store.StagingStrategyStrict,
	}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err != nil {
		t.Fatalf("single-mode plan should validate: %v", err)
	}
}

// TestValidateStagingPlan_SingleStage_NoStrategyOK — single mode allows
// strategy to be omitted (the field is meaningless in single mode).
func TestValidateStagingPlan_SingleStage_NoStrategyOK(t *testing.T) {
	plan := StagingPlan{StagingMode: string(store.StagingModeSingle)}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err != nil {
		t.Fatalf("single-mode plan with empty strategy should validate: %v", err)
	}
}

// TestValidateStagingPlan_StagedStrict_HappyPath: full 3-stage plan with
// soak_minutes / operator_confirm / null gate (terminal) — the canonical
// shape from docs/roadmap.md.
func TestValidateStagingPlan_StagedStrict_HappyPath(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{
				StageNum: 1,
				Intent:   "Add nullable user_account_status column",
				Tasks:    []store.TaskPlan{taskN(1, "api")},
				Gate:     soakGate(60),
			},
			{
				StageNum: 2,
				Intent:   "Dual-write to both old and new column",
				Tasks:    []store.TaskPlan{taskN(2, "api")},
				Gate:     operatorConfirmGate(),
			},
			{
				StageNum: 3,
				Intent:   "Read from new column only",
				Tasks:    []store.TaskPlan{taskN(3, "api")},
				Gate:     nil, // terminal stage allowed null gate
			},
		},
	}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err != nil {
		t.Fatalf("happy-path staged plan should validate: %v", err)
	}
}

// TestValidateStagingPlan_StagedMergeParallel_RejectsWithExplicitError —
// per spec, merge_parallel is forward-compat-recognised but not yet
// supported. The error message must explicitly say "not yet implemented"
// so the operator surface explains the deferral.
func TestValidateStagingPlan_StagedMergeParallel_RejectsWithExplicitError(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyMergeParallel,
		Stages:          []StagingPlanStage{{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: nil}},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected rejection of merge_parallel; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "merge_parallel") || !strings.Contains(msg, "not yet implemented") {
		t.Fatalf("merge_parallel rejection must name strategy + 'not yet implemented'; got %q", msg)
	}
}

// TestValidateStagingPlan_StagedStacked_RejectsWithExplicitError — same
// shape as merge_parallel, ensures `stacked` is rejected with its own
// dedicated message.
func TestValidateStagingPlan_StagedStacked_RejectsWithExplicitError(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStacked,
		Stages:          []StagingPlanStage{{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: nil}},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected rejection of stacked; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "stacked") || !strings.Contains(msg, "not yet implemented") {
		t.Fatalf("stacked rejection must name strategy + 'not yet implemented'; got %q", msg)
	}
}

// TestValidateStagingPlan_UnknownStrategy_Errors covers the catch-all
// for strategy values outside the recognised enum.
func TestValidateStagingPlan_UnknownStrategy_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: "rolling_canary",
		Stages:          []StagingPlanStage{{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: nil}},
	}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err == nil {
		t.Fatal("expected rejection of unknown strategy; got nil")
	}
}

// TestValidateStagingPlan_MissingStrategy_Errors — staged mode requires
// a non-empty strategy.
func TestValidateStagingPlan_MissingStrategy_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode: string(store.StagingModeStaged),
		Stages:      []StagingPlanStage{{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: nil}},
	}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err == nil {
		t.Fatal("expected rejection of empty strategy in staged mode; got nil")
	}
}

// TestValidateStagingPlan_EmptyStages_Errors — staged mode requires at
// least one stage.
func TestValidateStagingPlan_EmptyStages_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages:          []StagingPlanStage{},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected rejection of empty stages; got nil")
	}
	if !strings.Contains(err.Error(), "empty stages") {
		t.Fatalf("error message must name 'empty stages'; got %q", err.Error())
	}
}

// TestValidateStagingPlan_NonContiguousStageNum_Errors — stages must be
// 1-indexed contiguous (1, 2, 3, ...). Skipping or starting at 2 fails.
func TestValidateStagingPlan_NonContiguousStageNum_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: soakGate(60)},
			{StageNum: 3, Intent: "y", Tasks: []store.TaskPlan{taskN(2, "api")}, Gate: nil},
		},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected rejection of non-contiguous stage_num; got nil")
	}
	if !strings.Contains(err.Error(), "stage_num") {
		t.Fatalf("error should name stage_num; got %q", err.Error())
	}
}

// TestValidateStagingPlan_NullGateOnNonTerminalStage_Errors — gate=null
// is allowed only on the terminal stage. Anti-cheat directive #3.
func TestValidateStagingPlan_NullGateOnNonTerminalStage_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: nil},
			{StageNum: 2, Intent: "y", Tasks: []store.TaskPlan{taskN(2, "api")}, Gate: nil},
		},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected rejection of null gate on non-terminal stage; got nil")
	}
	if !strings.Contains(err.Error(), "gate=null") || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("error must name 'gate=null' + 'terminal'; got %q", err.Error())
	}
}

// TestValidateStagingPlan_NullGateOnTerminalStage_OK — the inverse: a
// null gate on the LAST stage validates cleanly.
func TestValidateStagingPlan_NullGateOnTerminalStage_OK(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: soakGate(60)},
			{StageNum: 2, Intent: "y", Tasks: []store.TaskPlan{taskN(2, "api")}, Gate: nil},
		},
	}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err != nil {
		t.Fatalf("null gate on terminal stage should validate: %v", err)
	}
}

// TestValidateStagingPlan_CompoundGate_NestingDepthExceeded_Errors —
// build a compound tree deeper than stagegate.MaxNestingDepth. The
// validator must refuse it.
func TestValidateStagingPlan_CompoundGate_NestingDepthExceeded_Errors(t *testing.T) {
	// Construct a chain of all_of compounds, each wrapping the next,
	// with one leaf at the bottom. Depth = MaxNestingDepth+1.
	deepest := soakGate(60)
	cur := &StagingPlanGate{Type: "all_of", Gates: []*StagingPlanGate{deepest}}
	for i := 0; i < stagegate.MaxNestingDepth+1; i++ {
		cur = &StagingPlanGate{Type: "all_of", Gates: []*StagingPlanGate{cur}}
	}
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: cur},
		},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected nesting-depth rejection; got nil")
	}
	if !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("error must mention nesting depth; got %q", err.Error())
	}
}

// TestValidateStagingPlan_CompoundGate_EmptyChildren_Errors — an
// all_of/any_of with zero children can never resolve and is rejected.
func TestValidateStagingPlan_CompoundGate_EmptyChildren_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{{
			StageNum: 1,
			Intent:   "x",
			Tasks:    []store.TaskPlan{taskN(1, "api")},
			Gate:     &StagingPlanGate{Type: "all_of", Gates: []*StagingPlanGate{}},
		}},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected empty-children rejection; got nil")
	}
	if !strings.Contains(err.Error(), "empty children") {
		t.Fatalf("error must mention 'empty children'; got %q", err.Error())
	}
}

// TestValidateStagingPlan_CompoundGate_DeepButValid_OK — a 3-deep
// all_of nest within the cap validates successfully.
func TestValidateStagingPlan_CompoundGate_DeepButValid_OK(t *testing.T) {
	leaf := soakGate(30)
	mid := &StagingPlanGate{Type: "all_of", Gates: []*StagingPlanGate{leaf, operatorConfirmGate()}}
	root := &StagingPlanGate{Type: "any_of", Gates: []*StagingPlanGate{mid, soakGate(60)}}
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: root},
		},
	}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err != nil {
		t.Fatalf("deep-but-valid compound should pass: %v", err)
	}
}

// TestValidateStagingPlan_StageWithoutTasks_Errors — every stage must
// carry at least one CodeEdit task.
func TestValidateStagingPlan_StageWithoutTasks_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{{
			StageNum: 1,
			Intent:   "x",
			Tasks:    []store.TaskPlan{},
			Gate:     nil,
		}},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected rejection of stage with no tasks; got nil")
	}
	if !strings.Contains(err.Error(), "tasks list is empty") {
		t.Fatalf("error must name 'tasks list is empty'; got %q", err.Error())
	}
}

// TestValidateStagingPlan_UnknownGateType_Errors — a gate type not in
// the registry is rejected. This covers (a) typos in the LLM output and
// (b) advanced-leaf names emitted before the registry has them
// registered (e.g. release_label_present in P3).
func TestValidateStagingPlan_UnknownGateType_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{{
			StageNum: 1,
			Intent:   "x",
			Tasks:    []store.TaskPlan{taskN(1, "api")},
			Gate:     &StagingPlanGate{Type: "release_label_present"},
		}},
	}
	err := ValidateStagingPlan(plan, newRegistry(t))
	if err == nil {
		t.Fatal("expected rejection of unknown gate type; got nil")
	}
	if !strings.Contains(err.Error(), "release_label_present") || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("error must name unknown gate type; got %q", err.Error())
	}
}

// TestValidateStagingPlan_MissingMode_Errors — empty staging_mode is rejected.
func TestValidateStagingPlan_MissingMode_Errors(t *testing.T) {
	plan := StagingPlan{}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err == nil {
		t.Fatal("expected rejection of missing staging_mode; got nil")
	}
}

// TestValidateStagingPlan_NilRegistry_Errors — caller bug; surface explicitly.
func TestValidateStagingPlan_NilRegistry_Errors(t *testing.T) {
	plan := StagingPlan{StagingMode: string(store.StagingModeSingle)}
	if err := ValidateStagingPlan(plan, nil); err == nil {
		t.Fatal("expected rejection of nil registry; got nil")
	}
}

// TestValidateStagingPlan_EmptyIntent_Errors — intent is required because
// the Commander's reasoning about why each stage is independently safe
// must be captured (it surfaces to ConvoyReview at each DraftPROpen).
func TestValidateStagingPlan_EmptyIntent_Errors(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{{
			StageNum: 1,
			Intent:   "   ",
			Tasks:    []store.TaskPlan{taskN(1, "api")},
			Gate:     nil,
		}},
	}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err == nil {
		t.Fatal("expected rejection of empty intent; got nil")
	}
}

// TestValidateStagingPlan_CompoundGate_NestedConfigShape_OK — accept the
// alternative shape where compound children live under config.gates
// rather than top-level gates. This forwards-compat with the docs
// example which puts children under config.
func TestValidateStagingPlan_CompoundGate_NestedConfigShape_OK(t *testing.T) {
	gate := &StagingPlanGate{
		Type: "all_of",
		Config: map[string]interface{}{
			"gates": []interface{}{
				map[string]interface{}{
					"type":   "soak_minutes",
					"config": map[string]interface{}{"minutes": 60},
				},
			},
		},
	}
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{StageNum: 1, Intent: "x", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: gate},
		},
	}
	if err := ValidateStagingPlan(plan, newRegistry(t)); err != nil {
		t.Fatalf("compound with config.gates shape should validate: %v", err)
	}
}

// TestParseStagingPlan_LegacyArrayShape_NotStaged proves a bare
// []TaskPlan JSON is recognised as not-staged so the legacy code path
// is undisturbed.
func TestParseStagingPlan_LegacyArrayShape_NotStaged(t *testing.T) {
	raw := []byte(`[{"id":1,"repo":"api","task":"x","blocked_by":[]}]`)
	plan, staged, err := ParseStagingPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error on legacy shape: %v", err)
	}
	if staged {
		t.Fatal("legacy array shape must not register as staged")
	}
	if plan != nil {
		t.Fatal("legacy shape must return nil plan")
	}
}

// TestParseStagingPlan_StagedObjectShape_Staged — recognises the staged
// envelope and returns the parsed plan.
func TestParseStagingPlan_StagedObjectShape_Staged(t *testing.T) {
	raw := []byte(`{"staging_mode":"staged","staging_strategy":"strict","stages":[
		{"stage_num":1,"intent":"add column","tasks":[{"id":1,"repo":"api","task":"x","blocked_by":[]}],"gate":{"type":"soak_minutes","config":{"minutes":60}}}
	]}`)
	plan, staged, err := ParseStagingPlan(raw)
	if err != nil {
		t.Fatalf("staged shape parse error: %v", err)
	}
	if !staged || plan == nil {
		t.Fatalf("staged shape must register as staged with non-nil plan (staged=%v plan=%v)", staged, plan)
	}
	if plan.StagingMode != "staged" || len(plan.Stages) != 1 || plan.Stages[0].Gate == nil {
		t.Fatalf("parsed plan shape mismatch: %+v", plan)
	}
}

// TestParseStagingPlan_SingleModeObject_NotStaged — when the LLM
// emits an object with `staging_mode: "single"`, ParseStagingPlan
// signals not-staged so the legacy path takes over.
func TestParseStagingPlan_SingleModeObject_NotStaged(t *testing.T) {
	raw := []byte(`{"staging_mode":"single"}`)
	plan, staged, err := ParseStagingPlan(raw)
	if err != nil {
		t.Fatalf("single-mode object parse error: %v", err)
	}
	if staged || plan != nil {
		t.Fatalf("single-mode object must register as not staged; got staged=%v plan=%v", staged, plan)
	}
}

// TestStagingPlan_ToStageSpecs_RoundTrip proves the conversion from
// validated StagingPlan to []store.StagedStageSpec preserves every
// per-stage field including the gate JSON.
func TestStagingPlan_ToStageSpecs_RoundTrip(t *testing.T) {
	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{StageNum: 1, Intent: "add column", Tasks: []store.TaskPlan{taskN(1, "api")}, Gate: soakGate(60)},
			{StageNum: 2, Intent: "dual-write", Tasks: []store.TaskPlan{taskN(2, "api")}, Gate: nil},
		},
	}
	specs, err := plan.ToStageSpecs()
	if err != nil {
		t.Fatalf("ToStageSpecs error: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}
	if specs[0].StageNum != 1 || specs[0].Intent != "add column" || specs[0].GateType != "soak_minutes" {
		t.Fatalf("stage 1 spec shape wrong: %+v", specs[0])
	}
	// gate_config_json should be valid JSON parseable as the gate spec.
	var roundTrip StagingPlanGate
	if uErr := json.Unmarshal([]byte(specs[0].GateConfigJSON), &roundTrip); uErr != nil {
		t.Fatalf("gate config json not parseable: %v", uErr)
	}
	if roundTrip.Type != "soak_minutes" {
		t.Fatalf("round-tripped gate type mismatch: %q", roundTrip.Type)
	}
	// terminal stage — null gate → empty GateType + empty config json.
	if specs[1].GateType != "" {
		t.Fatalf("stage 2 should have empty gate type (null gate); got %q", specs[1].GateType)
	}
}

// ── ValidateReleaseLabelGateForRepos (D5.5 P3 γ) ───────────────────────────

// p3Registry returns a stagegate.Registry pre-loaded with the baseline +
// the P3 advanced gates so structural validation passes for tests that
// exercise the per-repo pattern check. The release_label_present gate
// needs a PRLabelFetcher; the validator never invokes it (only the
// runtime dispatcher does), so we wire a no-op stub here.
func p3Registry(t *testing.T) *stagegate.Registry {
	t.Helper()
	r := stagegate.NewRegistry()
	stagegate.RegisterBaselineGates(r)
	stagegate.RegisterP3AdvancedGates(r, noopLabelFetcher{})
	return r
}

// noopLabelFetcher satisfies stagegate.PRLabelFetcher for the validator
// tests. The validator never calls PRLabels — only the runtime dispatcher
// does — so the stub returns empty results for any input.
type noopLabelFetcher struct{}

func (noopLabelFetcher) PRLabels(_, _ string, _ int) ([]string, error) {
	return nil, nil
}

// TestValidateStagingPlan_ReleaseLabelGate_AllReposHavePattern_OK — happy
// path: every repo touched by a release_label_present-gated stage has a
// non-empty release_label_pattern → planner-time check passes.
func TestValidateStagingPlan_ReleaseLabelGate_AllReposHavePattern_OK(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	store.AddRepo(db, "frontend", "/tmp/fe", "")
	if err := store.SetRepositoryReleaseLabelPattern(db, "api", `^released-prod$`); err != nil {
		t.Fatalf("set pattern api: %v", err)
	}
	if err := store.SetRepositoryReleaseLabelPattern(db, "frontend", `^released-prod$`); err != nil {
		t.Fatalf("set pattern frontend: %v", err)
	}

	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{
				StageNum: 1,
				Intent:   "stage 1 with release_label_present",
				Tasks:    []store.TaskPlan{taskN(1, "api"), taskN(2, "frontend")},
				Gate:     &StagingPlanGate{Type: "release_label_present"},
			},
			{
				StageNum: 2,
				Intent:   "terminal",
				Tasks:    []store.TaskPlan{taskN(3, "api")},
				Gate:     nil,
			},
		},
	}
	// Structural validation must pass first.
	if err := ValidateStagingPlan(plan, p3Registry(t)); err != nil {
		t.Fatalf("structural validation failed: %v", err)
	}
	if err := ValidateReleaseLabelGateForRepos(plan, db); err != nil {
		t.Fatalf("expected pattern check to pass; got %v", err)
	}
}

// TestValidateStagingPlan_ReleaseLabelGate_ReposLackPattern_Rejects — at
// least one repo touched by a release_label_present-gated stage has an
// empty pattern → reject with the operator-actionable message naming the
// stage and the offending repo.
func TestValidateStagingPlan_ReleaseLabelGate_ReposLackPattern_Rejects(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	store.AddRepo(db, "frontend", "/tmp/fe", "")
	// Only api has a pattern; frontend is intentionally left empty.
	if err := store.SetRepositoryReleaseLabelPattern(db, "api", `^released-prod$`); err != nil {
		t.Fatalf("set pattern api: %v", err)
	}

	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{
				StageNum: 1,
				Intent:   "release-label gate touches both repos",
				Tasks:    []store.TaskPlan{taskN(1, "api"), taskN(2, "frontend")},
				Gate:     &StagingPlanGate{Type: "release_label_present"},
			},
			{
				StageNum: 2,
				Intent:   "terminal",
				Tasks:    []store.TaskPlan{taskN(3, "api")},
				Gate:     nil,
			},
		},
	}
	if err := ValidateStagingPlan(plan, p3Registry(t)); err != nil {
		t.Fatalf("structural validation should still pass: %v", err)
	}
	err := ValidateReleaseLabelGateForRepos(plan, db)
	if err == nil {
		t.Fatal("expected rejection because frontend has no pattern; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "stage 1") {
		t.Errorf("error must name stage 1, got %q", msg)
	}
	if !strings.Contains(msg, "frontend") {
		t.Errorf("error must name the offending repo (frontend), got %q", msg)
	}
	if !strings.Contains(msg, "release_label_pattern") {
		t.Errorf("error must mention release_label_pattern, got %q", msg)
	}
	if !strings.Contains(msg, "force config set") {
		t.Errorf("error must include the operator-actionable hint, got %q", msg)
	}
}

// TestValidateStagingPlan_ReleaseLabelGate_NestedInCompound_Rejects —
// the per-repo check walks compound trees: a release_label_present
// nested under all_of must still trigger pattern enforcement.
func TestValidateStagingPlan_ReleaseLabelGate_NestedInCompound_Rejects(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	// api: no pattern set.

	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{
				StageNum: 1,
				Intent:   "compound gate with release_label_present nested",
				Tasks:    []store.TaskPlan{taskN(1, "api")},
				Gate: &StagingPlanGate{
					Type: "all_of",
					Gates: []*StagingPlanGate{
						soakGate(5),
						{Type: "release_label_present"},
					},
				},
			},
			{
				StageNum: 2,
				Intent:   "terminal",
				Tasks:    []store.TaskPlan{taskN(2, "api")},
				Gate:     nil,
			},
		},
	}
	if err := ValidateStagingPlan(plan, p3Registry(t)); err != nil {
		t.Fatalf("structural validation should pass: %v", err)
	}
	err := ValidateReleaseLabelGateForRepos(plan, db)
	if err == nil {
		t.Fatal("expected rejection because api has no pattern; got nil")
	}
	if !strings.Contains(err.Error(), "api") {
		t.Errorf("error must name api, got %q", err.Error())
	}
}

// TestValidateStagingPlan_ReleaseLabelGate_GateNotUsed_NoOp — when no
// stage uses release_label_present (even if some repos lack patterns),
// the per-repo check is a silent no-op.
func TestValidateStagingPlan_ReleaseLabelGate_GateNotUsed_NoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	// no pattern set — but the plan doesn't use release_label_present,
	// so the per-repo validator should accept it.

	plan := StagingPlan{
		StagingMode:     string(store.StagingModeStaged),
		StagingStrategy: store.StagingStrategyStrict,
		Stages: []StagingPlanStage{
			{
				StageNum: 1,
				Intent:   "soak only",
				Tasks:    []store.TaskPlan{taskN(1, "api")},
				Gate:     soakGate(60),
			},
			{
				StageNum: 2,
				Intent:   "terminal",
				Tasks:    []store.TaskPlan{taskN(2, "api")},
				Gate:     nil,
			},
		},
	}
	if err := ValidateReleaseLabelGateForRepos(plan, db); err != nil {
		t.Errorf("plan without release_label_present should pass; got %v", err)
	}
}

// TestValidateStagingPlan_ReleaseLabelGate_SingleMode_NoOp — single-mode
// plans never carry stage gates; the check short-circuits to nil.
func TestValidateStagingPlan_ReleaseLabelGate_SingleMode_NoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	plan := StagingPlan{StagingMode: string(store.StagingModeSingle)}
	if err := ValidateReleaseLabelGateForRepos(plan, db); err != nil {
		t.Errorf("single-mode plan should be a no-op; got %v", err)
	}
}

// TestValidateStagingPlan_ReleaseLabelGate_NilDB_Errors — wiring guard.
func TestValidateStagingPlan_ReleaseLabelGate_NilDB_Errors(t *testing.T) {
	plan := StagingPlan{StagingMode: string(store.StagingModeStaged)}
	if err := ValidateReleaseLabelGateForRepos(plan, nil); err == nil {
		t.Fatal("expected error on nil db; got nil")
	}
}
