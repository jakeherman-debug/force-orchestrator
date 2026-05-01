// Package agents — D3 P6B.9 transcript archival housekeeping.
//
// Daily housekeeping dog. Transcripts > 30 days OR for convoys closed
// > 7 days get summarised to a 1-line blurb (kept in-row); bodies
// offloaded to ~/.force/transcripts/<year>/<month>/<id>.txt.gz.
// archived_at is stamped. Drill UI loads the offloaded body lazily
// when the operator clicks expand.
//
// Anti-cheat:
//   - File path uses a controlled template — operator-supplied paths
//     can never escape ~/.force/transcripts/.
//   - File contents pre-redacted at write time (Fix #10 / 6B.1).
//   - No deletes — archive is one-way until operator manually removes.
//   - Bounded: max maxArchivesPerRun records archived per dog tick;
//     remaining rows roll over to the next run. Keeps the DB
//     transaction window small even on large backlogs.

package agents

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

const maxArchivesPerRun = 1000

// archiveBaseDir is the on-disk root for offloaded transcript bodies.
// Returns ~/.force/transcripts (per the brief). The constant template
// guarantees no operator input can pivot the path.
func archiveBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Tests run with HOME unset sometimes — fall back to a temp
		// dir under the current cwd. Production always has HOME.
		return filepath.Join(".force", "transcripts")
	}
	return filepath.Join(home, ".force", "transcripts")
}

// archivePathFor returns the canonical disk path for transcript id +
// year + month. Path components are derived from the parsed
// call_started_at; an unparseable timestamp falls back to the current
// year/month so no row gets silently skipped.
func archivePathFor(id int64, callStartedAt string) string {
	t, err := time.Parse("2006-01-02 15:04:05", callStartedAt)
	if err != nil || t.IsZero() {
		t = time.Now().UTC()
	}
	return filepath.Join(
		archiveBaseDir(),
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%02d", int(t.Month())),
		fmt.Sprintf("%d.txt.gz", id),
	)
}

// dogTranscriptArchive runs the archival sweep. Selects up to
// maxArchivesPerRun candidate transcripts whose bodies are still in
// the row, gzips each body to disk, replaces the in-row body with a
// 1-line summary blurb, stamps archived_at. Errors propagate so the
// inquisitor cycle's per-dog failure path mails the operator.
func dogTranscriptArchive(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	if db == nil {
		return fmt.Errorf("transcript-archive: nil db")
	}

	// Selection: rows with archived_at == '' AND
	// (call_started_at < now-30d OR convoy closed > 7d ago).
	// The convoy-closure branch joins via BountyBoard.convoy_id →
	// Convoys.status; rows with task_id=0 (no task) miss this branch
	// and are archived purely on the 30-day age criterion.
	thirtyDaysAgo := time.Now().Add(-30 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05")

	rows, err := db.QueryContext(ctx,
		`SELECT t.id, t.call_started_at, t.system_prompt, t.user_prompt, t.response_text
		   FROM LLMCallTranscripts t
		  WHERE (t.archived_at = '' OR t.archived_at IS NULL)
		    AND (
		         t.call_started_at < ?
		      OR EXISTS (
		           SELECT 1
		             FROM BountyBoard b
		             JOIN Convoys c ON c.id = b.convoy_id
		            WHERE b.id = t.task_id
		              AND c.status IN ('Completed','Cancelled','Closed','Shipped')
		              AND IFNULL(c.shipped_at, c.created_at) < ?
		         )
		      )
		  LIMIT ?`,
		thirtyDaysAgo, sevenDaysAgo, maxArchivesPerRun,
	)
	if err != nil {
		return fmt.Errorf("transcript-archive: select candidates: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id        int64
		startedAt string
		sys, usr  string
		resp      string
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if scanErr := rows.Scan(&c.id, &c.startedAt, &c.sys, &c.usr, &c.resp); scanErr != nil {
			log.Printf("transcript_archive.go: scan candidate: %v", scanErr)
			continue
		}
		cands = append(cands, c)
	}
	if rErr := rows.Err(); rErr != nil {
		return fmt.Errorf("transcript-archive: rows iter: %w", rErr)
	}

	archived := 0
	for _, c := range cands {
		path := archivePathFor(c.id, c.startedAt)
		if err := writeArchivedBody(path, c.sys, c.usr, c.resp); err != nil {
			log.Printf("transcript_archive.go: write %s: %v", path, err)
			continue
		}
		// 1-line summary blurb replacing response_text — the in-row
		// row stays small. The deterministic synth here is the same
		// shape as 6B.12: live-Haiku swap is mechanical when the
		// claude-package signature finalises.
		summary := summariseTranscript(c.resp)
		_, err := db.ExecContext(ctx,
			`UPDATE LLMCallTranscripts
			    SET response_text = ?,
			        archived_at   = ?,
			        system_prompt = '',
			        user_prompt   = ''
			  WHERE id = ?`,
			summary, store.NowSQLite(), c.id,
		)
		if err != nil {
			log.Printf("transcript_archive.go: update %d: %v", c.id, err)
			continue
		}
		archived++
	}
	logger.Printf("Dog transcript-archive: archived %d transcripts (cap %d)", archived, maxArchivesPerRun)
	return nil
}

// writeArchivedBody gzips the prompt + response triple to path. The
// destination directory is created with restrictive 0700 perms so
// other users on a shared host can't read transcripts.
func writeArchivedBody(path, sys, usr, resp string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()

	// Body shape is plain text with delimiter banners so a human
	// reading the gz can scan it without a JSON parser.
	if _, err := gz.Write([]byte("=== SYSTEM PROMPT ===\n")); err != nil {
		return err
	}
	if _, err := gz.Write([]byte(sys)); err != nil {
		return err
	}
	if _, err := gz.Write([]byte("\n=== USER PROMPT ===\n")); err != nil {
		return err
	}
	if _, err := gz.Write([]byte(usr)); err != nil {
		return err
	}
	if _, err := gz.Write([]byte("\n=== RESPONSE ===\n")); err != nil {
		return err
	}
	if _, err := gz.Write([]byte(resp)); err != nil {
		return err
	}
	return nil
}

// LoadArchivedBody reads a previously-archived transcript body from
// disk. Used by the Drill UI's lazy-expand on archived rows.
func LoadArchivedBody(id int64, callStartedAt string) (string, error) {
	path := archivePathFor(id, callStartedAt)
	// Defence in depth: ensure the resolved path is still inside
	// the archive base — guards against any future caller passing
	// a malicious id/timestamp.
	cleaned, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	baseAbs, _ := filepath.Abs(archiveBaseDir())
	if !strings.HasPrefix(cleaned, baseAbs) {
		return "", fmt.Errorf("LoadArchivedBody: refused path outside archive base: %s", cleaned)
	}
	f, err := os.Open(cleaned)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, rErr := gz.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if rErr != nil {
			break
		}
	}
	return sb.String(), nil
}

// summariseTranscript produces a 1-line blurb from the response body.
// Deterministic — the live-Haiku swap is mechanical and slated.
func summariseTranscript(resp string) string {
	resp = strings.TrimSpace(resp)
	if resp == "" {
		return "[archived: empty response]"
	}
	// First line, capped at 200 chars.
	first := resp
	if i := strings.Index(first, "\n"); i > 0 {
		first = first[:i]
	}
	if len(first) > 200 {
		first = first[:200] + "…"
	}
	return "[archived] " + first
}
