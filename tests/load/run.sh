#!/usr/bin/env bash
# End to end load run: stack up, reset provider, k6 burst, drain wait, measurements, stack down.
# Exits non zero when the rate limit check (or any step) fails. Needs docker, curl and go.
set -u
cd "$(dirname "$0")/../.."

TOTAL="${TOTAL:-10000}"
DRAIN_TIMEOUT="${DRAIN_TIMEOUT:-900}"
MOCK="http://localhost:9090"
rc=0

# docker-compose.yml pins the database and Redis URLs; PROVIDER_URL is still read from the host,
# so unset it to make sure the burst only ever hits the local mock provider.
unset DATABASE_URL REDIS_URL PROVIDER_URL

psql() { docker compose exec -T postgres psql -U notif -d notif -At -F ' ' -c "$1"; }

cleanup() { docker compose down -v >/dev/null 2>&1; }
trap cleanup EXIT

docker compose up -d --build --wait || { echo "compose up failed"; exit 1; }

curl -fsS -X POST "$MOCK/_control/reset" >/dev/null || { echo "provider reset failed"; exit 1; }
echo "provider config: $(curl -fsS "$MOCK/_control/config")"

t0=$(date +%s)
docker run --rm -i --add-host=host.docker.internal:host-gateway \
  -e TOTAL="$TOTAL" -e BASE_URL=http://host.docker.internal:8080 \
  grafana/k6:2.4.0 run - < tests/load/burst.js || rc=1
t1=$(date +%s)
echo "burst phase: $((t1 - t0))s"
[ "$rc" -eq 0 ] || { echo "k6 failed"; exit 1; }

deadline=$((t1 + DRAIN_TIMEOUT))
while :; do
  open=$(psql "SELECT count(*) FROM notifications WHERE status IN ('pending','processing')") || { echo "psql failed"; exit 1; }
  [ "$open" = "0" ] && break
  if [ "$(date +%s)" -ge "$deadline" ]; then echo "drain timeout with $open still open"; rc=1; break; fi
  echo "open=$open"
  sleep 10
done
t2=$(date +%s)
echo "drain phase: $((t2 - t1))s, total $((t2 - t0))s"

echo "--- status counts"
psql "SELECT status, count(*) FROM notifications GROUP BY status ORDER BY status"
echo "--- per channel: channel sent first_sent last_sent rate_per_s p50_s p95_s"
psql "SELECT channel, count(*), min(sent_at), max(sent_at),
  round((count(*) / greatest(extract(epoch FROM max(sent_at) - min(sent_at)), 1))::numeric, 1),
  round((percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM sent_at - created_at)))::numeric, 2),
  round((percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM sent_at - created_at)))::numeric, 2)
  FROM notifications WHERE status = 'sent' GROUP BY channel ORDER BY channel"
echo "--- all channels end to end latency: p50_s p95_s"
psql "SELECT round((percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM sent_at - created_at)))::numeric, 2),
  round((percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM sent_at - created_at)))::numeric, 2)
  FROM notifications WHERE status = 'sent'"
echo "--- rate limit check"
go run ./tests/load/arrivals -url "$MOCK/_control/arrivals" || rc=1

echo "RESULT: $([ "$rc" -eq 0 ] && echo PASS || echo FAIL)"
exit "$rc"
