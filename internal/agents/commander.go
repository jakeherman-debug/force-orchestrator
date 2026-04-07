package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
	"force-orchestrator/internal/util"
)

// findPlanCycle detects circular dependencies in a task plan using DFS with grey/black coloring.
// Returns the tempID of the first task found in a cycle, or 0 if the plan is acyclic.
func findPlanCycle(tasks []store.TaskPlan) int {
	// Build adjacency map: tempID → []dep tempIDs
	deps := make(map[int][]int, len(tasks))
	for _, t := range tasks {
		deps[t.TempID] = t.BlockedBy
	}

	const white, grey, black = 0, 1, 2
	color := make(map[int]int, len(tasks))

	var dfs func(id int) int
	dfs = func(id int) int {
		color[id] = grey
		for _, dep := range deps[id] {
			if color[dep] == grey {
				return dep // cycle: dep is on the current DFS path
			}
			if color[dep] == white {
				if cid := dfs(dep); cid != 0 {
					return cid
				}
			}
		}
		color[id] = black
		return 0
	}

	for _, t := range tasks {
		if color[t.TempID] == white {
			if cid := dfs(t.TempID); cid != 0 {
				return cid
			}
		}
	}
	return 0
}

// loadKnownRepos caches the registered repo names within a Commander cycle
// to avoid redundant DB queries during plan validation.
func loadKnownRepos(db *sql.DB) map[string]bool {
	repos := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM Repositories`)
	if err != nil {
		return repos
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			log.Printf("loadKnownRepos: scan error: %v", err)
			return repos
		}
		repos[name] = true
	}
	return repos
}

// readFilePreview reads up to maxLines lines from a file, returning empty string if unavailable.
func readFilePreview(path string, maxLines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return strings.Join(lines, "\n")
}

// loadRepoContext queries registered repositories, builds formatted context entries
// with description and README preview, and returns them as a list.
// Returns an error if the DB query fails or no repositories are registered.
func loadRepoContext(db *sql.DB, logger *log.Logger) ([]string, error) {
	rows, err := db.Query(`SELECT name, local_path, description FROM Repositories`)
	if err != nil {
		return nil, fmt.Errorf("failed to query repositories: %v", err)
	}
	defer rows.Close()
	var repoList []string
	for rows.Next() {
		var name, localPath, desc string
		if scanErr := rows.Scan(&name, &localPath, &desc); scanErr != nil {
			logger.Printf("loadRepoContext: scan error: %v", scanErr)
			continue
		}
		entry := fmt.Sprintf("### %s\n%s", name, desc)
		for _, candidate := range []string{"README.md", "readme.md", "README"} {
			preview := readFilePreview(filepath.Join(localPath, candidate), 60)
			if preview != "" {
				entry += fmt.Sprintf("\nREADME (first 60 lines):\n%s", preview)
				break
			}
		}
		repoList = append(repoList, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository scan error: %v", err)
	}
	if len(repoList) == 0 {
		return nil, fmt.Errorf("no repositories registered. Run: force add-repo <name> <path> <desc>")
	}
	return repoList, nil
}

// validateTaskPlan checks structural validity of a decoded task plan: non-empty repo,
// known repo, valid blocked_by references, and absence of dependency cycles.
// Returns a descriptive error on the first violation found.
func validateTaskPlan(tasks []store.TaskPlan, knownRepos map[string]bool) error {
	tempIDs := make(map[int]bool, len(tasks))
	for _, t := range tasks {
		tempIDs[t.TempID] = true
	}
	for _, t := range tasks {
		if t.Repo == "" {
			return fmt.Errorf("task %d has no repo assigned", t.TempID)
		}
		if !knownRepos[t.Repo] {
			return fmt.Errorf("task %d references unknown repo '%s' — register it with: force add-repo", t.TempID, t.Repo)
		}
		for _, depID := range t.BlockedBy {
			if depID != 0 && !tempIDs[depID] {
				return fmt.Errorf("task %d has invalid blocked_by=%v (no such task in plan)", t.TempID, t.BlockedBy)
			}
		}
	}
	if cycleID := findPlanCycle(tasks); cycleID != 0 {
		return fmt.Errorf("circular dependency detected at task %d — tasks cannot block each other in a cycle", cycleID)
	}
	return nil
}

// insertConvoyAndTasks removes stale subtasks from a prior decomposition attempt, then
// inserts the new task set and their dependency edges in a single database transaction.
// Any failure rolls back entirely, preventing partial state.
// Returns the mapping of plan tempID → real DB task ID.
func insertConvoyAndTasks(db *sql.DB, tasks []store.TaskPlan, bounty *store.Bounty, convoyID int) (map[int]int, error) {
	// Parse [PLAN_ONLY] prefix and extract the goal context from the payload.
	rawGoal := bounty.Payload
	planOnly := strings.HasPrefix(rawGoal, "[PLAN_ONLY]\n")
	if planOnly {
		rawGoal = strings.TrimPrefix(rawGoal, "[PLAN_ONLY]\n")
	}
	if strings.HasPrefix(rawGoal, "[GOAL: ") {
		if end := strings.Index(rawGoal, "]\n\n"); end != -1 {
			rawGoal = rawGoal[len("[GOAL: "):end]
		}
	}
	goalPrefix := fmt.Sprintf("[GOAL: %s]\n\n", rawGoal)
	initialStatus := "Pending"
	if planOnly {
		initialStatus = "Planned"
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %v", err)
	}

	if _, err := tx.Exec(`DELETE FROM BountyBoard WHERE parent_id = ? AND status IN ('Pending', 'Planned')`, bounty.ID); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("failed to delete stale subtasks: %v", err)
	}

	idMapping := make(map[int]int, len(tasks))
	for _, t := range tasks {
		realID, err := store.AddConvoyTaskTx(tx, bounty.ID, t.Repo, goalPrefix+t.Task, convoyID, bounty.Priority, initialStatus)
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to insert task %d: %v", t.TempID, err)
		}
		idMapping[t.TempID] = realID
	}

	for _, t := range tasks {
		realTaskID := idMapping[t.TempID]
		for _, depTempID := range t.BlockedBy {
			if realDepID, ok := idMapping[depTempID]; ok && realDepID > 0 {
				if err := store.AddDependencyTx(tx, realTaskID, realDepID); err != nil {
					tx.Rollback()
					return nil, fmt.Errorf("failed to add dependency: %v", err)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit task insertion: %v", err)
	}
	return idMapping, nil
}

func SpawnCommander(db *sql.DB, name string) {
	agentName := name
	logger := NewLogger(name)
	logger.Printf("Commander starting up")

	for {
		// Hard stop — operator activated e-stop
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		// Handle both Feature requests and Decompose escalations from Astromechs
		bounty, claimed := store.ClaimBounty(db, "Feature", agentName)
		if !claimed {
			bounty, claimed = store.ClaimBounty(db, "Decompose", agentName)
		}
		if !claimed {
			time.Sleep(time.Duration(1500+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		runCommanderTask(db, agentName, bounty, logger)
	}
}

// runCommanderTask decomposes a single Feature or Decompose bounty into CodeEdit subtasks.
func runCommanderTask(db *sql.DB, agentName string, bounty *store.Bounty, logger *log.Logger) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Claimed task %d (%s): %s", sessionID, bounty.ID, bounty.Type, util.TruncateStr(bounty.Payload, 80))
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		SessionID: sessionID, Agent: agentName, TaskID: bounty.ID,
		EventType: "task_decomposing",
		Payload:   map[string]any{"type": bounty.Type, "payload_preview": util.TruncateStr(bounty.Payload, 120)},
	})

	// Load repo context — fail fast if no repos are registered.
	repoList, err := loadRepoContext(db, logger)
	if err != nil {
		store.FailBounty(db, bounty.ID, "Commander Err: "+err.Error())
		logger.Printf("Task %d FAILED: %v", bounty.ID, err)
		return
	}
	repoContext := strings.Join(repoList, "\n\n")

	directive := LoadDirective("commander")
	directiveSection := ""
	if directive != "" {
		directiveSection = fmt.Sprintf("\n\nOPERATOR DIRECTIVE:\n%s\n", directive)
	}

	// Read Commander's inbox
	inboxContext := buildInboxContext(db, agentName, "commander", bounty.ID, logger)

	systemPrompt := fmt.Sprintf(`You are Commander-Cody, chief architect of the Galactic Fleet's software operations.
Your job is to receive a high-level feature request and decompose it into precise, self-contained missions
for the Astromech droid units. Each mission must be completable in a single focused coding session.
%s
AVAILABLE REPOSITORIES:
%s

RESEARCH TOOLS:
You have access to Jira, Confluence, and Glean. Use them before decomposing:
- If the request references a Jira ticket ID (e.g. PROJ-123), look it up to read the full description,
  acceptance criteria, and linked tickets before planning.
- Search Confluence or Glean for relevant design docs, ADRs, or API specs that affect how the work
  should be broken up.
- Only look up what is directly relevant — do not over-research.

YOUR JOB:
Break the request into a list of concrete, self-contained tasks. Each task must target exactly one repository.
Tasks can be ANY kind of change: creating files, modifying code, deleting files, reverting changes, fixing bugs, etc.

CRITICAL RULE — DESCRIBE DESIRED STATE, NOT GIT COMMANDS:
Each task description must describe WHAT the repo should look like when done, not HOW to do it with git.
- WRONG: "Amend the last commit to fix the message"
- WRONG: "Revert the commit that added health_check.sh"
- RIGHT: "Delete health_check.sh — this file should not exist in the repo"
- RIGHT: "Update health_check.sh to echo 'Fleet online' instead of 'Galactic Fleet is operational'"
The worker agents handle all git mechanics. You only describe the desired file state.

DEPENDENCY RULES:
- If task B cannot begin until task A is complete, include A's "id" in task B's "blocked_by" array.
- A task may depend on multiple tasks: "blocked_by": [1, 2] means wait for both.
- If a task has no dependencies, set "blocked_by" to an empty array [].
- "blocked_by" elements must reference an "id" from within this same response.
- Number tasks starting from 1, incrementing by 1.

OUTPUT FORMAT:
Respond with ONLY a raw JSON array — no explanation, no markdown, no code fences.

EXAMPLE:
[
  {"id": 1, "repo": "api-server", "task": "Add POST /users endpoint with email/password validation", "blocked_by": []},
  {"id": 2, "repo": "frontend", "task": "Add registration form that calls POST /users", "blocked_by": [1]}
]`, directiveSection, repoContext)

	userPrompt := bounty.Payload + inboxContext
	fullPrompt := fmt.Sprintf("SYSTEM INSTRUCTIONS:\n%s\n\nUSER PROMPT:\n%s", systemPrompt, userPrompt)
	cmdTimeout := claude.CommanderTimeoutForAttempt(bounty.InfraFailures)
	logger.Printf("[%s] Task %d: timeout %v (infra_failures=%d)", sessionID, bounty.ID, cmdTimeout, bounty.InfraFailures)
	taskLogPath := fmt.Sprintf("fleet-task-%d.log", bounty.ID)
	taskLogFile, _ := os.Create(taskLogPath)
	var taskWriter io.Writer = io.Discard
	if taskLogFile != nil {
		taskWriter = taskLogFile
	}

	rawOut, err := claude.RunCLIStreaming(fullPrompt, claude.CommanderTools, "", 10, cmdTimeout, taskWriter)

	if taskLogFile != nil {
		taskLogFile.Close()
		os.Remove(taskLogPath)
	}

	if err != nil {
		msg := fmt.Sprintf("Claude CLI Err: %v", err)
		// On timeout, check if Claude produced partial output — if so, it was making progress.
		if strings.Contains(err.Error(), "timed out") && len(strings.TrimSpace(rawOut)) > 200 {
			logger.Printf("Task %d: timed out but Claude was making progress (%d chars of output)", bounty.ID, len(rawOut))
			logger.Printf("Task %d: partial output preview: %.400s", bounty.ID, rawOut)
		}
		// Record history even on failure so token costs (if any) appear in force costs.
		histID := store.RecordTaskHistory(db, bounty.ID, agentName, sessionID, rawOut, "Failed")
		if tokIn, tokOut := claude.ParseTokenUsage(rawOut); tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		logger.Printf("Task %d: infra failure — %s", bounty.ID, msg)
		handleInfraFailure(db, agentName, "commander", bounty, sessionID, msg, "Pending", false, logger)
		return
	}
	response := rawOut

	cleanJSON := claude.ExtractJSON(response)

	var tasks []store.TaskPlan
	if err := json.Unmarshal([]byte(cleanJSON), &tasks); err != nil {
		msg := fmt.Sprintf("JSON Parse Err: %v | Raw output: %.500s", err, cleanJSON)
		logger.Printf("Task %d: Commander JSON parse failed — %s", bounty.ID, msg)
		handleInfraFailure(db, agentName, "commander", bounty, sessionID, msg, "Pending", false, logger)
		return
	}

	if len(tasks) == 0 {
		store.FailBounty(db, bounty.ID, "Commander Err: Claude returned an empty task list")
		return
	}

	// Validate the plan structure before any database writes.
	knownRepos := loadKnownRepos(db)
	if err := validateTaskPlan(tasks, knownRepos); err != nil {
		store.FailBounty(db, bounty.ID, "Commander Err: "+err.Error())
		return
	}

	// Create a convoy to track this feature's subtasks as a group.
	convoyPreview := strings.ReplaceAll(bounty.Payload, "\n", " ")
	if len(convoyPreview) > 50 {
		convoyPreview = convoyPreview[:50]
	}
	convoyName := fmt.Sprintf("[%d] %s", bounty.ID, convoyPreview)
	convoyID, convoyErr := store.CreateConvoy(db, convoyName)
	if convoyErr != nil {
		// Name collision on re-plan (UNIQUE constraint) — retry with a versioned suffix.
		for i := 2; i <= 10; i++ {
			convoyID, convoyErr = store.CreateConvoy(db, fmt.Sprintf("%s (re-plan %d)", convoyName, i))
			if convoyErr == nil {
				break
			}
		}
		if convoyErr != nil {
			store.FailBounty(db, bounty.ID, fmt.Sprintf("DB Err: could not create convoy after retries: %v", convoyErr))
			return
		}
	}
	store.SetConvoyCoordinated(db, convoyID)

	// Insert subtasks and dependencies in a single atomic transaction.
	idMapping, err := insertConvoyAndTasks(db, tasks, bounty, convoyID)
	if err != nil {
		store.FailBounty(db, bounty.ID, "DB Err: "+err.Error())
		return
	}

	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	histID := store.RecordTaskHistory(db, bounty.ID, agentName, sessionID, response, "Completed")
	if tokIn, tokOut := claude.ParseTokenUsage(response); tokIn > 0 || tokOut > 0 {
		store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
	}
	logger.Printf("Task %d: decomposed into %d subtask(s), convoy %d", bounty.ID, len(tasks), convoyID)
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		SessionID: sessionID, Agent: agentName, TaskID: bounty.ID,
		EventType: "task_decomposed",
		Payload:   map[string]any{"subtask_count": len(tasks), "convoy_id": convoyID},
	})

	// Notify operator
	var taskLines []string
	for _, t := range tasks {
		line := fmt.Sprintf("  #%d [%s] %s", idMapping[t.TempID], t.Repo, util.TruncateStr(t.Task, 80))
		if len(t.BlockedBy) > 0 {
			var afterIDs []string
			for _, depTempID := range t.BlockedBy {
				if realID, ok := idMapping[depTempID]; ok {
					afterIDs = append(afterIDs, fmt.Sprintf("#%d", realID))
				}
			}
			if len(afterIDs) > 0 {
				line += fmt.Sprintf(" (after %s)", strings.Join(afterIDs, ", "))
			}
		}
		taskLines = append(taskLines, line)
	}
	store.SendMail(db, agentName, "operator",
		fmt.Sprintf("[DECOMPOSED] Feature #%d → %d task(s)", bounty.ID, len(tasks)),
		fmt.Sprintf("Feature request #%d has been broken into %d task(s) in convoy #%d:\n\n%s\n\nOriginal request:\n%s",
			bounty.ID, len(tasks), convoyID, strings.Join(taskLines, "\n"), util.TruncateStr(bounty.Payload, 500)),
		bounty.ID, store.MailTypeInfo)
}
