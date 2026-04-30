#!/usr/bin/env bash
# scripts/pre-commit/dispatcher.sh
#
# Master pre-commit hook that runs every `*-check.sh` in this directory
# in stable (lexicographic) order. The first script to exit non-zero
# rejects the commit; remaining checks are skipped (no point continuing
# once one has refused).
#
# Operators install this via `make hooks-install`. Adding a new check is
# a one-file change: drop a new `<name>-check.sh` (chmod +x) under
# `scripts/pre-commit/` and the dispatcher picks it up on the next
# commit. No need to modify the installer.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
  printf 'pre-commit dispatcher: must run from inside a git repo\n' >&2
  exit 1
fi

CHECK_DIR="$REPO_ROOT/scripts/pre-commit"
shopt -s nullglob
checks=( "$CHECK_DIR"/*-check.sh )

if [[ ${#checks[@]} -eq 0 ]]; then
  # Nothing to run.
  exit 0
fi

for check in "${checks[@]}"; do
  if [[ ! -x "$check" ]]; then
    chmod +x "$check"
  fi
  if ! "$check" "$@"; then
    exit $?
  fi
done

exit 0
