import { expect, test } from "bun:test"
import { readFile } from "node:fs/promises"
import { fileURLToPath } from "node:url"

import {
  canonicalLocalManifests,
  localManifestsDigest,
  type LocalManifests,
} from "./local-manifests"

interface LocalManifestsFixture {
  readonly localManifests: {
    readonly canonical: string
    readonly digestHex: string
  }
  readonly localManifestsRejections: readonly {
    readonly name: string
    readonly mutation: string
  }[]
}

test("local manifests matches shared golden fixture", async () => {
  const fixture = await loadFixture()
  const manifests = JSON.parse(fixture.localManifests.canonical) as LocalManifests
  expect(new TextDecoder().decode(canonicalLocalManifests(manifests))).toBe(
    fixture.localManifests.canonical,
  )
  expect(Buffer.from(localManifestsDigest(manifests)).toString("hex")).toBe(
    fixture.localManifests.digestHex,
  )
})

test("local manifests rejects shared mutations", async () => {
  const fixture = await loadFixture()
  for (const item of fixture.localManifestsRejections) {
    const value = JSON.parse(fixture.localManifests.canonical) as Record<string, unknown>
    const entries = value["entries"] as Record<string, unknown>[]
    switch (item.mutation) {
      case "invalid_format_version":
        value["formatVersion"] = 1
        break
      case "root_not_first":
        ;[entries[0], entries[1]] = [entries[1]!, entries[0]!]
        break
      case "duplicate_root":
        entries.push({ ...entries[0]! })
        break
      case "entry_order":
        entries.push(localManifestEntry("packages/z", "2"), localManifestEntry("packages/a", "3"))
        break
      case "overlapping_path":
        entries[1]!["path"] = "packages"
        entries.push(localManifestEntry("packages/shared", "2"))
        break
      case "non_adjacent_overlapping_path":
        value["entries"] = [
          entries[0]!,
          localManifestEntry("a", "1"),
          localManifestEntry("a-", "2"),
          localManifestEntry("a/b", "3"),
        ]
        break
      case "reserved_path":
        entries[1]!["path"] = "node_modules/shared"
        break
      case "absolute_path":
        entries[1]!["path"] = "/packages/shared"
        break
      case "invalid_manifest_digest":
        entries[1]!["manifestDigest"] = "sha256:invalid"
        break
      default:
        throw new Error(`unknown mutation ${item.mutation}`)
    }
    expect(() => canonicalLocalManifests(value as unknown as LocalManifests), item.name).toThrow()
  }
})

test("local manifests rejects unknown and missing members", async () => {
  const fixture = await loadFixture()
  const missing = JSON.parse(fixture.localManifests.canonical) as Record<string, unknown>
  delete missing["formatVersion"]
  expect(() => canonicalLocalManifests(missing as unknown as LocalManifests)).toThrow(/member/)

  const unknown = JSON.parse(fixture.localManifests.canonical) as Record<string, unknown>
  unknown["unknown"] = true
  expect(() => canonicalLocalManifests(unknown as unknown as LocalManifests)).toThrow(/member/)

  const entry = JSON.parse(fixture.localManifests.canonical) as {
    entries: Record<string, unknown>[]
  }
  entry.entries[0]!["unknown"] = true
  expect(() => canonicalLocalManifests(entry as unknown as LocalManifests)).toThrow(/member/)
})

test("local manifests accepts root-only and unsigned UTF-8 order", () => {
  const rootOnly: LocalManifests = {
    formatVersion: 0,
    entries: [{ manifestDigest: digest("0"), path: "." }],
  }
  expect(() => localManifestsDigest(rootOnly)).not.toThrow()

  const ordered: LocalManifests = {
    formatVersion: 0,
    entries: [
      rootOnly.entries[0]!,
      { manifestDigest: digest("1"), path: "packages/z" },
      { manifestDigest: digest("2"), path: "packages/é" },
    ],
  }
  expect(() => localManifestsDigest(ordered)).not.toThrow()
})

test("local manifests rejects invalid collections and path bounds", async () => {
  const fixture = await loadFixture()
  const value = JSON.parse(fixture.localManifests.canonical) as {
    entries: { path: string }[]
  }
  value.entries[1]!.path = "a".repeat(256)
  expect(() => canonicalLocalManifests(value as unknown as LocalManifests)).toThrow(/component/)

  const nilEntries = { formatVersion: 0, entries: null }
  expect(() => canonicalLocalManifests(nilEntries as unknown as LocalManifests)).toThrow(/array/)
})

async function loadFixture(): Promise<LocalManifestsFixture> {
  const path = fileURLToPath(
    new URL("../../../../fixtures/contracts/deployment-v0/golden.json", import.meta.url),
  )
  return JSON.parse(await readFile(path, "utf8")) as LocalManifestsFixture
}

function localManifestEntry(path: string, fill: string): Record<string, unknown> {
  return { manifestDigest: digest(fill), path }
}

function digest(fill: string): string {
  return `sha256:${fill.repeat(64)}`
}
