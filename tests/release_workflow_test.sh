#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="$repo_root/.github/workflows/release.yaml"

if ! rg -F "scripts/build-embedded-adapter.sh" "$workflow" >/dev/null; then
	printf 'release workflow does not refresh the embedded adapter before CLI builds\n' >&2
	exit 1
fi

if ! rg -F "git diff --exit-code -- internal/adapter/js" "$workflow" >/dev/null; then
	printf 'release workflow does not verify embedded adapter artifacts are current\n' >&2
	exit 1
fi

if ! rg -F 'git status --porcelain -- internal/adapter/js' "$workflow" >/dev/null; then
	printf 'release workflow does not reject untracked embedded adapter artifacts\n' >&2
	exit 1
fi

if rg -F 'tar -C "$out_dir" -czf "dist/helmr-${os}-${arch}.tar.gz" helmr adapter' "$workflow" >/dev/null; then
	printf 'release workflow still packages external adapter sidecar files\n' >&2
	exit 1
fi

if ! rg -F 'tar -C "$out_dir" -czf "dist/helmr-${os}-${arch}.tar.gz" helmr' "$workflow" >/dev/null; then
	printf 'release workflow does not package the single helmr binary archive\n' >&2
	exit 1
fi

printf 'ok - release workflow tests\n'
