package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

// cmdConvoyPRReview prints the PR review-comment table for a convoy. Shows
// every comment with its classification, spawned task (if any), and reply
// status. Intended for operator debugging and for inspecting human comments
// that are awaiting action.
func cmdConvoyPRReview(db *sql.DB, convoyID int) {
	rollup := store.ComputePRReviewRollup(db, convoyID)
	comments := store.ListConvoyPRComments(db, convoyID)
	if len(comments) == 0 {
		fmt.Printf("Convoy %d: no PR review comments recorded yet.\n", convoyID)
		return
	}

	fmt.Printf("Convoy %d — %d PR review comment(s)\n", convoyID, rollup.Total)
	fmt.Printf("  Bots:   fix=%d  followup=%d  replied=%d  loop=%d  unclassified=%d\n",
		rollup.BotInScope, rollup.BotOutOfScope, rollup.BotNotAction,
		rollup.BotConflicted, rollup.BotUnclassified)
	fmt.Printf("  Humans: awaiting=%d\n\n", rollup.HumanAwaiting)

	fmt.Printf("%-8s %-14s %-18s %-12s %-18s %s\n",
		"ID", "AUTHOR", "CLASSIFICATION", "TYPE", "SPAWNED / REPLY", "COMMENT")
	fmt.Println(strings.Repeat("-", 140))

	for _, c := range comments {
		location := "PR-level"
		if c.Path != "" {
			location = c.Path
			if c.Line > 0 {
				location = fmt.Sprintf("%s:%d", c.Path, c.Line)
			}
		}

		classLabel := c.Classification
		if classLabel == "" {
			classLabel = "(pending)"
		}
		authorLabel := fmt.Sprintf("%s/%s", c.AuthorKind, truncate(c.Author, 10))

		spawnedOrReply := ""
		switch {
		case c.SpawnedTaskID > 0 && c.Classification == "in_scope_fix":
			spawnedOrReply = fmt.Sprintf("→ task #%d", c.SpawnedTaskID)
		case c.SpawnedTaskID > 0 && c.Classification == "out_of_scope":
			spawnedOrReply = fmt.Sprintf("→ feature #%d", c.SpawnedTaskID)
		case c.SpawnedTaskID > 0 && c.AuthorKind == "human":
			spawnedOrReply = fmt.Sprintf("→ feature #%d", c.SpawnedTaskID)
		case c.RepliedAt != "":
			spawnedOrReply = "replied"
		case c.Classification == "human":
			spawnedOrReply = "DRAFT (awaiting op)"
		case c.Classification == "conflicted_loop":
			spawnedOrReply = "ESCALATED"
		case c.Classification == "ignored":
			spawnedOrReply = "dismissed"
		}

		fmt.Printf("%-8d %-14s %-18s %-12s %-18s %s\n",
			c.ID,
			truncate(authorLabel, 14),
			truncate(classLabel, 18),
			truncate(location, 12),
			truncate(spawnedOrReply, 18),
			truncate(strings.ReplaceAll(c.Body, "\n", " "), 80),
		)
	}
	fmt.Println()

	// If there are human-drafted replies awaiting operator action, remind
	// the operator how to act on them.
	if rollup.HumanAwaiting > 0 {
		fmt.Println("Human comments are awaiting your decision. Use the dashboard to:")
		fmt.Println("  - Post the AI-drafted reply as-is,")
		fmt.Println("  - Edit the draft before posting,")
		fmt.Println("  - Queue a follow-up Feature, or")
		fmt.Println("  - Dismiss the comment.")
	}
	os.Stdout.Sync()
}
