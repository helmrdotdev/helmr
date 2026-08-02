#!/usr/bin/env bash
set -euo pipefail

# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/case-lib.sh"

artifact_dir="${HELMR_VALIDATION_CASE_ARTIFACT_DIR:?HELMR_VALIDATION_CASE_ARTIFACT_DIR is required}"
collector_source="${VALIDATION_ROOT}/dev/aws/validation-cases/datapath-host-collector.sh"
trace_source="${VALIDATION_ROOT}/dev/aws/validation-cases/datapath-host-trace.py"
probe_source="${VALIDATION_ROOT}/dev/workflows/tasks/smoke/datapath-network-probe.py"
remote_bin=/run/helmr/datapath-validation/bin

workspace_id=""
run_id=""
instance_id=""
campaign_id=""
remote_prepared=0
remote_tools_present=0
cleanup_complete=0
collector_sha="$(validation_sha256_file "${collector_source}")"
trace_sha="$(validation_sha256_file "${trace_source}")"

best_effort_cleanup() {
  local status=$?
  set +e
  if [ -z "${workspace_id}" ] && [ -f "${VALIDATION_TMP}/probe-workspace-id" ]; then
    workspace_id="$(cat "${VALIDATION_TMP}/probe-workspace-id")"
  fi
  if [ -z "${run_id}" ] && [ -f "${VALIDATION_TMP}/probe-run-id" ]; then
    run_id="$(cat "${VALIDATION_TMP}/probe-run-id")"
  fi
  if [ -z "${run_id}" ] && validation_require_resource_id "${workspace_id}"; then
    run_id="$(validation_probe_run_for_workspace "${workspace_id}" 2>/dev/null || true)"
  fi
  if [ "${remote_prepared}" = 1 ] && [ -n "${instance_id}" ] && [ -n "${campaign_id}" ]; then
    validation_ssm "${instance_id}" \
      "${remote_bin}/datapath-host-collector.sh cleanup ${campaign_id}" 120 >/dev/null 2>&1 || true
  fi
  if [ "${remote_tools_present}" = 1 ] && [ -n "${instance_id}" ]; then
    validation_ssm_remove_datapath_tools \
      "${instance_id}" "${collector_sha}" "${trace_sha}" >/dev/null 2>&1 || true
  fi
  validation_probe_cleanup "${workspace_id}" "${run_id}" >/dev/null 2>&1 || true
  validation_cleanup_tmp
  if [ "${cleanup_complete}" != 1 ] && [ "${status}" -eq 0 ]; then
    return 1
  fi
  return "${status}"
}
trap best_effort_cleanup EXIT INT TERM

fail_case() {
  local reason=$1
  validation_write_result failed "${reason}"
  exit 1
}

content_address_artifact() {
  local kind=$1 source=$2 sha destination
  sha="$(validation_sha256_file "${source}")"
  destination="${artifact_dir}/${kind}-${sha}.json"
  [ ! -e "${destination}" ] || fail_case duplicate_artifact
  mv "${source}" "${destination}"
  printf '%s\n' "${sha}"
}

require_empty_artifact_dir() {
  [ -d "${artifact_dir}" ] && [ ! -L "${artifact_dir}" ] ||
    fail_case invalid_artifact_directory
  [ -z "$(find "${artifact_dir}" -mindepth 1 -maxdepth 1 -print -quit)" ] ||
    fail_case artifact_directory_not_empty
}

wait_for_placement() {
  local output
  for _ in $(seq 1 60); do
    output="$(validation_exact_run_placements "${run_id}" 2>/dev/null || true)"
    if jq -e -s 'length == 1' <<<"${output}" >/dev/null 2>&1; then
      jq -c -s '.[0]' <<<"${output}"
      return 0
    fi
    sleep 2
  done
  return 1
}

denied_counter() {
  local netns=$1
  validation_ssm "${instance_id}" \
    "ip netns exec ${netns} nft list counter inet helmr_network_policy run_denied | awk '/counter run_denied/ {for (i=1;i<=NF;i++) if (\$i==\"packets\") {print \$(i+1); exit}}'" \
    120
}

snapshot_rules() {
  local netns=$1 tap=$2 peer=$3 output=$4 denied_before=$5 denied_after=$6
  local nft_json tap_qdisc tap_filters peer_qdisc peer_filters
  nft_json="$(validation_ssm "${instance_id}" \
    "ip netns exec ${netns} nft -j list table inet helmr_network_policy" 120)"
  tap_qdisc="$(validation_ssm "${instance_id}" \
    "ip netns exec ${netns} tc -j -s qdisc show dev ${tap}" 120)"
  tap_filters="$(validation_ssm "${instance_id}" \
    "ip netns exec ${netns} tc -j -s filter show dev ${tap} ingress" 120)"
  peer_qdisc="$(validation_ssm "${instance_id}" \
    "ip netns exec ${netns} tc -j -s qdisc show dev ${peer}" 120)"
  peer_filters="$(validation_ssm "${instance_id}" \
    "ip netns exec ${netns} tc -j -s filter show dev ${peer} ingress" 120)"
  jq -n \
    --argjson nft "${nft_json}" \
    --argjson tap_qdisc "${tap_qdisc}" \
    --argjson tap_filters "${tap_filters}" \
    --argjson peer_qdisc "${peer_qdisc}" \
    --argjson peer_filters "${peer_filters}" \
    --argjson denied_before "${denied_before}" \
    --argjson denied_after "${denied_after}" \
    '{
      schema:"helmrdotdev.datapath-rules-evidence.v0",
      selected_hook:"inet-forward",
      forward_denied_counter:{before:$denied_before,after:$denied_after},
      nftables:$nft,
      traffic_control:{
        tap:{qdisc:$tap_qdisc,filters:$tap_filters},
        namespace_veth:{qdisc:$peer_qdisc,filters:$peer_filters}
      }
    }' >"${output}"
}

write_topology_artifact() {
  local output=$1 instance vpc_id vpc cidr resolver freshness
  instance="$(
    aws ec2 describe-instances \
      --region "${AWS_REGION:-us-east-1}" \
      --instance-ids "${instance_id}" \
      --output json
  )"
  vpc_id="$(jq -er '.Reservations[0].Instances[0].VpcId' <<<"${instance}")"
  vpc="$(
    aws ec2 describe-vpcs \
      --region "${AWS_REGION:-us-east-1}" \
      --vpc-ids "${vpc_id}" \
      --output json
  )"
  cidr="$(jq -er 'first(.Vpcs[0].CidrBlockAssociationSet[] | select(.CidrBlockState.State == "associated") | .CidrBlock)' <<<"${vpc}")"
  resolver="$(
    python3 - "${cidr}" <<'PY'
import ipaddress
import sys
network = ipaddress.ip_network(sys.argv[1], strict=False)
print(network.network_address + 2)
PY
  )"
  freshness="$(
    python3 - <<'PY'
import datetime
import json
now = datetime.datetime.now(datetime.timezone.utc)
print(json.dumps({
    "observed_at": now.isoformat().replace("+00:00", "Z"),
    "not_after": (now + datetime.timedelta(minutes=15)).isoformat().replace("+00:00", "Z"),
}, separators=(",", ":"), sort_keys=True))
PY
  )"
  jq -n \
    --arg account_id "$(aws sts get-caller-identity --query Account --output text)" \
    --arg region "${AWS_REGION:-us-east-1}" \
    --arg instance_id "${instance_id}" \
    --arg eni_id "$(jq -er '.Reservations[0].Instances[0].NetworkInterfaces[] | select(.Attachment.DeviceIndex == 0) | .NetworkInterfaceId' <<<"${instance}")" \
    --arg vpc_id "${vpc_id}" \
    --argjson vpc_cidrs "$(jq -c '[.Vpcs[0].CidrBlockAssociationSet[] | select(.CidrBlockState.State == "associated") | .CidrBlock] | sort' <<<"${vpc}")" \
    --argjson worker_addresses "$(jq -c '[.Reservations[0].Instances[0].NetworkInterfaces[].PrivateIpAddresses[].PrivateIpAddress] | unique | sort' <<<"${instance}")" \
    --arg resolver "${resolver}" \
    --argjson freshness "${freshness}" \
    '{
      schema:"helmrdotdev.datapath-topology-evidence.v0",
      topology_facts:{
        schema:"helmrdotdev.datapath-topology-facts.candidate.v0",
        subject:{
          provider:"aws",
          account_id:$account_id,
          region:$region,
          worker_instance_id:$instance_id,
          primary_eni_id:$eni_id,
          vpc_id:$vpc_id
        },
        facts:{
          vpc_ipv4_cidrs:$vpc_cidrs,
          worker_ipv4_addresses:$worker_addresses,
          dns_resolver_ipv4:$resolver
        },
        freshness:$freshness
      }
    }' >"${output}"
}

main() {
  if validation_dry_run; then
    cleanup_complete=1
    return 0
  fi
  require_empty_artifact_dir
  for source in "${collector_source}" "${trace_source}" "${probe_source}"; do
    [ -f "${source}" ] && [ ! -L "${source}" ] || fail_case missing_validation_component
  done

  campaign_id="datapath-${HELMR_VALIDATION_CASE_ATTEMPT:-1}-$(printf '%s' "${VALIDATION_CASE_ID}" | tr '_' '-')"
  [[ "${campaign_id}" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || fail_case invalid_campaign_id
  local payload placement netns tap peer links netns_inode tap_ifindex peer_ifindex flow
  local baseline after terminal packet_remote packet_tmp rules_tmp binding_tmp topology_tmp cleanup_tmp
  local packet_sha rules_sha binding_sha topology_sha cleanup_sha manifest_tmp manifest_sha
  local trace_events artifact_bytes objects observations probe_sha hook_sha verdict_sha
  local created_workspace_id created_run_id

  payload="$(
    jq -cn --arg campaign_id "${campaign_id}" --arg case_id "${VALIDATION_CASE_ID}" '{
      campaignId:$campaign_id,
      caseId:$case_id,
      mode:"tcp",
      target:"example.com",
      port:443,
      attempts:1,
      startDelayMs:90000,
      timeoutMs:10000
    }'
  )"
  IFS=$'\t' read -r workspace_id run_id < <(
    validation_datapath_probe_start "${campaign_id}" "${payload}"
  )
  created_workspace_id="${workspace_id}"
  created_run_id="${run_id}"
  placement="$(wait_for_placement)" || fail_case exact_runtime_not_found
  instance_id="$(jq -er '.worker_instance_id' <<<"${placement}")"
  netns="$(jq -er '.netns_name' <<<"${placement}")"
  tap="$(jq -er '.tap_name' <<<"${placement}")"
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || fail_case invalid_worker_instance
  [[ "${netns}" =~ ^[0-9a-f-]{36}$ ]] || fail_case invalid_runtime_identity
  [[ "${tap}" =~ ^[a-zA-Z0-9_.-]{1,15}$ ]] || fail_case invalid_tap_identity

  remote_tools_present=1
  validation_ssm_upload_file "${instance_id}" "${collector_source}" \
    "${remote_bin}/datapath-host-collector.sh" 0700 >/dev/null ||
    fail_case collector_upload_failed
  validation_ssm_upload_file "${instance_id}" "${trace_source}" \
    "${remote_bin}/datapath-host-trace.py" 0700 >/dev/null ||
    fail_case trace_upload_failed
  validation_cleanup_ledger_init
  validation_cleanup_record tool_bundle "${instance_id}" "${campaign_id}" created
  validation_cleanup_record probe_runtime "${instance_id}" "${run_id}" created
  validation_ssm "${instance_id}" \
    "${remote_bin}/datapath-host-collector.sh prepare ${campaign_id}" 120 >/dev/null ||
    fail_case collector_prepare_failed
  remote_prepared=1
  validation_cleanup_record collector "${instance_id}" "${campaign_id}" created

  links="$(validation_ssm "${instance_id}" "ip netns exec ${netns} ip -j link show" 120)"
  peer="$(jq -er --arg tap "${tap}" '[.[] | select(.ifname != "lo" and .ifname != $tap) | .ifname] | select(length == 1) | .[0]' <<<"${links}")" ||
    fail_case namespace_veth_not_unique
  [[ "${peer}" =~ ^[a-zA-Z0-9_.-]{1,15}$ ]] || fail_case invalid_namespace_veth
  tap_ifindex="$(jq -er --arg tap "${tap}" '.[] | select(.ifname == $tap) | .ifindex' <<<"${links}")"
  peer_ifindex="$(jq -er --arg peer "${peer}" '.[] | select(.ifname == $peer) | .ifindex' <<<"${links}")"
  netns_inode="$(validation_ssm "${instance_id}" "stat -Lc '%i' /var/run/netns/${netns}" 120)"
  [[ "${netns_inode}" =~ ^[0-9]+$ ]] || fail_case invalid_netns_inode

  baseline="$(denied_counter "${netns}")"
  [[ "${baseline}" =~ ^[0-9]+$ ]] || fail_case denied_counter_unavailable
  validation_ssm "${instance_id}" \
    "${remote_bin}/datapath-host-collector.sh start ${campaign_id} public-tcp ${netns} 180 ${tap} ${peer}" \
    120 >/dev/null || fail_case collector_start_failed

  terminal="$(validation_wait_run_status "${run_id}" succeeded 180)" ||
    fail_case public_probe_failed
  [ "$(jq -r '.status' <<<"${terminal}")" = succeeded ] || fail_case public_probe_failed
  flow="$(jq -cer '
    .output.probe as $probe |
    select($probe.schema == "helmrdotdev.datapath-probe-result.v0") |
    select($probe.mode == "tcp" and ($probe.attempts | length) == 1) |
    $probe.attempts[0] |
    select(.outcome == "observed" and .errno == null) |
    .flow |
    select(
      .protocol == "tcp" and
      (.sourceAddress | test("^[0-9]+([.][0-9]+){3}$")) and
      (.destinationAddress | test("^[0-9]+([.][0-9]+){3}$")) and
      (.sourcePort | type == "number" and floor == . and . >= 1 and . <= 65535) and
      (.destinationPort | type == "number" and floor == . and . >= 1 and . <= 65535)
    )
  ' <<<"${terminal}")" || fail_case public_probe_flow_missing
  validation_cleanup_record collector "${instance_id}" "${campaign_id}" fenced
  validation_ssm "${instance_id}" \
    "${remote_bin}/datapath-host-collector.sh stop ${campaign_id} public-tcp" \
    120 >/dev/null || fail_case collector_stop_failed
  after="$(denied_counter "${netns}")"
  [[ "${after}" =~ ^[0-9]+$ ]] || fail_case denied_counter_unavailable
  validation_ssm "${instance_id}" \
    "${remote_bin}/datapath-host-collector.sh export ${campaign_id} public-tcp >/dev/null" \
    120 >/dev/null || fail_case packet_export_failed

  packet_remote="/run/helmr/datapath-validation/${campaign_id}/public-tcp.packet.json"
  packet_tmp="${artifact_dir}/packet.json"
  validation_ssm_retrieve_file "${instance_id}" "${packet_remote}" "${packet_tmp}" 8192 >/dev/null ||
    fail_case packet_retrieval_failed
  trace_events="$(jq -er '.events | length' "${packet_tmp}")"
  [ "${trace_events}" -gt 0 ] && [ "${trace_events}" -le 256 ] ||
    fail_case packet_trace_empty
  jq -e \
    --arg interface "${tap}" \
    --arg source "$(jq -r '.sourceAddress' <<<"${flow}")" \
    --arg destination "$(jq -r '.destinationAddress' <<<"${flow}")" \
    --argjson source_port "$(jq -r '.sourcePort' <<<"${flow}")" \
    --argjson destination_port "$(jq -r '.destinationPort' <<<"${flow}")" '
    any(.events[];
      .interface == $interface and
      .source_ip == $source and .destination_ip == $destination and
      .source_port == $source_port and .destination_port == $destination_port
    )
  ' "${packet_tmp}" >/dev/null ||
    fail_case tap_path_not_observed
  jq -e \
    --arg interface "${peer}" \
    --arg source "$(jq -r '.sourceAddress' <<<"${flow}")" \
    --arg destination "$(jq -r '.destinationAddress' <<<"${flow}")" \
    --argjson source_port "$(jq -r '.sourcePort' <<<"${flow}")" \
    --argjson destination_port "$(jq -r '.destinationPort' <<<"${flow}")" '
    any(.events[];
      .interface == $interface and
      .source_ip == $source and .destination_ip == $destination and
      .source_port == $source_port and .destination_port == $destination_port
    )
  ' "${packet_tmp}" >/dev/null ||
    fail_case namespace_veth_path_not_observed

  rules_tmp="${artifact_dir}/rules.json"
  snapshot_rules "${netns}" "${tap}" "${peer}" "${rules_tmp}" "${baseline}" "${after}"
  binding_tmp="${artifact_dir}/binding.json"
  jq -n \
    --argjson placement "${placement}" \
    --argjson links "${links}" \
    --argjson netns_inode "${netns_inode}" \
    --argjson tap_ifindex "${tap_ifindex}" \
    --argjson peer_ifindex "${peer_ifindex}" \
    --argjson probe_flow "${flow}" \
    --arg peer "${peer}" \
    '{
      schema:"helmrdotdev.datapath-binding-evidence.v0",
      placement:$placement,
      local_binding_facts:{
        netns_inode:$netns_inode,
        tap_ifindex:$tap_ifindex,
        namespace_veth_ifindex:$peer_ifindex,
        namespace_veth_name:$peer
      },
      probe_flow:$probe_flow,
      observed_links:$links
    }' >"${binding_tmp}"
  topology_tmp="${artifact_dir}/topology.json"
  write_topology_artifact "${topology_tmp}"

  [ "$(validation_ssm "${instance_id}" \
    "ip netns exec ${netns} nft list table inet helmr_network_policy >/dev/null && printf present" 120)" = present ] ||
    fail_case network_policy_missing
  validation_ssm "${instance_id}" \
    "${remote_bin}/datapath-host-collector.sh cleanup ${campaign_id}" 120 >/dev/null ||
    fail_case collector_cleanup_failed
  validation_cleanup_record collector "${instance_id}" "${campaign_id}" removed
  validation_ssm "${instance_id}" \
    "${remote_bin}/datapath-host-collector.sh verify-clean ${campaign_id}" 120 >/dev/null ||
    fail_case collector_cleanup_unverified
  validation_cleanup_record collector "${instance_id}" "${campaign_id}" verified
  remote_prepared=0
  validation_cleanup_record tool_bundle "${instance_id}" "${campaign_id}" fenced
  validation_ssm_remove_datapath_tools \
    "${instance_id}" "${collector_sha}" "${trace_sha}" ||
    fail_case tool_cleanup_failed
  validation_cleanup_record tool_bundle "${instance_id}" "${campaign_id}" removed
  validation_ssm_datapath_tools_absent "${instance_id}" ||
    fail_case tool_cleanup_unverified
  validation_cleanup_record tool_bundle "${instance_id}" "${campaign_id}" verified
  remote_tools_present=0
  validation_cleanup_record probe_runtime "${instance_id}" "${run_id}" fenced
  validation_probe_cleanup "${workspace_id}" "${run_id}" || fail_case probe_cleanup_failed
  validation_cleanup_record probe_runtime "${instance_id}" "${run_id}" removed
  validation_wait_run_reclaimed "${run_id}" 30 ||
    fail_case probe_cleanup_unverified
  validation_wait_workspace_delete_requested "${workspace_id}" 30 ||
    fail_case probe_cleanup_unverified
  validation_cleanup_record probe_runtime "${instance_id}" "${run_id}" verified
  validation_cleanup_ledger_proven || fail_case cleanup_ledger_unproven
  workspace_id=""
  run_id=""

  cleanup_tmp="${artifact_dir}/cleanup.json"
  jq -n \
    --argjson ledger "$(cat "${VALIDATION_CLEANUP_LEDGER}")" \
    '{
      schema:"helmrdotdev.datapath-cleanup-evidence.v0",
      cleanup_verified:true,
      candidate_objects_absent:true,
      network_policy_present:true,
      ledger:$ledger
    }' >"${cleanup_tmp}"

  packet_sha="$(content_address_artifact packet "${packet_tmp}")"
  rules_sha="$(content_address_artifact rules "${rules_tmp}")"
  binding_sha="$(content_address_artifact binding "${binding_tmp}")"
  topology_sha="$(content_address_artifact topology "${topology_tmp}")"
  cleanup_sha="$(content_address_artifact cleanup "${cleanup_tmp}")"
  probe_sha="$(validation_sha256_file "${probe_source}")"
  hook_sha="${rules_sha}"
  verdict_sha="$(
    printf '%s\n' 'tap-path-observed,namespace-veth-path-observed,forward-hook-correlation-measured,cleanup-proven' |
      validation_sha256_file /dev/stdin
  )"
  manifest_tmp="${artifact_dir}/manifest.json"
  jq -n \
    --arg source_commit "$(git -C "${VALIDATION_ROOT}" rev-parse HEAD)" \
    --arg worker_provenance "$(jq -er '.artifacts.worker_image_provenance_sha256' "${HELMR_VALIDATION_MANIFEST:?HELMR_VALIDATION_MANIFEST is required}")" \
    --arg collector_sha "${collector_sha}" \
    --arg probe_sha "${probe_sha}" \
    --arg hook_sha "${hook_sha}" \
    --arg verdict_sha "${verdict_sha}" \
    --arg packet "${packet_sha}" \
    --arg rules "${rules_sha}" \
    --arg binding "${binding_sha}" \
    --arg topology "${topology_sha}" \
    --arg cleanup "${cleanup_sha}" \
    '{
      schema:"helmrdotdev.datapath-evidence.v0",
      source_commit:$source_commit,
      worker_image_provenance_sha256:$worker_provenance,
      collector_sha256:$collector_sha,
      probe_sha256:$probe_sha,
      candidate_datapath_abi:"unselected",
      candidate_hook_set_sha256:$hook_sha,
      case_verdict_sha256:$verdict_sha,
      artifacts:{
        packet:$packet,
        rules:$rules,
        binding:$binding,
        topology:$topology,
        cleanup:$cleanup
      },
      truncated:false
    }' >"${manifest_tmp}"
  manifest_sha="$(validation_sha256_file "${manifest_tmp}")"
  mv "${manifest_tmp}" "${artifact_dir}/manifest-${manifest_sha}.json"
  artifact_bytes="$(find "${artifact_dir}" -maxdepth 1 -type f -exec wc -c {} + | awk 'END {print $1 + 0}')"
  [ "${artifact_bytes}" -le 262144 ] || fail_case artifact_limit_exceeded

  objects="$(
    jq -n --arg run "${created_run_id}" --arg workspace "${created_workspace_id}" '{
      run_ids:[$run],
      workspace_ids:[$workspace],
      deployment_ids:[],
      schedule_ids:[],
      token_ids:[],
      actor_ids:[]
    }'
  )"
  observations="$(
    jq -n \
      --argjson artifact_bytes "${artifact_bytes}" \
      --arg manifest_sha "${manifest_sha}" \
      --argjson trace_events "${trace_events}" \
      '{
        artifact_bytes:$artifact_bytes,
        artifact_count:6,
        artifact_manifest_sha256:$manifest_sha,
        cleanup_verified:true,
        trace_event_count:$trace_events,
        truncated:false
      }'
  )"
  cleanup_complete=1
  validation_write_result passed null "${objects}" "${observations}"
}

main
