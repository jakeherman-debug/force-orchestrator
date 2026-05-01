// D3 P6A.14 — `force attention <kind> <id> <level> [--rationale <text>]` CLI.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"force-orchestrator/internal/store"
)

func cmdAttention(db *sql.DB, args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: force attention <kind> <id> <level> [--rationale <text>] [--operator <email>]")
		return 1
	}
	kind, id, level := args[0], args[1], args[2]
	operator := "default@operator"
	rationale := ""
	for i := 3; i < len(args); i++ {
		switch args[i] {
		case "--operator":
			if i+1 < len(args) {
				operator = args[i+1]
				i++
			}
		case "--rationale":
			if i+1 < len(args) {
				rationale = args[i+1]
				i++
			}
		}
	}
	if err := store.SetAttentionTag(context.Background(), db, store.AttentionTag{
		OperatorEmail:  operator,
		TargetKind:     kind,
		TargetID:       id,
		AttentionLevel: level,
		Rationale:      rationale,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "set attention failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — attention set: %s/%s/%s = %s\n", operator, kind, id, level)
	return 0
}
