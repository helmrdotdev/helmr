import { describe, expect, test } from "bun:test"
import { createHash } from "node:crypto"
import { readFile } from "node:fs/promises"
import { resolve } from "node:path"

import { canonicalizeJson } from "./jsoncanon"
import {
  canonicalPackageGraph,
  PACKAGE_GRAPH_FORMAT_VERSION,
  parsePackageGraph,
  type PackageGraph,
} from "./package-graph"

interface PackageGraphFixture {
  readonly packageGraph: {
    readonly canonical: string
    readonly digestHex: string
    readonly rootOnlyCanonical: string
    readonly rootOnlyDigestHex: string
  }
  readonly packageGraphRejections: readonly {
    readonly name: string
    readonly mutation: string
  }[]
  readonly packageGraphRawRejections: readonly {
    readonly name: string
    readonly mutation: string
  }[]
}

const decoder = new TextDecoder()

describe("deployment package graph", async () => {
  const fixture = await loadFixture()

  test("matches the shared canonical fixture", () => {
    const graph = parsePackageGraph(fixture.packageGraph.canonical)
    const canonical = canonicalPackageGraph(graph)
    expect(decoder.decode(canonical)).toBe(fixture.packageGraph.canonical)
    expect(createHash("sha256").update(canonical).digest("hex")).toBe(fixture.packageGraph.digestHex)
  })

  test("rejects the shared invalid mutations", () => {
    for (const item of fixture.packageGraphRejections) {
      const value = packageGraphFixtureValue(fixture.packageGraph.canonical)
      const locals = value["localPackages"] as Array<Record<string, unknown>>
      const registries = value["registryPackages"] as Array<Record<string, unknown>>
      const resolutions = value["resolutions"] as Array<Record<string, unknown>>
      const root = locals[0]!
      const local = locals[1]!
      const registry = registries[0]!
      switch (item.mutation) {
        case "missing_format_version":
          delete value["formatVersion"]
          break
        case "unknown_root_member":
          value["unknown"] = true
          break
        case "unknown_local_member":
          local["unknown"] = true
          break
        case "unknown_registry_member":
          registry["unknown"] = true
          break
        case "unknown_resolution_member":
          resolutions[0]!["unknown"] = true
          break
        case "unknown_endpoint_member":
          ;(resolutions[0]!["from"] as Record<string, unknown>)["unknown"] = true
          break
        case "root_not_first":
          ;[locals[0], locals[1]] = [locals[1]!, locals[0]!]
          break
        case "duplicate_root":
          locals.push(structuredClone(root))
          break
        case "local_order":
          locals.push(localFixture("packages/alpha", null, null))
          break
        case "overlapping_local_path":
          locals.push(localFixture("packages/shared/nested", null, null))
          break
        case "non_adjacent_overlapping_local_path":
          value["localPackages"] = [
            locals[0],
            localFixture("a", null, null),
            localFixture("a-", null, null),
            localFixture("a/b", null, null),
            locals[1],
          ]
          break
        case "reserved_local_path":
          setFixtureLocalPath(value, "packages/shared", "helmr/shared")
          break
        case "absolute_local_path":
          setFixtureLocalPath(value, "packages/shared", "/packages/shared")
          break
        case "local_view_key":
          local["viewKey"] = "0".repeat(64)
          break
        case "root_view_key":
          root["viewKey"] = "0".repeat(64)
          break
        case "ambiguous_local_name":
          local["name"] = "app"
          break
        case "registry_order":
          ;[registries[0], registries[1]] = [registries[1]!, registries[0]!]
          break
        case "duplicate_registry_path":
          registries[1]!["installPath"] = registry["installPath"]
          break
        case "reserved_registry_path":
          registry["installPath"] = ".helmr/package"
          break
        case "registry_integrity":
          registry["integrity"] = "sha512-invalid"
          break
        case "registry_name":
          registry["name"] = "Invalid"
          break
        case "empty_registry_version":
          registry["version"] = ""
          break
        case "resolution_order":
          ;[resolutions[0], resolutions[1]] = [resolutions[1]!, resolutions[0]!]
          break
        case "resolution_relationship":
          resolutions[0]!["relationship"] = "development"
          break
        case "resolution_dependency":
          resolutions[0]!["dependency"] = "Invalid"
          break
        case "missing_from_node":
          ;(resolutions[0]!["from"] as Record<string, unknown>)["path"] = "packages/missing"
          break
        case "missing_to_node":
          ;(resolutions[0]!["to"] as Record<string, unknown>)["installPath"] = "missing"
          break
        case "unnamed_local_target":
          local["name"] = null
          break
        case "local_target_alias":
          resolutions[1]!["dependency"] = "alias"
          break
        case "mixed_endpoint_shape":
          ;(resolutions[0]!["from"] as Record<string, unknown>)["installPath"] = "zod"
          break
        case "oversized_path_component":
          setFixtureLocalPath(value, "packages/shared", `packages/${"a".repeat(256)}`)
          break
        case "oversized_mounted_path":
          setFixtureLocalPath(value, "packages/shared", pathWithLength(4096 - programMountPath.length - 1))
          break
        default:
          throw new Error(`unknown fixture mutation ${item.mutation}`)
      }
      expectPackageGraphRejection(value, item.name)
    }
  })

  test("rejects missing, null, and duplicate members", () => {
    for (const member of ["formatVersion", "localPackages", "registryPackages", "resolutions"]) {
      for (const mode of ["missing", "null"] as const) {
        const value = packageGraphFixtureValue(fixture.packageGraph.canonical)
        if (mode === "missing") delete value[member]
        else value[member] = null
        expectPackageGraphRejection(value, `${mode} root ${member}`)
      }
    }

    const objects = [
      { array: "localPackages", position: 1, members: ["manifestDigest", "name", "path", "version", "viewKey"] },
      { array: "registryPackages", position: 0, members: ["installPath", "integrity", "name", "version"] },
      { array: "resolutions", position: 0, members: ["dependency", "from", "relationship", "to"] },
    ] as const
    for (const object of objects) {
      for (const member of object.members) {
        const missing = packageGraphFixtureValue(fixture.packageGraph.canonical)
        const missingEntry = (missing[object.array] as Array<Record<string, unknown>>)[object.position]!
        delete missingEntry[member]
        expectPackageGraphRejection(missing, `missing ${object.array}.${member}`)

        if (object.array !== "localPackages" || (member !== "name" && member !== "version")) {
          const nullValue = packageGraphFixtureValue(fixture.packageGraph.canonical)
          const nullEntry = (nullValue[object.array] as Array<Record<string, unknown>>)[object.position]!
          nullEntry[member] = null
          expectPackageGraphRejection(nullValue, `null ${object.array}.${member}`)
        }
      }
    }

    for (const endpointName of ["from", "to"]) {
      for (const member of ["kind", "path"]) {
        for (const mode of ["missing", "null"] as const) {
          const value = packageGraphFixtureValue(fixture.packageGraph.canonical)
          const resolution = (value["resolutions"] as Array<Record<string, unknown>>)[1]!
          const endpoint = resolution[endpointName] as Record<string, unknown>
          if (mode === "missing") delete endpoint[member]
          else endpoint[member] = null
          expectPackageGraphRejection(value, `${mode} ${endpointName}.${member}`)
        }
      }
    }

    for (const item of fixture.packageGraphRawRejections) {
      let raw = fixture.packageGraph.canonical
      switch (item.mutation) {
        case "duplicate_root_member":
          raw = raw.replace('"formatVersion":0', '"formatVersion":0,"formatVersion":0')
          break
        case "duplicate_local_member":
          raw = raw.replace(
            '"manifestDigest":"sha256:000',
            `"manifestDigest":"sha256:${"0".repeat(64)}","manifestDigest":"sha256:000`,
          )
          break
        case "duplicate_endpoint_member":
          raw = raw.replace('"from":{"kind":"local"', '"from":{"kind":"local","kind":"local"')
          break
        default:
          throw new Error(`unknown raw fixture mutation ${item.mutation}`)
      }
      expect(() => parsePackageGraph(raw), item.name).toThrow(/duplicate/)
    }
  })

  test("accepts root-only graphs, cycles, and opaque version strings", () => {
    const rootOnly: PackageGraph = {
      formatVersion: PACKAGE_GRAPH_FORMAT_VERSION,
      localPackages: [
        {
          manifestDigest: `sha256:${"0".repeat(64)}`,
          name: null,
          path: ".",
          version: null,
          viewKey: null,
        },
      ],
      registryPackages: [],
      resolutions: [],
    }
    const canonical = canonicalPackageGraph(rootOnly)
    expect(decoder.decode(canonical)).toBe(fixture.packageGraph.rootOnlyCanonical)
    expect(createHash("sha256").update(canonical).digest("hex")).toBe(
      fixture.packageGraph.rootOnlyDigestHex,
    )
    expect(() => parsePackageGraph(canonical)).not.toThrow()

    const value = packageGraphFixtureValue(fixture.packageGraph.canonical)
    ;(value["localPackages"] as Array<Record<string, unknown>>)[1]!["version"] = "file:opaque"
    ;(value["registryPackages"] as Array<Record<string, unknown>>)[0]!["version"] = "link:opaque"
    expect(() => parsePackageGraph(canonicalizeJson(JSON.stringify(value)))).not.toThrow()
  })

  test("uses unsigned UTF-8 path order without Unicode normalization", () => {
    const paths = ["packages/e\u0301", "packages/é", "packages/\ue000", "packages/😀"]
    const graph: PackageGraph = {
      formatVersion: PACKAGE_GRAPH_FORMAT_VERSION,
      localPackages: [
        {
          manifestDigest: `sha256:${"0".repeat(64)}`,
          name: null,
          path: ".",
          version: null,
          viewKey: null,
        },
        ...paths.map((path, position) => ({
          manifestDigest: `sha256:${String(position + 1).repeat(64)}`,
          name: null,
          path,
          version: null,
          viewKey: fixtureViewKey(path),
        })),
      ],
      registryPackages: [],
      resolutions: [],
    }
    expect(() => parsePackageGraph(canonicalPackageGraph(graph))).not.toThrow()
  })

  test("enforces scalar, component, and mounted path bounds", () => {
    const value = packageGraphFixtureValue(fixture.packageGraph.canonical)
    const root = (value["localPackages"] as Array<Record<string, unknown>>)[0]!
    root["name"] = "a".repeat(214)
    root["version"] = "v".repeat(255)
    expect(() => parsePackageGraph(canonicalizeJson(JSON.stringify(value)))).not.toThrow()

    root["name"] = "a".repeat(215)
    expectPackageGraphRejection(value, "oversized name")
    root["name"] = "app"
    root["version"] = "v".repeat(256)
    expectPackageGraphRejection(value, "oversized version")

    const acceptedPath = pathWithLength(4096 - programMountPath.length - 2)
    setFixtureLocalPath(value, "packages/shared", acceptedPath)
    root["version"] = null
    expect(() => parsePackageGraph(canonicalizeJson(JSON.stringify(value)))).not.toThrow()
  })

  test("rejects non-canonical, empty, and oversized input", () => {
    expect(() => parsePackageGraph(` ${fixture.packageGraph.canonical}`)).toThrow(/canonical/)
    expect(() => parsePackageGraph(new Uint8Array())).toThrow(/size/)
    expect(() => parsePackageGraph(new Uint8Array(16777217))).toThrow(/size/)
  })
})

const programMountPath = "/opt/helmr/program"

async function loadFixture(): Promise<PackageGraphFixture> {
  const path = resolve(import.meta.dir, "../../../../fixtures/contracts/deployment-v0/golden.json")
  return JSON.parse(await readFile(path, "utf8")) as PackageGraphFixture
}

function packageGraphFixtureValue(canonical: string): Record<string, unknown> {
  return JSON.parse(canonical) as Record<string, unknown>
}

function expectPackageGraphRejection(value: Record<string, unknown>, label: string): void {
  const canonical = canonicalizeJson(JSON.stringify(value))
  expect(() => parsePackageGraph(canonical), label).toThrow()
}

function setFixtureLocalPath(
  value: Record<string, unknown>,
  oldPath: string,
  newPath: string,
): void {
  for (const local of value["localPackages"] as Array<Record<string, unknown>>) {
    if (local["path"] === oldPath) {
      local["path"] = newPath
      local["viewKey"] = fixtureViewKey(newPath)
    }
  }
  for (const resolution of value["resolutions"] as Array<Record<string, unknown>>) {
    for (const endpointName of ["from", "to"]) {
      const endpoint = resolution[endpointName] as Record<string, unknown>
      if (endpoint["kind"] === "local" && endpoint["path"] === oldPath) {
        endpoint["path"] = newPath
      }
    }
  }
}

function localFixture(path: string, name: string | null, version: string | null): Record<string, unknown> {
  return {
    manifestDigest: `sha256:${"2".repeat(64)}`,
    name,
    path,
    version,
    viewKey: fixtureViewKey(path),
  }
}

function fixtureViewKey(path: string): string {
  return createHash("sha256")
    .update("helmr.local-package-view.v0")
    .update(new Uint8Array([0]))
    .update(path)
    .digest("hex")
}

function pathWithLength(length: number): string {
  let path = ""
  while (path.length < length) {
    if (path.length > 0) path += "/"
    path += "z".repeat(Math.min(255, length - path.length))
  }
  return path
}
