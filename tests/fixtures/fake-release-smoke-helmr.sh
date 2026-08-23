#!/usr/bin/env bash
set -euo pipefail

: "${FAKE_HELMR_LOG:?FAKE_HELMR_LOG is required}"
printf '%s\n' "$*" >>"${FAKE_HELMR_LOG}"

case "${1:-} ${2:-}" in
  "workspace create")
    case "${3:-}" in
      helmr-child-task-target-smoke)
        printf '{"id":"workspace-target"}\n'
        ;;
      helmr-child-task-caller-smoke)
        printf '{"id":"workspace-caller"}\n'
        ;;
      *)
        printf 'unexpected Workspace declaration: %s\n' "${3:-}" >&2
        exit 2
        ;;
    esac
    ;;
  "workspace delete")
    if [ "${FAKE_HELMR_FAIL_MODE:-}" = "delete-already-gone" ] &&
      [ "${4:-}" = "workspace-caller" ]; then
      printf '404 Not Found: workspace was not found\n' >&2
      exit 1
    fi
    if [ "${FAKE_HELMR_FAIL_MODE:-}" = "delete-target" ] &&
      [ "${4:-}" = "workspace-target" ]; then
      printf 'intentional target Workspace delete failure\n' >&2
      exit 1
    fi
    printf '{"id":"%s"}\n' "${4:-unknown}"
    ;;
  "task start")
    case "$*" in
      *'"mode":"call-success"'*)
        printf '{"run_id":"run-call-success"}\n'
        ;;
      *'"mode":"same-sandbox-call"'*)
        printf '{"run_id":"run-same-sandbox-call"}\n'
        ;;
      *'"mode":"call-failure"'*)
        printf '{"run_id":"run-call-failure"}\n'
        ;;
      *'"mode":"start-detached"'*)
        printf '{"run_id":"run-start-detached"}\n'
        ;;
      *)
        printf 'unexpected child Task payload: %s\n' "$*" >&2
        exit 2
        ;;
    esac
    ;;
  "run wait")
    if [ "${FAKE_HELMR_FAIL_MODE:-}" = "call-failure" ] &&
      [ "${3:-}" = "run-call-failure" ]; then
      printf '{"status":"failed"}\n'
    else
      printf '{"status":"succeeded"}\n'
    fi
    ;;
  "run get")
    if [ "${3:-}" = "run-start-detached" ]; then
      printf '{"status":"succeeded","output":{"childRunId":"run-detached-child"}}\n'
    else
      printf '{"status":"succeeded"}\n'
    fi
    ;;
  "run events"|"run logs")
    if [ "${FAKE_HELMR_FAIL_MODE:-}" = "run-events" ] &&
      [ "${1:-} ${2:-}" = "run events" ]; then
      printf 'intentional Run events failure\n' >&2
      exit 1
    fi
    printf '{"items":[]}\n'
    ;;
  *)
    printf 'unexpected fake Helmr command: %s\n' "$*" >&2
    exit 2
    ;;
esac
