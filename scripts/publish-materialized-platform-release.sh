#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILDER_IMAGE="nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24"

stdin_credentials=false
if [ "${1:-}" = --credentials-stdin ]; then
  stdin_credentials=true
  shift
fi
if [ "$#" != 2 ]; then
  printf 'usage: scripts/publish-materialized-platform-release.sh [--credentials-stdin] STORE_URI DIRECTORY\n' >&2
  exit 2
fi
store_uri=$1
input=$2
[ -n "${store_uri}" ] || { printf 'Platform store URI is required\n' >&2; exit 1; }
[ -d "${input}" ] || { printf 'materialized Platform release directory is missing: %s\n' "${input}" >&2; exit 1; }
input="$(cd "${input}" && pwd)"
[ -f "${input}/platform-release.json" ] && [ ! -L "${input}/platform-release.json" ] || {
  printf 'materialized Platform release manifest is missing or is not a regular file\n' >&2
  exit 1
}
[ -d "${input}/objects/sha256" ] && [ ! -L "${input}/objects/sha256" ] || {
  printf 'materialized Platform release object directory is missing or invalid\n' >&2
  exit 1
}
[ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ] || {
  printf 'Platform release publication requires a clean Product checkout\n' >&2
  exit 1
}
command -v docker >/dev/null 2>&1 || { printf 'docker is required to publish the linux/amd64 Platform release\n' >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { printf 'tar is required to publish the Platform release\n' >&2; exit 1; }

work="$(mktemp -d "${TMPDIR:-/tmp}/helmr-platform-release-publish.XXXXXX")"
cleanup() {
  status=$?
  rm -rf "${work}"
  exit "${status}"
}
trap cleanup EXIT
source_dir="${work}/source"
publish_dir="${work}/input"
install -d -m0700 "${source_dir}" "${publish_dir}" "${publish_dir}/objects" "${publish_dir}/objects/sha256"

git -C "${ROOT}" archive --format=tar HEAD | tar -xf - -C "${source_dir}"
[ ! -e "${source_dir}/.git" ] || { printf 'Platform release source export contains Git metadata\n' >&2; exit 1; }
install -m0400 "${input}/platform-release.json" "${publish_dir}/platform-release.json"
while IFS= read -r -d '' object; do
  install -m0400 "${object}" "${publish_dir}/objects/sha256/$(basename "${object}")"
done < <(find "${input}/objects/sha256" -mindepth 1 -maxdepth 1 -type f -print0)

credential_args=(--env AWS_ACCESS_KEY_ID --env AWS_SECRET_ACCESS_KEY --env AWS_SESSION_TOKEN)
# shellcheck disable=SC2016
publish_command='exec go -C /work run ./cmd/control-plane release publish --store "$PLATFORM_STORE_URI" --input /input'
if [ "${stdin_credentials}" = true ]; then
  # No keys in Docker Config.Env or the environment captured by nix develop.
  unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
  # Host output suppression does not disable daemon-side container logs.
  credential_args=(-i --log-driver=none)
  # Python is in the default dev shell. Consume the bounded pipe only AFTER
  # Nix initialization; export in this final receiver and immediately exec Go.
  publish_command=$(cat <<'RECEIVER'
exec python3 -c '
import json, os, re, sys
try:
    raw = sys.stdin.buffer.read(16385)
    if len(raw) > 16384: raise ValueError()
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result: raise ValueError()
            result[key] = value
        return result
    packet = json.loads(raw.decode("utf-8"), object_pairs_hook=unique)
    names = ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN")
    if set(packet) != {"version", *names} or type(packet["version"]) is not int or packet["version"] != 1:
        raise ValueError()
    for name in names:
        if not isinstance(packet[name], str) or not re.fullmatch(r"[A-Za-z0-9/+=._-]{1,8192}", packet[name]):
            raise ValueError()
except Exception:
    print("publisher credential stdin rejected", file=sys.stderr)
    sys.exit(65)
os.environ.update({name: packet[name] for name in names})
os.environ["AWS_EC2_METADATA_DISABLED"] = "true"
os.execvp("go", ["go", "-C", "/work", "run", "./cmd/control-plane", "release", "publish",
                 "--store", os.environ["PLATFORM_STORE_URI"], "--input", "/input"])
'
RECEIVER
)
fi
PLATFORM_STORE_URI="${store_uri}" docker run --rm \
  --platform linux/amd64 \
  "${credential_args[@]}" \
  --env AWS_REGION \
  --env AWS_DEFAULT_REGION \
  --env PLATFORM_STORE_URI \
  --mount "type=bind,source=${source_dir},target=/work,readonly" \
  --mount "type=bind,source=${publish_dir},target=/input,readonly" \
  -w /work \
  "${BUILDER_IMAGE}" \
  sh -ceu '
    nix --extra-experimental-features "nix-command flakes" \
      --option sandbox false \
      --option filter-syscalls false \
      develop path:/work \
      -c bash -ceu "$1"
  ' publisher "${publish_command}"
