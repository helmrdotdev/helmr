import { afterAll, describe, expect, test } from "bun:test"
import {
  mkdir,
  mkdtemp,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"
import { fileURLToPath } from "node:url"

import { analyzeProject } from "./analysis"

const sdkRoot = fileURLToPath(new URL("../../../sdk/typescript", import.meta.url))

describe("declaration discovery", () => {
  test("uses only explicit dirs without name-based default ignores", async () => {
    const root = await project()
    await writeModule(root, "tasks/shared.ts", task("shared"))
    await writeModule(
      root,
      "tasks/barrel.ts",
      'export { shared } from "./shared.ts"\n',
    )
    await writeModule(root, "tasks/a.test.ts", task("test"))
    await writeModule(root, "tasks/_worker.ts", task("underscore"))
    await writeModule(root, "tasks/.hidden.ts", task("hidden"))
    await writeModule(root, "tasks/generated/ignored.ts", 'throw new Error("ignored module imported")\n')
    await writeModule(root, "outside.ts", task("outside"))
    await symlink(resolve(root, "outside.ts"), resolve(root, "tasks/link.ts"))
    await config(root, {
      dirs: ["./tasks", "./tasks/generated"],
      ignorePatterns: ["tasks/generated/**"],
    })

    const result = await analyzeProject({
      root,
      architecture: "x86_64",
    })

    expect(result.modules).toEqual([
      "tasks/.hidden.ts",
      "tasks/_worker.ts",
      "tasks/a.test.ts",
      "tasks/barrel.ts",
      "tasks/shared.ts",
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
      modulePath: "tasks/barrel.ts",
      exportName: "shared",
    })
  })

  test("deduplicates overlapping dirs and ignores directory symlinks", async () => {
    const root = await project()
    await writeModule(root, "tasks/nested/task.ts", task("nested"))
    await writeModule(root, "outside/task.ts", task("outside"))
    await symlink(resolve(root, "outside"), resolve(root, "tasks/linked"))
    await config(root, {
      dirs: ["./tasks", "./tasks/nested"],
    })

    const result = await analyzeProject({
      root,
      architecture: "aarch64",
    })
    expect(result.modules).toEqual(["tasks/nested/task.ts"])
    expect(result.buildPlan.definitions).toHaveLength(1)
  })

  test("fails reserved output, dependency dirs, and non-branded config", async () => {
    const reserved = await project()
    await mkdir(resolve(reserved, "helmr"))
    await config(reserved, { dirs: ["./tasks"] })
    await expect(analyzeProject({
      root: reserved,
      architecture: "x86_64",
    })).rejects.toThrow("reserved")

    const dependency = await project()
    await config(dependency, { dirs: ["./node_modules"] })
    await expect(analyzeProject({
      root: dependency,
      architecture: "x86_64",
    })).rejects.toThrow("dependency namespace")

    const unbranded = await project()
    await writeFile(
      resolve(unbranded, "helmr.config.ts"),
      'export default { project: "helmr", dirs: ["./tasks"] }\n',
    )
    await expect(analyzeProject({
      root: unbranded,
      architecture: "x86_64",
    })).rejects.toThrow("defineConfig")
  })
})

async function project(): Promise<string> {
  const root = await mkdtemp(resolve(tmpdir(), "helmr-discovery-"))
  await mkdir(resolve(root, "tasks"), { recursive: true })
  await mkdir(resolve(root, "node_modules/@helmr"), { recursive: true })
  await symlink(sdkRoot, resolve(root, "node_modules/@helmr/sdk"), "dir")
  testCleanup.push(root)
  return root
}

const testCleanup: string[] = []
afterAll(async () => {
  await Promise.all(
    testCleanup.map((root) => rm(root, { force: true, recursive: true })),
  )
})

async function config(
  root: string,
  options: {
    readonly dirs: readonly string[]
    readonly ignorePatterns?: readonly string[]
  },
): Promise<void> {
  await writeFile(
    resolve(root, "helmr.config.ts"),
    [
      'import { defineConfig } from "@helmr/sdk"',
      `export default defineConfig(${JSON.stringify({
        project: "helmr",
        dirs: options.dirs,
        ...(options.ignorePatterns === undefined
          ? {}
          : { ignorePatterns: options.ignorePatterns }),
      })})`,
    ].join("\n"),
  )
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
