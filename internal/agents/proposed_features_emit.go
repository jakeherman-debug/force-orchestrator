// D3 fix-loop-1 β2 — Investigator → ProposedFeatures emission glue.
//
// The Investigator's prompt grammar (investigator.go) lets the LLM
// emit zero or more `[PROPOSED_FEATURE] {...} [/PROPOSED_FEATURE]`
// blocks at the end of its report. This file:
//
//   - parses those blocks out of the raw LLM output
//   - validates the JSON shape
//   - routes each one through store.EmitProposedFeature (which handles
//     fingerprint, suppression check, and dedup-via-ON-CONFLICT)
//   - logs an audit row per emit so the proposer→pipeline lineage stays
//     auditable
//
// P22 (fingerprint determinism): satisfied because the fingerprint is
// computed inside store.EmitProposedFeature from canonical input fields
// — the parser here just unmarshals JSON; no time/rand sneak in.
//
// P23 (proposer write discipline): satisfied because every write path
// goes through store.EmitProposedFeature; no direct INSERT here.
package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

// proposedFeatureBlockRe matches a single
// [PROPOSED_FEATURE] {...} [/PROPOSED_FEATURE] envelope. The regex is
// non-greedy on the JSON body so multiple blocks in the same output
// don't merge.
var proposedFeatureBlockRe = regexp.MustCompile(`(?s)\[PROPOSED_FEATURE\]\s*(\{.*?\})\s*\[/PROPOSED_FEATURE\]`)

// llmEmittedFeature is the JSON shape the LLM hand-writes inside the
// [PROPOSED_FEATURE] block. Mirrors store.ProposedFeaturePayload but
// with a couple of LLM-friendly aliases (the LLM sees `title` interchangeably
// with `observation_summary`).
type llmEmittedFeature struct {
	ObservationSummary  string   `json:"observation_summary"`
	Title               string   `json:"title"`
	Category            string   `json:"category"`
	Topic               string   `json:"topic"`
	CodePaths           []string `json:"code_paths"`
	ATRefs              []string `json:"at_refs"`
	FleetRuleRefs       []string `json:"fleet_rule_refs"`
	ValueScore          string   `json:"value_score"`
	ComplexityScore     string   `json:"complexity_score"`
	ValueRationale      string   `json:"value_rationale"`
	ComplexityRationale string   `json:"complexity_rationale"`
}

// ParseProposedFeatureBlocks extracts every well-formed
// [PROPOSED_FEATURE] block from `output` and returns the parsed shapes
// plus a list of parse errors (one per block that failed to unmarshal).
// A malformed block is skipped — the rest of the output is still
// usable, and the caller logs the parse errors to AuditLog.
func ParseProposedFeatureBlocks(output string) ([]llmEmittedFeature, []error) {
	matches := proposedFeatureBlockRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	var features []llmEmittedFeature
	var parseErrs []error
	for _, m := range matches {
		body := strings.TrimSpace(m[1])
		var f llmEmittedFeature
		if err := json.Unmarshal([]byte(body), &f); err != nil {
			parseErrs = append(parseErrs, fmt.Errorf("malformed PROPOSED_FEATURE block: %w (body: %.200s)", err, body))
			continue
		}
		// Title is the LLM-friendly alias; coalesce to observation_summary.
		if f.ObservationSummary == "" && f.Title != "" {
			f.ObservationSummary = f.Title
		}
		features = append(features, f)
	}
	return features, parseErrs
}

// EmitInvestigatorProposedFeatures parses the investigator's report
// for [PROPOSED_FEATURE] blocks and routes each one through
// store.EmitProposedFeature. Returns the count of features that landed
// (inserted OR merged) and the count suppressed at ingress, plus any
// errors encountered. Errors are NOT propagated as fatal — the
// investigation report still ships even if proposed-feature emission
// hits a transient DB error.
//
// `taskID` is stamped into the source_observations so the pipeline can
// trace each emit back to the originating investigation.
func EmitInvestigatorProposedFeatures(db *sql.DB, taskID int, agentName string, output string, logger *log.Logger) (inserted, merged, suppressed int) {
	features, parseErrs := ParseProposedFeatureBlocks(output)
	for _, perr := range parseErrs {
		logger.Printf("Investigator #%d: %v", taskID, perr)
		store.LogAudit(db, agentName, "proposed-feature-parse-error", taskID, perr.Error())
	}
	for _, f := range features {
		payload := store.ProposedFeaturePayload{
			ObservationSummary: f.ObservationSummary,
			Category:           f.Category,
			Source:             "investigator",
			SourceObservations: []store.SourceObservation{
				{Kind: "task", Ref: fmt.Sprintf("%d", taskID), Note: "investigation report"},
			},
			CodePaths:           f.CodePaths,
			ATRefs:              f.ATRefs,
			FleetRuleRefs:       f.FleetRuleRefs,
			Topic:               f.Topic,
			ValueScore:          f.ValueScore,
			ComplexityScore:     f.ComplexityScore,
			ValueRationale:      f.ValueRationale,
			ComplexityRationale: f.ComplexityRationale,
			ScoredBy:            "investigator-v1",
		}
		res, err := store.EmitProposedFeature(db, payload)
		if err != nil {
			logger.Printf("Investigator #%d: EmitProposedFeature failed (%v) for %q — feature dropped",
				taskID, err, f.ObservationSummary)
			store.LogAudit(db, agentName, "proposed-feature-emit-error", taskID, err.Error())
			continue
		}
		switch {
		case res.Suppressed:
			suppressed++
			store.LogAudit(db, agentName, "proposed-feature-suppressed", taskID,
				fmt.Sprintf("suppressed by operator rule: %q", f.ObservationSummary))
		case res.Inserted:
			inserted++
			store.LogAudit(db, agentName, "proposed-feature-emit", taskID,
				fmt.Sprintf("inserted feature %d: %q", res.FeatureID, f.ObservationSummary))
		default:
			merged++
			store.LogAudit(db, agentName, "proposed-feature-merge", taskID,
				fmt.Sprintf("merged into feature %d: %q", res.FeatureID, f.ObservationSummary))
		}
	}
	return inserted, merged, suppressed
}
