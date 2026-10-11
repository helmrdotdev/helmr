import { expect, test } from "bun:test"
import { cp, mkdir, mkdtemp, realpath, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { resolve } from "node:path"
import { analyzeProject, runHostConfig } from "./test-process"

const repository = new URL("../../../", import.meta.url).pathname
const examples = {
  "hello-world": ["checked-reply", "hello-world"],
  "cli-tooling": ["cli-tooling"],
  "task-secrets": ["use-secret"],
  "github-pr-review": ["github-pr-review"],
}

for (const [name, agents] of Object.entries(examples)) {
  test(`${name} compiles with the packed SDK as an independent Agent project`, async () => {
    const root = await realpath(await mkdtemp(resolve(tmpdir(), "helmr-example-")))
    try {
      for (const path of ["tasks", "package.json", "helmr.config.ts", "tsconfig.json"]) {
        await cp(resolve(repository, "examples", name, path), resolve(root, path), { recursive: true })
      }
      await mkdir(resolve(root, "node_modules/@helmr"), { recursive: true })
      for (const name of ["sdk", "proto"]) {
        await cp(resolve(repository, `dist/npm/${name}/package`), resolve(root, `node_modules/@helmr/${name}`), { recursive: true })
      }
      for (const [name, source] of [
        ["@bufbuild/protobuf", "sdk/typescript/node_modules/@bufbuild/protobuf"],
        ["zod", "examples/hello-world/node_modules/zod"],
      ]) {
        await cp(await realpath(resolve(repository, source!)), resolve(root, "node_modules", name!), { recursive: true, dereference: true })
      }
      const result = await analyzeProject({ root, architecture: "x86_64", config: runHostConfig(root).discovery })
      expect(result.definitionIndex.agents.map((entry: { id: string }) => entry.id).sort()).toEqual(agents)
      expect(result.definitionIndex.computers).toHaveLength(1)
      expect(new Set(result.buildPlan.definitions.map((entry: { kind: string }) => entry.kind))).toEqual(new Set(["agent", "computer"]))
    } finally {
      await rm(root, { recursive: true, force: true })
    }
  })
}
