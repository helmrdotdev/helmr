#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local expected="$2"
  local label="$3"
  if ! grep -Fq "$expected" "$file"; then
    printf 'calls:\n' >&2
    cat "$file" >&2
    fail "$label: expected '$expected'"
  fi
}

assert_not_contains() {
  local file="$1"
  local unexpected="$2"
  local label="$3"
  if grep -Fq "$unexpected" "$file"; then
    printf 'calls:\n' >&2
    cat "$file" >&2
    fail "$label: did not expect '$unexpected'"
  fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/bin"
cat >"$tmp/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

calls="${MOCK_AWS_CALLS:?}"
region=""
image_id=""
snapshot_id=""
args=()

while [ "$#" -gt 0 ]; do
  case "$1" in
    --region)
      region="$2"
      shift 2
      ;;
    --image-id)
      image_id="$2"
      shift 2
      ;;
    --snapshot-id)
      snapshot_id="$2"
      shift 2
      ;;
    *)
      args+=("$1")
      shift
      ;;
  esac
done

case "${args[*]}" in
  "ec2 describe-images --owners self --filters Name=tag-key,Values=HelmrWorkerImageName --output json")
    cat "${MOCK_AWS_IMAGES:?}"
    ;;
  "ec2 deregister-image")
    printf 'deregister region=%s image=%s\n' "$region" "$image_id" >>"$calls"
    ;;
  "ec2 delete-snapshot")
    printf 'delete-snapshot region=%s snapshot=%s\n' "$region" "$snapshot_id" >>"$calls"
    ;;
  *)
    printf 'unexpected aws call: %s\n' "${args[*]}" >&2
    exit 2
    ;;
esac
EOF
chmod +x "$tmp/bin/aws"

cat >"$tmp/images.json" <<'JSON'
{
  "Images": [
    {
      "ImageId": "ami-oldest",
      "Name": "oldest",
      "CreationDate": "2026-01-01T00:00:00.000Z",
      "Public": true,
      "Tags": [{"Key":"HelmrWorkerImageName","Value":"helmr-release-image-v0-1-1-rc-1"}],
      "BlockDeviceMappings": [{"Ebs":{"SnapshotId":"snap-oldest"}}]
    },
    {
      "ImageId": "ami-second",
      "Name": "second",
      "CreationDate": "2026-01-02T00:00:00.000Z",
      "Public": true,
      "Tags": [{"Key":"HelmrWorkerImageName","Value":"helmr-release-image-v0-1-1-rc-2"}],
      "BlockDeviceMappings": [{"Ebs":{"SnapshotId":"snap-second-a"}},{"Ebs":{"SnapshotId":"snap-second-b"}}]
    },
    {
      "ImageId": "ami-keep-1",
      "Name": "keep-1",
      "CreationDate": "2026-01-03T00:00:00.000Z",
      "Public": true,
      "Tags": [{"Key":"HelmrWorkerImageName","Value":"helmr-release-image-v0-1-1-rc-3"}]
    },
    {
      "ImageId": "ami-keep-2",
      "Name": "keep-2",
      "CreationDate": "2026-01-04T00:00:00.000Z",
      "Public": true,
      "Tags": [{"Key":"HelmrWorkerImageName","Value":"helmr-release-image-v0-1-1-rc-4"}]
    },
    {
      "ImageId": "ami-private",
      "Name": "private",
      "CreationDate": "2026-01-05T00:00:00.000Z",
      "Public": false,
      "Tags": [{"Key":"HelmrWorkerImageName","Value":"helmr-release-image-v0-1-1-rc-private"}]
    },
    {
      "ImageId": "ami-other",
      "Name": "other",
      "CreationDate": "2026-01-06T00:00:00.000Z",
      "Public": true,
      "Tags": [{"Key":"HelmrWorkerImageName","Value":"other-release-image-v0-1-1"}]
    },
    {
      "ImageId": "ami-keep-3",
      "Name": "keep-3",
      "CreationDate": "2026-01-07T00:00:00.000Z",
      "Public": true,
      "Tags": [{"Key":"HelmrWorkerImageName","Value":"helmr-release-image-v0-1-1-rc-5"}]
    }
  ]
}
JSON

touch "$tmp/calls"
MOCK_AWS_CALLS="$tmp/calls" MOCK_AWS_IMAGES="$tmp/images.json" PATH="$tmp/bin:$PATH" \
  "$repo_root/scripts/release-worker-ami-cleanup.sh" helmr-release-image "us-east-1, us-west-2" 3

assert_contains "$tmp/calls" "deregister region=us-east-1 image=ami-oldest" "oldest image in first region should be deleted"
assert_contains "$tmp/calls" "deregister region=us-east-1 image=ami-second" "second-oldest image in first region should be deleted"
assert_contains "$tmp/calls" "delete-snapshot region=us-east-1 snapshot=snap-oldest" "oldest snapshot should be deleted"
assert_contains "$tmp/calls" "delete-snapshot region=us-east-1 snapshot=snap-second-a" "first second-oldest snapshot should be deleted"
assert_contains "$tmp/calls" "delete-snapshot region=us-east-1 snapshot=snap-second-b" "second second-oldest snapshot should be deleted"
assert_contains "$tmp/calls" "deregister region=us-west-2 image=ami-oldest" "oldest image in second region should be deleted"
assert_not_contains "$tmp/calls" "ami-private" "private image should not be deleted"
assert_not_contains "$tmp/calls" "ami-other" "unrelated release image should not be deleted"
assert_not_contains "$tmp/calls" "ami-keep-3" "newest matching image should be kept"

if "$repo_root/scripts/release-worker-ami-cleanup.sh" helmr-release-image us-east-1 invalid 2>/dev/null; then
  fail "invalid keep count should fail"
fi

printf 'ok - release worker AMI cleanup tests\n'
