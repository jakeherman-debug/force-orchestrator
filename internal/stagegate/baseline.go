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

// RegisterP3AdvancedGates wires the 2 D5.5 P3 γ advanced leaf gates into
// the registry: probe_endpoint and release_label_present. Datadog +
// Databricks threshold gates ship in P3 W2 (a separate slice).
//
// release_label_present requires a PRLabelFetcher (production: *gh.Client)
// because it needs to poll PR labels via the GitHub API.
//
// Wave 3 ζ wires this into the daemon startup path; for P3 γ the helper
// exists so the validator + dispatcher tests can register the new types
// against the same registry that runs in production.
//
// Panics on duplicate registration. Callers MUST register the baseline
// gates FIRST (or skip baseline if their test only exercises P3 types) —
// re-registering existing types panics by Registry.Register's contract.
func RegisterP3AdvancedGates(r *Registry, ghClient PRLabelFetcher) {
	r.Register(NewProbeEndpoint())
	r.Register(NewReleaseLabelPresent(ghClient))
}
