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

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

report_a="$tmp/path-reports/20260629T000000Z-token-a"
report_b="$tmp/path-reports/20260629T000100Z-token-b"
report_c="$tmp/path-reports/20260629T000200Z-continuation"
mkdir -p "$report_a" "$report_b" "$report_c"

cat >"$report_a/summary.txt" <<'EOF'
label=token-a
started_at=20260629T000000Z
command_name=run-release-smoke.sh
command_status=0
run_count=1
ux_timing_count=8
surface_attestation_failures=0
path_report_failures=0
restore_evidence_failures=0
EOF

cat >"$report_b/summary.txt" <<'EOF'
label=token-b
started_at=20260629T000100Z
command_name=run-release-smoke.sh
command_status=0
run_count=1
ux_timing_count=8
surface_attestation_failures=0
path_report_failures=0
restore_evidence_failures=0
EOF

cat >"$report_a/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa.path.txt" <<'EOF'
 section | has_live_wait | has_resident_live_resume_evidence | has_checkpoint_wait_evidence | has_checkpoint_restore_lease_evidence | has_prepared_runtime_claim_evidence | has_workspace_mount_evidence
---------+---------------+-----------------------------------+------------------------------+----------------------------------------+--------------------------------------+-------------------------------
 path_hints | t | f | t | t | f | t
 section | runtime_checkpoint_id | role | artifact_count | total_size_bytes | total_encrypt_duration_ms | total_store_duration_ms | max_encrypt_duration_ms | max_store_duration_ms
---------+-----------------------+------+----------------+------------------+---------------------------+-------------------------+-------------------------+-----------------------
 checkpoint_artifact_summary | cp-a | memory | 1 | 104857600 | 9000 | 3000 | 9000 | 3000
 checkpoint_artifact_summary | cp-a | scratch_disk | 1 | 5242880 | 500 | 200 | 500 | 200
 section | runtime_checkpoint_id | ordinal | name | role | media_type | duration_ms | error_class | filepack_logical_bytes | filepack_allocated_bytes | filepack_sparse_supported | filepack_sparse_data_ranges | filepack_sparse_data_bytes | filepack_zero_chunks_skipped | filepack_encoded_chunks | filepack_compressed_bytes | filepack_unpack_written_bytes
---------+-----------------------+---------+------+------|------------+-------------+-------------+------------------------+--------------------------+---------------------------+-----------------------------+----------------------------+------------------------------+-------------------------+---------------------------+-------------------------------
 checkpoint_phase | cp-a | 0 | pack_memory_filepack | memory | application/vnd.helmr.checkpoint-memory.v0.filepack | 2400 |  | 4294967296 | 268435456 | t | 12 | 268435456 | 128 | 64 | 123456789 | [null]
 section | id | runtime_checkpoint_id | run_wait_id | run_lease_id | worker_instance_id | status | started_at | acknowledged_at | finished_at | restore_start_to_ack_ms | restore_start_to_finished_ms | error_message
---------+----+-----------------------+-------------+--------------+--------------------+--------+------------+-----------------+-------------+-------------------------+------------------------------+--------------
 checkpoint_restore | restore-a | cp-a | wait-a | lease-a | worker-a | restored | 2026-06-29 00:00:10+00 | 2026-06-29 00:00:15+00 | 2026-06-29 00:00:16+00 | 5000 | 6000 |
 section | runtime_checkpoint_restore_id | ordinal | name | role | media_type | duration_ms | error_class | filepack_logical_bytes | filepack_allocated_bytes | filepack_sparse_supported | filepack_sparse_data_ranges | filepack_sparse_data_bytes | filepack_zero_chunks_skipped | filepack_encoded_chunks | filepack_compressed_bytes | filepack_unpack_written_bytes
---------+-------------------------------+---------+------+------|------------+-------------+-------------+------------------------+--------------------------+---------------------------+-----------------------------+----------------------------+------------------------------+-------------------------+---------------------------+-------------------------------
 checkpoint_restore_phase | restore-a | 0 | restore_materialize_memory_filepack | memory | application/vnd.helmr.checkpoint-memory.v0.filepack | 1200 |  | 104857600 | 8388608 | t | 2 | 8388608 | 1024 | 64 | 2097152 | 8388608
EOF

cat >"$report_b/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb.path.txt" <<'EOF'
 section | has_live_wait | has_resident_live_resume_evidence | has_checkpoint_wait_evidence | has_checkpoint_restore_lease_evidence | has_prepared_runtime_claim_evidence | has_workspace_mount_evidence
---------+---------------+-----------------------------------+------------------------------+----------------------------------------+--------------------------------------+-------------------------------
 path_hints | t | f | f | f | f | f
EOF

cat >"$report_a/ux-timing.log" <<'EOF'
ux_timing case=staging-token-checkpoint event=start_requested at_ms=1000 session_id= run_id= detail=task=token-checkpoint-smoke
ux_timing case=staging-token-checkpoint event=start_returned at_ms=1250 session_id=s1 run_id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa detail=task=token-checkpoint-smoke
ux_timing case=staging-token-checkpoint event=token_wait_requested at_ms=2000 session_id=s1 run_id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa detail=step=decision
ux_timing case=staging-token-checkpoint event=token_visible at_ms=2600 session_id=s1 run_id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa detail=step=decision
ux_timing case=staging-token-checkpoint event=token_complete_requested at_ms=3000 session_id=s1 run_id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa detail=step=decision
ux_timing case=staging-token-checkpoint event=token_complete_accepted at_ms=3050 session_id=s1 run_id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa detail=step=decision
ux_timing case=staging-token-checkpoint event=token_visible at_ms=3650 session_id=s1 run_id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa detail=step=reply
ux_timing case=staging-token-checkpoint event=terminal_observed at_ms=5000 session_id=s1 run_id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa detail=status=succeeded
EOF

cat >"$report_b/ux-timing.log" <<'EOF'
ux_timing case=staging-token-checkpoint event=start_requested at_ms=1000 session_id= run_id= detail=task=token-checkpoint-smoke
ux_timing case=staging-token-checkpoint event=start_returned at_ms=1100 session_id=s2 run_id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb detail=task=token-checkpoint-smoke
ux_timing case=staging-token-checkpoint event=token_wait_requested at_ms=2000 session_id=s2 run_id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb detail=step=decision
ux_timing case=staging-token-checkpoint event=token_visible at_ms=2200 session_id=s2 run_id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb detail=step=decision
ux_timing case=staging-token-checkpoint event=token_complete_requested at_ms=3000 session_id=s2 run_id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb detail=step=decision
ux_timing case=staging-token-checkpoint event=token_complete_accepted at_ms=3025 session_id=s2 run_id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb detail=step=decision
ux_timing case=staging-token-checkpoint event=token_visible at_ms=3225 session_id=s2 run_id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb detail=step=reply
ux_timing case=staging-token-checkpoint event=terminal_observed at_ms=4000 session_id=s2 run_id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb detail=status=succeeded
EOF

cat >"$report_c/summary.txt" <<'EOF'
label=continuation
started_at=20260629T000200Z
command_name=run-release-smoke.sh
command_status=0
run_count=2
ux_timing_count=7
surface_attestation_failures=0
path_report_failures=0
restore_evidence_failures=0
EOF

cat >"$report_c/cccccccc-cccc-cccc-cccc-cccccccccccc.path.txt" <<'EOF'
 section | has_live_wait | has_resident_live_resume_evidence | has_checkpoint_wait_evidence | has_checkpoint_restore_lease_evidence | has_prepared_runtime_claim_evidence | has_workspace_mount_evidence
---------+---------------+-----------------------------------+------------------------------+----------------------------------------+--------------------------------------+-------------------------------
 path_hints | f | f | f | f | t | t
EOF

cat >"$report_c/dddddddd-dddd-dddd-dddd-dddddddddddd.path.txt" <<'EOF'
 section | has_live_wait | has_resident_live_resume_evidence | has_checkpoint_wait_evidence | has_checkpoint_restore_lease_evidence | has_prepared_runtime_claim_evidence | has_workspace_mount_evidence
---------+---------------+-----------------------------------+------------------------------+----------------------------------------+--------------------------------------+-------------------------------
 path_hints | t | t | f | f | f | f
EOF

cat >"$report_c/ux-timing.log" <<'EOF'
ux_timing case=staging-session-continuation event=start_requested at_ms=1000 session_id= run_id= detail=task=session-continuation-smoke
ux_timing case=staging-session-continuation event=start_returned at_ms=1200 session_id=s3 run_id=cccccccc-cccc-cccc-cccc-cccccccccccc detail=task=session-continuation-smoke
ux_timing case=staging-session-continuation event=input_send_requested at_ms=3000 session_id=s3 run_id=cccccccc-cccc-cccc-cccc-cccccccccccc detail=step=continuation
ux_timing case=staging-session-continuation event=input_send_accepted at_ms=3100 session_id=s3 run_id=cccccccc-cccc-cccc-cccc-cccccccccccc detail=step=continuation
ux_timing case=staging-session-continuation event=continuation_run_visible at_ms=4700 session_id=s3 run_id=dddddddd-dddd-dddd-dddd-dddddddddddd detail=initial_run_id=cccccccc-cccc-cccc-cccc-cccccccccccc
ux_timing case=staging-session-continuation event=continuation_terminal_observed at_ms=6200 session_id=s3 run_id=dddddddd-dddd-dddd-dddd-dddddddddddd detail=status=succeeded
ux_timing case=staging-session-continuation event=continuation_visible at_ms=6400 session_id=s3 run_id=dddddddd-dddd-dddd-dddd-dddddddddddd detail=phase=continuation
EOF

out="$tmp/summary.tsv"
"$repo_root/dev/aws/summarize-measurement-reports.sh" "$report_a" "$report_b" "$report_c" >"$out"

assert_contains "$out" $'report_summary	'"$report_a"$'	token-a	20260629T000000Z	run-release-smoke.sh	0	1	8	0	0	0' "report A summary"
assert_contains "$out" $'run_path	'"$report_a"$'	aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa	checkpoint_restore	true	false	true	true	false	true' "checkpoint restore path classification should beat generic live wait"
assert_contains "$out" $'run_path	'"$report_b"$'	bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb	resident_live_wait	true	false	false	false	false	false' "resident live path classification"
assert_contains "$out" $'run_path	'"$report_c"$'	cccccccc-cccc-cccc-cccc-cccccccccccc	prepared_runtime	false	false	false	false	true	true' "prepared runtime path classification"
assert_contains "$out" $'checkpoint_artifact	'"$report_a"$'	aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa	cp-a	memory	1	104857600	9000	3000	9000	3000' "checkpoint memory artifact summary"
assert_contains "$out" $'checkpoint_artifact	'"$report_a"$'	aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa	cp-a	scratch_disk	1	5242880	500	200	500	200' "checkpoint scratch artifact summary"
assert_contains "$out" $'checkpoint_phase	'"$report_a"$'	aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa	cp-a	0	pack_memory_filepack	memory	application/vnd.helmr.checkpoint-memory.v0.filepack	2400		4294967296	268435456	t	12	268435456	128	64	123456789	[null]' "checkpoint creation phase summary"
assert_contains "$out" $'checkpoint_restore	'"$report_a"$'	aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa	restore-a	cp-a	wait-a	lease-a	worker-a	restored	2026-06-29 00:00:10+00	2026-06-29 00:00:15+00	2026-06-29 00:00:16+00	5000	6000	' "checkpoint restore summary"
assert_contains "$out" $'checkpoint_restore_phase	'"$report_a"$'	aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa	restore-a	0	restore_materialize_memory_filepack	memory	application/vnd.helmr.checkpoint-memory.v0.filepack	1200		104857600	8388608	t	2	8388608	1024	64	2097152	8388608' "checkpoint restore phase summary"
assert_contains "$out" $'ux_delta	'"$report_a"$'	staging-token-checkpoint	s1	aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa	step=decision	token_visible	token_wait_requested	token_visible	600' "token visible delta"
assert_contains "$out" $'ux_delta	'"$report_a"$'	staging-token-checkpoint	s1	aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa	step=decision	next_visible_after_token_ack	token_complete_accepted	token_visible	600' "next visible after token completion"
assert_contains "$out" $'ux_delta	'"$report_b"$'	staging-token-checkpoint	s2	bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb	status=succeeded	terminal_after_start	start_returned	terminal_observed	2900' "terminal after start delta"
assert_contains "$out" $'ux_delta	'"$report_c"$'	staging-session-continuation	s3	cccccccc-cccc-cccc-cccc-cccccccccccc	step=continuation	input_ack	input_send_requested	input_send_accepted	100' "continuation input ack delta"
assert_contains "$out" $'ux_delta	'"$report_c"$'	staging-session-continuation	s3	cccccccc-cccc-cccc-cccc-cccccccccccc	step=continuation	next_visible_after_input_ack	input_send_accepted	continuation_run_visible	1600' "continuation next visible after input ack across run ids"
assert_contains "$out" $'ux_delta	'"$report_c"$'	staging-session-continuation	s3	dddddddd-dddd-dddd-dddd-dddddddddddd	status=succeeded	terminal_after_start	start_returned	continuation_terminal_observed	5000' "continuation terminal after start"
assert_contains "$out" $'ux_aggregate	staging-token-checkpoint	token_visible	step=decision	2	200	200	600	600' "token visible aggregate"
assert_contains "$out" $'ux_aggregate	staging-token-checkpoint	next_visible_after_token_ack	step=decision	2	200	200	600	600' "next-visible aggregate"

printf 'ok - measurement report summary tests\n'
