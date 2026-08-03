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
  import { compileProgram } from "./compiler/typescript/src/bundle.ts"
  import { mkdtemp, rm } from "node:fs/promises"
  import { tmpdir } from "node:os"
  import { resolve } from "node:path"

  const outputRoot = await mkdtemp(resolve(tmpdir(), "helmr-dev-compiler-"))
  try {
    const result = await compileProgram({
      root: ".",
      runtimeRoot: process.cwd(),
      architecture: "x86_64",
      manager: "bun",
      nodeVersion: "24.16.0",
      outputRoot,
      config: {
        dirs: ["dev/workflows/tasks"],
        ignorePatterns: [],
      },
    })
    console.log(
      `compiled ${result.modules.length} dev workflow modules and ${result.analysis.buildPlan.definitions.length} definitions`,
    )
  } finally {
    await rm(outputRoot, { force: true, recursive: true })
  }
'
