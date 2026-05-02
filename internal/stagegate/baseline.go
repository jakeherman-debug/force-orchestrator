package stagegate

// RegisterBaselineGates wires the 5 D5.5 P1 baseline gates into the
// passed-in registry: soak_minutes, operator_confirm, null, and the
// compounds all_of + any_of. Phase 3 will add 4 advanced leaves
// (probe_endpoint, release_label_present, datadog_metric_threshold,
// databricks_query_threshold) via a sibling registration helper.
//
// P1 does NOT call this from the daemon startup path — wiring is P2's
// job once the dispatcher knows where to invoke evaluation. P1 ships
// the helper so the dog tests can build their own registry by calling
// RegisterBaselineGates(stagegate.NewRegistry()).
//
// Panics on duplicate registration (Registry.Register's contract).
// Tests that build a registry, register baseline, then register a
// stub gate must use a fresh registry — registering twice will panic
// and fail the test.
func RegisterBaselineGates(r *Registry) {
	r.Register(SoakMinutes{})
	r.Register(OperatorConfirm{})
	r.Register(NullGate{})
	r.Register(AllOf{})
	r.Register(AnyOf{})
}
