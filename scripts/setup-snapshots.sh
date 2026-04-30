#!/bin/sh
# Idempotent installer for hourly holocron.db snapshots.
# Snapshots use sqlite3 .backup so they're WAL-consistent (point-in-time
# checkpoint of committed state, including any in-flight WAL pages).
# Cleanup removes snapshots older than 30 days at 04:00 daily.

set -eu

REPO="/Users/jake.herman/code/force-orchestrator"
BACKUP_DIR="$HOME/.force/backups"
MARKER="# force-orchestrator hourly snapshot"

mkdir -p "$BACKUP_DIR"

if crontab -l 2>/dev/null | grep -qF "$MARKER"; then
    echo "Snapshot crontab entries already installed."
    echo "Verify: crontab -l | grep force-orchestrator"
    exit 0
fi

# Append entries to existing crontab (preserve any other entries).
(
    crontab -l 2>/dev/null
    cat <<EOF
$MARKER
0 * * * * /usr/bin/sqlite3 $REPO/holocron.db ".backup $BACKUP_DIR/holocron.\$(date +\%Y\%m\%d-\%H).db" 2>/dev/null
$MARKER cleanup
0 4 * * * /usr/bin/find $BACKUP_DIR -name "holocron.*.db" -mtime +30 -delete 2>/dev/null
EOF
) | crontab -

echo "Installed:"
echo "  Hourly snapshot: $BACKUP_DIR/holocron.YYYYMMDD-HH.db"
echo "  Daily cleanup:   removes snapshots older than 30 days"
echo ""
echo "Verify: crontab -l | grep force-orchestrator"
