#!/usr/bin/env bash
# scripts/pre-commit/forceignore-check.sh
#
# D1 T0-2 — local pre-commit hook (NOT installed automatically; the
# operator opts in via `make hooks-install`).
#
# Goal: refuse a commit that adds content matching one of the secret
# patterns the inbound-redact layer scrubs at runtime. The redact
# layer is a runtime defence; this hook closes the loop at write
# time so the repo's git history never contains the secret in the
# first place.
#
# Behaviour:
#   - Iterates the staged file set (`git diff --cached --name-only`).
#   - For each staged file, resolves symlinks (so `link → .env` is
#     treated as `.env`) and checks the path against `.forceignore`
#     using `git check-ignore` semantics — paths matched by
#     `.forceignore` are reported as a hard reject (you should not
#     be committing those at all).
#   - For paths NOT matched by `.forceignore`, scans the staged
#     content for a small inbound-secret pattern set: PEM block
#     markers, AWS access keys, GH PATs, GCP private_key JSON, and
#     SHELL_VAR=value lines whose LHS contains one of
#     API_KEY|SECRET|TOKEN|PASSWORD|PRIVATE_KEY|CREDENTIAL|AUTH.
#   - On any match: prints the offending file and exits 1.
#   - If `.forceignore` is absent, the hook is a no-op (no surprise
#     rejections in repos that haven't adopted the convention).
#
# Anti-cheat: the symlink resolver runs BEFORE pattern matching, so
# `link → .env` is rejected even if the operator never lists `link`
# in `.forceignore`.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
  # Not inside a git repo. Defer silently — there is nothing to gate.
  exit 0
fi

FORCEIGNORE="$REPO_ROOT/.forceignore"

# A missing `.forceignore` means the operator has not opted into the
# policy yet. Be permissive — refusing here would surprise contributors.
if [[ ! -f "$FORCEIGNORE" ]]; then
  exit 0
fi

# --------------------------------------------------------------------
# Helpers.

# resolve_symlink <path>: prints the resolved real path. We hop one
# level via `readlink` rather than `realpath` because realpath is not
# universally available on macOS without coreutils.
resolve_symlink() {
  local p="$1"
  if [[ -L "$p" ]]; then
    local target
    target="$(readlink "$p" 2>/dev/null || true)"
    if [[ -n "$target" ]]; then
      # Compose absolute path if target is relative.
      case "$target" in
        /*) printf '%s\n' "$target" ;;
        *)  printf '%s/%s\n' "$(dirname "$p")" "$target" ;;
      esac
      return
    fi
  fi
  printf '%s\n' "$p"
}

# matches_forceignore <relPath>: returns 0 if the path matches a
# .forceignore rule. Implementation uses `git check-ignore` with a
# transient `--exclude-from` because git itself does not auto-load
# .forceignore as an ignore source.
matches_forceignore() {
  local rel="$1"
  # Use git check-ignore in --no-index mode against an explicit
  # excludesFile pointing at .forceignore. -q is silent; exit 0 means
  # match, 1 means no match. Non-zero-non-1 (like 128) is unrelated
  # and we treat as no-match.
  local rc
  set +e
  git -c "core.excludesFile=$FORCEIGNORE" check-ignore -q --no-index -- "$rel"
  rc=$?
  set -e
  [[ $rc -eq 0 ]]
}

# --------------------------------------------------------------------
# Inbound-secret pattern set (mirrors internal/claude/inbound_redact.go).

declare -a CONTENT_PATTERNS=(
  '-----BEGIN (RSA |EC |DSA |OPENSSH |)PRIVATE KEY-----'
  'AKIA[0-9A-Z]{16}'
  '(ghp_|gho_|ghu_|ghs_|ghr_|github_pat_)[A-Za-z0-9_]{20,}'
  '"private_key"[[:space:]]*:[[:space:]]*"-----BEGIN'
  '\b((([A-Z_][A-Z0-9_]*_)?)(API_KEY|SECRET|TOKEN|PASSWORD|PRIVATE_KEY|CREDENTIAL|AUTH)((_[A-Z0-9_]+)?))[[:space:]]*=[[:space:]]*[^[:space:]]+'
)

# --------------------------------------------------------------------
# Main loop.

# Read the staged file list portably — `mapfile` requires bash 4+ which
# is not the default on macOS. Newline-delimited via a here-document so
# embedded spaces in filenames are preserved.
STAGED_LIST=""
STAGED_LIST="$(git diff --cached --name-only --diff-filter=ACMR 2>/dev/null || true)"

if [[ -z "$STAGED_LIST" ]]; then
  exit 0
fi

failures=0
while IFS= read -r rel; do
  [[ -z "$rel" ]] && continue
  abs="$REPO_ROOT/$rel"
  [[ -f "$abs" ]] || continue

  # Resolve symlinks (anti-cheat: link → .env must not bypass).
  resolved="$(resolve_symlink "$abs")"
  resolved_rel="${resolved#$REPO_ROOT/}"

  # Path-based gate via .forceignore.
  if matches_forceignore "$resolved_rel" || matches_forceignore "$rel"; then
    printf 'forceignore-check: REJECTED %s\n  -> matches a .forceignore rule; do not commit\n' "$rel" >&2
    failures=$((failures + 1))
    continue
  fi

  # Content-based gate.
  for pat in "${CONTENT_PATTERNS[@]}"; do
    if grep -nE -- "$pat" "$abs" >/dev/null 2>&1; then
      line="$(grep -nE -- "$pat" "$abs" | head -1)"
      printf 'forceignore-check: REJECTED %s\n  -> contains inbound-secret pattern\n  -> first match: %s\n' "$rel" "$line" >&2
      failures=$((failures + 1))
      break
    fi
  done
done <<< "$STAGED_LIST"

if [[ $failures -gt 0 ]]; then
  printf '\nforceignore-check: %d file(s) rejected. Resolve before committing.\n' "$failures" >&2
  printf '  - Add the path to .forceignore if it should never be tracked.\n' >&2
  printf '  - Rotate the secret if it was ever real.\n' >&2
  printf '  - Replace the secret with a placeholder if this is documentation.\n' >&2
  exit 1
fi

exit 0
