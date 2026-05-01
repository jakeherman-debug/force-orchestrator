// D3 P6B.10 / 6B.13 — `force ask` and `force retro` CLI parity for
// the dashboard's Ask + retro endpoints (Pattern P25).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"force-orchestrator/internal/agents"
)

func cmdAsk(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force ask <question…>")
		return 1
	}
	q := strings.Join(args, " ")
	a, err := agents.AskHandle(context.Background(), db, q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask failed: %v\n", err)
		return 1
	}
	fmt.Println(a.Answer)
	if len(a.CiteLinks) > 0 {
		fmt.Println()
		fmt.Println("Cite links:")
		for _, c := range a.CiteLinks {
			fmt.Printf("  %s/%d — %s\n", c.Kind, c.ID, c.Label)
		}
	}
	return 0
}

func cmdRetro(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force retro generate           — print the markdown draft to stdout")
		fmt.Fprintln(os.Stderr, "       force retro save                — save the draft to docs/retros/<date>.md")
		return 1
	}
	switch args[0] {
	case "generate":
		retro, err := agents.GenerateRetro(context.Background(), db, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "retro generate: %v\n", err)
			return 1
		}
		fmt.Println(retro.Markdown)
		fmt.Println()
		fmt.Printf("(suggested save path: %s)\n", retro.SuggestedPath)
		return 0
	case "save":
		retro, err := agents.GenerateRetro(context.Background(), db, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "retro generate: %v\n", err)
			return 1
		}
		path, err := agents.SaveRetroDraft(retro.SuggestedPath, retro.Markdown)
		if err != nil {
			fmt.Fprintf(os.Stderr, "retro save: %v\n", err)
			return 1
		}
		fmt.Printf("Saved to %s\n", path)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown retro sub-command: %q\n", args[0])
		return 1
	}
}
