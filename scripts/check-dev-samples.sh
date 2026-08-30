#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

dev/workflows/scripts/sync-local-sdk.sh
bun run --cwd dev/workflows typecheck
bun run --cwd dev/schedule-workflows typecheck
bun run --cwd dev/client typecheck
scripts/check-packed-sdk-consumer.sh
# shellcheck disable=SC2016
bun -e '
  import { compileProgram } from "./compiler/typescript/src/bundle.ts"
  import { mkdir, mkdtemp, rm } from "node:fs/promises"
  import { tmpdir } from "node:os"
  import { resolve } from "node:path"

  const outputRoot = await mkdtemp(resolve(tmpdir(), "helmr-dev-compiler-"))
  try {
    const compile = async (dirs: string[], name: string) => {
      const output = resolve(outputRoot, name)
      await mkdir(output)
      return compileProgram({
        root: ".", runtimeRoot: process.cwd(), architecture: "x86_64",
        manager: "bun", nodeVersion: "24.20.0", outputRoot: output,
        config: { dirs, ignorePatterns: [] },
      })
    }
    const result = await compile(["dev/workflows/tasks"], "execution")
    if (result.analysis.buildPlan.definitions.some(
      (definition) => definition.kind === "task" && definition.manifest.schedule !== undefined,
    )) throw new Error("ordinary dev workflows must be schedule-free")
    console.log(
      `compiled ${result.modules.length} dev workflow modules and ${result.analysis.buildPlan.definitions.length} definitions`,
    )
    const scheduleResult = await compile(["dev/schedule-workflows/tasks"], "schedule")
    const scheduleTasks = scheduleResult.analysis.buildPlan.definitions.filter(
      (definition) => definition.kind === "task",
    )
    if (
      scheduleTasks.length !== 1 ||
      scheduleTasks[0]?.manifest.schedule === undefined ||
      scheduleTasks[0]?.declaredId !== "schedule-smoke" ||
      scheduleTasks[0]?.manifest.run.ttlMs !== 300_000
    ) throw new Error("Schedule fixture must compile one bounded schedule-smoke Task")
    console.log(
      `compiled ${scheduleResult.modules.length} Schedule workflow modules and ${scheduleResult.analysis.buildPlan.definitions.length} definitions`,
    )
  } finally {
    await rm(outputRoot, { force: true, recursive: true })
  }
'
