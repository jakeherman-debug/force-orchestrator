package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// FleetRuleSeed describes one row that the bootstrap migration produces.
// Justification is documentation-only — it explains *why* this rule
// landed at the chosen render_to, especially for the narrow set that
// stay in the universal-load CLAUDE.md file. The DB does not store it.
type FleetRuleSeed struct {
	RuleKey       string
	Section       string // top-level CLAUDE.md H2 the seed came from
	Category      string // semantic-kind tag
	AgentScope    string
	RenderTo      string
	EnforcedBy    string
	Justification string
	Content       string
}

// BootstrapFleetRules inserts the audited FleetRules seeds into the
// database. Convergent on content: a second invocation against a clean
// DB no-ops; against a DB whose bootstrap-managed rows have drifted
// from the audit slice (stale content / hash) it REFRESHES those rows
// to match the current audit. Operator-direct-write rows
// (created_by != 'bootstrap') are never touched — the convergence
// scope is bootstrap-managed rows only.
//
// claudeMdPath is read for the all-sections-covered safety check —
// every H2 in the file MUST appear in at least one seed's Section field
// or the bootstrap returns an error rather than silently dropping
// content. Pass an empty string to skip the check (useful in tests
// that want to operate against a synthetic seed list).
//
// Returned int is the number of rows touched (fresh INSERT or
// content-hash mismatch UPDATE). Idempotent re-runs return 0.
func BootstrapFleetRules(ctx context.Context, db *sql.DB, claudeMdPath string) (int, error) {
	if claudeMdPath != "" {
		if err := assertAllSectionsCovered(claudeMdPath, bootstrapAudit); err != nil {
			return 0, err
		}
	}
	if err := assertJustifications(bootstrapAudit); err != nil {
		return 0, err
	}

	touched := 0
	for _, seed := range bootstrapAudit {
		hash := sha256Hex(seed.Content)
		// Convergent UPSERT.
		//   - Fresh row → INSERT → RETURNING fires → counted.
		//   - Conflict, bootstrap-managed, hash differs → UPDATE → RETURNING fires → counted.
		//   - Conflict, bootstrap-managed, hash matches → WHERE false → no-op (created_at preserved).
		//   - Conflict, operator-direct-write → WHERE false → no-op (operator row preserved).
		// The content_hash predicate is load-bearing: without it,
		// idempotent re-runs would UPDATE every row and break
		// TestBootstrapFleetRules_Idempotent. The created_by predicate
		// is load-bearing: without it, bootstrap could clobber
		// operator-routed rules (per docs/paired-runs.md § Direct-write
		// rules).
		var id int
		err := db.QueryRowContext(ctx, `
			INSERT INTO FleetRules (
				rule_key, category, agent_scope, render_to, enforced_by,
				content, content_hash, version, active_from, created_by
			) VALUES (?, ?, ?, ?, ?, ?, ?, 1, datetime('now'), 'bootstrap')
			ON CONFLICT(rule_key, version) DO UPDATE SET
				category     = excluded.category,
				agent_scope  = excluded.agent_scope,
				render_to    = excluded.render_to,
				enforced_by  = excluded.enforced_by,
				content      = excluded.content,
				content_hash = excluded.content_hash
			WHERE FleetRules.created_by = 'bootstrap'
			  AND FleetRules.content_hash != excluded.content_hash
			RETURNING id
		`,
			seed.RuleKey, seed.Category, seed.AgentScope, seed.RenderTo, seed.EnforcedBy,
			seed.Content, hash,
		).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			// Either (a) hash matched on a bootstrap row (idempotent
			// no-op), or (b) operator-direct-write row preserved.
			// Both are intended outcomes.
		case err != nil:
			return touched, fmt.Errorf("bootstrap rule %q: %w", seed.RuleKey, err)
		default:
			touched++
		}
	}
	return touched, nil
}

// BootstrapAudit returns the in-memory seed list — exposed for tests.
func BootstrapAudit() []FleetRuleSeed { return bootstrapAudit }

// assertAllSectionsCovered parses every `## ` heading from CLAUDE.md
// and returns an error if any heading lacks a corresponding seed entry.
// This forces the auditor to make an explicit categorization decision
// rather than silently dropping content during a re-bootstrap.
func assertAllSectionsCovered(claudeMdPath string, seeds []FleetRuleSeed) error {
	body, err := os.ReadFile(claudeMdPath)
	if err != nil {
		return fmt.Errorf("read CLAUDE.md: %w", err)
	}
	headings := parseClaudeMdH2s(string(body))
	covered := map[string]bool{}
	for _, s := range seeds {
		covered[normaliseSection(s.Section)] = true
	}
	var missing []string
	for _, h := range headings {
		if !covered[normaliseSection(h)] {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("BootstrapFleetRules: %d CLAUDE.md section(s) have no audit entry — categorize them or surface to operator: %v", len(missing), missing)
	}
	return nil
}

// assertJustifications enforces the operator-discipline directive: every
// claude-md-file seed MUST carry a non-empty Justification so a future
// reviewer can see why universal-load placement was chosen.
func assertJustifications(seeds []FleetRuleSeed) error {
	var unjustified []string
	for _, s := range seeds {
		if s.RenderTo == "claude-md-file" && strings.TrimSpace(s.Justification) == "" {
			unjustified = append(unjustified, s.RuleKey)
		}
	}
	if len(unjustified) > 0 {
		return fmt.Errorf("BootstrapFleetRules: claude-md-file entries missing Justification: %v", unjustified)
	}
	return nil
}

var h2HeadingRe = regexp.MustCompile(`(?m)^## +(.+?)\s*$`)

func parseClaudeMdH2s(body string) []string {
	matches := h2HeadingRe.FindAllStringSubmatch(body, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// normaliseSection lowercases + strips common decoration so a small
// title rewrite doesn't fail the all-sections-covered check.
func normaliseSection(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
