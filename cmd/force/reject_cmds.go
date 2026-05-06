// D3 P6A.11 — `force briefing-reject <kind> <id> --counter-kind <…> --text <…>` CLI.
//
// Note: `force reject` is the existing task-level reject command; this
// new command targets a Briefing decision so the namespaces don't collide.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"force-orchestrator/internal/agents"
)

// cmdReject — `force briefing-reject <kind> <id> --counter-kind whole_thing|different_approach|defer --text "..."`
func cmdReject(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("briefing-reject", flag.ContinueOnError)
	counterKind := fs.String("counter-kind", "", "whole_thing|different_approach|defer")
	text := fs.String("text", "", "free-form rejection reason")
	helped, perr := parseSubcommandFlags(fs, args, "briefing-reject",
		"Reject a briefing and route a CounterProposal of the chosen kind.",
		[]flagDoc{
			{Name: "--counter-kind K", Desc: "whole_thing|different_approach|defer"},
			{Name: "--text T", Desc: "free-form rejection reason"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force briefing-reject convoy 42 --counter-kind whole_thing --text \"...\""})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force briefing-reject <kind> <id> --counter-kind <kind> --text <reason>")
		return 1
	}
	kind := rest[0]
	id := int64(mustParseID(rest[1]))
	if *counterKind == "" {
		fmt.Fprintln(os.Stderr, "--counter-kind required")
		return 1
	}
	br, err := agents.RenderBriefing(context.Background(), db, kind, id, 70)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render: %v\n", err)
		return 1
	}
	newID, err := agents.RouteCounterProposal(context.Background(), db, br.ID, kind,
		agents.CounterProposalKind(*counterKind), *text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reject failed: %v\n", err)
		return 1
	}
	suffix := ""
	if newID != 0 {
		suffix = fmt.Sprintf(" (routed_id=%d)", newID)
	}
	fmt.Printf("OK — rejected %s/%d via %s%s\n", kind, id, *counterKind, suffix)
	return 0
}
