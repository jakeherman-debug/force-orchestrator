package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"force-orchestrator/internal/dashboard"
)

func cmdDashboard(db *sql.DB, args []string) {
	// `force dashboard status` is a special read-only path that bypasses the
	// helper (it doesn't take any flags).
	if len(args) >= 1 && args[0] == "status" {
		os.Exit(cmdDashboardStatus(db, args[1:]))
	}

	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	portFlag := fs.Int("port", 8080, "port to listen on")
	helped, perr := parseSubcommandFlags(fs, args, "dashboard",
		"Start the dashboard HTTP server. Subcommand `status` reads heartbeat + exits.",
		[]flagDoc{
			{Name: "--port N", Desc: "port to listen on (default 8080)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force dashboard", "force dashboard --port 8081", "force dashboard status"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}

	port := *portFlag
	// Backwards-compat: a bare positional integer ("force dashboard 8081") is
	// interpreted as the port. The flag-form is preferred; the positional
	// shape is kept for muscle-memory.
	rest := fs.Args()
	if len(rest) >= 1 && !strings.HasPrefix(rest[0], "--") {
		if p, err := strconv.Atoi(rest[0]); err == nil && p > 0 {
			port = p
		} else {
			fmt.Fprintf(os.Stderr, "force dashboard: positional arg %q is not a port number\n", rest[0])
			os.Exit(2)
		}
	}

	dashboard.RunDashboard(db, port)
}
