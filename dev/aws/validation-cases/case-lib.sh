#!/usr/bin/env bash
set -euo pipefail

VALIDATION_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
VALIDATION_CASE_JSON="${HELMR_VALIDATION_CASE:?HELMR_VALIDATION_CASE is required}"
VALIDATION_RESULT_FILE="${HELMR_VALIDATION_CASE_RESULT_FILE:?HELMR_VALIDATION_CASE_RESULT_FILE is required}"
VALIDATION_CASE_ID="$(jq -er '.id' <<<"${VALIDATION_CASE_JSON}")"
VALIDATION_PROJECT="$(jq -er '.payload.project // "helmr"' <<<"${VALIDATION_CASE_JSON}")"
VALIDATION_ENVIRONMENT="$(jq -er '.payload.environment // "staging"' <<<"${VALIDATION_CASE_JSON}")"
VALIDATION_TMP="$(mktemp -d)"
VALIDATION_HELMR=()

if [ -n "${HELMR_BIN:-}" ]; then
  VALIDATION_HELMR=("${HELMR_BIN}")
else
  VALIDATION_HELMR=(go run ./cmd/helmr)
fi

validation_cleanup_tmp() {
  rm -rf "${VALIDATION_TMP}"
}

validation_run_helmr() {
  (
    cd "${VALIDATION_ROOT}"
    HELMR_API_URL="${HELMR_API_URL:-https://dev.helmr.dev}" \
      "${VALIDATION_HELMR[@]}" "$@"
  )
}

validation_checks() {
  local status=$1
  jq -c --arg status "${status}" \
    '[.producer.checks[] | {id:.,status:$status}]' <<<"${VALIDATION_CASE_JSON}"
}

validation_empty_objects() {
  printf '%s\n' \
    '{"run_ids":[],"workspace_ids":[],"deployment_ids":[],"schedule_ids":[],"token_ids":[],"actor_ids":[]}'
}

validation_write_result() {
  local status=$1 reason=$2 objects=${3:-"$(validation_empty_objects)"} observations=${4:-'{}'}
  local reason_json
  if [ "${reason}" = "null" ]; then
    reason_json=null
  else
    reason_json="$(jq -Rn --arg value "${reason}" '$value')"
  fi
  jq -n \
    --arg status "${status}" \
    --argjson reason "${reason_json}" \
    --argjson checks "$(validation_checks "${status}")" \
    --argjson objects "${objects}" \
    --argjson observations "${observations}" \
    '{
      schema:"helmrdotdev.validation-case-source-result.v2",
      status:$status,
      reason:$reason,
      checks:$checks,
      objects:$objects,
      observations:$observations
    }' >"${VALIDATION_RESULT_FILE}.tmp"
  chmod 0600 "${VALIDATION_RESULT_FILE}.tmp"
  mv "${VALIDATION_RESULT_FILE}.tmp" "${VALIDATION_RESULT_FILE}"
}

validation_dry_run() {
  [ "${HELMR_VALIDATION_DRY_RUN:-0}" = "1" ] || return 1
  validation_write_result passed null "$(validation_empty_objects)" \
    '{"dry_run":true,"cleanup_verified":true}'
}

validation_require_resource_id() {
  local value=$1
  [[ "${value}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]
}

validation_wait_run_status() {
  local run_id=$1 accepted=$2 timeout=${3:-900}
  local deadline=$((SECONDS + timeout)) snapshot status
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    snapshot="$(validation_run_helmr run get "${run_id}" \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" --json 2>/dev/null)" || {
      sleep 2
      continue
    }
    status="$(jq -er '.status' <<<"${snapshot}")"
    case ",${accepted}," in
      *",${status},"*) printf '%s\n' "${snapshot}"; return 0 ;;
    esac
    case "${status}" in
      succeeded|failed|cancelled|expired|system-failed) return 1 ;;
    esac
    sleep 2
  done
  return 124
}

validation_db_query() {
  "${VALIDATION_ROOT}/dev/aws/db-query.sh" "$1"
}

validation_db_marker() {
  local sql=$1 marker=$2
  validation_db_query "${sql}" | grep -Fqx "${marker}"
}

validation_tf_output() {
  local name=$1
  "${TF_BIN:-tofu}" -chdir="${DEV_STACK:-${VALIDATION_ROOT}/infra/aws/stacks/dev}" \
    output -raw "${name}"
}

validation_single_asg_instance() {
  local asg=$1
  aws autoscaling describe-auto-scaling-groups \
    --auto-scaling-group-names "${asg}" |
    jq -er '
      .AutoScalingGroups
      | select(length == 1)
      | .[0]
      | select(.MaxSize == 1 and .DesiredCapacity <= 1)
      | [.Instances[] | select(.LifecycleState == "InService") | .InstanceId]
      | select(length == 1)
      | .[0]
    '
}

validation_ssm() {
  local instance_id=$1 command=$2 timeout=${3:-120}
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  local command_id deadline status output
  command_id="$(
    aws ssm send-command \
      --instance-ids "${instance_id}" \
      --document-name AWS-RunShellScript \
      --parameters "$(jq -cn --arg command "${command}" '{commands:[$command]}')" \
      --query Command.CommandId \
      --output text
  )"
  deadline=$((SECONDS + timeout))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    output="$(
      aws ssm get-command-invocation \
        --command-id "${command_id}" \
        --instance-id "${instance_id}" 2>/dev/null
    )" || {
      sleep 2
      continue
    }
    status="$(jq -er '.Status' <<<"${output}")"
    case "${status}" in
      Success) jq -r '.StandardOutputContent' <<<"${output}"; return 0 ;;
      Failed|Cancelled|TimedOut) return 1 ;;
    esac
    sleep 2
  done
  return 124
}

validation_ssm_start() {
  local instance_id=$1 command=$2
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  aws ssm send-command \
    --instance-ids "${instance_id}" \
    --document-name AWS-RunShellScript \
    --parameters "$(jq -cn --arg command "${command}" '{commands:[$command]}')" \
    --query Command.CommandId \
    --output text
}

validation_ssm_wait() {
  local instance_id=$1 command_id=$2 timeout=${3:-120}
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  [[ "${command_id}" =~ ^[0-9a-f-]{36}$ ]] || return 2
  local deadline=$((SECONDS + timeout)) status output
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    output="$(
      aws ssm get-command-invocation \
        --command-id "${command_id}" \
        --instance-id "${instance_id}" 2>/dev/null
    )" || {
      sleep 2
      continue
    }
    status="$(jq -er '.Status' <<<"${output}")"
    case "${status}" in
      Success) printf '%s\n' "${output}"; return 0 ;;
      Failed|Cancelled|TimedOut) printf '%s\n' "${output}"; return 1 ;;
    esac
    sleep 2
  done
  return 124
}

validation_ssm_retrieve_file() {
  local instance_id=$1 remote_path=$2 local_path=$3
  local chunk_bytes=${4:-16384}
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  [[ "${remote_path}" =~ ^/run/helmr/datapath-validation/[a-z0-9][a-z0-9-]{0,62}/[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$ ]] ||
    return 2
  [[ "${chunk_bytes}" =~ ^[0-9]+$ ]] &&
    [ "${chunk_bytes}" -ge 1024 ] && [ "${chunk_bytes}" -le 16384 ] || return 2
  [ ! -e "${local_path}" ] || return 2

  local metadata remote_bytes remote_sha offset chunk output encoded local_tmp
  local_tmp="$(mktemp "${local_path}.tmp.XXXXXX")"
  chmod 0600 "${local_tmp}"
  metadata="$(
    validation_ssm "${instance_id}" \
      "set -eu; test -f ${remote_path}; test ! -L ${remote_path}; bytes=\$(wc -c < ${remote_path}); sha=\$(sha256sum ${remote_path} | awk '{print \$1}'); printf '%s %s\\n' \"\${bytes}\" \"\${sha}\"" \
      120
  )" || {
    rm -f "${local_tmp}"
    return 1
  }
  read -r remote_bytes remote_sha <<<"${metadata}"
  if ! [[ "${remote_bytes}" =~ ^[0-9]+$ ]] ||
    ! [[ "${remote_sha}" =~ ^[0-9a-f]{64}$ ]] ||
    [ "${remote_bytes}" -gt 262144 ]; then
    rm -f "${local_tmp}"
    return 1
  fi

  offset=0
  while [ "${offset}" -lt "${remote_bytes}" ]; do
    chunk="${chunk_bytes}"
    if [ $((remote_bytes - offset)) -lt "${chunk}" ]; then
      chunk=$((remote_bytes - offset))
    fi
    output="$(
      validation_ssm "${instance_id}" \
        "set -eu; dd if=${remote_path} bs=1 skip=${offset} count=${chunk} status=none | base64 | tr -d '\\n'" \
        120
    )" || {
      rm -f "${local_tmp}"
      return 1
    }
    encoded="$(tr -d '\r\n' <<<"${output}")"
    if ! printf '%s' "${encoded}" |
      python3 -c 'import base64,sys; sys.stdout.buffer.write(base64.b64decode(sys.stdin.buffer.read(), validate=True))' \
      >>"${local_tmp}"; then
      rm -f "${local_tmp}"
      return 1
    fi
    offset=$((offset + chunk))
  done
  if [ "$(wc -c <"${local_tmp}" | tr -d ' ')" != "${remote_bytes}" ] ||
    [ "$(validation_sha256_file "${local_tmp}")" != "${remote_sha}" ]; then
    rm -f "${local_tmp}"
    return 1
  fi
  mv "${local_tmp}" "${local_path}"
  printf '%s\t%s\n' "${remote_bytes}" "${remote_sha}"
}

validation_sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

validation_ssm_upload_file() {
  local instance_id=$1 local_path=$2 remote_path=$3 mode=${4:-0600}
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  [ -f "${local_path}" ] && [ ! -L "${local_path}" ] || return 2
  [[ "${remote_path}" =~ ^/run/helmr/datapath-validation/(bin/[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}|[a-z0-9][a-z0-9-]{0,62}/[a-zA-Z0-9][a-zA-Z0-9._-]{0,127})$ ]] ||
    return 2
  [[ "${mode}" =~ ^0(600|700)$ ]] || return 2
  local bytes sha remote_tmp offset chunk encoded
  bytes="$(wc -c <"${local_path}" | tr -d ' ')"
  [ "${bytes}" -le 262144 ] || return 2
  sha="$(validation_sha256_file "${local_path}")"
  remote_tmp="${remote_path}.upload-${sha}"
  validation_ssm "${instance_id}" \
    "set -eu; install -d -o root -g root -m 0700 /run/helmr/datapath-validation /run/helmr/datapath-validation/bin; test ! -L /run/helmr/datapath-validation; test ! -L /run/helmr/datapath-validation/bin; rm -f ${remote_tmp}; umask 077; : > ${remote_tmp}" \
    120 >/dev/null
  offset=0
  while [ "${offset}" -lt "${bytes}" ]; do
    chunk=4096
    if [ $((bytes - offset)) -lt "${chunk}" ]; then
      chunk=$((bytes - offset))
    fi
    encoded="$(dd if="${local_path}" bs=1 skip="${offset}" count="${chunk}" status=none | base64 | tr -d '\r\n')"
    validation_ssm "${instance_id}" \
      "set -eu; printf '%s' '${encoded}' | base64 -d >> ${remote_tmp}" \
      120 >/dev/null || {
      validation_ssm "${instance_id}" "rm -f ${remote_tmp}" 120 >/dev/null 2>&1 || true
      return 1
    }
    offset=$((offset + chunk))
  done
  validation_ssm "${instance_id}" \
    "set -eu; test \"\$(wc -c < ${remote_tmp})\" = '${bytes}'; test \"\$(sha256sum ${remote_tmp} | awk '{print \$1}')\" = '${sha}'; chown root:root ${remote_tmp}; chmod ${mode} ${remote_tmp}; mv -f ${remote_tmp} ${remote_path}; test ! -L ${remote_path}" \
    120 >/dev/null || {
    validation_ssm "${instance_id}" "rm -f ${remote_tmp}" 120 >/dev/null 2>&1 || true
    return 1
  }
  printf '%s\t%s\n' "${bytes}" "${sha}"
}

validation_ssm_remove_datapath_tools() {
  local instance_id=$1 collector_sha=$2 trace_sha=$3
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  [[ "${collector_sha}" =~ ^[0-9a-f]{64}$ ]] || return 2
  [[ "${trace_sha}" =~ ^[0-9a-f]{64}$ ]] || return 2
  validation_ssm "${instance_id}" \
    "set -eu
remove_exact() {
  path=\$1
  expected=\$2
  if [ -e \"\${path}\" ]; then
    test -f \"\${path}\"
    test ! -L \"\${path}\"
    test \"\$(stat -c '%u' \"\${path}\")\" = 0
    test \"\$(sha256sum \"\${path}\" | awk '{print \$1}')\" = \"\${expected}\"
    rm -f \"\${path}\"
  fi
  test ! -e \"\${path}\"
}
remove_exact /run/helmr/datapath-validation/bin/datapath-host-collector.sh ${collector_sha}
remove_exact /run/helmr/datapath-validation/bin/datapath-host-trace.py ${trace_sha}
rmdir /run/helmr/datapath-validation/bin 2>/dev/null || true
rmdir /run/helmr/datapath-validation 2>/dev/null || true" \
    120 >/dev/null
}

validation_ssm_datapath_tools_absent() {
  local instance_id=$1
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  validation_ssm "${instance_id}" \
    "set -eu
test ! -e /run/helmr/datapath-validation/bin/datapath-host-collector.sh
test ! -e /run/helmr/datapath-validation/bin/datapath-host-trace.py" \
    120 >/dev/null
}

validation_exact_run_placements() {
  [ "$#" -ge 1 ] || return 2
  local run_id values="" separator=""
  for run_id in "$@"; do
    validation_require_resource_id "${run_id}" || return 2
    values+="${separator}('${run_id}'::uuid)"
    separator=","
  done
  validation_db_query "
    COPY (
      WITH requested(run_id) AS (VALUES ${values})
      SELECT jsonb_build_object(
        'run_id', requested.run_id,
        'worker_instance_id', worker_instances.resource_id,
        'worker_epoch', run_leases.worker_epoch,
        'runtime_instance_id', run_leases.runtime_instance_id,
        'slot_id', worker_network_slots.id,
        'slot_name', worker_network_slots.slot_name,
        'slot_generation', worker_network_slots.generation,
        'slot_state', worker_network_slots.state,
        'host_interface_name', worker_network_slots.host_interface_name,
        'guest_address', host(worker_network_slots.guest_address),
        'gateway_address', host(worker_network_slots.gateway_address),
        'subnet', worker_network_slots.subnet::text,
        'tap_name', worker_network_slots.tap_name,
        'netns_name', worker_network_slots.netns_name,
        'guest_mac', worker_network_slots.guest_mac::text
      )::text
      FROM requested
      JOIN runs ON runs.id = requested.run_id
      JOIN run_leases
        ON run_leases.id = runs.current_run_lease_id
       AND run_leases.run_id = runs.id
      JOIN worker_instances
        ON worker_instances.id = run_leases.worker_instance_id
       AND worker_instances.current_epoch = run_leases.worker_epoch
      JOIN worker_network_slots
        ON worker_network_slots.id = run_leases.network_slot_id
       AND worker_network_slots.generation = run_leases.network_slot_generation
       AND worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id
      WHERE run_leases.state = 'running'
        AND worker_network_slots.state = 'bound'
      ORDER BY requested.run_id
    ) TO STDOUT;
  "
}

validation_cleanup_ledger_init() {
  VALIDATION_CLEANUP_LEDGER="${VALIDATION_TMP}/cleanup-ledger.json"
  printf '[]\n' >"${VALIDATION_CLEANUP_LEDGER}"
  chmod 0600 "${VALIDATION_CLEANUP_LEDGER}"
}

validation_cleanup_record() {
  local kind=$1 worker=$2 object=$3 state=$4 sequence
  [[ "${kind}" =~ ^[a-z][a-z0-9_-]{0,31}$ ]] || return 2
  [[ "${worker}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  [[ "${object}" =~ ^[a-zA-Z0-9_.:/@+-]{1,128}$ ]] || return 2
  [[ "${state}" =~ ^(created|fenced|removed|verified)$ ]] || return 2
  sequence="$(jq 'length' "${VALIDATION_CLEANUP_LEDGER}")"
  jq \
    --arg kind "${kind}" \
    --arg worker "${worker}" \
    --arg object "${object}" \
    --arg state "${state}" \
    --argjson sequence "${sequence}" \
    '. + [{kind:$kind,worker_instance_id:$worker,object:$object,state:$state,sequence:$sequence}]' \
    "${VALIDATION_CLEANUP_LEDGER}" >"${VALIDATION_CLEANUP_LEDGER}.tmp"
  mv "${VALIDATION_CLEANUP_LEDGER}.tmp" "${VALIDATION_CLEANUP_LEDGER}"
}

validation_cleanup_ledger_proven() {
  jq -e '
    type == "array" and length >= 1 and length <= 256 and
    all(.[]; keys == ["kind","object","sequence","state","worker_instance_id"]) and
    ([.[].sequence] == [range(0; length)]) and
    (sort_by(.kind,.worker_instance_id,.object,.sequence) |
      group_by([.kind,.worker_instance_id,.object]) |
      all(.[];
        length == 4 and
        (sort_by(.sequence) | map(.state)) == ["created","fenced","removed","verified"]
      ))
  ' "${VALIDATION_CLEANUP_LEDGER}" >/dev/null
}

validation_probe_start() {
  local marker=$1 payload=$2
  local workspace_response workspace_id run_response run_id
  workspace_response="$(
    validation_run_helmr workspace create helmr-fault-probe \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --key "${marker}" --idempotency-key "${marker}:workspace" --json
  )"
  workspace_id="$(jq -er '.workspace_id' <<<"${workspace_response}")"
  validation_require_resource_id "${workspace_id}"
  run_response="$(
    validation_run_helmr task start fault-probe \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --workspace "${workspace_id}" \
      --idempotency-key "${marker}:run" \
      --payload-json "${payload}" --json
  )"
  run_id="$(jq -er '.run_id' <<<"${run_response}")"
  validation_require_resource_id "${run_id}"
  printf '%s\t%s\n' "${workspace_id}" "${run_id}"
}

validation_datapath_probe_start() {
  local marker=$1 payload=$2
  local workspace_response workspace_id run_response run_id
  workspace_response="$(
    validation_run_helmr workspace create helmr-datapath-network \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --key "${marker}" --idempotency-key "${marker}:workspace" --json
  )"
  workspace_id="$(jq -er '.workspace_id' <<<"${workspace_response}")"
  validation_require_resource_id "${workspace_id}"
  printf '%s\n' "${workspace_id}" >"${VALIDATION_TMP}/probe-workspace-id"
  chmod 0600 "${VALIDATION_TMP}/probe-workspace-id"
  if ! run_response="$(
    validation_run_helmr task start datapath-network \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --workspace "${workspace_id}" \
      --idempotency-key "${marker}:run" \
      --payload-json "${payload}" --json
  )"; then
    run_id="$(validation_probe_run_for_workspace "${workspace_id}" 2>/dev/null || true)"
    validation_probe_cleanup "${workspace_id}" "${run_id}" >/dev/null 2>&1 || true
    return 1
  fi
  run_id="$(jq -er '.run_id' <<<"${run_response}")" || {
    run_id="$(validation_probe_run_for_workspace "${workspace_id}" 2>/dev/null || true)"
    validation_probe_cleanup "${workspace_id}" "${run_id}" >/dev/null 2>&1 || true
    return 1
  }
  if ! validation_require_resource_id "${run_id}"; then
    run_id="$(validation_probe_run_for_workspace "${workspace_id}" 2>/dev/null || true)"
    validation_probe_cleanup "${workspace_id}" "${run_id}" >/dev/null 2>&1 || true
    return 1
  fi
  printf '%s\n' "${run_id}" >"${VALIDATION_TMP}/probe-run-id"
  chmod 0600 "${VALIDATION_TMP}/probe-run-id"
  printf '%s\t%s\n' "${workspace_id}" "${run_id}"
}

validation_probe_run_for_workspace() {
  local workspace_id=$1 output
  validation_require_resource_id "${workspace_id}" || return 2
  output="$(
    validation_db_query "
      COPY (
        SELECT runs.id::text
          FROM runs
         WHERE runs.workspace_id = '${workspace_id}'::uuid
         ORDER BY runs.created_at DESC
         LIMIT 1
      ) TO STDOUT;
    "
  )" || return 1
  validation_require_resource_id "${output}" || return 1
  printf '%s\n' "${output}"
}

validation_wait_run_reclaimed() {
  local run_id=$1 timeout=${2:-180} deadline
  validation_require_resource_id "${run_id}" || return 2
  deadline=$((SECONDS + timeout))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if validation_db_marker "
      COPY (
        SELECT 'reclaimed'
          FROM runs
          JOIN workspaces ON workspaces.id = runs.workspace_id
         WHERE runs.id = '${run_id}'::uuid
           AND runs.status IN ('succeeded','failed','cancelled','expired','system_failed')
           AND runs.current_run_lease_id IS NULL
           AND workspaces.owner_run_id IS NULL
           AND NOT EXISTS (
                 SELECT 1
                   FROM run_leases
                  WHERE run_leases.run_id = runs.id
                    AND run_leases.state IN ('assigned','starting','running','checkpointing','finalizing')
               )
           AND NOT EXISTS (
                 SELECT 1
                   FROM run_leases
                   JOIN runtime_instances
                     ON runtime_instances.id = run_leases.runtime_instance_id
                  WHERE run_leases.run_id = runs.id
                    AND runtime_instances.observed_state IN ('allocated','preparing','ready','closing')
               )
           AND NOT EXISTS (
                 SELECT 1
                   FROM run_leases
                   JOIN worker_network_slots
                     ON worker_network_slots.id = run_leases.network_slot_id
                    AND worker_network_slots.generation = run_leases.network_slot_generation
                  WHERE run_leases.run_id = runs.id
                    AND (
                      worker_network_slots.state <> 'available'
                      OR worker_network_slots.runtime_instance_id IS NOT NULL
                      OR worker_network_slots.host_interface_name IS NOT NULL
                      OR worker_network_slots.tap_name IS NOT NULL
                      OR worker_network_slots.netns_name IS NOT NULL
                    )
               )
      ) TO STDOUT;
    " reclaimed 2>/dev/null; then
      return 0
    fi
    sleep 2
  done
  return 124
}

validation_wait_workspace_delete_requested() {
  local workspace_id=$1 timeout=${2:-180} deadline
  validation_require_resource_id "${workspace_id}" || return 2
  deadline=$((SECONDS + timeout))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if validation_db_marker "
      COPY (
        SELECT 'delete_requested'
          FROM workspaces
         WHERE workspaces.id = '${workspace_id}'::uuid
           AND workspaces.state IN ('deleting','deleted')
           AND workspaces.desired_state = 'deleted'
           AND workspaces.owner_actor_id IS NULL
           AND workspaces.owner_run_id IS NULL
           AND NOT EXISTS (
                 SELECT 1
                   FROM workspace_leases
                  WHERE workspace_leases.workspace_id = workspaces.id
                    AND workspace_leases.state IN ('active','releasing')
               )
           AND NOT EXISTS (
                 SELECT 1
                   FROM workspace_processes
                  WHERE workspace_processes.workspace_id = workspaces.id
                    AND workspace_processes.state IN ('pending','starting','running','exit_requested')
               )
           AND NOT EXISTS (
                 SELECT 1
                   FROM workspace_mounts
                  WHERE workspace_mounts.workspace_id = workspaces.id
                    AND workspace_mounts.state IN ('mounting','mounted','unmounting')
               )
      ) TO STDOUT;
    " delete_requested 2>/dev/null; then
      return 0
    fi
    sleep 2
  done
  return 124
}

validation_probe_cleanup() {
  local workspace_id=${1:-} run_id=${2:-}
  if validation_require_resource_id "${run_id}"; then
    local snapshot status
    snapshot="$(validation_run_helmr run get "${run_id}" \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" --json)" ||
      return 1
    status="$(jq -er '.status' <<<"${snapshot}")" || return 1
    case "${status}" in
      succeeded|failed|cancelled|expired|system-failed) ;;
      *)
        if ! validation_run_helmr run cancel "${run_id}" \
          --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
          --idempotency-key "${VALIDATION_CASE_ID}:cancel" --json >/dev/null; then
          snapshot="$(validation_run_helmr run get "${run_id}" \
            --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" --json)" ||
            return 1
          status="$(jq -er '.status' <<<"${snapshot}")" || return 1
          case "${status}" in
            succeeded|failed|cancelled|expired|system-failed) ;;
            *) return 1 ;;
          esac
        fi
        ;;
    esac
    validation_wait_run_status \
      "${run_id}" "succeeded,failed,cancelled,expired,system-failed" 180 >/dev/null ||
      return 1
    validation_wait_run_reclaimed "${run_id}" 180 || return 1
  fi
  if validation_require_resource_id "${workspace_id}"; then
    validation_run_helmr workspace delete --id "${workspace_id}" \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --idempotency-key "${VALIDATION_CASE_ID}:delete" --json >/dev/null ||
      return 1
    validation_wait_workspace_delete_requested "${workspace_id}" 180 || return 1
  fi
}
