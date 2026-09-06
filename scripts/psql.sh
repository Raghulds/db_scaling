#!/usr/bin/env bash
# Open psql against a lab database.
#   scripts/psql.sh              -> labs 00-03 node
#   scripts/psql.sh shard 2      -> shard 2
#   scripts/psql.sh citus
#   scripts/psql.sh pub | sub
# Extra args after the target are passed straight to psql.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh

target=${1:-lab}
case "$target" in
  lab)   port=$LAB_PORT; shift || true ;;
  shard) port=$((SHARD_BASE_PORT + ${2:-0})); shift 2 || true ;;
  citus) port=$CITUS_PORT; shift || true ;;
  pub)   port=$PUB_PORT; shift || true ;;
  sub)   port=$SUB_PORT; shift || true ;;
  *)     port=$LAB_PORT ;;
esac

exec psql -h "$PGHOST" -p "$port" -U "$PGUSER" -d "$PGDATABASE" "$@"
