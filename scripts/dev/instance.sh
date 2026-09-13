# shellcheck shell=bash
# Shared Helmr local dev paths, ports, and helpers.
# Sourced by scripts/dev-console-stack.sh (not executed directly).

helmr_dev_init() {
  local root="$1"
  HELMR_DEV_ROOT="${root}"

  HELMR_DEV_DIR="${HELMR_DEV_DIR:-${root}/.helmr-dev}"
  mkdir -p "${HELMR_DEV_DIR}"
  HELMR_DEV_DIR="$(cd "${HELMR_DEV_DIR}" && pwd)"

  HELMR_DEV_STATE_HASH="$(printf '%s' "${HELMR_DEV_DIR}" | shasum -a 256 | awk '{print substr($1,1,8)}')"

  local tmp_base="${TMPDIR:-/tmp}"
  tmp_base="${tmp_base%/}"
  HELMR_DEV_RUNTIME_DIR="${tmp_base}/helmr-${HELMR_DEV_STATE_HASH}"

  HELMR_DEV_CAS_DIR="${HELMR_DEV_DIR}/cas"
  HELMR_DEV_LOCK_DIR="${HELMR_DEV_DIR}/.stack.lock"
  HELMR_DEV_LOCK_OWNED=0

  mkdir -p "${HELMR_DEV_DIR}" "${HELMR_DEV_RUNTIME_DIR}" "${HELMR_DEV_CAS_DIR}"

  local port_seed
  port_seed="$(printf '%s' "${HELMR_DEV_STATE_HASH}" | shasum -a 256 | awk '{print substr($1,1,4)}')"
  local port_base=$((3000 + (16#${port_seed} % 200) * 10))

  HELMR_DEV_CONSOLE_PORT="${HELMR_DEV_CONSOLE_PORT:-$((port_base + 0))}"
  HELMR_DEV_CONTROL_PLANE_PORT="${HELMR_DEV_CONTROL_PLANE_PORT:-$((port_base + 1))}"
  HELMR_DEV_POSTGRES_PORT="${HELMR_DEV_POSTGRES_PORT:-$((port_base + 2))}"
  HELMR_DEV_CLICKHOUSE_HTTP_PORT="${HELMR_DEV_CLICKHOUSE_HTTP_PORT:-$((port_base + 3))}"

  HELMR_DEV_CONSOLE_MODE="${HELMR_DEV_CONSOLE_MODE:-live}"
  HELMR_DEV_CONSOLE_HOST="127.0.0.1"

  HELMR_DEV_PGDATA="${HELMR_DEV_DIR}/postgres"
  HELMR_DEV_PGLOG="${HELMR_DEV_DIR}/postgres.log"
  HELMR_DEV_PGSOCKET="${HELMR_DEV_RUNTIME_DIR}/postgres-socket"

  HELMR_DEV_OWNED_POSTGRES=0
  HELMR_DEV_REDIS_PID=""
  HELMR_DEV_CLICKHOUSE_PID=""
  HELMR_DEV_CONTROLPLANE_PID=""
  HELMR_DEV_CONSOLE_PID=""

  case "${HELMR_DEV_CONSOLE_MODE}" in
    live|preview) ;;
    *)
      echo "HELMR_DEV_CONSOLE_MODE must be live or preview" >&2
      return 1
      ;;
  esac
}

helmr_dev_lock_holder_pid() {
  if [ -f "${HELMR_DEV_LOCK_DIR}/pid" ]; then
    cat "${HELMR_DEV_LOCK_DIR}/pid"
  fi
}

helmr_dev_acquire_lock() {
  if ! mkdir "${HELMR_DEV_LOCK_DIR}" 2>/dev/null; then
    local lock_pid
    lock_pid="$(helmr_dev_lock_holder_pid 2>/dev/null || true)"
    echo "Helmr dev stack lock already exists for ${HELMR_DEV_DIR}." >&2
    if [ -n "${lock_pid}" ]; then
      if kill -0 "${lock_pid}" >/dev/null 2>&1; then
        echo "Stack appears to be running (pid ${lock_pid}). Stop it with Ctrl-C or SIGTERM." >&2
      else
        echo "Lock pid ${lock_pid} is not running." >&2
      fi
    else
      echo "Lock exists but ${HELMR_DEV_LOCK_DIR}/pid is missing." >&2
    fi
    echo "Inspect owned services under ${HELMR_DEV_DIR}, stop any that remain," >&2
    echo "then remove ${HELMR_DEV_LOCK_DIR} manually before retrying." >&2
    return 1
  fi
  echo $$ >"${HELMR_DEV_LOCK_DIR}/pid"
  HELMR_DEV_LOCK_OWNED=1
}

helmr_dev_release_lock() {
  if [ "${HELMR_DEV_LOCK_OWNED}" = "1" ]; then
    rm -rf "${HELMR_DEV_LOCK_DIR}"
    HELMR_DEV_LOCK_OWNED=0
  fi
}

helmr_dev_port_listening() {
  local port="$1"
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1
    return
  fi
  (echo >/dev/tcp/127.0.0.1/"${port}") 2>/dev/null
}

helmr_dev_require_free_port() {
  local port="$1"
  local label="$2"
  if helmr_dev_port_listening "${port}"; then
    echo "${label} port ${port} is already in use on 127.0.0.1" >&2
    echo "Set an explicit HELMR_DEV_* port override or stop the conflicting process." >&2
    return 1
  fi
}

helmr_dev_require_ports() {
  helmr_dev_require_free_port "${HELMR_DEV_CONSOLE_PORT}" "Console"
  if [ "${HELMR_DEV_CONSOLE_MODE}" = "live" ]; then
    helmr_dev_require_free_port "${HELMR_DEV_CONTROL_PLANE_PORT}" "Control plane"
  fi
  if [ -z "${DATABASE_URL:-}" ]; then
    helmr_dev_require_free_port "${HELMR_DEV_POSTGRES_PORT}" "Postgres"
  fi
  if [ -z "${CLICKHOUSE_URL:-}" ]; then
    helmr_dev_require_free_port "${HELMR_DEV_CLICKHOUSE_HTTP_PORT}" "ClickHouse HTTP"
  fi
}

helmr_dev_postgres_major_version() {
  postgres --version | awk '{ split($3, version, "."); print version[1] }'
}

helmr_dev_start_postgres() {
  if [ -n "${DATABASE_URL:-}" ]; then
    return 0
  fi
  for name in initdb pg_ctl postgres; do
    if ! command -v "${name}" >/dev/null 2>&1; then
      echo "${name} is required unless DATABASE_URL is already set" >&2
      return 1
    fi
  done

  local pg_major
  pg_major="$(helmr_dev_postgres_major_version)"
  if [ "${pg_major}" != "18" ]; then
    echo "PostgreSQL 18 is required for the managed dev database; found $(postgres --version)" >&2
    echo "Run via nix develop or set DATABASE_URL to a PostgreSQL 18 database." >&2
    return 1
  fi

  if [ -f "${HELMR_DEV_PGDATA}/PG_VERSION" ] && [ "$(cat "${HELMR_DEV_PGDATA}/PG_VERSION")" != "${pg_major}" ]; then
    local archived_pgdata="${HELMR_DEV_PGDATA}.postgres-$(cat "${HELMR_DEV_PGDATA}/PG_VERSION").$(date +%Y%m%d%H%M%S)"
    echo "Archiving incompatible disposable dev database ${HELMR_DEV_PGDATA} to ${archived_pgdata}" >&2
    mv "${HELMR_DEV_PGDATA}" "${archived_pgdata}"
  fi

  if pg_ctl -D "${HELMR_DEV_PGDATA}" status >/dev/null 2>&1; then
    echo "Owned Postgres is already running under ${HELMR_DEV_PGDATA}" >&2
    echo "Stop it with: pg_ctl -D ${HELMR_DEV_PGDATA} stop" >&2
    return 1
  fi

  if [ ! -d "${HELMR_DEV_PGDATA}" ]; then
    initdb -D "${HELMR_DEV_PGDATA}" -A trust >/dev/null
  fi

  mkdir -p "${HELMR_DEV_PGSOCKET}"
  chmod 700 "${HELMR_DEV_PGSOCKET}"

  local postgres_options="-p ${HELMR_DEV_POSTGRES_PORT} -c listen_addresses= -k ${HELMR_DEV_PGSOCKET}"
  pg_ctl -D "${HELMR_DEV_PGDATA}" -l "${HELMR_DEV_PGLOG}" -o "${postgres_options}" -w start >/dev/null
  HELMR_DEV_OWNED_POSTGRES=1

  export DATABASE_URL="postgres://${USER}@/postgres?host=${HELMR_DEV_PGSOCKET}&port=${HELMR_DEV_POSTGRES_PORT}&sslmode=disable"
}

helmr_dev_write_runtime_descriptor() {
  if [ -n "${DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH:-}" ]; then
    return 0
  fi
  local runtime_descriptor="${HELMR_DEV_DIR}/runtime.descriptor.json"
  printf '%s' '{"architecture":"x86_64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","formatVersion":0,"mediaType":"application/vnd.helmr.runtime.v0+squashfs","runtimeContract":"helmr.runtime.v0","sizeBytes":4096}' >"${runtime_descriptor}"
  export DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH="${runtime_descriptor}"
}

helmr_dev_start_redis() {
  if [ -n "${REDIS_URL:-}" ]; then
    return 0
  fi
  if ! command -v redis-server >/dev/null 2>&1 || ! command -v redis-cli >/dev/null 2>&1; then
    echo "redis-server and redis-cli are required unless REDIS_URL is already set" >&2
    return 1
  fi

  local redis_socket="${HELMR_DEV_RUNTIME_DIR}/redis.sock"
  if [ -S "${redis_socket}" ] && redis-cli -s "${redis_socket}" ping 2>/dev/null | grep -qx PONG; then
    echo "Owned Redis socket is already active: ${redis_socket}" >&2
    return 1
  fi
  rm -f "${redis_socket}"
  redis-server --port 0 --unixsocket "${redis_socket}" --unixsocketperm 700 --save '' --appendonly no \
    >"${HELMR_DEV_DIR}/redis.log" 2>&1 &
  HELMR_DEV_REDIS_PID=$!
  for _ in $(seq 1 50); do
    if redis-cli -s "${redis_socket}" ping 2>/dev/null | grep -qx PONG; then
      export REDIS_URL="unix://${redis_socket}?db=0"
      return 0
    fi
    if ! kill -0 "${HELMR_DEV_REDIS_PID}" >/dev/null 2>&1; then
      cat "${HELMR_DEV_DIR}/redis.log" >&2
      return 1
    fi
    sleep 0.1
  done
  cat "${HELMR_DEV_DIR}/redis.log" >&2
  return 1
}

helmr_dev_start_clickhouse() {
  if [ -n "${CLICKHOUSE_URL:-}" ]; then
    return 0
  fi
  if ! command -v clickhouse >/dev/null 2>&1; then
    echo "clickhouse is required unless CLICKHOUSE_URL is already set" >&2
    return 1
  fi
  if ! command -v curl >/dev/null 2>&1; then
    echo "curl is required for the managed ClickHouse health check" >&2
    return 1
  fi

  local ping_url="http://${HELMR_DEV_CLICKHOUSE_HOST:-127.0.0.1}:${HELMR_DEV_CLICKHOUSE_HTTP_PORT}/ping"
  if curl -fsS --max-time 1 "${ping_url}" >/dev/null 2>&1; then
    echo "ClickHouse HTTP port ${HELMR_DEV_CLICKHOUSE_HTTP_PORT} is already in use" >&2
    return 1
  fi

  export HELMR_DEV_CLICKHOUSE_HOST="127.0.0.1"
  export HELMR_DEV_CLICKHOUSE_PATH="${HELMR_DEV_DIR}/clickhouse/"
  export HELMR_DEV_CLICKHOUSE_TMP_PATH="${HELMR_DEV_DIR}/clickhouse/tmp/"
  export HELMR_DEV_CLICKHOUSE_USER_FILES_PATH="${HELMR_DEV_DIR}/clickhouse/user_files/"
  export HELMR_DEV_CLICKHOUSE_FORMAT_SCHEMA_PATH="${HELMR_DEV_DIR}/clickhouse/format_schemas/"
  export HELMR_DEV_CLICKHOUSE_ACCESS_PATH="${HELMR_DEV_DIR}/clickhouse/access/"
  local clickhouse_bin clickhouse_root
  clickhouse_bin="$(command -v clickhouse)"
  if command -v readlink >/dev/null 2>&1; then
    clickhouse_bin="$(readlink -f "${clickhouse_bin}")"
  fi
  clickhouse_root="$(dirname "$(dirname "${clickhouse_bin}")")"
  export HELMR_DEV_CLICKHOUSE_USERS_CONFIG="${clickhouse_root}/etc/clickhouse-server/users.xml"
  export HELMR_DEV_CLICKHOUSE_HTTP_PORT="${HELMR_DEV_CLICKHOUSE_HTTP_PORT}"

  clickhouse server --config-file="${HELMR_DEV_ROOT}/scripts/dev/clickhouse.xml" \
    >"${HELMR_DEV_DIR}/clickhouse.log" 2>&1 &
  HELMR_DEV_CLICKHOUSE_PID=$!

  for _ in $(seq 1 100); do
    if curl -fsS --max-time 1 "${ping_url}" >/dev/null 2>&1; then
      export CLICKHOUSE_URL="${ping_url%/ping}"
      return 0
    fi
    if ! kill -0 "${HELMR_DEV_CLICKHOUSE_PID}" >/dev/null 2>&1; then
      cat "${HELMR_DEV_DIR}/clickhouse.log" >&2
      return 1
    fi
    sleep 0.1
  done
  cat "${HELMR_DEV_DIR}/clickhouse.log" >&2
  return 1
}

helmr_dev_export_urls() {
  export PUBLIC_URL="${PUBLIC_URL:-"http://${HELMR_DEV_CONSOLE_HOST}:${HELMR_DEV_CONSOLE_PORT}"}"
  export HELMR_DEV_CONSOLE_PORT="${HELMR_DEV_CONSOLE_PORT}"
  export HELMR_DEV_CAS_DIR="${HELMR_DEV_CAS_DIR}"
  export BOOTSTRAP_ENABLED="${BOOTSTRAP_ENABLED:-1}"
  export BOOTSTRAP_REGION_ID="${BOOTSTRAP_REGION_ID:-local}"
  export BOOTSTRAP_REGION_DISPLAY_NAME="${BOOTSTRAP_REGION_DISPLAY_NAME:-Local}"
  export BOOTSTRAP_WORKER_GROUP_NAME="${BOOTSTRAP_WORKER_GROUP_NAME:-default}"
  export BOOTSTRAP_WORKER_TOKEN="${BOOTSTRAP_WORKER_TOKEN:-hlmr_wgt_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8}"
  export WORKER_RESOURCE_ID="${WORKER_RESOURCE_ID:-local-worker}"

  if [ "${HELMR_DEV_CONSOLE_MODE}" = "preview" ]; then
    export CONTROL_PLANE_ADDR="${CONTROL_PLANE_ADDR:-${HELMR_DEV_CONSOLE_HOST}:${HELMR_DEV_CONSOLE_PORT}}"
    export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-${PUBLIC_URL}}"
  else
    export CONTROL_PLANE_ADDR="${CONTROL_PLANE_ADDR:-${HELMR_DEV_CONSOLE_HOST}:${HELMR_DEV_CONTROL_PLANE_PORT}}"
    export HELMR_DEV_BACKEND_URL="${HELMR_DEV_BACKEND_URL:-http://${HELMR_DEV_CONSOLE_HOST}:${HELMR_DEV_CONTROL_PLANE_PORT}}"
  fi
}

helmr_dev_build_controlplane() {
  local bin_dir="${HELMR_DEV_DIR}/bin"
  mkdir -p "${bin_dir}"
  local tags=()
  if [ "${HELMR_DEV_CONSOLE_MODE}" = "preview" ]; then
    (cd "${HELMR_DEV_ROOT}" && make console-build)
    tags=(-tags embed_console)
  fi
  if ! go build "${tags[@]}" -o "${bin_dir}/dev-controlplane" "${HELMR_DEV_ROOT}/cmd/internal/dev-controlplane"; then
    return 1
  fi
  HELMR_DEV_CONTROLPLANE_BIN="${bin_dir}/dev-controlplane"
}

helmr_dev_wait_ready() {
  local url="$1"
  local log_file="$2"
  for _ in $(seq 1 120); do
    if curl -fsS --max-time 1 "${url}" >/dev/null 2>&1; then
      return 0
    fi
    if [ -n "${HELMR_DEV_CONTROLPLANE_PID}" ] && ! kill -0 "${HELMR_DEV_CONTROLPLANE_PID}" >/dev/null 2>&1; then
      echo "dev control plane exited before becoming ready" >&2
      if [ -f "${log_file}" ]; then
        cat "${log_file}" >&2
      fi
      return 1
    fi
    sleep 0.5
  done
  echo "timed out waiting for ${url}" >&2
  if [ -f "${log_file}" ]; then
    cat "${log_file}" >&2
  fi
  return 1
}

helmr_dev_owned_services_running() {
  local lock_pid redis_socket ping_url
  if [ "${HELMR_DEV_LOCK_OWNED:-0}" != "1" ]; then
    if [ -d "${HELMR_DEV_LOCK_DIR}" ]; then
      lock_pid="$(helmr_dev_lock_holder_pid 2>/dev/null || true)"
      if [ -n "${lock_pid}" ] && kill -0 "${lock_pid}" >/dev/null 2>&1; then
        echo "Cannot reset while dev stack is running (pid ${lock_pid}). Stop it first." >&2
      else
        echo "Cannot reset while dev stack lock exists at ${HELMR_DEV_LOCK_DIR}." >&2
        echo "Inspect owned services, stop any that remain, then remove the lock directory manually." >&2
      fi
      return 0
    fi
  fi
  if [ -d "${HELMR_DEV_PGDATA}" ] && command -v pg_ctl >/dev/null 2>&1 \
    && pg_ctl -D "${HELMR_DEV_PGDATA}" status >/dev/null 2>&1; then
    echo "Cannot reset while owned Postgres is running under ${HELMR_DEV_PGDATA}." >&2
    echo "Stop it with: pg_ctl -D ${HELMR_DEV_PGDATA} stop" >&2
    return 0
  fi
  redis_socket="${HELMR_DEV_RUNTIME_DIR}/redis.sock"
  if [ -S "${redis_socket}" ] && command -v redis-cli >/dev/null 2>&1 \
    && redis-cli -s "${redis_socket}" ping 2>/dev/null | grep -qx PONG; then
    echo "Cannot reset while owned Redis is running (socket ${redis_socket}). Stop the dev stack first." >&2
    return 0
  fi
  if [ -d "${HELMR_DEV_DIR}/clickhouse" ] && helmr_dev_port_listening "${HELMR_DEV_CLICKHOUSE_HTTP_PORT}"; then
    echo "Cannot reset while a process is listening on owned ClickHouse HTTP port ${HELMR_DEV_CLICKHOUSE_HTTP_PORT}." >&2
    echo "Stop the dev stack or the conflicting listener first." >&2
    return 0
  fi
  return 1
}

helmr_dev_cleanup() {
  if [ -n "${HELMR_DEV_CONSOLE_PID}" ]; then
    kill "${HELMR_DEV_CONSOLE_PID}" >/dev/null 2>&1 || true
    wait "${HELMR_DEV_CONSOLE_PID}" >/dev/null 2>&1 || true
  fi
  if [ -n "${HELMR_DEV_CONTROLPLANE_PID}" ]; then
    kill "${HELMR_DEV_CONTROLPLANE_PID}" >/dev/null 2>&1 || true
    wait "${HELMR_DEV_CONTROLPLANE_PID}" >/dev/null 2>&1 || true
  fi
  if [ -n "${HELMR_DEV_REDIS_PID}" ]; then
    kill "${HELMR_DEV_REDIS_PID}" >/dev/null 2>&1 || true
    wait "${HELMR_DEV_REDIS_PID}" >/dev/null 2>&1 || true
  fi
  if [ -n "${HELMR_DEV_CLICKHOUSE_PID}" ]; then
    kill "${HELMR_DEV_CLICKHOUSE_PID}" >/dev/null 2>&1 || true
    wait "${HELMR_DEV_CLICKHOUSE_PID}" >/dev/null 2>&1 || true
  fi
  if [ "${HELMR_DEV_OWNED_POSTGRES}" = "1" ]; then
    if command -v pg_ctl >/dev/null 2>&1 \
      && [ -d "${HELMR_DEV_PGDATA}" ] \
      && pg_ctl -D "${HELMR_DEV_PGDATA}" status >/dev/null 2>&1; then
      pg_ctl -D "${HELMR_DEV_PGDATA}" -m fast -w stop >/dev/null 2>&1 || true
    fi
  fi
  helmr_dev_release_lock
}

helmr_dev_reset_owned_storage() {
  if ! helmr_dev_acquire_lock; then
    return 1
  fi
  trap 'helmr_dev_release_lock' RETURN
  if helmr_dev_owned_services_running; then
    return 1
  fi
  rm -rf "${HELMR_DEV_DIR}/postgres" "${HELMR_DEV_DIR}/clickhouse" "${HELMR_DEV_CAS_DIR}"
  helmr_dev_release_lock
  trap - RETURN
}
