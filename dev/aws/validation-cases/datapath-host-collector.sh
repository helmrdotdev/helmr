#!/usr/bin/env bash
set -euo pipefail

BASE=/run/helmr/datapath-validation
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TRACE_BIN="${SCRIPT_DIR}/datapath-host-trace.py"

usage() {
  cat <<'EOF'
Usage:
  datapath-host-collector.sh prepare CAMPAIGN_ID
  datapath-host-collector.sh start CAMPAIGN_ID CAPTURE_ID NETNS_NAME DURATION INTERFACE...
  datapath-host-collector.sh stop CAMPAIGN_ID CAPTURE_ID
  datapath-host-collector.sh export CAMPAIGN_ID CAPTURE_ID
  datapath-host-collector.sh cleanup CAMPAIGN_ID
  datapath-host-collector.sh verify-clean CAMPAIGN_ID

The collector records parsed packet headers only. It never stores packet
payload bytes. Each capture is limited to 256 events and 256 KiB.
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_root() {
  [ "$(id -u)" -eq 0 ] || die "root is required"
}

validate_campaign_id() {
  [[ "$1" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "invalid campaign ID"
}

validate_capture_id() {
  [[ "$1" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "invalid capture ID"
}

validate_netns_name() {
  [[ "$1" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] ||
    die "invalid network namespace name"
}

validate_interface() {
  [[ "$1" =~ ^[a-zA-Z0-9_.-]{1,15}$ ]] || die "invalid interface name"
}

campaign_dir() {
  validate_campaign_id "$1"
  printf '%s/%s\n' "${BASE}" "$1"
}

capture_prefix() {
  validate_campaign_id "$1"
  validate_capture_id "$2"
  printf '%s/%s/%s\n' "${BASE}" "$1" "$2"
}

assert_secure_directory() {
  local path=$1
  [ -d "${path}" ] || die "directory is absent: ${path}"
  [ ! -L "${path}" ] || die "directory cannot be a symlink: ${path}"
  [ "$(stat -c '%u:%a' "${path}")" = "0:700" ] ||
    die "directory must be root-owned mode 0700: ${path}"
}

unit_name() {
  printf 'helmr-datapath-%s-%s.service\n' "$1" "$2"
}

prepare() {
  local campaign=$1 dir
  dir="$(campaign_dir "${campaign}")"
  install -d -o root -g root -m 0700 "${BASE}"
  [ ! -L "${BASE}" ] || die "base directory cannot be a symlink"
  if [ -e "${dir}" ]; then
    assert_secure_directory "${dir}"
    [ -z "$(find "${dir}" -mindepth 1 -maxdepth 1 -print -quit)" ] ||
      die "campaign directory already contains state"
  else
    install -d -o root -g root -m 0700 "${dir}"
  fi
  [ -f "${TRACE_BIN}" ] && [ ! -L "${TRACE_BIN}" ] ||
    die "trace collector is unavailable"
  [ "$(stat -c '%u' "${TRACE_BIN}")" -eq 0 ] ||
    die "trace collector must be root-owned"
}

start_capture() {
  local campaign=$1 capture=$2 netns=$3 duration=$4
  shift 4
  [ "$#" -ge 1 ] || die "at least one interface is required"
  [[ "${duration}" =~ ^[0-9]+$ ]] && [ "${duration}" -ge 1 ] && [ "${duration}" -le 900 ] ||
    die "duration must be between 1 and 900 seconds"
  validate_netns_name "${netns}"
  local dir prefix unit interface args=()
  dir="$(campaign_dir "${campaign}")"
  assert_secure_directory "${dir}"
  prefix="$(capture_prefix "${campaign}" "${capture}")"
  unit="$(unit_name "${campaign}" "${capture}")"
  [ ! -e "${prefix}.trace.jsonl" ] || die "capture already exists"
  [ ! -e "${prefix}.summary.json" ] || die "capture summary already exists"
  [ ! -e "${prefix}.ready.json" ] || die "capture readiness already exists"
  [ ! -e "${prefix}.stop" ] || die "capture stop marker already exists"
  [ -e "/var/run/netns/${netns}" ] && [ ! -L "/var/run/netns/${netns}" ] ||
    die "network namespace is unavailable"
  for interface in "$@"; do
    validate_interface "${interface}"
    ip netns exec "${netns}" ip link show dev "${interface}" >/dev/null
    args+=(--interface "${interface}")
  done
  systemctl reset-failed "${unit}" >/dev/null 2>&1 || true
  systemd-run \
    --unit="${unit%.service}" \
    --property=Type=exec \
    --property=User=root \
    --property=Group=root \
    --property=UMask=0077 \
    --collect \
    --quiet \
    ip netns exec "${netns}" python3 "${TRACE_BIN}" \
      --output "${prefix}.trace.jsonl" \
      --summary "${prefix}.summary.json" \
      --ready "${prefix}.ready.json" \
      --stop "${prefix}.stop" \
      --duration "${duration}" \
      --max-events 256 \
      --max-bytes 262144 \
      "${args[@]}"
  local deadline=$((SECONDS + 15))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    [ -f "${prefix}.ready.json" ] && break
    systemctl is-active --quiet "${unit}" || die "collector exited before readiness"
    sleep 1
  done
  [ -f "${prefix}.ready.json" ] || die "collector readiness timed out"
  jq -e '.schema == "helmrdotdev.datapath-trace-ready.v0"' "${prefix}.ready.json" >/dev/null ||
    die "collector readiness is invalid"
  jq -cn --arg unit "${unit}" --arg netns "${netns}" '{
    schema:"helmrdotdev.datapath-collector-start.v0",
    unit:$unit,
    netns_name:$netns
  }'
}

stop_capture() {
  local campaign=$1 capture=$2 prefix unit deadline
  prefix="$(capture_prefix "${campaign}" "${capture}")"
  unit="$(unit_name "${campaign}" "${capture}")"
  [ -f "${prefix}.ready.json" ] || die "capture is not ready"
  : >"${prefix}.stop"
  chmod 0600 "${prefix}.stop"
  deadline=$((SECONDS + 20))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if ! systemctl is-active --quiet "${unit}"; then
      break
    fi
    sleep 1
  done
  if systemctl is-active --quiet "${unit}"; then
    systemctl stop "${unit}"
    die "collector did not stop after its exact stop marker"
  fi
  [ -f "${prefix}.summary.json" ] || die "collector summary is absent"
  jq -e '
    .schema == "helmrdotdev.datapath-trace-summary.v0" and
    .status == "completed" and .truncated == false and
    (.event_count | type == "number" and . >= 0 and . <= 256) and
    (.trace_bytes | type == "number" and . >= 0 and . <= 262144) and
    (.trace_sha256 | test("^[0-9a-f]{64}$"))
  ' "${prefix}.summary.json" >/dev/null || die "collector did not complete cleanly"
  cat "${prefix}.summary.json"
}

export_capture() {
  local campaign=$1 capture=$2 prefix summary_sha
  prefix="$(capture_prefix "${campaign}" "${capture}")"
  [ -f "${prefix}.summary.json" ] || die "capture must be stopped before export"
  [ -f "${prefix}.trace.jsonl" ] || die "capture trace is absent"
  jq -e '.status == "completed" and .truncated == false' "${prefix}.summary.json" >/dev/null ||
    die "truncated or failed capture cannot be exported"
  [ "$(wc -c <"${prefix}.trace.jsonl" | tr -d ' ')" -le 262144 ] ||
    die "trace exceeds the evidence limit"
  [ "$(wc -l <"${prefix}.trace.jsonl" | tr -d ' ')" -le 256 ] ||
    die "trace exceeds the event limit"
  summary_sha="$(sha256sum "${prefix}.summary.json" | awk '{print $1}')"
  jq -s \
    --arg summary_sha256 "${summary_sha}" \
    '{
      schema:"helmrdotdev.datapath-packet-evidence.v0",
      truncated:false,
      summary_sha256:$summary_sha256,
      events:.
    }' "${prefix}.trace.jsonl" >"${prefix}.packet.json.tmp"
  chmod 0600 "${prefix}.packet.json.tmp"
  [ "$(wc -c <"${prefix}.packet.json.tmp" | tr -d ' ')" -le 262144 ] ||
    die "packet evidence exceeds the evidence limit"
  mv "${prefix}.packet.json.tmp" "${prefix}.packet.json"
  cat "${prefix}.packet.json"
}

cleanup() {
  local campaign=$1 dir unit
  dir="$(campaign_dir "${campaign}")"
  [ -d "${dir}" ] || return 0
  assert_secure_directory "${dir}"
  while IFS= read -r unit; do
    [ -n "${unit}" ] || continue
    systemctl stop "${unit}" >/dev/null 2>&1 || true
    systemctl reset-failed "${unit}" >/dev/null 2>&1 || true
  done < <(systemctl list-units --all --plain --no-legend \
    "helmr-datapath-${campaign}-*.service" 2>/dev/null | awk '{print $1}')
  find "${dir}" -xdev -mindepth 1 -maxdepth 1 -type f -delete
  [ -z "$(find "${dir}" -xdev -mindepth 1 -print -quit)" ] ||
    die "campaign directory contains an unexpected object"
  rmdir "${dir}"
}

verify_clean() {
  local campaign=$1 dir
  dir="$(campaign_dir "${campaign}")"
  [ ! -e "${dir}" ] || die "campaign directory still exists"
  if systemctl list-units --all --plain --no-legend \
    "helmr-datapath-${campaign}-*.service" 2>/dev/null | grep -q .; then
    die "campaign collector unit still exists"
  fi
  jq -cn '{schema:"helmrdotdev.datapath-cleanup-check.v0",cleanup_verified:true}'
}

main() {
  local command=${1:-}
  case "${command}" in
    -h|--help|help|"")
      usage
      return
      ;;
  esac
  require_root
  case "${command}" in
    prepare)
      [ "$#" -eq 2 ] || die "prepare requires CAMPAIGN_ID"
      prepare "$2"
      ;;
    start)
      [ "$#" -ge 6 ] || die "start requires CAMPAIGN_ID CAPTURE_ID NETNS_NAME DURATION INTERFACE..."
      shift
      start_capture "$@"
      ;;
    stop)
      [ "$#" -eq 3 ] || die "stop requires CAMPAIGN_ID CAPTURE_ID"
      stop_capture "$2" "$3"
      ;;
    export)
      [ "$#" -eq 3 ] || die "export requires CAMPAIGN_ID CAPTURE_ID"
      export_capture "$2" "$3"
      ;;
    cleanup)
      [ "$#" -eq 2 ] || die "cleanup requires CAMPAIGN_ID"
      cleanup "$2"
      ;;
    verify-clean)
      [ "$#" -eq 2 ] || die "verify-clean requires CAMPAIGN_ID"
      verify_clean "$2"
      ;;
    *)
      usage >&2
      die "unknown command: ${command}"
      ;;
  esac
}

main "$@"
