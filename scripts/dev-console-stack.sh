#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_DIR="${HELMR_DEV_DIR:-"${ROOT}/.helmr-dev"}"
PGDATA="${HELMR_DEV_PGDATA:-"${DEV_DIR}/postgres"}"
PGPORT="${HELMR_DEV_POSTGRES_PORT:-55432}"
PGLOG="${DEV_DIR}/postgres.log"
CONSOLE_HOST="127.0.0.1"
CONSOLE_PORT="${HELMR_DEV_CONSOLE_PORT:-3000}"

for name in go bun; do
  if ! command -v "${name}" >/dev/null 2>&1; then
    echo "${name} is required for the dev console stack" >&2
    exit 1
  fi
done

postgres_major_version() {
  postgres --version | awk '{ split($3, version, "."); print version[1] }'
}

started_pg=0
if [ -z "${HELMR_DATABASE_URL:-}" ]; then
  for name in initdb pg_ctl postgres; do
    if ! command -v "${name}" >/dev/null 2>&1; then
      echo "${name} is required unless HELMR_DATABASE_URL is already set" >&2
      exit 1
    fi
  done

  mkdir -p "${DEV_DIR}"
  pg_major="$(postgres_major_version)"
  if [ "${pg_major}" != "18" ]; then
    echo "PostgreSQL 18 is required for the managed dev database; found $(postgres --version)" >&2
    echo "Run via nix develop or set HELMR_DATABASE_URL to a PostgreSQL 18 database." >&2
    exit 1
  fi

  if [ -f "${PGDATA}/PG_VERSION" ] && [ "$(cat "${PGDATA}/PG_VERSION")" != "${pg_major}" ]; then
    archived_pgdata="${PGDATA}.postgres-$(cat "${PGDATA}/PG_VERSION").$(date +%Y%m%d%H%M%S)"
    echo "Archiving incompatible disposable dev database ${PGDATA} to ${archived_pgdata}" >&2
    mv "${PGDATA}" "${archived_pgdata}"
  fi

  if [ ! -d "${PGDATA}" ]; then
    initdb -D "${PGDATA}" -A trust >/dev/null
  fi

  if ! pg_ctl -D "${PGDATA}" status >/dev/null 2>&1; then
    pg_ctl -D "${PGDATA}" -l "${PGLOG}" -o "-p ${PGPORT} -c listen_addresses=127.0.0.1" -w start >/dev/null
    started_pg=1
  fi

  export HELMR_DATABASE_URL="postgres://${USER}@127.0.0.1:${PGPORT}/postgres?sslmode=disable"
  export HELMR_DEV_RESET_DATABASE="${HELMR_DEV_RESET_DATABASE:-1}"
fi

cleanup() {
  if [ -n "${control_pid:-}" ]; then kill "${control_pid}" >/dev/null 2>&1 || true; fi
  if [ -n "${console_pid:-}" ]; then kill "${console_pid}" >/dev/null 2>&1 || true; fi
  if [ "${started_pg}" = "1" ]; then pg_ctl -D "${PGDATA}" -m fast -w stop >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

export HELMR_CONTROL_ADDR="${HELMR_CONTROL_ADDR:-":8080"}"
export HELMR_PUBLIC_URL="${HELMR_PUBLIC_URL:-"http://${CONSOLE_HOST}:${CONSOLE_PORT}"}"
export HELMR_REGION_ID="${HELMR_REGION_ID:-"local"}"
export HELMR_DEFAULT_REGION_ID="${HELMR_DEFAULT_REGION_ID:-"${HELMR_REGION_ID}"}"
export HELMR_PROVIDER="${HELMR_PROVIDER:-"local"}"
export HELMR_PROVIDER_REGION="${HELMR_PROVIDER_REGION:-"${HELMR_REGION_ID}"}"
export HELMR_REGION_DISPLAY_NAME="${HELMR_REGION_DISPLAY_NAME:-"Local"}"
export HELMR_WORKER_GROUP_ID="${HELMR_WORKER_GROUP_ID:-"${HELMR_REGION_ID}-worker-group-1"}"
case "${HELMR_CONTROL_ADDR}" in
  http://*|https://*) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"${HELMR_CONTROL_ADDR}"}" ;;
  :*) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"http://127.0.0.1${HELMR_CONTROL_ADDR}"}" ;;
  *) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"http://${HELMR_CONTROL_ADDR}"}" ;;
esac
export HELMR_DEV_CONSOLE_PORT="${CONSOLE_PORT}"

(
  cd "${ROOT}"
  go run ./dev/control
) &
control_pid=$!

(
  cd "${ROOT}"
  bun run --cwd packages/console dev
) &
console_pid=$!

cat <<EOF

Helmr dev console stack is starting.

  Console:  ${HELMR_PUBLIC_URL}
  Backend:  ${HELMR_DEV_BACKEND_URL}
  Login:    ${HELMR_PUBLIC_URL}/dev/login

Open the Login URL to create a local developer session.
Press Ctrl-C to stop the stack.

EOF

while kill -0 "${control_pid}" >/dev/null 2>&1 && kill -0 "${console_pid}" >/dev/null 2>&1; do
  sleep 1
done

cleanup
wait "${control_pid}" >/dev/null 2>&1 || true
wait "${console_pid}" >/dev/null 2>&1 || true
exit 1
