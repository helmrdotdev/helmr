import { compile } from "@helmr/sdk/internal/compile"
import {
  isConfigDefinition,
  isTaskDefinition,
  type AnyTask,
  type HelmrConfig,
} from "@helmr/sdk/internal"
import type { Bundle } from "@helmr/proto"
import { readdir, stat, unlink } from "node:fs/promises"
import { tmpdir } from "node:os"
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

let nextTempModuleId = 0

const DEFAULT_ADAPTER_SDK_PATH = "/opt/helmr/adapter/sdk.js"
const ADAPTER_SDK_PATH_ENV = "HELMR_ADAPTER_SDK_PATH"
const TASK_FILE_EXTENSION = /\.(?:ts|tsx|mts|cts|js|jsx|mjs|cjs)$/
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
    moduleValue = await importBuiltConfig(configPath)
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

async function importBuiltConfig(configPath: string): Promise<unknown> {
  const result = await Bun.build({
    entrypoints: [configPath],
    target: "bun",
    format: "esm",
    sourcemap: "inline",
    plugins: [configSdkRedirectPlugin()],
  })
  if (!result.success) {
    throw new Error(`config bundle failed:\n${formatBuildLogs(result.logs)}`)
  }
  const output = result.outputs[0]
  if (output === undefined) {
    throw new Error("config bundle failed: Bun.build produced no output")
  }
  const bundlePath = resolve(
    tmpdir(),
    `helmr-config-${process.pid}-${Date.now()}-${mintTempModuleId()}.mjs`,
  )
  await Bun.write(bundlePath, await output.text())
  try {
    return await import(`${pathToFileURL(bundlePath).href}?helmr=${Date.now()}`)
  } finally {
    await unlink(bundlePath).catch(() => undefined)
  }
}

function configSdkRedirectPlugin(): Bun.BunPlugin {
  return {
    name: "helmr-config-sdk-redirect",
    setup(build) {
      build.onResolve({ filter: /^@helmr\/sdk$/ }, () => ({
        path: process.env[ADAPTER_SDK_PATH_ENV] || DEFAULT_ADAPTER_SDK_PATH,
      }))
      build.onResolve({ filter: /^@helmr\/sdk\/internal($|\/)/ }, (args) => {
        throw new Error(`@helmr/sdk/internal/* is not a public API (attempted: ${args.path})`)
      })
    },
  }
}

function mintTempModuleId(): string {
  nextTempModuleId += 1
  return String(nextTempModuleId)
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
    throw new Error("helmr.config.ts must default export defineConfig({ dirs: [...] })")
  }
  const config = (moduleValue as { readonly default: unknown }).default
  if (!isConfigDefinition(config)) {
    throw new Error("helmr.config.ts must default export defineConfig({ dirs: [...] })")
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
  uniqueFiles.sort((left, right) => projectRelativePath(cwd, left).localeCompare(projectRelativePath(cwd, right)))
  if (uniqueFiles.length === 0) {
    throw new Error(`no task files found in configured dirs:\n${config.dirs.map((dir) => `  - ${dir}`).join("\n")}`)
  }
  return uniqueFiles
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
  const entryPath = resolve(
    tmpdir(),
    `helmr-task-modules-${process.pid}-${Date.now()}-${mintTempModuleId()}.ts`,
  )
  const source = taskModuleAggregatorSource(files)
  await Bun.write(entryPath, source)
  let bundlePath: string | undefined
  try {
    let result: Bun.BuildOutput
    try {
      result = await Bun.build({
        entrypoints: [entryPath],
        target: "bun",
        format: "esm",
        sourcemap: "inline",
        plugins: [configSdkRedirectPlugin()],
      })
    } catch (error) {
      throw new Error(`task module bundle failed: ${formatConfigLoadError(error)}`)
    }
    if (!result.success) {
      throw new Error(`task module bundle failed:\n${formatBuildLogs(result.logs)}`)
    }
    const output = result.outputs[0]
    if (output === undefined) {
      throw new Error("task module bundle failed: Bun.build produced no output")
    }
    bundlePath = resolve(
      tmpdir(),
      `helmr-task-modules-${process.pid}-${Date.now()}-${mintTempModuleId()}.mjs`,
    )
    await Bun.write(bundlePath, await output.text())
    const moduleValue = await import(`${pathToFileURL(bundlePath).href}?helmr=${Date.now()}`)
    return readImportedTaskModules(moduleValue)
  } finally {
    await unlink(entryPath).catch(() => undefined)
    if (bundlePath !== undefined) {
      await unlink(bundlePath).catch(() => undefined)
    }
  }
}

function taskModuleAggregatorSource(files: readonly string[]): string {
  const imports = files
    .map((file, index) => `import * as m${index} from ${JSON.stringify(file)}`)
    .join("\n")
  const modules = files
    .map((file, index) => `{ path: ${JSON.stringify(file)}, exports: m${index} }`)
    .join(",\n")
  return `${imports}\nexport const modules = [\n${modules}\n]\n`
}

function readImportedTaskModules(moduleValue: unknown): readonly ImportedTaskModule[] {
  if (moduleValue === null || typeof moduleValue !== "object" || !("modules" in moduleValue)) {
    throw new Error("task module bundle did not export discovered modules")
  }
  const modules = (moduleValue as { readonly modules: unknown }).modules
  if (!Array.isArray(modules)) {
    throw new Error("task module bundle exported invalid discovered modules")
  }
  return modules.map((mod) => {
    if (mod === null || typeof mod !== "object" || !("path" in mod) || !("exports" in mod)) {
      throw new Error("task module bundle exported invalid discovered module entry")
    }
    const path = (mod as { readonly path: unknown }).path
    const exports = (mod as { readonly exports: unknown }).exports
    if (typeof path !== "string" || exports === null || typeof exports !== "object") {
      throw new Error("task module bundle exported invalid discovered module entry")
    }
    return { path, exports: exports as Record<string, unknown> }
  })
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

function formatBuildLogs(logs: readonly unknown[]): string {
  if (logs.length === 0) {
    return "(no build logs)"
  }
  return logs
    .map((log) => (typeof log === "object" && log !== null && "message" in log ? log.message : log))
    .map(String)
    .join("\n")
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
