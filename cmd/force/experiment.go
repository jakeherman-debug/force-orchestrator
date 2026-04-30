package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"force-orchestrator/internal/experiments"
)

// cmdExperiment dispatches `force experiment <subcommand>`.
//
//	force experiment author <yaml-path>
//	force experiment ratify <id> [--operator email]
//	force experiment terminate <id> [--reason text]
//	force experiment status <id>
//	force experiment list [--status authored|running|terminated|all]
func cmdExperiment(ctx context.Context, db *sql.DB, args []string) {
	if len(args) == 0 {
		experimentUsage()
		os.Exit(1)
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "author":
		experimentAuthor(ctx, db, rest)
	case "ratify":
		experimentRatify(ctx, db, rest)
	case "terminate":
		experimentTerminate(ctx, db, rest)
	case "status":
		experimentStatus(ctx, db, rest)
	case "list":
		experimentList(ctx, db, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown experiment subcommand: %s\n", sub)
		experimentUsage()
		os.Exit(1)
	}
}

func experimentUsage() {
	fmt.Fprintln(os.Stderr, `Usage: force experiment <subcommand>

Subcommands:
  author <yaml-path>                       Author an experiment from a manifest YAML file.
  ratify <id> [--operator email]           Operator-pre-approve an authored experiment to start running.
  terminate <id> [--reason text]           Terminate a running experiment and compute its outcome.
  status <id>                              Show one experiment's lifecycle state and enrollment.
  list [--status running|terminated|all]   List experiments matching a status filter.`)
}

func experimentAuthor(ctx context.Context, db *sql.DB, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force experiment author <yaml-path>")
		os.Exit(1)
	}
	path := args[0]
	id, err := experiments.AuthorFromYAML(ctx, db, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "author: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Authored experiment %d from %s — status='authored', awaiting operator ratification.\n", id, path)
	fmt.Printf("  Next: force experiment ratify %d --operator <your.email@example.com>\n", id)
}

func experimentRatify(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("ratify", flag.ExitOnError)
	operator := fs.String("operator", "", "operator email — required, recorded in AuditLog")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force experiment ratify <id> [--operator email]")
		os.Exit(1)
	}
	id := mustParseID(fs.Arg(0))
	if strings.TrimSpace(*operator) == "" {
		// Default to $USER@upstart.com so operators don't have to
		// re-type the flag every time. Empty $USER falls through to
		// the Ratify error.
		if u := os.Getenv("USER"); u != "" {
			*operator = u + "@upstart.com"
		}
	}
	if err := experiments.Ratify(ctx, db, id, *operator); err != nil {
		fmt.Fprintf(os.Stderr, "ratify: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Experiment %d ratified by %s — now running.\n", id, *operator)
}

func experimentTerminate(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("terminate", flag.ExitOnError)
	reason := fs.String("reason", "operator_closed", "free-form reason recorded on the outcome row")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force experiment terminate <id> [--reason text]")
		os.Exit(1)
	}
	id := mustParseID(fs.Arg(0))
	if err := experiments.Terminate(ctx, db, id, *reason); err != nil {
		fmt.Fprintf(os.Stderr, "terminate: %v\n", err)
		os.Exit(1)
	}
	pid, err := experiments.MaybePromoteRule(ctx, db, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: terminate succeeded but promotion check failed: %v\n", err)
	}
	st, _ := experiments.GetStatus(ctx, db, id)
	fmt.Printf("Experiment %d terminated. Outcome: %s (winner_treatment_id=%d, posterior=%.4f)\n",
		id, st.OutcomeReason, st.WinnerTreatmentID, st.WinnerPosterior)
	if pid > 0 {
		fmt.Printf("Authored PromotionProposal %d for operator review.\n", pid)
	}
}

func experimentStatus(ctx context.Context, db *sql.DB, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force experiment status <id>")
		os.Exit(1)
	}
	id := mustParseID(args[0])
	st, err := experiments.GetStatus(ctx, db, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Experiment %d — %s\n", st.ID, st.Name)
	fmt.Printf("  status         %s\n", st.Status)
	fmt.Printf("  stakes_tier    %s\n", st.StakesTier)
	fmt.Printf("  subject_agent  %s\n", st.SubjectAgent)
	fmt.Printf("  assignment_unit %s\n", st.AssignmentUnit)
	if len(st.EnrollmentByArm) > 0 {
		fmt.Println("  enrollment by arm:")
		labels := make([]string, 0, len(st.EnrollmentByArm))
		for k := range st.EnrollmentByArm {
			labels = append(labels, k)
		}
		sort.Strings(labels)
		for _, l := range labels {
			fmt.Printf("    %-12s %d\n", l, st.EnrollmentByArm[l])
		}
	}
	if st.OutcomeReason != "" {
		fmt.Printf("  outcome        %s (winner_treatment=%d, posterior=%.4f)\n",
			st.OutcomeReason, st.WinnerTreatmentID, st.WinnerPosterior)
	}
}

func experimentList(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	statusFilter := fs.String("status", "all", "filter by status (authored|running|terminated|all)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	query := `SELECT id, name, status, IFNULL(subject_agent, ''), IFNULL(stakes_tier, ''), IFNULL(created_at, '') FROM Experiments`
	queryArgs := []any{}
	if *statusFilter != "all" {
		query += ` WHERE status = ?`
		queryArgs = append(queryArgs, *statusFilter)
	}
	query += ` ORDER BY id DESC`
	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	fmt.Printf("%-5s %-32s %-12s %-12s %-12s %s\n", "id", "name", "status", "agent", "tier", "created_at")
	for rows.Next() {
		var id int
		var name, status, agent, tier, created string
		if err := rows.Scan(&id, &name, &status, &agent, &tier, &created); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			continue
		}
		fmt.Printf("%-5d %-32s %-12s %-12s %-12s %s\n", id, name, status, agent, tier, created)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "list: rows: %v\n", err)
	}
}
