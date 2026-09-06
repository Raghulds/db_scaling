#!/usr/bin/env bash
# Apply the per-shard schema to every running shard. Idempotent.
#   scripts/shards-init.sh          -> shards 0..3
#   SHARDS=8 scripts/shards-init.sh -> shards 0..7
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh

n=${SHARDS:-4}
for i in $(seq 0 $((n - 1))); do
  port=$((SHARD_BASE_PORT + i))
  scripts/wait-for-pg.sh "$port" >/dev/null
  echo "-- shard $i (:$port)"
  psql -h "$PGHOST" -p "$port" -U "$PGUSER" -d "$PGDATABASE" \
    -v ON_ERROR_STOP=1 -q -f labs/04-app-sharding-go/schema.sql
done
echo "initialised $n shards"
