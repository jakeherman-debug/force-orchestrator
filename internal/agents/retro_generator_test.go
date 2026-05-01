package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestRetroGenerator covers 6B.13 invariants:
//   - GenerateRetro produces markdown with the expected sections
//     (top win, top frustration, suggested experiment, week's stats).
//   - Stats counters reflect rejections + escalations + flagged
//     annotations + ratified PromotionProposals in the 7-day window.
//   - SaveRetroDraft writes to docs/retros/<date>.md and refuses
//     paths outside the docs/retros/ root.
//   - Empty-week and busy-week shapes both produce valid markdown.
func TestRetroGenerator(t *testing.T) {
	t.Run("generates_markdown_sections", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		// Seed a busy week
		_, _ = db.Exec(`INSERT INTO BriefingRenders
			(decision_id, decision_kind, briefing_text, operator_decision, decision_time_seconds, rendered_at)
			VALUES (1, 'captain_ruling', 'b', 'reject', 30, datetime('now', '-1 days'))`)
		_, _ = db.Exec(`INSERT INTO Escalations (id, convoy_id, status, message, created_at)
			VALUES (1, 1, 'Open', 'help', datetime('now', '-1 days'))`)
		_, _ = db.Exec(`INSERT INTO OperatorEventAnnotations
			(operator_email, event_kind, event_ref, note_text, flag, noted_at)
			VALUES ('op', 'llm_call', '1', 'this prompt is bad', 'problem', datetime('now', '-1 days'))`)
		_, _ = db.Exec(`INSERT INTO PromotionProposals
			(experiment_id, kind, ratified_at) VALUES (1, 'promote', datetime('now', '-1 days'))`)

		retro, err := GenerateRetro(context.Background(), db, time.Now())
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		md := retro.Markdown
		for _, section := range []string{"Top win", "Top frustration", "Suggested experiment", "Week's stats"} {
			if !strings.Contains(md, section) {
				t.Errorf("missing section %q in:\n%s", section, md)
			}
		}
		if !strings.Contains(md, "1 PromotionProposal") {
			t.Errorf("expected ratified count: %s", md)
		}
		if !strings.Contains(retro.SuggestedPath, "docs/retros") {
			t.Errorf("suggested path missing docs/retros: %q", retro.SuggestedPath)
		}
	})

	t.Run("empty_week_still_produces_valid_markdown", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		retro, err := GenerateRetro(context.Background(), db, time.Now())
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.Contains(retro.Markdown, "Top win") {
			t.Errorf("missing section in empty-week shape")
		}
	})

	t.Run("save_writes_to_canonical_path_only", func(t *testing.T) {
		// sandbox cwd in a tempdir so docs/retros lands under tmp.
		tmp := t.TempDir()
		oldWD, _ := os.Getwd()
		t.Cleanup(func() { os.Chdir(oldWD) })
		os.Chdir(tmp)

		path, err := SaveRetroDraft("docs/retros/2026-04-30.md", "# hi")
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		body, _ := os.ReadFile(path)
		if !strings.Contains(string(body), "# hi") {
			t.Errorf("contents: %q", string(body))
		}

		// Path-traversal refused
		_, err = SaveRetroDraft("../../../etc/evil.md", "evil")
		if err == nil {
			t.Error("expected refusal of out-of-root path")
		}
	})

	t.Run("nil_db_errors", func(t *testing.T) {
		_, err := GenerateRetro(context.Background(), nil, time.Now())
		if err == nil {
			t.Error("expected error on nil db")
		}
	})

	t.Run("save_path_traversal_blocked", func(t *testing.T) {
		_, err := SaveRetroDraft("/etc/passwd", "evil")
		if err == nil {
			t.Error("expected refusal of absolute path")
		}
		_, err = SaveRetroDraft("docs/retros/../../etc/evil.md", "evil")
		if err == nil {
			t.Error("expected refusal of traversal")
		}
	})
}

// TestRetroGenerator_AbsPath ensures filepath.Abs of a saved retro
// resolves under the working directory.
func TestRetroGenerator_AbsPath(t *testing.T) {
	tmp := t.TempDir()
	oldWD, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(oldWD) })
	os.Chdir(tmp)

	path, err := SaveRetroDraft("docs/retros/2026-05-01.md", "ok")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	abs, _ := filepath.Abs(path)
	// macOS symlinks /var → /private/var; use EvalSymlinks for both
	// sides so the comparison is robust.
	tmpReal, _ := filepath.EvalSymlinks(tmp)
	absReal, _ := filepath.EvalSymlinks(abs)
	if !strings.HasPrefix(absReal, tmpReal) {
		t.Errorf("saved path outside tmp: %q (tmp %q)", absReal, tmpReal)
	}
}
