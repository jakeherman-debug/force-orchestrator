// Package isb implements the Imperial Security Bureau (D4 Phase 2).
//
// ISB is the deterministic-first security scanner. It plugs into the
// same SecurityFindings substrate as BoS (Phase 1) and runs in parallel
// with BoSReview at commit-time. Each rule is either a Go AST check
// (mirror of the BoS shape) or a regex-based check that wraps a
// vendored security library (gosec, gitleaks). All rules ship at
// SeverityAdvise per the anti-cheat directive in docs/roadmap.md § D4
// ("No block-default on new rules"); promotion to SeverityBlock
// happens via FleetRules promotion after 30 clean firings.
//
// Rationale: docs/next-gen-agents.md § "Imperial Security Bureau (ISB)"
// + § "Bureau of Standards (BoS)" share the SecurityFindings table via
// the `bureau` column. The rule shape mirrors BoS deliberately — a
// future refactor can extract a shared interface in
// internal/security/rule.go. We chose to mirror rather than extract
// today because ISB's rule library is small enough that one round of
// duplication is cheaper than the up-front abstraction cost; the
// shared substrate is the SecurityFindings *table*, not the Go-side
// type. See "Design choice" in package comment of internal/isb/registry.go.
//
// All-Go decision: no Python tooling. semgrep-class checks are
// implemented as deterministic regex fallbacks per the operator's
// directive at the top of D4 Phase 2.
package isb

import (
	"go/ast"
	"go/types"
)

// Severity is the enforcement strength of a rule. Mirrors bos.Severity
// deliberately so the SecurityFindings.severity column accepts the
// same vocabulary regardless of bureau.
type Severity string

const (
	// SeverityAdvise records the finding but does not block the task.
	// Default for all ISB rules at launch (anti-cheat: no block-default).
	SeverityAdvise Severity = "advise"

	// SeverityBlock rejects the task until the violation is fixed or a
	// // ISB-BYPASS: AUDIT-NNN <reason> comment downgrades the finding.
	SeverityBlock Severity = "block"
)

// Finding is one rule violation against one source location. Each
// Finding is recorded as a row in SecurityFindings by the ISB reviewer.
type Finding struct {
	RuleID   string   // 'ISB-001', 'ISB-002', ...
	Severity Severity // copy of the rule's severity at scan time
	Path     string   // file path, repo-relative or absolute as appropriate
	Line     int      // 1-indexed line number of the violation
	Message  string   // operator-facing one-liner
}

// Rule is the ISB contract. Every rule under internal/isb/rules/
// implements this interface and registers itself via Register() in an
// init() function.
//
// AST rules (most of the 10) are pure analysis — no LLM calls, no I/O,
// no DB access. Scanner-backed rules (ISB-001 wraps gitleaks) call
// vendored Go libraries — no subprocess shell-out (per the all-Go
// decision in D4 Phase 2 scope).
type Rule interface {
	// ID returns the stable rule identifier (e.g. "ISB-001").
	ID() string

	// CLAUDEMDAnchor returns the CLAUDE.md section title (or audit ID
	// reference) this rule maps back to. Operator-facing for the audit
	// trail; not load-bearing on the run-time gate.
	CLAUDEMDAnchor() string

	// Severity returns the enforcement strength. Per anti-cheat: every
	// ISB rule returns SeverityAdvise at launch.
	Severity() Severity

	// Check inspects an AST + raw source and returns a slice of
	// Findings (empty slice if no violations). The path argument is the
	// file path; the source argument is the raw text (some rules need
	// it for line-precise regex matching that AST nodes can't express
	// — e.g., HardcodedSecretPatterns wraps gitleaks's DetectString).
	// types.Info may be nil if the caller only ran parser.ParseFile.
	Check(file *ast.File, path, source string, info *types.Info) []Finding
}
