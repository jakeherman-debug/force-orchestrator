// D3 P6A.13 — `force cooldown <pause|resume|cancel> <id>` CLI parity.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"force-orchestrator/internal/agents"
)

func cmdCooldown(db *sql.DB, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force cooldown pause|resume|cancel <id> [--rationale <text>]")
		return 1
	}
	action := args[0]
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "id must be integer: %v\n", err)
		return 1
	}
	rationale := ""
	for i := 2; i < len(args); i++ {
		if args[i] == "--rationale" && i+1 < len(args) {
			rationale = args[i+1]
			i++
		}
	}
	ctx := context.Background()
	switch action {
	case "pause":
		if err := agents.PauseCooldown(ctx, db, id, "default@operator"); err != nil {
			fmt.Fprintf(os.Stderr, "pause failed: %v\n", err)
			return 1
		}
		fmt.Printf("OK — paused cooldown #%d\n", id)
	case "resume":
		if err := agents.ResumeCooldown(ctx, db, id, rationale); err != nil {
			fmt.Fprintf(os.Stderr, "resume failed: %v\n", err)
			return 1
		}
		fmt.Printf("OK — resumed cooldown #%d\n", id)
	case "cancel":
		if err := agents.CancelCooldown(ctx, db, id); err != nil {
			fmt.Fprintf(os.Stderr, "cancel failed: %v\n", err)
			return 1
		}
		fmt.Printf("OK — cancelled cooldown #%d\n", id)
	default:
		fmt.Fprintf(os.Stderr, "unknown action: %s\n", action)
		return 1
	}
	return 0
}
