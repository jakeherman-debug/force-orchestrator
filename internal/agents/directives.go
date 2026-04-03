package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// agentSignalPattern matches the signal tokens agents emit so they can be stripped
// from operator-supplied directive files, preventing injection attacks.
var agentSignalPattern = regexp.MustCompile(`\[(ESCALATED:[A-Z]+:[^\]]*|SHARD_NEEDED|DONE|CHECKPOINT:[^\]]*)\]`)

// LoadDirective reads a role-specific system prompt addition from disk.
// Lookup order (first match wins):
//  1. ./directives/<repo>/<role>.md     (per-repo local)
//  2. ~/.force/directives/<repo>/<role>.md (per-repo user-global)
//  3. ./directives/<role>.md            (global local)
//  4. ~/.force/directives/<role>.md     (global user-global)
//
// Pass an empty string for repo (or omit it) to skip per-repo lookup.
func LoadDirective(role string, repos ...string) string {
	filename := role + ".md"
	home, _ := os.UserHomeDir()

	// Per-repo lookup (if caller provides a repo name)
	if len(repos) > 0 && repos[0] != "" {
		repo := repos[0]
		if data, err := os.ReadFile(filepath.Join("directives", repo, filename)); err == nil {
			return sanitizeDirective(strings.TrimSpace(string(data)))
		}
		if home != "" {
			if data, err := os.ReadFile(filepath.Join(home, ".force", "directives", repo, filename)); err == nil {
				return sanitizeDirective(strings.TrimSpace(string(data)))
			}
		}
	}

	// Global fallback
	if data, err := os.ReadFile(filepath.Join("directives", filename)); err == nil {
		return sanitizeDirective(strings.TrimSpace(string(data)))
	}
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, ".force", "directives", filename)); err == nil {
			return sanitizeDirective(strings.TrimSpace(string(data)))
		}
	}

	return ""
}

// sanitizeDirective strips agent signal tokens from directive content to prevent
// operators from accidentally (or maliciously) injecting signals via directive files.
func sanitizeDirective(content string) string {
	return agentSignalPattern.ReplaceAllString(content, "[signal-removed]")
}

// CmdDirective handles the `force directive` subcommands.
// Moved here from cmd/force — called by cmd/force/main.go.
func CmdDirective(args []string) {
	dirSubCmd := ""
	if len(args) >= 1 {
		dirSubCmd = args[0]
	}
	dirRole := ""
	if len(args) >= 2 {
		dirRole = args[1]
	}
	switch dirSubCmd {
	case "show":
		if dirRole == "" {
			dirRole = "astromech"
		}
		content := LoadDirective(dirRole)
		if content == "" {
			fmt.Printf("No directive found for role '%s'.\n", dirRole)
			fmt.Printf("Create one at: ./directives/%s.md\n", dirRole)
		} else {
			fmt.Printf("=== Directive: %s ===\n\n%s\n", dirRole, content)
		}
	case "example", "":
		if dirRole == "" {
			dirRole = "astromech"
		}
		var example string
		switch dirRole {
		case "astromech":
			example = `# Astromech Directive Example
# Place this file at: ./directives/astromech.md
# Or per-repo:        ./directives/<repo-name>/astromech.md

## Style

- Prefer small, focused commits — one logical change per commit.
- Always add tests for new public functions.
- Follow the existing error-handling patterns in the codebase.

## Constraints

- Do not change existing API contracts without explicit instruction.
- Do not add new dependencies without operator approval.

## Workflow

- If the task is ambiguous, implement the most conservative interpretation
  and note your assumption in the commit message.`
		case "commander":
			example = `# Commander Directive Example
# Place this file at: ./directives/commander.md

## Decomposition strategy

- Prefer tasks that can be completed independently in < 2 hours.
- When a feature touches multiple services, split into one task per service.
- Always create a test task as a dependency of the implementation task.

## Constraints

- Do not create more than 8 subtasks per feature request.
- Group related file changes into one task where possible.`
		case "council":
			example = `# Jedi Council Directive Example
# Place this file at: ./directives/council.md

## Review criteria

- Reject if tests are missing for new functionality.
- Reject if the diff adds TODO comments without tracking issues.
- Approve even if style is imperfect — correctness first.

## Escalation

- Escalate at HIGH severity if security-sensitive code is modified
  (auth, crypto, network, file permissions).`
		default:
			example = fmt.Sprintf(`# %s Directive Example
# Place this file at: ./directives/%s.md
# Or per-repo:        ./directives/<repo-name>/%s.md

## Instructions

Add role-specific operator instructions here.
These are injected into the agent's system prompt at runtime.`, dirRole, dirRole, dirRole)
		}
		fmt.Printf("=== Example directive for role '%s' ===\n\n%s\n\n", dirRole, example)
		fmt.Printf("Save to: ./directives/%s.md\n", dirRole)
	default:
		fmt.Printf("Unknown directive subcommand: %s\n", dirSubCmd)
		fmt.Println("Usage: force directive [show|example] [role]")
		fmt.Println("  Roles: astromech, commander, council")
		os.Exit(1)
	}
}
