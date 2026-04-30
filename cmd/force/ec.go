package main

// D3 Phase 3 — `force ec` CLI sub-commands.
//
// Mirrors the shape of cmd/force/experiment.go: thin dispatcher,
// per-subcommand FlagSet, and the operator-routed gates always require
// a --operator email (default: $USER@upstart.com to match
// `force experiment ratify`).
//
// Subcommands:
//
//	force ec list [--status pending|ratified|rejected|all] [--kind candidate|promote]
//	force ec ratify <id> [--operator email]
//	force ec reject <id> [--operator email] [--action ...] [--rationale ...] [--reason ...]
//	force ec status <id>
//
// The CLI talks to the same SQLite store the dashboard reads/writes,
// so a CLI ratify and a dashboard ratify produce identical row state
// (same conditional update + same AuditLog row).

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

func cmdEC(ctx context.Context, db *sql.DB, args []string) {
	if len(args) == 0 {
		ecUsage()
		os.Exit(1)
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		ecList(ctx, db, rest)
	case "ratify":
		ecRatify(ctx, db, rest)
	case "reject":
		ecReject(ctx, db, rest)
	case "status":
		ecStatus(ctx, db, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown ec subcommand: %s\n", sub)
		ecUsage()
		os.Exit(1)
	}
}

func ecUsage() {
	fmt.Fprintln(os.Stderr, `Usage: force ec <subcommand>

Subcommands:
  list [--status pending|ratified|rejected|all] [--kind candidate|promote]
        List PromotionProposals matching the filters (default: pending, all kinds).

  ratify <id> [--operator email]
        Operator-ratify a pending proposal. Defaults --operator to $USER@upstart.com.

  reject <id> [--operator email] [--action <act>] [--rationale <text>] [--reason <text>]
        Operator-reject a pending proposal.
        --action one of leave_as_is | clean_revert | cascade_revert | surgical_revert | escalate
                 (default: leave_as_is).
        --rationale ≥ 20 chars when --action != leave_as_is (concern #7).
        --reason free-form summary stored on the row; defaults to rationale or action.

  status <id>
        Print one proposal's current state (pending / ratified / rejected).`)
}

func ecList(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("ec list", flag.ExitOnError)
	statusFilter := fs.String("status", "pending", "pending|ratified|rejected|all")
	kindFilter := fs.String("kind", "", "candidate|promote (empty = both)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	query := `SELECT id, kind, IFNULL(rule_key,''), IFNULL(authored_by,''), IFNULL(authored_at,''),
	                 IFNULL(ratified_at,''), IFNULL(ratified_by,''),
	                 IFNULL(rejected_at,''), IFNULL(rejection_action,'')
	            FROM PromotionProposals WHERE 1=1`
	queryArgs := []any{}
	switch *statusFilter {
	case "pending":
		query += ` AND IFNULL(ratified_at,'') = '' AND IFNULL(rejected_at,'') = ''`
	case "ratified":
		query += ` AND IFNULL(ratified_at,'') != ''`
	case "rejected":
		query += ` AND IFNULL(rejected_at,'') != ''`
	case "all":
		// no filter
	default:
		fmt.Fprintf(os.Stderr, "ec list: --status must be pending|ratified|rejected|all (got %q)\n", *statusFilter)
		os.Exit(1)
	}
	if *kindFilter != "" {
		if *kindFilter != "candidate" && *kindFilter != "promote" {
			fmt.Fprintf(os.Stderr, "ec list: --kind must be candidate|promote (got %q)\n", *kindFilter)
			os.Exit(1)
		}
		query += ` AND kind = ?`
		queryArgs = append(queryArgs, *kindFilter)
	}
	query += ` ORDER BY id DESC`

	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec list: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	fmt.Printf("%-5s %-10s %-30s %-22s %-22s %s\n",
		"id", "kind", "rule_key", "authored_by", "authored_at", "state")
	for rows.Next() {
		var id int
		var kind, ruleKey, authoredBy, authoredAt string
		var ratifiedAt, ratifiedBy, rejectedAt, rejectionAction string
		if err := rows.Scan(&id, &kind, &ruleKey, &authoredBy, &authoredAt,
			&ratifiedAt, &ratifiedBy, &rejectedAt, &rejectionAction); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			continue
		}
		state := "pending"
		switch {
		case ratifiedAt != "":
			state = "ratified by " + ratifiedBy
		case rejectedAt != "":
			state = "rejected (" + rejectionAction + ")"
		}
		fmt.Printf("%-5d %-10s %-30s %-22s %-22s %s\n",
			id, kind, ruleKey, authoredBy, authoredAt, state)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "ec list: rows: %v\n", err)
	}
}

func ecRatify(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("ec ratify", flag.ExitOnError)
	operator := fs.String("operator", "", "operator email — required, recorded in AuditLog")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force ec ratify <id> [--operator email]")
		os.Exit(1)
	}
	id := mustParseID(fs.Arg(0))
	op := defaultOperatorEmail(*operator)
	if op == "" {
		fmt.Fprintln(os.Stderr, "ec ratify: --operator is required (operator-routed gate)")
		os.Exit(1)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE PromotionProposals
		   SET ratified_at = datetime('now'),
		       ratified_by = ?
		 WHERE id = ?
		   AND IFNULL(ratified_at,'') = ''
		   AND IFNULL(rejected_at,'') = ''
	`, op, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec ratify: update: %v\n", err)
		os.Exit(1)
	}
	n, err := res.RowsAffected()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec ratify: rows: %v\n", err)
		os.Exit(1)
	}
	if n == 0 {
		var exists int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM PromotionProposals WHERE id = ?`, id).Scan(&exists)
		if exists == 0 {
			fmt.Fprintf(os.Stderr, "ec ratify: proposal %d not found\n", id)
		} else {
			fmt.Fprintf(os.Stderr, "ec ratify: proposal %d is not pending — refusing to flip\n", id)
		}
		os.Exit(1)
	}
	store.LogAudit(db, op, "ec.ratify", id,
		fmt.Sprintf("Ratified PromotionProposal %d via CLI", id))
	fmt.Printf("Proposal %d ratified by %s.\n", id, op)
}

func ecReject(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("ec reject", flag.ExitOnError)
	operator := fs.String("operator", "", "operator email — required, recorded in AuditLog")
	action := fs.String("action", "leave_as_is", "leave_as_is | clean_revert | cascade_revert | surgical_revert | escalate")
	rationale := fs.String("rationale", "", "rejection rationale (≥ 20 chars when action != leave_as_is)")
	reason := fs.String("reason", "", "free-form rejected_reason; defaults to rationale or action")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force ec reject <id> [--operator email] [--action ...] [--rationale ...] [--reason ...]")
		os.Exit(1)
	}
	id := mustParseID(fs.Arg(0))
	op := defaultOperatorEmail(*operator)
	if op == "" {
		fmt.Fprintln(os.Stderr, "ec reject: --operator is required")
		os.Exit(1)
	}
	validActions := map[string]bool{
		"leave_as_is": true, "clean_revert": true,
		"cascade_revert": true, "surgical_revert": true, "escalate": true,
	}
	if !validActions[*action] {
		fmt.Fprintf(os.Stderr, "ec reject: invalid --action %q\n", *action)
		os.Exit(1)
	}
	rt := strings.TrimSpace(*rationale)
	if *action != "leave_as_is" && len(rt) < 20 {
		fmt.Fprintf(os.Stderr, "ec reject: --rationale must be ≥ 20 chars when --action != leave_as_is\n")
		os.Exit(1)
	}
	r := strings.TrimSpace(*reason)
	if r == "" {
		if rt != "" {
			r = rt
		} else {
			r = *action
		}
	}
	res, err := db.ExecContext(ctx, `
		UPDATE PromotionProposals
		   SET rejected_at = datetime('now'),
		       rejected_reason = ?,
		       rejection_action = ?,
		       rejection_rationale = ?
		 WHERE id = ?
		   AND IFNULL(ratified_at,'') = ''
		   AND IFNULL(rejected_at,'') = ''
	`, r, *action, rt, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec reject: update: %v\n", err)
		os.Exit(1)
	}
	n, err := res.RowsAffected()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec reject: rows: %v\n", err)
		os.Exit(1)
	}
	if n == 0 {
		var exists int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM PromotionProposals WHERE id = ?`, id).Scan(&exists)
		if exists == 0 {
			fmt.Fprintf(os.Stderr, "ec reject: proposal %d not found\n", id)
		} else {
			fmt.Fprintf(os.Stderr, "ec reject: proposal %d is not pending — refusing to flip\n", id)
		}
		os.Exit(1)
	}
	store.LogAudit(db, op, "ec.reject", id,
		fmt.Sprintf("Rejected PromotionProposal %d action=%s reason=%s via CLI", id, *action, r))
	fmt.Printf("Proposal %d rejected by %s (action=%s).\n", id, op, *action)
}

func ecStatus(ctx context.Context, db *sql.DB, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force ec status <id>")
		os.Exit(1)
	}
	id := mustParseID(args[0])
	var kind, ruleKey, authoredBy, authoredAt string
	var ratifiedAt, ratifiedBy, rejectedAt, rejectedReason, rejAction, rejRationale string
	var content string
	err := db.QueryRowContext(ctx, `
		SELECT kind, IFNULL(rule_key,''), IFNULL(authored_by,''), IFNULL(authored_at,''),
		       IFNULL(ratified_at,''), IFNULL(ratified_by,''),
		       IFNULL(rejected_at,''), IFNULL(rejected_reason,''),
		       IFNULL(rejection_action,''), IFNULL(rejection_rationale,''),
		       IFNULL(proposed_content,'')
		  FROM PromotionProposals WHERE id = ?
	`, id).Scan(&kind, &ruleKey, &authoredBy, &authoredAt,
		&ratifiedAt, &ratifiedBy, &rejectedAt, &rejectedReason,
		&rejAction, &rejRationale, &content)
	if err == sql.ErrNoRows {
		fmt.Fprintf(os.Stderr, "ec status: proposal %d not found\n", id)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec status: %v\n", err)
		os.Exit(1)
	}
	state := "pending"
	switch {
	case ratifiedAt != "":
		state = fmt.Sprintf("ratified at %s by %s", ratifiedAt, ratifiedBy)
	case rejectedAt != "":
		state = fmt.Sprintf("rejected at %s (action=%s, reason=%s)",
			rejectedAt, rejAction, rejectedReason)
	}
	fmt.Printf("Proposal %d — kind=%s\n", id, kind)
	fmt.Printf("  rule_key      %s\n", ruleKey)
	fmt.Printf("  authored_by   %s (origin convention)\n", authoredBy)
	fmt.Printf("  authored_at   %s\n", authoredAt)
	fmt.Printf("  state         %s\n", state)
	if rejRationale != "" {
		fmt.Printf("  rationale     %s\n", rejRationale)
	}
	// Content: print the first 500 chars; full body via the dashboard.
	if len(content) > 500 {
		content = content[:500] + "…"
	}
	if content != "" {
		fmt.Printf("  content       %s\n", content)
	}
}

// defaultOperatorEmail mirrors experiment.go's behaviour: if --operator
// is empty AND $USER is set, default to $USER@upstart.com so the operator
// doesn't have to retype the flag every call.
func defaultOperatorEmail(flag string) string {
	if s := strings.TrimSpace(flag); s != "" {
		return s
	}
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u + "@upstart.com"
	}
	return ""
}
