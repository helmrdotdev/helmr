#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/dev/instance.sh
source "${ROOT}/scripts/dev/instance.sh"

fail() {
  echo "dev-console-stack.test.sh: $*" >&2
  exit 1
}

require_cmd() {
  for name in "$@"; do
    command -v "${name}" >/dev/null 2>&1 || fail "${name} is required"
  done
}

require_cmd go bun curl postgres psql redis-server redis-cli clickhouse pg_ctl initdb python3

configure_stack() {
  local dir="$1"
  local port="$2"
  local mode="${3:-preview}"
  export HELMR_DEV_DIR="${dir}"
  export HELMR_DEV_CONSOLE_MODE="${mode}"
  export HELMR_DEV_CONSOLE_PORT="${port}"
  if [ "${mode}" = "live" ]; then
    export HELMR_DEV_CONTROL_PLANE_PORT="$((port + 1))"
  else
    export HELMR_DEV_CONTROL_PLANE_PORT="${port}"
  fi
  export HELMR_DEV_POSTGRES_PORT="$((port + 2))"
  export HELMR_DEV_CLICKHOUSE_HTTP_PORT="$((port + 3))"
  export PUBLIC_URL="http://127.0.0.1:${port}"
  unset DATABASE_URL REDIS_URL CLICKHOUSE_URL DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH
  unset CONTROL_PLANE_ADDR HELMR_DEV_BACKEND_URL
  helmr_dev_init "${ROOT}"
}

wait_ready() {
  local port="$1"
  for _ in $(seq 1 120); do
    if curl -fsS --max-time 1 "http://127.0.0.1:${port}/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  if [ -n "${2:-}" ] && [ -f "${2}" ]; then
    tail -50 "${2}" >&2
  fi
  fail "timed out waiting for readyz on port ${port}"
}

start_wrapper_process() {
  local log="$1"
  (
    export HELMR_DEV_DIR HELMR_DEV_CONSOLE_MODE HELMR_DEV_CONSOLE_PORT HELMR_DEV_CONTROL_PLANE_PORT
    export HELMR_DEV_POSTGRES_PORT HELMR_DEV_CLICKHOUSE_HTTP_PORT PUBLIC_URL
    exec "${ROOT}/scripts/dev-console-stack.sh"
  ) >"${log}" 2>&1 &
  echo $!
}

launch_wrapper() {
  local dir="$1"
  local port="$2"
  local mode="${3:-preview}"
  local log="$4"
  configure_stack "${dir}" "${port}" "${mode}"
  start_wrapper_process "${log}"
}

launch_fresh_wrapper() {
  local dir="$1"
  local port="$2"
  local mode="${3:-preview}"
  local log="$4"
  configure_stack "${dir}" "${port}" "${mode}"
  helmr_dev_reset_owned_storage
  start_wrapper_process "${log}"
}

stop_wrapper() {
  local pid="$1"
  kill -TERM "${pid}" >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    if ! kill -0 "${pid}" >/dev/null 2>&1; then
      wait "${pid}" >/dev/null 2>&1 || true
      return 0
    fi
    sleep 0.5
  done
  fail "wrapper pid ${pid} did not stop"
}

wait_owned_services_stopped() {
  local dir="$1"
  local port="$2"
  local mode="${3:-preview}"
  configure_stack "${dir}" "${port}" "${mode}"
  for _ in $(seq 1 60); do
    if ! helmr_dev_owned_services_running >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  fail "owned services still running under ${dir}"
}

owned_database_url() {
  configure_stack "$1" "$2" "${3:-preview}"
  printf 'postgres://%s@/postgres?host=%s&port=%s&sslmode=disable' \
    "${USER}" "${HELMR_DEV_PGSOCKET}" "${HELMR_DEV_POSTGRES_PORT}"
}

start_foreign_listener() {
  local port="$1"
  local log="$2"
  local pid waited

  if ! command -v python3 >/dev/null 2>&1; then
    echo "python3 is required for foreign listener probe" >&2
    return 1
  fi

  python3 -m http.server "${port}" --bind 127.0.0.1 >>"${log}" 2>&1 &
  pid=$!

  for waited in $(seq 1 50); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      echo "foreign listener exited before port ${port} was ready" >&2
      tail -30 "${log}" >&2
      return 1
    fi
    if helmr_dev_port_listening "${port}"; then
      printf '%s' "${pid}"
      return 0
    fi
    sleep 0.2
  done

  kill -TERM "${pid}" >/dev/null 2>&1 || true
  wait "${pid}" >/dev/null 2>&1 || true
  echo "foreign listener on port ${port} never became ready" >&2
  tail -30 "${log}" >&2
  return 1
}

stop_foreign_listener() {
  local pid="$1"
  kill -TERM "${pid}" >/dev/null 2>&1 || true
  wait "${pid}" >/dev/null 2>&1 || true
}

foreign_listener_alive() {
  kill -0 "$1" >/dev/null 2>&1
}

run_wrapper_expect_failure() {
  local log="$1"
  local timeout_secs="${2:-30}"
  local wrapper_pid exit_code waited max_wait

  (
    export HELMR_DEV_DIR HELMR_DEV_CONSOLE_MODE HELMR_DEV_CONSOLE_PORT HELMR_DEV_CONTROL_PLANE_PORT
    export HELMR_DEV_POSTGRES_PORT HELMR_DEV_CLICKHOUSE_HTTP_PORT PUBLIC_URL
    exec "${ROOT}/scripts/dev-console-stack.sh"
  ) >>"${log}" 2>&1 &
  wrapper_pid=$!

  max_wait=$((timeout_secs * 2))
  waited=0
  while [ "${waited}" -lt "${max_wait}" ]; do
    if ! kill -0 "${wrapper_pid}" 2>/dev/null; then
      exit_code=0
      wait "${wrapper_pid}" 2>/dev/null || exit_code=$?
      if [ "${exit_code}" -eq 0 ]; then
        tail -30 "${log}" >&2
        fail "expected wrapper to fail but it exited 0"
      fi
      return 0
    fi
    sleep 0.5
    waited=$((waited + 1))
  done

  kill -TERM "${wrapper_pid}" >/dev/null 2>&1 || true
  waited=0
  while kill -0 "${wrapper_pid}" 2>/dev/null && [ "${waited}" -lt 20 ]; do
    sleep 0.5
    waited=$((waited + 1))
  done
  if kill -0 "${wrapper_pid}" 2>/dev/null; then
    kill -KILL "${wrapper_pid}" >/dev/null 2>&1 || true
    wait "${wrapper_pid}" >/dev/null 2>&1 || true
    tail -30 "${log}" >&2
    fail "wrapper pid ${wrapper_pid} still running after ${timeout_secs}s timeout"
  fi
  wait "${wrapper_pid}" >/dev/null 2>&1 || true
  tail -30 "${log}" >&2
  fail "wrapper pid ${wrapper_pid} did not exit within ${timeout_secs}s"
}

test_failed_second_start_preserves_lock() {
  local dir pid lock_pid
  dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-lock.XXXXXX")"
  pid="$(launch_fresh_wrapper "${dir}" 36200 preview "${dir}/wrapper.log")"
  wait_ready 36200 "${dir}/wrapper.log"
  lock_pid="$(cat "${dir}/.stack.lock/pid")"
  export HELMR_DEV_DIR="${dir}"
  export HELMR_DEV_CONSOLE_MODE=preview
  export HELMR_DEV_CONSOLE_PORT=36210
  export HELMR_DEV_CONTROL_PLANE_PORT=36210
  export HELMR_DEV_POSTGRES_PORT=36211
  export HELMR_DEV_CLICKHOUSE_HTTP_PORT=36212
  export PUBLIC_URL="http://127.0.0.1:36210"
  run_wrapper_expect_failure "${dir}/second.log" 15
  if [ ! -f "${dir}/.stack.lock/pid" ]; then
    fail "first stack lock was removed after failed second start"
  fi
  if [ "$(cat "${dir}/.stack.lock/pid")" != "${lock_pid}" ]; then
    fail "first stack lock pid changed after failed second start"
  fi
  curl -fsS "http://127.0.0.1:36200/readyz" >/dev/null
  if ! HELMR_DEV_DIR="${dir}" "${ROOT}/scripts/dev-reset.sh" >/dev/null 2>&1; then
    :
  else
    fail "expected reset to refuse while first stack is running"
  fi
  stop_wrapper "${pid}"
  wait_owned_services_stopped "${dir}" 36200 preview
  rm -rf "${dir}"
}

test_concurrent_stacks_isolated() {
  local dir_a dir_b pid_a pid_b runtime_a runtime_b
  dir_a="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-a.XXXXXX")"
  dir_b="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-b.XXXXXX")"
  pid_a="$(launch_fresh_wrapper "${dir_a}" 36220 preview "${dir_a}/wrapper.log")"
  pid_b="$(launch_fresh_wrapper "${dir_b}" 36230 preview "${dir_b}/wrapper.log")"
  wait_ready 36220
  wait_ready 36230
  configure_stack "${dir_a}" 36220 preview
  runtime_a="${HELMR_DEV_RUNTIME_DIR}"
  configure_stack "${dir_b}" 36230 preview
  runtime_b="${HELMR_DEV_RUNTIME_DIR}"
  if [ "${runtime_a}" = "${runtime_b}" ]; then
    fail "concurrent stacks share runtime directory ${runtime_a}"
  fi
  if [ ! -S "${runtime_a}/redis.sock" ] || [ ! -S "${runtime_b}/redis.sock" ]; then
    fail "expected isolated redis sockets for concurrent stacks"
  fi
  curl -fsS "http://127.0.0.1:36220/readyz" >/dev/null
  curl -fsS "http://127.0.0.1:36230/readyz" >/dev/null
  stop_wrapper "${pid_b}"
  wait_owned_services_stopped "${dir_b}" 36230 preview
  curl -fsS "http://127.0.0.1:36220/readyz" >/dev/null
  stop_wrapper "${pid_a}"
  wait_owned_services_stopped "${dir_a}" 36220 preview
  rm -rf "${dir_a}" "${dir_b}"
}

test_persistence_and_reset() {
  local dir pid db_url production_name token_state schedule_count
  dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-persist.XXXXXX")"
  pid="$(launch_fresh_wrapper "${dir}" 36240 preview "${dir}/wrapper.log")"
  wait_ready 36240
  db_url="$(owned_database_url "${dir}" 36240 preview)"
  psql "${db_url}" -v ON_ERROR_STOP=1 -c \
    "UPDATE environments SET name = 'Renamed Production' WHERE id = '00000000-0000-7000-8000-000000000401'" >/dev/null
  psql "${db_url}" -v ON_ERROR_STOP=1 -c \
    "UPDATE tokens SET state = 'completed', result = '{\"approved\":true}'::jsonb, completed_at = now() WHERE id = '00000000-0000-7000-8000-000000000801'" >/dev/null
  psql "${db_url}" -v ON_ERROR_STOP=1 -c \
    "DELETE FROM schedules WHERE id = '00000000-0000-7000-8000-000000000521'" >/dev/null
  stop_wrapper "${pid}"
  pid="$(launch_wrapper "${dir}" 36240 preview "${dir}/restart.log")"
  wait_ready 36240
  db_url="$(owned_database_url "${dir}" 36240 preview)"
  production_name="$(psql "${db_url}" -At -c \
    "SELECT name FROM environments WHERE id = '00000000-0000-7000-8000-000000000401'")"
  token_state="$(psql "${db_url}" -At -c \
    "SELECT state FROM tokens WHERE id = '00000000-0000-7000-8000-000000000801'")"
  schedule_count="$(psql "${db_url}" -At -c \
    "SELECT count(*) FROM schedules WHERE id = '00000000-0000-7000-8000-000000000521'")"
  if [ "${production_name}" != "Renamed Production" ] || [ "${token_state}" != "completed" ] || [ "${schedule_count}" != "0" ]; then
    fail "restart without reset did not preserve edits (${production_name}/${token_state}/${schedule_count})"
  fi
  stop_wrapper "${pid}"
  wait_owned_services_stopped "${dir}" 36240 preview
  helmr_dev_reset_owned_storage
  pid="$(launch_fresh_wrapper "${dir}" 36240 preview "${dir}/reset.log")"
  wait_ready 36240
  db_url="$(owned_database_url "${dir}" 36240 preview)"
  production_name="$(psql "${db_url}" -At -c \
    "SELECT name FROM environments WHERE id = '00000000-0000-7000-8000-000000000401'")"
  token_state="$(psql "${db_url}" -At -c \
    "SELECT state FROM tokens WHERE id = '00000000-0000-7000-8000-000000000801'")"
  schedule_count="$(psql "${db_url}" -At -c \
    "SELECT count(*) FROM schedules WHERE id = '00000000-0000-7000-8000-000000000521'")"
  if [ "${production_name}" != "Production" ] || [ "${token_state}" != "pending" ] || [ "${schedule_count}" != "1" ]; then
    fail "reset did not restore fixtures (${production_name}/${token_state}/${schedule_count})"
  fi
  stop_wrapper "${pid}"
  wait_owned_services_stopped "${dir}" 36240 preview
  rm -rf "${dir}"
}

test_foreign_clickhouse_port_untouched() {
  local dir base_port ch_port foreign_pid foreign_log wrapper_log
  dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-foreign.XXXXXX")"
  foreign_log="${dir}/foreign.log"
  wrapper_log="${dir}/wrapper.log"
  foreign_pid=""

  cleanup_foreign_test() {
    if [ -n "${foreign_pid}" ] && foreign_listener_alive "${foreign_pid}"; then
      stop_foreign_listener "${foreign_pid}"
    fi
    rm -rf "${dir}"
  }
  trap cleanup_foreign_test RETURN

  base_port=36310
  configure_stack "${dir}" "${base_port}" preview
  ch_port="${HELMR_DEV_CLICKHOUSE_HTTP_PORT}"

  if ! foreign_pid="$(start_foreign_listener "${ch_port}" "${foreign_log}")"; then
    fail "could not start foreign listener on port ${ch_port}"
  fi
  if ! foreign_listener_alive "${foreign_pid}" || ! helmr_dev_port_listening "${ch_port}"; then
    tail -30 "${foreign_log}" >&2
    fail "foreign listener not ready on port ${ch_port} before startup attempt"
  fi

  helmr_dev_reset_owned_storage
  run_wrapper_expect_failure "${wrapper_log}" 30

  if ! foreign_listener_alive "${foreign_pid}"; then
    tail -30 "${foreign_log}" >&2
    fail "foreign listener was killed after failed startup"
  fi

  mkdir -p "${dir}/clickhouse"
  if HELMR_DEV_DIR="${dir}" HELMR_DEV_CONSOLE_MODE="${HELMR_DEV_CONSOLE_MODE}" \
    HELMR_DEV_CONSOLE_PORT="${HELMR_DEV_CONSOLE_PORT}" \
    HELMR_DEV_CONTROL_PLANE_PORT="${HELMR_DEV_CONTROL_PLANE_PORT}" \
    HELMR_DEV_POSTGRES_PORT="${HELMR_DEV_POSTGRES_PORT}" \
    HELMR_DEV_CLICKHOUSE_HTTP_PORT="${HELMR_DEV_CLICKHOUSE_HTTP_PORT}" \
    "${ROOT}/scripts/dev-reset.sh" >/dev/null 2>&1; then
    fail "expected reset to refuse while ClickHouse port ${ch_port} is in use"
  fi
  if ! foreign_listener_alive "${foreign_pid}"; then
    tail -30 "${foreign_log}" >&2
    fail "foreign listener was killed after reset refusal"
  fi

  trap - RETURN
  cleanup_foreign_test
}

test_lock_missing_pid_refuses() {
  local dir
  dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-lock-missing.XXXXXX")"
  configure_stack "${dir}" 36320 preview
  mkdir -p "${HELMR_DEV_DIR}/.stack.lock"
  if helmr_dev_acquire_lock >/dev/null 2>&1; then
    fail "expected acquire to refuse lock with missing pid file"
  fi
  if [ ! -d "${HELMR_DEV_DIR}/.stack.lock" ]; then
    fail "lock directory was removed when pid file was missing"
  fi
  rm -rf "${dir}"
}

test_reset_holds_lock_exclusion() {
  local dir
  dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-reset-lock.XXXXXX")"
  configure_stack "${dir}" 36330 preview
  if ! helmr_dev_acquire_lock; then
    fail "expected reset-style lock acquire to succeed on empty state dir"
  fi
  export HELMR_DEV_DIR="${dir}"
  export HELMR_DEV_CONSOLE_MODE=preview
  export HELMR_DEV_CONSOLE_PORT=36330
  export HELMR_DEV_CONTROL_PLANE_PORT=36330
  export HELMR_DEV_POSTGRES_PORT=36332
  export HELMR_DEV_CLICKHOUSE_HTTP_PORT=36333
  export PUBLIC_URL="http://127.0.0.1:36330"
  run_wrapper_expect_failure "${dir}/blocked.log" 15
  helmr_dev_release_lock
  rm -rf "${dir}"
}

test_reset_refuses_owned_pg_with_database_url() {
  local dir pid
  dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-pg-refuse.XXXXXX")"
  pid="$(launch_fresh_wrapper "${dir}" 36340 preview "${dir}/wrapper.log")"
  wait_ready 36340 "${dir}/wrapper.log"
  configure_stack "${dir}" 36340 preview
  stop_wrapper "${pid}"
  wait_owned_services_stopped "${dir}" 36340 preview
  mkdir -p "${HELMR_DEV_PGSOCKET}"
  chmod 700 "${HELMR_DEV_PGSOCKET}"
  pg_ctl -D "${HELMR_DEV_PGDATA}" -l "${HELMR_DEV_PGLOG}" \
    -o "-p ${HELMR_DEV_POSTGRES_PORT} -c listen_addresses= -k ${HELMR_DEV_PGSOCKET}" -w start >/dev/null
  if DATABASE_URL="postgres://example@127.0.0.1:9/example" HELMR_DEV_DIR="${dir}" \
    "${ROOT}/scripts/dev-reset.sh" >/dev/null 2>&1; then
    pg_ctl -D "${HELMR_DEV_PGDATA}" -m fast -w stop >/dev/null 2>&1 || true
    fail "expected reset to refuse while owned Postgres is running even with DATABASE_URL set"
  fi
  pg_ctl -D "${HELMR_DEV_PGDATA}" -m fast -w stop >/dev/null
  rm -rf "${dir}"
}

test_live_sigterm_cleanup() {
  local dir pid console_port backend_port
  dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-stack-live.XXXXXX")"
  console_port=36250
  backend_port=36251
  pid="$(launch_fresh_wrapper "${dir}" "${console_port}" live "${dir}/wrapper.log")"
  wait_ready "${backend_port}"
  for _ in $(seq 1 60); do
    if helmr_dev_port_listening "${console_port}"; then
      break
    fi
    sleep 0.5
  done
  if ! helmr_dev_port_listening "${console_port}"; then
    if [ -f "${dir}/wrapper.log" ]; then
      tail -50 "${dir}/wrapper.log" >&2
    fi
    if [ -f "${dir}/console.log" ]; then
      tail -50 "${dir}/console.log" >&2
    fi
    fail "expected Vite console on port ${console_port}"
  fi
  if ! helmr_dev_port_listening "${backend_port}"; then
    fail "expected control plane on port ${backend_port}"
  fi
  stop_wrapper "${pid}"
  if helmr_dev_port_listening "${console_port}" || helmr_dev_port_listening "${backend_port}"; then
    fail "live stack ports still listening after SIGTERM"
  fi
  wait_owned_services_stopped "${dir}" "${console_port}" live
  rm -rf "${dir}"
}

test_foreign_clickhouse_port_untouched
test_lock_missing_pid_refuses
test_reset_holds_lock_exclusion
test_reset_refuses_owned_pg_with_database_url
test_failed_second_start_preserves_lock
test_concurrent_stacks_isolated
test_persistence_and_reset
test_live_sigterm_cleanup

echo "dev-console-stack.test.sh: ok"
