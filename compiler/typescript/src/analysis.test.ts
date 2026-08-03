import { afterAll, describe, expect, test } from "bun:test"
import { createHash } from "node:crypto"
import {
  mkdir,
  mkdtemp,
  rm,
  writeFile,
} from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"

import { compileProgram } from "./bundle"
import type { HelmrConfig } from "@helmr/sdk/internal"

async function analyzeProject(options: {
  readonly root: string
  readonly architecture: "x86_64"
  readonly config: HelmrConfig
}) {
  const compiled = await compileProgram({
    ...options,
    manager: "npm",
    nodeVersion: "24.16.0",
    outputRoot: await outputRoot(),
    runtimeRoot: options.root,
  })
  return {
    ...compiled.analysis,
    modules: compiled.modules,
  }
}

async function outputRoot(): Promise<string> {
  const root = await mkdtemp(resolve(tmpdir(), "helmr-analysis-output-"))
  testCleanup.push(root)
  return root
}

describe("declaration discovery", () => {
  test("uses only explicit dirs without name-based default ignores", async () => {
    const root = await project()
    await writeModule(root, "tasks/shared.js", task("shared"))
    await writeModule(
      root,
      "tasks/barrel.js",
      'export { shared } from "./shared.js"\n',
    )
    await writeModule(root, "tasks/a.test.js", task("test"))
    await writeModule(root, "tasks/_worker.js", task("underscore"))
    await writeModule(root, "tasks/.hidden.js", task("hidden"))
    await writeModule(root, "tasks/generated/ignored.js", 'throw new Error("ignored module imported")\n')
    await writeModule(root, "outside.js", task("outside"))
    const config = normalizedConfig({
      dirs: ["./tasks", "./tasks/generated"],
      ignorePatterns: ["tasks/generated/**"],
    })

    const result = await analyzeProject({
      root,
      architecture: "x86_64",
      config,
    })

    expect(result.modules).toEqual([
      "tasks/.hidden.js",
      "tasks/_worker.js",
      "tasks/a.test.js",
      "tasks/barrel.js",
      "tasks/shared.js",
    ])
    expect(result.buildPlan.definitions.map((item) => item.declaredId)).toEqual([
      "hidden",
      "shared",
      "test",
      "underscore",
    ])
    expect(result.declarationLocator.declarations.find(
      (item) => item.declaredId === "shared",
    )).toMatchObject({
      modulePath: generatedModule("tasks/barrel.js"),
      exportName: "shared",
      slot: "handler",
    })
  })

  test("deduplicates overlapping dirs and ignores directory symlinks", async () => {
    const root = await project()
    await writeModule(root, "tasks/nested/task.js", task("nested"))
    const config = normalizedConfig({
      dirs: ["./tasks", "./tasks/nested"],
    })

    const result = await analyzeProject({
      root,
      architecture: "x86_64",
      config,
    })
    expect(result.modules).toEqual(["tasks/nested/task.js"])
    expect(result.buildPlan.definitions).toHaveLength(1)
  })

  test("fails reserved output, dependency dirs, and invalid config", async () => {
    const reserved = await project()
    await mkdir(resolve(reserved, "helmr"))
    await expect(analyzeProject({
      root: reserved,
      architecture: "x86_64",
      config: normalizedConfig({ dirs: ["./tasks"] }),
    })).rejects.toThrow("reserved")

    const nestedReserved = await project()
    await mkdir(resolve(nestedReserved, "tasks/.helmr"))
    await expect(analyzeProject({
      root: nestedReserved,
      architecture: "x86_64",
      config: normalizedConfig({ dirs: ["./tasks"] }),
    })).rejects.toThrow("reserved")

    const dependency = await project()
    await expect(analyzeProject({
      root: dependency,
      architecture: "x86_64",
      config: normalizedConfig({ dirs: ["./node_modules"] }),
    })).rejects.toThrow("dependency namespace")
  })

  test("compiles TypeScript declaration candidates", async () => {
    const root = await project()
    await writeModule(root, "tasks/task.ts", task("plain"))
    await writeModule(root, "tasks/types.d.ts", "export interface Ignored {}\n")
    await writeModule(root, "tasks/types.d.mts", "export interface Ignored {}\n")
    await writeModule(root, "tasks/types.d.cts", "export interface Ignored {}\n")
    const result = await analyzeProject({
      root,
      architecture: "x86_64",
      config: normalizedConfig({ dirs: ["./tasks"] }),
    })
    expect(result.modules).toEqual(["tasks/task.ts"])
    expect(result.programDeclarations).toEqual([{
      declaredId: "plain",
      kind: "task",
      slots: ["handler"],
    }])
  })
})

async function project(): Promise<string> {
  const root = await mkdtemp(resolve(tmpdir(), "helmr-discovery-"))
  await mkdir(resolve(root, "tasks"), { recursive: true })
  await writeFile(
    resolve(root, "package.json"),
    JSON.stringify({ name: "analysis-fixture", private: true, type: "module" }),
  )
  await mkdir(resolve(root, "node_modules/@helmr"), { recursive: true })
  await mkdir(resolve(root, "node_modules/@helmr/sdk"))
  await writeFile(
    resolve(root, "node_modules/@helmr/sdk/package.json"),
    JSON.stringify({
      name: "@helmr/sdk",
      type: "module",
      exports: "./index.mjs",
    }),
  )
  await writeFile(
    resolve(root, "node_modules/@helmr/sdk/index.mjs"),
    [
      'const brand = Symbol.for("helmr.sdk.v0.definition")',
      "export function task(config) {",
      "  return Object.freeze({",
      "    [brand]: Object.freeze({",
      '      kind: "task",',
      "      id: config.id,",
      "      hasPayload: false,",
      "      handler: config.run,",
      "    }),",
      "  })",
      "}",
    ].join("\n"),
  )
  testCleanup.push(root)
  return root
}

const testCleanup: string[] = []
afterAll(async () => {
  await Promise.all(
    testCleanup.map((root) => rm(root, { force: true, recursive: true })),
  )
})

function normalizedConfig(
  options: {
    readonly dirs: readonly string[]
    readonly ignorePatterns?: readonly string[]
  },
): HelmrConfig {
  return {
    dirs: options.dirs.map((value) => value.replace(/^\.\//, "")).sort(),
    ignorePatterns: [...(options.ignorePatterns ?? [])].sort(),
  }
}

async function writeModule(
  root: string,
  path: string,
  source: string,
): Promise<void> {
  const target = resolve(root, path)
  await mkdir(dirname(target), { recursive: true })
  await writeFile(target, source)
}

function task(id: string): string {
  return [
    'import { task } from "@helmr/sdk"',
    `export const ${id.replace("-", "_")} = task({`,
    `  id: ${JSON.stringify(id)},`,
    "  run: () => null,",
    "})",
  ].join("\n")
}

function generatedModule(source: string): string {
  const digest = createHash("sha256").update(source).digest("hex")
  const directory = dirname(source)
  const prefix = directory === "." ? "" : `${directory}/`
  return `${prefix}.helmr/modules/${digest}.mjs`
}
