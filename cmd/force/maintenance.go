package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"force-orchestrator/internal/agents"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

func runCleanup(db *sql.DB) {
	fmt.Println("Running cleanup...")

	// Prune git worktree metadata for all registered repos
	rows, err := db.Query(`SELECT DISTINCT local_path FROM Repositories`)
	if err == nil {
		for rows.Next() {
			var repoPath string
			rows.Scan(&repoPath)
			if out, pruneErr := igit.RunCmd(repoPath, "worktree", "prune"); pruneErr != nil {
				fmt.Printf("  worktree prune [%s]: ERROR %v\n", repoPath, pruneErr)
			} else {
				msg := strings.TrimSpace(out)
				if msg == "" {
					msg = "OK"
				}
				fmt.Printf("  worktree prune [%s]: %s\n", repoPath, msg)
			}
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
			agentRows.Scan(&e.agent, &e.repo, &e.path)
			if _, statErr := os.Stat(e.path); statErr != nil {
				staleAgents = append(staleAgents, e)
			}
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
	gitOut, gitErr := exec.Command("git", "--version").Output()
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
			repoRows.Scan(&name, &path)
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
			// Check it's a git repo
			_, gitRepoErr := exec.Command("git", "-C", path, "rev-parse", "--git-dir").Output()
			if gitRepoErr != nil {
				check(name, path+" — not a git repo", false)
			} else {
				check(name, path, true)
			}
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
		repoRows.Scan(&r.Name, &r.LocalPath, &r.Description)
		export.Repositories = append(export.Repositories, r)
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
		taskRows.Scan(&t.ID, &t.ParentID, &t.TargetRepo, &t.Type, &t.Status,
			&t.Payload, &t.ErrorLog, &t.RetryCount, &t.InfraFailures, &t.ConvoyID, &t.Checkpoint, &t.BranchName)
		export.Tasks = append(export.Tasks, t)
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
	memRows, err := db.Query(`SELECT repo, task_id, outcome, summary, IFNULL(files_changed,'') FROM FleetMemory ORDER BY id ASC`)
	if err == nil {
		for memRows.Next() {
			var m MemoryExport
			memRows.Scan(&m.Repo, &m.TaskID, &m.Outcome, &m.Summary, &m.FilesChanged)
			export.Memories = append(export.Memories, m)
		}
		memRows.Close()
	}

	// Open escalations — closed ones are transient, open ones matter for recovery
	escRows, err := db.Query(`SELECT task_id, severity, message, status FROM Escalations WHERE status IN ('Open','Acknowledged') ORDER BY id ASC`)
	if err == nil {
		for escRows.Next() {
			var e EscalationExport
			escRows.Scan(&e.TaskID, &e.Severity, &e.Message, &e.Status)
			export.Escalations = append(export.Escalations, e)
		}
		escRows.Close()
	}

	// Recent audit log — last 500 entries for incident context
	auditRows, err := db.Query(`SELECT actor, action, task_id, IFNULL(detail,''), created_at FROM AuditLog ORDER BY id DESC LIMIT 500`)
	if err == nil {
		for auditRows.Next() {
			var a AuditExport
			auditRows.Scan(&a.Actor, &a.Action, &a.TaskID, &a.Detail, &a.CreatedAt)
			export.AuditLog = append(export.AuditLog, a)
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
			store.StoreFleetMemory(db, m.Repo, m.TaskID, m.Outcome, m.Summary, m.FilesChanged)
		}
	}

	return count, nil
}

// pruneFleet deletes old completed/failed tasks, their history, closed escalations,
// and old audit log entries to keep the database small.
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
	}
	targets := []pruneTarget{
		{
			"old task history",
			fmt.Sprintf(`SELECT COUNT(*) FROM TaskHistory WHERE created_at < datetime('now', '%s')
				AND task_id IN (SELECT id FROM BountyBoard WHERE created_at < datetime('now', '%s'))`, since, since),
			fmt.Sprintf(`DELETE FROM TaskHistory WHERE created_at < datetime('now', '%s')
				AND task_id IN (SELECT id FROM BountyBoard WHERE created_at < datetime('now', '%s'))`, since, since),
		},
		{
			"orphaned task dependencies",
			`SELECT COUNT(*) FROM TaskDependencies WHERE task_id NOT IN (SELECT id FROM BountyBoard) OR depends_on NOT IN (SELECT id FROM BountyBoard)`,
			`DELETE FROM TaskDependencies WHERE task_id NOT IN (SELECT id FROM BountyBoard) OR depends_on NOT IN (SELECT id FROM BountyBoard)`,
		},
		{
			"old tasks",
			fmt.Sprintf(`SELECT COUNT(*) FROM BountyBoard WHERE status IN ('Completed', 'Failed') AND created_at < datetime('now', '%s')`, since),
			fmt.Sprintf(`DELETE FROM BountyBoard WHERE status IN ('Completed', 'Failed') AND created_at < datetime('now', '%s')`, since),
		},
		{
			"closed escalations",
			fmt.Sprintf(`SELECT COUNT(*) FROM Escalations WHERE status = 'Closed' AND created_at < datetime('now', '%s')`, since),
			fmt.Sprintf(`DELETE FROM Escalations WHERE status = 'Closed' AND created_at < datetime('now', '%s')`, since),
		},
		{
			"old audit log entries",
			fmt.Sprintf(`SELECT COUNT(*) FROM AuditLog WHERE created_at < datetime('now', '%s')`, since),
			fmt.Sprintf(`DELETE FROM AuditLog WHERE created_at < datetime('now', '%s')`, since),
		},
		{
			"read fleet mail",
			fmt.Sprintf(`SELECT COUNT(*) FROM Fleet_Mail WHERE read_at != '' AND created_at < datetime('now', '%s')`, since),
			fmt.Sprintf(`DELETE FROM Fleet_Mail WHERE read_at != '' AND created_at < datetime('now', '%s')`, since),
		},
		{
			"old fleet memories",
			fmt.Sprintf(`SELECT COUNT(*) FROM FleetMemory WHERE created_at < datetime('now', '%s')`, since),
			fmt.Sprintf(`DELETE FROM FleetMemory WHERE created_at < datetime('now', '%s')`, since),
		},
	}

	total := 0
	for _, t := range targets {
		if dryRun {
			var n int
			db.QueryRow(t.countQuery).Scan(&n)
			fmt.Printf("  Would delete %5d rows  — %s\n", n, t.label)
			total += n
		} else {
			res, err := db.Exec(t.deleteQuery)
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
		store.LogAudit(db, "operator", "prune", 0, fmt.Sprintf("pruned %d rows older than %d days", total, keepDays))
	}
	fmt.Printf("%sTotal: %d rows %s.\n", prefix, total, map[bool]string{true: "would be removed", false: "removed"}[dryRun])
}

