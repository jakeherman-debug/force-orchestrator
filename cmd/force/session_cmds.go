// D3 P6A.5 — `force session save <route>` CLI parity.
package main

import (
	"context"
	"database/sql"
	"flag"
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
	case "--help", "-h", "help":
		fmt.Println("Usage: force session save <route> [--operator <email>] [--surface <surface>]")
		fmt.Println("       force session clear [--operator <email>]")
		return 0
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
	fs := flag.NewFlagSet("session save", flag.ContinueOnError)
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	surfaceFlag := fs.String("surface", "", "surface name (pulse|briefing|reflection)")
	helped, perr := parseSubcommandFlags(fs, args, "session save",
		"Persist the operator's last-viewed route + surface.",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--surface S", Desc: "surface name (pulse|briefing|reflection)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force session save /pulse"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force session save <route> [--operator <email>] [--surface <surface>]")
		return 1
	}
	route := rest[0]
	if err := store.SaveOperatorSession(context.Background(), db, store.OperatorSession{
		OperatorEmail:     *operatorFlag,
		LastViewedSurface: *surfaceFlag,
		LastViewedRoute:   route,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — session saved: %s @ %s\n", *operatorFlag, route)
	return 0
}

func cmdSessionClear(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("session clear", flag.ContinueOnError)
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	helped, perr := parseSubcommandFlags(fs, args, "session clear",
		"Clear the operator's saved session row.",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force session clear"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	if err := store.ClearOperatorSession(context.Background(), db, *operatorFlag); err != nil {
		fmt.Fprintf(os.Stderr, "clear failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — session cleared: %s\n", *operatorFlag)
	return 0
}
