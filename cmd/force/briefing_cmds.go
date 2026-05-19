package main

// briefing_cmds.go — `force briefing` CLI surface.
//
// D17 P2A: CLI parity for the briefing-queue endpoint. Queries the
// BountyBoard for tasks awaiting operator review and renders them as a
// table so operators can triage from the terminal without opening the
// dashboard.

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/agents"
)

// cmdBriefing — `force briefing`. Lists queued decisions (tasks in
// AwaitingCaptainReview / AwaitingCouncilReview / Escalated) sorted by
// stakes tier (high first), then created_at descending.
//
// The query is the same one backing GET /api/briefing/queue so the CLI
// and dashboard see exactly the same data.
func cmdBriefing(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("briefing", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "briefing",
		"List queued decisions awaiting operator review (mirrors GET /api/briefing/queue).",
		[]flagDoc{
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force briefing"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}

	q, err := agents.ListBriefingQueue(context.Background(), db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: briefing queue query failed: %v\n", err)
		os.Exit(1)
	}

	if len(q) == 0 {
		fmt.Println("No decisions awaiting review.")
		return
	}

	fmt.Printf("%-6s %-8s %-22s %-20s %s\n", "ID", "STAKES", "KIND", "CREATED", "TITLE")
	fmt.Println(strings.Repeat("-", 100))
	for _, row := range q {
		title := row.Title
		if len(title) > 48 {
			title = title[:47] + "…"
		}
		fmt.Printf("%-6d %-8s %-22s %-20s %s\n",
			row.DecisionID,
			row.StakesTier,
			row.DecisionKind,
			truncate(row.CreatedAt, 20),
			title,
		)
	}
	fmt.Printf("\n%d decision(s) queued. Use `force decide` or the dashboard to act.\n", len(q))
}
