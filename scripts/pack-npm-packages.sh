#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

pack_destination="${1:-dist/npm-tarballs}"
mkdir -p "$pack_destination"

for package_dir in dist/npm/proto/package dist/npm/sdk/package; do
	if [ ! -f "$package_dir/package.json" ]; then
		echo "missing built npm package: $package_dir" >&2
		exit 1
	fi
	npm pack "$package_dir" --pack-destination "$pack_destination"
done
