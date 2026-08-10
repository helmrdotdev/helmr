#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="$repo_root/scripts/write-aws-release-manifest.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

controlplane_image="ghcr.io/helmrdotdev/helmr-controlplane@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
worker_amis='{"ap-northeast-1":"ami-0fedcba9876543210","us-east-1":"ami-0123456789abcdef0","us-west-2":"ami-00112233445566778"}'
platform_release='{"archive":{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","mediaType":"application/vnd.helmr.platform-release.v0+tar","sizeBytes":4096},"buildPolicyDigest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","formatVersion":0,"sourceCommit":"dddddddddddddddddddddddddddddddddddddddd","sourceRef":"refs/tags/v0.1.0"}'
worker_image_provenance='{"ami":{"id":"ami-0123456789abcdef0","region":"us-east-1"},"formatVersion":1,"hostArtifactsBundleDigest":"sha256:7777777777777777777777777777777777777777777777777777777777777777","hostArtifactsManifestDigest":"sha256:8888888888888888888888888888888888888888888888888888888888888888","imageBuildVersionARN":"arn:aws:imagebuilder:us-east-1:123456789012:image/test/1.0.0/1","imageRecipeARN":"arn:aws:imagebuilder:us-east-1:123456789012:image-recipe/test/1.0.0","runtimeArtifactsBundleDigest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","runtimeArtifactsManifestDigest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","runtimeProfile":{"arch":"x86_64","contract":"helmr.vm-runtime.v0","id":"sha256:3333333333333333333333333333333333333333333333333333333333333333","initramfs_digest":"sha256:4444444444444444444444444444444444444444444444444444444444444444","kernel_digest":"sha256:5555555555555555555555555555555555555555555555555555555555555555","rootfs_digest":"sha256:6666666666666666666666666666666666666666666666666666666666666666"},"sourceCommit":"dddddddddddddddddddddddddddddddddddddddd","workerVersion":"dddddddddddddddddddddddddddddddddddddddd"}'

"$script" "$controlplane_image" "$worker_amis" "$worker_image_provenance" "$platform_release" "$tmp/aws-artifacts.json"
jq -e \
  --arg image "$controlplane_image" \
  --argjson amis "$worker_amis" \
  --argjson worker_provenance "$worker_image_provenance" \
  --argjson release "$platform_release" \
  'keys == ["controlplane_image", "format_version", "platform_release", "worker_amis", "worker_image_provenance"] and
   .format_version == 1 and
   .controlplane_image == $image and
   .worker_amis == $amis and
   .worker_image_provenance == $worker_provenance and
   .platform_release == $release' \
  "$tmp/aws-artifacts.json" >/dev/null

if "$script" \
  "ghcr.io/helmrdotdev/helmr-controlplane:latest" \
  "$worker_amis" \
  "$worker_image_provenance" \
  "$platform_release" \
  "$tmp/tagged-image.json" 2>/dev/null; then
  echo "tagged Control Plane image was accepted" >&2
  exit 1
fi
if "$script" \
  "$controlplane_image" \
  '{"us-east-1":"ami-0123456789abcdef0"}' \
  "$worker_image_provenance" \
  "$platform_release" \
  "$tmp/missing-region.json" 2>/dev/null; then
  echo "incomplete Worker AMI set was accepted" >&2
  exit 1
fi
if "$script" \
  "$controlplane_image" \
  "$worker_amis" \
  "$worker_image_provenance" \
  "${platform_release/sha256:cccc/sha256:CCCC}" \
  "$tmp/invalid-policy.json" 2>/dev/null; then
  echo "invalid Platform release was accepted" >&2
  exit 1
fi
if "$script" \
  "$controlplane_image" \
  "$worker_amis" \
  "${worker_image_provenance/dddddddddddddddddddddddddddddddddddddddd/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee}" \
  "$platform_release" \
  "$tmp/drifted-worker.json" 2>/dev/null; then
  echo "Worker provenance from a different source commit was accepted" >&2
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
  "$script" "$controlplane_image" "$worker_amis" "$worker_image_provenance" "$platform_release" "$tmp/verified.json"
if VERIFY_RELEASE_ARTIFACTS=1 MOCK_CONTROLPLANE_IMAGE_FAIL=1 PATH="$tmp/bin:$PATH" \
  "$script" "$controlplane_image" "$worker_amis" "$worker_image_provenance" "$platform_release" "$tmp/bad-image.json" 2>/dev/null; then
  echo "uninspectable Control Plane image was accepted" >&2
  exit 1
fi

printf 'ok - AWS release manifest tests\n'
