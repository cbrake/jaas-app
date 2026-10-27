#!/bin/sh
# Consistent SQLite snapshot of tgn-meetings, verified before it is kept.
# Run by tgn-meetings-backup.timer (daily). Keeps the newest $KEEP copies.
set -eu
umask 077

BASE=/opt/transgenia-meetings
DIR=$BASE/backups
KEEP=${KEEP:-14}
ts=$(date -u +%Y%m%dT%H%M%SZ)
tmp_name=".backup-$ts.db"

. "$BASE/.env" # IMAGE_TAG, TGN_UID, TGN_GID

# VACUUM INTO inside the running container: transactionally consistent and
# safe with the WAL in use.
docker exec tgn-meetings /app/jaas-app backup "/data/$tmp_name"
mv "$BASE/data/$tmp_name" "$DIR/meetings-$ts.db"
chown root:root "$DIR/meetings-$ts.db"
chmod 0600 "$DIR/meetings-$ts.db"

# Verify with the same image, without network, against a read-only copy.
docker run --rm --network none --read-only --user 0:0 \
	-v "$DIR/meetings-$ts.db:/check/meetings.db:ro" \
	"transgenia-meetings:$IMAGE_TAG" verify-db /check/meetings.db

sha256sum "$DIR/meetings-$ts.db" >"$DIR/meetings-$ts.db.sha256"

ls -1t "$DIR"/meetings-*.db 2>/dev/null | tail -n +$((KEEP + 1)) | while read -r old; do
	rm -f -- "$old" "$old.sha256"
done
echo "backup ok: $DIR/meetings-$ts.db"
