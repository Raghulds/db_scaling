#!/usr/bin/env bash
# Pre-flight check: is this machine ready to run the labs?
set -uo pipefail
cd "$(dirname "$0")/.."
fail=0
ok()   { printf '  \033[32mok\033[0m    %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=1; }
warn() { printf '  \033[33mwarn\033[0m  %s\n' "$1"; }

echo "db-scaling doctor"

command -v docker >/dev/null && ok "docker $(docker --version | awk '{print $3}' | tr -d ,)" \
  || bad "docker not on PATH"

if docker info >/dev/null 2>&1; then
  ok "docker daemon reachable"
else
  bad "docker daemon is not running -- start Docker Desktop, then re-run"
fi

command -v docker >/dev/null && docker compose version >/dev/null 2>&1 \
  && ok "docker compose $(docker compose version --short)" \
  || bad "docker compose v2+ required"

command -v go >/dev/null && ok "go $(go version | awk '{print $3}')" || bad "go not on PATH"
command -v psql >/dev/null && ok "psql $(psql --version | awk '{print $3}')" \
  || warn "psql not on PATH -- SQL labs need it (brew install libpq)"
command -v pg_isready >/dev/null || warn "pg_isready not on PATH -- scripts/wait-for-pg.sh needs it"

for p in 55432 55440 55450 55460; do
  if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    warn "port $p already in use"
  fi
done

echo
[ "$fail" -eq 0 ] && echo "ready. next: make lab00" || echo "fix the FAILs above first."
exit "$fail"
