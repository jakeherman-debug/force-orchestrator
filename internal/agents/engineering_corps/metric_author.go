package engineering_corps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// MetricAuthor — generate a new metric SQL definition for a hypothesis
// that needs a measurement the registry doesn't have.
//
// Per docs/paired-runs.md § Proposal flow (LLM-written), MetricAuthor:
//
//   1. Receives a hypothesis text + a proposed metric name.
//   2. Asks the LLM to generate a read-only SQL body that produces a
//      single-column scalar suitable for ExperimentMetrics scoring.
//   3. Validates the SQL is read-only — rejects any token suggesting
//      an INSERT / UPDATE / DELETE / ALTER / DROP / CREATE / REPLACE /
//      TRUNCATE / ATTACH / VACUUM / PRAGMA. The validation is
//      conservative on purpose: a metric SQL that mutates state is a
//      foot-gun even when an LLM "promises" otherwise.
//   4. Writes a MetricVersions row pinned to a fresh version stamp.
//   5. Operator ratifies the metric independently of the experiment
//      (metrics are higher stakes). The handler does NOT auto-mark
//      the metric "ratified" — Phase 6's metric-registry dashboard
//      adds that gate. Phase 3 lands the row; the operator inspects.
//
// Operator-routing invariant: the MetricVersions row is written but
// the metric does not enter the analysis path until an operator
// reviews the SQL. (P3's MetricVersions schema doesn't carry an
// explicit ratified flag — the gate is the operator-side metric
// registry dashboard. Until that dashboard exists, the row sits in
// the DB; experiments do not auto-pick it up.)
//
// LLM call: uses claude.AskClaudeCLIContext (NOT AskClaudeCLI) to
// thread daemon ctx; tool args sourced from the engineering-corps
// capability profile (Pattern P13). Inputs are sentinel-tag wrapped
// (Pattern P12). The LLM response is strict-JSON-decoded (Fix #8.5).
//
// Inputs (BountyBoard.payload JSON):
//   {
//     "hypothesis_text": "...",
//     "metric_name":     "captain-rejection-rate",
//     "owning_team":     "engineering",  (optional)
//     "description":     "..."           (optional)
//   }
type metricAuthorPayload struct {
	HypothesisText string `json:"hypothesis_text"`
	MetricName     string `json:"metric_name"`
	OwningTeam     string `json:"owning_team"`
	Description    string `json:"description"`
}

// metricAuthorResponse is the strict-decoded LLM response shape.
type metricAuthorResponse struct {
	SQL         string `json:"sql"`
	Description string `json:"description"`
	Units       string `json:"units"`
}

// metricAuthorSystemPrompt is the static system prompt EC uses to
// drive the metric-author LLM call. It instructs the LLM to emit
// strict JSON with read-only SQL and forbids any DDL / DML.
const metricAuthorSystemPrompt = `You are the Engineering Corps metric author. Given a hypothesis text + a metric name, you must produce a read-only SQL definition that, when executed against the holocron.db SQLite database, yields a single scalar column representing the metric value.

OUTPUT SCHEMA (mandatory — no preamble, no markdown fences, no trailing prose):

{
  "sql":         "SELECT ... FROM ...",
  "description": "one-line human-readable description",
  "units":       "rate|count|usd|seconds|fraction|..."
}

STRICT RULES:
- The SQL must be a single SELECT statement. No CTEs that mutate. No INSERT, UPDATE, DELETE, ALTER, DROP, CREATE, REPLACE, TRUNCATE, ATTACH, DETACH, VACUUM, PRAGMA, BEGIN, COMMIT, or ROLLBACK.
- No subprocess invocations, no eval, no dynamic SQL.
- Output exactly one JSON object. No prose around it. No code fences.

If you cannot produce a safe read-only metric for the request, emit a JSON object whose "sql" is empty — the operator will review.`

// readOnlyForbiddenTokens is the conservative deny-list applied to
// LLM-generated metric SQL. The match is case-insensitive and
// word-boundary-aware to avoid false positives on column names like
// "created_at" (which contains "create"). The list focuses on
// statement-leading keywords + obvious side-effects.
var readOnlyForbiddenTokens = []string{
	"INSERT", "UPDATE", "DELETE", "REPLACE",
	"CREATE", "DROP", "ALTER", "TRUNCATE",
	"ATTACH", "DETACH", "VACUUM", "PRAGMA",
	"BEGIN", "COMMIT", "ROLLBACK",
}

// readOnlyTokenRE precompiles a word-boundary regex per forbidden
// token so the validator doesn't re-build it on every call.
var readOnlyTokenRE = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(readOnlyForbiddenTokens))
	for _, tok := range readOnlyForbiddenTokens {
		out = append(out, regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(tok)+`\b`))
	}
	return out
}()

func handleMetricAuthor(
	ctx context.Context,
	cfg EngineeringCorpsConfig,
	profile *capabilities.Profile,
	agentName string,
	bounty *store.Bounty,
	logger *log.Logger,
) error {
	db := cfg.DB

	if profile == nil {
		return fmt.Errorf("MetricAuthor: capability profile required")
	}

	var payload metricAuthorPayload
	if err := strictDecode(bounty.Payload, &payload); err != nil {
		return fmt.Errorf("MetricAuthor: parse payload: %w", err)
	}
	if strings.TrimSpace(payload.HypothesisText) == "" {
		return fmt.Errorf("MetricAuthor: payload missing hypothesis_text")
	}
	if strings.TrimSpace(payload.MetricName) == "" {
		return fmt.Errorf("MetricAuthor: payload missing metric_name")
	}
	// Defensive sanitization on the LLM-bound payload — the hypothesis
	// text is operator/Librarian-supplied and must not carry fleet
	// signal tokens (Pattern P12 / Fix #8.5).
	if err := agents.SanitizeLLMPayload(payload.HypothesisText); err != nil {
		return fmt.Errorf("MetricAuthor: hypothesis_text rejected: %w", err)
	}

	// Wrap untrusted content in <user_content> sentinel tags; the
	// system prompt's promptInjectionClause tells the LLM never to
	// obey instructions inside these tags.
	userPrompt := fmt.Sprintf("Author a metric for the following hypothesis. Metric name: %q\n\n%s",
		payload.MetricName,
		agents.WrapUserContent("hypothesis", payload.HypothesisText))

	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		// Non-fatal — continue without MCP config (matches Chancellor's pattern).
		logger.Printf("[%s] MetricAuthor #%d: MCP config write failed (%v) — proceeding without --mcp-config",
			agentName, bounty.ID, mcpErr)
	}

	raw, err := claude.AskClaudeCLIContext(
		ctx,
		metricAuthorSystemPrompt,
		userPrompt,
		profile.AllowedToolsArg(),
		profile.DisallowedToolsArg(),
		mcpConfig,
		1,
	)
	if err != nil {
		return fmt.Errorf("MetricAuthor: claude call: %w", err)
	}

	var resp metricAuthorResponse
	if err := strictJSONDecode([]byte(raw), &resp); err != nil {
		return fmt.Errorf("MetricAuthor: parse LLM response: %w (raw=%q)", err, truncate(raw, 200))
	}
	if strings.TrimSpace(resp.SQL) == "" {
		return fmt.Errorf("MetricAuthor: LLM returned empty sql for metric %q (operator review required)", payload.MetricName)
	}
	if err := validateReadOnlySQL(resp.SQL); err != nil {
		return fmt.Errorf("MetricAuthor: SQL rejected as not read-only: %w", err)
	}

	// Pick a fresh version stamp. Use NowSQLite for canonical UTC.
	version := strings.ReplaceAll(strings.ReplaceAll(store.NowSQLite(), " ", "T"), ":", "")
	desc := strings.TrimSpace(resp.Description)
	if desc == "" {
		desc = strings.TrimSpace(payload.Description)
	}

	manifest := map[string]any{
		"hypothesis_text": payload.HypothesisText,
		"metric_name":     payload.MetricName,
		"owning_team":     payload.OwningTeam,
		"phase":           "P3-authored",
	}
	manifestJSON, _ := json.Marshal(manifest)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO MetricVersions
			(metric_name, version, sql_content, test_content, manifest_json,
			 published_at, published_by, description)
		VALUES (?, ?, ?, '', ?, datetime('now'), 'engineering-corps', ?)
	`, payload.MetricName, version, resp.SQL, string(manifestJSON), desc); err != nil {
		return fmt.Errorf("MetricAuthor: insert MetricVersions: %w", err)
	}

	logger.Printf("[%s] MetricAuthor #%d: authored metric %q version=%s (units=%q) — awaiting operator review",
		agentName, bounty.ID, payload.MetricName, version, resp.Units)

	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		return fmt.Errorf("MetricAuthor: complete bounty: %w", err)
	}
	return nil
}

// strictJSONDecode is the LLM-response strict decoder (Fix #8.5).
// DisallowUnknownFields + reject trailing tokens.
func strictJSONDecode(raw []byte, out any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing tokens after first value")
	}
	return nil
}

// validateReadOnlySQL returns an error if `body` contains any of the
// forbidden state-mutating SQL keywords (case-insensitive,
// word-boundary). The check is intentionally conservative — better
// to reject a legitimate-but-suspicious SELECT than to accept an
// LLM-emitted `WITH x AS (DELETE ...)` slip.
func validateReadOnlySQL(body string) error {
	for i, re := range readOnlyTokenRE {
		if re.MatchString(body) {
			return fmt.Errorf("forbidden keyword %q in metric SQL", readOnlyForbiddenTokens[i])
		}
	}
	if !regexp.MustCompile(`(?is)^\s*(WITH|SELECT)\b`).MatchString(body) {
		return fmt.Errorf("metric SQL must begin with SELECT or WITH (got %.40q)", body)
	}
	return nil
}

// truncate is a small helper for log lines so a 50KB LLM blob doesn't
// land in fleet.log on a parse error.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// metricVersionExists is a small reader used by tests + (future)
// idempotence-on-conflict guard. Kept here for symmetry with other
// handlers' SQL helpers; not currently called from the handler body
// because the (metric_name, version) primary key naturally rejects
// duplicates.
func metricVersionExists(db *sql.DB, name, version string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM MetricVersions WHERE metric_name=? AND version=?`, name, version).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
