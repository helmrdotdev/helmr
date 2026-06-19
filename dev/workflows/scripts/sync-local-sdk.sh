#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
WORKFLOWS_DIR="${ROOT}/dev/workflows"
VENDOR_DIR="${WORKFLOWS_DIR}/vendor"

rm -rf "${VENDOR_DIR}"
mkdir -p "${VENDOR_DIR}"

"${ROOT}/scripts/build-npm-packages.sh"

rsync -a "${ROOT}/dist/npm/sdk/package/" "${VENDOR_DIR}/helmr-sdk/"
rsync -a "${ROOT}/dist/npm/proto/package/" "${VENDOR_DIR}/helmr-proto/"

node --input-type=module - "${VENDOR_DIR}/helmr-sdk/package.json" <<'NODE'
import { readFile, writeFile } from "node:fs/promises"

const path = process.argv[2]
const pkg = JSON.parse(await readFile(path, "utf8"))
pkg.dependencies = {
  ...(pkg.dependencies ?? {}),
  "@helmr/proto": "file:../helmr-proto",
}
await writeFile(path, `${JSON.stringify(pkg, null, 2)}\n`)
NODE

(
  cd "${WORKFLOWS_DIR}"
  bun install
)
