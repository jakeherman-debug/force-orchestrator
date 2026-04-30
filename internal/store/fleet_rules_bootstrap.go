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
// database. Idempotent: a second invocation finds the (rule_key, version=1)
// rows already present and skips without changing them.
//
// claudeMdPath is read for the all-sections-covered safety check —
// every H2 in the file MUST appear in at least one seed's Section field
// or the bootstrap returns an error rather than silently dropping
// content. Pass an empty string to skip the check (useful in tests
// that want to operate against a synthetic seed list).
func BootstrapFleetRules(ctx context.Context, db *sql.DB, claudeMdPath string) (int, error) {
	if claudeMdPath != "" {
		if err := assertAllSectionsCovered(claudeMdPath, bootstrapAudit); err != nil {
			return 0, err
		}
	}
	if err := assertJustifications(bootstrapAudit); err != nil {
		return 0, err
	}

	inserted := 0
	for _, seed := range bootstrapAudit {
		hash := sha256Hex(seed.Content)
		// (rule_key, version) UNIQUE means a second run with the same key
		// at version=1 conflicts and silently no-ops. The RETURNING id
		// shape lets us count actual inserts vs no-ops.
		var id int
		err := db.QueryRowContext(ctx, `
			INSERT INTO FleetRules (
				rule_key, category, agent_scope, render_to, enforced_by,
				content, content_hash, version, active_from, created_by
			) VALUES (?, ?, ?, ?, ?, ?, ?, 1, datetime('now'), 'bootstrap')
			ON CONFLICT(rule_key, version) DO NOTHING
			RETURNING id
		`,
			seed.RuleKey, seed.Category, seed.AgentScope, seed.RenderTo, seed.EnforcedBy,
			seed.Content, hash,
		).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			// Already present; idempotent no-op.
		case err != nil:
			return inserted, fmt.Errorf("bootstrap rule %q: %w", seed.RuleKey, err)
		default:
			inserted++
		}
	}
	return inserted, nil
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
