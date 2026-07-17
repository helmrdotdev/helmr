import { createHash } from "node:crypto"

import {
  canonicalizeJsonValue,
  type JsonObject,
  type JsonValue,
} from "./jsoncanon"
import {
  compareUtf8,
  validateLocalPackagePath,
} from "./package-graph"

export const LOCAL_MANIFESTS_FORMAT_VERSION = 0 as const

const localManifestsDigestDomain = "helmr.deployment-local-manifests.v0\0"
const maxLocalManifestsSizeBytes = 16777216
const sha256DigestPattern = /^sha256:[0-9a-f]{64}$/

export interface LocalManifestEntry {
  readonly manifestDigest: string
  readonly path: string
}

export interface LocalManifests {
  readonly formatVersion: 0
  readonly entries: readonly LocalManifestEntry[]
}

export function canonicalLocalManifests(manifests: LocalManifests): Uint8Array {
  validateLocalManifests(manifests)
  const canonical = canonicalizeJsonValue(manifests as unknown as JsonValue)
  if (canonical.length > maxLocalManifestsSizeBytes) {
    throw new Error(`local manifests size must be at most ${maxLocalManifestsSizeBytes} bytes`)
  }
  return canonical
}

export function localManifestsDigest(manifests: LocalManifests): Uint8Array {
  const canonical = canonicalLocalManifests(manifests)
  return new Uint8Array(
    createHash("sha256")
      .update(localManifestsDigestDomain)
      .update(canonical)
      .digest(),
  )
}

export function validateLocalManifests(manifests: LocalManifests): void {
  validateLocalManifestsValue(manifests as unknown as JsonValue)
}

function validateLocalManifestsValue(value: JsonValue): LocalManifests {
  const root = requireObject(value, "local manifests")
  requireKeys(root, ["entries", "formatVersion"], "local manifests")
  if (root["formatVersion"] !== LOCAL_MANIFESTS_FORMAT_VERSION) {
    throw new Error(`local manifests formatVersion must be ${LOCAL_MANIFESTS_FORMAT_VERSION}`)
  }

  const entriesValue = requireArray(root["entries"], "local manifests entries")
  if (entriesValue.length === 0) {
    throw new Error("local manifests entries must begin with exactly one root record")
  }
  const entries: LocalManifestEntry[] = []
  const paths = new Set<string>()
  for (const [position, entryValue] of entriesValue.entries()) {
    const object = requireObject(entryValue, `local manifests entries[${position}]`)
    requireKeys(object, ["manifestDigest", "path"], `local manifests entries[${position}]`)
    const path = requireString(object["path"], `local manifests entries[${position}].path`)
    const manifestDigest = requireDigest(
      object["manifestDigest"],
      `local manifests entries[${position}].manifestDigest`,
    )
    if (position === 0) {
      if (path !== ".") {
        throw new Error("local manifests entries must begin with exactly one root record")
      }
    } else {
      if (path === ".") {
        throw new Error("only the first local manifest may use root path .")
      }
      validateLocalPackagePath(path, `local manifests entries[${position}].path`)
      if (position > 1 && compareUtf8(entries[position - 1]!.path, path) >= 0) {
        throw new Error(`local manifests entries are not in canonical path order at position ${position}`)
      }
    }
    if (paths.has(path)) {
      throw new Error(`local manifests entries contains duplicate path ${JSON.stringify(path)}`)
    }
    for (let separator = path.indexOf("/"); separator >= 0;) {
      const ancestor = path.slice(0, separator)
      if (paths.has(ancestor)) {
        throw new Error(
          `local manifest paths ${JSON.stringify(ancestor)} and ${JSON.stringify(path)} overlap`,
        )
      }
      separator = path.indexOf("/", separator + 1)
    }
    paths.add(path)
    entries.push({ manifestDigest, path })
  }
  return { formatVersion: LOCAL_MANIFESTS_FORMAT_VERSION, entries }
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
