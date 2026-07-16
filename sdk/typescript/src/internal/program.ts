import { createHash } from "node:crypto"

import {
  canonicalizeJsonValue,
  parseJson,
  type JsonObject,
  type JsonValue,
} from "./jsoncanon"

export const PROGRAM_INDEX_FORMAT_VERSION = 0 as const
export const PROGRAM_BUNDLE_FORMAT_VERSION = "helmr.program-bundle.v0" as const
export const RUNTIME_API_VERSION = "helmr.runtime-api.v0" as const
export const CHECKPOINT_PROTOCOL_VERSION = "helmr.checkpoint.v0" as const

const declaredIdPattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/
const manifestDigestDomain = "helmr.deployment-definition-manifest.v0\0"
const runtimeContractDigestDomain = "helmr.program-runtime-abi.v0\0"

export interface ProgramRuntimeAbi {
  readonly bundleFormatVersion: string
  readonly runtimeApiVersion: string
  readonly checkpointProtocolVersion: string
}

export const currentProgramRuntimeAbi: Readonly<ProgramRuntimeAbi> = Object.freeze({
  bundleFormatVersion: PROGRAM_BUNDLE_FORMAT_VERSION,
  runtimeApiVersion: RUNTIME_API_VERSION,
  checkpointProtocolVersion: CHECKPOINT_PROTOCOL_VERSION,
})

export type RuntimeArchitecture = "aarch64" | "x86_64"
export type ProgramDeclaration =
  | Readonly<{ kind: "task"; declaredId: string; slots: readonly ["handler"] | readonly ["handler", "payloadSchema"] }>
  | Readonly<{ kind: "actor"; declaredId: string; slots: readonly ["handler"] }>
  | Readonly<{ kind: "run_stream"; declaredId: string; slots: readonly ["schema"] }>

export interface ProgramIndex {
  readonly formatVersion: 0
  readonly runtimeContract: ProgramRuntimeAbi
  readonly supportedArchitectures: readonly RuntimeArchitecture[]
  readonly declarations: readonly ProgramDeclaration[]
}

export function parseProgramIndex(raw: string | Uint8Array): ProgramIndex {
  const input = typeof raw === "string" ? new TextEncoder().encode(raw) : raw
  const value = parseJson(input)
  const canonical = canonicalizeJsonValue(value)
  if (!bytesEqual(input, canonical)) {
    throw new Error("program index is not RFC 8785 canonical JSON")
  }
  return validateProgramIndexValue(value)
}

export function canonicalProgramIndex(index: ProgramIndex): Uint8Array {
  validateProgramIndex(index)
  return canonicalizeJsonValue(index as unknown as JsonValue)
}

export function validateProgramIndex(index: ProgramIndex): void {
  canonicalizeJsonValue(index as unknown as JsonValue)
  validateProgramIndexValue(index as unknown as JsonValue)
}

export function validateCurrentProgramRuntimeAbi(abi: ProgramRuntimeAbi): void {
  canonicalizeJsonValue(abi as unknown as JsonValue)
  const value = requireObject(abi as unknown as JsonValue, "program runtime ABI")
  requireKeys(value, ["bundleFormatVersion", "checkpointProtocolVersion", "runtimeApiVersion"], "program runtime ABI")
  if (
    value["bundleFormatVersion"] !== PROGRAM_BUNDLE_FORMAT_VERSION ||
    value["runtimeApiVersion"] !== RUNTIME_API_VERSION ||
    value["checkpointProtocolVersion"] !== CHECKPOINT_PROTOCOL_VERSION
  ) {
    throw new Error("program runtime ABI does not match the toolchain-owned v0 tuple")
  }
}

export function programRuntimeAbiDigest(abi: ProgramRuntimeAbi): Uint8Array {
  validateCurrentProgramRuntimeAbi(abi)
  const canonical = canonicalizeJsonValue(abi as unknown as JsonValue)
  return domainDigest(runtimeContractDigestDomain, canonical)
}

export function canonicalManifestAndDigest(raw: string | Uint8Array): {
  readonly canonical: Uint8Array
  readonly digest: Uint8Array
} {
  const value = parseJson(raw)
  if (!isObject(value)) {
    throw new Error("deployment manifest root must be an object")
  }
  const canonical = canonicalizeJsonValue(value)
  return { canonical, digest: domainDigest(manifestDigestDomain, canonical) }
}

function validateProgramIndexValue(value: JsonValue): ProgramIndex {
  const root = requireObject(value, "program index")
  requireKeys(root, ["declarations", "formatVersion", "runtimeContract", "supportedArchitectures"], "program index")
  if (root["formatVersion"] !== PROGRAM_INDEX_FORMAT_VERSION) {
    throw new Error(`program index formatVersion must be ${PROGRAM_INDEX_FORMAT_VERSION}`)
  }

  const runtimeContract = requireObject(root["runtimeContract"], "program index runtimeContract")
  requireKeys(runtimeContract, ["bundleFormatVersion", "checkpointProtocolVersion", "runtimeApiVersion"], "program index runtimeContract")
  const abi: ProgramRuntimeAbi = {
    bundleFormatVersion: requireString(runtimeContract["bundleFormatVersion"], "bundleFormatVersion"),
    runtimeApiVersion: requireString(runtimeContract["runtimeApiVersion"], "runtimeApiVersion"),
    checkpointProtocolVersion: requireString(runtimeContract["checkpointProtocolVersion"], "checkpointProtocolVersion"),
  }
  validateCurrentProgramRuntimeAbi(abi)

  const architectureValues = requireArray(root["supportedArchitectures"], "program index supportedArchitectures")
  const architectures = architectureValues.map((item) => requireArchitecture(item))
  if (!validArchitectures(architectures)) {
    throw new Error("program index supportedArchitectures is not a non-empty canonical architecture set")
  }

  const declarationValues = requireArray(root["declarations"], "program index declarations")
  if (declarationValues.length === 0) {
    throw new Error("program index declarations must not be empty")
  }
  const declarations = declarationValues.map((item, position) => parseDeclaration(item, position))
  for (let position = 1; position < declarations.length; position++) {
    if (compareDeclarations(declarations[position - 1] as ProgramDeclaration, declarations[position] as ProgramDeclaration) >= 0) {
      throw new Error(`program index declarations are not in canonical order at position ${position}`)
    }
  }

  return {
    formatVersion: PROGRAM_INDEX_FORMAT_VERSION,
    runtimeContract: abi,
    supportedArchitectures: architectures,
    declarations,
  }
}

function parseDeclaration(value: JsonValue, position: number): ProgramDeclaration {
  const declaration = requireObject(value, `program index declaration ${position}`)
  requireKeys(declaration, ["declaredId", "kind", "slots"], `program index declaration ${position}`)
  const kind = requireString(declaration["kind"], "declaration kind")
  const declaredId = requireString(declaration["declaredId"], "declaration declaredId")
  if (!declaredIdPattern.test(declaredId)) {
    throw new Error(`program index declaredId ${JSON.stringify(declaredId)} is outside the exact ASCII ID domain`)
  }
  const slots = requireArray(declaration["slots"], "declaration slots").map((slot) => requireString(slot, "declaration slot"))
  if (kind === "task" && (sameStrings(slots, ["handler"]) || sameStrings(slots, ["handler", "payloadSchema"]))) {
    return { kind, declaredId, slots: slots as ["handler"] | ["handler", "payloadSchema"] }
  }
  if (kind === "actor" && sameStrings(slots, ["handler"])) {
    return { kind, declaredId, slots: slots as ["handler"] }
  }
  if (kind === "run_stream" && sameStrings(slots, ["schema"])) {
    return { kind, declaredId, slots: slots as ["schema"] }
  }
  throw new Error(`program index declaration ${JSON.stringify(kind)} has invalid slots`)
}

function compareDeclarations(left: ProgramDeclaration, right: ProgramDeclaration): number {
  const kindOrder = { task: 0, actor: 1, run_stream: 2 } as const
  const kindDifference = kindOrder[left.kind] - kindOrder[right.kind]
  if (kindDifference !== 0) {
    return kindDifference
  }
  if (left.declaredId < right.declaredId) return -1
  if (left.declaredId > right.declaredId) return 1
  return 0
}

function requireObject(value: JsonValue | undefined, label: string): JsonObject {
  if (!isObject(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value
}

function isObject(value: JsonValue | undefined): value is JsonObject {
  return value !== null && typeof value === "object" && !Array.isArray(value)
}

function requireArray(value: JsonValue | undefined, label: string): readonly JsonValue[] {
  if (!Array.isArray(value)) {
    throw new Error(`${label} must be an array`)
  }
  return value
}

function requireString(value: JsonValue | undefined, label: string): string {
  if (typeof value !== "string") {
    throw new Error(`${label} must be a string`)
  }
  return value
}

function requireKeys(value: JsonObject, expected: readonly string[], label: string): void {
  const keys = Object.keys(value).sort()
  if (!sameStrings(keys, expected)) {
    throw new Error(`${label} has unknown or missing members`)
  }
}

function requireArchitecture(value: JsonValue): RuntimeArchitecture {
  if (value !== "aarch64" && value !== "x86_64") {
    throw new Error(`unsupported runtime architecture ${JSON.stringify(value)}`)
  }
  return value
}

function validArchitectures(value: readonly RuntimeArchitecture[]): boolean {
  return sameStrings(value, ["aarch64"]) ||
    sameStrings(value, ["x86_64"]) ||
    sameStrings(value, ["aarch64", "x86_64"])
}

function sameStrings(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

function domainDigest(domain: string, canonical: Uint8Array): Uint8Array {
  return new Uint8Array(createHash("sha256").update(domain).update(canonical).digest())
}

function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}
