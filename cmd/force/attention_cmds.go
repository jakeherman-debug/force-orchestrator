// D3 P6A.14 — `force attention <kind> <id> <level> [--rationale <text>]` CLI.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"force-orchestrator/internal/store"
)

func cmdAttention(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("attention", flag.ContinueOnError)
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	rationaleFlag := fs.String("rationale", "", "free-form rationale")
	helped, perr := parseSubcommandFlags(fs, args, "attention",
		"Set an operator AttentionTag against a kind/id pair.",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--rationale T", Desc: "free-form rationale"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force attention task 42 high --rationale flaky"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: force attention <kind> <id> <level> [--rationale <text>] [--operator <email>]")
		return 1
	}
	kind, id, level := rest[0], rest[1], rest[2]
	if err := store.SetAttentionTag(context.Background(), db, store.AttentionTag{
		OperatorEmail:  *operatorFlag,
		TargetKind:     kind,
		TargetID:       id,
		AttentionLevel: level,
		Rationale:      *rationaleFlag,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "set attention failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — attention set: %s/%s/%s = %s\n", *operatorFlag, kind, id, level)
	return 0
}
