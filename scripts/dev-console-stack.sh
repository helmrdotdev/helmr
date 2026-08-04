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

if [ -z "${BUILD_POLICY_PATH:-}" ]; then
  echo "BUILD_POLICY_PATH must point to a canonical local build policy" >&2
  exit 1
fi

postgres_major_version() {
  postgres --version | awk '{ split($3, version, "."); print version[1] }'
}

started_pg=0
if [ -z "${DATABASE_URL:-}" ]; then
  for name in initdb pg_ctl postgres; do
    if ! command -v "${name}" >/dev/null 2>&1; then
      echo "${name} is required unless DATABASE_URL is already set" >&2
      exit 1
    fi
  done

  mkdir -p "${DEV_DIR}"
  pg_major="$(postgres_major_version)"
  if [ "${pg_major}" != "18" ]; then
    echo "PostgreSQL 18 is required for the managed dev database; found $(postgres --version)" >&2
    echo "Run via nix develop or set DATABASE_URL to a PostgreSQL 18 database." >&2
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

  export DATABASE_URL="postgres://${USER}@127.0.0.1:${PGPORT}/postgres?sslmode=disable"
  export HELMR_DEV_RESET_DATABASE="${HELMR_DEV_RESET_DATABASE:-1}"
fi

cleanup() {
  if [ -n "${controlplane_pid:-}" ]; then kill "${controlplane_pid}" >/dev/null 2>&1 || true; fi
  if [ -n "${console_pid:-}" ]; then kill "${console_pid}" >/dev/null 2>&1 || true; fi
  if [ "${started_pg}" = "1" ]; then pg_ctl -D "${PGDATA}" -m fast -w stop >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

export CONTROL_PLANE_ADDR="${CONTROL_PLANE_ADDR:-":8080"}"
export PUBLIC_URL="${PUBLIC_URL:-"http://${CONSOLE_HOST}:${CONSOLE_PORT}"}"
export REGION_ID="${REGION_ID:-"local"}"
export DEFAULT_REGION_ID="${DEFAULT_REGION_ID:-"${REGION_ID}"}"
export PROVIDER="${PROVIDER:-"local"}"
export PROVIDER_REGION="${PROVIDER_REGION:-"${REGION_ID}"}"
export REGION_DISPLAY_NAME="${REGION_DISPLAY_NAME:-"Local"}"
export WORKER_GROUP_ID="${WORKER_GROUP_ID:-"${REGION_ID}-worker-group-1"}"
export WORKER_GROUP_ENROLLMENT_SECRET_LOCAL="${WORKER_GROUP_ENROLLMENT_SECRET_LOCAL:-"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}"
export WORKER_RESOURCE_ID="${WORKER_RESOURCE_ID:-"local-worker"}"
if [ -z "${WORKER_GROUPS:-}" ]; then
  export WORKER_GROUPS="[{\"id\":\"${WORKER_GROUP_ID}\",\"name\":\"local\",\"enrollment_secret_env\":\"WORKER_GROUP_ENROLLMENT_SECRET_LOCAL\",\"allows_run\":true,\"allows_build\":true,\"observation_ttl_seconds\":120,\"instance_capacity\":{\"milli_cpu\":1000,\"memory_bytes\":1073741824,\"guest_ephemeral_disk_bytes\":1073741824,\"build_cache_bytes\":1073741824,\"artifact_cache_bytes\":1073741824,\"vm_slots\":1,\"build_executors\":1}}]"
fi
if ! bun -e 'const groups = JSON.parse(process.env.WORKER_GROUPS); if (!Array.isArray(groups) || groups.length === 0) process.exit(1)' >/dev/null; then
  echo "WORKER_GROUPS must be a non-empty JSON array" >&2
  exit 1
fi
case "${CONTROL_PLANE_ADDR}" in
  http://*|https://*) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"${CONTROL_PLANE_ADDR}"}" ;;
  :*) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"http://127.0.0.1${CONTROL_PLANE_ADDR}"}" ;;
  *) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"http://${CONTROL_PLANE_ADDR}"}" ;;
esac
export HELMR_DEV_CONSOLE_PORT="${CONSOLE_PORT}"

(
  cd "${ROOT}"
  go run ./cmd/internal/dev-controlplane
) &
controlplane_pid=$!

(
  cd "${ROOT}"
  bun run --cwd packages/console dev
) &
console_pid=$!

cat <<EOF

Helmr dev console stack is starting.

  Console:  ${PUBLIC_URL}
  Backend:  ${HELMR_DEV_BACKEND_URL}
  Login:    ${PUBLIC_URL}/dev/login

Open the Login URL to create a local developer session.
Press Ctrl-C to stop the stack.

EOF

while kill -0 "${controlplane_pid}" >/dev/null 2>&1 && kill -0 "${console_pid}" >/dev/null 2>&1; do
  sleep 1
done

cleanup
wait "${controlplane_pid}" >/dev/null 2>&1 || true
wait "${console_pid}" >/dev/null 2>&1 || true
exit 1
