#!/usr/bin/env bash
set -euo pipefail

: "${FAKE_HELMR_LOG:?FAKE_HELMR_LOG is required}"
printf '%s\n' "$*" >>"${FAKE_HELMR_LOG}"

network_run=run_aaaaaaaaaaaaaaaaaaaaaaaaaa
invalid_run=run_bbbbbbbbbbbbbbbbbbbbbbbbbb
error_run=run_cccccccccccccccccccccccccc
concurrent_run=run_dddddddddddddddddddddddddd

case "${1:-} ${2:-}" in
  "workspace create")
    case "${3:-}" in
      helmr-network-smoke)
        printf '{"workspace_id":"wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"}\n'
        ;;
      helmr-runtime-smoke)
        printf '{"workspace_id":"wsp_bbbbbbbbbbbbbbbbbbbbbbbbbb"}\n'
        ;;
      helmr-edge-smoke)
        printf '{"workspace_id":"wsp_cccccccccccccccccccccccccc"}\n'
        ;;
      helmr-secret-smoke)
        printf '{"workspace_id":"wsp_dddddddddddddddddddddddddd"}\n'
        ;;
      *)
        printf 'unexpected Workspace declaration: %s\n' "${3:-}" >&2
        exit 2
        ;;
    esac
    ;;
  "workspace delete")
    printf '{"workspace_id":"%s"}\n' "${4:-unknown}"
    ;;
  "task start")
    case "${3:-} $*" in
      "network-smoke "*)
        printf '{"run_id":"%s"}\n' "${network_run}"
        ;;
      "runtime-smoke "*'"unknown":true'*)
        printf '{"run_id":"%s"}\n' "${invalid_run}"
        ;;
      "edge-smoke "*'"mode":"expected-error"'*)
        printf '{"run_id":"%s"}\n' "${error_run}"
        ;;
      "edge-smoke "*'"mode":"concurrent-wait"'*)
        printf '{"run_id":"%s"}\n' "${concurrent_run}"
        ;;
      "missing-secret-smoke "*)
        printf 'required Secret is unavailable\n' >&2
        exit 1
        ;;
      *)
        printf 'unexpected Run start: %s\n' "$*" >&2
        exit 2
        ;;
    esac
    ;;
  "run wait")
    case "${3:-}" in
      "${invalid_run}"|"${error_run}")
        printf '{"status":"failed"}\n'
        ;;
      "${network_run}"|"${concurrent_run}")
        printf '{"status":"succeeded"}\n'
        ;;
      *)
        printf 'unexpected Run wait: %s\n' "${3:-}" >&2
        exit 2
        ;;
    esac
    ;;
  "run get")
    case "${3:-}" in
      "${network_run}")
        printf '%s\n' \
          '{"status":"succeeded","output":{"publicIPv4":true,"ipv6DefaultRoute":false}}'
        ;;
      "${invalid_run}")
        printf '{"status":"failed","error":{"code":"%s"}}\n' \
          "${FAKE_CONTRACT_ERROR_CODE:-task_payload_invalid}"
        ;;
      "${error_run}")
        printf '{"status":"failed","error":{"code":"%s"}}\n' \
          "${FAKE_CONTRACT_ERROR_CODE:-task_failed}"
        ;;
      "${concurrent_run}")
        printf '{"status":"succeeded","output":{"mode":"concurrent-wait"}}\n'
        ;;
      *)
        printf 'unexpected Run get: %s\n' "${3:-}" >&2
        exit 2
        ;;
    esac
    ;;
  "run events"|"run logs")
    printf '{"items":[]}\n'
    ;;
  *)
    printf 'unexpected fake Helmr command: %s\n' "$*" >&2
    exit 2
    ;;
esac
