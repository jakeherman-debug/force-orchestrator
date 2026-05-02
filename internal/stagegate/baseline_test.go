package stagegate

import (
	"context"
	"testing"
	"time"

	"force-orchestrator/internal/clients/databricks"
	"force-orchestrator/internal/clients/datadog"
)

// stubDDClientForBaseline is a minimal datadog.Client stub so the
// RegisterAllP3Gates tests can exercise the "non-nil ddClient → register"
// branch without spinning up a real Datadog HTTP path. The methods are
// never called by these tests (registration only walks the type name).
type stubDDClientForBaseline struct{}

func (stubDDClientForBaseline) QueryMetric(_ context.Context, _ string, _ time.Duration) (float64, time.Time, error) {
	return 0, time.Time{}, nil
}
func (stubDDClientForBaseline) Health(_ context.Context) error { return nil }

var _ datadog.Client = stubDDClientForBaseline{}

// stubDBXClientForBaseline mirrors stubDDClientForBaseline for databricks.
type stubDBXClientForBaseline struct{}

func (stubDBXClientForBaseline) ExecuteQuery(_ context.Context, _, _ string, _ time.Duration) (float64, error) {
	return 0, nil
}
func (stubDBXClientForBaseline) Health(_ context.Context) error { return nil }

var _ databricks.Client = stubDBXClientForBaseline{}

// stubGHForBaseline implements PRLabelFetcher with no-op label lookup.
type stubGHForBaseline struct{}

func (stubGHForBaseline) PRLabels(_, _ string, _ int) ([]string, error) { return nil, nil }

// TestRegisterAllP3Gates_AllNil_NoCrash — passing nil for every client
// must not panic and must register zero P3 gates. Baseline gates (registered
// before this call) stay intact. This proves an operator with no GitHub /
// Datadog / Databricks credentials still gets a working baseline daemon.
func TestRegisterAllP3Gates_AllNil_NoCrash(t *testing.T) {
	r := NewRegistry()
	RegisterBaselineGates(r)
	// Baseline registration sanity check first — guards against the
	// test passing because the registry is empty, not because nil-skip
	// works.
	if _, ok := r.Lookup("soak_minutes"); !ok {
		t.Fatal("baseline soak_minutes should be registered")
	}

	// Should not panic with all nil clients.
	RegisterAllP3Gates(r, nil, nil, nil)

	// None of the four P3 advanced gates should be registered.
	for _, gateType := range []string{
		"probe_endpoint",
		"release_label_present",
		"datadog_metric_threshold",
		"databricks_query_threshold",
	} {
		if _, ok := r.Lookup(gateType); ok {
			t.Errorf("expected %q to be NOT registered when its client is nil", gateType)
		}
	}

	// Baseline gates must still be present.
	if _, ok := r.Lookup("operator_confirm"); !ok {
		t.Error("baseline operator_confirm registration must survive RegisterAllP3Gates(nil,...)")
	}
}

// TestRegisterAllP3Gates_PartialClients_OnlyConfiguredGatesRegister — each
// non-nil client toggles its corresponding gate(s) on independently. Mirrors
// the production scenario where an operator has Datadog configured but not
// Databricks (or vice versa).
func TestRegisterAllP3Gates_PartialClients_OnlyConfiguredGatesRegister(t *testing.T) {
	t.Run("only_dd_client", func(t *testing.T) {
		r := NewRegistry()
		RegisterAllP3Gates(r, nil, stubDDClientForBaseline{}, nil)
		if _, ok := r.Lookup("datadog_metric_threshold"); !ok {
			t.Error("datadog gate must register when ddClient non-nil")
		}
		if _, ok := r.Lookup("databricks_query_threshold"); ok {
			t.Error("databricks gate must NOT register when dbxClient nil")
		}
		if _, ok := r.Lookup("probe_endpoint"); ok {
			t.Error("probe_endpoint must NOT register when ghClient nil")
		}
	})
	t.Run("only_dbx_client", func(t *testing.T) {
		r := NewRegistry()
		RegisterAllP3Gates(r, nil, nil, stubDBXClientForBaseline{})
		if _, ok := r.Lookup("databricks_query_threshold"); !ok {
			t.Error("databricks gate must register when dbxClient non-nil")
		}
		if _, ok := r.Lookup("datadog_metric_threshold"); ok {
			t.Error("datadog gate must NOT register when ddClient nil")
		}
	})
	t.Run("only_gh_client", func(t *testing.T) {
		r := NewRegistry()
		RegisterAllP3Gates(r, stubGHForBaseline{}, nil, nil)
		if _, ok := r.Lookup("probe_endpoint"); !ok {
			t.Error("probe_endpoint must register when ghClient non-nil")
		}
		if _, ok := r.Lookup("release_label_present"); !ok {
			t.Error("release_label_present must register when ghClient non-nil")
		}
		if _, ok := r.Lookup("datadog_metric_threshold"); ok {
			t.Error("datadog gate must NOT register when ddClient nil")
		}
	})
	t.Run("all_clients_present", func(t *testing.T) {
		r := NewRegistry()
		RegisterBaselineGates(r)
		RegisterAllP3Gates(r, stubGHForBaseline{}, stubDDClientForBaseline{}, stubDBXClientForBaseline{})
		for _, gateType := range []string{
			"soak_minutes", "operator_confirm", "null", "all_of", "any_of",
			"probe_endpoint", "release_label_present",
			"datadog_metric_threshold", "databricks_query_threshold",
		} {
			if _, ok := r.Lookup(gateType); !ok {
				t.Errorf("gate %q should be registered when all clients are present", gateType)
			}
		}
	})
}

// TestRegisterDatabricksGate — mirrors TestRegisterDatadogGate. Wave 3 ζ
// added this helper so RegisterAllP3Gates can compose both threshold gates
// via a uniform shape.
func TestRegisterDatabricksGate(t *testing.T) {
	r := NewRegistry()
	RegisterDatabricksGate(r, stubDBXClientForBaseline{})
	if _, ok := r.Lookup("databricks_query_threshold"); !ok {
		t.Error("expected databricks_query_threshold to be registered")
	}

	r2 := NewRegistry()
	RegisterDatabricksGate(r2, nil)
	if _, ok := r2.Lookup("databricks_query_threshold"); ok {
		t.Error("expected nil-client path to skip registration")
	}
}
