#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
WORKFLOWS_DIR="${ROOT}/dev/workflows"
FAILING_BUILD_DIR="${ROOT}/dev/workflows-failing-build"

rm -rf "${WORKFLOWS_DIR}/vendor" "${FAILING_BUILD_DIR}/vendor"
mkdir -p "${WORKFLOWS_DIR}/vendor" "${FAILING_BUILD_DIR}/vendor"

"${ROOT}/scripts/build-npm-packages.sh"

vendor_sdk() {
  local vendor_dir=$1
  rsync -a "${ROOT}/dist/npm/sdk/package/" "${vendor_dir}/helmr-sdk/"
  rsync -a "${ROOT}/dist/npm/proto/package/" "${vendor_dir}/helmr-proto/"
  node --input-type=module - "${vendor_dir}/helmr-sdk/package.json" <<'NODE'
import { readFile, writeFile } from "node:fs/promises"

const path = process.argv[2]
const pkg = JSON.parse(await readFile(path, "utf8"))
pkg.dependencies = {
  ...(pkg.dependencies ?? {}),
  "@helmr/proto": "file:../helmr-proto",
}
await writeFile(path, `${JSON.stringify(pkg, null, 2)}\n`)
NODE
}

vendor_sdk "${WORKFLOWS_DIR}/vendor"
vendor_sdk "${FAILING_BUILD_DIR}/vendor"

for project_dir in "${WORKFLOWS_DIR}" "${FAILING_BUILD_DIR}"; do
  (
    cd "${project_dir}"
    bun install --frozen-lockfile
  )
done
