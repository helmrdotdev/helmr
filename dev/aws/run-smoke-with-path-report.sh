#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: dev/aws/run-smoke-with-path-report.sh LABEL -- COMMAND [ARG...]

Run a smoke command, capture its output, extract reported run ids, and write
AWS dev runtime path reports for each run. The command is not modified; this is
observation glue for latency/root-cause analysis.

Set HELMR_PATH_REPORT_REQUIRE_RUNS=1 when a measurement must create at least
one run. Leave it unset for smoke cases that can pass before run creation, such
as expected validation rejections.

Example:
  AWS_PROFILE=helmr-dev HELMR_PATH_REPORT_ALLOW_ECS_TASK=1 nix develop .#infra -c \
    dev/aws/run-smoke-with-path-report.sh stream-hot -- \
    env HELMR_API_URL=https://dev.helmr.dev SKIP_DEPLOY=1 \
    dev/workflows/scripts/run-release-smoke.sh
EOF
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  usage
  exit 0
fi

if [ "$#" -lt 3 ] || [ "${2:-}" != "--" ]; then
  usage >&2
  exit 2
fi

label=$1
shift 2
command_display=$1
if [ "${command_display}" = "env" ]; then
  for arg in "${@:2}"; do
    case "${arg}" in
      *=*) ;;
      *) command_display="${arg}"; break ;;
    esac
  done
fi

case "${label}" in
  *[!A-Za-z0-9._-]*|"")
    echo "LABEL must contain only letters, numbers, '.', '_', or '-'" >&2
    exit 2
    ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE_DIR="${STATE_DIR:-${ROOT}/.helmr-aws-dev-smoke}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
report_dir="${STATE_DIR}/path-reports/${stamp}-${label}"
mkdir -p "${report_dir}"

smoke_log="${report_dir}/smoke.log"
stdout_log="${report_dir}/stdout.log"
stderr_log="${report_dir}/stderr.log"
run_ids_file="${report_dir}/run-ids.txt"
summary_file="${report_dir}/summary.txt"
ux_timing_file="${report_dir}/ux-timing.log"
surface_attestation="${HELMR_SURFACE_ATTESTATION:-${HELMR_PATH_REPORT_REQUIRE_RUNS:-0}}"

{
  printf 'label=%s\n' "${label}"
  printf 'started_at=%s\n' "${stamp}"
  printf 'command_name=%s\n' "${command_display##*/}"
  printf 'command_argc=%s\n' "$#"
} >"${summary_file}"

run_surface_attestation() {
  local phase=$1
  local attestation_log="${report_dir}/surface-${phase}.txt"
  if [ "${surface_attestation}" != "1" ]; then
    printf 'surface_attestation_%s=skipped\n' "${phase}" >>"${summary_file}"
    return 0
  fi
  if "${ROOT}/dev/aws/run-surface-attestation.sh" "${label}-${phase}" >"${attestation_log}" 2>&1; then
    printf 'surface_attestation_%s=%s\n' "${phase}" "${attestation_log}" >>"${summary_file}"
    return 0
  fi
  printf 'surface_attestation_%s_failed=%s\n' "${phase}" "${attestation_log}" >>"${summary_file}"
  cat "${attestation_log}" >&2
  return 1
}

attestation_failures=0
if ! run_surface_attestation "before"; then
  attestation_failures=$((attestation_failures + 1))
fi

set +e
"$@" >"${stdout_log}" 2>"${stderr_log}"
command_status=$?
set -e
cat "${stdout_log}"
cat "${stderr_log}" >&2
cat "${stdout_log}" "${stderr_log}" >"${smoke_log}"
grep -E '^ux_timing ' "${smoke_log}" >"${ux_timing_file}" || true

if ! run_surface_attestation "after"; then
  attestation_failures=$((attestation_failures + 1))
fi

uuid_egrep='[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}'
{
  awk '/^release smoke run ids:/ { for (i = 5; i <= NF; i++) print $i }' "${smoke_log}" |
    grep -E "^${uuid_egrep}$" || true
  grep -Eo "(^|[[:space:]])[A-Za-z_]*run_id=${uuid_egrep}([^0-9a-fA-F-]|$)" "${smoke_log}" |
    sed -E "s/.*=(${uuid_egrep})([^0-9a-fA-F-])?$/\\1/" || true
  if command -v jq >/dev/null 2>&1; then
    jq -Rr --arg uuid_re "^${uuid_egrep}$" '
      fromjson?
      | select(type == "object")
      | (.run.id? // .run_id? // .runId? // empty)
      | select(type == "string" and test($uuid_re))
    ' "${smoke_log}" || true
  fi
  if command -v perl >/dev/null 2>&1; then
    export HELMR_PATH_REPORT_UUID_RE="^${uuid_egrep}$"
    perl -0ne '
      my $uuid_re = qr/$ENV{HELMR_PATH_REPORT_UUID_RE}/;
      while (/"run"\s*:\s*\{[^{}]*"id"\s*:\s*"([^"]+)"/sg) { my $id = $1; print "$id\n" if $id =~ $uuid_re }
      while (/"(?:run_id|runId)"\s*:\s*"([^"]+)"/sg) { my $id = $1; print "$id\n" if $id =~ $uuid_re }
    ' "${smoke_log}" || true
  fi
} |
  grep -E "^${uuid_egrep}$" |
  sort -u >"${run_ids_file}" || true

run_count="$(wc -l <"${run_ids_file}" | tr -d ' ')"
ux_timing_count="$(wc -l <"${ux_timing_file}" | tr -d ' ')"
path_report_failures=0
restore_evidence_failures=0

{
  printf 'command_status=%s\n' "${command_status}"
  printf 'run_count=%s\n' "${run_count}"
  printf 'ux_timing_count=%s\n' "${ux_timing_count}"
  printf 'surface_attestation_failures=%s\n' "${attestation_failures}"
} >>"${summary_file}"

while IFS= read -r run_id; do
  [ -n "${run_id}" ] || continue
  report="${report_dir}/${run_id}.path.txt"
  path_report_ok=0
  path_report_attempts="${HELMR_PATH_REPORT_ATTEMPTS:-3}"
  for attempt in $(seq 1 "${path_report_attempts}"); do
    attempt_report="${report}.attempt-${attempt}"
    if "${ROOT}/dev/aws/run-path-report.sh" "${run_id}" >"${attempt_report}" 2>&1 && [ -s "${attempt_report}" ]; then
      mv "${attempt_report}" "${report}"
      path_report_ok=1
      if [ "${attempt}" != "1" ]; then
        printf 'path_report_retry_ok=%s attempt=%s\n' "${run_id}" "${attempt}" >>"${summary_file}"
      fi
      break
    fi
    mv "${attempt_report}" "${report}"
    printf 'path_report_attempt_failed=%s attempt=%s\n' "${run_id}" "${attempt}" >>"${summary_file}"
    if [ "${attempt}" != "${path_report_attempts}" ]; then
      sleep 2
    fi
  done
  if [ "${path_report_ok}" = "1" ]; then
    printf 'path_report_ok=%s\n' "${run_id}" >>"${summary_file}"
  else
    path_report_failures=$((path_report_failures + 1))
    printf 'path_report_failed=%s\n' "${run_id}" >>"${summary_file}"
  fi
done <"${run_ids_file}"

validate_restore_evidence() {
  python3 - "${report_dir}" <<'PY'
import sys
from pathlib import Path

report_dir = Path(sys.argv[1])
failures = []

def normalize(value):
    return value.strip().lower()

def parse_rows(path):
    rows = []
    for raw in path.read_text(encoding="utf-8", errors="replace").splitlines():
        if "|" not in raw:
            continue
        parts = [part.strip() for part in raw.split("|")]
        if not parts or parts[0] in {"section", "---------", "-------------"}:
            continue
        rows.append(parts)
    return rows

for path in sorted(report_dir.glob("*.path.txt")):
    run_id = path.name.removesuffix(".path.txt")
    rows = parse_rows(path)
    has_restore_lease = any(
        len(row) >= 5
        and row[0] == "path_hints"
        and normalize(row[4]) in {"t", "true", "1", "yes"}
        for row in rows
    )
    if not has_restore_lease:
        continue
    restore_rows = [row for row in rows if row and row[0] == "checkpoint_restore"]
    if not restore_rows:
        failures.append(f"{run_id}:missing_checkpoint_restore_row")
        continue
    checkpoint_phase_rows = [row for row in rows if row and row[0] == "checkpoint_phase"]
    if not checkpoint_phase_rows:
        failures.append(f"{run_id}:missing_checkpoint_phase_row")
        continue
    restore_phase_rows = [row for row in rows if row and row[0] == "checkpoint_restore_phase"]
    if not restore_phase_rows:
        failures.append(f"{run_id}:missing_checkpoint_restore_phase_row")
        continue
    row_failures = []
    for row in restore_rows:
        if len(row) < 13:
            row_failures.append("truncated_checkpoint_restore_row")
            continue
        status = row[6]
        acknowledged_at = row[8]
        finished_at = row[9]
        start_to_ack_ms = row[10]
        start_to_finished_ms = row[11]
        error_message = row[12]
        if status != "restored":
            row_failures.append(f"status={status}")
            continue
        if acknowledged_at in {"", "[null]"} or finished_at in {"", "[null]"}:
            row_failures.append("missing_restore_timestamps")
            continue
        if start_to_ack_ms in {"", "[null]"} or start_to_finished_ms in {"", "[null]"}:
            row_failures.append("missing_restore_durations")
            continue
        if error_message not in {"", "[null]"}:
            row_failures.append("restore_error_message")
            continue
    if row_failures:
        failures.append(f"{run_id}:{','.join(row_failures) or 'invalid_checkpoint_restore_row'}")

for failure in failures:
    print(failure)
raise SystemExit(1 if failures else 0)
PY
}

if [ "${command_status}" = "0" ] && [ "${path_report_failures}" = "0" ] && [ "${HELMR_PATH_REPORT_REQUIRE_RUNS:-0}" = "1" ]; then
  restore_evidence_log="${report_dir}/restore-evidence-failures.txt"
  if validate_restore_evidence >"${restore_evidence_log}" 2>&1; then
    rm -f "${restore_evidence_log}"
  else
    restore_evidence_failures="$(wc -l <"${restore_evidence_log}" | tr -d ' ')"
    cat "${restore_evidence_log}" >&2
  fi
fi

{
  printf 'path_report_failures=%s\n' "${path_report_failures}"
  printf 'restore_evidence_failures=%s\n' "${restore_evidence_failures}"
  if [ "${command_status}" = "0" ] && [ "${run_count}" = "0" ]; then
    printf 'path_report_skipped=no_run_ids_extracted\n'
  fi
} >>"${summary_file}"
printf 'report_dir=%s\n' "${report_dir}"
cat "${summary_file}"

if [ "${command_status}" = "0" ] && [ "${run_count}" = "0" ] && [ "${HELMR_PATH_REPORT_REQUIRE_RUNS:-0}" = "1" ]; then
  printf 'path_report_error=no_run_ids_extracted\n' >&2
  exit 3
fi
if [ "${command_status}" = "0" ] && [ "${path_report_failures}" != "0" ]; then
  printf 'path_report_error=report_failed\n' >&2
  exit 3
fi
if [ "${command_status}" = "0" ] && [ "${restore_evidence_failures}" != "0" ]; then
  printf 'path_report_error=restore_evidence_failed\n' >&2
  exit 3
fi
if [ "${command_status}" = "0" ] && [ "${attestation_failures}" != "0" ]; then
  printf 'path_report_error=surface_attestation_failed\n' >&2
  exit 3
fi
exit "${command_status}"
