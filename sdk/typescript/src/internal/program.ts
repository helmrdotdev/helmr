import { createHash } from "node:crypto"

import { canonicalizeJsonValue, parseJson, type JsonObject, type JsonValue } from "./jsoncanon"

export const PROGRAM_INDEX_FORMAT_VERSION = 0 as const
export const RUNTIME_API_VERSION = "helmr.runtime.v0" as const
export const PROGRAM_BUILD_CONTRACT_VERSION = "helmr.program-build.v0" as const
export const PROGRAM_ARTIFACT_MEDIA_TYPE =
  "application/vnd.helmr.deployment-program.v0+squashfs" as const

const declaredIdPattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/
const sha256DigestPattern = /^sha256:[0-9a-f]{64}$/
const packageManagerVersionPattern =
  /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$/
const manifestDigestDomain = "helmr.deployment-definition-manifest.v0\0"
const maxProgramFileSizeBytes = 16777216
const maxPackageManagerVersionBytes = 64

export type RuntimeArchitecture = "x86_64"
export type ProgramDeclaration =
  | Readonly<{
      kind: "task"
      declaredId: string
      slots: readonly ["handler"] | readonly ["handler", "payloadSchema"]
    }>
  | Readonly<{ kind: "actor"; declaredId: string; slots: readonly ["handler"] }>

export interface ProgramIndex {
  readonly architecture: RuntimeArchitecture
  readonly buildContractVersion: typeof PROGRAM_BUILD_CONTRACT_VERSION
  readonly declarations: readonly ProgramDeclaration[]
  readonly formatVersion: 0
  readonly manager: Readonly<{
    digest: string
    name: "bun" | "npm" | "pnpm"
    version: string
  }>
  readonly runtimeApiVersion: typeof RUNTIME_API_VERSION
  readonly runtimeDigest: string
  readonly standardToolchainDigest: string
  readonly submitted: Readonly<{
    lockfileDigest: string
    lockfileName:
      | "bun.lock"
      | "npm-shrinkwrap.json"
      | "package-lock.json"
      | "pnpm-lock.yaml"
    sourceDigest: string
  }>
}

export function parseProgramIndex(raw: string | Uint8Array): ProgramIndex {
  const input = typeof raw === "string" ? new TextEncoder().encode(raw) : raw
  if (input.length === 0 || input.length > maxProgramFileSizeBytes) {
    throw new Error(`program index size is outside [1,${maxProgramFileSizeBytes}]`)
  }
  const value = parseJson(raw)
  const canonical = canonicalizeJsonValue(value)
  if (!bytesEqual(input, canonical)) {
    throw new Error("program index is not RFC 8785 canonical JSON")
  }
  return validateProgramIndexValue(value)
}

export function canonicalProgramIndex(index: ProgramIndex): Uint8Array {
  validateProgramIndex(index)
  const canonical = canonicalizeJsonValue(index as unknown as JsonValue)
  if (canonical.length > maxProgramFileSizeBytes) {
    throw new Error(`program index size is outside [1,${maxProgramFileSizeBytes}]`)
  }
  return canonical
}

export function validateProgramIndex(index: ProgramIndex): void {
  canonicalizeJsonValue(index as unknown as JsonValue)
  validateProgramIndexValue(index as unknown as JsonValue)
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
  requireKeys(
    root,
    [
      "architecture",
      "buildContractVersion",
      "declarations",
      "formatVersion",
      "manager",
      "runtimeApiVersion",
      "runtimeDigest",
      "standardToolchainDigest",
      "submitted",
    ],
    "program index",
  )
  if (root["formatVersion"] !== PROGRAM_INDEX_FORMAT_VERSION) {
    throw new Error(`program index formatVersion must be ${PROGRAM_INDEX_FORMAT_VERSION}`)
  }
  const buildContractVersion = requireString(
    root["buildContractVersion"],
    "program index buildContractVersion",
  )
  if (buildContractVersion !== PROGRAM_BUILD_CONTRACT_VERSION) {
    throw new Error(
      `program index buildContractVersion must be ${PROGRAM_BUILD_CONTRACT_VERSION}`,
    )
  }

  const runtimeApiVersion = requireString(
    root["runtimeApiVersion"],
    "program index runtimeApiVersion",
  )
  if (runtimeApiVersion !== RUNTIME_API_VERSION) {
    throw new Error(`program index runtimeApiVersion must be ${RUNTIME_API_VERSION}`)
  }
  const runtimeDigest = requireDigest(root["runtimeDigest"], "program index runtimeDigest")
  const architecture = requireArchitecture(root["architecture"])
  const standardToolchainDigest = requireDigest(
    root["standardToolchainDigest"],
    "program index standardToolchainDigest",
  )
  const managerValue = requireObject(root["manager"], "program index manager")
  requireKeys(managerValue, ["digest", "name", "version"], "program index manager")
  const managerName = requireString(managerValue["name"], "program index manager.name")
  if (managerName !== "npm" && managerName !== "pnpm" && managerName !== "bun") {
    throw new Error(`program index manager.name ${JSON.stringify(managerName)} is unsupported`)
  }
  const managerVersion = requireString(managerValue["version"], "program index manager.version")
  if (
    new TextEncoder().encode(managerVersion).length > maxPackageManagerVersionBytes ||
    !packageManagerVersionPattern.test(managerVersion)
  ) {
    throw new Error(
      `program index manager.version ${JSON.stringify(managerVersion)} is not an admitted SemVer`,
    )
  }
  const manager = {
    digest: requireDigest(
      managerValue["digest"],
      "program index manager.digest",
    ),
    name: managerName,
    version: managerVersion,
  } as const

  const submittedValue = requireObject(root["submitted"], "program index submitted")
  requireKeys(
    submittedValue,
    ["lockfileDigest", "lockfileName", "sourceDigest"],
    "program index submitted",
  )
  const lockfileName = requireString(
    submittedValue["lockfileName"],
    "program index submitted.lockfileName",
  )
  const validLockfile =
    (managerName === "npm" &&
      (lockfileName === "package-lock.json" || lockfileName === "npm-shrinkwrap.json")) ||
    (managerName === "pnpm" && lockfileName === "pnpm-lock.yaml") ||
    (managerName === "bun" && lockfileName === "bun.lock")
  if (!validLockfile) {
    throw new Error(
      `program index submitted.lockfileName ${JSON.stringify(lockfileName)} is unsupported for ${managerName}`,
    )
  }
  const submitted = {
    lockfileDigest: requireDigest(
      submittedValue["lockfileDigest"],
      "program index submitted.lockfileDigest",
    ),
    lockfileName: lockfileName as
      | "bun.lock"
      | "npm-shrinkwrap.json"
      | "package-lock.json"
      | "pnpm-lock.yaml",
    sourceDigest: requireDigest(
      submittedValue["sourceDigest"],
      "program index submitted.sourceDigest",
    ),
  }

  const declarationValues = requireArray(root["declarations"], "program index declarations")
  if (declarationValues.length === 0) {
    throw new Error("program index declarations must not be empty")
  }
  const declarations = declarationValues.map((item, position) => parseDeclaration(item, position))
  for (let position = 1; position < declarations.length; position++) {
    if (
      compareDeclarations(
        declarations[position - 1] as ProgramDeclaration,
        declarations[position] as ProgramDeclaration,
      ) >= 0
    ) {
      throw new Error(
        `program index declarations are not in canonical order at position ${position}`,
      )
    }
  }

  return {
    architecture,
    buildContractVersion,
    declarations,
    formatVersion: PROGRAM_INDEX_FORMAT_VERSION,
    manager,
    runtimeApiVersion,
    runtimeDigest,
    standardToolchainDigest,
    submitted,
  }
}

function parseDeclaration(value: JsonValue, position: number): ProgramDeclaration {
  const declaration = requireObject(value, `program index declaration ${position}`)
  requireKeys(declaration, ["declaredId", "kind", "slots"], `program index declaration ${position}`)
  const kind = requireString(declaration["kind"], "declaration kind")
  const declaredId = requireString(declaration["declaredId"], "declaration declaredId")
  if (!declaredIdPattern.test(declaredId)) {
    throw new Error(
      `program index declaredId ${JSON.stringify(declaredId)} is outside the exact ASCII ID domain`,
    )
  }
  const slots = requireArray(declaration["slots"], "declaration slots").map((slot) =>
    requireString(slot, "declaration slot"),
  )
  if (
    kind === "task" &&
    (sameStrings(slots, ["handler"]) || sameStrings(slots, ["handler", "payloadSchema"]))
  ) {
    return {
      kind,
      declaredId,
      slots: slots as ["handler"] | ["handler", "payloadSchema"],
    }
  }
  if (kind === "actor" && sameStrings(slots, ["handler"])) {
    return { kind, declaredId, slots: slots as ["handler"] }
  }
  throw new Error(`program index declaration ${JSON.stringify(kind)} has invalid slots`)
}

function compareDeclarations(left: ProgramDeclaration, right: ProgramDeclaration): number {
  const kindOrder = { task: 0, actor: 1 } as const
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

function requireDigest(value: JsonValue | undefined, label: string): string {
  const digest = requireString(value, label)
  if (!sha256DigestPattern.test(digest)) {
    throw new Error(`${label} must be a lowercase SHA-256 digest`)
  }
  return digest
}

function requireKeys(value: JsonObject, expected: readonly string[], label: string): void {
  const keys = Object.keys(value).sort()
  if (!sameStrings(keys, expected)) {
    throw new Error(`${label} has unknown or missing members`)
  }
}

function requireArchitecture(value: JsonValue | undefined): RuntimeArchitecture {
  if (value !== "x86_64") {
    throw new Error(`unsupported runtime architecture ${JSON.stringify(value)}`)
  }
  return value
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
