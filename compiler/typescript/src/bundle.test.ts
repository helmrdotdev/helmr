import { afterAll, describe, expect, test } from "bun:test"
import {
  mkdir,
  mkdtemp,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"
import { pathToFileURL } from "node:url"

import { compileConfig, compileProgram, ESBUILD_VERSION } from "./bundle"
import { inspectCanonicalConfig } from "./config"

describe("v0 compiler contract", () => {
  test("accepts only the canonical Config Evaluator handoff", () => {
    expect(inspectCanonicalConfig({
      dirs: ["tasks"],
      ignorePatterns: [],
    })).toEqual({ dirs: ["tasks"], ignorePatterns: [] })
    expect(() => inspectCanonicalConfig({
      dirs: ["tasks"],
      ignorePatterns: [],
      unknown: true,
    })).toThrow("build contract")
  })

  test("evaluates real TypeScript config through the pinned compiler", async () => {
    const root = await project()
    await source(root, "config-values.ts", 'export const dirs = ["tasks"]\n')
    await source(
      root,
      "helmr.config.ts",
      [
        'import { dirs } from "./config-values.ts"',
        'const ignorePatterns = ["**/*.test.ts"]',
        "export default { dirs: [...dirs], ignorePatterns }",
      ].join("\n"),
    )
    const compiled = await compileConfig({
      nodeVersion: "24.16.0",
      outputRoot: await output(),
      root,
    })
    try {
      const namespace = await import(pathToFileURL(compiled.path).href)
      expect(namespace.default).toEqual({
        dirs: ["tasks"],
        ignorePatterns: ["**/*.test.ts"],
      })
    } finally {
      await compiled.cleanup()
    }
  })

  test("compiles standard JavaScript and TypeScript modules without shared chunks", async () => {
    for (
      const extension of ["cjs", "cts", "js", "jsx", "mjs", "mts", "ts", "tsx"]
    ) {
      const root = await project()
      await source(
        root,
        `tasks/example.${extension}`,
        extension === "cjs"
          ? commonjsTask(extension)
          : `${extension === "jsx" || extension === "tsx" ? "const view = <div />;\n" : ""}${task(extension)}`,
      )
      const compiled = await compile(root)
      expect(compiled.analysis.programDeclarations).toEqual([{
        declaredId: extension,
        kind: "task",
        slots: ["handler"],
      }])
      const paths = [...compiled.files.keys()]
      expect(paths.filter((path) => path.endsWith(".mjs"))).toHaveLength(1)
      expect(paths.some((path) => path.includes("chunk"))).toBe(false)
      const sourceMapPath = paths.find((path) => path.endsWith(".mjs.map"))
      expect(sourceMapPath).toBeDefined()
      const sourceMap = JSON.parse(
        new TextDecoder().decode(compiled.files.get(sourceMapPath!)),
      )
      expect(sourceMap.sourceRoot).toBeUndefined()
      expect(sourceMap.sources.length).toBeGreaterThan(0)
      expect(
        sourceMap.sources.every((source: unknown) =>
          typeof source === "string" &&
          source.startsWith("file:///opt/helmr/program/")
        ),
      ).toBe(true)
      expect(sourceMap.sourcesContent).toBeUndefined()
    }
  })

  test("compiles a Schedule that references the exact exported Sandbox", async () => {
    const root = await project()
    await source(
      root,
      "tasks/sandbox.ts",
      [
        'import { sandbox } from "@helmr/sdk"',
        'export const maintenance = sandbox({ id: "maintenance" })',
      ].join("\n"),
    )
    await source(
      root,
      "tasks/index.ts",
      'export { maintenance } from "./sandbox.ts"\n',
    )
    await source(
      root,
      "tasks/schedule.ts",
      [
        'import { schedules } from "@helmr/sdk"',
        'import { maintenance } from "./index.ts"',
        "export const nightly = schedules.task({",
        '  id: "nightly",',
        '  cron: { pattern: "0 3 * * *", timezone: "UTC" },',
        "  workspace: { sandbox: maintenance },",
        "  run: () => null,",
        "})",
      ].join("\n"),
    )
    const compiled = await compile(root)
    expect(compiled.analysis.buildPlan.definitions).toContainEqual(
      expect.objectContaining({
        kind: "task",
        manifest: expect.objectContaining({
          schedule: expect.objectContaining({
            workspace: { sandboxId: "maintenance", secrets: [] },
          }),
        }),
      }),
    )
  })

  test("rejects a Schedule whose Sandbox is not exported", async () => {
    const root = await project()
    await source(
      root,
      "tasks/schedule.ts",
      [
        'import { sandbox, schedules } from "@helmr/sdk"',
        'const maintenance = sandbox({ id: "maintenance" })',
        "export const nightly = schedules.task({",
        '  id: "nightly",',
        '  cron: { pattern: "0 3 * * *", timezone: "UTC" },',
        "  workspace: { sandbox: maintenance },",
        "  run: () => null,",
        "})",
      ].join("\n"),
    )
    await expect(compile(root)).rejects.toThrow(
      'task "nightly" schedule references unexported Sandbox "maintenance"',
    )
  })

  test("uses pinned esbuild semantics for first-party imports", async () => {
    const root = await project()
    await source(root, "tasks/asset.json", '{"value":"json"}')
    await source(
      root,
      "tasks/example.ts",
      [
        'import asset from "./asset.json"',
        'const metadata = import.meta.url',
        "void asset",
        "void metadata",
        task("standard-inputs"),
      ].join("\n"),
    )

    const compiled = await compile(root)
    expect(compiled.analysis.programDeclarations).toEqual([{
      declaredId: "standard-inputs",
      kind: "task",
      slots: ["handler"],
    }])
  })

  test("preserves Node import and require export conditions", async () => {
    const root = await project()
    await source(
      root,
      "node_modules/conditional-package/package.json",
      JSON.stringify({
        name: "conditional-package",
        exports: {
          import: "./import.mjs",
          require: "./require.cjs",
        },
      }),
    )
    await source(
      root,
      "node_modules/conditional-package/import.mjs",
      'export const value = "import"\n',
    )
    await source(
      root,
      "node_modules/conditional-package/require.cjs",
      'exports.value = "require"\n',
    )
    await source(
      root,
      "tasks/import.ts",
      [
        'import { value } from "conditional-package"',
        'import { task } from "@helmr/sdk"',
        'export const declaration = task({ id: "import-condition", run: () => value })',
      ].join("\n"),
    )
    await source(
      root,
      "tasks/require.cjs",
      [
        'const { value } = require("conditional-package")',
        'const { task } = require("@helmr/sdk")',
        'exports.declaration = task({ id: "require-condition", run: () => value })',
      ].join("\n"),
    )

    const compiled = await compile(root)
    const manifest = JSON.parse(
      new TextDecoder().decode(compiled.files.get("helmr/compiler-result.json")),
    )
    const bySource = new Map(
      manifest.outputs.map(
        (item: { modulePath: string; sourcePath: string }) =>
          [item.sourcePath, item.modulePath],
      ),
    )
    const imported = new TextDecoder().decode(
      compiled.files.get(bySource.get("tasks/import.ts")),
    )
    const required = new TextDecoder().decode(
      compiled.files.get(bySource.get("tasks/require.cjs")),
    )
    expect(imported).toContain(
      resolve(root, "node_modules/conditional-package/import.mjs"),
    )
    expect(required).toContain(
      resolve(root, "node_modules/conditional-package/require.cjs"),
    )
  })

  test("bundles workspace source and externalizes installed packages", async () => {
    const root = await project()
    await source(
      root,
      "packages/local/package.json",
      JSON.stringify({
        name: "@example/local",
        type: "module",
        exports: "./index.ts",
      }),
    )
    await source(
      root,
      "packages/local/index.ts",
      'export const local = "workspace"\n',
    )
    await mkdir(resolve(root, "node_modules/@example"), { recursive: true })
    await symlink(
      resolve(root, "packages/local"),
      resolve(root, "node_modules/@example/local"),
      "dir",
    )
    await source(
      root,
      "node_modules/registry-package/package.json",
      JSON.stringify({
        name: "registry-package",
        type: "module",
        exports: "./index.mjs",
      }),
    )
    await source(
      root,
      "node_modules/registry-package/index.mjs",
      'export const registry = "registry"\n',
    )
    await source(
      root,
      "tasks/example.ts",
      [
        'import { local } from "@example/local"',
        'import { registry } from "registry-package"',
        'import { task } from "@helmr/sdk"',
        'export const declaration = task({ id: "dependencies", run: () => local + registry })',
      ].join("\n"),
    )

    const compiled = await compile(root)
    const modulePath = [...compiled.files.keys()].find(
      (path) =>
        path.startsWith("tasks/.helmr/modules/") && path.endsWith(".mjs"),
    )
    expect(modulePath).toBeDefined()
    const output = new TextDecoder().decode(compiled.files.get(modulePath!))
    expect(output).toContain("workspace")
    expect(output).toContain(
      resolve(root, "node_modules/registry-package/index.mjs"),
    )
  })

  test("bundles copied file dependencies from the installed-tree local map", async () => {
    const root = await project()
    await source(
      root,
      "package.json",
      JSON.stringify({
        dependencies: { "@example/local": "file:packages/local" },
        type: "module",
      }),
    )
    const manifest = JSON.stringify({
      name: "@example/local",
      type: "module",
      exports: "./index.ts",
    })
    await source(root, "packages/local/package.json", manifest)
    await source(
      root,
      "packages/local/index.ts",
      'export const local = "copied-local-package"\n',
    )
    await source(root, "node_modules/@example/local/package.json", manifest)
    await source(
      root,
      "node_modules/@example/local/index.ts",
      'export const local = "copied-local-package"\n',
    )
    await source(
      root,
      "tasks/example.ts",
      [
        'import { local } from "@example/local"',
        'import { task } from "@helmr/sdk"',
        'export const declaration = task({ id: "copied-local", run: () => local })',
      ].join("\n"),
    )

    const compiled = await compile(root)
    const manifestRaw = JSON.parse(
      new TextDecoder().decode(compiled.files.get("helmr/compiler-result.json")),
    )
    expect(manifestRaw.localPackages).toEqual([{
      installedRoot: "node_modules/@example/local",
      name: "@example/local",
      sourceRoot: "packages/local",
    }])
    expect(
      manifestRaw.inputs.some(
        (input: { path: string }) =>
          input.path === "node_modules/@example/local/index.ts",
      ),
    ).toBe(true)
  })

  test("bundles a workspace installed as a copy", async () => {
    const root = await project()
    await source(
      root,
      "package.json",
      JSON.stringify({
        dependencies: { "@example/workspace": "workspace:*" },
        name: "workspace-root",
        private: true,
        type: "module",
        workspaces: ["packages/*"],
      }),
    )
    const manifest = JSON.stringify({
      name: "@example/workspace",
      type: "module",
      exports: "./index.ts",
    })
    await source(root, "packages/workspace/package.json", manifest)
    await source(
      root,
      "packages/workspace/index.ts",
      'export const local = "copied-workspace"\n',
    )
    await source(root, "node_modules/@example/workspace/package.json", manifest)
    await source(
      root,
      "node_modules/@example/workspace/index.ts",
      'export const local = "copied-workspace"\n',
    )
    await source(
      root,
      "tasks/example.ts",
      [
        'import { local } from "@example/workspace"',
        'import { task } from "@helmr/sdk"',
        'export const declaration = task({ id: "copied-workspace", run: () => local })',
      ].join("\n"),
    )

    const compiled = await compile(root)
    const result = JSON.parse(
      new TextDecoder().decode(compiled.files.get("helmr/compiler-result.json")),
    )
    expect(result.localPackages).toEqual([{
      installedRoot: "node_modules/@example/workspace",
      name: "@example/workspace",
      sourceRoot: "packages/workspace",
    }])
    const output = new TextDecoder().decode(
      compiled.files.get(result.outputs[0].modulePath),
    )
    expect(output).toContain("copied-workspace")
    expect(output).not.toContain(
      resolve(root, "node_modules/@example/workspace/index.ts"),
    )
  })

  test("uses project tsconfig paths and records exact compiler authority", async () => {
    const root = await project()
    await source(
      root,
      "node_modules/@example/tsconfig/package.json",
      JSON.stringify({
        name: "@example/tsconfig",
        exports: { "./base.json": "./base.json" },
      }),
    )
    await source(
      root,
      "node_modules/@example/tsconfig/base.json",
      '{"extends":"./shared.json","compilerOptions":{"target":"ES2022"}}',
    )
    await source(
      root,
      "node_modules/@example/tsconfig/shared.json",
      '{"compilerOptions":{"strict":true}}',
    )
    await source(
      root,
      "tsconfig.json",
      JSON.stringify({
        extends: "@example/tsconfig/base.json",
        compilerOptions: {
          baseUrl: ".",
          paths: { "#local/*": ["lib/*"] },
        },
      }),
    )
    await source(root, "lib/value.ts", 'export const value = "aliased"\n')
    await source(
      root,
      "tasks/example.ts",
      ['import { value } from "#local/value"', "void value", task("paths")].join(
        "\n",
      ),
    )

    const compiled = await compile(root)
    const manifest = JSON.parse(
      new TextDecoder().decode(compiled.files.get("helmr/compiler-result.json")),
    )
    expect(manifest.compiler.esbuildVersion).toBe(ESBUILD_VERSION)
    expect(manifest.compiler.optionsContractDigest).toMatch(
      /^sha256:[0-9a-f]{64}$/,
    )
    expect(manifest.execution).toEqual({
      nodeVersion: "24.16.0",
      optionsDigest: compiled.optionsDigest,
    })
    expect(manifest.tsconfigs).toEqual([
      {
        digest: expect.stringMatching(/^sha256:[0-9a-f]{64}$/),
        path: "node_modules/@example/tsconfig/base.json",
      },
      {
        digest: expect.stringMatching(/^sha256:[0-9a-f]{64}$/),
        path: "node_modules/@example/tsconfig/shared.json",
      },
      {
        digest: expect.stringMatching(/^sha256:[0-9a-f]{64}$/),
        path: "tsconfig.json",
      },
    ])
  })

  test("preserves computed runtime load semantics", async () => {
    const dynamicRoot = await project()
    await source(
      dynamicRoot,
      "tasks/example.ts",
      [
        'import { task } from "@helmr/sdk"',
        'const target = "node:fs"',
        'export const declaration = task({ id: "dynamic", run: () => import(target) })',
      ].join("\n"),
    )
    await expect(compile(dynamicRoot)).resolves.toBeDefined()

    const requireRoot = await project()
    await source(
      requireRoot,
      "tasks/example.ts",
      [
        'import { task } from "@helmr/sdk"',
        'const target = "node:fs"',
        'export const declaration = task({ id: "require", run: () => require(target) })',
      ].join("\n"),
    )
    await expect(compile(requireRoot)).resolves.toBeDefined()

    const aliasedRoot = await project()
    await source(aliasedRoot, "tasks/raw.js", 'export const raw = "raw"\n')
    await source(
      aliasedRoot,
      "tasks/example.cjs",
      [
        'const { task } = require("@helmr/sdk")',
        "const load = require",
        'exports.declaration = task({ id: "aliased-require", run: () => load("./raw.js") })',
      ].join("\n"),
    )
    await expect(compile(aliasedRoot)).resolves.toBeDefined()

    const createRequireRoot = await project()
    await source(
      createRequireRoot,
      "tasks/example.ts",
      [
        'import { createRequire } from "node:module"',
        'import { task } from "@helmr/sdk"',
        "const load = createRequire(import.meta.url)",
        'export const declaration = task({ id: "create-require", run: () => load("node:path") })',
      ].join("\n"),
    )
    await expect(compile(createRequireRoot)).resolves.toBeDefined()

    const functionRoot = await project()
    await source(
      functionRoot,
      "tasks/example.ts",
      [
        'import { task } from "@helmr/sdk"',
        "function require(value: string) { return value }",
        'export const declaration = task({ id: "ordinary-call", run: () => require("value") })',
      ].join("\n"),
    )
    await expect(compile(functionRoot)).resolves.toBeDefined()
  })

  test("binds the exact Managed Node target into compiler authority", async () => {
    const firstRoot = await project()
    await source(firstRoot, "tasks/example.ts", task("node-target"))
    const first = await compileProgram({
      architecture: "x86_64",
      config: { dirs: ["tasks"], ignorePatterns: [] },
      nodeVersion: "22.22.0",
      outputRoot: await output(),
      root: firstRoot,
      runtimeRoot: firstRoot,
    })

    const secondRoot = await project()
    await source(secondRoot, "tasks/example.ts", task("node-target"))
    const second = await compileProgram({
      architecture: "x86_64",
      config: { dirs: ["tasks"], ignorePatterns: [] },
      nodeVersion: "24.16.0",
      outputRoot: await output(),
      root: secondRoot,
      runtimeRoot: secondRoot,
    })

    expect(first.optionsDigest).not.toBe(second.optionsDigest)
    const invalidRoot = await project()
    await source(invalidRoot, "tasks/example.ts", task("invalid-target"))
    await expect(compileProgram({
      architecture: "x86_64",
      config: { dirs: ["tasks"], ignorePatterns: [] },
      nodeVersion: "24",
      outputRoot: await output(),
      root: invalidRoot,
    })).rejects.toThrow("exact canonical SemVer")
  })
})

async function compile(root: string) {
  return compileProgram({
    architecture: "x86_64",
    config: { dirs: ["tasks"], ignorePatterns: [] },
    nodeVersion: "24.16.0",
    outputRoot: await output(),
    root,
    runtimeRoot: root,
  })
}

async function output(): Promise<string> {
  const root = await mkdtemp(resolve(tmpdir(), "helmr-compiler-output-"))
  cleanup.push(root)
  return root
}

async function project(): Promise<string> {
  const root = await mkdtemp(resolve(tmpdir(), "helmr-compiler-"))
  cleanup.push(root)
  await mkdir(resolve(root, "tasks"), { recursive: true })
  await source(
    root,
    "package.json",
    JSON.stringify({ name: "compiler-fixture", private: true, type: "module" }),
  )
  await mkdir(resolve(root, "node_modules/@helmr/sdk"), { recursive: true })
  await source(
    root,
    "node_modules/@helmr/sdk/package.json",
    JSON.stringify({
      name: "@helmr/sdk",
      type: "module",
      exports: {
        import: "./index.mjs",
        require: "./index.cjs",
      },
    }),
  )
  await source(
    root,
    "node_modules/@helmr/sdk/index.mjs",
    [
      'const brand = Symbol.for("helmr.sdk.v0.definition")',
      'const sandboxBrand = Symbol.for("helmr.sdk.v0.sandbox")',
      "const scheduledPayload = Object.freeze({",
      '  "~standard": Object.freeze({ version: 1, vendor: "fixture", validate: (value) => ({ value }) }),',
      "})",
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
      "export function sandbox(config) {",
      "  return Object.freeze({",
      "    id: config.id,",
      "    internal: Object.freeze({",
      '      kind: "sandbox",',
      "      id: config.id,",
      '      image: Object.freeze({ key: `sandbox/${config.id}`, steps: Object.freeze([{ kind: "from", ref: "debian:bookworm-slim" }]) }),',
      '      resources: Object.freeze({ cpu: 1, memory: "1GiB" }),',
      "    }),",
      "    [sandboxBrand]: true,",
      "  })",
      "}",
      "export const schedules = Object.freeze({",
      "  task(config) {",
      "    return Object.freeze({",
      "      [brand]: Object.freeze({",
      '        kind: "task",',
      "        id: config.id,",
      "        hasPayload: true,",
      "        handler: config.run,",
      "        payloadSchema: scheduledPayload,",
      "        schedule: Object.freeze({",
      "          cron: config.cron.pattern,",
      "          timezone: config.cron.timezone,",
      "          workspace: Object.freeze({ sandbox: config.workspace.sandbox, secrets: Object.freeze([]) }),",
      "        }),",
      "      }),",
      "    })",
      "  },",
      "})",
    ].join("\n"),
  )
  await source(
    root,
    "node_modules/@helmr/sdk/index.cjs",
    [
      'const brand = Symbol.for("helmr.sdk.v0.definition")',
      "exports.task = function task(config) {",
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
  return root
}

async function source(root: string, path: string, contents: string) {
  const target = resolve(root, path)
  await mkdir(dirname(target), { recursive: true })
  await writeFile(target, contents)
}

function task(id: string): string {
  return [
    'import { task } from "@helmr/sdk"',
    `export const declaration = task({ id: ${JSON.stringify(id)}, run: () => null })`,
  ].join("\n")
}

function commonjsTask(id: string): string {
  return [
    'const { task } = require("@helmr/sdk")',
    `exports.declaration = task({ id: ${JSON.stringify(id)}, run: () => null })`,
  ].join("\n")
}

const cleanup: string[] = []
afterAll(async () => {
  await Promise.all(cleanup.map((root) => rm(root, { force: true, recursive: true })))
})
