// D3 P6A.10/P6A.11 — `force decide` and `force reject` CLI parity.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"force-orchestrator/internal/agents"
)

// cmdDecide — `force decide <kind> <id> [--approve|--reject] [--rationale <text>]`
func cmdDecide(db *sql.DB, args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: force decide <kind> <id> --approve|--reject [--rationale <text>]")
		return 1
	}
	kind := args[0]
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "id must be integer: %v\n", err)
		return 1
	}
	decision := ""
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--approve":
			decision = "approved"
		case "--reject":
			decision = "rejected"
		}
	}
	if decision == "" {
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
