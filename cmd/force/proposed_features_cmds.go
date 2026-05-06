// D3 fix-loop-1 β2 — `force proposed-features` CLI parity for the
// dashboard's /api/proposed-features mutating endpoints (Pattern P25).
//
// Subcommands:
//
//   force proposed-features list [--status <pending|promoted|archived|all>]
//   force proposed-features suppress <id> --rationale <txt> [--operator <email>] [--days <N>]
//   force proposed-features score <id> --value <low|medium|high> --complexity <low|medium|high>
//                                       --rationale <txt> [--operator <email>]
//   force proposed-features promote <id> [--deadline <date>] [--operator <email>]
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

func cmdProposedFeatures(db *sql.DB, args []string) int {
	// `--help` / `-h` short-circuit. Pre-fix-loop-2 the dispatcher
	// treated these as unknown verbs and exited 1, even though the
	// help text printed sensibly. Mirror sleep_hook_cmd.go's pattern:
	// print to stdout and exit 0 so tab-completion / man-page tooling
	// doesn't report a spurious failure.
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printProposedFeaturesUsage(os.Stdout)
		return 0
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "list":
		return cmdProposedFeaturesList(db, rest)
	case "suppress":
		return cmdProposedFeaturesSuppress(db, rest)
	case "score":
		return cmdProposedFeaturesScore(db, rest)
	case "promote":
		return cmdProposedFeaturesPromote(db, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown verb: %s (want list|suppress|score|promote)\n", verb)
		return 1
	}
}

// printProposedFeaturesUsage emits the help banner for the command
// group. Routed through io.Writer so the test in
// proposed_features_cmds_help_test.go can capture stdout exactly.
func printProposedFeaturesUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: force proposed-features <list|suppress|score|promote> [args]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Verbs:")
	fmt.Fprintln(w, "  list      [--status <pending|promoted|archived|all>]")
	fmt.Fprintln(w, "  suppress  <id> --rationale <txt> [--operator <email>] [--days <N>]")
	fmt.Fprintln(w, "  score     <id> --value <low|medium|high> --complexity <low|medium|high> --rationale <txt> [--operator <email>]")
	fmt.Fprintln(w, "  promote   <id> [--deadline <date>] [--operator <email>]")
}

func cmdProposedFeaturesList(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("proposed-features list", flag.ContinueOnError)
	statusFlag := fs.String("status", "", "pending|promoted|archived|all")
	helped, perr := parseSubcommandFlags(fs, args, "proposed-features list",
		"List proposed features (filterable by status).",
		[]flagDoc{
			{Name: "--status S", Desc: "pending|promoted|archived|all"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force proposed-features list"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	status := *statusFlag
	if status == "all" {
		status = ""
	}
	rows, err := store.ListProposedFeatures(db, status)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list failed: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Println("No proposed features.")
		return 0
	}
	for _, r := range rows {
		fmt.Printf("[%d] (%s) %s — value=%s complexity=%s occ=%d status=%s\n",
			r.ID, r.Source, truncatePF(r.ObservationSummary, 80),
			r.ValueScore, r.ComplexityScore, r.OccurrenceCount, r.Status)
	}
	return 0
}

func cmdProposedFeaturesSuppress(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("proposed-features suppress", flag.ContinueOnError)
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	rationaleFlag := fs.String("rationale", "", "rationale (≥ 20 chars)")
	daysFlag := fs.Int("days", 0, "suppression duration in days (0 = permanent)")
	helped, perr := parseSubcommandFlags(fs, args, "proposed-features suppress",
		"Suppress a proposed feature by fingerprint.",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--rationale T", Desc: "rationale (≥ 20 chars)"},
			{Name: "--days N", Desc: "suppression duration in days (0 = permanent)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force proposed-features suppress 7 --rationale not-needed-yet-this-quarter --days 30"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force proposed-features suppress <id> --rationale <txt> [--operator <email>] [--days <N>]")
		return 1
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(os.Stderr, "invalid feature id")
		return 1
	}
	operator := *operatorFlag
	rationale := *rationaleFlag
	days := *daysFlag
	if len(strings.TrimSpace(rationale)) < 20 {
		fmt.Fprintln(os.Stderr, "rationale must be ≥ 20 chars")
		return 1
	}
	// Resolve the fingerprint from the DB.
	var fp string
	err = db.QueryRow(`SELECT IFNULL(fingerprint,'') FROM ProposedFeatures WHERE id = ?`, id).Scan(&fp)
	if err == sql.ErrNoRows || fp == "" {
		fmt.Fprintf(os.Stderr, "feature %d not found or has empty fingerprint\n", id)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
		return 1
	}
	until := time.Time{}
	if days > 0 {
		until = time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
	}
	suppID, err := store.SuppressProposedFeature(db, fp, rationale, until, operator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "suppress failed: %v\n", err)
		return 1
	}
	store.LogAudit(db, operator, "proposed-feature-suppress", int(id),
		fmt.Sprintf("suppression %d installed via CLI", suppID))
	fmt.Printf("OK — suppression %d installed for fingerprint %s\n", suppID, fp)
	return 0
}

func cmdProposedFeaturesScore(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("proposed-features score", flag.ContinueOnError)
	valueFlag := fs.String("value", "", "low|medium|high")
	complexityFlag := fs.String("complexity", "", "low|medium|high")
	rationaleFlag := fs.String("rationale", "", "rationale (required)")
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	helped, perr := parseSubcommandFlags(fs, args, "proposed-features score",
		"Override the system-computed value/complexity scores for a proposed feature.",
		[]flagDoc{
			{Name: "--value V", Desc: "low|medium|high"},
			{Name: "--complexity V", Desc: "low|medium|high"},
			{Name: "--rationale T", Desc: "rationale (required)"},
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force proposed-features score 7 --value high --complexity low --rationale ..."})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force proposed-features score <id> [--value <low|medium|high>] [--complexity <low|medium|high>] --rationale <txt> [--operator <email>]")
		return 1
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(os.Stderr, "invalid feature id")
		return 1
	}
	value := *valueFlag
	complexity := *complexityFlag
	rationale := *rationaleFlag
	operator := *operatorFlag
	if rationale == "" {
		fmt.Fprintln(os.Stderr, "--rationale required")
		return 1
	}
	err = store.OverrideProposedFeatureScore(db, id, value, complexity, rationale, operator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "score override failed: %v\n", err)
		return 1
	}
	store.LogAudit(db, operator, "proposed-feature-score-override", int(id),
		fmt.Sprintf("value=%s complexity=%s via CLI", value, complexity))
	fmt.Printf("OK — feature %d score updated\n", id)
	return 0
}

func cmdProposedFeaturesPromote(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("proposed-features promote", flag.ContinueOnError)
	deadlineFlag := fs.String("deadline", "", "deadline (ISO date)")
	operatorFlag := fs.String("operator", "default@operator", "operator email")
	helped, perr := parseSubcommandFlags(fs, args, "proposed-features promote",
		"Promote a proposed feature to active status.",
		[]flagDoc{
			{Name: "--deadline D", Desc: "deadline (ISO date)"},
			{Name: "--operator E", Desc: "operator email"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force proposed-features promote 7 --deadline 2026-12-31"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force proposed-features promote <id> [--deadline <date>] [--operator <email>]")
		return 1
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(os.Stderr, "invalid feature id")
		return 1
	}
	deadline := *deadlineFlag
	operator := *operatorFlag
	if err := store.PromoteProposedFeature(db, id, deadline, operator); err != nil {
		fmt.Fprintf(os.Stderr, "promote failed: %v\n", err)
		return 1
	}
	store.LogAudit(db, operator, "proposed-feature-promote", int(id),
		fmt.Sprintf("deadline=%s via CLI", deadline))
	fmt.Printf("OK — feature %d promoted (deadline=%s)\n", id, deadline)
	return 0
}

// truncatePF is a small helper for list output. The internal/util package
// has TruncateStr but cmd/force/ doesn't otherwise import util; renamed
// to avoid shadowing watch.go's existing `truncate`.
func truncatePF(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
