#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
WORKFLOWS_DIR="${ROOT}/dev/workflows"

rm -rf "${WORKFLOWS_DIR}/vendor"
mkdir -p "${WORKFLOWS_DIR}/vendor"

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

(
  cd "${WORKFLOWS_DIR}"
  bun install --frozen-lockfile
)
