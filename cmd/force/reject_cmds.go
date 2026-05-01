// D3 P6A.11 — `force briefing-reject <kind> <id> --counter-kind <…> --text <…>` CLI.
//
// Note: `force reject` is the existing task-level reject command; this
// new command targets a Briefing decision so the namespaces don't collide.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"

	"force-orchestrator/internal/agents"
)

// cmdReject — `force reject <kind> <id> --counter-kind whole_thing|different_approach|defer --text "..."`
func cmdReject(db *sql.DB, args []string) int {
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: force reject <kind> <id> --counter-kind <kind> --text <reason>")
		return 1
	}
	kind := args[0]
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "id must be integer: %v\n", err)
		return 1
	}
	counterKind := ""
	text := ""
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--counter-kind":
			if i+1 < len(args) {
				counterKind = args[i+1]
				i++
			}
		case "--text":
			if i+1 < len(args) {
				text = args[i+1]
				i++
			}
		}
	}
	if counterKind == "" {
		fmt.Fprintln(os.Stderr, "--counter-kind required")
		return 1
	}
	br, err := agents.RenderBriefing(context.Background(), db, kind, id, 70)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render: %v\n", err)
		return 1
	}
	newID, err := agents.RouteCounterProposal(context.Background(), db, br.ID, kind,
		agents.CounterProposalKind(counterKind), text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reject failed: %v\n", err)
		return 1
	}
	suffix := ""
	if newID != 0 {
		suffix = fmt.Sprintf(" (routed_id=%d)", newID)
	}
	fmt.Printf("OK — rejected %s/%d via %s%s\n", kind, id, counterKind, suffix)
	_ = strings.TrimSpace // keep `strings` import in case formatting expands
	return 0
}
