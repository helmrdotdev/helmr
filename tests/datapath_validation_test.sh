#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

profile="${root}/dev/aws/datapath-validation-profile.json"
bash -n \
  "${root}/dev/aws/validation-cases/datapath-host-collector.sh" \
  "${root}/dev/aws/validation-cases/datapath-validation.sh" ||
  fail "datapath shell syntax"
python3 - <<PY
from pathlib import Path
for name in (
    "dev/aws/validation-cases/datapath-host-trace.py",
    "dev/workflows/tasks/smoke/datapath-network-probe.py",
):
    compile(Path("${root}", name).read_bytes(), name, "exec")
PY
PYTHONDONTWRITEBYTECODE=1 \
  python3 - "${root}/dev/workflows/tasks/smoke/datapath-network-probe.py" <<'PY'
import importlib.util
import socket
import sys
import threading

spec = importlib.util.spec_from_file_location("datapath_network_probe", sys.argv[1])
assert spec is not None and spec.loader is not None
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
server.bind(("127.0.0.1", 0))
server.listen(1)
port = server.getsockname()[1]

def accept_once():
    connection, _ = server.accept()
    connection.close()
    server.close()

thread = threading.Thread(target=accept_once)
thread.start()
result = module.run({
    "mode": "tcp",
    "target": "127.0.0.1",
    "port": port,
    "attempts": 1,
    "timeoutMs": 1000,
})
thread.join(timeout=2)
assert not thread.is_alive()
attempt = result["attempts"][0]
assert attempt["outcome"] == "observed"
assert attempt["errno"] is None
assert attempt["flow"]["protocol"] == "tcp"
assert attempt["flow"]["sourceAddress"] == "127.0.0.1"
assert attempt["flow"]["destinationAddress"] == "127.0.0.1"
assert attempt["flow"]["destinationPort"] == port
PY
"${root}/dev/aws/validation-cases/datapath-host-collector.sh" --help |
  grep -Fq "records parsed packet headers only" ||
  fail "collector help must state the payload boundary"
jq -e '
  .schema == "helmrdotdev.aws-datapath-validation-profile.v0" and
  .topology == {
    allow_extended_worker_capacity:true,
    build_worker_max:1,
    isolated_stack:true,
    nat_gateway_max:1,
    ready_run_workers:2,
    run_worker_execution_slots:2,
    run_worker_max:2
  } and
  .evidence.max_trace_events == 256 and
  .evidence.max_case_bytes == 262144 and
  .evidence.remote_retrieval == "chunked-byte-count-sha256" and
  .evidence.artifact_kinds == ["packet","rules","binding","topology","cleanup"] and
  (.cases | length) == 1 and
  .cases[0].id == "hook-discovery" and
  .cases[0].producer.path == "dev/aws/validation-cases/datapath-validation.sh"
' "${profile}" >/dev/null || fail "strict datapath profile"

dry_result="${tmp}/dry-result.json"
mkdir "${tmp}/dry-artifacts"
HELMR_VALIDATION_CASE='{"id":"hook-discovery","payload":null,"producer":{"checks":["cleanup-proven"]}}' \
HELMR_VALIDATION_CASE_RESULT_FILE="${dry_result}" \
HELMR_VALIDATION_CASE_ARTIFACT_DIR="${tmp}/dry-artifacts" \
HELMR_VALIDATION_DRY_RUN=1 \
  "${root}/dev/aws/validation-cases/datapath-validation.sh"
jq -e '
  .schema == "helmrdotdev.validation-case-source-result.v2" and
  .status == "passed" and .observations.dry_run == true
' "${dry_result}" >/dev/null || fail "datapath producer dry run"

# shellcheck disable=SC1091
HELMR_VALIDATION_PRODUCT_ROOT="${root}" \
HELMR_VALIDATION_PROFILE="${profile}" \
  source "${root}/dev/aws/run-validation-campaign.sh"

isolated_manifest="${tmp}/isolated-manifest.json"
jq -cn '{
  environment:{
    dev_name:"helmr-datapath-a1b2c3d4",
    state_key:"helmr/stacks/dev/datapath/helmr-datapath-a1b2c3d4.tfstate"
  }
}' >"${isolated_manifest}"
validate_datapath_manifest_environment "${isolated_manifest}" ||
  fail "dedicated datapath stack namespace"
jq '.environment.state_key="helmr/stacks/dev/shared.tfstate"' \
  "${isolated_manifest}" >"${tmp}/shared-manifest.json"
if validate_datapath_manifest_environment "${tmp}/shared-manifest.json"; then
  fail "shared state key must be rejected"
fi
(
  # shellcheck disable=SC2329
  terraform_state_bucket_name() {
    printf 'state-bucket\n'
  }
  # shellcheck disable=SC2329
  aws() {
    printf '{"Versions":[],"DeleteMarkers":[]}\n'
  }
  verify_fresh_datapath_state "${isolated_manifest}"
) || fail "never-used datapath state key"
if datapath_state_versions_are_fresh \
  '{"Versions":[{"Key":"helmr/stacks/dev/datapath/helmr-datapath-a1b2c3d4.tfstate"}],"DeleteMarkers":[]}' \
  "helmr/stacks/dev/datapath/helmr-datapath-a1b2c3d4.tfstate"; then
  fail "used datapath state key must be rejected"
fi

artifact_dir="${tmp}/artifacts"
mkdir -p "${artifact_dir}"
packet_sha=""
rules_sha=""
binding_sha=""
topology_sha=""
cleanup_sha=""
for kind in packet rules binding topology cleanup; do
  case "${kind}" in
    packet)
      jq -cn '{schema:"helmrdotdev.datapath-packet-evidence.v0",events:[],truncated:false}' \
        >"${tmp}/${kind}.json"
      ;;
    cleanup)
      jq -cn '{
        schema:"helmrdotdev.datapath-cleanup-evidence.v0",
        cleanup_verified:true,
        candidate_objects_absent:true,
        legacy_defense_present:true
      }' >"${tmp}/${kind}.json"
      ;;
    *)
      jq -cn --arg kind "${kind}" \
        '{schema:("helmrdotdev.datapath-" + $kind + "-evidence.v0")}' \
        >"${tmp}/${kind}.json"
      ;;
  esac
  digest="$(sha256_file "${tmp}/${kind}.json")"
  mv "${tmp}/${kind}.json" "${artifact_dir}/${kind}-${digest}.json"
  printf -v "${kind}_sha" '%s' "${digest}"
done

jq -cn \
  --arg source_commit "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
  --arg worker_provenance "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" \
  --arg packet "${packet_sha}" \
  --arg rules "${rules_sha}" \
  --arg binding "${binding_sha}" \
  --arg topology "${topology_sha}" \
  --arg cleanup "${cleanup_sha}" \
  '{
    schema:"helmrdotdev.datapath-evidence.v0",
    source_commit:$source_commit,
    worker_image_provenance_sha256:$worker_provenance,
    collector_sha256:("c" * 64),
    probe_sha256:("d" * 64),
    candidate_datapath_abi:"datapath-candidate.v0",
    candidate_hook_set_sha256:("e" * 64),
    case_verdict_sha256:("f" * 64),
    artifacts:{
      packet:$packet,rules:$rules,binding:$binding,topology:$topology,cleanup:$cleanup
    },
    truncated:false
  }' >"${tmp}/manifest.json"
manifest_sha="$(sha256_file "${tmp}/manifest.json")"
mv "${tmp}/manifest.json" "${artifact_dir}/manifest-${manifest_sha}.json"
artifact_bytes="$(find "${artifact_dir}" -maxdepth 1 -type f -exec wc -c {} + | awk 'END {print $1 + 0}')"

manifest="${tmp}/campaign.json"
jq -cn '{
  source:{commit:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  artifacts:{worker_image_provenance_sha256:("b" * 64)}
}' >"${manifest}"
evidence="${tmp}/case.json"
jq -cn --arg sha "${manifest_sha}" --argjson bytes "${artifact_bytes}" '{
  schema:"helmrdotdev.validation-case-source-result.v2",
  status:"passed",
  reason:null,
  checks:[{id:"cleanup-proven",status:"passed"}],
  objects:{run_ids:[],workspace_ids:[],deployment_ids:[],schedule_ids:[],token_ids:[],actor_ids:[]},
  observations:{
    artifact_bytes:$bytes,
    artifact_count:6,
    artifact_manifest_sha256:$sha,
    cleanup_verified:true,
    trace_event_count:0,
    truncated:false
  }
}' >"${evidence}"

datapath_case_artifacts_are_valid "${manifest}" "${evidence}" "${artifact_dir}" ||
  fail "valid content-addressed artifact set"

jq '.observations.truncated=true' "${evidence}" >"${tmp}/truncated.json"
if datapath_case_artifacts_are_valid "${manifest}" "${tmp}/truncated.json" "${artifact_dir}"; then
  fail "truncation must fail"
fi

cp "${artifact_dir}/packet-${packet_sha}.json" "${tmp}/packet-original.json"
printf ' ' >>"${artifact_dir}/packet-${packet_sha}.json"
if datapath_case_artifacts_are_valid "${manifest}" "${evidence}" "${artifact_dir}"; then
  fail "artifact content/hash mismatch must fail"
fi
mv "${tmp}/packet-original.json" "${artifact_dir}/packet-${packet_sha}.json"

case_json='{"id":"hook-discovery","payload":null,"producer":{"checks":["cleanup-proven"]}}'
result_file="${tmp}/unused-result.json"
# shellcheck disable=SC2030
(
  export HELMR_VALIDATION_CASE="${case_json}"
  export HELMR_VALIDATION_CASE_RESULT_FILE="${result_file}"
  # shellcheck disable=SC1091
  source "${root}/dev/aws/validation-cases/case-lib.sh"
  validation_cleanup_ledger_init
  for state in created fenced removed verified; do
    validation_cleanup_record nft i-0123456789abcdef0 datapath-table "${state}"
  done
  validation_cleanup_ledger_proven
) || fail "cleanup ledger requires fence/remove/verify closure"

# shellcheck disable=SC2030,SC2031
(
  export HELMR_VALIDATION_CASE="${case_json}"
  export HELMR_VALIDATION_CASE_RESULT_FILE="${result_file}"
  # shellcheck disable=SC1091
  source "${root}/dev/aws/validation-cases/case-lib.sh"
  validation_cleanup_ledger_init
  validation_cleanup_record nft i-0123456789abcdef0 datapath-table created
  validation_cleanup_record nft i-0123456789abcdef0 datapath-table removed
  validation_cleanup_record nft i-0123456789abcdef0 datapath-table fenced
  validation_cleanup_record nft i-0123456789abcdef0 datapath-table verified
  ! validation_cleanup_ledger_proven
) || fail "cleanup ledger must reject an out-of-order teardown"

# shellcheck disable=SC2030,SC2031
(
  export HELMR_VALIDATION_CASE="${case_json}"
  export HELMR_VALIDATION_CASE_RESULT_FILE="${result_file}"
  # shellcheck disable=SC1091
  source "${root}/dev/aws/validation-cases/case-lib.sh"
  remote_fixture="${tmp}/remote-evidence.json"
  printf '%050000d\n' 0 >"${remote_fixture}"
  # shellcheck disable=SC2329
  validation_ssm() {
    local command=$2
    if [[ "${command}" == *"bytes=\$("* ]]; then
      printf '%s %s\n' \
        "$(wc -c <"${remote_fixture}" | tr -d ' ')" \
        "$(validation_sha256_file "${remote_fixture}")"
      return
    fi
    local skip count
    skip="$(sed -E 's/.* skip=([0-9]+) .*/\1/' <<<"${command}")"
    count="$(sed -E 's/.* count=([0-9]+) .*/\1/' <<<"${command}")"
    dd if="${remote_fixture}" bs=1 skip="${skip}" count="${count}" status=none |
      base64 | tr -d '\n'
  }
  retrieved="${tmp}/retrieved.json"
  validation_ssm_retrieve_file \
    i-0123456789abcdef0 \
    /run/helmr/datapath-validation/test/evidence.json \
    "${retrieved}" 4096 >/dev/null
  cmp "${remote_fixture}" "${retrieved}"
) || fail "chunked retrieval must verify byte count and SHA-256"

# shellcheck disable=SC2030,SC2031
(
  export HELMR_VALIDATION_CASE="${case_json}"
  export HELMR_VALIDATION_CASE_RESULT_FILE="${result_file}"
  # shellcheck disable=SC1091
  source "${root}/dev/aws/validation-cases/case-lib.sh"
  removal_command="${tmp}/tool-removal-command"
  # shellcheck disable=SC2329
  validation_ssm() {
    printf '%s\n' "$2" >"${removal_command}"
  }
  validation_ssm_remove_datapath_tools \
    i-0123456789abcdef0 \
    aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  grep -Fq "/run/helmr/datapath-validation/bin/datapath-host-collector.sh" \
    "${removal_command}"
  grep -Fq "/run/helmr/datapath-validation/bin/datapath-host-trace.py" \
    "${removal_command}"
  grep -Fq "sha256sum" "${removal_command}"
  grep -Fq "test ! -e" "${removal_command}"
) || fail "uploaded datapath tools must be hash-checked, removed, and verified absent"

# shellcheck disable=SC2030,SC2031
(
  export HELMR_VALIDATION_CASE="${case_json}"
  export HELMR_VALIDATION_CASE_RESULT_FILE="${result_file}"
  # shellcheck disable=SC1091
  source "${root}/dev/aws/validation-cases/case-lib.sh"
  cleanup_log="${tmp}/failed-start-cleanup"
  # shellcheck disable=SC2329
  validation_run_helmr() {
    case "$1 $2" in
      "workspace create")
        printf '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}\n'
        ;;
      "task start")
        return 1
        ;;
      *)
        return 1
        ;;
    esac
  }
  # shellcheck disable=SC2329
  validation_probe_run_for_workspace() {
    printf '019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32\n'
  }
  # shellcheck disable=SC2329
  validation_probe_cleanup() {
    printf '%s\t%s\n' "$1" "$2" >"${cleanup_log}"
  }
  if validation_datapath_probe_start failed-start '{}'; then
    exit 1
  fi
  grep -Fqx "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" \
    "${VALIDATION_TMP}/probe-workspace-id"
  grep -Fqx $'019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31\t019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32' \
    "${cleanup_log}"
) || fail "failed Run start must preserve and clean its created Workspace"

# shellcheck disable=SC2030,SC2031
(
  export HELMR_VALIDATION_CASE="${case_json}"
  export HELMR_VALIDATION_CASE_RESULT_FILE="${result_file}"
  # shellcheck disable=SC1091
  source "${root}/dev/aws/validation-cases/case-lib.sh"
  cleanup_steps="${tmp}/strict-cleanup-steps"
  : >"${cleanup_steps}"
  # shellcheck disable=SC2329
  validation_run_helmr() {
    case "$1 $2" in
      "run get")
        printf '{"status":"succeeded"}\n'
        ;;
      "workspace delete")
        printf '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}\n'
        ;;
      *)
        return 1
        ;;
    esac
  }
  # shellcheck disable=SC2329
  validation_wait_run_status() {
    printf 'terminal\n' >>"${cleanup_steps}"
  }
  # shellcheck disable=SC2329
  validation_wait_run_reclaimed() {
    printf 'runtime-reclaimed\n' >>"${cleanup_steps}"
  }
  # shellcheck disable=SC2329
  validation_wait_workspace_delete_requested() {
    printf 'workspace-delete-requested\n' >>"${cleanup_steps}"
  }
  validation_probe_cleanup \
    019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31 \
    019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32
  [ "$(cat "${cleanup_steps}")" = $'terminal\nruntime-reclaimed\nworkspace-delete-requested' ]
) || fail "probe cleanup must wait for terminal, reclaim, and Workspace deletion state"

forbidden_pattern='ga''te.?1|ga''te1'
if rg -n -i "${forbidden_pattern}" \
  "${root}/dev/aws/datapath-validation-profile.json" \
  "${root}/dev/aws/run-validation-campaign.sh" \
  "${root}/dev/aws/validation-cases/case-lib.sh" \
  "${root}/dev/aws/validation-cases/datapath-host-collector.sh" \
  "${root}/dev/aws/validation-cases/datapath-host-trace.py" \
  "${root}/dev/aws/validation-cases/datapath-validation.sh" \
  "${root}/dev/workflows/tasks/smoke/datapath-network-probe.py" \
  "${root}/dev/workflows/tasks/smoke/datapath-network.ts" \
  "${root}/tests/datapath_validation_test.sh" >/dev/null; then
  fail "implementation names must remain generic"
fi

printf 'ok - datapath validation evidence lane\n'
