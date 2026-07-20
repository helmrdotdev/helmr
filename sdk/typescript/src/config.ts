const configBrand = Symbol.for("helmr.sdk.v0.config")

export interface HelmrConfigInput {
  readonly project: string
  readonly dirs: readonly string[]
  readonly ignorePatterns?: readonly string[]
}

export interface HelmrConfig {
  readonly project: string
  readonly dirs: readonly string[]
  readonly ignorePatterns: readonly string[]
}

type BrandedConfig = HelmrConfig & {
  readonly [configBrand]: true
}

export function defineConfig(input: HelmrConfigInput): HelmrConfig {
  if (
    input === null ||
    typeof input !== "object" ||
    Array.isArray(input) ||
    !hasExactKeys(input as unknown as Record<string, unknown>, [
      "dirs",
      "ignorePatterns",
      "project",
    ])
  ) {
    throw new Error("defineConfig() requires exactly project, dirs, and optional ignorePatterns")
  }
  if (
    typeof input.project !== "string" ||
    input.project.trim() === "" ||
    hasControl(input.project)
  ) {
    throw new Error("defineConfig({ project }) requires a non-empty string without controls")
  }
  if (!Array.isArray(input.dirs) || input.dirs.length === 0) {
    throw new Error("defineConfig({ dirs }) requires a non-empty array")
  }
  const dirs = input.dirs.map(validateDirectory)
  if (
    input.ignorePatterns !== undefined &&
    !Array.isArray(input.ignorePatterns)
  ) {
    throw new Error("defineConfig({ ignorePatterns }) must be an array")
  }
  const ignorePatterns = (input.ignorePatterns ?? []).map(validateIgnorePattern)
  const config = {
    project: input.project,
    dirs: Object.freeze(dirs),
    ignorePatterns: Object.freeze(ignorePatterns),
  }
  Object.defineProperty(config, configBrand, { value: true })
  return Object.freeze(config)
}

export function inspectConfig(value: unknown): HelmrConfig | undefined {
  if (typeof value !== "object" || value === null) return undefined
  if (!Object.hasOwn(value, configBrand)) return undefined
  if ((value as Partial<BrandedConfig>)[configBrand] !== true) {
    throw new Error("invalid defineConfig() private record")
  }
  const config = value as Partial<HelmrConfig>
  if (
    typeof config.project !== "string" ||
    config.project.trim() === "" ||
    hasControl(config.project) ||
    !Array.isArray(config.dirs) ||
    config.dirs.length === 0 ||
    !Array.isArray(config.ignorePatterns)
  ) {
    throw new Error("invalid defineConfig() private record")
  }
  for (const directory of config.dirs) validateDirectory(directory)
  for (const pattern of config.ignorePatterns) validateIgnorePattern(pattern)
  return value as HelmrConfig
}

export function matchesIgnorePattern(pattern: string, path: string): boolean {
  const patternSegments = pattern.split("/")
  const pathSegments = path.split("/")
  const matches = (
    patternIndex: number,
    pathIndex: number,
  ): boolean => {
    if (patternIndex === patternSegments.length) {
      return pathIndex === pathSegments.length
    }
    const segment = patternSegments[patternIndex] as string
    if (segment === "**") {
      if (patternIndex === patternSegments.length - 1) {
        return pathIndex < pathSegments.length
      }
      for (
        let candidate = pathIndex;
        candidate <= pathSegments.length;
        candidate++
      ) {
        if (matches(patternIndex + 1, candidate)) return true
      }
      return false
    }
    return (
      pathIndex < pathSegments.length &&
      matchesSegment(segment, pathSegments[pathIndex] as string) &&
      matches(patternIndex + 1, pathIndex + 1)
    )
  }
  return matches(0, 0)
}

function validateDirectory(value: unknown): string {
  if (
    typeof value !== "string" ||
    !value.startsWith("./") ||
    value.includes("\\") ||
    value.includes("?") ||
    value.includes("#") ||
    hasControl(value)
  ) {
    throw new Error("defineConfig({ dirs }) entries must be project-relative POSIX directories beginning ./")
  }
  if (value !== "./") {
    const segments = value.slice(2).split("/")
    if (
      segments.some((segment) =>
        segment === "" || segment === "." || segment === ".."
      )
    ) {
      throw new Error(
        "defineConfig({ dirs }) entries must be normalized project-relative paths",
      )
    }
  }
  return value
}

function validateIgnorePattern(value: unknown): string {
  if (
    typeof value !== "string" ||
    value === "" ||
    value.startsWith("./") ||
    value.startsWith("/") ||
    value.endsWith("/") ||
    value.includes("//") ||
    value.includes("\\") ||
    value.split("/").includes("..") ||
    hasControl(value) ||
    value.startsWith("!") ||
    /[[\]{}]/.test(value) ||
    /[?*+@!]\(/.test(value) ||
    value.split("/").some((segment) =>
      segment.includes("**") && segment !== "**"
    )
  ) {
    throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`)
  }
  return value
}

function matchesSegment(pattern: string, value: string): boolean {
  const patternCharacters = Array.from(pattern)
  const valueCharacters = Array.from(value)
  let patternIndex = 0
  let valueIndex = 0
  let star = -1
  let starValue = -1
  while (valueIndex < valueCharacters.length) {
    const token = patternCharacters[patternIndex]
    if (token === "?" || token === valueCharacters[valueIndex]) {
      patternIndex++
      valueIndex++
      continue
    }
    if (token === "*") {
      star = patternIndex++
      starValue = valueIndex
      continue
    }
    if (star !== -1) {
      patternIndex = star + 1
      valueIndex = ++starValue
      continue
    }
    return false
  }
  while (patternCharacters[patternIndex] === "*") patternIndex++
  return patternIndex === patternCharacters.length
}

function hasControl(value: string): boolean {
  for (const character of value) {
    const code = character.codePointAt(0) as number
    if (code <= 0x1f || (code >= 0x7f && code <= 0x9f)) return true
  }
  return false
}

function hasExactKeys(
  value: Record<string, unknown>,
  allowed: readonly string[],
): boolean {
  const keys = Object.keys(value)
  return (
    keys.every((key) => allowed.includes(key)) &&
    allowed
      .filter((key) => key !== "ignorePatterns")
      .every((key) => Object.hasOwn(value, key))
  )
}
