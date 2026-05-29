#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/release.yaml"

assert_release_workflow_copies_adapter_file() {
	local source_file="$1"
	local archive_file="$2"

	if ! rg -F "cp $source_file \"\$out_dir/adapter/$archive_file\"" "$workflow" >/dev/null; then
		printf 'release workflow does not copy %s into CLI archives\n' "$archive_file" >&2
		exit 1
	fi
}

assert_release_workflow_copies_adapter_file "dist/cli/main.js" "main.js"
assert_release_workflow_copies_adapter_file "runtime/typescript/src/register.mjs" "register.mjs"
assert_release_workflow_copies_adapter_file "runtime/typescript/src/loader.mjs" "loader.mjs"

printf 'ok - release workflow tests\n'
