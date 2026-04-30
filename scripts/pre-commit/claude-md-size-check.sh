#!/usr/bin/env bash
# scripts/pre-commit/claude-md-size-check.sh
#
# D3 Phase 1 — local pre-commit hook (NOT installed automatically; the
# operator opts in via `make hooks-install`).
#
# Goal: refuse a commit that grows the rendered CLAUDE.md beyond the
# 20 KB hard cap, AND refuse a commit that hand-edits any auto-rendered
# file (CLAUDE.md / FIX-LOG.md / docs/* listed under render_to=
# 'per-domain-doc:*'). Drift detection runs `force render-rules --check`,
# which rebuilds every target in memory and compares to disk.
#
# Behaviour:
#   - If CLAUDE.md is staged:
#       (a) reject if `wc -c CLAUDE.md` > 20480.
#       (b) reject if it disagrees with the renderer's output (i.e.
#           the operator hand-edited the auto-generated file). Suggest
#           `make render-rules` instead.
#   - If any other auto-rendered file is staged: same drift check.
#
# Exit codes:
#   0 — all checks passed (no staged auto-rendered files OR drift OK)
#   1 — hard reject (over cap or drift detected)
#   2 — pre-flight failure (binary not built, etc.)

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
  printf 'claude-md-size-check: must run from inside a git repo\n' >&2
  exit 2
fi

cd "$REPO_ROOT"

CLAUDE_MD_HARD_CAP=20480

# ── Hard-cap check ────────────────────────────────────────────────────
# Independent of the staged set — defends against the case where the
# rendered file has grown without anyone touching it (very unlikely,
# but the cap is the only thing standing between us and unbounded
# universal-load growth).
if [[ -f CLAUDE.md ]]; then
  size=$(wc -c < CLAUDE.md | tr -d '[:space:]')
  if [[ "$size" -gt "$CLAUDE_MD_HARD_CAP" ]]; then
    printf 'claude-md-size-check: REJECT — CLAUDE.md is %d bytes; hard cap is %d.\n' \
      "$size" "$CLAUDE_MD_HARD_CAP" >&2
    printf '  The 10 KB Phase 1 target was tightened deliberately. To grow this\n' >&2
    printf '  budget, edit CLAUDE_MD_HARD_CAP / ClaudeMdHardCapBytes / TestPattern_PNN_ClaudeMdSize\n' >&2
    printf '  in lockstep — but first make sure the new content actually warrants\n' >&2
    printf '  universal-load placement (operator + Claude-Code-build + every review agent).\n' >&2
    exit 1
  fi
fi

# ── Drift check ───────────────────────────────────────────────────────
# Run only if at least one auto-rendered file is staged. Avoids paying
# the build cost on every commit.
staged="$(git diff --cached --name-only --diff-filter=ACM)"
needs_drift_check=0
while IFS= read -r path; do
  case "$path" in
    CLAUDE.md|FIX-LOG.md|docs/*.md)
      needs_drift_check=1
      break
      ;;
  esac
done <<< "$staged"

if [[ "$needs_drift_check" -eq 0 ]]; then
  exit 0
fi

if [[ ! -x ./force ]]; then
  # Build it. The hook is opt-in; the operator already accepted that
  # commits in this tree carry a small extra latency.
  go build -tags sqlite_fts5 -o ./force ./cmd/force/ >/dev/null 2>&1 || {
    printf 'claude-md-size-check: failed to build ./force binary (skipping drift check)\n' >&2
    exit 2
  }
fi

if ! ./force render-rules --check; then
  printf '\n' >&2
  printf 'claude-md-size-check: REJECT — drift detected. Either:\n' >&2
  printf '  - run `make render-rules` to regenerate from FleetRules, OR\n' >&2
  printf '  - if you want to change content, edit the FleetRules row in\n' >&2
  printf '    internal/store/fleet_rules_audit.go and re-render.\n' >&2
  exit 1
fi

exit 0
