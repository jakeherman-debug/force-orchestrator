// D3 P6B.7 / 6B.8 — `force annotate` and `force replay` CLI parity
// for the drill diagnostic surface (Pattern P25).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

func cmdAnnotate(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("annotate", flag.ContinueOnError)
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	helped, perr := parseSubcommandFlags(fs, args, "annotate",
		"Insert an operator annotation against a kind/ref pair (drill diagnostic).",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force annotate captain_ruling 42 problem something looked off"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: force annotate <kind> <ref> <flag> <text...> [--operator <email>]")
		fmt.Fprintln(os.Stderr, "       flag in: problem | interesting | follow_up | none")
		return 1
	}
	kind, ref, flagVal := rest[0], rest[1], rest[2]
	if flagVal == "none" {
		flagVal = ""
	}
	noteText := strings.Join(rest[3:], " ")
	if noteText == "" {
		fmt.Fprintln(os.Stderr, "annotate: note text required")
		return 1
	}
	id, err := store.InsertAnnotation(context.Background(), db, store.Annotation{
		OperatorEmail: *operatorFlag, EventKind: kind, EventRef: ref, NoteText: noteText, Flag: flagVal,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "annotate failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — annotation %d created (kind=%s ref=%s flag=%q)\n", id, kind, ref, flagVal)
	return 0
}

func cmdReplay(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	pvFlag := fs.String("prompt-version", "current", "prompt version to replay against")
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	helped, perr := parseSubcommandFlags(fs, args, "replay",
		"Re-run a recorded decision against a prompt version. Writes a ReplayResults row.",
		[]flagDoc{
			{Name: "--prompt-version V", Desc: "prompt version to replay against"},
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force replay captain_ruling 42"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force replay <kind> <event_id> [--prompt-version <v>] [--operator <email>]")
		fmt.Fprintln(os.Stderr, "       kind in: captain_ruling | council_ruling | medic_decision | convoy_review_cycle")
		return 1
	}
	kind := rest[0]
	id := int64(mustParseID(rest[1]))
	if id <= 0 {
		fmt.Fprintln(os.Stderr, "replay: invalid event id")
		return 1
	}
	res, err := agents.ReplayDecision(context.Background(), db, kind, id, *pvFlag, *operatorFlag)
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
