import { afterAll, describe, expect, test } from "bun:test"
import {
  cp,
  realpath,
  mkdir,
  mkdtemp,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"

import { analyzeProject } from "./test-process"
import type { HelmrConfig } from "@helmr/sdk/internal"

describe("declaration discovery", () => {
  test("uses only explicit dirs without name-based default ignores", async () => {
    const root = await project()
    await writeModule(root, "agents/shared.js", agentModule("shared"))
    await writeModule(
      root,
      "agents/barrel.js",
      'export { shared } from "./shared.js"\n',
    )
    await writeModule(root, "agents/a.test.js", agentModule("test"))
    await writeModule(root, "agents/_worker.js", agentModule("underscore"))
    await writeModule(root, "agents/.hidden.js", agentModule("hidden"))
    await writeModule(root, "agents/generated/ignored.js", 'throw new Error("ignored module imported")\n')
    await writeModule(root, "outside.js", agentModule("outside"))
    const config = normalizedConfig({
      dirs: ["./agents", "./agents/generated"],
      ignorePatterns: ["agents/generated/**"],
    })

    const result = await analyzeProject({
      root,
      architecture: "x86_64",
      config,
    })

    expect(result.modules).toEqual([
      "agents/.hidden.js",
      "agents/_worker.js",
      "agents/a.test.js",
      "agents/barrel.js",
      "agents/shared.js",
    ])
    expect(result.buildPlan.definitions.filter((item) => item.kind === "agent").map((item) => item.declaredId)).toEqual([
      "hidden",
      "shared",
      "test",
      "underscore",
    ])
    expect(result.definitionIndex.agents.find(
      (item) => item.id === "shared",
    )).toMatchObject({
      modulePath: "helmr/app/entry-3.mjs",
      exportName: "shared",
    })
  })

  test("deduplicates overlapping dirs and ignores directory symlinks", async () => {
    const root = await project()
    await writeModule(root, "agents/nested/agent.js", agentModule("nested"))
    await writeModule(root, "linked-only/agent.js", agentModule("linked"))
    const direct = await analyzeProject({
      root,
      architecture: "x86_64",
      config: normalizedConfig({ dirs: ["./linked-only"] }),
    })
    expect(direct.modules).toEqual(["linked-only/agent.js"])
    expect(direct.buildPlan.definitions.filter((item) => item.kind === "agent").map((item) => item.declaredId)).toEqual(["linked"])
    await symlink("../linked-only", resolve(root, "agents/linked"), "dir")
    const config = normalizedConfig({
      dirs: ["./agents", "./agents/nested"],
    })

    const result = await analyzeProject({
      root,
      architecture: "x86_64",
      config,
    })
    expect(result.modules).toEqual(["agents/nested/agent.js"])
    expect(result.buildPlan.definitions).toHaveLength(2)
  })

  test("fails reserved output, dependency dirs, and invalid config", async () => {
    const reserved = await project()
    await mkdir(resolve(reserved, "helmr"))
    await expect(analyzeProject({
      root: reserved,
      architecture: "x86_64",
      config: normalizedConfig({ dirs: ["./agents"] }),
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
    await writeModule(root, "agents/agent.ts", agentModule("plain"))
    await writeModule(root, "agents/types.d.ts", "export interface Ignored {}\n")
    await writeModule(root, "agents/types.d.mts", "export interface Ignored {}\n")
    await writeModule(root, "agents/types.d.cts", "export interface Ignored {}\n")
    const result = await analyzeProject({
      root,
      architecture: "x86_64",
      config: normalizedConfig({ dirs: ["./agents"] }),
    })
    expect(result.modules).toEqual(["agents/agent.ts"])
    expect(result.buildPlan.definitions.filter((item) => item.kind === "agent")).toEqual([{
      declaredId: "plain",
      kind: "agent",
      manifest: { templateDefinitionId: "plain-computer", hasInit: false, inputKind: "text", closeAfterIdleMs: 604800000, hibernateAfterIdleMs: null, triggers: {} },
    }])
  })
})

async function project(): Promise<string> {
  const root = await mkdtemp(resolve(tmpdir(), "helmr-discovery-"))
  await mkdir(resolve(root, "agents"), { recursive: true })
  await writeFile(
    resolve(root, "package.json"),
    JSON.stringify({ name: "analysis-fixture", private: true, type: "module" }),
  )
  const repository = new URL("../../../", import.meta.url).pathname
  for (const name of ["sdk", "proto"]) await cp(resolve(repository, `dist/npm/${name}/package`), resolve(root, `node_modules/@helmr/${name}`), { recursive: true })
  await cp(await realpath(resolve(repository, "sdk/typescript/node_modules/@bufbuild/protobuf")), resolve(root, "node_modules/@bufbuild/protobuf"), { recursive: true, dereference: true })
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

function agentModule(id: string): string {
  return [
    'import { agent, template, image } from "@helmr/sdk"',
    `export const ${id.replace("-", "_")} = agent({`,
    `  id: ${JSON.stringify(id)},`,
    `  template: template({ id: ${JSON.stringify(id+"-computer")}, image: image("root").from("ubuntu:24.04"), resources: { cpu: 1, memory: "1GiB" } }),`,
    "  turn: () => null,",
    "})",
  ].join("\n")
}
