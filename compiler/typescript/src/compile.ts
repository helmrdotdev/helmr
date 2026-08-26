import {
  inspectDefinition,
  inspectImage,
  isQueue,
  inspectSandboxDefinition,
  validateQueueName,
  type InternalImage,
  type InternalImageStep,
  type InternalActorDefinition,
  type InternalDefinition,
  type InternalTaskDefinition,
  type InternalSandboxDefinition,
  type EncodedWorkspaceSecret,
  type ProgramDeclaration,
  type RuntimeArchitecture,
} from "@helmr/sdk/internal"
import { canonicalizeJsonValue, type JsonValue } from "@helmr/sdk/internal"
import { compareUTF8, hasOnlyUnicodeScalarValues } from "./utf8"

export const BUILD_PLAN_FORMAT_VERSION = 0 as const
export const DECLARATION_LOCATOR_FORMAT_VERSION = 0 as const
export const PROGRAM_ENTRYPOINT =
  'import { runProgram } from "file:///opt/helmr/runtime/helmr/entry.mjs";\nawait runProgram(new URL("./declarations.json", import.meta.url));\n' as const

export type NormalizedRetry =
  | Readonly<{ enabled: false }>
  | Readonly<{
      enabled: true
      maxAttempts: number
      backoff: Readonly<{
        minMs: number
        maxMs: number
        factor: number
        jitter: "none" | "full"
      }>
    }>

export interface NormalizedRunManifest {
  readonly queue: string
  readonly maxDurationMs: number
  readonly retry: NormalizedRetry
  readonly ttlMs?: number
}

export type BuildPlanDefinition =
  | Readonly<{
      kind: "task"
      declaredId: string
      manifest: Readonly<{
        payload:
          | Readonly<{ kind: "none" }>
          | Readonly<{ kind: "standard_schema" }>
        run: NormalizedRunManifest
        schedule?: Readonly<{
          cron: string
          timezone: string
          workspace: Readonly<{
            sandboxId: string
            secrets: readonly EncodedWorkspaceSecret[]
          }>
        }>
      }>
    }>
  | Readonly<{
      kind: "actor"
      declaredId: string
      manifest: Readonly<{
        run: NormalizedRunManifest
        idleTimeoutMs: number
      }>
    }>
  | Readonly<{
      kind: "sandbox"
      declaredId: string
      manifest: Readonly<{
        imageBuild: ImageBuild
        resources: Readonly<{
          milliCpu: number
          memoryMiB: number
        }>
      }>
    }>

export interface BuildPlanQueue {
  readonly name: string
  readonly concurrencyLimit?: number
}

export interface BuildPlan {
  readonly formatVersion: 0
  readonly definitions: readonly BuildPlanDefinition[]
  readonly queues: readonly BuildPlanQueue[]
}

export interface ImageBuild {
  readonly root: string
  readonly images: readonly ImageSpec[]
}

export interface ImageSpec {
  readonly key: string
  readonly platform: Readonly<{
    os: "linux"
    architecture: RuntimeArchitecture
  }>
  readonly steps: readonly ImageStep[]
}

export type ImageStep =
  | Readonly<{
      from: Readonly<{
        ref: string
      }>
    }>
  | Readonly<{
      run: Readonly<{
        argv: readonly string[]
      }>
    }>
  | Readonly<{
      copySourceFile: Readonly<{
        dst: string
        path: string
      }>
    }>
  | Readonly<{
      copySourceDir: Readonly<{
        dst: string
        path: string
      }>
    }>
  | Readonly<{
      copyFromImage: Readonly<{
        dst: string
        imageKey: string
        srcPath: string
      }>
    }>
  | Readonly<{ workdir: Readonly<{ path: string }> }>
  | Readonly<{ user: Readonly<{ name: string }> }>
  | Readonly<{ env: Readonly<{ key: string; value: string }> }>

export interface DeclarationLocatorEntry {
  readonly declaredId: string
  readonly exportName: string
  readonly kind: "task" | "actor"
  readonly modulePath: string
  readonly slot: "handler"
}

export interface DeclarationLocator {
  readonly declarations: readonly DeclarationLocatorEntry[]
  readonly formatVersion: 0
}

export interface AnalysisExport {
  readonly modulePath: string
  readonly exportName: string
  readonly value: unknown
}

export interface AnalyzeOptions {
  readonly architecture: RuntimeArchitecture
  readonly exports: readonly AnalysisExport[]
}

export interface AnalysisResult {
  readonly buildPlan: BuildPlan
  readonly buildPlanBytes: Uint8Array
  readonly declarationLocator: DeclarationLocator
  readonly declarationLocatorBytes: Uint8Array
  readonly programDeclarations: readonly ProgramDeclaration[]
  readonly entrypointBytes: Uint8Array
}

export interface ProgramExportAnalysis {
  readonly declarationLocator: DeclarationLocator
  readonly programDeclarations: readonly ProgramDeclaration[]
}

interface LocatedDefinition {
  readonly definition: InternalDefinition | InternalSandboxDefinition
  readonly modulePath: string
  readonly exportName: string
  readonly value: object
}

export function analyze(options: AnalyzeOptions): AnalysisResult {
  const located = locateDefinitions(options)
  const queues = compileQueues(located, options.exports)
  const sandboxExports = new Map<string, object>()
  for (const item of located) {
    if (item.definition.kind === "sandbox") {
      sandboxExports.set(item.definition.id, item.value)
    }
  }
  const definitions = located.map(({ definition }) =>
    compileDefinition(definition, options, queues, sandboxExports),
  )
  const programExports = compileProgramExports(located)
  const buildPlan: BuildPlan = Object.freeze({
    formatVersion: BUILD_PLAN_FORMAT_VERSION,
    definitions: Object.freeze(definitions),
    queues: Object.freeze(
      [...queues.values()]
        .map((entry) => Object.freeze({ ...entry }))
        .sort((left, right) => compareUTF8(left.name, right.name)),
    ),
  })
  const declarationLocator: DeclarationLocator = Object.freeze({
    declarations: programExports.declarationLocator.declarations,
    formatVersion: DECLARATION_LOCATOR_FORMAT_VERSION,
  })
  return {
    buildPlan,
    buildPlanBytes: canonicalizeJsonValue(buildPlan as unknown as JsonValue),
    declarationLocator,
    declarationLocatorBytes: canonicalizeJsonValue(
      declarationLocator as unknown as JsonValue,
    ),
    programDeclarations: programExports.programDeclarations,
    entrypointBytes: new TextEncoder().encode(PROGRAM_ENTRYPOINT),
  }
}

export function analyzeProgramExports(
  options: AnalyzeOptions,
): ProgramExportAnalysis {
  return compileProgramExports(locateDefinitions(options))
}

function locateDefinitions(options: AnalyzeOptions): LocatedDefinition[] {
  if (options.architecture !== "x86_64") {
    throw new Error(`unsupported architecture ${JSON.stringify(options.architecture)}`)
  }
  const located = discoverDefinitions(options.exports)
  if (located.length === 0) {
    throw new Error("BuildPlan definitions must be non-empty")
  }
  if (located.length > 10_000) {
    throw new Error("BuildPlan definitions exceed 10000")
  }
  located.sort(compareLocatedDefinitions)
  return located
}

function compileProgramExports(
  located: readonly LocatedDefinition[],
): ProgramExportAnalysis {
  const declarations = located.flatMap((item) =>
    item.definition.kind === "sandbox" ? [] : [locatorEntry(item)],
  )
  const programDeclarations = located.flatMap(({ definition }) =>
    definition.kind === "sandbox"
      ? []
      : [programDeclaration(definition)],
  )
  return Object.freeze({
    declarationLocator: Object.freeze({
      declarations: Object.freeze(declarations),
      formatVersion: DECLARATION_LOCATOR_FORMAT_VERSION,
    }),
    programDeclarations: Object.freeze(programDeclarations),
  })
}

export function normalizeWorkspaceResources(
  resources: Readonly<{ cpu: number; memory: string }>,
): Readonly<{
  milliCpu: number
  memoryMiB: number
}> {
  return Object.freeze({
    milliCpu: normalizeCpu(resources.cpu),
    memoryMiB: normalizeIecMiB(resources.memory, "memory"),
  })
}

function discoverDefinitions(
  exports: readonly AnalysisExport[],
): LocatedDefinition[] {
  const identities = new Map<string, {
    readonly value: object
    located: LocatedDefinition
  }>()
  for (const item of exports) {
    const definition =
      inspectDefinition(item.value) ?? inspectSandboxDefinition(item.value)
    if (definition === undefined) continue
    validateModulePath(item.modulePath)
    validateExportName(item.exportName)
    const key = `${definition.kind}\0${definition.id}`
    const existing = identities.get(key)
    if (existing !== undefined) {
      if (existing.value === item.value) {
        const candidate = {
          definition,
          modulePath: item.modulePath,
          exportName: item.exportName,
          value: item.value as object,
        }
        if (compareLocatorOccurrence(candidate, existing.located) < 0) {
          existing.located = candidate
        }
        continue
      }
      throw new Error(
        `duplicate ${definition.kind} declaration ${JSON.stringify(definition.id)} at ${existing.located.modulePath}#${existing.located.exportName} and ${item.modulePath}#${item.exportName}`,
      )
    }
    const located = {
      definition,
      modulePath: item.modulePath,
      exportName: item.exportName,
      value: item.value as object,
    }
    identities.set(key, {
      value: item.value as object,
      located,
    })
  }
  return [...identities.values()].map((item) => item.located)
}

function compileDefinition(
  definition: InternalDefinition | InternalSandboxDefinition,
  options: AnalyzeOptions,
  queues: ReadonlyMap<string, BuildPlanQueue>,
  sandboxExports: ReadonlyMap<string, object>,
): BuildPlanDefinition {
  switch (definition.kind) {
    case "task":
      return {
        kind: "task",
        declaredId: definition.id,
        manifest: {
          payload: {
            kind: definition.hasPayload ? "standard_schema" : "none",
          },
          run: normalizeRun(definition, "task", queues),
          ...(definition.schedule === undefined
            ? {}
            : {
                schedule: compileSchedule(definition, sandboxExports),
              }),
        },
      }
    case "actor":
      return {
        kind: "actor",
        declaredId: definition.id,
        manifest: {
          run: normalizeRun(definition, "actor", queues),
          idleTimeoutMs:
            definition.idleTimeout === undefined
              ? 30_000
              : normalizeDuration(
                  definition.idleTimeout,
                  `actor ${JSON.stringify(definition.id)} idleTimeout`,
                  1,
                  3_600_000,
                ),
        },
      }
    case "sandbox":
      return {
        kind: "sandbox",
        declaredId: definition.id,
        manifest: {
          imageBuild: compileImageBuild(definition.image, options),
          resources: normalizeWorkspaceResources(definition.resources),
        },
      }
  }
}

function compileSchedule(
  definition: InternalTaskDefinition,
  sandboxExports: ReadonlyMap<string, object>,
): NonNullable<
  Extract<BuildPlanDefinition, { kind: "task" }>["manifest"]["schedule"]
> {
  const schedule = definition.schedule
  if (schedule === undefined) throw new Error("Task schedule is undefined")
  const sandbox = inspectSandboxDefinition(schedule.workspace.sandbox)
  if (sandbox === undefined) {
    throw new Error(
      `task ${JSON.stringify(definition.id)} schedule has an invalid Sandbox definition`,
    )
  }
  const exported = sandboxExports.get(sandbox.id)
  if (exported === undefined) {
    throw new Error(
      `task ${JSON.stringify(definition.id)} schedule references unexported Sandbox ${JSON.stringify(sandbox.id)}`,
    )
  }
  if (exported !== schedule.workspace.sandbox) {
    throw new Error(
      `task ${JSON.stringify(definition.id)} schedule references a different Sandbox object than the exported definition ${JSON.stringify(sandbox.id)}`,
    )
  }
  return {
    cron: schedule.cron,
    timezone: schedule.timezone,
    workspace: {
      sandboxId: sandbox.id,
      secrets: schedule.workspace.secrets,
    },
  }
}

function compileQueues(
  located: readonly LocatedDefinition[],
  exports: readonly AnalysisExport[],
): Map<string, BuildPlanQueue> {
  const queues = new Map<string, QueueEntry>()
  for (const item of exports) {
    if (isQueue(item.value)) {
      addQueue(queues, item.value, item.value)
    }
  }
  for (const { definition } of located) {
    if (definition.kind !== "task" && definition.kind !== "actor") continue
    if (typeof definition.queue === "object") {
      addQueue(queues, definition.queue, definition.queue)
    } else if (definition.queue === undefined) {
      addQueue(queues, {
        name: `${definition.kind}/${definition.id}`,
      }, definition)
    }
  }
  for (const { definition } of located) {
    if (
      (definition.kind === "task" || definition.kind === "actor") &&
      typeof definition.queue === "string" &&
      !queues.has(definition.queue)
    ) {
      throw new Error(
        `${definition.kind} ${JSON.stringify(definition.id)} references undefined queue ${JSON.stringify(definition.queue)}`,
      )
    }
  }
  if (queues.size > 1000) throw new Error("BuildPlan queues exceed 1000")
  return new Map(
    [...queues].map(([name, entry]) => [name, entry.queue]),
  )
}

interface QueueEntry {
  readonly owner: object
  readonly queue: BuildPlanQueue
}

function addQueue(
  queues: Map<string, QueueEntry>,
  queue: { readonly name: string; readonly concurrencyLimit?: number | null },
  owner: object,
): void {
  validateQueueName(queue.name)
  const next: BuildPlanQueue = {
    name: queue.name,
    ...(queue.concurrencyLimit === undefined ||
    queue.concurrencyLimit === null
      ? {}
      : { concurrencyLimit: queue.concurrencyLimit }),
  }
  const existing = queues.get(queue.name)
  if (existing !== undefined) {
    if (existing.owner === owner) return
    throw new Error(`duplicate queue declaration ${JSON.stringify(queue.name)}`)
  }
  queues.set(queue.name, { owner, queue: next })
}

function normalizeRun(
  definition: InternalTaskDefinition | InternalActorDefinition,
  kind: "task" | "actor",
  queues: ReadonlyMap<string, BuildPlanQueue>,
): NormalizedRunManifest {
  const queue =
    definition.queue === undefined
      ? `${kind}/${definition.id}`
      : typeof definition.queue === "string"
        ? definition.queue
        : definition.queue.name
  if (!queues.has(queue)) {
    throw new Error(`${kind} ${JSON.stringify(definition.id)} queue is undefined`)
  }
  const maxDurationMs =
    definition.maxDuration === undefined
      ? 900_000
      : normalizeDuration(
          definition.maxDuration,
          `${kind} ${JSON.stringify(definition.id)} maxDuration`,
          5_000,
          86_400_000,
        )
  return {
    queue,
    maxDurationMs,
    retry: normalizeRetry(definition.retry),
    ...(definition.ttl === undefined
      ? {}
      : {
          ttlMs: normalizeDuration(
            definition.ttl,
            `${kind} ${JSON.stringify(definition.id)} ttl`,
            1,
            31_536_000_000,
          ),
        }),
  }
}

function normalizeRetry(
  retry: InternalTaskDefinition["retry"],
): NormalizedRetry {
  if (retry === undefined || retry.enabled === false) {
    return { enabled: false }
  }
  if (!Number.isInteger(retry.maxAttempts) || retry.maxAttempts < 1 || retry.maxAttempts > 10) {
    throw new Error("retry maxAttempts must be an integer in [1,10]")
  }
  const minMs =
    retry.backoff?.minDelay === undefined
      ? 1_000
      : normalizeDuration(
          retry.backoff.minDelay,
          "retry backoff minDelay",
          1,
          86_400_000,
        )
  const maxMs =
    retry.backoff?.maxDelay === undefined
      ? 30_000
      : normalizeDuration(
          retry.backoff.maxDelay,
          "retry backoff maxDelay",
          1,
          86_400_000,
        )
  const factor = retry.backoff?.factor ?? 2
  const jitter = retry.backoff?.jitter ?? "full"
  if (minMs > maxMs) {
    throw new Error("retry backoff minDelay must not exceed maxDelay")
  }
  if (!Number.isSafeInteger(factor) || factor < 1 || factor > 100) {
    throw new Error("retry backoff factor must be an integer in [1,100]")
  }
  if (jitter !== "none" && jitter !== "full") {
    throw new Error("retry backoff jitter must be none or full")
  }
  return {
    enabled: true,
    maxAttempts: retry.maxAttempts,
    backoff: { minMs, maxMs, factor, jitter },
  }
}

function compileImageBuild(
  root: InternalImage,
  options: AnalyzeOptions,
): ImageBuild {
  const images = new Map<string, InternalImage>()
  const visiting = new Set<string>()
  const visit = (image: InternalImage): void => {
    if (visiting.has(image.key)) {
      throw new Error(`image graph contains a cycle at ${JSON.stringify(image.key)}`)
    }
    const existing = images.get(image.key)
    if (existing !== undefined) {
      if (existing !== image) {
        throw new Error(`image key ${JSON.stringify(image.key)} is not unique`)
      }
      return
    }
    visiting.add(image.key)
    images.set(image.key, image)
    for (const step of image.steps) {
      if (step.kind === "copy_from_image") {
        const source = inspectImage(step.source)
        if (source === undefined) throw new Error("invalid copyFrom image")
        visit(source)
      }
    }
    visiting.delete(image.key)
  }
  visit(root)
  const specs = [...images.values()]
    .sort((left, right) => compareUTF8(left.key, right.key))
    .map((image) => ({
      key: image.key,
      platform: {
        os: "linux" as const,
        architecture: options.architecture,
      },
      steps: image.steps.map((step) => compileImageStep(step, options)),
    }))
  const stepCount = specs.reduce((total, image) => total + image.steps.length, 0)
  if (stepCount > 10_000) throw new Error("image build exceeds 10000 steps")
  return {
    root: root.key,
    images: specs,
  }
}

function compileImageStep(
  step: InternalImageStep,
  options: AnalyzeOptions,
): ImageStep {
  switch (step.kind) {
    case "from":
      assertExactKeys(step, ["kind", "ref"], "image from step")
      return { from: { ref: step.ref } }
    case "run":
      assertExactKeys(step, ["argv", "kind"], "image run step")
      return {
        run: {
          argv: [...step.argv],
        },
      }
    case "copy_source_file":
      assertExactKeys(
        step,
        ["destination", "kind", "source"],
        "image source-file copy step",
      )
      return {
        copySourceFile: {
          dst: step.destination,
          path: step.source.path,
        },
      }
    case "copy_source_directory":
      assertExactKeys(
        step,
        ["destination", "kind", "source"],
        "image source-directory copy step",
      )
      return {
        copySourceDir: {
          dst: step.destination,
          path: step.source.path,
        },
      }
    case "copy_from_image": {
      assertExactKeys(
        step,
        ["destination", "kind", "source", "sourcePath"],
        "image cross-image copy step",
      )
      const source = inspectImage(step.source)
      if (source === undefined) throw new Error("invalid copyFrom image")
      return {
        copyFromImage: {
          dst: step.destination,
          imageKey: source.key,
          srcPath: step.sourcePath,
        },
      }
    }
    case "workdir":
      assertExactKeys(step, ["kind", "path"], "image workdir step")
      return { workdir: { path: step.path } }
    case "env":
      assertExactKeys(step, ["key", "kind", "value"], "image env step")
      return { env: { key: step.key, value: step.value } }
    case "user":
      assertExactKeys(step, ["kind", "name"], "image user step")
      return { user: { name: step.name } }
  }
}

function assertExactKeys(
  value: object,
  expected: readonly string[],
  label: string,
): void {
  const actual = Object.keys(value).sort(compareUTF8)
  if (
    actual.length !== expected.length ||
    actual.some((key, index) => key !== expected[index])
  ) {
    throw new Error(`${label} has unknown members`)
  }
}

function locatorEntry(item: LocatedDefinition): DeclarationLocatorEntry {
  if (item.definition.kind === "sandbox") {
    throw new Error("Sandbox has no executable locator")
  }
  return {
    declaredId: item.definition.id,
    exportName: item.exportName,
    kind: item.definition.kind,
    modulePath: item.modulePath,
    slot: "handler",
  }
}

function programDeclaration(definition: InternalDefinition): ProgramDeclaration {
  switch (definition.kind) {
    case "task":
      return {
        kind: "task",
        declaredId: definition.id,
        slots: definition.hasPayload
          ? ["handler", "payloadSchema"]
          : ["handler"],
      }
    case "actor":
      return {
        kind: "actor",
        declaredId: definition.id,
        slots: ["handler"],
      }
  }
}

function normalizeCpu(cpu: number): number {
  if (!Number.isFinite(cpu) || cpu <= 0) {
    throw new Error("workspace cpu must be a finite positive number")
  }
  const text = cpu.toString()
  const match = /^(\d+)(?:\.(\d+))?(?:e([+-]?\d+))?$/i.exec(text)
  if (match === null) throw new Error("workspace cpu cannot be normalized")
  const integer = match[1] as string
  const fraction = match[2] ?? ""
  const exponent = Number(match[3] ?? "0")
  const significand = BigInt(`${integer}${fraction}`)
  const scale = exponent - fraction.length + 3
  let milliCpu: bigint
  if (scale >= 0) {
    milliCpu = significand * 10n ** BigInt(scale)
  } else {
    const divisor = 10n ** BigInt(-scale)
    if (significand % divisor !== 0n) {
      throw new Error("workspace cpu must resolve to whole milliCPU")
    }
    milliCpu = significand / divisor
  }
  return safePositiveNumber(milliCpu, "workspace milliCPU")
}

function normalizeIecMiB(value: string, label: string): number {
  const match = /^([1-9]\d*)(MiB|GiB)$/.exec(value)
  if (match === null) {
    throw new Error(
      `workspace ${label} must be a positive canonical integer suffixed by MiB or GiB`,
    )
  }
  const result =
    BigInt(match[1] as string) * (match[2] === "GiB" ? 1024n : 1n)
  return safePositiveNumber(result, `workspace ${label} MiB`)
}

function normalizeDuration(
  value: string,
  label: string,
  minimumMs: number,
  maximumMs: number,
): number {
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(value)
  if (match === null) {
    throw new Error(
      `${label} must match ^[1-9][0-9]*(ms|s|m|h|d)$`,
    )
  }
  const multipliers: Readonly<Record<string, bigint>> = {
    ms: 1n,
    s: 1_000n,
    m: 60_000n,
    h: 3_600_000n,
    d: 86_400_000n,
  }
  const milliseconds =
    BigInt(match[1] as string) * (multipliers[match[2] as string] as bigint)
  if (
    milliseconds < BigInt(minimumMs) ||
    milliseconds > BigInt(maximumMs)
  ) {
    throw new Error(
      `${label} must resolve to milliseconds in [${minimumMs},${maximumMs}]`,
    )
  }
  return Number(milliseconds)
}

function safePositiveNumber(value: bigint, label: string): number {
  if (value <= 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`${label} must be a positive safe integer`)
  }
  return Number(value)
}

function validateModulePath(path: string): void {
  const suffixes = [
    ".cjs",
    ".cts",
    ".js",
    ".jsx",
    ".mjs",
    ".mts",
    ".ts",
    ".tsx",
  ]
  const components = path.split("/")
  if (
    path.length === 0 ||
    !hasOnlyUnicodeScalarValues(path) ||
    path.startsWith("/") ||
    path.includes("\\") ||
    /[\p{Cc}]/u.test(path) ||
    components.some(
      (component) =>
        component === "" || component === "." || component === "..",
    ) ||
    components.includes("node_modules") ||
    components[0] === "helmr" ||
    path.endsWith(".d.ts") ||
    path.endsWith(".d.mts") ||
    path.endsWith(".d.cts") ||
    !suffixes.some((suffix) => path.endsWith(suffix))
  ) {
    throw new Error(
      `modulePath ${JSON.stringify(path)} is not an admitted first-party module path`,
    )
  }
}

function validateExportName(name: string): void {
  const length = new TextEncoder().encode(name).length
  if (
    length < 1 ||
    length > 256 ||
    !hasOnlyUnicodeScalarValues(name) ||
    /[\p{Cc}]/u.test(name)
  ) {
    throw new Error(`exportName ${JSON.stringify(name)} is invalid`)
  }
}

function compareLocatedDefinitions(
  left: LocatedDefinition,
  right: LocatedDefinition,
): number {
  const order: Readonly<Record<LocatedDefinition["definition"]["kind"], number>> = {
    task: 0,
    actor: 1,
    sandbox: 2,
  }
  return (
    order[left.definition.kind] - order[right.definition.kind] ||
    compareUTF8(left.definition.id, right.definition.id)
  )
}

function compareLocatorOccurrence(
  left: LocatedDefinition,
  right: LocatedDefinition,
): number {
  return (
    compareUTF8(left.modulePath, right.modulePath) ||
    compareUTF8(left.exportName, right.exportName)
  )
}
