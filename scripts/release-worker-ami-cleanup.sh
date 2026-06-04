#!/usr/bin/env bash
set -euo pipefail

base_name="${1:-${WORKER_IMAGE_NAME_BASE:-helmr-release-image}}"
regions_arg="${2:-${WORKER_AMI_REGIONS:-${WORKER_IMAGE_DISTRIBUTION_REGIONS:-${AWS_REGION:-us-east-1}}}}"
keep="${3:-${RELEASE_WORKER_AMI_KEEP:-${WORKER_IMAGE_CLEANUP_KEEP:-4}}}"

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

info() {
  printf '==> %s\n' "$*" >&2
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

slugify() {
  printf '%s' "$1" |
    tr '[:upper:]' '[:lower:]' |
    sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
}

trim() {
  printf '%s' "$1" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//'
}

need_command aws
need_command jq

[ -n "${base_name}" ] || die "base worker image name is required"
[ -n "${regions_arg}" ] || die "at least one AWS region is required"
case "${keep}" in
  ''|*[!0-9]*) die "keep count must be a non-negative integer" ;;
esac

name_prefix="$(slugify "${base_name}")"
[ -n "${name_prefix}" ] || die "failed to derive worker image prefix from ${base_name}"

IFS=',' read -r -a raw_regions <<<"${regions_arg}"
regions=()
for raw_region in "${raw_regions[@]}"; do
  region="$(trim "${raw_region}")"
  [ -n "${region}" ] || continue
  regions+=("${region}")
done
[ "${#regions[@]}" -gt 0 ] || die "at least one non-empty AWS region is required"

for region in "${regions[@]}"; do
  images="$(
    aws ec2 describe-images \
      --region "${region}" \
      --owners self \
      --filters "Name=tag-key,Values=HelmrWorkerImageName" \
      --output json
  )"

  candidates="$(
    printf '%s\n' "${images}" |
      jq -c --arg prefix "${name_prefix}" '
        [
          .Images[]?
          | select((.Public // false) == true)
          | . as $image
          | (($image.Tags // [])
              | map(select(.Key == "HelmrWorkerImageName"))
              | .[0].Value // "") as $worker_name
          | select($worker_name == $prefix or ($worker_name | startswith($prefix + "-")))
          | {
              image_id: .ImageId,
              name: (.Name // ""),
              created: (.CreationDate // ""),
              snapshots: [.BlockDeviceMappings[]?.Ebs.SnapshotId // empty]
            }
        ]
        | sort_by(.created)
      '
  )"

  count="$(printf '%s\n' "${candidates}" | jq 'length')"
  if [ "${count}" -le "${keep}" ]; then
    info "release worker AMI cleanup ${region}: ${count} public image(s), keeping ${keep}; nothing to delete"
    continue
  fi

  delete_count=$((count - keep))
  info "release worker AMI cleanup ${region}: deleting ${delete_count} oldest public image(s), keeping ${keep}"

  printf '%s\n' "${candidates}" | jq -c ".[:${delete_count}][]" | while read -r image; do
    image_id="$(printf '%s\n' "${image}" | jq -r '.image_id')"
    image_name="$(printf '%s\n' "${image}" | jq -r '.name')"
    snapshot_ids=()
    while IFS= read -r snapshot_id; do
      snapshot_ids+=("${snapshot_id}")
    done < <(printf '%s\n' "${image}" | jq -r '.snapshots[]?')

    if [ "${WORKER_IMAGE_CLEANUP_DRY_RUN:-0}" = "1" ]; then
      printf 'would_delete_region=%s image=%s name=%s snapshots=%s\n' "${region}" "${image_id}" "${image_name}" "${snapshot_ids[*]}"
      continue
    fi

    aws ec2 deregister-image \
      --region "${region}" \
      --image-id "${image_id}" >/dev/null
    for snapshot_id in "${snapshot_ids[@]}"; do
      aws ec2 delete-snapshot \
        --region "${region}" \
        --snapshot-id "${snapshot_id}" >/dev/null || true
    done
    info "deleted release worker AMI ${region} ${image_id} (${image_name})"
  done
done
