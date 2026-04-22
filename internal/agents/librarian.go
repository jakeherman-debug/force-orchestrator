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

Your job is to produce a concise, retrieval-friendly memory nugget about a completed task so that future agents can find and learn from it.

# OUTPUT FORMAT
Respond ONLY with valid JSON (no markdown, no preamble):
{
  "summary": "2-4 sentences in plain prose covering (1) what was built or fixed — name the function/file/system component; (2) what was non-obvious or tricky; (3) patterns, gotchas, or pitfalls that aren't obvious from the code alone",
  "tags": ["tag1", "tag2", "tag3", "tag4", "tag5"]
}

# SUMMARY RULES
- Write 2-4 sentences, plain prose, no bullets/headings/markdown inside the string.
- Be specific enough that a future agent can act on it without reading the code.
- Do NOT describe infrastructure failures (timeouts, worktree errors) as "tricky" — focus on code logic.
- Do NOT mention attempt counts or rejection history — only the final solution.
- Do NOT include meta-commentary about the task process.

# TAG RULES
- Emit 3-6 short topic keywords (one or two words each, lowercase, hyphen-separated if multi-word).
- Tags should capture retrieval-useful concepts: subsystem names (auth, migrations, dashboard), the kind of change (bugfix, refactor, new-feature, test-infra), distinctive identifiers (cors, oauth, sqlite-fts5), and file-level concepts (handler, middleware, schema).
- Prefer distinct, searchable terms. Avoid generic words like "task", "fix", "add", "code" that appear in every memory.
- Include synonyms for common concepts — a memory about "authentication" should also tag "login" or "auth" if relevant — this is what broadens FTS recall when future queries use different vocabulary.`

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
	var summary, tags string
	if err != nil {
		logger.Printf("WriteMemory #%d: Claude failed (%v) — using fallback summary", bounty.ID, err)
		summary = fmt.Sprintf("Task: %s", util.TruncateStr(directiveText(payload.Task), 400))
	} else {
		summary, tags = parseLibrarianOutput(rawOut)
		if summary == "" {
			// Prompt returned unparseable output — use the raw text as the summary.
			summary = strings.TrimSpace(rawOut)
		}
		if summary == "" {
			summary = fmt.Sprintf("Task: %s", util.TruncateStr(directiveText(payload.Task), 400))
		}
	}

	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	store.StoreFleetMemory(db, payload.Repo, parentID, "success", summary, payload.Files, tags)
	logger.Printf("WriteMemory #%d: memory stored for parent task #%d (repo: %s, tags: %s)",
		bounty.ID, parentID, payload.Repo, tags)
}

// librarianResponse is the structured output from the Librarian's LLM call.
type librarianResponse struct {
	Summary string   `json:"summary"`
	Tags    []string `json:"tags"`
}

// parseLibrarianOutput extracts (summary, comma-separated tags) from the
// Librarian's LLM response. Falls back to (raw text, "") on parse errors so
// a malformed response still stores a usable memory.
//
// Handles three response shapes:
//   1. Pure JSON: `{"summary":...,"tags":[...]}`
//   2. Fenced JSON: ```json\n{...}\n``` (claude.ExtractJSON strips the fences)
//   3. JSON embedded in prose (Claude occasionally ignores the "JSON only"
//      instruction): we slice from the first `{` to the last `}` and try
//      parsing that.
func parseLibrarianOutput(raw string) (summary, tagsCSV string) {
	candidate := claude.ExtractJSON(raw)
	if candidate == "" {
		return strings.TrimSpace(raw), ""
	}

	// First attempt: parse the candidate directly.
	var resp librarianResponse
	if json.Unmarshal([]byte(candidate), &resp) != nil {
		// Second attempt: slice to the outermost braces and retry.
		if first := strings.Index(candidate, "{"); first >= 0 {
			if last := strings.LastIndex(candidate, "}"); last > first {
				sliced := candidate[first : last+1]
				if json.Unmarshal([]byte(sliced), &resp) != nil {
					return strings.TrimSpace(raw), ""
				}
			} else {
				return strings.TrimSpace(raw), ""
			}
		} else {
			return strings.TrimSpace(raw), ""
		}
	}

	summary = strings.TrimSpace(resp.Summary)
	// Normalize tags: lowercase, trim, drop empties, dedup, cap at 8.
	seen := map[string]bool{}
	var tags []string
	for _, t := range resp.Tags {
		lw := strings.ToLower(strings.TrimSpace(t))
		if lw == "" || seen[lw] {
			continue
		}
		seen[lw] = true
		tags = append(tags, lw)
		if len(tags) >= 8 {
			break
		}
	}
	tagsCSV = strings.Join(tags, ", ")
	return summary, tagsCSV
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
