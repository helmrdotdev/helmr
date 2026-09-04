#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_DIR="${HELMR_DEV_DIR:-"${ROOT}/.helmr-dev"}"
PGDATA="${HELMR_DEV_PGDATA:-"${DEV_DIR}/postgres"}"
PGPORT="${HELMR_DEV_POSTGRES_PORT:-55432}"
PGLOG="${DEV_DIR}/postgres.log"
CONSOLE_HOST="127.0.0.1"
CONSOLE_PORT="${HELMR_DEV_CONSOLE_PORT:-3000}"
CONSOLE_MODE="${HELMR_DEV_CONSOLE_MODE:-live}"
redis_pid=""
clickhouse_pid=""

case "${CONSOLE_MODE}" in
  live|preview) ;;
  *)
    echo "HELMR_DEV_CONSOLE_MODE must be live or preview" >&2
    exit 1
    ;;
esac

if [ "${CONSOLE_MODE}" = "preview" ]; then
  unset DATABASE_URL
fi

required_commands=(go bun)
if [ "${CONSOLE_MODE}" = "preview" ]; then
  required_commands+=(clickhouse curl make redis-cli redis-server)
fi
for name in "${required_commands[@]}"; do
  if ! command -v "${name}" >/dev/null 2>&1; then
    echo "${name} is required for the dev console stack" >&2
    exit 1
  fi
done

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

  postgres_options="-p ${PGPORT} -c listen_addresses=127.0.0.1"
  if [ "${CONSOLE_MODE}" = "preview" ]; then
    PGSOCKET="${DEV_DIR}/postgres-socket"
    mkdir -p "${PGSOCKET}"
    chmod 700 "${PGSOCKET}"
    postgres_options="-p ${PGPORT} -c listen_addresses= -k ${PGSOCKET}"
  fi

  if ! pg_ctl -D "${PGDATA}" status >/dev/null 2>&1; then
    pg_ctl -D "${PGDATA}" -l "${PGLOG}" -o "${postgres_options}" -w start >/dev/null
    started_pg=1
  fi

  if [ "${CONSOLE_MODE}" = "preview" ]; then
    export DATABASE_URL="postgres://${USER}@/postgres?host=${PGSOCKET}&port=${PGPORT}&sslmode=disable"
  else
    export DATABASE_URL="postgres://${USER}@127.0.0.1:${PGPORT}/postgres?sslmode=disable"
  fi
  export HELMR_DEV_RESET_DATABASE="${HELMR_DEV_RESET_DATABASE:-1}"
fi

cleanup() {
  if [ -n "${controlplane_pid:-}" ]; then kill "${controlplane_pid}" >/dev/null 2>&1 || true; fi
  if [ -n "${console_pid:-}" ]; then kill "${console_pid}" >/dev/null 2>&1 || true; fi
  if [ -n "${redis_pid}" ]; then kill "${redis_pid}" >/dev/null 2>&1 || true; fi
  if [ -n "${clickhouse_pid}" ]; then kill "${clickhouse_pid}" >/dev/null 2>&1 || true; fi
  if [ "${started_pg}" = "1" ]; then pg_ctl -D "${PGDATA}" -m fast -w stop >/dev/null 2>&1 || true; fi
  if [ -n "${redis_pid}" ]; then wait "${redis_pid}" >/dev/null 2>&1 || true; fi
  if [ -n "${clickhouse_pid}" ]; then wait "${clickhouse_pid}" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

if [ "${CONSOLE_MODE}" = "preview" ]; then
  mkdir -p "${DEV_DIR}"
  runtime_descriptor="${DEV_DIR}/runtime.descriptor.json"
  printf '%s' '{"architecture":"x86_64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","formatVersion":0,"mediaType":"application/vnd.helmr.runtime.v0+squashfs","runtimeContract":"helmr.runtime.v0","sizeBytes":4096}' >"${runtime_descriptor}"
  export DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH="${runtime_descriptor}"

  redis_socket="${DEV_DIR}/redis.sock"
  rm -f "${redis_socket}"
  redis-server --port 0 --unixsocket "${redis_socket}" --unixsocketperm 700 --save '' --appendonly no \
    >"${DEV_DIR}/redis.log" 2>&1 &
  redis_pid=$!
  for _ in $(seq 1 50); do
    if redis-cli -s "${redis_socket}" ping 2>/dev/null | grep -qx PONG; then
      break
    fi
    if ! kill -0 "${redis_pid}" >/dev/null 2>&1; then
      cat "${DEV_DIR}/redis.log" >&2
      exit 1
    fi
    sleep 0.1
  done
  if ! redis-cli -s "${redis_socket}" ping 2>/dev/null | grep -qx PONG; then
    cat "${DEV_DIR}/redis.log" >&2
    exit 1
  fi
  export REDIS_URL="unix://${redis_socket}?db=0"

  clickhouse_host="127.0.0.1"
  if [ "$(uname -s)" = "Linux" ] && [ -n "${PASEO_PORT:-}" ]; then
    clickhouse_host="127.1.$((PASEO_PORT / 256)).$((PASEO_PORT % 256))"
  fi
  clickhouse_root="$(dirname "$(dirname "$(readlink -f "$(command -v clickhouse)")")")"
  export HELMR_DEV_CLICKHOUSE_HOST="${clickhouse_host}"
  export HELMR_DEV_CLICKHOUSE_PATH="${DEV_DIR}/clickhouse/"
  export HELMR_DEV_CLICKHOUSE_TMP_PATH="${DEV_DIR}/clickhouse/tmp/"
  export HELMR_DEV_CLICKHOUSE_USER_FILES_PATH="${DEV_DIR}/clickhouse/user_files/"
  export HELMR_DEV_CLICKHOUSE_FORMAT_SCHEMA_PATH="${DEV_DIR}/clickhouse/format_schemas/"
  export HELMR_DEV_CLICKHOUSE_ACCESS_PATH="${DEV_DIR}/clickhouse/access/"
  export HELMR_DEV_CLICKHOUSE_USERS_CONFIG="${clickhouse_root}/etc/clickhouse-server/users.xml"
  clickhouse server --config-file="${ROOT}/scripts/dev/clickhouse.xml" \
    >"${DEV_DIR}/clickhouse.log" 2>&1 &
  clickhouse_pid=$!
  for _ in $(seq 1 100); do
    if curl -fsS --max-time 1 "http://${clickhouse_host}:8123/ping" >/dev/null 2>&1; then
      break
    fi
    if ! kill -0 "${clickhouse_pid}" >/dev/null 2>&1; then
      cat "${DEV_DIR}/clickhouse.log" >&2
      exit 1
    fi
    sleep 0.1
  done
  if ! curl -fsS --max-time 1 "http://${clickhouse_host}:8123/ping" >/dev/null 2>&1; then
    cat "${DEV_DIR}/clickhouse.log" >&2
    exit 1
  fi
  export CLICKHOUSE_URL="http://${clickhouse_host}:8123"
fi

if [ "${CONSOLE_MODE}" = "preview" ]; then
  export CONTROL_PLANE_ADDR="${CONTROL_PLANE_ADDR:-"${CONSOLE_HOST}:${CONSOLE_PORT}"}"
else
  export CONTROL_PLANE_ADDR="${CONTROL_PLANE_ADDR:-":8080"}"
fi
export PUBLIC_URL="${PUBLIC_URL:-"http://${CONSOLE_HOST}:${CONSOLE_PORT}"}"
export BOOTSTRAP_ENABLED="${BOOTSTRAP_ENABLED:-"1"}"
export BOOTSTRAP_REGION_ID="${BOOTSTRAP_REGION_ID:-"local"}"
export BOOTSTRAP_REGION_DISPLAY_NAME="${BOOTSTRAP_REGION_DISPLAY_NAME:-"Local"}"
export BOOTSTRAP_WORKER_GROUP_NAME="${BOOTSTRAP_WORKER_GROUP_NAME:-"default"}"
export BOOTSTRAP_WORKER_TOKEN="${BOOTSTRAP_WORKER_TOKEN:-"hlmr_wgt_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}"
export WORKER_RESOURCE_ID="${WORKER_RESOURCE_ID:-"local-worker"}"
case "${CONTROL_PLANE_ADDR}" in
  http://*|https://*) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"${CONTROL_PLANE_ADDR}"}" ;;
  :*) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"http://127.0.0.1${CONTROL_PLANE_ADDR}"}" ;;
  *) export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-"http://${CONTROL_PLANE_ADDR}"}" ;;
esac
export HELMR_DEV_CONSOLE_PORT="${CONSOLE_PORT}"

if [ "${CONSOLE_MODE}" = "preview" ]; then
  (
    cd "${ROOT}"
    make console-build
    go run -tags embed_console ./cmd/internal/dev-controlplane
  ) &
else
  (
    cd "${ROOT}"
    go run ./cmd/internal/dev-controlplane
  ) &
fi
controlplane_pid=$!

if [ "${CONSOLE_MODE}" = "live" ]; then
  (
    cd "${ROOT}"
    bun run --cwd packages/console dev
  ) &
  console_pid=$!
fi

cat <<EOF

Helmr dev console stack is starting.

  Console:  ${PUBLIC_URL}
  Backend:  ${HELMR_DEV_BACKEND_URL}
  Login:    ${PUBLIC_URL}/dev/login

Open the Login URL to create a local developer session.
Press Ctrl-C to stop the stack.

EOF

while kill -0 "${controlplane_pid}" >/dev/null 2>&1; do
  if [ "${CONSOLE_MODE}" = "live" ] && ! kill -0 "${console_pid}" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

cleanup
wait "${controlplane_pid}" >/dev/null 2>&1 || true
if [ -n "${console_pid:-}" ]; then wait "${console_pid}" >/dev/null 2>&1 || true; fi
exit 1
