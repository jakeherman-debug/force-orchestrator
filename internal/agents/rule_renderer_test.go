package agents

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestRenderClaudeMdFile_BootstrapOutput_Under10KB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	if _, err := store.BootstrapFleetRules(ctx, db, ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	body, err := RenderClaudeMdFile(ctx, db)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	const target10K = 10 * 1024
	if len(body) > target10K {
		t.Logf("WARNING: rendered CLAUDE.md is %d bytes; Phase 1 target is ≤ %d. Hard cap is %d.", len(body), target10K, ClaudeMdHardCapBytes)
	}
	if len(body) > ClaudeMdHardCapBytes {
		t.Fatalf("rendered CLAUDE.md is %d bytes; hard cap is %d", len(body), ClaudeMdHardCapBytes)
	}
	if !strings.Contains(string(body), "AUTO-GENERATED") {
		t.Errorf("rendered CLAUDE.md missing the AUTO-GENERATED preamble — operator-readable provenance must be visible")
	}
	t.Logf("rendered CLAUDE.md: %d bytes (target ≤ %d, hard cap %d)", len(body), target10K, ClaudeMdHardCapBytes)
}

func TestRenderClaudeMdFile_HardCapEnforced(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Seed a single oversized claude-md-file rule so the renderer is
	// guaranteed to bust the cap.
	bigContent := strings.Repeat("a", ClaudeMdHardCapBytes+1024)
	if _, err := db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, created_by)
		VALUES ('test-overflow', 'architecture', 'all', 'claude-md-file', 'trust-only', ?, '', 1, datetime('now'), 'test')`,
		bigContent); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := RenderClaudeMdFile(ctx, db)
	if err == nil {
		t.Fatalf("expected RenderClaudeMdFile to reject oversized output; got nil error")
	}
	if !strings.Contains(err.Error(), "RULE-RENDERER OVERFLOW") {
		t.Errorf("error %q lacks the expected [RULE-RENDERER OVERFLOW] marker", err.Error())
	}
}

func TestAssemblePerAgentPrompt_FilteredCorrectly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	mustInsertRule(t, db, "rule-all",     "all",                "agent-prompt", "ALL_RULE_BODY")
	mustInsertRule(t, db, "rule-captain", "captain",            "agent-prompt", "CAPTAIN_RULE_BODY")
	mustInsertRule(t, db, "rule-multi",   "captain,council",    "agent-prompt", "MULTI_RULE_BODY")
	mustInsertRule(t, db, "rule-other",   "medic",              "agent-prompt", "MEDIC_RULE_BODY")
	mustInsertRule(t, db, "rule-cmd-only","captain",            "claude-md-file", "CAPTAIN_CLAUDEMD_BODY")

	got, err := AssemblePerAgentPrompt(ctx, db, "captain")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	checks := []struct {
		needle string
		want   bool
	}{
		{"ALL_RULE_BODY", true},
		{"CAPTAIN_RULE_BODY", true},
		{"MULTI_RULE_BODY", true},
		{"MEDIC_RULE_BODY", false},
		{"CAPTAIN_CLAUDEMD_BODY", false}, // wrong render_to — should not appear
	}
	for _, c := range checks {
		has := strings.Contains(got, c.needle)
		if has != c.want {
			t.Errorf("captain prompt: contains %q = %v, want %v\n--- assembled ---\n%s", c.needle, has, c.want, got)
		}
	}
}

func TestRenderer_DispatchesByRenderTo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	mustInsertRule(t, db, "test-claude",  "all", "claude-md-file",                    "CLAUDE_BODY")
	mustInsertRule(t, db, "test-fixlog",  "all", "fix-log",                           "FIXLOG_BODY")
	mustInsertRule(t, db, "test-domain",  "all", "per-domain-doc:docs/test-doc.md",   "DOMAIN_BODY")
	mustInsertRule(t, db, "test-prompt",  "all", "agent-prompt",                      "PROMPT_BODY")
	mustInsertRule(t, db, "test-discard", "all", "discard",                           "DISCARD_BODY")

	claudeMd, err := RenderClaudeMdFile(ctx, db)
	if err != nil {
		t.Fatalf("render claude-md: %v", err)
	}
	if !strings.Contains(string(claudeMd), "CLAUDE_BODY") {
		t.Errorf("CLAUDE.md missing CLAUDE_BODY")
	}
	for _, leaked := range []string{"FIXLOG_BODY", "DOMAIN_BODY", "PROMPT_BODY", "DISCARD_BODY"} {
		if strings.Contains(string(claudeMd), leaked) {
			t.Errorf("CLAUDE.md leaked %q from a different render target", leaked)
		}
	}

	fixLog, err := RenderFixLog(ctx, db)
	if err != nil {
		t.Fatalf("render fix-log: %v", err)
	}
	if !strings.Contains(string(fixLog), "FIXLOG_BODY") {
		t.Errorf("FIX-LOG.md missing FIXLOG_BODY")
	}
	for _, leaked := range []string{"CLAUDE_BODY", "DOMAIN_BODY", "PROMPT_BODY", "DISCARD_BODY"} {
		if strings.Contains(string(fixLog), leaked) {
			t.Errorf("FIX-LOG.md leaked %q", leaked)
		}
	}

	docs, err := RenderPerDomainDocs(ctx, db)
	if err != nil {
		t.Fatalf("render per-domain: %v", err)
	}
	body, ok := docs["docs/test-doc.md"]
	if !ok {
		t.Fatalf("per-domain rendering missing docs/test-doc.md; got keys %v", keys(docs))
	}
	if !strings.Contains(string(body), "DOMAIN_BODY") {
		t.Errorf("docs/test-doc.md missing DOMAIN_BODY")
	}
}

func TestRenderClaudeMdFile_ExcludesRetiredRules(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	mustInsertRule(t, db, "active-rule",  "all", "claude-md-file", "ACTIVE_BODY")
	mustInsertRetiredRule(t, db, "retired-rule", "all", "claude-md-file", "RETIRED_BODY")

	body, err := RenderClaudeMdFile(ctx, db)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(body), "ACTIVE_BODY") {
		t.Errorf("renderer dropped active rule")
	}
	if strings.Contains(string(body), "RETIRED_BODY") {
		t.Errorf("renderer included retired rule")
	}
}

// TestCheckRenderDrift_IncludesFixLog asserts that the drift check
// detects hand-edits to FIX-LOG.md by default (D3-P1 follow-up C).
// Phase 1 had FIX-LOG.md gated behind --include-fix-log because the
// initial audit only covered ~5 narratives; the audit now owns every
// narrative so drift detection is on by default.
func TestCheckRenderDrift_IncludesFixLog(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	mustInsertRule(t, db, "drift-fixlog", "all", "fix-log", "FIXLOG_RENDERED_BODY")

	dir := t.TempDir()

	// Write CLAUDE.md / docs/* matching the in-memory render so they
	// don't confound the drift check.
	claudeMd, err := RenderClaudeMdFile(ctx, db)
	if err != nil {
		t.Fatalf("render claude-md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), claudeMd, 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	// Write FIX-LOG.md with a hand-edited (deliberately stale) body.
	if err := os.WriteFile(filepath.Join(dir, "FIX-LOG.md"), []byte("hand-edited drift content\n"), 0o644); err != nil {
		t.Fatalf("write FIX-LOG.md: %v", err)
	}

	diverged, err := CheckRenderDrift(ctx, db, dir)
	if err != nil {
		t.Fatalf("CheckRenderDrift: %v", err)
	}

	foundFixLog := false
	for _, p := range diverged {
		if p == "FIX-LOG.md" {
			foundFixLog = true
			break
		}
	}
	if !foundFixLog {
		t.Errorf("CheckRenderDrift did not detect drift on FIX-LOG.md; got %v — drift check is silently skipping the file", diverged)
	}

	// Sanity: write the rendered FIX-LOG.md back; drift should clear.
	fixLog, err := RenderFixLog(ctx, db)
	if err != nil {
		t.Fatalf("render fix-log: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "FIX-LOG.md"), fixLog, 0o644); err != nil {
		t.Fatalf("write FIX-LOG.md: %v", err)
	}
	diverged2, err := CheckRenderDrift(ctx, db, dir)
	if err != nil {
		t.Fatalf("CheckRenderDrift (post-write): %v", err)
	}
	for _, p := range diverged2 {
		if p == "FIX-LOG.md" {
			t.Errorf("CheckRenderDrift reports drift on FIX-LOG.md after writing the rendered bytes back: %v", diverged2)
		}
	}
}

func mustInsertRule(t *testing.T, db *sql.DB, key, scope, renderTo, body string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, created_by)
		VALUES (?, 'architecture', ?, ?, 'trust-only', ?, '', 1, datetime('now'), 'test')`,
		key, scope, renderTo, body)
	if err != nil {
		t.Fatalf("insert rule %q: %v", key, err)
	}
}

func mustInsertRetiredRule(t *testing.T, db *sql.DB, key, scope, renderTo, body string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, active_until, created_by)
		VALUES (?, 'architecture', ?, ?, 'trust-only', ?, '', 1, datetime('now','-1 day'), datetime('now','-1 hour'), 'test')`,
		key, scope, renderTo, body)
	if err != nil {
		t.Fatalf("insert retired rule %q: %v", key, err)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
