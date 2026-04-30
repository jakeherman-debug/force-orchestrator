#!/bin/sh
# Mirror of setup-snapshots.sh — removes the marker entries (and the
# cron line that follows each marker) from the operator's crontab.
# Existing snapshot files in ~/.force/backups/ are left untouched.

set -eu

MARKER="# force-orchestrator hourly snapshot"

if ! crontab -l 2>/dev/null | grep -qF "$MARKER"; then
    echo "No snapshot crontab entries to remove."
    exit 0
fi

# Remove marker lines and the line immediately after each.
crontab -l 2>/dev/null | awk -v marker="$MARKER" '
    $0 ~ marker { skip=2; next }
    skip > 0 { skip--; next }
    { print }
' | crontab -

echo "Removed snapshot crontab entries."
echo "Existing snapshots in ~/.force/backups/ left untouched."
