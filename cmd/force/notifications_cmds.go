// D3 P6A.4 — `force notifications budget <source> <channel> <max> <period_min>` CLI.
package main

import (
	"context"
	"database/sql"
	"flag"
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
	case "--help", "-h", "help":
		fmt.Println("Usage: force notifications budget <source> <channel> <max_per_period> <period_minutes> [--operator <email>] [--no-digest]")
		return 0
	case "budget":
		return cmdNotificationsBudget(db, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown notifications subcommand: %s\n", args[0])
		return 1
	}
}

func cmdNotificationsBudget(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("notifications budget", flag.ContinueOnError)
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	noDigestFlag := fs.Bool("no-digest", false, "disable digest mode for this budget")
	helped, perr := parseSubcommandFlags(fs, args, "notifications budget",
		"Set a notification budget (rate-limit) row for a source/channel pair.",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--no-digest", Desc: "disable digest mode"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force notifications budget escalations slack 5 60"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: force notifications budget <source> <channel> <max_per_period> <period_minutes> [--operator <email>] [--no-digest]")
		return 1
	}

	source := rest[0]
	channel := rest[1]
	maxPer := mustParseID(rest[2])
	periodMin := mustParseID(rest[3])
	digest := !*noDigestFlag

	if err := store.SetNotificationBudget(context.Background(), db, *operatorFlag, source, channel, maxPer, periodMin, digest); err != nil {
		fmt.Fprintf(os.Stderr, "set budget failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — budget set: %s/%s/%s = %d per %dm (digest=%v)\n", *operatorFlag, source, channel, maxPer, periodMin, digest)
	return 0
}
