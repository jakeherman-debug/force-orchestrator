// D3 P6A.13 — `force cooldown <pause|resume|cancel> <id>` CLI parity.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"force-orchestrator/internal/agents"
)

func runCooldownAction(ctx context.Context, db *sql.DB, action string, id int64, rationale string) int {
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
	}
	return 0
}

func cmdCooldown(db *sql.DB, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: force cooldown pause|resume|cancel <id> [--rationale <text>]")
		return 1
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "--help", "-h", "help":
		fmt.Println("Usage: force cooldown pause|resume|cancel <id> [--rationale <text>]")
		return 0
	case "pause", "resume", "cancel":
		// fall through
	default:
		fmt.Fprintf(os.Stderr, "unknown action: %s\n", action)
		return 1
	}

	fs := flag.NewFlagSet("cooldown "+action, flag.ContinueOnError)
	rationaleFlag := fs.String("rationale", "", "free-form rationale")
	helped, perr := parseSubcommandFlags(fs, rest, "cooldown "+action,
		"Pause / resume / cancel a cooldown row.",
		[]flagDoc{
			{Name: "--rationale T", Desc: "free-form rationale"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force cooldown " + action + " 17"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest2 := fs.Args()
	if len(rest2) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force cooldown "+action+" <id>")
		return 1
	}
	id := int64(mustParseID(rest2[0]))
	ctx := context.Background()
	return runCooldownAction(ctx, db, action, id, *rationaleFlag)
}
