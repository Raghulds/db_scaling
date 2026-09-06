#!/usr/bin/env bash
# Block until a Postgres port answers, or fail after ~60s.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
port=${1:?usage: wait-for-pg.sh PORT}
for _ in $(seq 1 60); do
  if pg_isready -h "$PGHOST" -p "$port" -U "$PGUSER" -d "$PGDATABASE" -q; then
    echo "postgres ready on :$port"; exit 0
  fi
  sleep 1
done
echo "timed out waiting for postgres on :$port" >&2
exit 1
