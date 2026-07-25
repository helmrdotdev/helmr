#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

dev/workflows/scripts/sync-local-sdk.sh
bun run --cwd dev/workflows typecheck
bun run --cwd dev/client typecheck
scripts/check-packed-sdk-consumer.sh
# shellcheck disable=SC2016
bun -e '
  import { analyzeProject } from "./runtime/typescript/src/analysis.ts"

  const result = await analyzeProject({
    root: "./dev/workflows",
    architecture: "x86_64",
  })
  console.log(
    `analyzed ${result.modules.length} dev workflow modules and ${result.buildPlan.definitions.length} definitions`,
  )
'
