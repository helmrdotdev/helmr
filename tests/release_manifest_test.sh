#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

"$repo_root/scripts/write-aws-release-manifest.sh" \
  "ghcr.io/helmrdotdev/helmr-control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
  '{"us-east-1":"ami-0123456789abcdef0","us-west-2":"ami-00112233445566778","ap-northeast-1":"ami-0fedcba9876543210"}' \
  "$tmp/aws-artifacts.json"

control_image="$(jq -r '.control_image' "$tmp/aws-artifacts.json")"
[ "$control_image" = "ghcr.io/helmrdotdev/helmr-control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ] || fail "control image mismatch"

worker_ami="$(jq -r '.worker_amis["ap-northeast-1"]' "$tmp/aws-artifacts.json")"
[ "$worker_ami" = "ami-0fedcba9876543210" ] || fail "worker AMI mismatch"

if "$repo_root/scripts/write-aws-release-manifest.sh" "ghcr.io/helmrdotdev/helmr-control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" '[]' "$tmp/invalid.json" 2>/dev/null; then
  fail "array worker AMI JSON should fail"
fi

if "$repo_root/scripts/write-aws-release-manifest.sh" "ghcr.io/helmrdotdev/helmr-control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" '{"us-east-1":"ami-0123456789abcdef0"}' "$tmp/missing-region.json" 2>/dev/null; then
  fail "missing required worker AMI region should fail"
fi

if "$repo_root/scripts/write-aws-release-manifest.sh" "ghcr.io/helmrdotdev/helmr-control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" '{"us-east-1":"not-an-ami","us-west-2":"ami-00112233445566778","ap-northeast-1":"ami-0fedcba9876543210"}' "$tmp/invalid-ami.json" 2>/dev/null; then
  fail "invalid worker AMI ID should fail"
fi

if "$repo_root/scripts/write-aws-release-manifest.sh" "ghcr.io/helmrdotdev/helmr-control:latest" '{"us-east-1":"ami-0123456789abcdef0","us-west-2":"ami-00112233445566778","ap-northeast-1":"ami-0fedcba9876543210"}' "$tmp/tagged-image.json" 2>/dev/null; then
  fail "tagged control image should fail"
fi

mkdir -p "$tmp/bin"

cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [ "${MOCK_CONTROL_IMAGE_FAIL:-0}" = "1" ]; then
  exit 1
fi

case "$*" in
  "buildx imagetools inspect "* | "manifest inspect "*) exit 0 ;;
  *) exit 1 ;;
esac
EOF

cat >"$tmp/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

region=""
image_id=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --region)
      region="$2"
      shift 2
      ;;
    --image-ids)
      image_id="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [ "${MOCK_MISSING_AMI_REGION:-}" = "$region" ]; then
  exit 254
fi

printf '%s\n' "$image_id"
EOF

chmod +x "$tmp/bin/docker" "$tmp/bin/aws"

VERIFY_RELEASE_ARTIFACTS=1 PATH="$tmp/bin:$PATH" "$repo_root/scripts/write-aws-release-manifest.sh" \
  "ghcr.io/helmrdotdev/helmr-control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
  '{"us-east-1":"ami-0123456789abcdef0","us-west-2":"ami-00112233445566778","ap-northeast-1":"ami-0fedcba9876543210"}' \
  "$tmp/verified.json"

if VERIFY_RELEASE_ARTIFACTS=1 MOCK_CONTROL_IMAGE_FAIL=1 PATH="$tmp/bin:$PATH" "$repo_root/scripts/write-aws-release-manifest.sh" "ghcr.io/helmrdotdev/helmr-control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" '{"us-east-1":"ami-0123456789abcdef0","us-west-2":"ami-00112233445566778","ap-northeast-1":"ami-0fedcba9876543210"}' "$tmp/bad-image.json" 2>/dev/null; then
  fail "uninspectable control image should fail verification"
fi
[ ! -e "$tmp/bad-image.json" ] || fail "verification failure should not write manifest"

if VERIFY_RELEASE_ARTIFACTS=1 MOCK_MISSING_AMI_REGION=us-west-2 PATH="$tmp/bin:$PATH" "$repo_root/scripts/write-aws-release-manifest.sh" "ghcr.io/helmrdotdev/helmr-control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" '{"us-east-1":"ami-0123456789abcdef0","us-west-2":"ami-00112233445566778","ap-northeast-1":"ami-0fedcba9876543210"}' "$tmp/bad-ami.json" 2>/dev/null; then
  fail "missing worker AMI should fail verification"
fi
[ ! -e "$tmp/bad-ami.json" ] || fail "AMI verification failure should not write manifest"

printf 'ok - release manifest tests\n'
