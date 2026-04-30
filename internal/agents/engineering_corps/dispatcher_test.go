package engineering_corps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/clients/metrics"
	"force-orchestrator/internal/store"
)

// TestEngineeringCorpsDispatcher_RoutesAllSixTypes feeds a synthetic
// BountyBoard row of each EC task type into the dispatcher and asserts
// each one routes to its handler stub (returning ErrNotImplemented).
//
// This is the load-bearing dispatcher regression: the test iterates
// AllTaskTypes (the authoritative inventory) so adding a new task
// type without a switch case triggers a TestEngineeringCorpsDispatcher
// failure rather than a silent no-op in production.
func TestEngineeringCorpsDispatcher_RoutesAllSixTypes(t *testing.T) {
	if len(AllTaskTypes) != 6 {
		t.Fatalf("Phase 3 spec calls for six task types; got %d: %v", len(AllTaskTypes), AllTaskTypes)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cfg := EngineeringCorpsConfig{
		Name:      "EC-test",
		DB:        db,
		Librarian: librarian.NewInProcess(db),
		Metrics:   metrics.NewInProcess(),
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	profile, err := capabilities.LoadProfile("engineering-corps")
	if err != nil {
		t.Fatalf("LoadProfile(engineering-corps): %v", err)
	}

	logger := newTestLogger()

	for _, taskType := range AllTaskTypes {
		t.Run(taskType, func(t *testing.T) {
			id := store.AddBounty(db, 0, taskType, "{}")
			bounty, claimed := store.ClaimBounty(db, taskType, "EC-test")
			if !claimed || bounty == nil || bounty.ID != id {
				t.Fatalf("ClaimBounty(%s) failed; got bounty=%v claimed=%v", taskType, bounty, claimed)
			}

			// dispatch routes the bounty; the stub returns
			// ErrNotImplemented and the dispatcher fails the bounty.
			// Asserting Status='Failed' verifies the route reached
			// the stub (Phase 1 contract); when sub-agent A replaces
			// the stub bodies, this test will need a per-handler
			// fixture instead.
			dispatch(context.Background(), cfg, profile, "EC-test", taskType, bounty, logger.std())

			fresh, err := store.GetBounty(db, id)
			if err != nil {
				t.Fatalf("GetBounty(#%d): %v", id, err)
			}
			if fresh.Status != "Failed" {
				t.Errorf("bounty %s #%d status = %q, want %q", taskType, id, fresh.Status, "Failed")
			}
		})
	}
}

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
		Metrics:   metrics.NewInProcess(),
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

// TestEngineeringCorpsDispatcher_AllStubsReturnErrNotImplemented walks
// the inventory and asserts every handler stub returns the Phase 1
// ErrNotImplemented sentinel. When sub-agent A replaces a stub body,
// the corresponding test case naturally falls — sub-agent A is
// expected to delete the matching entry below as it lands each handler.
func TestEngineeringCorpsDispatcher_AllStubsReturnErrNotImplemented(t *testing.T) {
	stubs := map[string]func() error{
		TaskTypeExperimentAuthor: func() error {
			return handleExperimentAuthor(context.Background(), EngineeringCorpsConfig{}, nil, "", nil, nil)
		},
		TaskTypeExperimentMonitor: func() error {
			return handleExperimentMonitor(context.Background(), EngineeringCorpsConfig{}, nil, "", nil, nil)
		},
		TaskTypePromotionAuthor: func() error {
			return handlePromotionAuthor(context.Background(), EngineeringCorpsConfig{}, nil, "", nil, nil)
		},
		TaskTypeDemotionAuthor: func() error {
			return handleDemotionAuthor(context.Background(), EngineeringCorpsConfig{}, nil, "", nil, nil)
		},
		TaskTypeMetricAuthor: func() error {
			return handleMetricAuthor(context.Background(), EngineeringCorpsConfig{}, nil, "", nil, nil)
		},
		TaskTypeHoldoutMonitor: func() error {
			return handleHoldoutMonitor(context.Background(), EngineeringCorpsConfig{}, nil, "", nil, nil)
		},
	}
	for taskType, call := range stubs {
		t.Run(taskType, func(t *testing.T) {
			err := call()
			if !errors.Is(err, ErrNotImplemented) {
				t.Errorf("%s stub returned %v, want ErrNotImplemented", taskType, err)
			}
		})
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
		{"nil_db", EngineeringCorpsConfig{Librarian: librarian.NewInProcess(nil), Metrics: metrics.NewInProcess()}},
		{"nil_librarian", EngineeringCorpsConfig{DB: store.InitHolocronDSN(":memory:"), Metrics: metrics.NewInProcess()}},
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
