import { describe, expect, test } from "bun:test"
import { readFile } from "node:fs/promises"
import { resolve } from "node:path"

import {
  canonicalDependencyCacheInput,
  canonicalDependencyIndex,
  dependencyCacheKey,
  parseDependencyIndex,
  type DependencyCacheInput,
  type DependencyIndex,
} from "./dependencies"
import { canonicalizeJson } from "./jsoncanon"

interface DependencyIndexFixture {
  readonly dependencyIndex: { readonly canonical: string }
  readonly dependencyIndexRejections: readonly {
    readonly name: string
    readonly mutation: string
  }[]
  readonly dependencyIndexRawRejections: readonly {
    readonly name: string
    readonly mutation: string
  }[]
  readonly dependencyCacheInput: {
    readonly canonical: string
    readonly key: string
  }
  readonly dependencyCacheInputRejections: readonly {
    readonly name: string
    readonly mutation: string
  }[]
}

const decoder = new TextDecoder()

describe("deployment dependency index", async () => {
  const fixture = await loadFixture()

  test("matches the shared canonical fixture", () => {
    const index = parseDependencyIndex(fixture.dependencyIndex.canonical)
    expect(decoder.decode(canonicalDependencyIndex(index))).toBe(fixture.dependencyIndex.canonical)
  })

  test("rejects the shared invalid mutations", () => {
    for (const item of fixture.dependencyIndexRejections) {
      const value = JSON.parse(fixture.dependencyIndex.canonical) as Record<string, unknown>
      const manager = value["packageManager"] as Record<string, unknown>
      const lockfile = value["lockfile"] as Record<string, unknown>
      switch (item.mutation) {
        case "missing_format_version":
          delete value["formatVersion"]
          break
        case "unknown_root_member":
          value["unknown"] = true
          break
        case "unknown_manager_member":
          manager["unknown"] = true
          break
        case "unknown_lockfile_member":
          lockfile["unknown"] = true
          break
        case "runtime_api_member":
          value["runtimeApiVersion"] = "helmr.runtime.v0"
          break
        case "manager_name":
          manager["name"] = "pnpm"
          break
        case "manager_version_leading_v":
          manager["version"] = "v1.3.10"
          break
        case "manager_version_range":
          manager["version"] = "^1.3.10"
          break
        case "manager_version_build":
          manager["version"] = "1.3.10+build"
          break
        case "manager_version_leading_zero":
          manager["version"] = "01.3.10"
          break
        case "manager_version_prerelease_zero":
          manager["version"] = "1.3.10-01"
          break
        case "manager_version_newline":
          manager["version"] = "1.3.10\n"
          break
        case "manager_version_oversize":
          manager["version"] = `1.2.3-${"a".repeat(59)}`
          break
        case "lockfile_name":
          lockfile["name"] = "package-lock.json"
          break
        case "lockfile_digest":
          lockfile["digest"] = "sha256:invalid"
          break
        case "local_manifests_digest":
          value["localManifestsDigest"] = "sha256:invalid"
          break
        case "package_graph_digest":
          value["packageGraphDigest"] = "sha256:invalid"
          break
        case "package_graph_size_zero":
          value["packageGraphSizeBytes"] = 0
          break
        case "package_graph_size_fractional":
          value["packageGraphSizeBytes"] = 1.5
          break
        case "package_graph_size_oversize":
          value["packageGraphSizeBytes"] = 16777217
          break
        case "materializer_version":
          value["materializerVersion"] = "helmr.dependencies.v1"
          break
        case "runtime_digest":
          value["runtimeDigest"] = "sha256:invalid"
          break
        case "architecture":
          value["architecture"] = "amd64"
          break
        default:
          throw new Error(`unknown fixture mutation ${item.mutation}`)
      }
      const canonical = canonicalizeJson(JSON.stringify(value))
      expect(() => parseDependencyIndex(canonical), item.name).toThrow()
    }
  })

  test("rejects every missing or null root and nested member", () => {
    const rootMembers = [
      "architecture",
      "formatVersion",
      "localManifestsDigest",
      "lockfile",
      "materializerVersion",
      "packageGraphDigest",
      "packageGraphSizeBytes",
      "packageManager",
      "runtimeDigest",
    ] as const
    for (const member of rootMembers) {
      for (const mode of ["missing", "null"] as const) {
        const value = JSON.parse(fixture.dependencyIndex.canonical) as Record<string, unknown>
        if (mode === "missing") delete value[member]
        else value[member] = null
        expectDependencyIndexRejection(value, `${mode} root ${member}`)
      }
    }
    const nestedMembers = {
      packageManager: ["name", "version"],
      lockfile: ["name", "digest"],
    } as const
    for (const [objectName, members] of Object.entries(nestedMembers)) {
      for (const member of members) {
        for (const mode of ["missing", "null"] as const) {
          const value = JSON.parse(fixture.dependencyIndex.canonical) as Record<string, unknown>
          const nested = value[objectName] as Record<string, unknown>
          if (mode === "missing") delete nested[member]
          else nested[member] = null
          expectDependencyIndexRejection(value, `${mode} ${objectName} ${member}`)
        }
      }
    }
  })

  test("rejects shared duplicate-member inputs without relying on missing fields", () => {
    for (const item of fixture.dependencyIndexRawRejections) {
      let raw = fixture.dependencyIndex.canonical
      switch (item.mutation) {
        case "duplicate_root_member":
          raw = raw.replace('"formatVersion":0', '"formatVersion":0,"formatVersion":0')
          break
        case "duplicate_manager_member":
          raw = raw.replace('"packageManager":{"name":"bun"', '"packageManager":{"name":"bun","name":"bun"')
          break
        case "duplicate_lockfile_member":
          raw = raw.replace(
            '"lockfile":{"digest":',
            `"lockfile":{"digest":"sha256:${"1".repeat(64)}","digest":`,
          )
          break
        default:
          throw new Error(`unknown raw fixture mutation ${item.mutation}`)
      }
      expect(() => parseDependencyIndex(raw), item.name).toThrow(/duplicate/)
    }
  })

  test("accepts both manager-lockfile pairs and prerelease versions", () => {
    const bun = structuredClone(parseDependencyIndex(fixture.dependencyIndex.canonical)) as MutableDependencyIndex
    bun.packageManager.version = "1.3.10-rc.1"
    expect(() => canonicalDependencyIndex(bun as DependencyIndex)).not.toThrow()

    bun.packageManager.version = `1.2.3-${"a".repeat(58)}`
    expect(() => canonicalDependencyIndex(bun as DependencyIndex)).not.toThrow()

    bun.packageManager = { name: "npm", version: "10.9.4" }
    bun.lockfile.name = "package-lock.json"
    expect(() => parseDependencyIndex(canonicalDependencyIndex(bun as DependencyIndex))).not.toThrow()
  })

  test("rejects non-canonical, empty, and oversized input", () => {
    expect(() => parseDependencyIndex(` ${fixture.dependencyIndex.canonical}`)).toThrow(/canonical/)
    expect(() => parseDependencyIndex(new Uint8Array())).toThrow(/size/)
    expect(() => parseDependencyIndex(new Uint8Array(4097))).toThrow(/size/)
  })

  test("matches the shared dependency cache input and key", () => {
    const input = JSON.parse(fixture.dependencyCacheInput.canonical) as DependencyCacheInput
    expect(decoder.decode(canonicalDependencyCacheInput(input))).toBe(
      fixture.dependencyCacheInput.canonical,
    )
    expect(dependencyCacheKey(input)).toBe(fixture.dependencyCacheInput.key)
  })

  test("rejects shared dependency cache input mutations", () => {
    for (const item of fixture.dependencyCacheInputRejections) {
      const input = JSON.parse(fixture.dependencyCacheInput.canonical) as MutableDependencyCacheInput
      switch (item.mutation) {
        case "invalid_format_version":
          input.formatVersion = 1
          break
        case "invalid_manager":
          input.packageManager.name = "pnpm"
          break
        case "invalid_manager_version":
          input.packageManager.version = "^1.3.10"
          break
        case "mismatched_lockfile":
          input.lockfile.name = "package-lock.json"
          break
        case "invalid_lockfile_digest":
          input.lockfile.digest = "sha256:invalid"
          break
        case "invalid_local_manifests_digest":
          input.localManifestsDigest = "sha256:invalid"
          break
        case "invalid_materializer_version":
          input.materializerVersion = "helmr.dependencies.v1"
          break
        case "invalid_runtime_digest":
          input.runtimeDigest = "sha256:invalid"
          break
        case "invalid_architecture":
          input.architecture = "amd64"
          break
        default:
          throw new Error(`unknown fixture mutation ${item.mutation}`)
      }
      expect(
        () => dependencyCacheKey(input as DependencyCacheInput),
        item.name,
      ).toThrow()
    }
  })

  test("dependency cache key binds every admitted input and excludes graph output", () => {
    const base = JSON.parse(fixture.dependencyCacheInput.canonical) as MutableDependencyCacheInput
    const baseKey = dependencyCacheKey(base as DependencyCacheInput)
    const mutations: Record<string, (input: MutableDependencyCacheInput) => void> = {
      architecture: (input) => {
        input.architecture = "aarch64"
      },
      localManifests: (input) => {
        input.localManifestsDigest = `sha256:${"5".repeat(64)}`
      },
      lockfile: (input) => {
        input.lockfile.digest = `sha256:${"6".repeat(64)}`
      },
      manager: (input) => {
        input.packageManager.version = "1.3.11"
      },
      runtime: (input) => {
        input.runtimeDigest = `sha256:${"7".repeat(64)}`
      },
    }
    for (const [name, mutate] of Object.entries(mutations)) {
      const input = structuredClone(base)
      mutate(input)
      expect(dependencyCacheKey(input as DependencyCacheInput), name).not.toBe(baseKey)
    }

    const graphOutput = {
      ...base,
      packageGraphDigest: `sha256:${"8".repeat(64)}`,
      packageGraphSizeBytes: 1,
    }
    expect(() => dependencyCacheKey(graphOutput as DependencyCacheInput)).toThrow(/member/)
  })
})

type MutableDependencyIndex = {
  formatVersion: number
  packageManager: { name: string; version: string }
  lockfile: { name: string; digest: string }
  localManifestsDigest: string
  packageGraphDigest: string
  packageGraphSizeBytes: number
  materializerVersion: string
  runtimeDigest: string
  architecture: string
}

type MutableDependencyCacheInput = {
  formatVersion: number
  packageManager: { name: string; version: string }
  lockfile: { name: string; digest: string }
  localManifestsDigest: string
  materializerVersion: string
  runtimeDigest: string
  architecture: string
}

async function loadFixture(): Promise<DependencyIndexFixture> {
  const path = resolve(import.meta.dir, "../../../../fixtures/contracts/deployment-v0/golden.json")
  return JSON.parse(await readFile(path, "utf8")) as DependencyIndexFixture
}

function expectDependencyIndexRejection(value: Record<string, unknown>, label: string): void {
  const canonical = canonicalizeJson(JSON.stringify(value))
  expect(() => parseDependencyIndex(canonical), label).toThrow()
}
