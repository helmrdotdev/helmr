import {
  inspectConfig,
  type HelmrConfig,
} from "@helmr/sdk/internal"
import { lstat } from "node:fs/promises"
import { pathToFileURL } from "node:url"

export class MissingConfigError extends Error {
  constructor(path: string) {
    super(`missing helmr.config.ts at ${path}`)
    this.name = "MissingConfigError"
  }
}

export async function loadConfig(path: string): Promise<HelmrConfig> {
  let metadata
  try {
    metadata = await lstat(path)
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new MissingConfigError(path)
    }
    throw error
  }
  if (!metadata.isFile()) {
    throw new Error("helmr.config.ts must be a regular file")
  }
  let namespace: Record<string, unknown>
  try {
    const value: unknown = await import(pathToFileURL(path).href)
    if (typeof value !== "object" || value === null) {
      throw new Error("config did not evaluate to a module namespace")
    }
    namespace = value as Record<string, unknown>
  } catch (error) {
    throw new Error("failed to evaluate helmr.config.ts", { cause: error })
  }
  try {
    return inspectConfig(namespace["default"])
  } catch (error) {
    throw new Error("helmr.config.ts must default-export a valid config object", {
      cause: error,
    })
  }
}

export function inspectCanonicalConfig(value: unknown): HelmrConfig {
  if (
    typeof value !== "object" ||
    value === null ||
    Array.isArray(value) ||
    Object.getPrototypeOf(value) !== Object.prototype
  ) {
    throw new Error("canonical config must be an ordinary object")
  }
  const record = value as Record<string, unknown>
  const keys = Object.keys(record).sort()
  if (
    keys.length !== 2 ||
    keys[0] !== "dirs" ||
    keys[1] !== "ignorePatterns"
  ) {
    throw new Error("canonical config does not match the build contract")
  }
  return inspectConfig({
    dirs: record["dirs"],
    ignorePatterns: record["ignorePatterns"],
  })
}
