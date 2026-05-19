package main

// senate_cmds.go — `force senate` CLI surface.
//
// D17 P2A: CLI parity for the senate roster and refresh surfaces.
//
//   force senate            — list all senators and their chamber status
//   force senate refresh <name> — queue a SenatorRefresh task for <name>

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

// cmdSenate dispatches `force senate [subcommand]`.
func cmdSenate(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 {
		subCmd = args[0]
	}
	switch subCmd {
	case "--help", "-h", "help":
		printSubcommandHelp(os.Stdout, "senate", "Inspect the Senate roster or queue a senator refresh.",
			[]flagDoc{
				{Name: "--help, -h", Desc: "show this help and exit"},
			},
			[]string{
				"force senate                     # list all senators",
				"force senate refresh <name>      # queue SenatorRefresh for a repo",
			})
	case "refresh":
		cmdSenateRefresh(db, args[1:])
	case "":
		cmdSenateList(db, args)
	default:
		fmt.Fprintf(os.Stderr, "Unknown senate subcommand: %s\n", subCmd)
		fmt.Fprintln(os.Stderr, "Usage: force senate [refresh <name>]")
		os.Exit(1)
	}
}

// cmdSenateList — `force senate`. Prints all SenateChambers rows.
func cmdSenateList(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("senate", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "senate",
		"List all senators and their chamber status.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force senate"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}

	chambers, err := store.ListAllSenateChambers(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(chambers) == 0 {
		fmt.Println("No senators registered. Run `force add-repo` to onboard a repo.")
		return
	}

	fmt.Printf("%-25s %-10s %-28s %-20s %s\n", "SENATOR", "STATUS", "SCOPE", "ONBOARDED", "LAST REFRESHED")
	fmt.Println(strings.Repeat("-", 110))
	for _, c := range chambers {
		refreshed := c.LastRefreshedAt
		if refreshed == "" {
			refreshed = "(never)"
		}
		fmt.Printf("%-25s %-10s %-28s %-20s %s\n",
			truncate(c.SenatorName, 25),
			c.Status,
			truncate(c.Scope, 28),
			truncate(c.OnboardedAt, 20),
			truncate(refreshed, 20),
		)
	}
	fmt.Printf("\n%d senator(s) total.\n", len(chambers))
}

// cmdSenateRefresh — `force senate refresh <name>`. Queues a SenatorRefresh
// task for the named repo's Senator. Idempotent: if a non-terminal
// SenatorRefresh already exists for the repo, the store deduplicates and
// reports accordingly.
func cmdSenateRefresh(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("senate refresh", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "senate refresh",
		"Queue a SenatorRefresh task to re-run knowledge-digest + rule/tag suggestions for a senator.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force senate refresh force-orchestrator"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force senate refresh <name>")
		os.Exit(1)
	}
	repoName := rest[0]

	// Verify the senator exists before queuing.
	chamber, err := store.GetSenateChamber(db, repoName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: GetSenateChamber: %v\n", err)
		os.Exit(1)
	}
	if chamber == nil {
		fmt.Fprintf(os.Stderr, "error: no senator for repo %q — run `force add-repo` to onboard it first\n", repoName)
		os.Exit(1)
	}

	// QueueSenatorRefresh returns (taskID, alreadyExisted, error).
	// alreadyExisted=true means the dedup gate fired; no new row was inserted.
	id, alreadyExisted, err := store.QueueSenatorRefresh(db, repoName, "operator-cli")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: QueueSenatorRefresh: %v\n", err)
		os.Exit(1)
	}
	if alreadyExisted {
		fmt.Printf("SenatorRefresh already pending for %q — no new task queued.\n", repoName)
		return
	}
	fmt.Printf("SenatorRefresh #%d queued for senator %q.\n", id, repoName)
}
