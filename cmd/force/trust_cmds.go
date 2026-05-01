// D3 P6A.6 — `force trust <agent> <value> [--rationale <text>]` CLI.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"force-orchestrator/internal/store"
)

func cmdTrust(db *sql.DB, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force trust <agent> <value> [--rationale <text>] [--operator <email>]")
		fmt.Fprintln(os.Stderr, "       force trust list [--operator <email>]")
		return 1
	}
	if args[0] == "list" {
		return cmdTrustList(db, args[1:])
	}
	agent := args[0]
	value, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "value must be 0-100: %v\n", err)
		return 1
	}
	operator := "default@operator"
	rationale := ""
	for i := 2; i < len(args); i++ {
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
	if err := store.SetTrustDial(context.Background(), db, store.TrustDial{
		OperatorEmail: operator,
		Agent:         agent,
		DialValue:     value,
		SetBy:         string(store.TrustDialOperator),
		Rationale:     rationale,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "set trust failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — trust dial set: %s/%s = %d\n", operator, agent, value)
	return 0
}

func cmdTrustList(db *sql.DB, args []string) int {
	operator := "default@operator"
	for i := 0; i < len(args); i++ {
		if args[i] == "--operator" && i+1 < len(args) {
			operator = args[i+1]
			i++
		}
	}
	if err := store.BootstrapTrustDials(context.Background(), db, operator); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		return 1
	}
	dials, err := store.ListCurrentTrustDials(context.Background(), db, operator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		return 1
	}
	fmt.Printf("Trust dials for %s:\n", operator)
	for _, d := range dials {
		fmt.Printf("  %-15s %3d  (set_by=%s, rationale=%s)\n", d.Agent, d.DialValue, d.SetBy, d.Rationale)
	}
	return 0
}
