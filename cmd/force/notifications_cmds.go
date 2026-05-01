// D3 P6A.4 — `force notifications budget <source> <channel> <max> <period_min>` CLI.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"force-orchestrator/internal/store"
)

// cmdNotifications dispatches the `force notifications ...` subtree.
func cmdNotifications(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force notifications budget <source> <channel> <max_per_period> <period_minutes> [--operator <email>] [--no-digest]")
		return 1
	}

	switch args[0] {
	case "budget":
		return cmdNotificationsBudget(db, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown notifications subcommand: %s\n", args[0])
		return 1
	}
}

func cmdNotificationsBudget(db *sql.DB, args []string) int {
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: force notifications budget <source> <channel> <max_per_period> <period_minutes> [--operator <email>] [--no-digest]")
		return 1
	}

	source := args[0]
	channel := args[1]
	maxPer := mustParseID(args[2])
	periodMin := mustParseID(args[3])
	operator := "default@operator"
	digest := true
	for i := 4; i < len(args); i++ {
		switch args[i] {
		case "--operator":
			if i+1 < len(args) {
				operator = args[i+1]
				i++
			}
		case "--no-digest":
			digest = false
		}
	}

	if err := store.SetNotificationBudget(context.Background(), db, operator, source, channel, maxPer, periodMin, digest); err != nil {
		fmt.Fprintf(os.Stderr, "set budget failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — budget set: %s/%s/%s = %d per %dm (digest=%v)\n", operator, source, channel, maxPer, periodMin, digest)
	return 0
}
