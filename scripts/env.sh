# Shared connection settings for every script in this repo.
# Source it, don't execute it.
export PGUSER=${PGUSER:-lab}
export PGPASSWORD=${PGPASSWORD:-lab}
export PGDATABASE=${PGDATABASE:-lab}
export PGHOST=${PGHOST:-127.0.0.1}

# labs 00-03
export LAB_PORT=${LAB_PORT:-55432}
# labs 04-05
export SHARD_BASE_PORT=${SHARD_BASE_PORT:-55440}
# lab 06
export CITUS_PORT=${CITUS_PORT:-55450}
# lab 07
export PUB_PORT=${PUB_PORT:-55460}
export SUB_PORT=${SUB_PORT:-55461}

# Every DSN the Go code needs, derived from the above.
shard_dsn() { # $1 = shard index
  echo "postgres://${PGUSER}:${PGPASSWORD}@${PGHOST}:$((SHARD_BASE_PORT + $1))/${PGDATABASE}?sslmode=disable"
}
