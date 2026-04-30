#!/usr/bin/env bash
# scripts/pre-commit/render-coherence-check.sh
#
# D3 systemic — pre-commit drift gate, source-side.
#
# The existing claude-md-size-check.sh runs when a rendered file
# (CLAUDE.md / FIX-LOG.md / docs/*.md) is staged, catching the case
# where an operator hand-edits an auto-generated file. This check
# runs when an *audit-side* file is staged — fleet_rules_audit.go,
# the fixlog/ slice, fleet_rules_bootstrap.go, rule_renderer.go —
# catching the inverse case: editing the audit slice without re-running
# `make render-rules` to refresh the rendered output.
#
# Belt-and-suspenders alongside TestPattern_P18_RenderCoherence.
# Pattern P18 catches drift in `make test` / CI; this hook catches it
# one step earlier at commit time.
#
# Cheap fast-path: skip unless audit-relevant files are staged. Slow
# path runs only when the audit slice or its inputs are actually being
# modified.
#
# Exit codes:
#   0 — clean (no audit-relevant files staged, OR drift check passed)
#   1 — hard reject (drift detected; remedy printed)
#   2 — pre-flight failure (skipped with WARN; force binary missing)

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
  printf 'render-coherence-check: must run from inside a git repo\n' >&2
  exit 2
fi

cd "$REPO_ROOT"

# ── Fast-path: skip unless audit-side inputs are staged ──────────────
# Match anything under fixlog/ (the FIX-LOG.md narrative slices) and
# the three source files whose edits change rendered output.
relevant_paths='internal/store/fleet_rules_audit\.go|internal/store/fixlog/|internal/store/fleet_rules_bootstrap\.go|internal/agents/rule_renderer\.go'

if ! git diff --cached --name-only --diff-filter=ACM | grep -qE "$relevant_paths"; then
  exit 0
fi

# ── Slow path: locate or build the force binary, then drift-check ────
force_bin=""
[[ -x ./bin/force ]] && force_bin=./bin/force
[[ -z "$force_bin" && -x ./force ]] && force_bin=./force

if [[ -z "$force_bin" ]]; then
  printf 'render-coherence-check: WARN — no force binary present (skipping coherence check).\n' >&2
  printf '  Audit-relevant files are staged but the hook cannot validate\n' >&2
  printf '  whether rendered output drifts from the audit slice.\n' >&2
  printf '  Run `make build` (or `go build -tags sqlite_fts5 -o force ./cmd/force/`)\n' >&2
  printf '  before re-committing so the hook can validate coherence.\n' >&2
  exit 0
fi

if ! "$force_bin" render-rules --check >/dev/null 2>&1; then
  printf '\n' >&2
  printf 'render-coherence-check: REJECT — rendered docs drift from audit slice.\n' >&2
  printf '\n' >&2
  printf '  Audit-relevant files are staged, but the rendered output\n' >&2
  printf '  (CLAUDE.md / FIX-LOG.md / docs/*.md) does not match the\n' >&2
  printf '  audit-slice render against a fresh in-memory DB.\n' >&2
  printf '\n' >&2
  printf '  Fix:\n' >&2
  printf '    1. make render-rules\n' >&2
  printf '    2. git add CLAUDE.md FIX-LOG.md docs/    # whichever changed\n' >&2
  printf '    3. git commit\n' >&2
  printf '\n' >&2
  printf '  If you are intentionally hand-editing an auto-generated file,\n' >&2
  printf '  edit the audit slice (internal/store/fleet_rules_audit.go) instead.\n' >&2
  exit 1
fi

exit 0
