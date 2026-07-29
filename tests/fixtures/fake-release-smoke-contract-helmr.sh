#!/usr/bin/env bash
set -euo pipefail

: "${FAKE_HELMR_LOG:?FAKE_HELMR_LOG is required}"
printf '%s\n' "$*" >>"${FAKE_HELMR_LOG}"

network_run=019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31
invalid_run=019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38
error_run=019c10d5-a6f7-7af1-8f5f-bb97bcc0dc3c
concurrent_run=019c10d5-a6f7-7af1-8f5f-bb97bcc0dc3d

case "${1:-} ${2:-}" in
  "workspace create")
    case "${3:-}" in
      helmr-network-smoke)
        printf '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}\n'
        ;;
      helmr-runtime-smoke)
        printf '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc3f"}\n'
        ;;
      helmr-edge-smoke)
        printf '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc40"}\n'
        ;;
      helmr-secret-smoke)
        printf '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc41"}\n'
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
