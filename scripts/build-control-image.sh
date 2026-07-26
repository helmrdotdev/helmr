#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
image_uri="${1:-}"
runtime_release_dir="${CONTROL_IMAGE_RUNTIME_RELEASE_DIR:-}"
manager_release_dir="${CONTROL_IMAGE_MANAGER_RELEASE_DIR:-}"

if [ -z "$image_uri" ]; then
  echo "usage: scripts/build-control-image.sh <image-uri>" >&2
  exit 1
fi
if [ -z "$runtime_release_dir" ]; then
  echo "CONTROL_IMAGE_RUNTIME_RELEASE_DIR is required" >&2
  exit 1
fi
if [ -z "$manager_release_dir" ]; then
  echo "CONTROL_IMAGE_MANAGER_RELEASE_DIR is required" >&2
  exit 1
fi
runtime_release_dir=$(cd -- "$runtime_release_dir" 2>/dev/null && pwd -P) || {
  echo "CONTROL_IMAGE_RUNTIME_RELEASE_DIR must be an existing directory" >&2
  exit 1
}
manager_release_dir=$(cd -- "$manager_release_dir" 2>/dev/null && pwd -P) || {
  echo "CONTROL_IMAGE_MANAGER_RELEASE_DIR must be an existing directory" >&2
  exit 1
}

platform="${CONTROL_IMAGE_PLATFORM:-linux/amd64}"
os="${platform%%/*}"
arch="${platform#*/}"
arch="${arch%%/*}"
context="${CONTROL_IMAGE_CONTEXT:-$repo_root/dist/control-image}"
control_binary="$context/helmr-control"
dispatcher_binary="$context/helmr-dispatcher"
build_version="${HELMR_BUILD_VERSION:-}"
release_trust_mode="${HELMR_RELEASE_TRUST_MODE:-production}"
release_trust_san="${HELMR_RELEASE_TRUST_SAN:-}"
release_trust_source_digest="${HELMR_RELEASE_TRUST_SOURCE_DIGEST:-}"
dev_release_provenance_sha256="${HELMR_DEV_RELEASE_PROVENANCE_SHA256:-}"
dev_release_artifact_digest="${HELMR_DEV_RELEASE_ARTIFACT_DIGEST:-}"
build_tags="embed_console"
ldflags="-s -w"

case "$os/$arch" in
  linux/amd64|linux/arm64) ;;
  *)
    echo "unsupported CONTROL_IMAGE_PLATFORM: $platform" >&2
    exit 1
    ;;
esac

if [ -n "$build_version" ]; then
  ldflags="$ldflags -X github.com/helmrdotdev/helmr/internal/version.Version=$build_version"
fi
case "$release_trust_mode" in
  production)
    if [ -n "$release_trust_san" ] || [ -n "$release_trust_source_digest" ] ||
      [ -n "$dev_release_provenance_sha256" ] || [ -n "$dev_release_artifact_digest" ]; then
      echo "production release trust does not accept development identity inputs" >&2
      exit 1
    fi
    ;;
  development)
    case "$release_trust_san" in
      https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/heads/*) ;;
      *)
        echo "HELMR_RELEASE_TRUST_SAN must be an exact Helmr release workflow branch identity" >&2
        exit 1
        ;;
    esac
    printf '%s\n' "$release_trust_source_digest" | grep -Eq '^[0-9a-f]{40}$' || {
      echo "HELMR_RELEASE_TRUST_SOURCE_DIGEST must be an exact lowercase commit" >&2
      exit 1
    }
    printf '%s\n' "$dev_release_provenance_sha256" | grep -Eq '^[0-9a-f]{64}$' || {
      echo "HELMR_DEV_RELEASE_PROVENANCE_SHA256 must be an exact lowercase digest" >&2
      exit 1
    }
    printf '%s\n' "$dev_release_artifact_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || {
      echo "HELMR_DEV_RELEASE_ARTIFACT_DIGEST must be an exact GitHub artifact digest" >&2
      exit 1
    }
    build_tags="${build_tags},helmrdevtrust"
    ldflags="$ldflags -X github.com/helmrdotdev/helmr/internal/deployment.devReleaseCertificateSAN=$release_trust_san"
    ldflags="$ldflags -X github.com/helmrdotdev/helmr/internal/deployment.devReleaseSourceRepositoryDigest=$release_trust_source_digest"
    ;;
  *)
    echo "HELMR_RELEASE_TRUST_MODE must be production or development" >&2
    exit 1
    ;;
esac

rm -rf "$context"
mkdir -p "$context"
mkdir -p "$context/runtime-release"
mkdir -p "$context/toolchain-release"
mkdir -p "$context/manager-release"
for name in catalog.json catalog.sigstore.json trusted-root.json; do
  source_path="$runtime_release_dir/$name"
  if [ ! -f "$source_path" ] || [ -L "$source_path" ]; then
    echo "verified runtime release is missing regular file: $name" >&2
    exit 1
  fi
  install -m 0444 "$source_path" "$context/runtime-release/$name"
done
for name in catalog.json catalog.sigstore.json trusted-root.json; do
  source_path="$manager_release_dir/$name"
  if [ ! -f "$source_path" ] || [ -L "$source_path" ]; then
    echo "verified Manager release is missing regular file: $name" >&2
    exit 1
  fi
  install -m 0444 "$source_path" "$context/manager-release/$name"
done
for name in catalog.json catalog.sigstore.json trusted-root.json; do
  source_path="$runtime_release_dir/toolchain-release/$name"
  if [ ! -f "$source_path" ] || [ -L "$source_path" ]; then
    echo "verified standard-toolchain release is missing regular file: $name" >&2
    exit 1
  fi
  install -m 0444 "$source_path" "$context/toolchain-release/$name"
done

cd "$repo_root"
bun install --frozen-lockfile --ignore-scripts
make console-build

GOFLAGS='' GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build \
  -tags "$build_tags" \
  -trimpath \
  -ldflags="$ldflags" \
  -o "$control_binary" \
  ./cmd/helmr-control
GOFLAGS='' GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build \
  -tags "$build_tags" \
  -trimpath \
  -ldflags="$ldflags" \
  -o "$dispatcher_binary" \
  ./cmd/helmr-dispatcher

for binary in "$control_binary" "$dispatcher_binary"; do
  case "$release_trust_mode" in
    production)
      if strings "$binary" | grep -Fq 'release.yaml@refs/heads/'; then
        echo "production binary contains the development release identity" >&2
        exit 1
      fi
      ;;
    development)
      strings "$binary" | grep -Fqx "$release_trust_san" ||
        {
          echo "development binary is missing its exact release identity" >&2
          exit 1
        }
      strings "$binary" | grep -Fqx "$release_trust_source_digest" ||
        {
          echo "development binary is missing its exact source commit" >&2
          exit 1
        }
      ;;
  esac
done

cat >"$context/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot
ARG HELMR_SOURCE_COMMIT
ARG HELMR_DEV_RELEASE_PROVENANCE_SHA256
ARG HELMR_DEV_RELEASE_ARTIFACT_DIGEST
LABEL dev.helmr.source-commit="${HELMR_SOURCE_COMMIT}"
LABEL dev.helmr.dev-release-provenance-sha256="${HELMR_DEV_RELEASE_PROVENANCE_SHA256}"
LABEL dev.helmr.dev-release-artifact-digest="${HELMR_DEV_RELEASE_ARTIFACT_DIGEST}"
COPY helmr-control /usr/local/bin/helmr-control
COPY helmr-dispatcher /usr/local/bin/helmr-dispatcher
COPY --chown=0:0 --chmod=0444 runtime-release/catalog.json /usr/lib/helmr/runtime-release/catalog.json
COPY --chown=0:0 --chmod=0444 runtime-release/catalog.sigstore.json /usr/lib/helmr/runtime-release/catalog.sigstore.json
COPY --chown=0:0 --chmod=0444 runtime-release/trusted-root.json /usr/lib/helmr/runtime-release/trusted-root.json
COPY --chown=0:0 --chmod=0444 toolchain-release/catalog.json /usr/lib/helmr/toolchain-release/catalog.json
COPY --chown=0:0 --chmod=0444 toolchain-release/catalog.sigstore.json /usr/lib/helmr/toolchain-release/catalog.sigstore.json
COPY --chown=0:0 --chmod=0444 toolchain-release/trusted-root.json /usr/lib/helmr/toolchain-release/trusted-root.json
COPY --chown=0:0 --chmod=0444 manager-release/catalog.json /usr/lib/helmr/manager-release/catalog.json
COPY --chown=0:0 --chmod=0444 manager-release/catalog.sigstore.json /usr/lib/helmr/manager-release/catalog.sigstore.json
COPY --chown=0:0 --chmod=0444 manager-release/trusted-root.json /usr/lib/helmr/manager-release/trusted-root.json
ENTRYPOINT ["/usr/local/bin/helmr-control"]
EOF

docker build \
  --platform "$platform" \
  --build-arg "HELMR_SOURCE_COMMIT=$release_trust_source_digest" \
  --build-arg "HELMR_DEV_RELEASE_PROVENANCE_SHA256=$dev_release_provenance_sha256" \
  --build-arg "HELMR_DEV_RELEASE_ARTIFACT_DIGEST=$dev_release_artifact_digest" \
  -t "$image_uri" \
  "$context"
