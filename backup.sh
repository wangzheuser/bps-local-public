#!/bin/bash
# bps-server backup.
#
# The archive contains bps.db plus the OAuth credential files in accounts/, so
# it is treated as secret material: owner-only directory and archive, and the
# destination is excluded from Git by .gitignore.
#
# SQLite runs in WAL mode, so the database is snapshotted with `.backup`
# instead of being copied file-by-file.
set -euo pipefail

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEST="${BPS_BACKUP_DIR:-$DIR/backups}"
RETAIN="${BPS_BACKUP_RETAIN:-7}"
TS=$(date -u +%Y%m%dT%H%M%SZ)
NAME="bps-server-$TS.tgz"

CONTAINER=$(docker compose -f "$DIR/docker-compose.yml" ps -q bps-server 2>/dev/null | head -1)
if [ -z "$CONTAINER" ]; then
	echo "bps-server container is not running; nothing to back up" >&2
	exit 1
fi

VOL=$(docker inspect "$CONTAINER" \
	-f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}')
if [ -z "$VOL" ]; then
	echo "cannot resolve the /data volume of $CONTAINER" >&2
	exit 1
fi

# 0700: the archive names and sizes alone are metadata worth hiding.
install -d -m 0700 "$DEST"

docker run --rm -v "$VOL":/d:ro -v "$DEST":/b alpine sh -c "
	set -e
	apk add --no-cache sqlite >/dev/null 2>&1
	mkdir -p /tmp/snap
	sqlite3 /d/bps.db '.backup /tmp/snap/bps.db'
	if [ -d /d/accounts ]; then cp -a /d/accounts /tmp/snap/accounts; fi
	# Sealing key: without it the encrypted API keys in the database are lost.
	if [ -f /d/api_key_secret ]; then cp -a /d/api_key_secret /tmp/snap/api_key_secret; fi
	tar czf /b/$NAME -C /tmp/snap .
	chmod 600 /b/$NAME
" >/dev/null

# Prune oldest archives beyond the retention window.
ls -1t "$DEST"/bps-server-*.tgz 2>/dev/null | tail -n +$((RETAIN + 1)) | xargs -r rm -f
echo "backup ok: $VOL -> $DEST/$NAME ($(du -h "$DEST/$NAME" | cut -f1))"
