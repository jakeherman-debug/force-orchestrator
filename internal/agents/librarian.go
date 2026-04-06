package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// writeMemoryPayload is the structured payload for WriteMemory tasks.
type writeMemoryPayload struct {
	Description    string   `json:"description"`     // task description, truncated to 800 chars
	FilesChanged   []string `json:"files_changed"`   // list of files modified
	CouncilFeedback string  `json:"council_feedback"` // approval feedback from council
	Diff           string   `json:"diff"`             // diff of changes, truncated to 4000 chars
	ParentTaskID   int64    `json:"parent_task_id"`  // ID of the task that was completed
}

const librarianSystemPrompt = `You are the Fleet Librarian — a specialist knowledge-curation agent in the Galactic Fleet.

Your job is to write a concise, high-quality memory nugget about a completed task so that future agents can learn from it.

# OUTPUT REQUIREMENTS
Write exactly 2-4 sentences that cover:
1. What was built or fixed (be specific — name the function, file, or system component)
2. What was non-obvious or tricky about the implementation
3. Any patterns, gotchas, or pitfalls that are NOT obvious from reading the code alone

# IMPORTANT
- Do NOT describe infrastructure failures (timeouts, worktree errors, network issues) as "tricky" — focus on the code logic.
- Do NOT mention attempt counts or rejection history — only the final solution.
- Do NOT include meta-commentary about the task process.
- Write in plain prose — no bullet points, no headings, no markdown.
- Be specific enough that a future agent reading this can act on it without looking at the code.`

func SpawnLibrarian(db *sql.DB, name string) {
	logger := NewLogger(name)
	logger.Printf("Librarian %s starting up", name)

	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		bounty, claimed := store.ClaimBounty(db, "WriteMemory", name)
		if !claimed {
			time.Sleep(time.Duration(2000+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		runLibrarianTask(db, name, bounty, logger)
	}
}

func runLibrarianTask(db *sql.DB, name string, bounty *store.Bounty, logger *log.Logger) {
	logger.Printf("Claimed WriteMemory #%d: %s", bounty.ID, util.TruncateStr(bounty.Payload, 80))

	// Parse the structured payload.
	var payload writeMemoryPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		// Fallback: treat the raw payload as the description.
		payload.Description = util.TruncateStr(bounty.Payload, 800)
	}

	parentTaskID := int(payload.ParentTaskID)

	// Collect rejection history for this parent task.
	mails := store.ReadInboxForAgent(db, name, "librarian", parentTaskID)
	priorAttemptsSection := formatLibrarianMailContext(mails)

	// Build the user prompt with all available context.
	var userParts []string

	userParts = append(userParts, fmt.Sprintf("TASK DESCRIPTION:\n%s", payload.Description))

	if len(payload.FilesChanged) > 0 {
		userParts = append(userParts, fmt.Sprintf("FILES CHANGED:\n%s", strings.Join(payload.FilesChanged, "\n")))
	}

	if payload.CouncilFeedback != "" {
		userParts = append(userParts, fmt.Sprintf("COUNCIL APPROVAL FEEDBACK:\n%s", payload.CouncilFeedback))
	}

	if payload.Diff != "" {
		userParts = append(userParts, fmt.Sprintf("DIFF:\n%s", payload.Diff))
	}

	if priorAttemptsSection != "" {
		userParts = append(userParts, priorAttemptsSection)
	}

	userPrompt := strings.Join(userParts, "\n\n")

	// Call Claude to generate the memory nugget (no tools, 1 turn).
	rawOut, err := claude.AskClaudeCLI(librarianSystemPrompt, userPrompt, "", 1)

	var summary string
	if err != nil || strings.TrimSpace(rawOut) == "" {
		logger.Printf("Task #%d: Claude error (%v), using fallback summary", bounty.ID, err)
		summary = fmt.Sprintf("Task: %s", payload.Description)
	} else {
		summary = strings.TrimSpace(rawOut)
	}

	// Store the memory, attributed to the parent task.
	filesChanged := strings.Join(payload.FilesChanged, ", ")
	store.StoreFleetMemory(db, bounty.TargetRepo, parentTaskID, "success", summary, filesChanged)

	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	logger.Printf("Task #%d: memory written for parent task #%d (%d chars)", bounty.ID, parentTaskID, len(summary))
}

// formatLibrarianMailContext formats rejection-history mail into a prompt section.
func formatLibrarianMailContext(mails []store.FleetMail) string {
	var feedback []store.FleetMail
	for _, m := range mails {
		if m.MessageType == store.MailTypeFeedback {
			feedback = append(feedback, m)
		}
	}
	if len(feedback) == 0 {
		return ""
	}
	var lines []string
	for _, m := range feedback {
		lines = append(lines, fmt.Sprintf("- [%s] %s", m.Subject, m.Body))
	}
	return "PRIOR REJECTION CONTEXT (use to understand what approaches did NOT work):\n" + strings.Join(lines, "\n")
}
