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

// writeMemoryPayload is the JSON structure placed in WriteMemory bounty payloads by jedi_council.
type writeMemoryPayload struct {
	Task     string `json:"task"`
	Files    string `json:"files"`
	Feedback string `json:"feedback"`
	Diff     string `json:"diff"`
	Repo     string `json:"repo"`
}

func SpawnLibrarian(db *sql.DB, name string) {
	logger := NewLogger(name)
	logger.Printf("Librarian %s coming online", name)

	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		bounty, claimed := store.ClaimBounty(db, "WriteMemory", name)
		if !claimed {
			time.Sleep(time.Duration(3000+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		runLibrarianTask(db, name, bounty, logger)
	}
}

func runLibrarianTask(db *sql.DB, name string, bounty *store.Bounty, logger *log.Logger) {
	logger.Printf("Librarian claimed WriteMemory #%d", bounty.ID)

	// Parse the structured payload from jedi_council.
	var payload writeMemoryPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		// Fallback: treat raw payload as task description.
		payload.Task = bounty.Payload
	}

	// parentID is the original task (CodeEdit) that this WriteMemory was spawned from.
	parentID := bounty.ParentID
	if parentID == 0 {
		parentID = bounty.ID
	}

	// Collect any rejection feedback mailed to "librarian" for the original task.
	priorMail := store.ReadInboxForAgent(db, name, "librarian", parentID)
	priorContext := formatLibrarianMailContext(priorMail)

	userPrompt := fmt.Sprintf(
		"TASK DESCRIPTION:\n%s\n\nFILES CHANGED:\n%s\n\nCOUNCIL FEEDBACK:\n%s\n\nDIFF:\n%s",
		payload.Task,
		payload.Files,
		payload.Feedback,
		payload.Diff,
	)
	if priorContext != "" {
		userPrompt += "\n\n" + priorContext
	}

	rawOut, err := claude.AskClaudeCLI(librarianSystemPrompt, userPrompt, "", 1)
	var summary string
	if err != nil {
		logger.Printf("WriteMemory #%d: Claude failed (%v) — using fallback summary", bounty.ID, err)
		summary = fmt.Sprintf("Task: %s", util.TruncateStr(directiveText(payload.Task), 400))
	} else {
		summary = strings.TrimSpace(rawOut)
		if summary == "" {
			summary = fmt.Sprintf("Task: %s", util.TruncateStr(directiveText(payload.Task), 400))
		}
	}

	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	store.StoreFleetMemory(db, payload.Repo, parentID, "success", summary, payload.Files)
	logger.Printf("WriteMemory #%d: memory stored for parent task #%d (repo: %s)", bounty.ID, parentID, payload.Repo)
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
