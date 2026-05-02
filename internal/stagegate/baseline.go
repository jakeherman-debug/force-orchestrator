package stagegate

import (
	"log"

	"force-orchestrator/internal/clients/databricks"
	"force-orchestrator/internal/clients/datadog"
)

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

// RegisterAllP3Gates registers the four D5.5 P3 advanced leaf gates against
// the registry, given non-nil clients. Pass nil for any client to skip that
// gate's registration (logs an info-level line so the operator can see in
// daemon stdout which integrations are absent).
//
// This is the single composition point Wave 3 ζ wires into daemon startup
// (cmd/force/fleet_cmds.go). Tests that need the full set call this with
// stub clients; tests that only need a subset call the per-gate helpers
// directly.
//
// Wiring split:
//   - probe_endpoint     — registered when ghClient is non-nil. The gate
//                          itself doesn't call PRLabels, but the package
//                          ships RegisterP3AdvancedGates as the canonical
//                          probe + release_label pair; we follow that
//                          pattern so a daemon with no GitHub client also
//                          skips probe_endpoint (rare but possible).
//   - release_label_present — same gate as above, both register together.
//   - datadog_metric_threshold — registered when ddClient is non-nil.
//   - databricks_query_threshold — registered when dbxClient is non-nil.
//
// The skip-with-log shape (rather than panic-on-nil) is deliberate: an
// operator who hasn't configured Datadog or Databricks credentials should
// still get a working daemon with the other gates available, and a clear
// stdout line explaining what's missing.
//
// Panics on duplicate registration (Registry.Register's contract); callers
// MUST pass a fresh registry (typically baseline-registered + then this).
func RegisterAllP3Gates(r *Registry, ghClient PRLabelFetcher, ddClient datadog.Client, dbxClient databricks.Client) {
	if ghClient != nil {
		RegisterP3AdvancedGates(r, ghClient)
	} else {
		log.Printf("stagegate: probe_endpoint + release_label_present gates skipped (nil GitHub client)")
	}
	if ddClient != nil {
		RegisterDatadogGate(r, ddClient)
	} else {
		log.Printf("stagegate: datadog_metric_threshold gate skipped (nil datadog client)")
	}
	if dbxClient != nil {
		RegisterDatabricksGate(r, dbxClient)
	} else {
		log.Printf("stagegate: databricks_query_threshold gate skipped (nil databricks client)")
	}
}
