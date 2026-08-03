#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="$repo_root/scripts/write-aws-release-manifest.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

controlplane_image="ghcr.io/helmrdotdev/helmr-controlplane@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
worker_amis='{"ap-northeast-1":"ami-0fedcba9876543210","us-east-1":"ami-0123456789abcdef0","us-west-2":"ami-00112233445566778"}'
platform_release='{"archive":{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","mediaType":"application/vnd.helmr.platform-release.v0+tar","sizeBytes":4096},"buildPolicyDigest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","formatVersion":0,"sourceCommit":"dddddddddddddddddddddddddddddddddddddddd","sourceRef":"refs/tags/v0.1.0"}'

"$script" "$controlplane_image" "$worker_amis" "$platform_release" "$tmp/aws-artifacts.json"
jq -e \
  --arg image "$controlplane_image" \
  --argjson amis "$worker_amis" \
  --argjson release "$platform_release" \
  'keys == ["controlplane_image", "platform_release", "worker_amis"] and
   .controlplane_image == $image and
   .worker_amis == $amis and
   .platform_release == $release' \
  "$tmp/aws-artifacts.json" >/dev/null

if "$script" \
  "ghcr.io/helmrdotdev/helmr-controlplane:latest" \
  "$worker_amis" \
  "$platform_release" \
  "$tmp/tagged-image.json" 2>/dev/null; then
  echo "tagged Control Plane image was accepted" >&2
  exit 1
fi
if "$script" \
  "$controlplane_image" \
  '{"us-east-1":"ami-0123456789abcdef0"}' \
  "$platform_release" \
  "$tmp/missing-region.json" 2>/dev/null; then
  echo "incomplete Worker AMI set was accepted" >&2
  exit 1
fi
if "$script" \
  "$controlplane_image" \
  "$worker_amis" \
  "${platform_release/sha256:cccc/sha256:CCCC}" \
  "$tmp/invalid-policy.json" 2>/dev/null; then
  echo "invalid Platform release was accepted" >&2
  exit 1
fi

mkdir -p "$tmp/bin"
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
exit "${MOCK_CONTROLPLANE_IMAGE_FAIL:-0}"
EOF
cat >"$tmp/bin/aws" <<'EOF'
#!/usr/bin/env bash
if [ "${MOCK_MISSING_AMI_REGION:-}" = "${4:-}" ]; then
  printf 'None\n'
else
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "--image-ids" ]; then
      printf '%s\n' "$2"
      exit 0
    fi
    shift
  done
fi
EOF
chmod 0755 "$tmp/bin/docker" "$tmp/bin/aws"

VERIFY_RELEASE_ARTIFACTS=1 PATH="$tmp/bin:$PATH" \
  "$script" "$controlplane_image" "$worker_amis" "$platform_release" "$tmp/verified.json"
if VERIFY_RELEASE_ARTIFACTS=1 MOCK_CONTROLPLANE_IMAGE_FAIL=1 PATH="$tmp/bin:$PATH" \
  "$script" "$controlplane_image" "$worker_amis" "$platform_release" "$tmp/bad-image.json" 2>/dev/null; then
  echo "uninspectable Control Plane image was accepted" >&2
  exit 1
fi

printf 'ok - AWS release manifest tests\n'
