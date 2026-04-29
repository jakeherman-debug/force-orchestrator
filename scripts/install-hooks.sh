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

SOURCE="$REPO_ROOT/scripts/pre-commit/forceignore-check.sh"
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

chmod +x "$SOURCE"

# Use a relative symlink so the installation survives clone-to-clone
# moves. The dirname dance computes the path from .git/hooks/ up to
# scripts/pre-commit/ regardless of where the repo lives.
ln -sf "../../scripts/pre-commit/forceignore-check.sh" "$TARGET"

printf 'install-hooks: installed %s -> %s\n' "$TARGET" "$SOURCE"
printf '  pre-commit checks will now run on every commit.\n'
printf '  Uninstall: rm %s\n' "$TARGET"
