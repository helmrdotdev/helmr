import { compile } from "@helmr/sdk/internal/compile"
import {
  isConfigDefinition,
  isTaskDefinition,
  type AnyTask,
  type HelmrConfig,
} from "@helmr/sdk/internal"
import type { Bundle } from "@helmr/proto"
import { readdir, stat } from "node:fs/promises"
import { relative, resolve, sep } from "node:path"
import { pathToFileURL } from "node:url"

export interface ConfigTaskRef {
  readonly id: string
  readonly originFile: string
  readonly modulePath: string
  readonly exportName: string
  readonly task: AnyTask
}

export interface RegisteredTask {
  readonly originFile: string
  readonly modulePath: string
  readonly exportName: string
  readonly task: AnyTask
  readonly bundle: Bundle
}

export class MissingConfigError extends Error {
  constructor(cwd: string) {
    super(`no helmr.config.ts found at ${resolve(cwd, "helmr.config.ts")}`)
    this.name = "MissingConfigError"
  }
}

let nextImportVersion = 0

const TASK_FILE_EXTENSION = /\.(?:ts|mts|cts|js|mjs|cjs)$/
const DECLARATION_FILE_EXTENSION = /\.d\.(?:ts|mts|cts)$/

const DEFAULT_IGNORE_PATTERNS = [
  "**/*.test.*",
  "**/*.spec.*",
  "**/_*.*",
] as const

const HARD_IGNORE_PATTERNS = [
  "**/node_modules/**",
  "**/.git/**",
  "**/.helmr/**",
  "**/.next/**",
] as const

export async function loadConfigTaskRefs(cwd: string): Promise<readonly ConfigTaskRef[]> {
  const config = await loadConfig(cwd)
  const taskFiles = await discoverTaskFiles(cwd, config)
  return collectTaskRefs(cwd, await importDiscoveredTaskModules(taskFiles))
}

export async function loadConfig(cwd: string): Promise<HelmrConfig> {
  const configPath = resolve(cwd, "helmr.config.ts")
  await assertConfigFileExists(cwd, configPath)
  let moduleValue: unknown
  try {
    moduleValue = await importProjectModule(configPath, "helmr.config.ts")
  } catch (error) {
    const message = formatConfigLoadError(error)
    const duplicate = parseDuplicateTaskIdError(message)
    if (duplicate !== null) {
      throw duplicate
    }
    throw new Error(`failed to load helmr.config.ts: ${message}`)
  }
  return readDefaultConfig(moduleValue)
}

export async function loadTaskRegistry(cwd: string): Promise<ReadonlyMap<string, RegisteredTask>> {
  return buildTaskRegistry(await loadConfigTaskRefs(cwd))
}

export function buildTaskRegistry(
  refs: readonly ConfigTaskRef[],
): ReadonlyMap<string, RegisteredTask> {
  const registry = new Map<string, RegisteredTask>()
  for (const ref of refs) {
    const existing = registry.get(ref.id)
    if (existing) {
      throw new DuplicateTaskIdError(ref.id, [existing.originFile, ref.originFile])
    }
    registry.set(ref.id, {
      originFile: ref.originFile,
      modulePath: ref.modulePath,
      exportName: ref.exportName,
      task: ref.task,
      bundle: compile({ task: ref.task, modulePath: ref.modulePath, exportName: ref.exportName }),
    })
  }
  return registry
}

function readDefaultConfig(moduleValue: unknown): HelmrConfig {
  if (moduleValue === null || typeof moduleValue !== "object" || !("default" in moduleValue)) {
    throw new Error("helmr.config.ts must default export defineConfig({ project, dirs: [...] })")
  }
  const config = (moduleValue as { readonly default: unknown }).default
  if (!isConfigDefinition(config)) {
    throw new Error("helmr.config.ts must default export defineConfig({ project, dirs: [...] })")
  }
  return config
}

async function discoverTaskFiles(cwd: string, config: HelmrConfig): Promise<readonly string[]> {
  const matchers = compileIgnoreMatchers([
    ...(config.ignorePatterns ?? DEFAULT_IGNORE_PATTERNS),
    ...HARD_IGNORE_PATTERNS,
  ])
  const files: string[] = []
  for (const dir of config.dirs) {
    const root = resolve(cwd, dir)
    assertInsideProjectRoot(cwd, root, dir)
    await assertTaskDirExists(root, dir)
    await appendTaskFiles(cwd, root, matchers, files)
  }
  const uniqueFiles = [...new Set(files)]
  uniqueFiles.sort((left, right) => compareAscii(projectRelativePath(cwd, left), projectRelativePath(cwd, right)))
  if (uniqueFiles.length === 0) {
    throw new Error(`no task files found in configured dirs:\n${config.dirs.map((dir) => `  - ${dir}`).join("\n")}`)
  }
  return uniqueFiles
}

function compareAscii(left: string, right: string): number {
  if (left < right) return -1
  if (left > right) return 1
  return 0
}

function assertInsideProjectRoot(cwd: string, path: string, configuredDir: string): void {
  const rel = relative(cwd, path)
  if (rel === ".." || rel.startsWith(`..${sep}`)) {
    throw new Error(`configured task dir must be inside the project root: ${configuredDir}`)
  }
}

async function assertTaskDirExists(path: string, configuredDir: string): Promise<void> {
  let metadata
  try {
    metadata = await stat(path)
  } catch (error) {
    if ((error as NodeJS.ErrnoException | undefined)?.code === "ENOENT") {
      throw new Error(`configured task dir not found: ${configuredDir}`)
    }
    throw error
  }
  if (!metadata.isDirectory()) {
    throw new Error(`configured task dir is not a directory: ${configuredDir}`)
  }
}

async function appendTaskFiles(
  cwd: string,
  dir: string,
  ignoreMatchers: readonly RegExp[],
  files: string[],
): Promise<void> {
  const entries = await readdir(dir, { withFileTypes: true })
  for (const entry of entries) {
    const path = resolve(dir, entry.name)
    const rel = projectRelativePath(cwd, path)
    if (entry.isDirectory()) {
      if (!isIgnored(`${rel}/`, ignoreMatchers)) {
        await appendTaskFiles(cwd, path, ignoreMatchers, files)
      }
      continue
    }
    if (!entry.isFile() || !isTaskFile(path) || isIgnored(rel, ignoreMatchers)) {
      continue
    }
    files.push(path)
  }
}

function isTaskFile(path: string): boolean {
  return TASK_FILE_EXTENSION.test(path) && !DECLARATION_FILE_EXTENSION.test(path)
}

function compileIgnoreMatchers(patterns: readonly string[]): readonly RegExp[] {
  return patterns.map((pattern) => globPatternToRegExp(pattern))
}

function isIgnored(path: string, matchers: readonly RegExp[]): boolean {
  return matchers.some((matcher) => matcher.test(path))
}

function globPatternToRegExp(pattern: string): RegExp {
  const normalized = pattern.split(sep).join("/")
  let source = "^"
  for (let index = 0; index < normalized.length;) {
    const char = normalized[index]
    const next = normalized[index + 1]
    const afterNext = normalized[index + 2]
    if (char === "*" && next === "*" && afterNext === "/") {
      source += "(?:.*/)?"
      index += 3
      continue
    }
    if (char === "*" && next === "*") {
      source += ".*"
      index += 2
      continue
    }
    if (char === "*") {
      source += "[^/]*"
      index += 1
      continue
    }
    if (char === "?") {
      source += "[^/]"
      index += 1
      continue
    }
    source += escapeRegExp(char ?? "")
    index += 1
  }
  return new RegExp(`${source}$`)
}

function escapeRegExp(value: string): string {
  return /[\\^$.*+?()[\]{}|]/.test(value) ? `\\${value}` : value
}

interface ImportedTaskModule {
  readonly path: string
  readonly exports: Record<string, unknown>
}

async function importDiscoveredTaskModules(files: readonly string[]): Promise<readonly ImportedTaskModule[]> {
  return Promise.all(
    files.map(async (file) => ({
      path: file,
      exports: await importProjectModule(file, `task module ${file}`),
    })),
  )
}

async function importProjectModule(path: string, label: string): Promise<Record<string, unknown>> {
  const moduleValue = await import(`${pathToFileURL(path).href}?helmr=${Date.now()}-${mintImportVersion()}`)
  if (moduleValue === null || typeof moduleValue !== "object") {
    throw new Error(`${label} did not export an object`)
  }
  return moduleValue as Record<string, unknown>
}

function mintImportVersion(): string {
  nextImportVersion += 1
  return String(nextImportVersion)
}

function collectTaskRefs(cwd: string, modules: readonly ImportedTaskModule[]): readonly ConfigTaskRef[] {
  const refs: ConfigTaskRef[] = []
  for (const mod of modules) {
    const seen = new WeakSet<object>()
    for (const [exportName, value] of Object.entries(mod.exports)) {
      if (!isTaskDefinition(value)) {
        continue
      }
      if (exportName === "default") {
        throw new Error(
          `task file ${projectRelativePath(cwd, mod.path)} default-exports task "${value.id}"; use a named export instead`,
        )
      }
      if (seen.has(value)) {
        continue
      }
      seen.add(value)
      refs.push({
        id: value.id,
        originFile: mod.path,
        modulePath: projectRelativePath(cwd, mod.path),
        exportName,
        task: value,
      })
    }
  }
  if (refs.length === 0) {
    throw new Error("no named exports created by task(...) were found in configured dirs")
  }
  return refs
}

async function assertConfigFileExists(cwd: string, configPath: string): Promise<void> {
  try {
    const metadata = await stat(configPath)
    if (!metadata.isFile()) {
      throw new MissingConfigError(cwd)
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException | undefined)?.code === "ENOENT") {
      throw new MissingConfigError(cwd)
    }
    throw error
  }
}

function projectRelativePath(cwd: string, path: string): string {
  if (path === "unknown") {
    return "unknown"
  }
  for (const root of equivalentRoots(cwd)) {
    const rel = relative(root, path)
    if (!rel.startsWith("..") && rel !== "" && !rel.startsWith(`..${sep}`)) {
      return rel.split(sep).join("/")
    }
  }
  return path
}

function equivalentRoots(path: string): readonly string[] {
  const roots = [path]
  if (path.startsWith("/var/")) {
    roots.push(`/private${path}`)
  } else if (path.startsWith("/private/var/")) {
    roots.push(path.slice("/private".length))
  }
  return roots
}

function formatConfigLoadError(error: unknown): string {
  if (error instanceof Error) {
    return error.message
  }
  return String(error)
}

function parseDuplicateTaskIdError(message: string): DuplicateTaskIdError | null {
  const match = /^duplicate task id "([^"]+)":\n  - ([^\n]+)\n  - ([^\n]+)/.exec(message)
  if (!match?.[1] || !match[2] || !match[3]) {
    return null
  }
  return new DuplicateTaskIdError(match[1], [match[2], match[3]])
}

export class DuplicateTaskIdError extends Error {
  readonly id: string
  readonly originFiles: readonly [string, string]

  constructor(id: string, originFiles: readonly [string, string]) {
    super(`duplicate task id "${id}":\n  - ${originFiles[0]}\n  - ${originFiles[1]}`)
    this.name = "DuplicateTaskIdError"
    this.id = id
    this.originFiles = originFiles
  }
}
