import {
  canonicalizeJsonValue,
  matchesIgnorePattern,
  type HelmrConfig,
  type JsonValue,
} from "@helmr/sdk/internal"
import type { AnalysisResult } from "./compile"
import { lstat, readdir, realpath } from "node:fs/promises"
import { relative, resolve, sep } from "node:path"
import { compareUTF8 } from "./utf8"

const executableExtension = /\.(?:cjs|cts|js|jsx|mjs|mts|ts|tsx)$/
const textDecoder = new TextDecoder("utf-8", { fatal: true })
const maxVerificationFailureMessageBytes = 16 << 10

export const VERIFICATION_RESULT_FORMAT_VERSION = 0 as const

export type VerificationGeneratedFile = Readonly<{
  path:
    | "helmr/build-plan.json"
    | "helmr/analysis-locators.json"
    | "helmr/entry.mjs"
  content: string
}>

export type VerificationResultFrame =
  | Readonly<{
      formatVersion: 0
      outcome: "succeeded"
      declarations: AnalysisResult["programDeclarations"]
      files: readonly VerificationGeneratedFile[]
    }>
  | Readonly<{
      formatVersion: 0
      outcome: "failed"
      error: Readonly<{
        reason: "verification_failed"
        message: string
      }>
    }>

export function successfulVerificationResult(
  analysis: AnalysisResult,
): VerificationResultFrame {
  const files: VerificationGeneratedFile[] = [{
    path: "helmr/build-plan.json",
    content: decodeGeneratedFile(analysis.buildPlanBytes),
  }]
  if (analysis.programDeclarations.length > 0) {
    files.push(
      {
        path: "helmr/analysis-locators.json",
        content: decodeGeneratedFile(analysis.declarationLocatorBytes),
      },
      {
        path: "helmr/entry.mjs",
        content: decodeGeneratedFile(analysis.entrypointBytes),
      },
    )
  }
  return Object.freeze({
    formatVersion: VERIFICATION_RESULT_FORMAT_VERSION,
    outcome: "succeeded",
    declarations: analysis.programDeclarations,
    files: Object.freeze(files.map((file) => Object.freeze(file))),
  })
}

export function failedVerificationResult(message: string): VerificationResultFrame {
  const encoded = new TextEncoder().encode(message)
  if (
    encoded.length === 0 ||
    encoded.length > maxVerificationFailureMessageBytes ||
    message.trim() === ""
  ) {
    throw new Error(
      `verification failure message must be nonblank UTF-8 of at most ${maxVerificationFailureMessageBytes} bytes`,
    )
  }
  return Object.freeze({
    formatVersion: VERIFICATION_RESULT_FORMAT_VERSION,
    outcome: "failed",
    error: Object.freeze({
      reason: "verification_failed",
      message,
    }),
  })
}

export function encodeVerificationResultFrame(
  result: VerificationResultFrame,
): Uint8Array {
  const body = canonicalizeJsonValue(result as unknown as JsonValue)
  const frame = new Uint8Array(4 + body.length)
  new DataView(frame.buffer).setUint32(0, body.length, false)
  frame.set(body, 4)
  return frame
}

export async function discoverModules(
  root: string,
  config: HelmrConfig,
): Promise<string[]> {
  const canonicalRoot = await realpath(root)
  await rejectReservedRoot(canonicalRoot)
  const candidates = new Set<string>()
  for (const configured of config.dirs) {
    const directory = resolve(canonicalRoot, configured)
    if (!inside(canonicalRoot, directory)) {
      throw new Error(`configured dir escapes the project root: ${configured}`)
    }
    const relativeDirectory = projectPath(canonicalRoot, directory)
    if (hasComponent(relativeDirectory, "node_modules")) {
      throw new Error(`configured dir enters the dependency namespace: ${configured}`)
    }
    if (hasComponent(relativeDirectory, ".helmr")) {
      throw new Error(`configured dir enters reserved Platform output: ${configured}`)
    }
    await requireUnlinkedDirectory(canonicalRoot, directory, configured)
    await appendCandidates(canonicalRoot, directory, candidates)
  }
  const modules = [...candidates].filter((path) =>
    !config.ignorePatterns.some((pattern) => matchesIgnorePattern(pattern, path))
  )
  modules.sort(compareUTF8)
  return modules
}

async function appendCandidates(
  root: string,
  directory: string,
  candidates: Set<string>,
): Promise<void> {
  const entries = await readdir(directory, { withFileTypes: true })
  entries.sort((left, right) => compareUTF8(left.name, right.name))
  for (const entry of entries) {
    const absolute = resolve(directory, entry.name)
    const path = projectPath(root, absolute)
    if (hasComponent(path, "node_modules")) continue
    if (entry.name === ".helmr") {
      throw new Error(`declaration tree contains reserved Platform output: ${path}`)
    }
    const metadata = await lstat(absolute)
    if (metadata.isSymbolicLink()) continue
    if (metadata.isDirectory()) {
      await appendCandidates(root, absolute, candidates)
      continue
    }
    if (
      metadata.isFile() &&
      executableExtension.test(path) &&
      !isDeclarationOnly(path) &&
      path !== "helmr.config.ts"
    ) {
      candidates.add(path)
      continue
    }
    if (!metadata.isFile()) {
      throw new Error(`unsupported declaration tree entry: ${path}`)
    }
  }
}

function isDeclarationOnly(path: string): boolean {
  return path.endsWith(".d.ts") ||
    path.endsWith(".d.mts") ||
    path.endsWith(".d.cts")
}

async function rejectReservedRoot(root: string): Promise<void> {
  try {
    await lstat(resolve(root, "helmr"))
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return
    throw error
  }
  throw new Error("project root helmr/ is reserved for Platform output")
}

async function requireUnlinkedDirectory(
  root: string,
  directory: string,
  configured: string,
): Promise<void> {
  let metadata
  try {
    metadata = await lstat(directory)
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new Error(`configured dir does not exist: ${configured}`)
    }
    throw error
  }
  if (!metadata.isDirectory()) {
    throw new Error(`configured dir is not a regular directory: ${configured}`)
  }
  if (await realpath(directory) !== directory || !inside(root, directory)) {
    throw new Error(`configured dir traverses a symbolic link: ${configured}`)
  }
}

function projectPath(root: string, value: string): string {
  return relative(root, value).split(sep).join("/")
}

function inside(root: string, value: string): boolean {
  const path = relative(root, value)
  return path === "" ||
    (!path.startsWith(`..${sep}`) && path !== ".." && !path.startsWith("/"))
}

function hasComponent(path: string, component: string): boolean {
  return path.split("/").includes(component)
}


function decodeGeneratedFile(value: Uint8Array): string {
  try {
    return textDecoder.decode(value)
  } catch {
    throw new Error("generated analysis file is not valid UTF-8")
  }
}
