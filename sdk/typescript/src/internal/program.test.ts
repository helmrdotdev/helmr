import { describe, expect, test } from "bun:test"
import { readFile } from "node:fs/promises"
import { resolve } from "node:path"

import { canonicalizeJson } from "./jsoncanon"
import {
  canonicalManifestAndDigest,
  canonicalProgramIndex,
  currentProgramRuntimeAbi,
  parseProgramIndex,
  programRuntimeAbiDigest,
  validateCurrentProgramRuntimeAbi,
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
  readonly runtimeAbi: {
    readonly canonical: string
    readonly digestHex: string
  }
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
    expect(index.declarations.filter((item) => item.kind === "task").map((item) => item.declaredId)).toEqual([
      "Build-",
      "Build.",
      "Build0",
      "BuildA",
      "Build_",
      "Builda",
    ])
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
          [index.declarations[0], index.declarations[1]] = [index.declarations[1], index.declarations[0]]
          break
        case "task_slots":
          index.declarations[0] = { ...index.declarations[0], slots: ["payloadSchema", "handler"] }
          break
        case "duplicate_declaration":
          index.declarations[1] = structuredClone(index.declarations[0])
          break
        case "runtime_abi":
          index.runtimeContract.runtimeApiVersion = "helmr.runtime-api.v2"
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

  test("rejects every single-field ABI mismatch", () => {
    for (const field of ["bundleFormatVersion", "runtimeApiVersion", "checkpointProtocolVersion"] as const) {
      const abi = { ...currentProgramRuntimeAbi, [field]: `${currentProgramRuntimeAbi[field]}.mismatch` }
      expect(() => validateCurrentProgramRuntimeAbi(abi), field).toThrow()
    }

    const abiWithUnknownMember = { ...currentProgramRuntimeAbi, unknown: "value" }
    expect(() => validateCurrentProgramRuntimeAbi(abiWithUnknownMember)).toThrow(/unknown or missing members/)
    expect(() => programRuntimeAbiDigest(abiWithUnknownMember)).toThrow(/unknown or missing members/)
  })

  test("matches the shared ABI and manifest digests", () => {
    expect(toHex(programRuntimeAbiDigest(currentProgramRuntimeAbi))).toBe(fixture.runtimeAbi.digestHex)
    expect(decoder.decode(canonicalizeJson(JSON.stringify(currentProgramRuntimeAbi)))).toBe(fixture.runtimeAbi.canonical)

    const manifest = canonicalManifestAndDigest(fixture.manifest.input)
    expect(decoder.decode(manifest.canonical)).toBe(fixture.manifest.canonical)
    expect(toHex(manifest.digest)).toBe(fixture.manifest.digestHex)
  })
})

type MutableProgramIndex = {
  formatVersion: number
  runtimeContract: {
    bundleFormatVersion: string
    runtimeApiVersion: string
    checkpointProtocolVersion: string
  }
  supportedArchitectures: string[]
  declarations: Array<{
    kind: string
    declaredId: string
    slots: string[]
  }>
}

async function loadFixture(): Promise<GoldenFixture> {
  const path = resolve(import.meta.dir, "../../../../fixtures/contracts/deployment-v0/golden.json")
  return JSON.parse(await readFile(path, "utf8")) as GoldenFixture
}

function toHex(value: Uint8Array): string {
  return Buffer.from(value).toString("hex")
}
