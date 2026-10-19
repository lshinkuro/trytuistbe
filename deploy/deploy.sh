#!/usr/bin/env bash
# Dipanggil CI lewat SSH di server: ./deploy.sh <image:tag>
# Urutan: tarik image → backup → migrasi → ganti api → cek /healthz. Migrasi gagal = api lama tetap jalan.
set -euo pipefail
cd "$(dirname "$0")"
IMAGE="${1:?pakai: deploy.sh <image:tag>}"

sed -i.bak "s#^API_IMAGE=.*#API_IMAGE=${IMAGE}#" .env.prod && rm -f .env.prod.bak
compose() { docker compose -f docker-compose.prod.yml --env-file .env.prod "$@"; }

compose pull api
compose up -d db
./backup.sh                              # cadangan tepat sebelum skema berubah
compose run --rm migrate                 # gagal → set -e berhenti di sini, api lama tidak disentuh
compose up -d --no-deps api caddy

set -a; . ./.env.prod; set +a
for i in $(seq 1 30); do
  if curl -fsS "https://${API_DOMAIN}/healthz" >/dev/null; then
    echo "✓ ${IMAGE} live di https://${API_DOMAIN}"
    docker image prune -f >/dev/null
    exit 0
  fi
  sleep 2
done
echo "✗ /healthz tidak sehat setelah deploy" >&2
compose logs --tail=100 api >&2
exit 1
