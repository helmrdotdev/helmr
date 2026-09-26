#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [ -n "${RELEASE_TAG:-}" ] && [ "$#" -ne 2 ]; then
  printf 'release cohort requires platform version and full source commit\n' >&2
  exit 2
fi
version=${1:-v0.0.0-cohort-test}
source_commit=${2:-$(git -C "$repo_root" rev-parse HEAD)}
expected="$version ($source_commit)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
ldflags="-X github.com/helmrdotdev/helmr/internal/version.Version=$version -X github.com/helmrdotdev/helmr/internal/version.SourceCommit=$source_commit"

cd "$repo_root"
for command in helmr control-plane dispatcher; do
  go build -trimpath -ldflags="$ldflags" -o "$tmp/$command" "./cmd/$command"
done
worker=$(nix build --no-link --print-out-paths .#worker)

for binary in "$tmp/helmr" "$tmp/control-plane" "$tmp/dispatcher"; do
  actual=$($binary --version)
  [ "$actual" = "$expected" ] || {
    printf '%s reported %s, expected %s\n' "$binary" "$actual" "$expected" >&2
    exit 1
  }
done

actual=$("$worker/bin/worker" --version)
[ "$actual" = 'worker (build identity: signed release manifest and worker-host-artifacts.json)' ] || {
  printf 'Worker must report artifact identity guidance, got %s\n' "$actual" >&2
  exit 1
}

printf 'ok - version cohort contract\n'
