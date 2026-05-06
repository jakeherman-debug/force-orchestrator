// D3 P6A.6 — `force trust <agent> <value> [--rationale <text>]` CLI.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"

	"force-orchestrator/internal/store"
)

func cmdTrust(db *sql.DB, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force trust <agent> <value> [--rationale <text>] [--operator <email>]")
		fmt.Fprintln(os.Stderr, "       force trust list [--operator <email>]")
		return 1
	}
	switch args[0] {
	case "--help", "-h", "help":
		fmt.Println("Usage: force trust <agent> <value> [--rationale <text>] [--operator <email>]")
		fmt.Println("       force trust list [--operator <email>]")
		return 0
	case "list":
		return cmdTrustList(db, args[1:])
	}

	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	rationaleFlag := fs.String("rationale", "", "free-form rationale")
	helped, perr := parseSubcommandFlags(fs, args, "trust",
		"Set the operator's trust dial for an agent (0-100).",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--rationale T", Desc: "free-form rationale"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force trust astromech 90 --rationale solid recently"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: force trust <agent> <value> [--rationale <text>] [--operator <email>]")
		return 1
	}
	agent := rest[0]
	value, err := strconv.Atoi(rest[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "value must be 0-100: %v\n", err)
		return 1
	}
	if err := store.SetTrustDial(context.Background(), db, store.TrustDial{
		OperatorEmail: *operatorFlag,
		Agent:         agent,
		DialValue:     value,
		SetBy:         string(store.TrustDialOperator),
		Rationale:     *rationaleFlag,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "set trust failed: %v\n", err)
		return 1
	}
	fmt.Printf("OK — trust dial set: %s/%s = %d\n", *operatorFlag, agent, value)
	return 0
}

func cmdTrustList(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("trust list", flag.ContinueOnError)
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	helped, perr := parseSubcommandFlags(fs, args, "trust list",
		"List current trust dials for the given operator (or default@operator).",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force trust list"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	if err := store.BootstrapTrustDials(context.Background(), db, *operatorFlag); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		return 1
	}
	dials, err := store.ListCurrentTrustDials(context.Background(), db, *operatorFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		return 1
	}
	fmt.Printf("Trust dials for %s:\n", *operatorFlag)
	for _, d := range dials {
		fmt.Printf("  %-15s %3d  (set_by=%s, rationale=%s)\n", d.Agent, d.DialValue, d.SetBy, d.Rationale)
	}
	return 0
}
