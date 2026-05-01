package agents

import (
	"compress/gzip"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// stubLogger satisfies the dog logger interface.
type stubLogger struct {
	t *testing.T
}

func (s *stubLogger) Printf(format string, args ...any) {
	if s.t != nil {
		s.t.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// TestTranscriptArchive covers 6B.9 invariants:
//   - Old transcripts (>30d) are archived: row's response replaced
//     with summary, archived_at stamped, body offloaded to disk.
//   - Recent transcripts are NOT touched.
//   - Idempotence: running the dog twice in a row archives nothing
//     the second time.
//   - LoadArchivedBody round-trips the persisted body.
//   - Path traversal defence: the resolved path stays inside the
//     archive base.
func TestTranscriptArchive(t *testing.T) {
	t.Run("archives_old_rows_skips_recent", func(t *testing.T) {
		// Sandbox HOME so the archive lands in a tmp dir.
		oldHome := os.Getenv("HOME")
		t.Cleanup(func() { os.Setenv("HOME", oldHome) })
		os.Setenv("HOME", t.TempDir())

		db := store.InitHolocronDSN(":memory:")
		defer db.Close()

		// Old transcript (40 days ago) — should be archived.
		_, err := db.Exec(`INSERT INTO LLMCallTranscripts
			(id, task_id, agent, prompt_version, call_started_at, call_completed_at,
			 system_prompt, user_prompt, response_text)
			VALUES (1, 0, 'captain', 'v1',
			 datetime('now', '-40 days'), datetime('now', '-40 days'),
			 'sys-old', 'usr-old', 'response-old')`)
		if err != nil {
			t.Fatalf("seed old: %v", err)
		}
		// Recent transcript (today) — should be SKIPPED.
		_, err = db.Exec(`INSERT INTO LLMCallTranscripts
			(id, task_id, agent, prompt_version, call_started_at, call_completed_at,
			 system_prompt, user_prompt, response_text)
			VALUES (2, 0, 'captain', 'v1',
			 datetime('now'), datetime('now'),
			 'sys-new', 'usr-new', 'response-new')`)
		if err != nil {
			t.Fatalf("seed new: %v", err)
		}

		if err := dogTranscriptArchive(context.Background(), db, &stubLogger{t: t}); err != nil {
			t.Fatalf("archive: %v", err)
		}

		// Old row: archived_at set, response_text replaced with summary
		var archAt, resp, sys, usr string
		db.QueryRow(`SELECT archived_at, response_text, system_prompt, user_prompt FROM LLMCallTranscripts WHERE id=1`).Scan(&archAt, &resp, &sys, &usr)
		if archAt == "" {
			t.Errorf("old row not archived: archived_at=%q", archAt)
		}
		if !strings.Contains(resp, "[archived]") {
			t.Errorf("old row response not summarised: %q", resp)
		}
		if sys != "" || usr != "" {
			t.Errorf("old row prompts not cleared: sys=%q usr=%q", sys, usr)
		}

		// Recent row: untouched
		var newArchAt, newResp string
		db.QueryRow(`SELECT archived_at, response_text FROM LLMCallTranscripts WHERE id=2`).Scan(&newArchAt, &newResp)
		if newArchAt != "" {
			t.Errorf("recent row should NOT be archived: %q", newArchAt)
		}
		if newResp != "response-new" {
			t.Errorf("recent row response mutated: %q", newResp)
		}

		// File on disk exists and contains the original body.
		var startedAt string
		db.QueryRow(`SELECT call_started_at FROM LLMCallTranscripts WHERE id=1`).Scan(&startedAt)
		body, err := LoadArchivedBody(1, startedAt)
		if err != nil {
			t.Fatalf("LoadArchivedBody: %v", err)
		}
		if !strings.Contains(body, "response-old") {
			t.Errorf("archived body missing original response: %q", body)
		}
		if !strings.Contains(body, "sys-old") {
			t.Errorf("archived body missing system prompt: %q", body)
		}
	})

	t.Run("idempotence_run_twice_no_double_archive", func(t *testing.T) {
		oldHome := os.Getenv("HOME")
		t.Cleanup(func() { os.Setenv("HOME", oldHome) })
		os.Setenv("HOME", t.TempDir())

		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO LLMCallTranscripts
			(id, task_id, agent, prompt_version, call_started_at, call_completed_at,
			 system_prompt, user_prompt, response_text)
			VALUES (10, 0, 'captain', 'v1',
			 datetime('now', '-31 days'), datetime('now', '-31 days'),
			 's', 'u', 'r')`)

		// Run twice; second pass should archive zero new rows.
		_ = dogTranscriptArchive(context.Background(), db, &stubLogger{t: t})
		_ = dogTranscriptArchive(context.Background(), db, &stubLogger{t: t})

		var rows int
		db.QueryRow(`SELECT COUNT(*) FROM LLMCallTranscripts WHERE id=10 AND archived_at != ''`).Scan(&rows)
		if rows != 1 {
			t.Errorf("expected exactly 1 archived row after idempotent run, got %d", rows)
		}
	})

	t.Run("path_inside_archive_base", func(t *testing.T) {
		// archivePathFor must produce a path INSIDE archiveBaseDir().
		path := archivePathFor(42, "2026-01-15 12:00:00")
		baseAbs, _ := filepath.Abs(archiveBaseDir())
		pathAbs, _ := filepath.Abs(path)
		if !strings.HasPrefix(pathAbs, baseAbs) {
			t.Errorf("archive path escaped base: %q (base %q)", pathAbs, baseAbs)
		}
		// Year/month/id segments
		if !strings.Contains(path, "2026") || !strings.Contains(path, "01") || !strings.Contains(path, "42.txt.gz") {
			t.Errorf("path malformed: %q", path)
		}
	})

	t.Run("nil_db_errors", func(t *testing.T) {
		err := dogTranscriptArchive(context.Background(), nil, &stubLogger{t: t})
		if err == nil {
			t.Fatal("expected error on nil db")
		}
	})

	t.Run("LoadArchivedBody_round_trip", func(t *testing.T) {
		oldHome := os.Getenv("HOME")
		t.Cleanup(func() { os.Setenv("HOME", oldHome) })
		os.Setenv("HOME", t.TempDir())
		path := archivePathFor(99, "2026-01-15 10:00:00")
		os.MkdirAll(filepath.Dir(path), 0700)
		f, _ := os.Create(path)
		gz := gzip.NewWriter(f)
		gz.Write([]byte("=== RESPONSE ===\nhello"))
		gz.Close()
		f.Close()

		body, err := LoadArchivedBody(99, "2026-01-15 10:00:00")
		if err != nil {
			t.Fatalf("LoadArchivedBody: %v", err)
		}
		if !strings.Contains(body, "hello") {
			t.Errorf("missing payload: %q", body)
		}

		// Sanity check the round-trip even on a multi-MB body
		path2 := archivePathFor(100, "2026-01-15 10:00:00")
		f2, _ := os.Create(path2)
		gz2 := gzip.NewWriter(f2)
		io.WriteString(gz2, strings.Repeat("X", 500*1024))
		gz2.Close()
		f2.Close()
		body2, err := LoadArchivedBody(100, "2026-01-15 10:00:00")
		if err != nil {
			t.Fatalf("LoadArchivedBody large: %v", err)
		}
		if len(body2) < 500*1024 {
			t.Errorf("large body underread: %d bytes", len(body2))
		}
	})
}

func TestSummariseTranscript(t *testing.T) {
	if got := summariseTranscript(""); !strings.Contains(got, "empty") {
		t.Errorf("empty got %q", got)
	}
	if got := summariseTranscript("only one line"); !strings.Contains(got, "only one line") {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 500)
	got := summariseTranscript(long)
	if len(got) >= 500 {
		t.Errorf("not truncated: len=%d", len(got))
	}
}
