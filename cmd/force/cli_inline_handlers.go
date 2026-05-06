package main

// Wrappers for top-level CLI subcommands whose flag-parsing previously
// lived inline in main.go's dispatch switch. Each wrapper:
//
//   - Builds a flag.FlagSet with ContinueOnError
//   - Routes through parseSubcommandFlags so --help / -h prints help and
//     exits 0, and unknown flags are rejected with a non-zero exit
//     BEFORE any side-effect
//   - Reads positional args from fs.Args() after a successful parse
//
// Generalized from the D12 daemon-family fix (parseDaemonFlags) per
// fix(cli)/cli-flag-parsing.

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
)

// cmdList — `force list [statuses] [--status <s>] [--repo <name>] [--type <t>] [--limit N]`.
//
// The legacy parser accepted a single positional status (or "active") AND
// the named flags; we preserve that shape here. Unknown --flags reject;
// stray positional tokens after the first are rejected too (the legacy
// shape silently overwrote statusFilter on each non-flag token).
func cmdList(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	statusFlag := fs.String("status", "", "comma-separated statuses (e.g. Pending,Failed)")
	repoFlag := fs.String("repo", "", "filter by target_repo")
	typeFlag := fs.String("type", "", "filter by task type (Feature, CodeEdit, ...)")
	limitFlag := fs.Int("limit", 0, "max rows (0 = unlimited)")
	helped, perr := parseSubcommandFlags(fs, args, "list",
		"List BountyBoard rows. Filter by status / repo / type / limit.",
		[]flagDoc{
			{Name: "--status S", Desc: "comma-separated statuses (e.g. Pending,Failed)"},
			{Name: "--repo R", Desc: "filter by target_repo"},
			{Name: "--type T", Desc: "filter by task type"},
			{Name: "--limit N", Desc: "max rows (0 = unlimited)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force list", "force list active", "force list --status Failed --limit 20"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	statusFilter := *statusFlag
	if rest := fs.Args(); len(rest) > 0 {
		// Legacy shape: a single positional status (or "active").
		positional := rest[0]
		if strings.EqualFold(positional, "active") {
			statusFilter = "Pending,Locked,Planned,AwaitingCaptainReview,UnderCaptainReview,AwaitingCouncilReview,UnderReview,Failed,Escalated,ConflictPending"
		} else {
			statusFilter = positional
		}
	}
	printList(db, statusFilter, *repoFlag, *typeFlag, *limitFlag)
}

// cmdLogs — `force logs <task-id>`.
func cmdLogs(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "logs",
		"Print a task's error_log + recent task history.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force logs 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force logs <task-id>")
		os.Exit(1)
	}
	printLogs(db, mustParseID(rest[0]))
}

// cmdHistory — `force history [--full] <task-id>`.
func cmdHistory(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fullFlag := fs.Bool("full", false, "include the full output for each attempt")
	helped, perr := parseSubcommandFlags(fs, args, "history",
		"Print a task's attempt history. --full surfaces the per-attempt output.",
		[]flagDoc{
			{Name: "--full", Desc: "include the full output for each attempt"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force history 42", "force history --full 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force history [--full] <task-id>")
		os.Exit(1)
	}
	printHistory(db, mustParseID(rest[0]), *fullFlag)
}

// cmdAgents — `force agents`.
func cmdAgents(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "agents",
		"List currently-registered agents and their roles.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force agents"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	printAgents(db)
}

// cmdRunForeground — `force run <task-id>`.
func cmdRunForeground(db *sql.DB, args []string, fn func(int)) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "run",
		"Foreground claim + execute a single task with streamed output.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force run 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force run <task-id>")
		os.Exit(1)
	}
	fn(mustParseID(rest[0]))
}

// cmdBounty — `force bounty <subcommand>`.
func cmdBounty(db *sql.DB, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: force bounty <subcommand>\n  stats   — print bounty board statistics\n")
		os.Exit(1)
	}
	switch args[0] {
	case "stats":
		cmdBountyStats(db, args[1:])
	case "--help", "-h", "help":
		fmt.Fprintln(os.Stdout, "Usage: force bounty <subcommand>")
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Subcommands:")
		fmt.Fprintln(os.Stdout, "  stats   — print bounty board statistics")
	default:
		fmt.Fprintf(os.Stderr, "Unknown bounty subcommand: %s\nUsage: force bounty stats\n", args[0])
		os.Exit(1)
	}
}

// cmdTask — `force task <subcommand>`.
func cmdTask(db *sql.DB, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: force task <subcommand>\n  note <id> <text>  — append an operator note to a task\n")
		os.Exit(1)
	}
	switch args[0] {
	case "note":
		cmdTaskNote(db, args[1:])
	case "--help", "-h", "help":
		fmt.Fprintln(os.Stdout, "Usage: force task <subcommand>")
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Subcommands:")
		fmt.Fprintln(os.Stdout, "  note <id> <text>  — append an operator note to a task")
	default:
		fmt.Fprintf(os.Stderr, "Unknown task subcommand: %s\nUsage: force task note <id> <text>\n", args[0])
		os.Exit(1)
	}
}

// cmdRepo — `force repo <subcommand>`.
func cmdRepo(db *sql.DB, args []string, syncFn func([]string), setPRFlowFn func([]string)) {
	if len(args) == 0 {
		fmt.Println("Usage: force repo sync | force repo set-pr-flow <name> on|off")
		os.Exit(1)
	}
	switch args[0] {
	case "sync":
		syncFn(args[1:])
	case "set-pr-flow":
		setPRFlowFn(args[1:])
	case "--help", "-h", "help":
		fmt.Fprintln(os.Stdout, "Usage: force repo <subcommand>")
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Subcommands:")
		fmt.Fprintln(os.Stdout, "  sync                              — re-run PR-flow preflight + remote-info backfill")
		fmt.Fprintln(os.Stdout, "  set-pr-flow <name> on|off         — toggle pr_flow_enabled for a registered repo")
	default:
		fmt.Printf("Unknown repo subcommand: %s\n", args[0])
		fmt.Println("Usage: force repo sync | force repo set-pr-flow <name> on|off")
		os.Exit(1)
	}
}
