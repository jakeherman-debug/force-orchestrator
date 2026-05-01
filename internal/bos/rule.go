// Package bos implements the Bureau of Standards (D4 Phase 1).
//
// BoS enforces CLAUDE.md invariants mechanically at commit-time. Every
// rule is a Go AST check living under internal/bos/rules/, registered
// via the package-level registry (registry.go), and surfaced to
// operators through the SecurityFindings table.
//
// Rationale: docs/next-gen-agents.md § "Bureau of Standards (BoS)"
//   "AST-first, not regex-first. Most invariants are structural, not
//    lexical. Function returns void AND body contains db.Exec is an
//    AST query. Every new Spawn* has an e-stop guard is an AST
//    pattern. Regex can't reliably express either."
//
// A rule whose body lives here but has no FleetRules row is NOT active
// — that's by design (anti-cheat per docs/roadmap.md § D4).
package bos

import (
	"go/ast"
	"go/types"
)

// Severity is the enforcement strength of a rule. New rules ship at
// SeverityAdvise per the anti-cheat directive in docs/roadmap.md
// § D4: "every new rule ships at severity=advise for 30 clean firings
// before promoting to block." BOS-011 is the sole exception — it
// graduates D0 Pattern P16's already-enforced check to commit-time.
type Severity string

const (
	// SeverityAdvise records the finding but does not block the task.
	// Default for all new BoS rules.
	SeverityAdvise Severity = "advise"

	// SeverityBlock rejects the task until the violation is fixed or
	// a // BOS-BYPASS: AUDIT-NNN comment downgrades the finding.
	SeverityBlock Severity = "block"
)

// Finding is one rule violation against one source location. Each
// Finding is recorded as a row in SecurityFindings by the BoS reviewer.
type Finding struct {
	RuleID   string   // 'BOS-001', 'BOS-002', ...
	Severity Severity // copy of the rule's severity at scan time
	Path     string   // file path, repo-relative or absolute as appropriate
	Line     int      // 1-indexed line number of the violation
	Message  string   // operator-facing one-liner
}

// Rule is the BoS contract. Every rule under internal/bos/rules/
// implements this interface and registers itself via Register() in an
// init() function.
//
// Rules are pure AST analysis — no LLM calls, no I/O, no DB access.
// That keeps the per-task cost ~free and makes the rule library
// trivially testable (red/green fixtures).
type Rule interface {
	// ID returns the stable rule identifier (e.g. "BOS-001").
	ID() string

	// CLAUDEMDAnchor returns the CLAUDE.md section title (or
	// invariant label) this rule enforces. Pattern P14 cross-checks
	// these against actual CLAUDE.md headings to prevent rule drift.
	CLAUDEMDAnchor() string

	// Severity returns the enforcement strength. New rules return
	// SeverityAdvise; only BOS-011 returns SeverityBlock at launch.
	Severity() Severity

	// Check inspects an AST and returns a slice of Findings (empty
	// slice if no violations). The path argument is the file path —
	// pure AST nodes don't carry their original filename, so the
	// caller threads it in for Finding.Path. types.Info may be nil if
	// the caller only ran parser.ParseFile (no type-check pass);
	// rules that need types.Info defensively check for nil.
	Check(file *ast.File, path string, info *types.Info) []Finding
}
