import {
  canonicalizeJsonValue,
  parseJson,
  type JsonObject,
  type JsonValue,
} from "./jsoncanon"

export const DEPENDENCY_INDEX_FORMAT_VERSION = 0 as const
export const DEPENDENCY_MATERIALIZER_VERSION = "helmr.dependencies.v0" as const

const maxDependencyIndexSizeBytes = 4096
const maxPackageGraphSizeBytes = 16777216
const maxPackageManagerVersionBytes = 64
const sha256DigestPattern = /^sha256:[0-9a-f]{64}$/
const packageManagerVersionPattern = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$/
const textEncoder = new TextEncoder()

export type DependencyArchitecture = "aarch64" | "x86_64"
export type PackageManagerName = "bun" | "npm"

export interface DependencyPackageManager {
  readonly name: PackageManagerName
  readonly version: string
}

export interface DependencyLockfile {
  readonly name: "bun.lock" | "package-lock.json"
  readonly digest: string
}

export interface DependencyIndex {
  readonly formatVersion: 0
  readonly packageManager: DependencyPackageManager
  readonly lockfile: DependencyLockfile
  readonly localManifestsDigest: string
  readonly packageGraphDigest: string
  readonly packageGraphSizeBytes: number
  readonly materializerVersion: typeof DEPENDENCY_MATERIALIZER_VERSION
  readonly runtimeDigest: string
  readonly architecture: DependencyArchitecture
}

export function parseDependencyIndex(raw: string | Uint8Array): DependencyIndex {
  const input = typeof raw === "string" ? textEncoder.encode(raw) : raw
  if (input.length === 0 || input.length > maxDependencyIndexSizeBytes) {
    throw new Error(`dependency index size must be between 1 and ${maxDependencyIndexSizeBytes} bytes`)
  }
  const value = parseJson(raw)
  const canonical = canonicalizeJsonValue(value)
  if (!bytesEqual(input, canonical)) {
    throw new Error("dependency index is not RFC 8785 canonical JSON")
  }
  return validateDependencyIndexValue(value)
}

export function canonicalDependencyIndex(index: DependencyIndex): Uint8Array {
  validateDependencyIndex(index)
  const canonical = canonicalizeJsonValue(index as unknown as JsonValue)
  if (canonical.length > maxDependencyIndexSizeBytes) {
    throw new Error(`dependency index size must be at most ${maxDependencyIndexSizeBytes} bytes`)
  }
  return canonical
}

export function validateDependencyIndex(index: DependencyIndex): void {
  canonicalizeJsonValue(index as unknown as JsonValue)
  validateDependencyIndexValue(index as unknown as JsonValue)
}

function validateDependencyIndexValue(value: JsonValue): DependencyIndex {
  const root = requireObject(value, "dependency index")
  requireKeys(
    root,
    [
      "architecture",
      "formatVersion",
      "localManifestsDigest",
      "lockfile",
      "materializerVersion",
      "packageGraphDigest",
      "packageGraphSizeBytes",
      "packageManager",
      "runtimeDigest",
    ],
    "dependency index",
  )
  if (root["formatVersion"] !== DEPENDENCY_INDEX_FORMAT_VERSION) {
    throw new Error(`dependency index formatVersion must be ${DEPENDENCY_INDEX_FORMAT_VERSION}`)
  }
  const managerValue = requireObject(root["packageManager"], "dependency index packageManager")
  requireKeys(managerValue, ["name", "version"], "dependency index packageManager")
  const managerName = requirePackageManagerName(managerValue["name"])
  const managerVersion = requireString(managerValue["version"], "dependency index packageManager.version")
  const versionMatch = packageManagerVersionPattern.exec(managerVersion)
  if (
    textEncoder.encode(managerVersion).length > maxPackageManagerVersionBytes ||
    versionMatch?.[0] !== managerVersion
  ) {
    throw new Error("dependency index packageManager.version is not an admitted SemVer")
  }

  const lockfileValue = requireObject(root["lockfile"], "dependency index lockfile")
  requireKeys(lockfileValue, ["digest", "name"], "dependency index lockfile")
  const lockfileName = requireString(lockfileValue["name"], "dependency index lockfile.name")
  const expectedLockfileName = managerName === "bun" ? "bun.lock" : "package-lock.json"
  if (lockfileName !== expectedLockfileName) {
    throw new Error(`dependency index lockfile.name must be ${expectedLockfileName}`)
  }
  const packageGraphSizeBytes = requirePositiveSafeInteger(
    root["packageGraphSizeBytes"],
    "dependency index packageGraphSizeBytes",
  )
  if (packageGraphSizeBytes > maxPackageGraphSizeBytes) {
    throw new Error(`dependency index packageGraphSizeBytes must be at most ${maxPackageGraphSizeBytes}`)
  }
  if (root["materializerVersion"] !== DEPENDENCY_MATERIALIZER_VERSION) {
    throw new Error(`dependency index materializerVersion must be ${DEPENDENCY_MATERIALIZER_VERSION}`)
  }
  const architecture = root["architecture"]
  if (architecture !== "aarch64" && architecture !== "x86_64") {
    throw new Error(`dependency index architecture ${JSON.stringify(architecture)} is unsupported`)
  }
  return {
    formatVersion: DEPENDENCY_INDEX_FORMAT_VERSION,
    packageManager: { name: managerName, version: managerVersion },
    lockfile: {
      name: lockfileName,
      digest: requireDigest(lockfileValue["digest"], "dependency index lockfile.digest"),
    },
    localManifestsDigest: requireDigest(
      root["localManifestsDigest"],
      "dependency index localManifestsDigest",
    ),
    packageGraphDigest: requireDigest(root["packageGraphDigest"], "dependency index packageGraphDigest"),
    packageGraphSizeBytes,
    materializerVersion: DEPENDENCY_MATERIALIZER_VERSION,
    runtimeDigest: requireDigest(root["runtimeDigest"], "dependency index runtimeDigest"),
    architecture,
  }
}

function requireObject(value: JsonValue | undefined, label: string): JsonObject {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as JsonObject
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

function requirePackageManagerName(value: JsonValue | undefined): PackageManagerName {
  if (value !== "bun" && value !== "npm") {
    throw new Error(`dependency index packageManager.name ${JSON.stringify(value)} is unsupported`)
  }
  return value
}

function requirePositiveSafeInteger(value: JsonValue | undefined, label: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1) {
    throw new Error(`${label} must be a positive JavaScript-safe integer`)
  }
  return value
}

function requireKeys(value: JsonObject, expected: readonly string[], label: string): void {
  const keys = Object.keys(value).sort()
  if (keys.length !== expected.length || keys.some((key, index) => key !== expected[index])) {
    throw new Error(`${label} has unknown or missing members`)
  }
}

function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}
