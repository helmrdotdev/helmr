import { createHash } from "node:crypto"

import {
  canonicalizeJsonValue,
  parseJson,
  type JsonObject,
  type JsonValue,
} from "./jsoncanon"

export const PACKAGE_GRAPH_FORMAT_VERSION = 0 as const

const maxPackageGraphSizeBytes = 16777216
const maxPackageNameBytes = 214
const maxPackageVersionBytes = 255
const maxPackagePathComponentBytes = 255
const maxMountedPackagePathBytes = 4096
const localPackageViewKeyDomain = "helmr.local-package-view.v0"
const programMountPath = "/opt/helmr/program"
const dependencyMountPath = "/opt/helmr/program/node_modules"
const packageNamePattern =
  /^(?:[a-z0-9][a-z0-9._~-]*|@[a-z0-9][a-z0-9._~-]*\/[a-z0-9][a-z0-9._~-]*)$/
const sha256DigestPattern = /^sha256:[0-9a-f]{64}$/
const sha512IntegrityPattern = /^sha512-[A-Za-z0-9+/]{86}==$/
const textEncoder = new TextEncoder()

export type PackageKind = "local" | "registry"
export type PackageRelationship = "production" | "optional" | "peer"

export interface LocalPackage {
  readonly manifestDigest: string
  readonly name: string | null
  readonly path: string
  readonly version: string | null
  readonly viewKey: string | null
}

export interface RegistryPackage {
  readonly installPath: string
  readonly integrity: string
  readonly name: string
  readonly version: string
}

export type PackageEndpoint =
  | Readonly<{ kind: "local"; path: string }>
  | Readonly<{ installPath: string; kind: "registry" }>

export interface PackageResolution {
  readonly dependency: string
  readonly from: PackageEndpoint
  readonly relationship: PackageRelationship
  readonly to: PackageEndpoint
}

export interface PackageGraph {
  readonly formatVersion: 0
  readonly localPackages: readonly LocalPackage[]
  readonly registryPackages: readonly RegistryPackage[]
  readonly resolutions: readonly PackageResolution[]
}

export function parsePackageGraph(raw: string | Uint8Array): PackageGraph {
  const input = typeof raw === "string" ? textEncoder.encode(raw) : raw
  if (input.length === 0 || input.length > maxPackageGraphSizeBytes) {
    throw new Error(`package graph size must be between 1 and ${maxPackageGraphSizeBytes} bytes`)
  }
  const value = parseJson(raw)
  const canonical = canonicalizeJsonValue(value)
  if (!bytesEqual(input, canonical)) {
    throw new Error("package graph is not RFC 8785 canonical JSON")
  }
  return validatePackageGraphValue(value)
}

export function canonicalPackageGraph(graph: PackageGraph): Uint8Array {
  validatePackageGraph(graph)
  const canonical = canonicalizeJsonValue(graph as unknown as JsonValue)
  if (canonical.length > maxPackageGraphSizeBytes) {
    throw new Error(`package graph size must be at most ${maxPackageGraphSizeBytes} bytes`)
  }
  return canonical
}

export function validatePackageGraph(graph: PackageGraph): void {
  canonicalizeJsonValue(graph as unknown as JsonValue)
  validatePackageGraphValue(graph as unknown as JsonValue)
}

function validatePackageGraphValue(value: JsonValue): PackageGraph {
  const root = requireObject(value, "package graph")
  requireKeys(
    root,
    ["formatVersion", "localPackages", "registryPackages", "resolutions"],
    "package graph",
  )
  if (root["formatVersion"] !== PACKAGE_GRAPH_FORMAT_VERSION) {
    throw new Error(`package graph formatVersion must be ${PACKAGE_GRAPH_FORMAT_VERSION}`)
  }

  const localValues = requireArray(root["localPackages"], "package graph localPackages")
  const registryValues = requireArray(root["registryPackages"], "package graph registryPackages")
  const resolutionValues = requireArray(root["resolutions"], "package graph resolutions")
  const localPackages = localValues.map((item, position) => parseLocalPackage(item, position))
  const registryPackages = registryValues.map((item, position) =>
    parseRegistryPackage(item, position),
  )
  if (localPackages.length === 0 || localPackages[0]!.path !== ".") {
    throw new Error("package graph localPackages must begin with exactly one root record")
  }

  const locals = new Map<string, LocalPackage>()
  const localNames = new Set<string>()
  const viewKeys = new Set<string>()
  for (let position = 0; position < localPackages.length; position++) {
    const local = localPackages[position]!
    validateLocalPackage(local, position === 0)
    if (locals.has(local.path)) {
      throw new Error(`package graph localPackages contains duplicate path ${JSON.stringify(local.path)}`)
    }
    locals.set(local.path, local)
    if (local.name !== null) {
      if (localNames.has(local.name)) {
        throw new Error(`package graph localPackages contains ambiguous name ${JSON.stringify(local.name)}`)
      }
      localNames.add(local.name)
    }
    if (local.viewKey !== null) {
      if (viewKeys.has(local.viewKey)) {
        throw new Error(`package graph localPackages contains colliding viewKey ${JSON.stringify(local.viewKey)}`)
      }
      viewKeys.add(local.viewKey)
    }
    if (position > 1) {
      const previous = localPackages[position - 1]!.path
      if (compareUtf8(previous, local.path) >= 0) {
        throw new Error(`package graph localPackages are not in canonical path order at position ${position}`)
      }
    }
    for (let separator = local.path.indexOf("/"); separator >= 0; ) {
      const ancestor = local.path.slice(0, separator)
      if (locals.has(ancestor)) {
        throw new Error(`package graph non-root local paths ${JSON.stringify(ancestor)} and ${JSON.stringify(local.path)} overlap`)
      }
      separator = local.path.indexOf("/", separator + 1)
    }
  }

  const registries = new Set<string>()
  for (let position = 0; position < registryPackages.length; position++) {
    const registry = registryPackages[position]!
    validateRegistryPackage(registry)
    if (registries.has(registry.installPath)) {
      throw new Error(`package graph registryPackages contains duplicate installPath ${JSON.stringify(registry.installPath)}`)
    }
    registries.add(registry.installPath)
    if (
      position > 0 &&
      compareUtf8(registryPackages[position - 1]!.installPath, registry.installPath) >= 0
    ) {
      throw new Error(`package graph registryPackages are not in canonical installPath order at position ${position}`)
    }
  }

  const resolutions = resolutionValues.map((item, position) =>
    parseResolution(item, position, locals, registries),
  )
  for (let position = 1; position < resolutions.length; position++) {
    if (compareResolutions(resolutions[position - 1]!, resolutions[position]!) >= 0) {
      throw new Error(`package graph resolutions are not in canonical order at position ${position}`)
    }
  }

  return {
    formatVersion: PACKAGE_GRAPH_FORMAT_VERSION,
    localPackages,
    registryPackages,
    resolutions,
  }
}

function parseLocalPackage(value: JsonValue, position: number): LocalPackage {
  const local = requireObject(value, `package graph localPackages[${position}]`)
  requireKeys(
    local,
    ["manifestDigest", "name", "path", "version", "viewKey"],
    `package graph localPackages[${position}]`,
  )
  return {
    manifestDigest: requireDigest(
      local["manifestDigest"],
      `package graph localPackages[${position}].manifestDigest`,
    ),
    name: requireNullableString(local["name"], `package graph localPackages[${position}].name`),
    path: requireString(local["path"], `package graph localPackages[${position}].path`),
    version: requireNullableString(
      local["version"],
      `package graph localPackages[${position}].version`,
    ),
    viewKey: requireNullableString(
      local["viewKey"],
      `package graph localPackages[${position}].viewKey`,
    ),
  }
}

function parseRegistryPackage(value: JsonValue, position: number): RegistryPackage {
  const registry = requireObject(value, `package graph registryPackages[${position}]`)
  requireKeys(
    registry,
    ["installPath", "integrity", "name", "version"],
    `package graph registryPackages[${position}]`,
  )
  return {
    installPath: requireString(
      registry["installPath"],
      `package graph registryPackages[${position}].installPath`,
    ),
    integrity: requireString(
      registry["integrity"],
      `package graph registryPackages[${position}].integrity`,
    ),
    name: requireString(registry["name"], `package graph registryPackages[${position}].name`),
    version: requireString(
      registry["version"],
      `package graph registryPackages[${position}].version`,
    ),
  }
}

function parseResolution(
  value: JsonValue,
  position: number,
  locals: ReadonlyMap<string, LocalPackage>,
  registries: ReadonlySet<string>,
): PackageResolution {
  const resolution = requireObject(value, `package graph resolutions[${position}]`)
  requireKeys(
    resolution,
    ["dependency", "from", "relationship", "to"],
    `package graph resolutions[${position}]`,
  )
  const dependency = requireString(
    resolution["dependency"],
    `package graph resolutions[${position}].dependency`,
  )
  validatePackageName(dependency, `package graph resolutions[${position}].dependency`)
  const relationship = resolution["relationship"]
  if (relationship !== "production" && relationship !== "optional" && relationship !== "peer") {
    throw new Error(`package graph resolutions[${position}].relationship is unsupported`)
  }
  const from = parseEndpoint(
    resolution["from"],
    `package graph resolutions[${position}].from`,
    locals,
    registries,
  )
  const to = parseEndpoint(
    resolution["to"],
    `package graph resolutions[${position}].to`,
    locals,
    registries,
  )
  if (to.kind === "local") {
    const local = locals.get(to.path)!
    if (local.name === null) {
      throw new Error(`package graph local target ${JSON.stringify(to.path)} has no name`)
    }
    if (dependency !== local.name) {
      throw new Error(`package graph dependency ${JSON.stringify(dependency)} does not equal local target name ${JSON.stringify(local.name)}`)
    }
  }
  return { dependency, from, relationship, to }
}

function parseEndpoint(
  value: JsonValue | undefined,
  label: string,
  locals: ReadonlyMap<string, LocalPackage>,
  registries: ReadonlySet<string>,
): PackageEndpoint {
  const endpoint = requireObject(value, label)
  if (endpoint["kind"] === "local") {
    requireKeys(endpoint, ["kind", "path"], label)
    const path = requireString(endpoint["path"], `${label}.path`)
    if (!locals.has(path)) {
      throw new Error(`${label}.path ${JSON.stringify(path)} does not name a graph node`)
    }
    return { kind: "local", path }
  }
  if (endpoint["kind"] === "registry") {
    requireKeys(endpoint, ["installPath", "kind"], label)
    const installPath = requireString(endpoint["installPath"], `${label}.installPath`)
    if (!registries.has(installPath)) {
      throw new Error(`${label}.installPath ${JSON.stringify(installPath)} does not name a graph node`)
    }
    return { installPath, kind: "registry" }
  }
  throw new Error(`${label}.kind is unsupported`)
}

function validateLocalPackage(local: LocalPackage, root: boolean): void {
  if (root) {
    if (local.path !== ".") {
      throw new Error(`package graph root path must be .`)
    }
    if (local.viewKey !== null) {
      throw new Error("package graph root viewKey must be null")
    }
  } else {
    if (local.path === ".") {
      throw new Error("only the first local package may use root path .")
    }
    validatePackagePath(local.path, programMountPath, true, "package graph local path")
    if (local.viewKey === null) {
      throw new Error("package graph non-root viewKey must be a string")
    }
    const expected = localPackageViewKey(local.path)
    if (local.viewKey !== expected) {
      throw new Error(`package graph viewKey must be ${expected}`)
    }
  }
  if (local.name !== null) validatePackageName(local.name, "package graph local name")
  if (local.version !== null) validatePackageVersion(local.version, "package graph local version")
}

function validateRegistryPackage(registry: RegistryPackage): void {
  validatePackagePath(
    registry.installPath,
    dependencyMountPath,
    false,
    "package graph registry installPath",
  )
  validatePackageIntegrity(registry.integrity)
  validatePackageName(registry.name, "package graph registry name")
  validatePackageVersion(registry.version, "package graph registry version")
}

function validatePackageName(name: string, label: string): void {
  const length = textEncoder.encode(name).length
  if (length === 0 || length > maxPackageNameBytes || !packageNamePattern.test(name)) {
    throw new Error(`${label} ${JSON.stringify(name)} is outside the exact package-name domain`)
  }
}

function validatePackageVersion(version: string, label: string): void {
  const length = textEncoder.encode(version).length
  if (length === 0 || length > maxPackageVersionBytes) {
    throw new Error(`${label} ${JSON.stringify(version)} is outside the exact package-version domain`)
  }
}

function validatePackageIntegrity(integrity: string): void {
  if (!sha512IntegrityPattern.test(integrity)) {
    throw new Error("package graph integrity is not a canonical SHA-512 SRI value")
  }
  const encoded = integrity.slice("sha512-".length)
  const digest = Buffer.from(encoded, "base64")
  if (digest.length !== 64 || digest.toString("base64") !== encoded) {
    throw new Error("package graph integrity is not a canonical SHA-512 SRI value")
  }
}

function validatePackagePath(path: string, mountPath: string, local: boolean, label: string): void {
  if (path.length === 0 || path.startsWith("/") || path.includes("\\") || /[\p{Cc}]/u.test(path)) {
    throw new Error(`${label} ${JSON.stringify(path)} is not a confined relative POSIX path`)
  }
  const components = path.split("/")
  if (
    components.some(
      (component) =>
        component === "" ||
        component === "." ||
        component === ".." ||
        textEncoder.encode(component).length > maxPackagePathComponentBytes,
    )
  ) {
    throw new Error(`${label} ${JSON.stringify(path)} is not normalized or exceeds a component bound`)
  }
  const root = components[0]!
  if (local && (root === "helmr" || root === ".helmr" || root === "node_modules")) {
    throw new Error(`${label} ${JSON.stringify(path)} uses reserved Deployment root ${JSON.stringify(root)}`)
  }
  if (!local && root === ".helmr") {
    throw new Error(`${label} ${JSON.stringify(path)} uses the reserved dependency root`)
  }
  if (textEncoder.encode(`${mountPath}/${path}\0`).length > maxMountedPackagePathBytes) {
    throw new Error(`${label} ${JSON.stringify(path)} exceeds the mounted path bound`)
  }
}

function localPackageViewKey(path: string): string {
  return createHash("sha256")
    .update(localPackageViewKeyDomain)
    .update(new Uint8Array([0]))
    .update(path)
    .digest("hex")
}

function compareResolutions(left: PackageResolution, right: PackageResolution): number {
  return (
    compareEndpoints(left.from, right.from) ||
    compareUtf8(left.dependency, right.dependency) ||
    relationshipOrder(left.relationship) - relationshipOrder(right.relationship) ||
    compareEndpoints(left.to, right.to)
  )
}

function compareEndpoints(left: PackageEndpoint, right: PackageEndpoint): number {
  return kindOrder(left.kind) - kindOrder(right.kind) || compareUtf8(locator(left), locator(right))
}

function locator(endpoint: PackageEndpoint): string {
  return endpoint.kind === "local" ? endpoint.path : endpoint.installPath
}

function kindOrder(kind: PackageKind): number {
  return kind === "local" ? 0 : 1
}

function relationshipOrder(relationship: PackageRelationship): number {
  switch (relationship) {
    case "production":
      return 0
    case "optional":
      return 1
    case "peer":
      return 2
  }
}

function compareUtf8(left: string, right: string): number {
  const leftBytes = textEncoder.encode(left)
  const rightBytes = textEncoder.encode(right)
  const length = Math.min(leftBytes.length, rightBytes.length)
  for (let index = 0; index < length; index++) {
    const difference = leftBytes[index]! - rightBytes[index]!
    if (difference !== 0) return difference
  }
  return leftBytes.length - rightBytes.length
}

function requireObject(value: JsonValue | undefined, label: string): JsonObject {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as JsonObject
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

function requireNullableString(value: JsonValue | undefined, label: string): string | null {
  if (value !== null && typeof value !== "string") {
    throw new Error(`${label} must be a string or null`)
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
  if (keys.length !== expected.length || keys.some((key, index) => key !== expected[index])) {
    throw new Error(`${label} has unknown or missing members`)
  }
}

function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}
