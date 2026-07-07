#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: dev/aws/summarize-measurement-reports.sh [REPORT_DIR...]

Summarize strict latency measurement report directories produced by
dev/aws/run-smoke-with-path-report.sh.

Output is TSV with section types:
  report_summary  per report directory summary.txt metadata
  run_path        per run runtime-path classification from *.path.txt
  checkpoint_restore
                  per checkpoint restore attempt timing
  checkpoint_phase
                  per checkpoint creation phase timing
  checkpoint_restore_phase
                  per checkpoint restore attempt phase timing
  checkpoint_artifact
                  per checkpoint artifact role size/encrypt/store summary
  ux_delta        per user-visible timing delta from ux-timing.log
  ux_aggregate    aggregate count/p50/p95 by case, metric, and detail

When REPORT_DIR is omitted, the script reads every directory under
.helmr-aws-dev-smoke/path-reports.
EOF
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  usage
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE_DIR="${STATE_DIR:-${ROOT}/.helmr-aws-dev-smoke}"

if [ "$#" -gt 0 ]; then
  report_dirs=("$@")
else
  report_dirs=()
  if [ -d "${STATE_DIR}/path-reports" ]; then
    while IFS= read -r dir; do
      report_dirs+=("${dir}")
    done < <(find "${STATE_DIR}/path-reports" -mindepth 1 -maxdepth 1 -type d | sort)
  fi
fi

if [ "${#report_dirs[@]}" -eq 0 ]; then
  echo "no measurement report directories found" >&2
  exit 2
fi

python3 - "${report_dirs[@]}" <<'PY'
import math
import os
import sys
from pathlib import Path

report_dirs = [Path(arg) for arg in sys.argv[1:]]

summary_keys = (
    "label",
    "started_at",
    "command_name",
    "command_status",
    "run_count",
    "ux_timing_count",
    "surface_attestation_failures",
    "path_report_failures",
    "restore_evidence_failures",
)
path_hint_columns = (
    "has_live_wait",
    "has_resident_live_resume_evidence",
    "has_checkpoint_wait_evidence",
    "has_checkpoint_restore_lease_evidence",
    "has_prepared_runtime_claim_evidence",
    "has_workspace_mount_evidence",
)
checkpoint_artifact_summary_columns = (
    "run_checkpoint_id",
    "role",
    "artifact_count",
    "total_size_bytes",
    "total_encrypt_duration_ms",
    "total_store_duration_ms",
    "max_encrypt_duration_ms",
    "max_store_duration_ms",
)
checkpoint_restore_columns = (
    "id",
    "run_checkpoint_id",
    "run_wait_id",
    "run_lease_id",
    "worker_instance_id",
    "status",
    "started_at",
    "acknowledged_at",
    "finished_at",
    "restore_start_to_ack_ms",
    "restore_start_to_finished_ms",
    "error_message",
)
checkpoint_restore_phase_columns = (
    "run_checkpoint_restore_id",
    "ordinal",
    "name",
    "role",
    "media_type",
    "duration_ms",
    "error_class",
    "filepack_logical_bytes",
    "filepack_allocated_bytes",
    "filepack_sparse_supported",
    "filepack_sparse_data_ranges",
    "filepack_sparse_data_bytes",
    "filepack_zero_chunks_skipped",
    "filepack_encoded_chunks",
    "filepack_compressed_bytes",
    "filepack_unpack_written_bytes",
)
checkpoint_phase_columns = (
    "run_checkpoint_id",
    "ordinal",
    "name",
    "role",
    "media_type",
    "duration_ms",
    "error_class",
    "filepack_logical_bytes",
    "filepack_allocated_bytes",
    "filepack_sparse_supported",
    "filepack_sparse_data_ranges",
    "filepack_sparse_data_bytes",
    "filepack_zero_chunks_skipped",
    "filepack_encoded_chunks",
    "filepack_compressed_bytes",
    "filepack_unpack_written_bytes",
)

def tsv(*values):
    print("\t".join(str(value).replace("\t", " ").replace("\n", " ") for value in values))

def read_summary(report_dir):
    values = {}
    summary = report_dir / "summary.txt"
    if not summary.is_file():
        return values
    for line in summary.read_text(encoding="utf-8", errors="replace").splitlines():
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key] = value
    return values

def normalize_bool(value):
    value = value.strip().lower()
    if value in {"t", "true", "1", "yes"}:
        return "true"
    if value in {"f", "false", "0", "no"}:
        return "false"
    return ""

def parse_psql_table(path):
    rows = []
    for raw in path.read_text(encoding="utf-8", errors="replace").splitlines():
        if "|" not in raw:
            continue
        parts = [part.strip() for part in raw.split("|")]
        if not parts or parts[0] in {"section", "---------", "-------------"}:
            continue
        if parts[0] == "path_hints" and len(parts) >= 7:
            rows.append(dict(zip(("section",) + path_hint_columns, parts[:7])))
        if parts[0] == "checkpoint_artifact_summary" and len(parts) >= 9:
            rows.append(dict(zip(("section",) + checkpoint_artifact_summary_columns, parts[:9])))
        if parts[0] == "checkpoint_restore" and len(parts) >= 13:
            rows.append(dict(zip(("section",) + checkpoint_restore_columns, parts[:13])))
        if parts[0] == "checkpoint_phase" and len(parts) >= 17:
            rows.append(dict(zip(("section",) + checkpoint_phase_columns, parts[:17])))
        if parts[0] == "checkpoint_restore_phase" and len(parts) >= 17:
            rows.append(dict(zip(("section",) + checkpoint_restore_phase_columns, parts[:17])))
    return rows

def classify_path(hints):
    bools = {key: normalize_bool(hints.get(key, "")) == "true" for key in path_hint_columns}
    if bools["has_checkpoint_restore_lease_evidence"]:
        return "checkpoint_restore"
    if bools["has_checkpoint_wait_evidence"]:
        return "checkpoint_wait"
    if bools["has_live_wait"] or bools["has_resident_live_resume_evidence"]:
        return "resident_live_wait"
    if bools["has_prepared_runtime_claim_evidence"]:
        return "prepared_runtime"
    if bools["has_workspace_mount_evidence"]:
        return "workspace_mount"
    return "unknown"

def parse_ux_timing_line(line):
    if not line.startswith("ux_timing "):
        return None
    fields = {}
    for item in line[len("ux_timing "):].split():
        if "=" not in item:
            continue
        key, value = item.split("=", 1)
        fields[key] = value
    required = ("case", "event", "at_ms", "session_id", "run_id", "detail")
    if any(key not in fields for key in required):
        return None
    try:
        fields["at_ms"] = int(fields["at_ms"])
    except ValueError:
        return None
    return fields

def percentile(values, pct):
    if not values:
        return ""
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    rank = math.ceil((pct / 100.0) * len(ordered)) - 1
    rank = max(0, min(rank, len(ordered) - 1))
    return ordered[rank]

def ux_deltas(report_dir):
    ux_file = report_dir / "ux-timing.log"
    if not ux_file.is_file():
        return []
    records = []
    for line in ux_file.read_text(encoding="utf-8", errors="replace").splitlines():
        parsed = parse_ux_timing_line(line)
        if parsed:
            records.append(parsed)

    by_key_event = {}
    for record in records:
        key = (record["case"], record["session_id"], record["run_id"], record["detail"])
        by_key_event.setdefault(key, {})[record["event"]] = record

    event_pairs = (
        ("start_ack", "start_requested", "start_returned"),
        ("input_ack", "input_send_requested", "input_send_accepted"),
        ("token_visible", "token_wait_requested", "token_visible"),
        ("token_complete_ack", "token_complete_requested", "token_complete_accepted"),
        ("phase_visible", "phase_wait_requested", "phase_visible"),
        ("continuation_run_visible", "input_send_accepted", "continuation_run_visible"),
        ("terminal_after_start", "start_returned", "terminal_observed"),
        ("terminal_after_continuation_visible", "continuation_run_visible", "continuation_terminal_observed"),
    )

    deltas = []
    for (case, session_id, run_id, detail), events in by_key_event.items():
        for metric, start_event, end_event in event_pairs:
            if start_event not in events or end_event not in events:
                continue
            delta = events[end_event]["at_ms"] - events[start_event]["at_ms"]
            if delta < 0:
                continue
            deltas.append({
                "case": case,
                "session_id": session_id,
                "run_id": run_id,
                "detail": detail,
                "metric": metric,
                "start_event": start_event,
                "end_event": end_event,
                "delta_ms": delta,
            })

    visible_events = {
        "phase_visible",
        "token_visible",
        "continuation_run_visible",
        "continuation_visible",
        "terminal_observed",
        "continuation_terminal_observed",
    }
    def is_next_visible_candidate(record, candidate):
        if candidate["case"] != record["case"] or candidate["session_id"] != record["session_id"]:
            return False
        if candidate["event"] not in visible_events or candidate["at_ms"] < record["at_ms"]:
            return False
        if candidate["run_id"] == record["run_id"]:
            return True
        return candidate["detail"] == f"initial_run_id={record['run_id']}"

    for record in records:
        if record["event"] not in {"input_send_accepted", "token_complete_accepted"}:
            continue
        candidates = [
            candidate for candidate in records
            if is_next_visible_candidate(record, candidate)
        ]
        if not candidates:
            continue
        next_visible = min(candidates, key=lambda item: item["at_ms"])
        metric = "next_visible_after_input_ack" if record["event"] == "input_send_accepted" else "next_visible_after_token_ack"
        deltas.append({
            "case": record["case"],
            "session_id": record["session_id"],
            "run_id": record["run_id"],
            "detail": record["detail"],
            "metric": metric,
            "start_event": record["event"],
            "end_event": next_visible["event"],
            "delta_ms": next_visible["at_ms"] - record["at_ms"],
        })

    by_case = {}
    by_run = {}
    for record in records:
        by_case.setdefault(record["case"], []).append(record)
        if record["session_id"] and record["run_id"]:
            by_run.setdefault((record["case"], record["session_id"], record["run_id"]), []).append(record)

    for case, case_records in by_case.items():
        starts = sorted([record for record in case_records if record["event"] == "start_requested"], key=lambda item: item["at_ms"])
        returns = sorted([record for record in case_records if record["event"] == "start_returned"], key=lambda item: item["at_ms"])
        for start in starts:
            later_returns = [record for record in returns if record["at_ms"] >= start["at_ms"]]
            if not later_returns:
                continue
            returned = later_returns[0]
            deltas.append({
                "case": case,
                "session_id": returned["session_id"],
                "run_id": returned["run_id"],
                "detail": start["detail"],
                "metric": "start_ack",
                "start_event": "start_requested",
                "end_event": "start_returned",
                "delta_ms": returned["at_ms"] - start["at_ms"],
            })

    for (case, session_id, run_id), run_records in by_run.items():
        starts = sorted([record for record in run_records if record["event"] == "start_returned"], key=lambda item: item["at_ms"])
        terminals = sorted([
            record for record in run_records
            if record["event"] in {"terminal_observed", "continuation_terminal_observed"}
        ], key=lambda item: item["at_ms"])
        if starts and terminals:
            terminal = next((record for record in terminals if record["at_ms"] >= starts[0]["at_ms"]), None)
            if terminal:
                deltas.append({
                    "case": case,
                    "session_id": session_id,
                    "run_id": run_id,
                    "detail": terminal["detail"],
                    "metric": "terminal_after_start",
                    "start_event": "start_returned",
                    "end_event": terminal["event"],
                    "delta_ms": terminal["at_ms"] - starts[0]["at_ms"],
                })

    for record in records:
        if record["event"] != "continuation_run_visible" or not record["detail"].startswith("initial_run_id="):
            continue
        initial_run_id = record["detail"].split("=", 1)[1]
        initial_starts = [
            candidate for candidate in records
            if candidate["case"] == record["case"]
            and candidate["session_id"] == record["session_id"]
            and candidate["run_id"] == initial_run_id
            and candidate["event"] == "start_returned"
            and candidate["at_ms"] <= record["at_ms"]
        ]
        terminals = [
            candidate for candidate in records
            if candidate["case"] == record["case"]
            and candidate["session_id"] == record["session_id"]
            and candidate["run_id"] == record["run_id"]
            and candidate["event"] == "continuation_terminal_observed"
            and candidate["at_ms"] >= record["at_ms"]
        ]
        if initial_starts and terminals:
            start = max(initial_starts, key=lambda item: item["at_ms"])
            terminal = min(terminals, key=lambda item: item["at_ms"])
            deltas.append({
                "case": record["case"],
                "session_id": record["session_id"],
                "run_id": record["run_id"],
                "detail": terminal["detail"],
                "metric": "terminal_after_start",
                "start_event": "start_returned",
                "end_event": "continuation_terminal_observed",
                "delta_ms": terminal["at_ms"] - start["at_ms"],
            })
            deltas.append({
                "case": record["case"],
                "session_id": record["session_id"],
                "run_id": record["run_id"],
                "detail": record["detail"],
                "metric": "terminal_after_continuation_visible",
                "start_event": "continuation_run_visible",
                "end_event": "continuation_terminal_observed",
                "delta_ms": terminal["at_ms"] - record["at_ms"],
            })
    return deltas

all_deltas = []

tsv("section", "report_dir", *summary_keys)
for report_dir in report_dirs:
    summary = read_summary(report_dir)
    tsv("report_summary", report_dir, *(summary.get(key, "") for key in summary_keys))

tsv("section", "report_dir", "run_id", "path_class", *path_hint_columns)
for report_dir in report_dirs:
    for path_file in sorted(report_dir.glob("*.path.txt")):
        run_id = path_file.name.removesuffix(".path.txt")
        for hints in parse_psql_table(path_file):
            if hints.get("section") != "path_hints":
                continue
            tsv(
                "run_path",
                report_dir,
                run_id,
                classify_path(hints),
                *(normalize_bool(hints.get(key, "")) for key in path_hint_columns),
            )

tsv("section", "report_dir", "run_id", *checkpoint_artifact_summary_columns)
for report_dir in report_dirs:
    for path_file in sorted(report_dir.glob("*.path.txt")):
        run_id = path_file.name.removesuffix(".path.txt")
        for row in parse_psql_table(path_file):
            if row.get("section") != "checkpoint_artifact_summary":
                continue
            tsv(
                "checkpoint_artifact",
                report_dir,
                run_id,
                *(row.get(key, "") for key in checkpoint_artifact_summary_columns),
            )

tsv("section", "report_dir", "run_id", *checkpoint_restore_columns)
for report_dir in report_dirs:
    for path_file in sorted(report_dir.glob("*.path.txt")):
        run_id = path_file.name.removesuffix(".path.txt")
        for row in parse_psql_table(path_file):
            if row.get("section") != "checkpoint_restore":
                continue
            tsv(
                "checkpoint_restore",
                report_dir,
                run_id,
                *(row.get(key, "") for key in checkpoint_restore_columns),
            )

tsv("section", "report_dir", "run_id", *checkpoint_phase_columns)
for report_dir in report_dirs:
    for path_file in sorted(report_dir.glob("*.path.txt")):
        run_id = path_file.name.removesuffix(".path.txt")
        for row in parse_psql_table(path_file):
            if row.get("section") != "checkpoint_phase":
                continue
            tsv(
                "checkpoint_phase",
                report_dir,
                run_id,
                *(row.get(key, "") for key in checkpoint_phase_columns),
            )

tsv("section", "report_dir", "run_id", *checkpoint_restore_phase_columns)
for report_dir in report_dirs:
    for path_file in sorted(report_dir.glob("*.path.txt")):
        run_id = path_file.name.removesuffix(".path.txt")
        for row in parse_psql_table(path_file):
            if row.get("section") != "checkpoint_restore_phase":
                continue
            tsv(
                "checkpoint_restore_phase",
                report_dir,
                run_id,
                *(row.get(key, "") for key in checkpoint_restore_phase_columns),
            )

tsv("section", "report_dir", "case", "session_id", "run_id", "detail", "metric", "start_event", "end_event", "delta_ms")
for report_dir in report_dirs:
    for delta in ux_deltas(report_dir):
        all_deltas.append(delta)
        tsv(
            "ux_delta",
            report_dir,
            delta["case"],
            delta["session_id"],
            delta["run_id"],
            delta["detail"],
            delta["metric"],
            delta["start_event"],
            delta["end_event"],
            delta["delta_ms"],
        )

aggregate = {}
for delta in all_deltas:
    key = (delta["case"], delta["metric"], delta["detail"])
    aggregate.setdefault(key, []).append(delta["delta_ms"])

tsv("section", "case", "metric", "detail", "count", "min_ms", "p50_ms", "p95_ms", "max_ms")
for (case, metric, detail), values in sorted(aggregate.items()):
    ordered = sorted(values)
    tsv(
        "ux_aggregate",
        case,
        metric,
        detail,
        len(ordered),
        ordered[0],
        percentile(ordered, 50),
        percentile(ordered, 95),
        ordered[-1],
    )
PY
