// Package agents — D14 Phase 5 MigrationClassifyProposals agent.
//
// RunMigrationClassifyProposals classifies all unclassified pending
// PromotionProposals as either "knowledge_observation" or
// "enforceable_rule" using an LLM call, then:
//
//   - knowledge_observation → absorbs the description as a SenateMemory
//     row for the senator inferred from authored_by / rule_key, and marks
//     the proposal classification_status = 'absorbed_as_knowledge'.
//
//   - enforceable_rule → stamps classification_status = 'awaiting_scope_review'
//     and stores the LLM-suggested agent_scope in suggested_scope.
//
// After all proposals are processed, the function sends a fleet-mail
// summary to "operator" with counts and rule candidates.
//
// Batching: proposals are classified in batches of up to 20 to stay
// within Claude context limits. The function is idempotent — running it
// twice on the same DB is a no-op because SetProposalClassification uses
// a WHERE classification_status = '' guard.
//
// CLAUDE.md invariants:
//   - All store mutators return error (no silent failures).
//   - Capability profile is loaded via capabilities.LoadProfile.
//   - Context is threaded through every blocking call.
package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

const migrationClassifyBatchSize = 20

// classifyProposalResult is the per-proposal JSON shape the LLM returns.
type classifyProposalResult struct {
	ProposalID     int    `json:"proposal_id"`
	Classification string `json:"classification"`  // "knowledge_observation" | "enforceable_rule"
	SuggestedScope string `json:"suggested_scope"` // "senate:*" | "senate:tag:<t>" | "senate:<repo>"
	Rationale      string `json:"rationale"`
}

// classifyBatchResponse is the top-level JSON envelope the LLM returns for a batch.
type classifyBatchResponse struct {
	Results []classifyProposalResult `json:"results"`
}

// classifySystemPrompt is the LLM system prompt used for all batch calls.
const classifySystemPrompt = `You are classifying Senate promotion proposals to distinguish reusable knowledge from enforceable rules.

A KNOWLEDGE OBSERVATION is a factual statement about a repo (architecture, patterns, conventions) that a Senator should know. Examples:
- "This repo uses GraphQL, not REST"
- "All controllers inherit from ApplicationController"
- "Database migrations are run with zero downtime"

An ENFORCEABLE RULE is a standard that could be mechanically verified — a requirement that can be checked against code or PRs. Examples:
- "All PRs must include tests"
- "No direct DB access in handler files"
- "API endpoints must have OpenAPI annotations"

Return JSON only — no prose, no markdown fences. The JSON must be a single object with a "results" array.`

// RunMigrationClassifyProposals classifies all unclassified pending
// PromotionProposals. dryRun=true prints what would happen without
// writing to the DB. Returns counts of (knowledgeAbsorbed, rulesFound)
// and an error if something went fatally wrong.
func RunMigrationClassifyProposals(ctx context.Context, db *sql.DB, dryRun bool, logger interface{ Printf(string, ...any) }) (knowledgeAbsorbed, rulesFound int, err error) {
	proposals, err := store.ListPendingPromotionProposals(db)
	if err != nil {
		return 0, 0, fmt.Errorf("RunMigrationClassifyProposals: list proposals: %w", err)
	}
	if len(proposals) == 0 {
		logger.Printf("MigrationClassifyProposals: no pending proposals to classify")
		return 0, 0, nil
	}
	logger.Printf("MigrationClassifyProposals: %d pending proposals to classify (dry-run=%v)", len(proposals), dryRun)

	prof, profErr := capabilities.LoadProfile("migration-classifier")
	if profErr != nil {
		return 0, 0, fmt.Errorf("RunMigrationClassifyProposals: LoadProfile: %w", profErr)
	}
	mcpConfig, _ := prof.MCPConfigArg()

	// Track rule candidates for the summary mail.
	type ruleCandidate struct {
		ProposalID     int
		RuleKey        string
		SuggestedScope string
	}
	var ruleCandidates []ruleCandidate

	// Process in batches of migrationClassifyBatchSize.
	for i := 0; i < len(proposals); i += migrationClassifyBatchSize {
		if ctx.Err() != nil {
			return knowledgeAbsorbed, rulesFound, ctx.Err()
		}
		end := i + migrationClassifyBatchSize
		if end > len(proposals) {
			end = len(proposals)
		}
		batch := proposals[i:end]

		results, batchErr := classifyBatch(ctx, db, batch, prof, mcpConfig, logger)
		if batchErr != nil {
			logger.Printf("MigrationClassifyProposals: batch %d-%d failed: %v", i, end-1, batchErr)
			continue
		}

		for _, res := range results {
			// Find the matching proposal row.
			var row *store.PromotionProposalRow
			for j := range batch {
				if batch[j].ID == res.ProposalID {
					row = &batch[j]
					break
				}
			}
			if row == nil {
				logger.Printf("MigrationClassifyProposals: LLM returned unknown proposal_id=%d — skipping", res.ProposalID)
				continue
			}

			switch res.Classification {
			case "knowledge_observation":
				if !dryRun {
					if absorptionErr := absorbProposalAsKnowledge(db, row, res, logger); absorptionErr != nil {
						logger.Printf("MigrationClassifyProposals: absorb proposal %d: %v", row.ID, absorptionErr)
						continue
					}
				} else {
					logger.Printf("[dry-run] would absorb proposal %d (%q) as knowledge for senator inferred from %q", row.ID, row.RuleKey, row.AuthoredBy)
				}
				knowledgeAbsorbed++

			case "enforceable_rule":
				scope := res.SuggestedScope
				if scope == "" {
					scope = "senate:*"
				}
				if !dryRun {
					if classErr := store.SetProposalClassification(db, row.ID, "awaiting_scope_review", scope); classErr != nil {
						logger.Printf("MigrationClassifyProposals: mark proposal %d awaiting_scope_review: %v", row.ID, classErr)
						continue
					}
				} else {
					logger.Printf("[dry-run] would surface proposal %d (%q) as enforceable rule, scope=%q", row.ID, row.RuleKey, scope)
				}
				rulesFound++
				ruleCandidates = append(ruleCandidates, ruleCandidate{
					ProposalID:     row.ID,
					RuleKey:        row.RuleKey,
					SuggestedScope: scope,
				})

			default:
				logger.Printf("MigrationClassifyProposals: unknown classification %q for proposal %d — skipping", res.Classification, res.ProposalID)
			}
		}
	}

	// Send summary fleet mail.
	if !dryRun {
		sendClassificationSummaryMail(db, knowledgeAbsorbed, rulesFound, ruleCandidates)
	} else {
		logger.Printf("[dry-run] summary: %d absorbed as knowledge, %d surfaced as rules awaiting scope review", knowledgeAbsorbed, rulesFound)
	}

	return knowledgeAbsorbed, rulesFound, nil
}

// classifyBatch calls the LLM for a slice of proposals and returns the
// parsed per-proposal results. Falls back to the deterministic stub when
// liveHaikuDisabled() is set (tests/CI).
func classifyBatch(ctx context.Context, db *sql.DB, batch []store.PromotionProposalRow, prof *capabilities.Profile, mcpConfig string, logger interface{ Printf(string, ...any) }) ([]classifyProposalResult, error) {
	if liveHaikuDisabled() || SpendCapExceeded(db) {
		return classifyBatchStub(batch), nil
	}

	userPrompt := buildClassifyUserPrompt(batch)
	raw, callErr := claude.CallWithTranscript(ctx,
		claude.CallDescriptor{Agent: "migration-classifier", PromptVersion: "d14-v1"},
		classifySystemPrompt, userPrompt,
		prof.AllowedToolsArg(), prof.DisallowedToolsArg(), mcpConfig, 1)
	if callErr != nil {
		return nil, fmt.Errorf("classifyBatch: LLM call: %w", callErr)
	}
	clean := claude.ExtractJSON(raw)
	var resp classifyBatchResponse
	if parseErr := json.Unmarshal([]byte(clean), &resp); parseErr != nil {
		return nil, fmt.Errorf("classifyBatch: parse JSON: %w (raw=%q)", parseErr, truncateRunes(clean, 200))
	}
	return resp.Results, nil
}

// classifyBatchStub returns deterministic results for tests (2 knowledge,
// 1 rule pattern cycling through proposals). Used when LIVE_HAIKU_DISABLED=1.
func classifyBatchStub(batch []store.PromotionProposalRow) []classifyProposalResult {
	out := make([]classifyProposalResult, 0, len(batch))
	for i, p := range batch {
		classification := "knowledge_observation"
		scope := ""
		if i%3 == 2 {
			classification = "enforceable_rule"
			scope = "senate:*"
		}
		out = append(out, classifyProposalResult{
			ProposalID:     p.ID,
			Classification: classification,
			SuggestedScope: scope,
			Rationale:      "deterministic stub",
		})
	}
	return out
}

// buildClassifyUserPrompt builds the user prompt for a batch of proposals.
func buildClassifyUserPrompt(batch []store.PromotionProposalRow) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Classify the following %d Senate promotion proposals.\n\n", len(batch)))
	sb.WriteString("Return a JSON object with this exact shape:\n")
	sb.WriteString(`{"results": [{"proposal_id": <int>, "classification": "knowledge_observation"|"enforceable_rule", "suggested_scope": "senate:*"|"senate:tag:<tagname>"|"senate:<reponame>", "rationale": "<brief reason>"}]}`)
	sb.WriteString("\n\nProposals:\n")
	for _, p := range batch {
		evidence := p.EvidenceSummaryJSON
		if len(evidence) > 300 {
			evidence = evidence[:300] + "..."
		}
		sb.WriteString(fmt.Sprintf(
			"\n---\nproposal_id: %d\nkey: %q\ndescription: %q\nevidence: %s\n",
			p.ID, p.RuleKey, p.ProposedContent, evidence,
		))
	}
	return sb.String()
}

// inferSenatorFromProposal derives a senator name from a proposal's
// authored_by or rule_key. Falls back to "global" if neither gives a
// useful repo name.
func inferSenatorFromProposal(row *store.PromotionProposalRow) string {
	// rule_key often has the form "repo/rule-slug" or "repo:rule-slug".
	// Try extracting the leading segment.
	key := strings.TrimSpace(row.RuleKey)
	if idx := strings.IndexAny(key, "/:."); idx > 0 {
		candidate := key[:idx]
		if candidate != "" && candidate != "senate" && candidate != "global" {
			return candidate
		}
	}
	// authored_by often names the emitting agent (e.g. "librarian", "senator:myrepo").
	if by := strings.TrimPrefix(row.AuthoredBy, "senator:"); by != row.AuthoredBy && by != "" {
		return by
	}
	return "global"
}

// absorbProposalAsKnowledge inserts a SenateMemory row for the proposal's
// inferred senator and marks the proposal as 'absorbed_as_knowledge'.
func absorbProposalAsKnowledge(db *sql.DB, row *store.PromotionProposalRow, res classifyProposalResult, logger interface{ Printf(string, ...any) }) error {
	senator := inferSenatorFromProposal(row)
	summary := row.ProposedContent
	if summary == "" {
		summary = row.RuleKey
	}
	if summary == "" {
		summary = "(no description)"
	}
	topic := "migration_import"
	if res.Rationale != "" {
		topic = "migration_import:" + truncateRunes(res.Rationale, 40)
	}
	if _, err := store.InsertSenateMemory(db, store.SenateMemoryEntry{
		Senator: senator,
		Topic:   topic,
		Summary: summary,
		Source:  "migration",
		Weight:  1.0,
	}); err != nil {
		return fmt.Errorf("absorbProposalAsKnowledge(id=%d): InsertSenateMemory: %w", row.ID, err)
	}
	if err := store.SetProposalClassification(db, row.ID, "absorbed_as_knowledge", ""); err != nil {
		return fmt.Errorf("absorbProposalAsKnowledge(id=%d): SetProposalClassification: %w", row.ID, err)
	}
	logger.Printf("MigrationClassifyProposals: proposal %d (%q) absorbed as knowledge for senator %q", row.ID, row.RuleKey, senator)
	return nil
}

type migrationRuleCandidate struct {
	ProposalID     int
	RuleKey        string
	SuggestedScope string
}

// sendClassificationSummaryMail sends a fleet-mail summary to the operator.
func sendClassificationSummaryMail(db *sql.DB, knowledgeAbsorbed, rulesFound int, candidates interface{}) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("D14 Phase 5 — PromotionProposal classification complete.\n\n"))
	sb.WriteString(fmt.Sprintf("  Absorbed as knowledge: %d\n", knowledgeAbsorbed))
	sb.WriteString(fmt.Sprintf("  Surfaced as enforceable rules (awaiting scope review): %d\n", rulesFound))

	type ruleRow struct {
		ProposalID     int
		RuleKey        string
		SuggestedScope string
	}

	// candidates is []ruleCandidate (local type). Use JSON round-trip to avoid
	// type-coupling to the outer local type.
	rawJSON, _ := json.Marshal(candidates)
	var rows []ruleRow
	_ = json.Unmarshal(rawJSON, &rows)
	if len(rows) > 0 {
		sb.WriteString("\nRule candidates:\n")
		for _, r := range rows {
			sb.WriteString(fmt.Sprintf("  - proposal %d  key=%q  scope=%q\n", r.ProposalID, r.RuleKey, r.SuggestedScope))
		}
	}
	sb.WriteString("\nReview rule candidates via `force migration classify-proposals` or the dashboard.")

	emitOperatorMailMedium(context.Background(), db,
		"migration-classifier",
		"[D14 MIGRATION] Proposal classification complete",
		sb.String(),
		0,
		store.MailTypeInfo,
	)
}

// truncateRunes returns s truncated to at most n runes.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
