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

const librarianSystemPrompt = `You are the Librarian — a specialist agent in the Galactic Fleet responsible for writing high-quality knowledge entries for FleetMemory.

FleetMemory is a RAG knowledge store that other agents read before starting work. Your entries must be useful, specific, and non-obvious.

You will receive a completed task description, the files changed, council feedback, and the diff of what was built.

Write a 2-4 sentence knowledge nugget that covers:
1. What was built or fixed (specific — name the mechanism, function, or pattern)
2. What was non-obvious or tricky about the implementation
3. Any patterns, gotchas, or lessons that would not be obvious from reading the code alone

RULES:
- Do NOT mention infrastructure failures (timeouts, worktree errors, network issues) when assessing difficulty — only technical/design complexity counts
- Be specific: name files, functions, patterns, and mechanisms when relevant
- Avoid generic summaries like "the task was completed successfully"
- Write in past tense, third person (e.g., "The implementation added...", "The fix required...")
- Output ONLY the knowledge nugget — no preamble, no metadata, no markdown`

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
	priorContext := ""
	if len(priorMail) > 0 {
		var priorLines []string
		for _, m := range priorMail {
			priorLines = append(priorLines, fmt.Sprintf("- %s: %s", m.Subject, m.Body))
		}
		priorContext = fmt.Sprintf("\n\nPRIOR ATTEMPT HISTORY:\n%s", strings.Join(priorLines, "\n"))
	}

	userPrompt := fmt.Sprintf(
		"TASK DESCRIPTION:\n%s\n\nFILES CHANGED:\n%s\n\nCOUNCIL FEEDBACK:\n%s\n\nDIFF:\n%s%s",
		payload.Task,
		payload.Files,
		payload.Feedback,
		payload.Diff,
		priorContext,
	)

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
