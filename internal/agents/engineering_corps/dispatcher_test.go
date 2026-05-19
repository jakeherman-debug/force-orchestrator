package engineering_corps

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/clients/metrics"
	"force-orchestrator/internal/store"
)

// TestEngineeringCorpsDispatcher_UnknownTypeFailsCleanly feeds a bogus
// task type through the dispatcher's default branch. The expected
// shape is: no panic, no silent no-op, bounty fails with a message
// naming the unknown type.
//
// This is the captain-pattern P12 fail-closed-on-unknown-decision
// regression for EC. A new const added to AllTaskTypes WITHOUT a
// switch case would land here too — the test fires loudly instead of
// the dispatcher silently failing the row with no diagnostic.
func TestEngineeringCorpsDispatcher_UnknownTypeFailsCleanly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := newTestLogger()

	id := store.AddBounty(db, 0, "ECPhantomTaskTypeForTest", "{}")
	bounty, claimed := store.ClaimBounty(db, "ECPhantomTaskTypeForTest", "EC-test")
	if !claimed || bounty == nil {
		t.Fatalf("seeded bounty did not claim")
	}

	// Calling dispatch directly with the unknown type avoids needing
	// to seed the inventory list with a phantom value.
	dispatch(context.Background(), EngineeringCorpsConfig{
		Name:      "EC-test",
		DB:        db,
		Librarian: librarian.NewInProcess(db),
		Metrics:   metrics.NewInProcess(db),
	}, nil, "EC-test", "ECPhantomTaskTypeForTest", bounty, logger.std())

	fresh, err := store.GetBounty(db, id)
	if err != nil {
		t.Fatalf("GetBounty(#%d): %v", id, err)
	}
	if fresh.Status != "Failed" {
		t.Errorf("unknown-type bounty status = %q, want %q", fresh.Status, "Failed")
	}
	if !logger.containsAny([]string{"unknown task type", "ECPhantomTaskTypeForTest"}) {
		t.Errorf("logger output should mention the unknown type and the phantom name; got:\n%s", logger.dump())
	}
}

// TestEngineeringCorpsConfig_FailsClosedOnMissingDeps verifies that a
// SpawnEngineeringCorps call with a zero-value config short-circuits
// (validate() returns an error and the loop never enters) instead of
// crashing on a nil DB / Librarian dereference.
func TestEngineeringCorpsConfig_FailsClosedOnMissingDeps(t *testing.T) {
	cases := []struct {
		name string
		cfg  EngineeringCorpsConfig
	}{
		{"nil_db", EngineeringCorpsConfig{Librarian: librarian.NewInProcess(nil), Metrics: metrics.NewInProcess(nil)}},
		{"nil_librarian", EngineeringCorpsConfig{DB: store.InitHolocronDSN(":memory:"), Metrics: metrics.NewInProcess(nil)}},
		{"nil_metrics", EngineeringCorpsConfig{DB: store.InitHolocronDSN(":memory:"), Librarian: librarian.NewInProcess(nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if err == nil {
				t.Fatalf("zero-value EC config (%s) must fail validation", tc.name)
			}
			if !strings.Contains(err.Error(), "engineering_corps") {
				t.Errorf("validation error should be namespaced; got %v", err)
			}
		})
	}
}

// TestAllTaskTypesIsSix asserts the canonical inventory hasn't drifted.
// If a sub-agent adds a new task type, both the inventory and the
// dispatcher switch must be extended together; this guard keeps the
// invariant visible.
func TestAllTaskTypesIsSix(t *testing.T) {
	if len(AllTaskTypes) != 6 {
		t.Fatalf("Phase 3 spec calls for six task types; got %d: %v", len(AllTaskTypes), AllTaskTypes)
	}
}
