#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
	printf 'not ok - %s\n' "$1" >&2
	exit 1
}

assert_contains() {
	local file="$1"
	local needle="$2"
	local label="$3"
	grep -Fq "$needle" "$file" || fail "$label: expected '$needle' in $file"
}

assert_not_contains() {
	local file="$1"
	local needle="$2"
	local label="$3"
	! grep -Fq "$needle" "$file" || fail "$label: did not expect '$needle' in $file"
}

assert_equal() {
	local expected="$1"
	local actual="$2"
	local label="$3"
	[ "$actual" = "$expected" ] || fail "$label: expected '$expected', got '$actual'"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fake_root="$tmp/repo"
mkdir -p "$fake_root/dev/aws"
cp "$repo_root/dev/aws/run-smoke-with-path-report.sh" "$fake_root/dev/aws/run-smoke-with-path-report.sh"
cat >"$fake_root/dev/aws/run-path-report.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >>"${FAKE_PATH_REPORT_CALLS:?FAKE_PATH_REPORT_CALLS is required}"
if [ "${FAKE_PATH_REPORT_MODE:-}" = "missing_restore" ]; then
	cat <<'REPORT'
 section | has_live_wait | has_resident_live_resume_evidence | has_checkpoint_wait_evidence | has_checkpoint_restore_lease_evidence | has_prepared_runtime_claim_evidence | has_workspace_mount_evidence
---------+---------------+-----------------------------------+------------------------------+----------------------------------------+--------------------------------------+-------------------------------
 path_hints | t | f | t | t | f | t
REPORT
	exit 0
fi
if [ "${FAKE_PATH_REPORT_MODE:-}" = "orphan_restore" ]; then
	cat <<'REPORT'
 section | has_live_wait | has_resident_live_resume_evidence | has_checkpoint_wait_evidence | has_checkpoint_restore_lease_evidence | has_prepared_runtime_claim_evidence | has_workspace_mount_evidence
---------+---------------+-----------------------------------+------------------------------+----------------------------------------+--------------------------------------+-------------------------------
 path_hints | t | f | t | t | f | t
 section | runtime_checkpoint_id | ordinal | name | role | media_type | duration_ms | error_class | filepack_logical_bytes | filepack_allocated_bytes | filepack_sparse_supported | filepack_sparse_data_ranges | filepack_sparse_data_bytes | filepack_zero_chunks_skipped | filepack_encoded_chunks | filepack_compressed_bytes | filepack_unpack_written_bytes
---------+-----------------------+---------+------+------|------------+-------------+-------------+------------------------+--------------------------+---------------------------+-----------------------------+----------------------------+------------------------------+-------------------------+---------------------------+-------------------------------
 checkpoint_phase | cp-a | 0 | pack_memory_filepack | memory | application/vnd.helmr.checkpoint-memory.v0.filepack | 20 |  | 1024 | [null] | t | 1 | 1024 | 0 | 1 | 512 | [null]
 section | id | runtime_checkpoint_id | run_wait_id | run_lease_id | worker_instance_id | status | started_at | acknowledged_at | finished_at | restore_start_to_ack_ms | restore_start_to_finished_ms | error_message
---------+----+-----------------------+-------------+--------------+--------------------+--------+------------+-----------------+-------------+-------------------------+------------------------------+--------------
 checkpoint_restore | restore-ok | cp-a | wait-a | lease-a | worker-a | restored | 2026-06-29 00:00:10+00 | 2026-06-29 00:00:15+00 | 2026-06-29 00:00:16+00 | 5000 | 6000 |
 checkpoint_restore | restore-orphan | cp-a | wait-a | lease-b | worker-a | restoring | 2026-06-29 00:00:20+00 | [null] | [null] | [null] | [null] |
 section | runtime_checkpoint_restore_id | ordinal | name | role | media_type | duration_ms | error_class | filepack_logical_bytes | filepack_allocated_bytes | filepack_sparse_supported | filepack_sparse_data_ranges | filepack_sparse_data_bytes | filepack_zero_chunks_skipped | filepack_encoded_chunks | filepack_compressed_bytes | filepack_unpack_written_bytes
---------+-------------------------------+---------+------+------|------------+-------------+-------------+------------------------+--------------------------+---------------------------+-----------------------------+----------------------------+------------------------------+-------------------------+---------------------------+-------------------------------
 checkpoint_restore_phase | restore-ok | 0 | restore_attach_guest_resume |  |  | 20 |  | [null] | [null] | [null] | [null] | [null] | [null] | [null] | [null] | [null]
REPORT
	exit 0
fi
printf 'fake path report for %s\n' "$1"
EOF
cat >"$fake_root/dev/aws/run-surface-attestation.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >>"${FAKE_SURFACE_ATTESTATION_CALLS:?FAKE_SURFACE_ATTESTATION_CALLS is required}"
printf 'fake surface attestation for %s\n' "$1"
EOF
chmod +x \
	"$fake_root/dev/aws/run-smoke-with-path-report.sh" \
	"$fake_root/dev/aws/run-path-report.sh" \
	"$fake_root/dev/aws/run-surface-attestation.sh"

state_dir="$tmp/state"
calls="$tmp/path-report-calls.txt"
attestation_calls="$tmp/surface-attestation-calls.txt"
stdout="$tmp/stdout"
stderr="$tmp/stderr"
: >"$calls"
: >"$attestation_calls"

STATE_DIR="$state_dir" FAKE_PATH_REPORT_CALLS="$calls" FAKE_SURFACE_ATTESTATION_CALLS="$attestation_calls" \
	"$fake_root/dev/aws/run-smoke-with-path-report.sh" no-runs-ok -- \
	sh -c 'printf "release smoke run ids: \n"' >"$stdout" 2>"$stderr"

summary="$(find "$state_dir/path-reports" -name summary.txt -print | sort | tail -1)"
assert_contains "$summary" "run_count=0" "no-run smoke run count"
assert_contains "$summary" "path_report_skipped=no_run_ids_extracted" "no-run smoke skipped path report"
assert_contains "$summary" "surface_attestation_before=skipped" "surface attestation should be skipped by default"
assert_contains "$summary" "surface_attestation_after=skipped" "surface attestation should be skipped by default after command"
assert_equal "" "$(cat "$calls")" "no-run smoke should not call path report"
assert_equal "" "$(cat "$attestation_calls")" "non-measurement smoke should not call attestation"

: >"$attestation_calls"
if STATE_DIR="$state_dir" FAKE_PATH_REPORT_CALLS="$calls" FAKE_SURFACE_ATTESTATION_CALLS="$attestation_calls" HELMR_PATH_REPORT_REQUIRE_RUNS=1 \
	"$fake_root/dev/aws/run-smoke-with-path-report.sh" no-runs-strict -- \
	sh -c 'printf "release smoke run ids: \n"' >"$stdout" 2>"$stderr"; then
	fail "strict no-run smoke should fail"
fi
assert_contains "$stderr" "path_report_error=no_run_ids_extracted" "strict no-run smoke error"
assert_equal "no-runs-strict-before
no-runs-strict-after" "$(cat "$attestation_calls")" "strict measurement should attest before and after even when no run is extracted"

: >"$calls"
: >"$attestation_calls"
top_level_id="11111111-1111-1111-1111-111111111111"
json_run_id="22222222-2222-2222-2222-222222222222"
line_run_id="33333333-3333-3333-3333-333333333333"
STATE_DIR="$state_dir" FAKE_PATH_REPORT_CALLS="$calls" FAKE_SURFACE_ATTESTATION_CALLS="$attestation_calls" HELMR_PATH_REPORT_REQUIRE_RUNS=1 \
	"$fake_root/dev/aws/run-smoke-with-path-report.sh" extract-json -- \
	sh -c 'printf "%s\n" "{\"id\":\"'"$top_level_id"'\",\"run\":{\"id\":\"'"$json_run_id"'\"}}" "ux_timing case=extract-json event=start_returned at_ms=123 session_id=s run_id='"$json_run_id"' detail=test" "run_id='"$line_run_id"'"' >"$stdout" 2>"$stderr"

report_dir="$(awk -F= '/^report_dir=/ { print $2 }' "$stdout" | tail -1)"
run_ids_file="$report_dir/run-ids.txt"
assert_not_contains "$run_ids_file" "$top_level_id" "top-level JSON id must not be treated as run id"
assert_contains "$run_ids_file" "$json_run_id" "JSON run.id extraction"
assert_contains "$run_ids_file" "$line_run_id" "run_id line extraction"
assert_contains "$report_dir/summary.txt" "run_count=2" "extracted run count"
assert_contains "$report_dir/summary.txt" "ux_timing_count=1" "UX timing extraction count"
assert_contains "$report_dir/summary.txt" "restore_evidence_failures=0" "restore evidence should pass when no checkpoint restore path is reported"
assert_contains "$report_dir/ux-timing.log" "ux_timing case=extract-json event=start_returned" "UX timing log extraction"
assert_contains "$report_dir/summary.txt" "surface_attestation_failures=0" "surface attestation should be required and successful in strict mode"
assert_equal "$json_run_id
$line_run_id" "$(cat "$calls")" "path reports should run for extracted run ids only"
assert_equal "extract-json-before
extract-json-after" "$(cat "$attestation_calls")" "strict measurement should attest before and after run extraction"

: >"$calls"
: >"$attestation_calls"
if STATE_DIR="$state_dir" FAKE_PATH_REPORT_CALLS="$calls" FAKE_SURFACE_ATTESTATION_CALLS="$attestation_calls" FAKE_PATH_REPORT_MODE=missing_restore HELMR_PATH_REPORT_REQUIRE_RUNS=1 \
	"$fake_root/dev/aws/run-smoke-with-path-report.sh" missing-restore -- \
	sh -c 'printf "run_id=44444444-4444-4444-4444-444444444444\n"' >"$stdout" 2>"$stderr"; then
	fail "strict checkpoint restore without durable restore evidence should fail"
fi
missing_restore_report_dir="$(awk -F= '/^report_dir=/ { print $2 }' "$stdout" | tail -1)"
assert_contains "$missing_restore_report_dir/summary.txt" "restore_evidence_failures=1" "missing restore row should be counted"
assert_contains "$stderr" "path_report_error=restore_evidence_failed" "missing restore row should fail strict report"

: >"$calls"
: >"$attestation_calls"
if STATE_DIR="$state_dir" FAKE_PATH_REPORT_CALLS="$calls" FAKE_SURFACE_ATTESTATION_CALLS="$attestation_calls" FAKE_PATH_REPORT_MODE=orphan_restore HELMR_PATH_REPORT_REQUIRE_RUNS=1 \
	"$fake_root/dev/aws/run-smoke-with-path-report.sh" orphan-restore -- \
	sh -c 'printf "run_id=55555555-5555-5555-5555-555555555555\n"' >"$stdout" 2>"$stderr"; then
	fail "strict checkpoint restore with orphan restoring row should fail"
fi
orphan_restore_report_dir="$(awk -F= '/^report_dir=/ { print $2 }' "$stdout" | tail -1)"
assert_contains "$orphan_restore_report_dir/summary.txt" "restore_evidence_failures=1" "orphan restore row should be counted"
assert_contains "$stderr" "status=restoring" "orphan restoring status should be reported"

: >"$calls"
: >"$attestation_calls"
STATE_DIR="$state_dir" FAKE_PATH_REPORT_CALLS="$calls" FAKE_SURFACE_ATTESTATION_CALLS="$attestation_calls" FAKE_PATH_REPORT_MODE=missing_restore \
	"$fake_root/dev/aws/run-smoke-with-path-report.sh" non-strict-missing-restore -- \
	sh -c 'printf "run_id=66666666-6666-6666-6666-666666666666\n"' >"$stdout" 2>"$stderr"
non_strict_report_dir="$(awk -F= '/^report_dir=/ { print $2 }' "$stdout" | tail -1)"
assert_contains "$non_strict_report_dir/summary.txt" "restore_evidence_failures=0" "non-strict observation should not enforce restore evidence"

printf 'ok - path report wrapper tests\n'
