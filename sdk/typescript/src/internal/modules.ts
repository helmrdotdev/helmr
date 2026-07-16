import { createHash } from "node:crypto"

import {
  canonicalizeJsonValue,
  parseJson,
  type JsonObject,
  type JsonValue,
} from "./jsoncanon"

export const MODULE_MAP_FORMAT_VERSION = 0 as const
export const TYPESCRIPT_TRANSFORMER = "helmr.typescript.v0" as const

const maxModuleMapSizeBytes = 16777216
const maxModuleCount = 65536
const moduleKeyDomain = "helmr.typescript-module.v0"
const sha256DigestPattern = /^sha256:[0-9a-f]{64}$/
const textEncoder = new TextEncoder()

export type ModuleFormat = "module" | "commonjs"

export interface ProgramModule {
  readonly codeDigest: string
  readonly codePath: string
  readonly format: ModuleFormat
  readonly path: string
  readonly sourceDigest: string
}

export interface ModuleMap {
  readonly formatVersion: 0
  readonly modules: readonly ProgramModule[]
  readonly transformer: typeof TYPESCRIPT_TRANSFORMER
}

export function parseModuleMap(raw: string | Uint8Array): ModuleMap {
  const input = typeof raw === "string" ? textEncoder.encode(raw) : raw
  if (input.length === 0 || input.length > maxModuleMapSizeBytes) {
    throw new Error(`module map size must be between 1 and ${maxModuleMapSizeBytes} bytes`)
  }
  const value = parseJson(raw)
  const canonical = canonicalizeJsonValue(value)
  if (!bytesEqual(input, canonical)) {
    throw new Error("module map is not RFC 8785 canonical JSON")
  }
  return validateModuleMapValue(value)
}

export function canonicalModuleMap(moduleMap: ModuleMap): Uint8Array {
  validateModuleMap(moduleMap)
  const canonical = canonicalizeJsonValue(moduleMap as unknown as JsonValue)
  if (canonical.length > maxModuleMapSizeBytes) {
    throw new Error(`module map size must be at most ${maxModuleMapSizeBytes} bytes`)
  }
  return canonical
}

export function validateModuleMap(moduleMap: ModuleMap): void {
  canonicalizeJsonValue(moduleMap as unknown as JsonValue)
  validateModuleMapValue(moduleMap as unknown as JsonValue)
}

function validateModuleMapValue(value: JsonValue): ModuleMap {
  const root = requireObject(value, "module map")
  requireKeys(root, ["formatVersion", "modules", "transformer"], "module map")
  if (root["formatVersion"] !== MODULE_MAP_FORMAT_VERSION) {
    throw new Error(`module map formatVersion must be ${MODULE_MAP_FORMAT_VERSION}`)
  }
  if (root["transformer"] !== TYPESCRIPT_TRANSFORMER) {
    throw new Error(`module map transformer must be ${TYPESCRIPT_TRANSFORMER}`)
  }
  const values = requireArray(root["modules"], "module map modules")
  if (values.length > maxModuleCount) {
    throw new Error(`module map contains ${values.length} modules; maximum is ${maxModuleCount}`)
  }
  const modules = values.map((item, position) => parseModule(item, position))
  for (let position = 1; position < modules.length; position++) {
    if (compareUtf8(modules[position - 1]!.path, modules[position]!.path) >= 0) {
      throw new Error(`module map entries are not in canonical path order at position ${position}`)
    }
  }
  return {
    formatVersion: MODULE_MAP_FORMAT_VERSION,
    modules,
    transformer: TYPESCRIPT_TRANSFORMER,
  }
}

function parseModule(value: JsonValue, position: number): ProgramModule {
  const module = requireObject(value, `module map entry ${position}`)
  requireKeys(
    module,
    ["codeDigest", "codePath", "format", "path", "sourceDigest"],
    `module map entry ${position}`,
  )
  const path = requireString(module["path"], `module map entry ${position}.path`)
  validateModulePath(path)
  const format = requireFormat(module["format"], position)
  if (path.endsWith(".mts") && format !== "module") {
    throw new Error(`module map entry ${position} .mts path must use module format`)
  }
  if (path.endsWith(".cts") && format !== "commonjs") {
    throw new Error(`module map entry ${position} .cts path must use commonjs format`)
  }
  const codePath = requireString(module["codePath"], `module map entry ${position}.codePath`)
  const expectedCodePath = moduleCodePath(path, format)
  if (codePath !== expectedCodePath) {
    throw new Error(`module map entry ${position}.codePath must be ${expectedCodePath}`)
  }
  return {
    codeDigest: requireDigest(module["codeDigest"], `module map entry ${position}.codeDigest`),
    codePath,
    format,
    path,
    sourceDigest: requireDigest(module["sourceDigest"], `module map entry ${position}.sourceDigest`),
  }
}

function validateModulePath(path: string): void {
  if (path.length === 0 || path.startsWith("/") || path.includes("\\") || /[\p{Cc}]/u.test(path)) {
    throw new Error(`module path ${JSON.stringify(path)} is not a confined relative POSIX path`)
  }
  if (path.split("/").some((component) => component === "" || component === "." || component === "..")) {
    throw new Error(`module path ${JSON.stringify(path)} is not normalized`)
  }
  const root = path.split("/", 1)[0]
  if (root === "helmr" || root === ".helmr" || root === "node_modules") {
    throw new Error(`module path ${JSON.stringify(path)} uses reserved Deployment root ${JSON.stringify(root)}`)
  }
  if (path.endsWith(".d.ts") || path.endsWith(".d.mts") || path.endsWith(".d.cts")) {
    throw new Error(`module path ${JSON.stringify(path)} is declaration-only`)
  }
  if (!path.endsWith(".ts") && !path.endsWith(".mts") && !path.endsWith(".cts")) {
    throw new Error(`module path ${JSON.stringify(path)} is not an admitted TypeScript module`)
  }
}

function moduleCodePath(path: string, format: ModuleFormat): string {
  const key = createHash("sha256")
    .update(moduleKeyDomain)
    .update(new Uint8Array([0]))
    .update(path)
    .digest("hex")
  return `helmr/files/modules/${key}.${format === "module" ? "mjs" : "cjs"}`
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

function requireFormat(value: JsonValue | undefined, position: number): ModuleFormat {
  if (value !== "module" && value !== "commonjs") {
    throw new Error(`module map entry ${position}.format is unsupported`)
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
