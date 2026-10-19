#!/usr/bin/env bash
# Backup harian Postgres prod. Cron di server (jam 02:00 WIB):
#   0 2 * * * /opt/trytuist/backup.sh >> /opt/trytuist/backups/backup.log 2>&1
# Restore: gunzip -c FILE.sql.gz | docker compose -f docker-compose.prod.yml --env-file .env.prod exec -T db psql -U trytuist -d trytuist
set -euo pipefail
cd "$(dirname "$0")"
set -a; . ./.env.prod; set +a
DIR="${BACKUP_DIR:-./backups}"
mkdir -p "$DIR"
FILE="$DIR/trytuist-$(date +%Y%m%d-%H%M%S).sql.gz"

docker compose -f docker-compose.prod.yml --env-file .env.prod exec -T db \
  pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --clean --if-exists | gzip > "$FILE.tmp"
# File kosong / rusak tidak boleh menggantikan backup yang baik.
# pg_dump menulis baris penutup ini hanya bila dump selesai utuh (DB kosong pun tetap lolos).
gzip -t "$FILE.tmp"
gzip -cd "$FILE.tmp" | tail -n 5 | grep -q "PostgreSQL database dump complete" \
  || { echo "✗ dump tidak lengkap" >&2; exit 1; }
mv "$FILE.tmp" "$FILE"
echo "✓ backup $FILE ($(du -h "$FILE" | cut -f1))"

# Salinan di luar VPS: backup yang hanya ada di server ikut hilang bila server rusak.
if [ -n "${BACKUP_RCLONE_REMOTE:-}" ]; then
  rclone copy "$FILE" "$BACKUP_RCLONE_REMOTE"
  echo "✓ disalin ke $BACKUP_RCLONE_REMOTE"
fi
find "$DIR" -name 'trytuist-*.sql.gz' -mtime +"${BACKUP_KEEP_DAYS:-14}" -delete
