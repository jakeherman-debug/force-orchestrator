#!/usr/bin/env bash
# scripts/pre-commit/docs-check.sh
#
# D13 P3 — pre-commit hook: docs drift detection.
#
# This is a thin dispatcher shim. The actual validation lives in three
# Go test files under internal/audittools/:
#
#   - audit_pattern_p_docs_links_test.go        (TestPatternP_DocsBrokenLinks)
#   - audit_pattern_p_docs_orphan_test.go       (TestPatternP_DocsOrphan)
#   - audit_pattern_p_docs_architecture_test.go (TestPatternP_DocsArchitecture)
#
# The hook only runs when at least one *.md file is staged — typical
# Go-only commits don't pay any cost. When the slow path runs, it
# invokes `go test` directly (with -count=1 to bypass the cache) for
# the three test functions in question. Total budget on the slow path:
# ~3-5 seconds.
#
# Mirrors the project pattern (claude-md-size-check.sh, render-coherence-
# check.sh): shell shim → rich Go validation. The shim is the smallest
# thing that the dispatcher.sh master hook can pick up; richer logic
# is intentionally NOT inlined here.
#
# Exit codes:
#   0 — no *.md staged OR all three docs-drift tests pass
#   1 — hard reject (broken link, orphan doc, or architectural-invariant
#       drift detected)
#   2 — pre-flight failure (not a git repo, or `go` not in PATH)

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
  printf 'docs-check: must run from inside a git repo\n' >&2
  exit 2
fi

cd "$REPO_ROOT"

# ── Fast-path: skip unless an *.md file is staged ────────────────────
if ! git diff --cached --name-only --diff-filter=ACMR | grep -qE '\.md$'; then
  exit 0
fi

# ── Slow path: run the three docs-drift tests ────────────────────────
if ! command -v go >/dev/null 2>&1; then
  printf 'docs-check: WARN — `go` not in PATH; skipping docs drift gate.\n' >&2
  exit 0
fi

# Use the same -tags as the rest of the suite so a fresh checkout
# without sqlite_fts5 enabled doesn't bypass the gate via build error.
if ! go test -tags sqlite_fts5 -count=1 -timeout 120s \
      -run '^(TestPatternP_DocsBrokenLinks|TestPatternP_DocsOrphan|TestPatternP_DocsArchitecture|TestReadmeSizeUnder200Lines|TestDocsIndexExists|TestDocsSubdirsHaveIndex|TestMetadataBlockOnAllNewDocs)$' \
      ./internal/audittools/... 2>&1; then
  printf '\n' >&2
  printf 'docs-check: REJECT — docs drift detected.\n' >&2
  printf '\n' >&2
  printf '  At least one of the docs-tree drift detectors failed:\n' >&2
  printf '    - TestPatternP_DocsBrokenLinks    (every *.md link resolves)\n' >&2
  printf '    - TestPatternP_DocsOrphan         (every doc reachable from its index)\n' >&2
  printf '    - TestPatternP_DocsArchitecture   (H2 floor + auto-rendered exemption + index pointers)\n' >&2
  printf '    - TestReadme...  / TestDocs...    (README cap + index stubs + metadata blocks)\n' >&2
  printf '\n' >&2
  printf '  Fix options:\n' >&2
  printf '    1. Update the broken link to the correct path / anchor.\n' >&2
  printf '    2. Stub the missing target file (with metadata block + at least 4 H2 sections).\n' >&2
  printf '    3. Link the orphan file from the appropriate index README.\n' >&2
  printf '    4. Re-run the failed test for full output: `make docs-check`.\n' >&2
  printf '\n' >&2
  exit 1
fi

exit 0
