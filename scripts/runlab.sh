#!/usr/bin/env bash
# Run every numbered .sql file in a lab directory, in order, against the
# labs 00-03 node. Stops at the first error so a broken step is obvious.
#
#   scripts/runlab.sh labs/01-range-partitioning
#   ROWS=20000000 scripts/runlab.sh labs/00-baseline
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh

dir=${1:?usage: runlab.sh LAB_DIR}
port=${LAB_PORT}

scripts/wait-for-pg.sh "$port" >/dev/null

for f in "$dir"/[0-9][0-9]-*.sql; do
  [ -e "$f" ] || continue
  echo
  echo "=============================================================="
  echo "  $f"
  echo "=============================================================="
  psql -h "$PGHOST" -p "$port" -U "$PGUSER" -d "$PGDATABASE" \
    -v ON_ERROR_STOP=1 \
    -v rows="${ROWS:-5000000}" \
    -v tenants="${TENANTS:-500}" \
    -f "$f"
done
