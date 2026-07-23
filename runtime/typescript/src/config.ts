import {
  inspectConfig,
  type HelmrConfig,
} from "@helmr/sdk/internal"
import { lstat } from "node:fs/promises"
import { resolve } from "node:path"
import { pathToFileURL } from "node:url"

export class MissingConfigError extends Error {
  constructor(path: string) {
    super(`missing helmr.config.ts at ${path}`)
    this.name = "MissingConfigError"
  }
}

export async function loadConfig(root: string): Promise<HelmrConfig> {
  const path = resolve(root, "helmr.config.ts")
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
  const config = inspectConfig(namespace["default"])
  if (config === undefined) {
    throw new Error("helmr.config.ts must default-export defineConfig()")
  }
  return config
}
