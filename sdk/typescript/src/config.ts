import {
  markConfig,
  type HelmrConfig,
} from "./internal"

export interface HelmrConfigInput {
  readonly project: string
  readonly dirs: readonly string[]
  readonly ignorePatterns?: readonly string[]
}

export function defineConfig(config: HelmrConfigInput): HelmrConfig {
  if (config === null || typeof config !== "object") {
    throw new Error("defineConfig() requires an object")
  }
  if (!("project" in config)) {
    throw new Error("defineConfig({ project }) requires a non-empty string")
  }
  if (typeof config.project !== "string" || config.project.trim() === "") {
    throw new Error("defineConfig({ project }) must be a non-empty string")
  }
  if (config.project.includes("\0")) {
    throw new Error("defineConfig({ project }) must not contain NUL")
  }
  if (!("dirs" in config)) {
    throw new Error("defineConfig({ dirs }) requires a non-empty dirs array")
  }
  if (!Array.isArray(config.dirs) || config.dirs.length === 0) {
    throw new Error("defineConfig({ dirs }) requires a non-empty dirs array")
  }
  for (const dir of config.dirs) {
    if (typeof dir !== "string" || dir.trim() === "") {
      throw new Error("defineConfig({ dirs }) entries must be non-empty strings")
    }
    if (dir.includes("\0")) {
      throw new Error("defineConfig({ dirs }) entries must not contain NUL")
    }
  }
  if (config.ignorePatterns !== undefined) {
    if (!Array.isArray(config.ignorePatterns)) {
      throw new Error("defineConfig({ ignorePatterns }) must be an array of strings")
    }
    for (const pattern of config.ignorePatterns) {
      if (typeof pattern !== "string" || pattern.trim() === "") {
        throw new Error("defineConfig({ ignorePatterns }) entries must be non-empty strings")
      }
      if (pattern.includes("\0")) {
        throw new Error("defineConfig({ ignorePatterns }) entries must not contain NUL")
      }
    }
  }

  return markConfig({
    project: config.project,
    dirs: [...config.dirs],
    ...(config.ignorePatterns === undefined ? {} : { ignorePatterns: [...config.ignorePatterns] }),
  })
}

export type { HelmrConfig }
