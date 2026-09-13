#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/dev/instance.sh
source "${ROOT}/scripts/dev/instance.sh"

helmr_dev_init "${ROOT}"

required_commands=(go bun curl)
if [ "${HELMR_DEV_CONSOLE_MODE}" = "preview" ]; then
  required_commands+=(make)
fi
for name in "${required_commands[@]}"; do
  if ! command -v "${name}" >/dev/null 2>&1; then
    echo "${name} is required for the dev console stack" >&2
    exit 1
  fi
done

helmr_dev_acquire_lock

HELMR_DEV_CLEANED_UP=0
helmr_dev_cleanup_and_exit() {
  if [ "${HELMR_DEV_CLEANED_UP}" = "1" ]; then
    return
  fi
  HELMR_DEV_CLEANED_UP=1
  helmr_dev_cleanup
}
trap 'helmr_dev_cleanup_and_exit; exit 143' INT TERM
trap helmr_dev_cleanup_and_exit EXIT

helmr_dev_require_ports

helmr_dev_start_postgres
helmr_dev_write_runtime_descriptor
helmr_dev_start_redis
helmr_dev_start_clickhouse
helmr_dev_export_urls

if ! helmr_dev_build_controlplane; then
  echo "failed to build dev control plane" >&2
  exit 1
fi

controlplane_log="${HELMR_DEV_DIR}/controlplane.log"
"${HELMR_DEV_CONTROLPLANE_BIN}" >"${controlplane_log}" 2>&1 &
HELMR_DEV_CONTROLPLANE_PID=$!

ready_url="http://${HELMR_DEV_CONSOLE_HOST}"
if [ "${HELMR_DEV_CONSOLE_MODE}" = "preview" ]; then
  ready_url="${ready_url}:${HELMR_DEV_CONSOLE_PORT}"
else
  ready_url="${ready_url}:${HELMR_DEV_CONTROL_PLANE_PORT}"
fi
ready_url="${ready_url}/readyz"

if ! helmr_dev_wait_ready "${ready_url}" "${controlplane_log}"; then
  exit 1
fi

if [ "${HELMR_DEV_CONSOLE_MODE}" = "live" ]; then
  (
    cd "${ROOT}/packages/console"
    exec bun run dev
  ) >"${HELMR_DEV_DIR}/console.log" 2>&1 &
  HELMR_DEV_CONSOLE_PID=$!
fi

cat <<EOF

Helmr dev console stack is running.

  State:    ${HELMR_DEV_DIR}
  Sockets:  ${HELMR_DEV_RUNTIME_DIR}
  Console:  ${PUBLIC_URL}
  Backend:  ${HELMR_DEV_BACKEND_URL}
  Postgres: $([ "${HELMR_DEV_OWNED_POSTGRES}" = "1" ] && echo "${HELMR_DEV_PGDATA}" || echo "external")
  ClickHouse: $([ -n "${HELMR_DEV_CLICKHOUSE_PID}" ] && echo "${HELMR_DEV_DIR}/clickhouse" || echo "external")
  Login:    ${PUBLIC_URL}/dev/login

Ports (override with HELMR_DEV_* env vars):
  console=${HELMR_DEV_CONSOLE_PORT} control_plane=${HELMR_DEV_CONTROL_PLANE_PORT} postgres=${HELMR_DEV_POSTGRES_PORT} clickhouse_http=${HELMR_DEV_CLICKHOUSE_HTTP_PORT}

Press Ctrl-C to stop the stack.
Reset owned storage: make dev-reset

EOF

exit_code=0
while kill -0 "${HELMR_DEV_CONTROLPLANE_PID}" >/dev/null 2>&1; do
  if [ "${HELMR_DEV_CONSOLE_MODE}" = "live" ] && [ -n "${HELMR_DEV_CONSOLE_PID}" ]; then
    if ! kill -0 "${HELMR_DEV_CONSOLE_PID}" >/dev/null 2>&1; then
      echo "console dev server exited unexpectedly" >&2
      if [ -f "${HELMR_DEV_DIR}/console.log" ]; then
        tail -50 "${HELMR_DEV_DIR}/console.log" >&2
      fi
      exit_code=1
      break
    fi
  fi
  sleep 1
done

if [ "${exit_code}" -eq 0 ] && ! kill -0 "${HELMR_DEV_CONTROLPLANE_PID}" >/dev/null 2>&1; then
  wait "${HELMR_DEV_CONTROLPLANE_PID}" >/dev/null 2>&1 || exit_code=$?
  if [ "${exit_code}" -ne 0 ] && [ -f "${controlplane_log}" ]; then
    tail -50 "${controlplane_log}" >&2
  fi
fi

exit "${exit_code}"
