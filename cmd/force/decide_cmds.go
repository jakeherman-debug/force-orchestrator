// D3 P6A.10/P6A.11 — `force decide` and `force reject` CLI parity.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"force-orchestrator/internal/agents"
)

// cmdDecide — `force decide <kind> <id> --approve|--reject [--rationale <text>]`
func cmdDecide(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	approveFlag := fs.Bool("approve", false, "approve the briefing decision")
	rejectFlag := fs.Bool("reject", false, "reject the briefing decision")
	helped, perr := parseSubcommandFlags(fs, args, "decide",
		"Approve or reject a briefing decision (writes a BriefingDecision row).",
		[]flagDoc{
			{Name: "--approve", Desc: "approve the briefing decision"},
			{Name: "--reject", Desc: "reject the briefing decision"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force decide convoy 42 --approve", "force decide briefing 7 --reject"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force decide <kind> <id> --approve|--reject")
		return 1
	}
	kind := rest[0]
	id := int64(mustParseID(rest[1]))
	decision := ""
	switch {
	case *approveFlag:
		decision = "approved"
	case *rejectFlag:
		decision = "rejected"
	default:
		fmt.Fprintln(os.Stderr, "must pass --approve or --reject")
		return 1
	}
	br, err := agents.RenderBriefing(context.Background(), db, kind, id, 70)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render: %v\n", err)
		return 1
	}
	if err := agents.RecordBriefingDecision(context.Background(), db, br.ID, decision, 0, "", "", 0); err != nil {
		fmt.Fprintf(os.Stderr, "record: %v\n", err)
		return 1
	}
	fmt.Printf("OK — %s %s/%d (briefing_id=%d)\n", decision, kind, id, br.ID)
	return 0
}
