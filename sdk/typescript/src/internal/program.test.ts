import { describe, expect, test } from "bun:test"
import { readFile } from "node:fs/promises"
import { resolve } from "node:path"

import { canonicalizeJson } from "./jsoncanon"
import {
  canonicalManifestAndDigest,
  canonicalProgramIndex,
  parseProgramIndex,
  validateProgramIndex,
  type ProgramIndex,
} from "./program"

interface GoldenFixture {
  readonly canonicalization: readonly {
    readonly name: string
    readonly input: string
    readonly canonical: string
  }[]
  readonly canonicalRejections: readonly {
    readonly name: string
    readonly inputHex: string
  }[]
  readonly programIndex: { readonly canonical: string }
  readonly programRejections: readonly {
    readonly name: string
    readonly mutation: string
  }[]
  readonly manifest: {
    readonly input: string
    readonly canonical: string
    readonly digestHex: string
  }
}

const decoder = new TextDecoder()

describe("canonical deployment contract", async () => {
  const fixture = await loadFixture()

  test("matches the shared RFC 8785 vectors", () => {
    for (const item of fixture.canonicalization) {
      expect(decoder.decode(canonicalizeJson(item.input)), item.name).toBe(item.canonical)
    }
  })

  test("rejects the shared invalid I-JSON vectors", () => {
    for (const item of fixture.canonicalRejections) {
      expect(() => canonicalizeJson(Buffer.from(item.inputHex, "hex")), item.name).toThrow()
    }
  })

  test("parses and emits the shared canonical program index", () => {
    const index = parseProgramIndex(fixture.programIndex.canonical)
    expect(decoder.decode(canonicalProgramIndex(index))).toBe(fixture.programIndex.canonical)
    expect(
      index.declarations.filter((item) => item.kind === "task").map((item) => item.declaredId),
    ).toEqual(["Build-", "Build.", "Build0", "BuildA", "Build_", "Builda"])
  })

  test("rejects the shared invalid program mutations", () => {
    const base = parseProgramIndex(fixture.programIndex.canonical)
    for (const item of fixture.programRejections) {
      const index = structuredClone(base) as MutableProgramIndex
      switch (item.mutation) {
        case "empty_declarations":
          index.declarations = []
          break
        case "missing_format_version": {
          const value = JSON.parse(fixture.programIndex.canonical) as Record<string, unknown>
          delete value["formatVersion"]
          const input = decoder.decode(canonicalizeJson(JSON.stringify(value)))
          expect(() => parseProgramIndex(input), item.name).toThrow()
          continue
        }
        case "unknown_root_member": {
          const value = JSON.parse(fixture.programIndex.canonical) as Record<string, unknown>
          value["unknown"] = true
          const input = decoder.decode(canonicalizeJson(JSON.stringify(value)))
          expect(() => parseProgramIndex(input), item.name).toThrow()
          continue
        }
        case "declaration_order":
          ;[index.declarations[0], index.declarations[1]] = [
            index.declarations[1],
            index.declarations[0],
          ]
          break
        case "task_slots":
          index.declarations[0] = {
            ...index.declarations[0],
            slots: ["payloadSchema", "handler"],
          }
          break
        case "duplicate_declaration":
          index.declarations[1] = structuredClone(index.declarations[0])
          break
        case "build_contract":
          index.buildContractVersion = "helmr.program-build.v1"
          break
        case "runtime_api":
          index.runtimeApiVersion = "helmr.runtime.v1"
          break
        case "runtime_digest":
          index.runtimeDigest = `sha256:${"A".repeat(64)}`
          break
        case "toolchain_digest":
          index.standardToolchainDigest = "sha256:invalid"
          break
        case "manager_name":
          index.manager.name = "pnpm"
          break
        case "manager_version":
          index.manager.version = "^1.3.10"
          break
        case "manager_capsule_digest":
          index.manager.capsuleDigest = "sha256:invalid"
          break
        case "lockfile_name":
          index.submitted.lockfileName = "package-lock.json"
          break
        case "lockfile_digest":
          index.submitted.lockfileDigest = "sha256:invalid"
          break
        case "source_digest":
          index.submitted.sourceDigest = "sha256:invalid"
          break
        case "architecture":
          index.architecture = "amd64"
          break
        case "declared_id":
          index.declarations[0] = {
            ...index.declarations[0],
            declaredId: "invalid/id",
          }
          break
        default:
          throw new Error(`unknown fixture mutation ${item.mutation}`)
      }
      expect(() => validateProgramIndex(index as ProgramIndex), item.name).toThrow()
    }
  })

  test("requires canonical bytes for the embedded program index", () => {
    expect(() => parseProgramIndex(` ${fixture.programIndex.canonical}`)).toThrow(/canonical/)
  })

  test("accepts Bun's binary lockfile", () => {
    const index = structuredClone(
      parseProgramIndex(fixture.programIndex.canonical),
    ) as MutableProgramIndex
    index.submitted.lockfileName = "bun.lockb"
    expect(() => validateProgramIndex(index as ProgramIndex)).not.toThrow()
  })

  test("enforces the program index size bound", () => {
    expect(() => parseProgramIndex(new Uint8Array())).toThrow(/size/)
    expect(() => parseProgramIndex(new Uint8Array(16_777_217))).toThrow(/size/)

    const index = structuredClone(
      parseProgramIndex(fixture.programIndex.canonical),
    ) as MutableProgramIndex
    index.declarations = Array.from({ length: 100_000 }, (_, position) => ({
      kind: "task",
      declaredId: `${position.toString().padStart(6, "0")}${"a".repeat(122)}`,
      slots: ["handler"],
    }))
    expect(() => canonicalProgramIndex(index as ProgramIndex)).toThrow(/size/)
  })

  test("matches the shared manifest digest", () => {
    const manifest = canonicalManifestAndDigest(fixture.manifest.input)
    expect(decoder.decode(manifest.canonical)).toBe(fixture.manifest.canonical)
    expect(toHex(manifest.digest)).toBe(fixture.manifest.digestHex)
  })
})

type MutableProgramIndex = {
  architecture: string
  buildContractVersion: string
  declarations: Array<{
    kind: string
    declaredId: string
    slots: string[]
  }>
  formatVersion: number
  manager: {
    capsuleDigest: string
    name: string
    version: string
  }
  runtimeApiVersion: string
  runtimeDigest: string
  standardToolchainDigest: string
  submitted: {
    lockfileDigest: string
    lockfileName: string
    sourceDigest: string
  }
}

async function loadFixture(): Promise<GoldenFixture> {
  const path = resolve(import.meta.dir, "../../../../fixtures/contracts/deployment-v0/golden.json")
  return JSON.parse(await readFile(path, "utf8")) as GoldenFixture
}

function toHex(value: Uint8Array): string {
  return Buffer.from(value).toString("hex")
}
