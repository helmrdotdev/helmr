import {
  inspectConfig,
  type HelmrConfig,
} from "@helmr/sdk/internal"
import { stat } from "node:fs/promises"
import { pathToFileURL } from "node:url"

export class MissingConfigError extends Error {
  constructor(path: string) {
    super(`missing helmr.config.ts at ${path}`)
    this.name = "MissingConfigError"
  }
}

export async function loadConfig(path: string, importSourceExports: (url: URL) => Promise<Record<string, unknown>>): Promise<HelmrConfig> {
  let metadata
  try {
    metadata = await stat(path)
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
    const value: unknown = await importSourceExports(pathToFileURL(path))
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

// Only code and file selection cross into target compilation. Environment
// preparation, install commands and secret names remain host-owned.
export type DiscoveryConfig = Pick<HelmrConfig, "dirs" | "ignorePatterns"> & {
  readonly external: readonly string[]
  readonly assets: readonly string[]
}

export function inspectCanonicalConfig(value: unknown): DiscoveryConfig {
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
    keys.length !== 4 ||
    keys[0] !== "assets" || keys[1] !== "dirs" ||
    keys[2] !== "external" || keys[3] !== "ignorePatterns"
  ) {
    throw new Error("canonical config does not match the build contract")
  }
  const config = inspectConfig({
    dirs: record["dirs"],
    ignorePatterns: record["ignorePatterns"],
    build: { external: record["external"], assets: record["assets"] },
  })
  return { dirs: config.dirs, ignorePatterns: config.ignorePatterns, external: config.build.external, assets: config.build.assets }
}
