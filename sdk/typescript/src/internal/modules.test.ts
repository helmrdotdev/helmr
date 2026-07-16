import { describe, expect, test } from "bun:test"
import { createHash } from "node:crypto"
import { readFile } from "node:fs/promises"
import { resolve } from "node:path"

import { canonicalizeJson } from "./jsoncanon"
import {
  canonicalModuleMap,
  MODULE_MAP_FORMAT_VERSION,
  parseModuleMap,
  TYPESCRIPT_TRANSFORMER,
  validateModuleMap,
  type ModuleMap,
} from "./modules"

interface ModuleMapFixture {
  readonly moduleMap: { readonly canonical: string }
  readonly moduleMapRejections: readonly {
    readonly name: string
    readonly mutation: string
  }[]
}

const decoder = new TextDecoder()

describe("deployment module map", async () => {
  const fixture = await loadFixture()

  test("matches the shared canonical fixture and unsigned UTF-8 path order", () => {
    const moduleMap = parseModuleMap(fixture.moduleMap.canonical)
    expect(decoder.decode(canonicalModuleMap(moduleMap))).toBe(fixture.moduleMap.canonical)
    expect(moduleMap.modules.slice(2).map((module) => module.path)).toEqual([
      "packages/shared/src/\ue000.ts",
      "packages/shared/src/😀.ts",
    ])
  })

  test("rejects the shared invalid mutations", () => {
    for (const item of fixture.moduleMapRejections) {
      const value = JSON.parse(fixture.moduleMap.canonical) as Record<string, unknown>
      const modules = value["modules"] as Array<Record<string, unknown>>
      const first = modules[0] as Record<string, unknown>
      switch (item.mutation) {
        case "missing_format_version":
          delete value["formatVersion"]
          break
        case "unknown_root_member":
          value["unknown"] = true
          break
        case "transformer":
          value["transformer"] = "helmr.typescript.v1"
          break
        case "module_order":
          ;[modules[0], modules[1]] = [modules[1] as Record<string, unknown>, modules[0] as Record<string, unknown>]
          break
        case "duplicate_path":
          modules[1] = { ...first }
          break
        case "absolute_path":
          setFixtureModulePath(first, "/packages/shared/src/legacy.cts")
          break
        case "escaping_path":
          setFixtureModulePath(first, "packages/../src/legacy.cts")
          break
        case "backslash_path":
          setFixtureModulePath(first, "packages\\shared\\src\\legacy.cts")
          break
        case "reserved_helmr_root":
          setFixtureModulePath(first, "helmr/legacy.cts")
          break
        case "reserved_dot_helmr_root":
          setFixtureModulePath(first, ".helmr/legacy.cts")
          break
        case "reserved_node_modules_root":
          setFixtureModulePath(first, "node_modules/legacy.cts")
          break
        case "declaration_path":
          setFixtureModulePath(first, "packages/shared/src/legacy.d.cts")
          break
        case "unsupported_extension":
          setFixtureModulePath(first, "packages/shared/src/legacy.tsx")
          break
        case "source_digest":
          first["sourceDigest"] = "sha256:invalid"
          break
        case "code_digest":
          first["codeDigest"] = "sha256:invalid"
          break
        case "format":
          first["format"] = "module"
          setFixtureModulePath(first, first["path"] as string)
          break
        case "code_path_key":
          first["codePath"] = `helmr/files/modules/${"0".repeat(64)}.cjs`
          break
        case "code_path_extension":
          first["codePath"] = (first["codePath"] as string).replace(/\.cjs$/, ".mjs")
          break
        case "unknown_module_member":
          first["unknown"] = true
          break
        default:
          throw new Error(`unknown fixture mutation ${item.mutation}`)
      }
      const canonical = canonicalizeJson(JSON.stringify(value))
      expect(() => parseModuleMap(canonical), item.name).toThrow()
    }
  })

  test("accepts an empty module array for an all-JavaScript program", () => {
    const moduleMap: ModuleMap = {
      formatVersion: MODULE_MAP_FORMAT_VERSION,
      modules: [],
      transformer: TYPESCRIPT_TRANSFORMER,
    }
    expect(() => parseModuleMap(canonicalModuleMap(moduleMap))).not.toThrow()
  })

  test("rejects non-canonical, empty, and oversized input", () => {
    expect(() => parseModuleMap(` ${fixture.moduleMap.canonical}`)).toThrow(/canonical/)
    expect(() => parseModuleMap(new Uint8Array())).toThrow(/size/)
    expect(() => parseModuleMap(new Uint8Array(16777217))).toThrow(/size/)
  })

  test("rejects a string containing an unpaired surrogate instead of repairing it", () => {
    const path = "packages/shared/src/\ufffd.ts"
    const valid: ModuleMap = {
      formatVersion: MODULE_MAP_FORMAT_VERSION,
      modules: [
        {
          codeDigest: `sha256:${"a".repeat(64)}`,
          codePath: fixtureModuleCodePath(path, "module"),
          format: "module",
          path,
          sourceDigest: `sha256:${"1".repeat(64)}`,
        },
      ],
      transformer: TYPESCRIPT_TRANSFORMER,
    }
    const repairedCanonical = decoder.decode(canonicalModuleMap(valid))
    expect(() => parseModuleMap(repairedCanonical.replace("\ufffd", "\ud800"))).toThrow(/surrogate/)
  })

  test("rejects more than 65,536 entries", () => {
    const moduleMap = {
      formatVersion: MODULE_MAP_FORMAT_VERSION,
      modules: Array.from({ length: 65537 }, () => null),
      transformer: TYPESCRIPT_TRANSFORMER,
    } as unknown as ModuleMap
    expect(() => validateModuleMap(moduleMap)).toThrow(/maximum/)
  })
})

async function loadFixture(): Promise<ModuleMapFixture> {
  const path = resolve(import.meta.dir, "../../../../fixtures/contracts/deployment-v0/golden.json")
  return JSON.parse(await readFile(path, "utf8")) as ModuleMapFixture
}

function setFixtureModulePath(module: Record<string, unknown>, path: string): void {
  module["path"] = path
  module["codePath"] = fixtureModuleCodePath(path, module["format"] as string)
}

function fixtureModuleCodePath(path: string, format: string): string {
  const key = createHash("sha256")
    .update("helmr.typescript-module.v0")
    .update(new Uint8Array([0]))
    .update(path)
    .digest("hex")
  return `helmr/files/modules/${key}.${format === "module" ? "mjs" : "cjs"}`
}
