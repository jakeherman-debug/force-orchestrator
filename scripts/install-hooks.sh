#!/usr/bin/env bash
# scripts/install-hooks.sh
#
# Operator-invoked one-shot installer for the Force-orchestrator git
# hooks. Run this once after cloning; subsequent commits will then go
# through the forceignore-check pre-commit gate.
#
# Usage:
#   ./scripts/install-hooks.sh
#   make hooks-install            # equivalent target in the Makefile
#
# The installer is intentionally NOT run automatically by any Force
# subcommand — that would alter the operator's git environment without
# explicit consent. Force commits during chunked development do not
# trigger the hook unless the operator has installed it themselves.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
  printf 'install-hooks: must be run from inside a git repo (got: %s)\n' "$(pwd)" >&2
  exit 1
fi

SOURCE="$REPO_ROOT/scripts/pre-commit/dispatcher.sh"
TARGET="$REPO_ROOT/.git/hooks/pre-commit"

if [[ ! -f "$SOURCE" ]]; then
  printf 'install-hooks: source not found: %s\n' "$SOURCE" >&2
  exit 1
fi

if [[ -e "$TARGET" && ! -L "$TARGET" ]]; then
  printf 'install-hooks: %s already exists and is not a symlink — refusing to overwrite\n' "$TARGET" >&2
  printf '  manually back up your existing hook, then re-run this installer.\n' >&2
  exit 1
fi

# chmod +x every check + the dispatcher so the freshly-cloned
# repository state is runnable.
chmod +x "$SOURCE"
for check in "$REPO_ROOT/scripts/pre-commit/"*-check.sh; do
  [[ -f "$check" ]] && chmod +x "$check"
done

# Use a relative symlink so the installation survives clone-to-clone
# moves. The dispatcher walks scripts/pre-commit/*-check.sh and runs
# each one in order — adding a new check is a one-file change with no
# installer update.
ln -sf "../../scripts/pre-commit/dispatcher.sh" "$TARGET"

printf 'install-hooks: installed %s -> %s\n' "$TARGET" "$SOURCE"
printf '  Pre-commit checks (run via dispatcher):\n'
for check in "$REPO_ROOT/scripts/pre-commit/"*-check.sh; do
  [[ -f "$check" ]] && printf '    %s\n' "$(basename "$check")"
done
printf '  Uninstall: rm %s\n' "$TARGET"
