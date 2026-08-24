import { compareUTF8, hasOnlyUnicodeScalarValues } from "./internal/utf8"

const arrayIsArray = Array.isArray
const arrayPrototype = Array.prototype
const defineProperty = Object.defineProperty
const objectPrototype = Object.prototype
const getOwnPropertyDescriptor = Object.getOwnPropertyDescriptor
const getOwnPropertyDescriptors = Object.getOwnPropertyDescriptors
const getPrototypeOf = Object.getPrototypeOf
const hasOwn = Object.hasOwn
const freeze = Object.freeze
const ownKeys = Reflect.ownKeys
const startsWith = String.prototype.startsWith.call.bind(
  String.prototype.startsWith,
) as (value: string, search: string) => boolean
const endsWith = String.prototype.endsWith.call.bind(
  String.prototype.endsWith,
) as (value: string, search: string) => boolean
const includes = String.prototype.includes.call.bind(
  String.prototype.includes,
) as (value: string, search: string) => boolean
const split = String.prototype.split.call.bind(
  String.prototype.split,
) as (value: string, separator: string) => string[]
const slice = String.prototype.slice.call.bind(
  String.prototype.slice,
) as (value: string, start: number, end?: number) => string
const charCodeAt = String.prototype.charCodeAt.call.bind(
  String.prototype.charCodeAt,
) as (value: string, index: number) => number
const regexpTest = RegExp.prototype.test.call.bind(
  RegExp.prototype.test,
) as (regexp: RegExp, value: string) => boolean

export interface HelmrConfigInput {
  readonly dirs: readonly string[]
  readonly ignorePatterns?: readonly string[]
}

export interface HelmrConfig {
  readonly dirs: readonly string[]
  readonly ignorePatterns: readonly string[]
}

export function defineConfig(input: HelmrConfigInput): HelmrConfig {
  return normalizeConfig(input)
}

export function inspectConfig(value: unknown): HelmrConfig {
  if (typeof value !== "object" || value === null) {
    throw new Error("config must be an ordinary object")
  }
  return normalizeConfig(value)
}

export function matchesIgnorePattern(pattern: string, path: string): boolean {
  const patternSegments = split(pattern, "/")
  const pathSegments = split(path, "/")
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
    value === "" ||
    !hasOnlyUnicodeScalarValues(value) ||
    startsWith(value, "/") ||
    includes(value, "\\") ||
    hasControl(value)
  ) {
    throw new Error("config dirs entries must be non-empty root-relative POSIX paths")
  }
  const normalized = startsWith(value, "./") ? slice(value, 2) : value
  const segments = split(normalized, "/")
  let invalidSegment = normalized === ""
  for (let index = 0; index < segments.length; index++) {
    const segment = segments[index]
    if (segment === "" || segment === "." || segment === "..") {
      invalidSegment = true
      break
    }
  }
  if (invalidSegment) {
    throw new Error("config dirs entries must be normalized root-relative paths")
  }
  return normalized
}

function validateIgnorePattern(value: unknown): string {
  if (
    typeof value !== "string" ||
    value === "" ||
    !hasOnlyUnicodeScalarValues(value) ||
    startsWith(value, "./") ||
    startsWith(value, "/") ||
    endsWith(value, "/") ||
    includes(value, "//") ||
    includes(value, "\\") ||
    hasControl(value) ||
    startsWith(value, "!") ||
    regexpTest(/[[\]{}]/, value) ||
    regexpTest(/[?*+@!]\(/, value)
  ) {
    throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`)
  }
  const segments = split(value, "/")
  for (let index = 0; index < segments.length; index++) {
    const segment = segments[index] as string
    if (segment === ".." || (includes(segment, "**") && segment !== "**")) {
      throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`)
    }
  }
  return value
}

function matchesSegment(pattern: string, value: string): boolean {
  const patternCharacters = codePoints(pattern)
  const valueCharacters = codePoints(value)
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
  for (let index = 0; index < value.length; index++) {
    const code = charCodeAt(value, index)
    if (code <= 0x1f || (code >= 0x7f && code <= 0x9f)) return true
  }
  return false
}

function codePoints(value: string): string[] {
  const result: string[] = []
  for (let index = 0; index < value.length;) {
    const first = charCodeAt(value, index)
    const width = first >= 0xd800 && first <= 0xdbff ? 2 : 1
    setArrayIndex(result, result.length, slice(value, index, index + width))
    index += width
  }
  return result
}

function normalizeConfig(value: object): HelmrConfig {
  if (arrayIsArray(value) || getPrototypeOf(value) !== objectPrototype) {
    throw new Error("config must be an ordinary object")
  }
  const descriptors = getOwnPropertyDescriptors(value)
  const keys = ownKeys(value)
  let invalidKey = !hasOwn(descriptors, "dirs")
  for (let index = 0; index < keys.length; index++) {
    const key = keys[index]
    if (
      typeof key !== "string" ||
      (key !== "dirs" && key !== "ignorePatterns")
    ) {
      invalidKey = true
      break
    }
  }
  if (invalidKey) {
    throw new Error("config requires exactly dirs and optional ignorePatterns")
  }
  for (let index = 0; index < keys.length; index++) {
    const key = keys[index]
    if (typeof key !== "string") {
      throw new Error("config requires exactly dirs and optional ignorePatterns")
    }
    const descriptor = descriptors[key]
    if (
      descriptor === undefined ||
      !descriptor.enumerable ||
      !hasOwn(descriptor, "value")
    ) {
      throw new Error("config properties must be enumerable data properties")
    }
  }
  const dirs = normalizeStringSet(
    descriptors["dirs"]?.value,
    "config dirs",
    validateDirectory,
    true,
  )
  const ignorePatterns = normalizeStringSet(
    hasOwn(descriptors, "ignorePatterns")
      ? descriptors["ignorePatterns"]?.value
      : [],
    "config ignorePatterns",
    validateIgnorePattern,
    false,
  )
  return freeze({
    dirs: freeze(dirs),
    ignorePatterns: freeze(ignorePatterns),
  })
}

function normalizeStringSet(
  value: unknown,
  name: string,
  normalize: (value: unknown) => string,
  nonempty: boolean,
): string[] {
  if (!arrayIsArray(value) || getPrototypeOf(value) !== arrayPrototype) {
    throw new Error(`${name} must be an array`)
  }
  const keys = ownKeys(value)
  const lengthDescriptor = getOwnPropertyDescriptor(value, "length")
  const length = lengthDescriptor?.value
  if (
    typeof length !== "number" ||
    keys.length !== length + 1 ||
    keys[length] !== "length"
  ) {
    throw new Error(`${name} must be a dense ordinary array`)
  }
  const normalized: string[] = []
  for (let index = 0; index < length; index++) {
    const key = `${index}`
    if (keys[index] !== key) {
      throw new Error(`${name} must be a dense ordinary array`)
    }
    const descriptor = getOwnPropertyDescriptor(value, key)
    if (
      descriptor === undefined ||
      !descriptor.enumerable ||
      !hasOwn(descriptor, "value")
    ) {
      throw new Error(`${name} entries must be enumerable data properties`)
    }
    const current = normalize(descriptor.value)
    let insertion = normalized.length
    while (
      insertion > 0 &&
      compareUTF8(current, normalized[insertion - 1] as string) < 0
    ) {
      setArrayIndex(
        normalized,
        insertion,
        normalized[insertion - 1] as string,
      )
      insertion--
    }
    setArrayIndex(normalized, insertion, current)
  }
  if (nonempty && length === 0) {
    throw new Error(`${name} must be non-empty`)
  }
  for (let index = 1; index < normalized.length; index++) {
    if (normalized[index] === normalized[index - 1]) {
      throw new Error(`${name} contains a duplicate entry`)
    }
  }
  return normalized
}

function setArrayIndex<T>(array: T[], index: number, value: T): void {
  defineProperty(array, `${index}`, {
    configurable: true,
    enumerable: true,
    value,
    writable: true,
  })
}
