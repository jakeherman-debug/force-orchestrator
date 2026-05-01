package main

import (
	"log"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"force-orchestrator/internal/agents"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// Fix #8e: ctx threads from main's signal-cancellation ctx.
func runCleanup(ctx context.Context, db *sql.DB) {
	fmt.Println("Running cleanup...")

	// Prune git worktree metadata for all registered repos
	rows, err := db.Query(`SELECT DISTINCT local_path FROM Repositories`)
	if err == nil {
		for rows.Next() {
			var repoPath string
			if err := rows.Scan(&repoPath); err != nil { fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err); continue }
			if out, pruneErr := igit.RunCmd(ctx, repoPath, "worktree", "prune"); pruneErr != nil {
				fmt.Printf("  worktree prune [%s]: ERROR %v\n", repoPath, pruneErr)
			} else {
				msg := strings.TrimSpace(out)
				if msg == "" {
					msg = "OK"
				}
				fmt.Printf("  worktree prune [%s]: %s\n", repoPath, msg)
			}
		}
		if rErr := rows.Err(); rErr != nil {
			log.Printf("maintenance.go:runCleanup: rows iter error: %v", rErr)
		}
		rows.Close()
	}

	// Remove Agents table entries where the worktree directory no longer exists on disk.
	// Drain the cursor before executing DELETEs — avoids deadlock on single-connection pool.
	type agentEntry struct{ agent, repo, path string }
	agentRows, err := db.Query(`SELECT agent_name, repo, worktree_path FROM Agents`)
	if err == nil {
		var staleAgents []agentEntry
		for agentRows.Next() {
			var e agentEntry
			if err := agentRows.Scan(&e.agent, &e.repo, &e.path); err != nil { fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err); continue }
			if _, statErr := os.Stat(e.path); statErr != nil {
				staleAgents = append(staleAgents, e)
			}
		}
		if rErr := agentRows.Err(); rErr != nil {
			log.Printf("maintenance.go:runCleanup: rows iter error: %v", rErr)
		}
		agentRows.Close()
		for _, e := range staleAgents {
			db.Exec(`DELETE FROM Agents WHERE agent_name = ? AND repo = ?`, e.agent, e.repo)
			fmt.Printf("  Removed stale agent entry: %s / %s\n", e.agent, e.repo)
		}
	}

	fmt.Println("Cleanup complete.")
}

func runDoctor(db *sql.DB, clean bool) {
	// D3 polish-pass iteration 2 (B4r): operator-invoked CLI subcommand,
	// no daemon ctx to thread. context.Background() is the canonical
	// shape for one-shot CLI ops; igit.LogAndRun records the op when
	// the holocron is wired (`force doctor` runs against an existing
	// DB) and silently degrades when not.
	ctx := context.Background()
	pass := 0
	warn := 0
	fail := 0
	fixed := 0

	check := func(label, detail string, ok bool) {
		status := "OK  "
		if !ok {
			status = "FAIL"
			fail++
		} else {
			pass++
		}
		fmt.Printf("  [%s] %-42s %s\n", status, label, detail)
	}
	advisory := func(label, detail string) {
		fmt.Printf("  [WARN] %-40s %s\n", label, detail)
		warn++
	}
	fix := func(label, action string) {
		fmt.Printf("  [FIX ] %-42s %s\n", label, action)
		fixed++
		warn-- // converted from advisory to fix, don't double-count
	}

	fmt.Println("=== Force Doctor ===")
	fmt.Println()
	fmt.Println("Binaries:")

	// git
	gitOut, gitErr := igit.LogAndRun(ctx, igit.OpContext{}, "doctor-git-version", "git", "--version")
	check("git binary", strings.TrimSpace(string(gitOut)), gitErr == nil)

	// claude CLI
	claudeOut, claudeErr := exec.Command("claude", "--version").Output()
	claudeDetail := strings.TrimSpace(string(claudeOut))
	if claudeErr != nil {
		claudeDetail = claudeErr.Error()
	}
	check("claude CLI", claudeDetail, claudeErr == nil)

	fmt.Println()
	fmt.Println("Database:")

	// DB integrity
	var integrityResult string
	db.QueryRow(`PRAGMA integrity_check`).Scan(&integrityResult)
	check("integrity_check", integrityResult, integrityResult == "ok")

	// WAL mode
	var journalMode string
	db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode)
	check("journal_mode=WAL", journalMode, strings.EqualFold(journalMode, "wal"))

	fmt.Println()
	fmt.Println("Repositories:")

	repoRows, err := db.Query(`SELECT name, local_path FROM Repositories ORDER BY name`)
	if err == nil {
		found := false
		for repoRows.Next() {
			found = true
			var name, path string
			if err := repoRows.Scan(&name, &path); err != nil { fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err); continue }
			// Check dir exists
			fi, statErr := os.Stat(path)
			if statErr != nil {
				check(name, path+" — NOT FOUND", false)
				continue
			}
			if !fi.IsDir() {
				check(name, path+" — not a directory", false)
				continue
			}
			// Check it's a git repo. Route through igit.LogAndRun (B4r).
			_, gitRepoErr := igit.LogAndRun(ctx,
				igit.OpContext{Repo: path},
				"doctor-rev-parse",
				"git", "-C", path, "rev-parse", "--git-dir")
			if gitRepoErr != nil {
				check(name, path+" — not a git repo", false)
			} else {
				check(name, path, true)
			}
		}
		if rErr := repoRows.Err(); rErr != nil {
			log.Printf("maintenance.go:runDoctor: rows iter error: %v", rErr)
		}
		repoRows.Close()
		if !found {
			advisory("no repos registered", "run: force add-repo <name> <path> <desc>")
		}
	}

	fmt.Println()
	fmt.Println("System state:")

	if agents.IsEstopped(db) {
		advisory("E-stop is ACTIVE", "run: force resume to re-enable agents")
	} else {
		check("E-stop", "inactive", true)
	}

	var pending, active, failed int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status = 'Pending'`).Scan(&pending)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status IN ('Locked', 'UnderCaptainReview', 'UnderReview')`).Scan(&active)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status = 'Failed'`).Scan(&failed)
	fmt.Printf("  [INFO] Tasks: %d pending, %d active, %d failed\n", pending, active, failed)

	// Check for permanently blocked tasks — depend on a Failed/Escalated/missing task
	var permBlocked int
	db.QueryRow(`
		SELECT COUNT(DISTINCT td.task_id) FROM TaskDependencies td
		JOIN BountyBoard b ON b.id = td.task_id
		WHERE b.status IN ('Pending','Planned')
		  AND (
		    NOT EXISTS (SELECT 1 FROM BountyBoard WHERE id = td.depends_on)
		    OR EXISTS (SELECT 1 FROM BountyBoard WHERE id = td.depends_on AND status IN ('Failed','Escalated'))
		  )`).Scan(&permBlocked)
	if permBlocked > 0 {
		if clean {
			db.Exec(`
				DELETE FROM TaskDependencies
				WHERE EXISTS (SELECT 1 FROM BountyBoard b WHERE b.id = task_id AND b.status IN ('Pending','Planned'))
				  AND (
				    NOT EXISTS (SELECT 1 FROM BountyBoard WHERE id = depends_on)
				    OR EXISTS (SELECT 1 FROM BountyBoard WHERE id = depends_on AND status IN ('Failed','Escalated'))
				  )`)
			advisory(fmt.Sprintf("%d permanently blocked task(s)", permBlocked), "")
			fix("permanently blocked tasks", fmt.Sprintf("removed bad dependency edges for %d task(s)", permBlocked))
		} else {
			advisory(fmt.Sprintf("%d permanently blocked task(s)", permBlocked),
				"dependency points to a Failed/Escalated/missing task — use force unblock <id> or --clean")
		}
	} else {
		check("no permanently blocked tasks", "", true)
	}

	// Check for tasks with a dependency on themselves (trivial cycle)
	var selfBlocked int
	db.QueryRow(`SELECT COUNT(*) FROM TaskDependencies WHERE task_id = depends_on`).Scan(&selfBlocked)
	if selfBlocked > 0 {
		if clean {
			db.Exec(`DELETE FROM TaskDependencies WHERE task_id = depends_on`)
			advisory(fmt.Sprintf("%d self-blocked task(s)", selfBlocked), "")
			fix("self-blocked tasks", fmt.Sprintf("removed %d self-dependency edge(s)", selfBlocked))
		} else {
			advisory(fmt.Sprintf("%d self-blocked task(s)", selfBlocked),
				"task depends on itself — use force unblock <id> or --clean")
		}
	}

	// Check daemon PID file
	if pidData, pidErr := os.ReadFile("fleet.pid"); pidErr == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if pid > 0 {
			proc, procErr := os.FindProcess(pid)
			if procErr == nil && proc.Signal(syscall.Signal(0)) == nil {
				check("daemon running", fmt.Sprintf("PID %d", pid), true)
			} else {
				if clean {
					os.Remove("fleet.pid")
					advisory("stale fleet.pid", "")
					fix("stale fleet.pid", fmt.Sprintf("removed stale PID file (PID %d)", pid))
				} else {
					advisory("stale fleet.pid", fmt.Sprintf("PID %d not running — delete fleet.pid or run with --clean", pid))
				}
			}
		}
	} else {
		fmt.Println("  [INFO] No fleet.pid — daemon is not running")
	}

	fmt.Println()
	if clean && fixed > 0 {
		fmt.Printf("Result: %d passed, %d warning(s), %d failed, %d fixed\n", pass, warn, fail, fixed)
	} else {
		fmt.Printf("Result: %d passed, %d warning(s), %d failed\n", pass, warn, fail)
	}
	if fail > 0 {
		os.Exit(1)
	}
}

// FleetExport is the JSON structure written by `force export`.
type FleetExport struct {
	ExportedAt   string             `json:"exported_at"`
	Repositories []RepoExport       `json:"repositories"`
	Tasks        []TaskExport       `json:"tasks"`
	Convoys      []store.Convoy     `json:"convoys"`
	Memories     []MemoryExport     `json:"memories,omitempty"`
	Escalations  []EscalationExport `json:"escalations,omitempty"`
	AuditLog     []AuditExport      `json:"audit_log,omitempty"`
}

type MemoryExport struct {
	Repo         string `json:"repo"`
	TaskID       int    `json:"task_id"`
	Outcome      string `json:"outcome"`
	Summary      string `json:"summary"`
	FilesChanged string `json:"files_changed,omitempty"`
	TopicTags    string `json:"topic_tags,omitempty"`
}

type EscalationExport struct {
	TaskID   int    `json:"task_id"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Status   string `json:"status"`
}

type AuditExport struct {
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	TaskID    int    `json:"task_id"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

type RepoExport struct {
	Name        string `json:"name"`
	LocalPath   string `json:"local_path"`
	Description string `json:"description"`
}

type TaskExport struct {
	ID            int    `json:"id"`
	ParentID      int    `json:"parent_id"`
	TargetRepo    string `json:"target_repo"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	Payload       string `json:"payload"`
	ErrorLog      string `json:"error_log,omitempty"`
	RetryCount    int    `json:"retry_count"`
	InfraFailures int    `json:"infra_failures"`
	ConvoyID      int    `json:"convoy_id,omitempty"`
	Checkpoint    string `json:"checkpoint,omitempty"`
	BranchName    string `json:"branch_name,omitempty"`
	BlockedBy     []int  `json:"blocked_by,omitempty"`
}

func exportFleet(db *sql.DB, outFile string) error {
	export := FleetExport{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Repos
	repoRows, err := db.Query(`SELECT name, local_path, description FROM Repositories ORDER BY name`)
	if err != nil {
		return fmt.Errorf("query repos: %w", err)
	}
	defer repoRows.Close()
	for repoRows.Next() {
		var r RepoExport
		if err := repoRows.Scan(&r.Name, &r.LocalPath, &r.Description); err != nil { fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err); continue }
		export.Repositories = append(export.Repositories, r)
	}
	if rErr := repoRows.Err(); rErr != nil {
		log.Printf("maintenance.go:exportFleet: rows iter error: %v", rErr)
	}
	repoRows.Close()

	// Tasks
	taskRows, err := db.Query(`
		SELECT id, parent_id, target_repo, type, status, payload,
		       IFNULL(error_log,''), retry_count, infra_failures, convoy_id, checkpoint, branch_name
		FROM BountyBoard ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("query tasks: %w", err)
	}
	defer taskRows.Close()
	for taskRows.Next() {
		var t TaskExport
		if err := taskRows.Scan(&t.ID, &t.ParentID, &t.TargetRepo, &t.Type, &t.Status, &t.Payload, &t.ErrorLog, &t.RetryCount, &t.InfraFailures, &t.ConvoyID, &t.Checkpoint, &t.BranchName); err != nil { fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err); continue }
		export.Tasks = append(export.Tasks, t)
	}
	if rErr := taskRows.Err(); rErr != nil {
		log.Printf("maintenance.go:exportFleet: rows iter error: %v", rErr)
	}
	taskRows.Close()

	// Populate dependencies after closing taskRows — SQLite's single connection
	// would deadlock if GetDependencies opened a new query while taskRows was open.
	for i := range export.Tasks {
		export.Tasks[i].BlockedBy = store.GetDependencies(db, export.Tasks[i].ID)
	}

	// Convoys
	export.Convoys = store.ListConvoys(db)

	// Fleet memories
	memRows, err := db.Query(`SELECT repo, task_id, outcome, summary, IFNULL(files_changed,''), IFNULL(topic_tags,'') FROM FleetMemory ORDER BY id ASC`)
	if err == nil {
		for memRows.Next() {
			var m MemoryExport
			if err := memRows.Scan(&m.Repo, &m.TaskID, &m.Outcome, &m.Summary, &m.FilesChanged, &m.TopicTags); err != nil { fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err); continue }
			export.Memories = append(export.Memories, m)
		}
		if rErr := memRows.Err(); rErr != nil {
			log.Printf("maintenance.go:exportFleet: rows iter error: %v", rErr)
		}
		memRows.Close()
	}

	// Open escalations — closed ones are transient, open ones matter for recovery
	escRows, err := db.Query(`SELECT task_id, severity, message, status FROM Escalations WHERE status IN ('Open','Acknowledged') ORDER BY id ASC`)
	if err == nil {
		for escRows.Next() {
			var e EscalationExport
			if err := escRows.Scan(&e.TaskID, &e.Severity, &e.Message, &e.Status); err != nil { fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err); continue }
			export.Escalations = append(export.Escalations, e)
		}
		if rErr := escRows.Err(); rErr != nil {
			log.Printf("maintenance.go:exportFleet: rows iter error: %v", rErr)
		}
		escRows.Close()
	}

	// Recent audit log — last 500 entries for incident context
	auditRows, err := db.Query(`SELECT actor, action, task_id, IFNULL(detail,''), created_at FROM AuditLog ORDER BY id DESC LIMIT 500`)
	if err == nil {
		for auditRows.Next() {
			var a AuditExport
			if err := auditRows.Scan(&a.Actor, &a.Action, &a.TaskID, &a.Detail, &a.CreatedAt); err != nil { fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err); continue }
			export.AuditLog = append(export.AuditLog, a)
		}
		if rErr := auditRows.Err(); rErr != nil {
			log.Printf("maintenance.go:exportFleet: rows iter error: %v", rErr)
		}
		auditRows.Close()
	}

	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(outFile, data, 0644)
}

// importFleet reads a JSON export file and inserts all Pending/Failed tasks as
// new Pending tasks. It does NOT import Completed tasks (they're done) or
// duplicate tasks that already exist by ID.
func importFleet(db *sql.DB, inFile string) (int, error) {
	data, err := os.ReadFile(inFile)
	if err != nil {
		return 0, fmt.Errorf("read file: %w", err)
	}
	var export FleetExport
	if err := json.Unmarshal(data, &export); err != nil {
		return 0, fmt.Errorf("parse JSON: %w", err)
	}

	// Import repos (idempotent via INSERT OR IGNORE)
	for _, r := range export.Repositories {
		db.Exec(`INSERT OR IGNORE INTO Repositories (name, local_path, description) VALUES (?, ?, ?)`,
			r.Name, r.LocalPath, r.Description)
	}

	// First pass: insert non-Completed tasks as new Pending entries, building old→new ID map.
	count := 0
	oldToNew := make(map[int]int)
	for _, t := range export.Tasks {
		if t.Status == "Completed" {
			continue
		}
		res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, created_at)
			VALUES (?, ?, ?, 'Pending', ?, ?, datetime('now'))`,
			t.ParentID, t.TargetRepo, t.Type, t.Payload, t.ConvoyID)
		if err != nil {
			continue
		}
		newID, _ := res.LastInsertId()
		oldToNew[t.ID] = int(newID)
		count++
	}

	// Second pass: restore dependency edges using the new IDs.
	for _, t := range export.Tasks {
		if t.Status == "Completed" {
			continue
		}
		newTaskID, ok := oldToNew[t.ID]
		if !ok {
			continue
		}
		for _, oldDepID := range t.BlockedBy {
			if newDepID, depOK := oldToNew[oldDepID]; depOK && newDepID > 0 {
				store.AddDependency(db, newTaskID, newDepID)
			}
		}
	}

	// Import fleet memories (idempotent — skip exact duplicates by repo+task_id+outcome+summary)
	for _, m := range export.Memories {
		var exists int
		db.QueryRow(`SELECT COUNT(*) FROM FleetMemory WHERE repo = ? AND task_id = ? AND outcome = ? AND summary = ?`,
			m.Repo, m.TaskID, m.Outcome, m.Summary).Scan(&exists)
		if exists == 0 {
			store.StoreFleetMemory(db, m.Repo, m.TaskID, m.Outcome, m.Summary, m.FilesChanged, m.TopicTags)
		}
	}

	return count, nil
}

// pruneFleet deletes old completed/failed tasks, their history, closed escalations,
// and old audit log entries to keep the database small.
//
// AUDIT-066 (Fix #8d): the time window is passed as a bound `?` placeholder
// (not fmt.Sprintf-interpolated). keepDays is operator-supplied via the
// CLI, so SQL injection would be trivial without the placeholder — a
// negative integer or an appended subquery would execute as written. The
// ? placeholder binds the value as data and eliminates the class.
func pruneFleet(db *sql.DB, keepDays int, dryRun bool) {
	since := fmt.Sprintf("-%d days", keepDays)
	prefix := ""
	if dryRun {
		prefix = "[DRY RUN] "
	}
	fmt.Printf("%sPruning data older than %d days...\n", prefix, keepDays)

	type pruneTarget struct {
		label       string
		countQuery  string
		deleteQuery string
		argCount    int // how many times to repeat `since` in the query's ? placeholders
	}
	targets := []pruneTarget{
		{
			"old task history",
			`SELECT COUNT(*) FROM TaskHistory WHERE created_at < datetime('now', ?)
				AND task_id IN (SELECT id FROM BountyBoard WHERE created_at < datetime('now', ?))`,
			`DELETE FROM TaskHistory WHERE created_at < datetime('now', ?)
				AND task_id IN (SELECT id FROM BountyBoard WHERE created_at < datetime('now', ?))`,
			2,
		},
		{
			"orphaned task dependencies",
			`SELECT COUNT(*) FROM TaskDependencies WHERE task_id NOT IN (SELECT id FROM BountyBoard) OR depends_on NOT IN (SELECT id FROM BountyBoard)`,
			`DELETE FROM TaskDependencies WHERE task_id NOT IN (SELECT id FROM BountyBoard) OR depends_on NOT IN (SELECT id FROM BountyBoard)`,
			0,
		},
		{
			"old tasks",
			`SELECT COUNT(*) FROM BountyBoard WHERE status IN ('Completed', 'Failed') AND created_at < datetime('now', ?)`,
			`DELETE FROM BountyBoard WHERE status IN ('Completed', 'Failed') AND created_at < datetime('now', ?)`,
			1,
		},
		{
			"closed escalations",
			`SELECT COUNT(*) FROM Escalations WHERE status = 'Closed' AND created_at < datetime('now', ?)`,
			`DELETE FROM Escalations WHERE status = 'Closed' AND created_at < datetime('now', ?)`,
			1,
		},
		{
			"old audit log entries",
			`SELECT COUNT(*) FROM AuditLog WHERE created_at < datetime('now', ?)`,
			`DELETE FROM AuditLog WHERE created_at < datetime('now', ?)`,
			1,
		},
		{
			"read fleet mail",
			`SELECT COUNT(*) FROM Fleet_Mail WHERE read_at != '' AND created_at < datetime('now', ?)`,
			`DELETE FROM Fleet_Mail WHERE read_at != '' AND created_at < datetime('now', ?)`,
			1,
		},
		{
			"old fleet memories",
			`SELECT COUNT(*) FROM FleetMemory WHERE created_at < datetime('now', ?)`,
			`DELETE FROM FleetMemory WHERE created_at < datetime('now', ?)`,
			1,
		},
	}

	total := 0
	for _, t := range targets {
		args := make([]any, t.argCount)
		for i := 0; i < t.argCount; i++ {
			args[i] = since
		}
		if dryRun {
			var n int
			if err := db.QueryRow(t.countQuery, args...).Scan(&n); err != nil {
				fmt.Printf("  ERROR counting %s: %v\n", t.label, err)
				continue
			}
			fmt.Printf("  Would delete %5d rows  — %s\n", n, t.label)
			total += n
		} else {
			res, err := db.Exec(t.deleteQuery, args...)
			if err != nil {
				fmt.Printf("  ERROR pruning %s: %v\n", t.label, err)
				continue
			}
			n, _ := res.RowsAffected()
			fmt.Printf("  Deleted %5d rows  — %s\n", n, t.label)
			total += int(n)
		}
	}

	if !dryRun {
		// Clean up orphaned FTS entries for deleted memories
		db.Exec(`DELETE FROM FleetMemory_fts WHERE rowid NOT IN (SELECT id FROM FleetMemory)`)
		db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		db.Exec(`VACUUM`)
	}
	fmt.Printf("%sTotal: %d rows %s.\n", prefix, total, map[bool]string{true: "would be removed", false: "removed"}[dryRun])
}

// purgeFilesystem removes all filesystem artifacts created by fleet runs:
// log files, rotated telemetry, agent git worktrees, agent branches, and the
// Dogs cooldown table. Safe to call while the daemon is stopped.
// Fix #8e: ctx threads from main's signal-cancellation ctx.
func purgeFilesystem(ctx context.Context, db *sql.DB) {
	// Log and telemetry files
	for _, name := range []string{"fleet.log", "holonet.jsonl", "fleet.pid"} {
		if err := os.Remove(name); err == nil {
			fmt.Printf("  Deleted %s\n", name)
		}
	}
	rotated, _ := filepath.Glob("holonet-*.jsonl")
	for _, f := range rotated {
		if err := os.Remove(f); err == nil {
			fmt.Printf("  Deleted %s\n", f)
		}
	}

	// Worktrees and agent branches for every registered repo
	rows, err := db.Query(`SELECT name, local_path FROM Repositories`)
	if err == nil {
		type repo struct{ name, path string }
		var repos []repo
		for rows.Next() {
			var r repo
			if err := rows.Scan(&r.name, &r.path); err != nil {
				fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
				continue
			}
			repos = append(repos, r)
		}
		if rErr := rows.Err(); rErr != nil {
			log.Printf("maintenance.go:purgeFilesystem: rows iter error: %v", rErr)
		}
		rows.Close()

		for _, r := range repos {
			worktreeBase := filepath.Join(filepath.Dir(r.path), ".force-worktrees", filepath.Base(r.path))
			entries, readErr := os.ReadDir(worktreeBase)
			if readErr == nil {
				for _, e := range entries {
					wtPath := filepath.Join(worktreeBase, e.Name())
					out, rmErr := igit.RunCmd(ctx, r.path, "worktree", "remove", "--force", wtPath)
					if rmErr == nil {
						fmt.Printf("  Removed worktree %s/%s\n", r.name, e.Name())
					} else if !strings.Contains(out, "not a working tree") {
						// Directory exists but git doesn't know about it — remove directly
						os.RemoveAll(wtPath)
						fmt.Printf("  Removed orphaned worktree dir %s/%s\n", r.name, e.Name())
					}
				}
				// Prune any remaining stale git refs
				igit.RunCmd(ctx, r.path, "worktree", "prune")
				// Remove the now-empty worktree base dir
				os.Remove(worktreeBase)
			}

			// Delete all agent/* branches.
			// D3 polish-pass iteration 2 (B4r): route through igit.LogAndRun
			// so the purge ops are recorded in GitOperationLog (Pattern P32).
			branchOut, branchErr := igit.LogAndRun(ctx,
				igit.OpContext{Repo: r.path},
				"maintenance-purge-list-branches",
				"git", "-C", r.path, "for-each-ref",
				"--format=%(refname:short)", "refs/heads/agent/")
			if branchErr == nil {
				deleted := 0
				for _, branch := range strings.Split(strings.TrimSpace(string(branchOut)), "\n") {
					branch = strings.TrimSpace(branch)
					if branch == "" {
						continue
					}
					if _, delErr := igit.LogAndRun(ctx,
						igit.OpContext{Repo: r.path},
						"maintenance-purge-branch-D",
						"git", "-C", r.path, "branch", "-D", branch); delErr == nil {
						deleted++
					}
				}
				if deleted > 0 {
					fmt.Printf("  Deleted %d agent branch(es) in %s\n", deleted, r.name)
				}
			}
		}
	}

	// Reset dog cooldown timers
	if res, err := db.Exec(`DELETE FROM Dogs`); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			fmt.Printf("  Cleared %d dog cooldown timer(s)\n", n)
		}
	}
}

// cmdPurge cleans all filesystem run artifacts and dog timers without touching
// task data in the database.
// Fix #8e: ctx threads from main's signal-cancellation ctx.
func cmdPurge(ctx context.Context, db *sql.DB, confirmed bool) {
	// Always print the warning regardless of --confirm (defense-in-depth).
	fmt.Fprintln(os.Stderr, "WARNING: This will permanently delete:")
	fmt.Fprintln(os.Stderr, "  - fleet.log and holonet.jsonl log files")
	fmt.Fprintln(os.Stderr, "  - All agent worktrees (from disk)")
	fmt.Fprintln(os.Stderr, "  - All agent branches in registered repositories")
	fmt.Fprintln(os.Stderr, "  - Dog cooldown timers")
	fmt.Fprintln(os.Stderr, "Task data in the database is NOT affected.")
	fmt.Fprintln(os.Stderr, "This action is IRREVERSIBLE.")
	if !confirmed {
		fmt.Fprint(os.Stderr, "\nType DELETE to confirm, or press Ctrl-C to abort: ")
		var input string
		fmt.Scanln(&input)
		if input != "DELETE" {
			fmt.Fprintln(os.Stderr, "Aborted.")
			os.Exit(1)
		}
	}
	if _, alive := readDaemonPID(); alive {
		fmt.Fprintln(os.Stderr, "Daemon is running — stop it first with 'force estop' then kill the daemon process.")
		os.Exit(1)
	}
	fmt.Println("Purging filesystem artifacts...")
	purgeFilesystem(ctx, db)
	fmt.Println("Done.")
}

// cmdHardReset wipes all task data from the database, purges filesystem artifacts,
// and resets the fleet to a factory-fresh state. Repositories and SystemConfig are
// preserved unless --purge-repos is passed.
// Fix #8e: ctx threads from main's signal-cancellation ctx.
func cmdHardReset(ctx context.Context, db *sql.DB, confirmed, purgeRepos bool) {
	// Always print the warning regardless of --confirm (defense-in-depth).
	fmt.Fprintln(os.Stderr, "WARNING: This permanently destroys ALL fleet state:")
	fmt.Fprintln(os.Stderr, "  - All task data, history, and dependencies")
	fmt.Fprintln(os.Stderr, "  - Fleet memories, mail, and escalations")
	fmt.Fprintln(os.Stderr, "  - Audit log")
	fmt.Fprintln(os.Stderr, "  - All agent worktrees and branches")
	fmt.Fprintln(os.Stderr, "  - Log files (fleet.log, holonet.jsonl)")
	if purgeRepos {
		fmt.Fprintln(os.Stderr, "  - Repositories and system config (--purge-repos)")
	} else {
		fmt.Fprintln(os.Stderr, "  Repositories and system config will be preserved.")
	}
	fmt.Fprintln(os.Stderr, "This action is IRREVERSIBLE.")
	if !confirmed {
		fmt.Fprint(os.Stderr, "\nType DELETE to confirm, or press Ctrl-C to abort: ")
		var input string
		fmt.Scanln(&input)
		if input != "DELETE" {
			fmt.Fprintln(os.Stderr, "Aborted.")
			os.Exit(1)
		}
	}
	if _, alive := readDaemonPID(); alive {
		fmt.Fprintln(os.Stderr, "Daemon is running — stop it first with 'force estop' then kill the daemon process.")
		os.Exit(1)
	}

	fmt.Println("Hard reset: purging filesystem artifacts...")
	purgeFilesystem(ctx, db)

	fmt.Println("Hard reset: wiping database...")
	tables := []string{
		"TaskHistory", "TaskDependencies", "BountyBoard",
		"Convoys", "Agents", "Escalations", "Fleet_Mail",
		"AuditLog", "FleetMemory",
	}
	if purgeRepos {
		tables = append(tables, "Repositories", "SystemConfig")
	}
	for _, t := range tables {
		res, err := db.Exec("DELETE FROM " + t)
		if err != nil {
			fmt.Printf("  ERROR clearing %s: %v\n", t, err)
			continue
		}
		n, _ := res.RowsAffected()
		fmt.Printf("  Cleared %5d row(s) — %s\n", n, t)
	}

	// Rebuild FTS from scratch (FleetMemory is now empty)
	db.Exec(`DELETE FROM FleetMemory_fts WHERE rowid NOT IN (SELECT id FROM FleetMemory)`)

	// Reset autoincrement counters so IDs restart from 1
	resetTables := []string{"BountyBoard", "Convoys", "TaskHistory", "Escalations", "Fleet_Mail", "AuditLog", "FleetMemory"}
	for _, t := range resetTables {
		db.Exec(`DELETE FROM sqlite_sequence WHERE name = ?`, t)
	}

	db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	db.Exec(`VACUUM`)
	fmt.Println("Hard reset complete — fleet is ready for fresh use.")
}

