// D3 P6B.7 / 6B.8 — `force annotate` and `force replay` CLI parity
// for the drill diagnostic surface (Pattern P25).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

func cmdAnnotate(db *sql.DB, args []string) int {
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: force annotate <kind> <ref> <flag> <text...> [--operator <email>]")
		fmt.Fprintln(os.Stderr, "       flag in: problem | interesting | follow_up | none")
		return 1
	}
	kind, ref, flag := args[0], args[1], args[2]
	if flag == "none" {
		flag = ""
	}
	operator := "default@operator"
	textParts := []string{}
	for i := 3; i < len(args); i++ {
		if args[i] == "--operator" && i+1 < len(args) {
			operator = args[i+1]
			i++
			continue
		}
		textParts = append(textParts, args[i])
	}
	noteText := strings.Join(textParts, " ")
	if noteText == "" {
		fmt.Fprintln(os.Stderr, "annotate: note text required")
		return 1
	}
	id, err := store.InsertAnnotation(context.Background(), db, store.Annotation{
		OperatorEmail: operator, EventKind: kind, EventRef: ref, NoteText: noteText, Flag: flag,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "annotate failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — annotation %d created (kind=%s ref=%s flag=%q)\n", id, kind, ref, flag)
	return 0
}

func cmdReplay(db *sql.DB, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force replay <kind> <event_id> [--prompt-version <v>] [--operator <email>]")
		fmt.Fprintln(os.Stderr, "       kind in: captain_ruling | council_ruling | medic_decision | convoy_review_cycle")
		return 1
	}
	kind := args[0]
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(os.Stderr, "replay: invalid event id")
		return 1
	}
	pv := "current"
	operator := "default@operator"
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--prompt-version":
			if i+1 < len(args) {
				pv = args[i+1]
				i++
			}
		case "--operator":
			if i+1 < len(args) {
				operator = args[i+1]
				i++
			}
		}
	}
	res, err := agents.ReplayDecision(context.Background(), db, kind, id, pv, operator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay failed: %v\n", err)
		return 1
	}
	fmt.Printf("ReplayResults/%d — decision_changed=%t cost_usd=%.4f\n",
		res.ID, res.DecisionChanged, res.CostUSD)
	fmt.Printf("Original (head): %s\n", head(res.OriginalResponse, 200))
	fmt.Printf("Replayed (head): %s\n", head(res.ReplayResponse, 200))
	return 0
}

func head(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
