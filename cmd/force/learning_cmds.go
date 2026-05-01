// D3 P6B.12 — `force learning {refresh,show}` CLI parity for the
// fleet learning panel dashboard endpoints (Pattern P25).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"force-orchestrator/internal/agents"
)

func cmdLearning(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force learning refresh   — re-render the fleet learning panel now")
		fmt.Fprintln(os.Stderr, "       force learning show      — print the most recent panel prose")
		return 1
	}
	switch args[0] {
	case "refresh":
		id, err := agents.RenderFleetLearningPanel(context.Background(), db, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "learning refresh failed: %v\n", err)
			return 1
		}
		fmt.Printf("OK — FleetLearningPanels/%d rendered\n", id)
		return 0
	case "show":
		id, renderedAt, prose, sources, err := agents.LatestFleetLearningPanel(context.Background(), db)
		if err != nil {
			fmt.Fprintf(os.Stderr, "learning show failed: %v\n", err)
			return 1
		}
		if id == 0 {
			fmt.Println("(no fleet learning panel rendered yet — try: force learning refresh)")
			return 0
		}
		fmt.Printf("FleetLearningPanels/%d (rendered %s)\n\n", id, renderedAt)
		fmt.Println(prose)
		if len(sources) > 0 {
			fmt.Printf("\nSources: %v\n", sources)
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown learning sub-command: %q\n", args[0])
		return 1
	}
}
