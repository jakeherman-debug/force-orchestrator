package agents

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"testing"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// TestModelAvailabilityDog_NoTreatmentSpecs_NoOp confirms the dog
// is a clean no-op when TreatmentSpecs has nothing to probe.
// Importantly: returns nil (not an error) so the inquisitor's per-dog
// error mail doesn't fire.
func TestModelAvailabilityDog_NoTreatmentSpecs_NoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(os.Stderr, "[test] ", 0)
	if err := dogModelAvailabilityWatch(context.Background(), db, logger); err != nil {
		t.Fatalf("dog with empty TreatmentSpecs: got err %v; want nil", err)
	}

	// No ModelAvailability rows should have been written.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM ModelAvailability`).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 ModelAvailability rows; got %d", n)
	}
}

// TestModelAvailabilityDog_RecordOnlyMode is the default-production
// path: the dog runs, lists distinct model_identifiers from
// TreatmentSpecs, records last_checked_at on each, but does NOT
// advance last_success_at (the default probe is record-only).
func TestModelAvailabilityDog_RecordOnlyMode(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1") // pin record-only mode
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed two TreatmentSpecs with distinct model_identifiers.
	mustExecMA(t, db, `INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('h1', 'claude-opus-4-5')`)
	mustExecMA(t, db, `INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('h2', 'claude-haiku-4-5')`)
	// And one with empty model_identifier — should be skipped.
	mustExecMA(t, db, `INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('h3', '')`)

	logger := log.New(os.Stderr, "[test] ", 0)
	if err := dogModelAvailabilityWatch(context.Background(), db, logger); err != nil {
		t.Fatalf("dog: %v", err)
	}

	// Two rows in ModelAvailability — both with last_checked_at set,
	// neither with last_success_at (no live probe).
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM ModelAvailability`).Scan(&n)
	if n != 2 {
		t.Errorf("expected 2 ModelAvailability rows; got %d", n)
	}
	var lastChecked, lastSuccess string
	db.QueryRow(`SELECT IFNULL(last_checked_at,''), IFNULL(last_success_at,'') FROM ModelAvailability WHERE model_id='claude-opus-4-5'`).
		Scan(&lastChecked, &lastSuccess)
	if lastChecked == "" {
		t.Errorf("opus row: last_checked_at should be populated")
	}
	if lastSuccess != "" {
		t.Errorf("opus row: last_success_at should be empty in record-only mode; got %q", lastSuccess)
	}
}

// TestModelAvailabilityDog_LiveProbeSuccess pins the probe seam to a
// stub that returns "available", and confirms last_success_at gets
// stamped.
func TestModelAvailabilityDog_LiveProbeSuccess(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	mustExecMA(t, db, `INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('h-success', 'claude-test-success')`)

	prior := modelAvailabilityProbe
	defer func() { modelAvailabilityProbe = prior }()
	modelAvailabilityProbe = func(_ context.Context, modelID string) (bool, error) {
		return true, nil // simulated probe success
	}

	logger := log.New(os.Stderr, "[test] ", 0)
	if err := dogModelAvailabilityWatch(context.Background(), db, logger); err != nil {
		t.Fatalf("dog: %v", err)
	}

	var checked, success string
	db.QueryRow(`SELECT IFNULL(last_checked_at,''), IFNULL(last_success_at,'') FROM ModelAvailability WHERE model_id=?`,
		"claude-test-success").Scan(&checked, &success)
	if checked == "" {
		t.Errorf("last_checked_at should be set after success probe")
	}
	if success == "" {
		t.Errorf("last_success_at should be set after success probe")
	}
}

// TestModelAvailabilityDog_LiveProbeFailureSetsDeprecation pins the
// probe to "fail" but the row already had a prior success — the dog
// should set deprecation_detected_at to surface the regression.
func TestModelAvailabilityDog_LiveProbeFailureSetsDeprecation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	mustExecMA(t, db, `INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('h-deprecate', 'claude-test-deprecating')`)
	// Pre-seed a row with a prior success.
	mustExecMA(t, db, `INSERT INTO ModelAvailability (model_id, last_checked_at, last_success_at) VALUES ('claude-test-deprecating', '2026-04-01 00:00:00', '2026-04-01 00:00:00')`)

	prior := modelAvailabilityProbe
	defer func() { modelAvailabilityProbe = prior }()
	modelAvailabilityProbe = func(_ context.Context, _ string) (bool, error) {
		return false, errors.New("simulated 404 model not found")
	}

	logger := log.New(os.Stderr, "[test] ", 0)
	if err := dogModelAvailabilityWatch(context.Background(), db, logger); err != nil {
		t.Fatalf("dog: %v", err)
	}

	var deprecated string
	db.QueryRow(`SELECT IFNULL(deprecation_detected_at,'') FROM ModelAvailability WHERE model_id=?`,
		"claude-test-deprecating").Scan(&deprecated)
	if deprecated == "" {
		t.Errorf("deprecation_detected_at should be set after first failure with prior success")
	}
}

// TestModelAvailabilityDog_FailureWithoutPriorSuccessNoDeprecation
// confirms a fresh model_id that has never succeeded yet doesn't get
// flagged as "deprecated" on its first failure — that would be a
// false positive (we just don't know the endpoint yet).
func TestModelAvailabilityDog_FailureWithoutPriorSuccessNoDeprecation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	mustExecMA(t, db, `INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('h-fresh', 'claude-test-never-worked')`)

	prior := modelAvailabilityProbe
	defer func() { modelAvailabilityProbe = prior }()
	modelAvailabilityProbe = func(_ context.Context, _ string) (bool, error) {
		return false, errors.New("network blip")
	}

	logger := log.New(os.Stderr, "[test] ", 0)
	if err := dogModelAvailabilityWatch(context.Background(), db, logger); err != nil {
		t.Fatalf("dog: %v", err)
	}

	var deprecated, success string
	db.QueryRow(`SELECT IFNULL(deprecation_detected_at,''), IFNULL(last_success_at,'') FROM ModelAvailability WHERE model_id=?`,
		"claude-test-never-worked").Scan(&deprecated, &success)
	if success != "" {
		t.Errorf("last_success_at should be empty for never-worked model")
	}
	if deprecated != "" {
		t.Errorf("deprecation_detected_at should NOT be set for first-failure-no-prior-success; got %q", deprecated)
	}
}

// TestModelAvailabilityDog_Idempotent confirms running the dog twice
// is safe: it advances last_checked_at on the second call but leaves
// the prior deprecation_detected_at alone (preserving the FIRST
// failure timestamp).
func TestModelAvailabilityDog_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	mustExecMA(t, db, `INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('h-idem', 'claude-idem')`)
	mustExecMA(t, db, `INSERT INTO ModelAvailability (model_id, last_checked_at, last_success_at) VALUES ('claude-idem', '2026-04-01 00:00:00', '2026-04-01 00:00:00')`)

	prior := modelAvailabilityProbe
	defer func() { modelAvailabilityProbe = prior }()
	modelAvailabilityProbe = func(_ context.Context, _ string) (bool, error) {
		return false, errors.New("simulated outage")
	}

	logger := log.New(os.Stderr, "[test] ", 0)
	if err := dogModelAvailabilityWatch(context.Background(), db, logger); err != nil {
		t.Fatalf("first run: %v", err)
	}
	var firstDep string
	db.QueryRow(`SELECT IFNULL(deprecation_detected_at,'') FROM ModelAvailability WHERE model_id=?`, "claude-idem").Scan(&firstDep)
	if firstDep == "" {
		t.Fatalf("first run should set deprecation_detected_at")
	}

	// Run again — deprecation_detected_at must NOT be overwritten.
	if err := dogModelAvailabilityWatch(context.Background(), db, logger); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var secondDep string
	db.QueryRow(`SELECT IFNULL(deprecation_detected_at,'') FROM ModelAvailability WHERE model_id=?`, "claude-idem").Scan(&secondDep)
	if secondDep != firstDep {
		t.Errorf("deprecation_detected_at must be sticky — first failure timestamp; got first=%q second=%q", firstDep, secondDep)
	}
}

// TestModelAvailabilityDog_RegisteredInDogOrder is the regression
// against future drift between dogCooldowns / dogOrder / runDog
// switch. If any of the three forgets the model-availability-watch
// entry, this test catches it.
func TestModelAvailabilityDog_RegisteredInDogOrder(t *testing.T) {
	if _, ok := dogCooldowns["model-availability-watch"]; !ok {
		t.Errorf("model-availability-watch missing from dogCooldowns")
	}
	found := false
	for _, n := range dogOrder {
		if n == "model-availability-watch" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("model-availability-watch missing from dogOrder")
	}
	// Smoke-test the dispatch via runDog.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "[test] ", 0)
	if err := runDog(context.Background(), db, "model-availability-watch", nil, nil, nil, nil, logger); err != nil {
		t.Errorf("runDog model-availability-watch: %v", err)
	}
}

// mustExecMA is a tiny helper for this file's test setup — keeps row
// inserts terse without pulling in a shared helpers file.
func mustExecMA(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestDefaultModelAvailabilityProbe_LivePathWiredViaFakeAdapter exercises
// the production live-path code (LIVE_HAIKU_DISABLED unset) by injecting
// a fake adapter at the modelAvailabilityProbeCaller seam. The fake
// records that it was invoked with the configured model_id and returns a
// non-empty body — the probe should report success.
//
// This is the regression that catches "default probe is record-only"
// silently coming back in a future refactor: the test fails if the fake
// adapter is never called.
func TestDefaultModelAvailabilityProbe_LivePathWiredViaFakeAdapter(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	t.Setenv("FORCE_MODEL_AVAILABILITY_LIVE_PROBE", "")

	priorCaller := modelAvailabilityProbeCaller
	defer func() { modelAvailabilityProbeCaller = priorCaller }()

	var gotModelID string
	var calls int
	modelAvailabilityProbeCaller = func(ctx context.Context, desc claude.CallDescriptor, systemPrompt, userPrompt, allowed, disallowed, mcpConfig string, maxTurns int) (string, error) {
		calls++
		gotModelID = desc.PromptVersion
		return "pong", nil
	}

	ok, err := defaultModelAvailabilityProbe(context.Background(), "claude-test-live")
	if err != nil {
		t.Fatalf("live probe with fake adapter: unexpected error %v", err)
	}
	if !ok {
		t.Errorf("live probe: expected ok=true on non-empty body")
	}
	if calls != 1 {
		t.Errorf("expected fake adapter to be called exactly once; got %d", calls)
	}
	if gotModelID != "claude-test-live" {
		t.Errorf("expected fake adapter to receive model_id in PromptVersion; got %q", gotModelID)
	}
}

// TestDefaultModelAvailabilityProbe_LivePathFailureSurfaces — when the
// adapter returns an error (model endpoint dead), the probe propagates
// it so recordModelAvailability flips the deprecation_detected_at path.
func TestDefaultModelAvailabilityProbe_LivePathFailureSurfaces(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	t.Setenv("FORCE_MODEL_AVAILABILITY_LIVE_PROBE", "")

	priorCaller := modelAvailabilityProbeCaller
	defer func() { modelAvailabilityProbeCaller = priorCaller }()

	modelAvailabilityProbeCaller = func(ctx context.Context, desc claude.CallDescriptor, systemPrompt, userPrompt, allowed, disallowed, mcpConfig string, maxTurns int) (string, error) {
		return "", errors.New("simulated 404 from anthropic")
	}

	ok, err := defaultModelAvailabilityProbe(context.Background(), "claude-test-dead")
	if err == nil {
		t.Fatalf("live probe: expected error from dead endpoint, got nil")
	}
	if ok {
		t.Errorf("live probe: expected ok=false on error path")
	}
}

// TestDefaultModelAvailabilityProbe_EmptyBodyTreatedAsFailure —
// belt-and-suspenders: an adapter that returns ("", nil) is treated as
// a failure. A live model that returned nothing is unhealthy.
func TestDefaultModelAvailabilityProbe_EmptyBodyTreatedAsFailure(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	t.Setenv("FORCE_MODEL_AVAILABILITY_LIVE_PROBE", "")

	priorCaller := modelAvailabilityProbeCaller
	defer func() { modelAvailabilityProbeCaller = priorCaller }()

	modelAvailabilityProbeCaller = func(ctx context.Context, desc claude.CallDescriptor, systemPrompt, userPrompt, allowed, disallowed, mcpConfig string, maxTurns int) (string, error) {
		return "", nil
	}

	ok, err := defaultModelAvailabilityProbe(context.Background(), "claude-test-empty")
	if err == nil {
		t.Fatalf("live probe with empty body: expected error, got nil")
	}
	if ok {
		t.Errorf("live probe: expected ok=false for empty body")
	}
}

// TestDefaultModelAvailabilityProbe_LiveHaikuDisabledIsRecordOnly —
// regression for the test-mode pin. Setting LIVE_HAIKU_DISABLED MUST
// short-circuit BEFORE the live caller would be invoked, even if the
// adapter would have returned success.
func TestDefaultModelAvailabilityProbe_LiveHaikuDisabledIsRecordOnly(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")

	priorCaller := modelAvailabilityProbeCaller
	defer func() { modelAvailabilityProbeCaller = priorCaller }()

	calls := 0
	modelAvailabilityProbeCaller = func(ctx context.Context, desc claude.CallDescriptor, systemPrompt, userPrompt, allowed, disallowed, mcpConfig string, maxTurns int) (string, error) {
		calls++
		return "pong", nil
	}

	ok, err := defaultModelAvailabilityProbe(context.Background(), "claude-test-disabled")
	if err != nil {
		t.Fatalf("record-only mode: unexpected error %v", err)
	}
	if ok {
		t.Errorf("record-only mode: expected ok=false (no probe issued)")
	}
	if calls != 0 {
		t.Errorf("record-only mode: live caller should NOT have been invoked; got %d calls", calls)
	}
}

// TestDefaultModelAvailabilityProbe_KillSwitchSuppressesLiveCall —
// FORCE_MODEL_AVAILABILITY_LIVE_PROBE=0 is the per-dog kill switch.
// Lets the operator suppress live probes without flipping
// LIVE_HAIKU_DISABLED (which would also pin renderers / judge).
func TestDefaultModelAvailabilityProbe_KillSwitchSuppressesLiveCall(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	t.Setenv("FORCE_MODEL_AVAILABILITY_LIVE_PROBE", "0")

	priorCaller := modelAvailabilityProbeCaller
	defer func() { modelAvailabilityProbeCaller = priorCaller }()

	calls := 0
	modelAvailabilityProbeCaller = func(ctx context.Context, desc claude.CallDescriptor, systemPrompt, userPrompt, allowed, disallowed, mcpConfig string, maxTurns int) (string, error) {
		calls++
		return "pong", nil
	}

	ok, err := defaultModelAvailabilityProbe(context.Background(), "claude-test-kill-switch")
	if err != nil {
		t.Fatalf("kill switch: unexpected error %v", err)
	}
	if ok {
		t.Errorf("kill switch: expected ok=false (no probe issued)")
	}
	if calls != 0 {
		t.Errorf("kill switch: live caller should NOT have been invoked; got %d calls", calls)
	}
}
