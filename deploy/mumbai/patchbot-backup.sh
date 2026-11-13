#!/bin/sh
# Consistent online SQLite backup (WAL-safe), keeps 14 days. Runs as patchbot.
set -eu
DATA=/srv/patchbot/data
OUT=/srv/patchbot/backups
mkdir -p "$OUT"; chmod 700 "$OUT"
DB="$DATA/patchbot.sqlite"
[ -f "$DB" ] || exit 0
F="$OUT/patchbot-$(date -u +%Y%m%dT%H%M%SZ).db"
sqlite3 "$DB" ".backup '$F'"
sqlite3 "$F" "PRAGMA integrity_check;" | grep -qx ok
chmod 600 "$F"
find "$OUT" -name 'patchbot-*.db' -mtime +14 -delete
