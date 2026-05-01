// D3 P6A.5 — `force session save <route>` CLI parity.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"force-orchestrator/internal/store"
)

func cmdSession(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force session save <route> [--operator <email>] [--surface <pulse|briefing|reflection>]")
		return 1
	}
	switch args[0] {
	case "save":
		return cmdSessionSave(db, args[1:])
	case "clear":
		return cmdSessionClear(db, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown session subcommand: %s\n", args[0])
		return 1
	}
}

func cmdSessionSave(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force session save <route> [--operator <email>] [--surface <surface>]")
		return 1
	}
	route := args[0]
	operator := "default@operator"
	surface := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--operator":
			if i+1 < len(args) {
				operator = args[i+1]
				i++
			}
		case "--surface":
			if i+1 < len(args) {
				surface = args[i+1]
				i++
			}
		}
	}
	if err := store.SaveOperatorSession(context.Background(), db, store.OperatorSession{
		OperatorEmail:     operator,
		LastViewedSurface: surface,
		LastViewedRoute:   route,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — session saved: %s @ %s\n", operator, route)
	return 0
}

func cmdSessionClear(db *sql.DB, args []string) int {
	operator := "default@operator"
	for i := 0; i < len(args); i++ {
		if args[i] == "--operator" && i+1 < len(args) {
			operator = args[i+1]
			i++
		}
	}
	if err := store.ClearOperatorSession(context.Background(), db, operator); err != nil {
		fmt.Fprintf(os.Stderr, "clear failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — session cleared: %s\n", operator)
	return 0
}
