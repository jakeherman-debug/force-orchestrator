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
	case "--help", "-h", "help":
		ecUsage()
		return
	case "list":
		ecList(ctx, db, rest)
	case "ratify":
		ecRatify(ctx, db, rest)
	case "reject":
		ecReject(ctx, db, rest)
	case "status":
		ecStatus(ctx, db, rest)
	case "promote":
		ecPromote(ctx, db, rest)
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

  promote <id> --scope <global|tag:<tagname>|repo:<reponame>>
        Promote an EC experiment to a FleetRule with explicit scope. Ratifies
        the PromotionProposal and sets FleetRules.agent_scope.
        --scope global      → agent_scope = 'senate:*'
        --scope tag:X       → agent_scope = 'senate:tag:X'
        --scope repo:X      → agent_scope = 'senate:X'
        Omitting --scope defaults to repo scope (uses the proposal's rule_key).

  status <id>
        Print one proposal's current state (pending / ratified / rejected).`)
}

func ecList(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("ec list", flag.ContinueOnError)
	statusFilter := fs.String("status", "pending", "pending|ratified|rejected|all")
	kindFilter := fs.String("kind", "", "candidate|promote (empty = both)")
	helped, perr := parseSubcommandFlags(fs, args, "ec list",
		"List PromotionProposals matching --status / --kind filters.",
		[]flagDoc{
			{Name: "--status S", Desc: "pending|ratified|rejected|all"},
			{Name: "--kind K", Desc: "candidate|promote (empty = both)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force ec list", "force ec list --status all"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
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
	fs := flag.NewFlagSet("ec ratify", flag.ContinueOnError)
	operator := fs.String("operator", "", "operator email — required, recorded in AuditLog")
	helped, perr := parseSubcommandFlags(fs, args, "ec ratify",
		"Operator-ratify a pending PromotionProposal.",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email (required, recorded in AuditLog)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force ec ratify 17", "force ec ratify 17 --operator jake@example.com"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
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
	fs := flag.NewFlagSet("ec reject", flag.ContinueOnError)
	operator := fs.String("operator", "", "operator email — required, recorded in AuditLog")
	action := fs.String("action", "leave_as_is", "leave_as_is | clean_revert | cascade_revert | surgical_revert | escalate")
	rationale := fs.String("rationale", "", "rejection rationale (≥ 20 chars when action != leave_as_is)")
	reason := fs.String("reason", "", "free-form rejected_reason; defaults to rationale or action")
	helped, perr := parseSubcommandFlags(fs, args, "ec reject",
		"Operator-reject a pending PromotionProposal.",
		[]flagDoc{
			{Name: "--operator E", Desc: "operator email (required, recorded in AuditLog)"},
			{Name: "--action A", Desc: "leave_as_is | clean_revert | cascade_revert | surgical_revert | escalate"},
			{Name: "--rationale T", Desc: "rejection rationale (≥ 20 chars when action != leave_as_is)"},
			{Name: "--reason T", Desc: "free-form rejected_reason"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force ec reject 17 --operator jake@example.com"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
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
	fs := flag.NewFlagSet("ec status", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "ec status",
		"Print one PromotionProposal's current state.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force ec status 17"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force ec status <id>")
		os.Exit(1)
	}
	id := mustParseID(rest[0])
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

// ecPromote handles `force ec promote <id> --scope <global|tag:<t>|repo:<r>>`.
//
// It ratifies the PromotionProposal and, when the proposal's rule_key maps to
// an active FleetRules row, updates agent_scope on that row.
//
// Scope mapping:
//
//	--scope global    → agent_scope = "senate:*"
//	--scope tag:X     → agent_scope = "senate:tag:X"
//	--scope repo:X    → agent_scope = "senate:X"
//	(omitted)         → falls back to ecRatify behaviour (repo scope, rule_key as key)
func ecPromote(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("ec promote", flag.ContinueOnError)
	operator := fs.String("operator", "", "operator email — recorded in AuditLog")
	scopeFlag := fs.String("scope", "", "global|tag:<tagname>|repo:<reponame> (omit = repo scope)")
	helped, perr := parseSubcommandFlags(fs, args, "ec promote",
		"Promote an EC experiment to a FleetRule with explicit scope.",
		[]flagDoc{
			{Name: "--scope S", Desc: "global|tag:<t>|repo:<r> (default: repo scope from rule_key)"},
			{Name: "--operator E", Desc: "operator email (default: $USER@upstart.com)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force ec promote 17 --scope global",
			"force ec promote 17 --scope tag:payments",
			"force ec promote 17 --scope repo:myrepo",
		})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force ec promote <id> [--scope <global|tag:<t>|repo:<r>>]")
		os.Exit(1)
	}
	id := mustParseID(fs.Arg(0))
	op := defaultOperatorEmail(*operator)
	if op == "" {
		fmt.Fprintln(os.Stderr, "ec promote: --operator is required (operator-routed gate)")
		os.Exit(1)
	}

	// Load the proposal to get rule_key.
	var ruleKey string
	err := db.QueryRowContext(ctx,
		`SELECT IFNULL(rule_key,'') FROM PromotionProposals WHERE id = ?`, id,
	).Scan(&ruleKey)
	if err == sql.ErrNoRows {
		fmt.Fprintf(os.Stderr, "ec promote: proposal %d not found\n", id)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec promote: load proposal: %v\n", err)
		os.Exit(1)
	}

	// Resolve the target agent_scope.
	var agentScope string
	switch {
	case *scopeFlag == "" || *scopeFlag == "repo" || strings.HasPrefix(*scopeFlag, "repo:"):
		// Default: repo scope. Use the rule_key as the repo identifier if no
		// explicit repo name is given.
		if strings.HasPrefix(*scopeFlag, "repo:") {
			repoName := strings.TrimPrefix(*scopeFlag, "repo:")
			agentScope = "senate:" + repoName
		} else if ruleKey != "" {
			// Fallback: derive repo from rule_key (convention: rule_key starts with
			// the repo name or is the repo name itself for simple senate rules).
			agentScope = "senate:" + ruleKey
		} else {
			fmt.Fprintln(os.Stderr, "ec promote: cannot derive repo scope — provide --scope repo:<name> explicitly")
			os.Exit(1)
		}
	case *scopeFlag == "global":
		agentScope = "senate:*"
	case strings.HasPrefix(*scopeFlag, "tag:"):
		tagName := strings.TrimPrefix(*scopeFlag, "tag:")
		if strings.TrimSpace(tagName) == "" {
			fmt.Fprintln(os.Stderr, "ec promote: --scope tag: requires a non-empty tag name")
			os.Exit(1)
		}
		agentScope = "senate:tag:" + tagName
	default:
		// Accept the full "senate:..." form directly.
		if err2 := validateRuleScope(*scopeFlag); err2 != nil {
			fmt.Fprintf(os.Stderr, "ec promote: invalid --scope: %v\n", err2)
			os.Exit(1)
		}
		agentScope = *scopeFlag
	}

	// Ratify the proposal (same CAS shape as ecRatify).
	res, err := db.ExecContext(ctx, `
		UPDATE PromotionProposals
		   SET ratified_at = datetime('now'),
		       ratified_by = ?
		 WHERE id = ?
		   AND IFNULL(ratified_at,'') = ''
		   AND IFNULL(rejected_at,'') = ''
	`, op, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec promote: ratify: %v\n", err)
		os.Exit(1)
	}
	n, err := res.RowsAffected()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ec promote: rows: %v\n", err)
		os.Exit(1)
	}
	if n == 0 {
		var exists int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM PromotionProposals WHERE id = ?`, id).Scan(&exists)
		if exists == 0 {
			fmt.Fprintf(os.Stderr, "ec promote: proposal %d not found\n", id)
		} else {
			fmt.Fprintf(os.Stderr, "ec promote: proposal %d is not pending — refusing to flip\n", id)
		}
		os.Exit(1)
	}

	// Update FleetRules.agent_scope when the rule_key maps to an active row.
	if ruleKey != "" {
		upRes, upErr := db.ExecContext(ctx,
			`UPDATE FleetRules SET agent_scope = ? WHERE rule_key = ? AND active_until = ''`,
			agentScope, ruleKey,
		)
		if upErr != nil {
			fmt.Fprintf(os.Stderr, "ec promote: update FleetRules scope: %v\n", upErr)
			// Non-fatal — proposal was already ratified; log and continue.
		} else if upN, _ := upRes.RowsAffected(); upN > 0 {
			fmt.Printf("FleetRules %q agent_scope → %q (%d row(s) updated).\n", ruleKey, agentScope, upN)
		}
	}

	store.LogAudit(db, op, "ec.promote", id,
		fmt.Sprintf("Promoted PromotionProposal %d scope=%s via CLI", id, agentScope))
	fmt.Printf("Proposal %d promoted by %s (scope=%s).\n", id, op, agentScope)
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
