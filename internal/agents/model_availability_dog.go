package agents

// model_availability_dog.go — D3 fix-loop-1 (slice δ).
//
// Periodic health watch over the model identifiers used as treatment
// dimensions. The Phase 3 HoldoutMonitor emits a debug heartbeat but
// defers the actual availability probe; this dog completes that loop
// by:
//
//  1. Enumerating distinct non-empty model_identifier values from
//     TreatmentSpecs (the canonical "models the fleet is currently
//     using as treatment knobs"; if a model is referenced by a spec,
//     a deprecation breaks an in-flight experiment).
//  2. Probing each model — by default a no-op record (the LIVE_HAIKU_
//     DISABLED env-flag pattern from live_haiku.go: tests + CI never
//     burn budget on real Anthropic calls). When the flag is unset
//     and an operator opts in via SystemConfig key
//     "model_availability_live_probe", we issue a 1-token prompt via
//     CallWithTranscript so the dog records a real success/failure.
//  3. Upserting one ModelAvailability row per model_id with
//     last_checked_at + last_success_at populated.
//
// Cadence: 30 minutes. Set in dogs.go:dogCooldowns alongside the
// other recurring health watches.
//
// Why a separate dog rather than wiring this into HoldoutMonitor:
// HoldoutMonitor is a per-tick heartbeat against GlobalHoldouts (a
// GC dimension), this is a per-fleet ledger of model identifiers.
// Conflating them muddies which row-set is authoritative when a
// signal fires, so they live as separate handlers writing separate
// tables.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// modelAvailabilityProbeFn is the function signature the dog uses to
// probe a model. Production wires this to a CallWithTranscript-backed
// minimal-cost probe; tests inject a deterministic stub. Returning
// (true, nil) means "available", (false, err) means "unavailable, here
// is why", and (false, nil) is reserved for the no-probe / dry-run
// shape (LIVE_HAIKU_DISABLED set, no live probe attempted).
type modelAvailabilityProbeFn func(ctx context.Context, modelID string) (probed bool, err error)

// modelAvailabilityProbe is the package-level seam for tests. Default
// is the env-flag-aware no-op; tests overwrite via the helper at
// model_availability_dog_test.go.
var modelAvailabilityProbe modelAvailabilityProbeFn = defaultModelAvailabilityProbe

// modelAvailabilityProbeCaller is the seam the live probe uses to
// reach claude.CallWithTranscript. Tests overwrite this to inject a
// fake adapter so the live-path code can be exercised without burning
// budget. Production wires it to the real wrapper.
var modelAvailabilityProbeCaller = func(ctx context.Context, desc claude.CallDescriptor, systemPrompt, userPrompt, allowed, disallowed, mcpConfig string, maxTurns int) (string, error) {
	return claude.CallWithTranscript(ctx, desc, systemPrompt, userPrompt, allowed, disallowed, mcpConfig, maxTurns)
}

// defaultModelAvailabilityProbe is the production default. It honours
// the LIVE_HAIKU_DISABLED env-flag — when set, the dog records a
// heartbeat row but does NOT issue a real Anthropic call. When unset,
// the probe issues a minimal one-shot Claude call against the given
// model_id and reports success / failure to recordModelAvailability so
// a deprecation surfaces as a [HOLDOUT AT RISK] signal.
//
// The probe is deliberately minimal — a tiny "ping" user prompt with
// max_turns=1 — because the goal is just "did the model_id resolve
// to a live endpoint", not "is the model healthy under load". The
// model_id is propagated as the CallDescriptor.PromptVersion field so
// LLMCallTranscripts rows for the dog are filterable by which model
// they probed; this is the cheapest way to attribute the call without
// adding a new transcript column.
//
// Pattern P13: routes through claude.CallWithTranscript with the
// model-availability-dog capability profile. Pattern P31: the wrapper
// records an LLMCallTranscripts row.
func defaultModelAvailabilityProbe(ctx context.Context, modelID string) (bool, error) {
	if liveHaikuDisabled() {
		return false, nil // no probe — record-only mode
	}
	if os.Getenv("FORCE_MODEL_AVAILABILITY_LIVE_PROBE") == "0" {
		// Explicit per-dog kill switch — operator can suppress live
		// probes without flipping the global LIVE_HAIKU_DISABLED flag.
		return false, nil
	}

	prof, err := loadRendererProfile("model-availability-dog")
	if err != nil {
		// Profile failure is fatal-shape per Pattern P13. Fall back to
		// record-only so a transient profile load issue doesn't strand
		// the dog (the recorded heartbeat row still surfaces the dog is
		// running; the operator sees the failure in logs).
		return false, fmt.Errorf("load profile: %w", err)
	}

	// Minimal one-shot probe. The system prompt asks for a single token;
	// the user prompt is a literal "ping". A live model returns
	// something (anything non-empty); a dead endpoint returns an error
	// from CallWithTranscript.
	out, callErr := modelAvailabilityProbeCaller(ctx,
		claude.CallDescriptor{
			Agent:         "model-availability-dog",
			PromptVersion: modelID, // identifies which model this row probed
		},
		modelAvailabilitySystemPrompt,
		modelAvailabilityUserPrompt,
		prof.allowedTools, prof.disallowedTools, prof.mcpConfig, 1)
	if callErr != nil {
		return false, callErr
	}
	if len(out) == 0 {
		// Empty body without an error is rare but possible — the
		// endpoint accepted the request but returned nothing. Treat as
		// failure so deprecation detection fires.
		return false, fmt.Errorf("probe returned empty body")
	}
	return true, nil
}

// modelAvailabilitySystemPrompt is the system half of the probe call.
// Kept tiny so even a probe failure (rate limit / token budget) is
// cheap. We deliberately do NOT instruct the model to do anything
// useful — the goal is "endpoint resolves, returns SOMETHING."
const modelAvailabilitySystemPrompt = `Reply with exactly one word: "pong".`

// modelAvailabilityUserPrompt is the user half. Same minimal shape.
const modelAvailabilityUserPrompt = `ping`

// dogModelAvailabilityWatch is the entry point registered in dogs.go.
// Returns nil even when individual probes fail — partial success is
// the right shape (one bad model_id shouldn't suppress the others'
// rows). Per-row failures are recorded into the model's
// ModelAvailability row so the operator can see which probe failed.
func dogModelAvailabilityWatch(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	if db == nil {
		return fmt.Errorf("model-availability-watch: db is nil")
	}

	models, err := listConfiguredModels(db)
	if err != nil {
		return fmt.Errorf("model-availability-watch: list models: %w", err)
	}

	if len(models) == 0 {
		logger.Printf("Dog model-availability-watch: no model_identifier rows in TreatmentSpecs — nothing to probe")
		return nil
	}

	probed := 0
	failed := 0
	for _, modelID := range models {
		// Per-probe ctx with a tight timeout so a hung Anthropic
		// endpoint can't exhaust the dog's overall 5-minute budget.
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ok, perr := modelAvailabilityProbe(probeCtx, modelID)
		cancel()
		now := store.NowSQLite()
		if perr != nil {
			failed++
			logger.Printf("Dog model-availability-watch: probe %s failed: %v", modelID, perr)
			if err := recordModelAvailability(db, modelID, now, "", perr); err != nil {
				logger.Printf("Dog model-availability-watch: record failure for %s: %v", modelID, err)
			}
			continue
		}
		// On no-probe (ok=false, perr=nil) we still stamp last_checked_at
		// so the operator can see the dog is running; last_success_at
		// only advances when the probe actually returned successfully.
		successAt := ""
		if ok {
			probed++
			successAt = now
		}
		if err := recordModelAvailability(db, modelID, now, successAt, nil); err != nil {
			logger.Printf("Dog model-availability-watch: record success for %s: %v", modelID, err)
		}
	}

	logger.Printf("Dog model-availability-watch: %d model(s) checked (%d probed, %d failed, mode=%s)",
		len(models), probed, failed, probeMode())
	return nil
}

// probeMode reports the operator-visible mode string for the dog
// summary line. Pure cosmetics; the real signal is the row data.
func probeMode() string {
	if liveHaikuDisabled() {
		return "record-only (LIVE_HAIKU_DISABLED)"
	}
	if os.Getenv("FORCE_MODEL_AVAILABILITY_LIVE_PROBE") == "0" {
		// Operator-visible kill switch in case live probing needs to be
		// disabled without flipping the global LIVE_HAIKU_DISABLED flag
		// (which also pins renderers / judge to deterministic mode).
		return "record-only (FORCE_MODEL_AVAILABILITY_LIVE_PROBE=0)"
	}
	return "live-probe"
}

// listConfiguredModels returns the distinct non-empty model_identifier
// values from TreatmentSpecs. The dog's job is to keep ModelAvailability
// in lockstep with the set of models the fleet is actually using as
// treatment knobs — a deprecation against an unused model is harmless,
// a deprecation against an in-flight treatment is an [HOLDOUT AT RISK]
// signal.
func listConfiguredModels(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`
		SELECT DISTINCT IFNULL(model_identifier, '')
		  FROM TreatmentSpecs
		 WHERE IFNULL(model_identifier, '') != ''
		 ORDER BY model_identifier ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// recordModelAvailability is an UPSERT into ModelAvailability for a
// single model_id. Idempotent: re-running advances last_checked_at and
// (if successAt != "") last_success_at; deprecation_detected_at is
// only set on first failure-after-success, never overwritten on
// repeated failures (the operator wants to see the FIRST time the
// model fell over, not the latest probe).
//
// probeErr != nil triggers deprecation detection: if the row exists
// AND has a non-empty last_success_at, set deprecation_detected_at to
// now (only once — IFNULL on the existing column).
func recordModelAvailability(db *sql.DB, modelID, checkedAt, successAt string, probeErr error) error {
	if modelID == "" {
		return fmt.Errorf("recordModelAvailability: modelID is required")
	}

	// First INSERT-or-IGNORE so the row exists; then UPDATE the
	// columns appropriately. Splitting it lets the deprecation
	// detection branch read the prior row state and decide whether
	// this is the first failure (set deprecation_detected_at) or a
	// repeat (leave it).
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO ModelAvailability (model_id, last_checked_at)
		VALUES (?, ?)
	`, modelID, checkedAt); err != nil {
		return fmt.Errorf("recordModelAvailability: insert-or-ignore: %w", err)
	}

	if probeErr != nil {
		// Deprecation detection — set deprecation_detected_at IFF
		// the row has a non-empty last_success_at AND
		// deprecation_detected_at is currently empty. A model that
		// has never succeeded yet (fresh entry) doesn't get flagged
		// as "deprecated" on first failure.
		if _, err := db.Exec(`
			UPDATE ModelAvailability
			   SET last_checked_at = ?,
			       deprecation_detected_at = CASE
			           WHEN IFNULL(last_success_at, '') != ''
			            AND IFNULL(deprecation_detected_at, '') = ''
			           THEN ?
			           ELSE deprecation_detected_at
			       END
			 WHERE model_id = ?
		`, checkedAt, checkedAt, modelID); err != nil {
			return fmt.Errorf("recordModelAvailability: update on failure: %w", err)
		}
		return nil
	}

	// Success path. Always advance last_checked_at; advance
	// last_success_at only when the probe actually succeeded
	// (successAt != "").
	if successAt != "" {
		if _, err := db.Exec(`
			UPDATE ModelAvailability
			   SET last_checked_at = ?,
			       last_success_at = ?
			 WHERE model_id = ?
		`, checkedAt, successAt, modelID); err != nil {
			return fmt.Errorf("recordModelAvailability: update on success: %w", err)
		}
		return nil
	}

	// Record-only path — heartbeat the last_checked_at without
	// claiming success.
	if _, err := db.Exec(`
		UPDATE ModelAvailability
		   SET last_checked_at = ?
		 WHERE model_id = ?
	`, checkedAt, modelID); err != nil {
		return fmt.Errorf("recordModelAvailability: update on heartbeat: %w", err)
	}
	return nil
}
