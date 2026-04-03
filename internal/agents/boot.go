package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/util"
)

// BootDecision is the triage verdict for a stalled task.
type BootDecision string

const (
	BootIgnore   BootDecision = "IGNORE"   // task is making progress, leave it alone
	BootWarn     BootDecision = "WARN"     // emit a warning but don't reset
	BootReset    BootDecision = "RESET"    // reset to Pending, agent is clearly stuck
	BootEscalate BootDecision = "ESCALATE" // needs human attention
)

type BootVerdict struct {
	Decision BootDecision `json:"decision"`
	Reason   string       `json:"reason"`
}

const bootSystemPrompt = `You are the Boot Agent — a cheap triage AI for the Galactic Fleet orchestration system.
You are given a summary of a potentially stalled coding task. Your job is to decide what the Inquisitor should do.

Respond in raw JSON ONLY (no markdown):
{"decision": "IGNORE|WARN|RESET|ESCALATE", "reason": "one sentence explanation"}

Decision guide:
- IGNORE: the task description or recent log evidence suggests the agent is still making reasonable progress
- WARN:   the task appears stalled but may self-recover; emit a log warning only
- RESET:  the agent is clearly stuck (infinite loop, repeated identical errors, no progress possible without restart)
- ESCALATE: the task requires human intervention (auth failure, missing infra, unresolvable conflict)`

// BootTriage asks the Boot Agent to decide what to do with a stalled task.
// Returns WARN on any Claude call failure so Inquisitor falls back to normal behavior.
func BootTriage(db *sql.DB, taskID int, owner, repo string, lockedMinutes float64, errorLog string) BootVerdict {
	summary := fmt.Sprintf(
		"Task ID: %d\nAgent: %s\nRepo: %s\nLocked for: %.0f minutes\nError log: %s",
		taskID, owner, repo, lockedMinutes, util.TruncateStr(errorLog, 500),
	)

	resp, err := claude.AskClaudeCLI(bootSystemPrompt, summary, "", 3)
	if err != nil {
		return BootVerdict{Decision: BootWarn, Reason: fmt.Sprintf("boot triage unavailable: %v", err)}
	}

	clean := claude.ExtractJSON(resp)
	// Also handle bare JSON without markdown fences
	if !strings.HasPrefix(strings.TrimSpace(clean), "{") {
		clean = resp
	}

	var verdict BootVerdict
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(clean)), &verdict); jsonErr != nil {
		return BootVerdict{Decision: BootWarn, Reason: "boot triage parse error"}
	}

	// Validate decision field
	switch verdict.Decision {
	case BootIgnore, BootWarn, BootReset, BootEscalate:
	default:
		verdict.Decision = BootWarn
	}
	return verdict
}
