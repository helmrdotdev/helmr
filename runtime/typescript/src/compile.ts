import {
  inspectDefinition,
  inspectImage,
  isQueueDefinition,
  inspectWorkspaceDefinition,
  validateQueueName,
  type InternalImage,
  type InternalImageStep,
  type InternalActorDefinition,
  type InternalDefinition,
  type InternalTaskDefinition,
  type InternalWorkspaceDefinition,
  type ProgramDeclaration,
  type RuntimeArchitecture,
  type WorkspaceNetwork,
} from "@helmr/sdk/internal"
import { canonicalizeJsonValue, type JsonValue } from "@helmr/sdk/internal"

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
          workspace:
            | Readonly<{ id: string }>
            | Readonly<{ key: string }>
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
      kind: "workspace"
      declaredId: string
      manifest: Readonly<{
        imageBuild: ImageBuild
        resources: Readonly<{
          milliCpu: number
          memoryMiB: number
          ephemeralDiskMiB: number
        }>
        network: Readonly<{
          internet: boolean
          denyCidrs: readonly string[]
        }>
        architecture: RuntimeArchitecture
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
  readonly formatVersion: 0
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
  | Readonly<{ from: Readonly<{ ref: string }> }>
  | Readonly<{
      run: Readonly<{
        argv: readonly string[]
        cacheMounts: readonly Readonly<{
          dst: string
          cacheId: string
          sharing: "locked"
        }>[]
        secretMounts: readonly Readonly<{
          dst: string
          name: string
        }>[]
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

interface LocatedDefinition {
  readonly definition: InternalDefinition | InternalWorkspaceDefinition
  readonly modulePath: string
  readonly exportName: string
}

export function analyze(options: AnalyzeOptions): AnalysisResult {
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
  const queues = compileQueues(located, options.exports)
  const definitions = located.map(({ definition }) =>
    compileDefinition(definition, options, queues),
  )
  const locatorEntries = located.flatMap((item) =>
    item.definition.kind === "workspace" ? [] : [locatorEntry(item)],
  )
  const programDeclarations = located.flatMap(({ definition }) =>
    definition.kind === "workspace"
      ? []
      : [programDeclaration(definition)],
  )
  const buildPlan: BuildPlan = Object.freeze({
    formatVersion: BUILD_PLAN_FORMAT_VERSION,
    definitions: Object.freeze(definitions),
    queues: Object.freeze(
      [...queues.values()]
        .map((entry) => Object.freeze({ ...entry }))
        .sort((left, right) => compareUtf8(left.name, right.name)),
    ),
  })
  const declarationLocator: DeclarationLocator = Object.freeze({
    declarations: Object.freeze(locatorEntries),
    formatVersion: DECLARATION_LOCATOR_FORMAT_VERSION,
  })
  return {
    buildPlan,
    buildPlanBytes: canonicalizeJsonValue(buildPlan as unknown as JsonValue),
    declarationLocator,
    declarationLocatorBytes: canonicalizeJsonValue(
      declarationLocator as unknown as JsonValue,
    ),
    programDeclarations: Object.freeze(programDeclarations),
    entrypointBytes: new TextEncoder().encode(PROGRAM_ENTRYPOINT),
  }
}

export function normalizeWorkspaceResources(
  resources: Readonly<{ cpu: number; memory: string }>,
): Readonly<{
  milliCpu: number
  memoryMiB: number
  ephemeralDiskMiB: number
}> {
  return Object.freeze({
    milliCpu: normalizeCpu(resources.cpu),
    memoryMiB: normalizeIecMiB(resources.memory, "memory"),
    ephemeralDiskMiB: 32768,
  })
}

export function normalizeWorkspaceNetwork(
  network: WorkspaceNetwork | undefined,
): Readonly<{ internet: boolean; denyCidrs: readonly string[] }> {
  if (network === undefined) {
    return Object.freeze({ internet: true, denyCidrs: Object.freeze([]) })
  }
  if (network.internet === false) {
    return Object.freeze({ internet: false, denyCidrs: Object.freeze([]) })
  }
  const denyCidrs = [...new Set((network.denyCidrs ?? []).map(canonicalCidr))]
  denyCidrs.sort(compareUtf8)
  return Object.freeze({
    internet: true,
    denyCidrs: Object.freeze(denyCidrs),
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
      inspectDefinition(item.value) ?? inspectWorkspaceDefinition(item.value)
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
    }
    identities.set(key, {
      value: item.value as object,
      located,
    })
  }
  return [...identities.values()].map((item) => item.located)
}

function compileDefinition(
  definition: InternalDefinition | InternalWorkspaceDefinition,
  options: AnalyzeOptions,
  queues: ReadonlyMap<string, BuildPlanQueue>,
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
                schedule: {
                  cron: definition.schedule.cron,
                  timezone: definition.schedule.timezone,
                  workspace:
                    "id" in definition.schedule.workspace
                      ? { id: definition.schedule.workspace.id }
                      : { key: definition.schedule.workspace.key },
                },
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
    case "workspace":
      return {
        kind: "workspace",
        declaredId: definition.id,
        manifest: {
          imageBuild: compileImageBuild(definition.image, options),
          resources: normalizeWorkspaceResources(definition.resources),
          network: normalizeWorkspaceNetwork(definition.network),
          architecture: options.architecture,
        },
      }
  }
}

function compileQueues(
  located: readonly LocatedDefinition[],
  exports: readonly AnalysisExport[],
): Map<string, BuildPlanQueue> {
  const queues = new Map<string, QueueEntry>()
  for (const item of exports) {
    if (isQueueDefinition(item.value)) {
      addQueue(queues, item.value, item.value)
    }
  }
  for (const { definition } of located) {
    if (definition.kind !== "task" && definition.kind !== "actor") continue
    if (typeof definition.queue === "object") {
      addQueue(queues, definition.queue, definition.queue)
    } else if (definition.queue === undefined) {
      addQueue(queues, {
        id: `${definition.kind}/${definition.id}`,
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
  queue: { readonly id: string; readonly concurrencyLimit?: number | null },
  owner: object,
): void {
  validateQueueName(queue.id)
  const next: BuildPlanQueue = {
    name: queue.id,
    ...(queue.concurrencyLimit === undefined ||
    queue.concurrencyLimit === null
      ? {}
      : { concurrencyLimit: queue.concurrencyLimit }),
  }
  const existing = queues.get(queue.id)
  if (existing !== undefined) {
    if (existing.owner === owner) return
    throw new Error(`duplicate queue declaration ${JSON.stringify(queue.id)}`)
  }
  queues.set(queue.id, { owner, queue: next })
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
        : definition.queue.id
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
    if (visiting.has(image.id)) {
      throw new Error(`image graph contains a cycle at ${JSON.stringify(image.id)}`)
    }
    const existing = images.get(image.id)
    if (existing !== undefined) {
      if (existing !== image) {
        throw new Error(`image key ${JSON.stringify(image.id)} is not unique`)
      }
      return
    }
    visiting.add(image.id)
    images.set(image.id, image)
    for (const step of image.steps) {
      if (step.kind === "copy_from_image") {
        const source = inspectImage(step.source)
        if (source === undefined) throw new Error("invalid copyFrom image")
        visit(source)
      }
    }
    visiting.delete(image.id)
  }
  visit(root)
  const specs = [...images.values()]
    .sort((left, right) => compareUtf8(left.id, right.id))
    .map((image) => ({
      key: image.id,
      platform: {
        os: "linux" as const,
        architecture: options.architecture,
      },
      steps: image.steps.map((step) => compileImageStep(step, options)),
    }))
  const stepCount = specs.reduce((total, image) => total + image.steps.length, 0)
  if (stepCount > 10_000) throw new Error("image build exceeds 10000 steps")
  return {
    formatVersion: 0,
    root: root.id,
    images: specs,
  }
}

function compileImageStep(
  step: InternalImageStep,
  options: AnalyzeOptions,
): ImageStep {
  switch (step.kind) {
    case "from":
      return { from: { ref: step.ref } }
    case "run":
      return {
        run: {
          argv: [...step.argv],
          cacheMounts: step.cache.map((binding) => ({
            dst: binding.mountPath,
            cacheId: binding.cache.id,
            sharing: "locked" as const,
          })),
          secretMounts: step.secrets.map((binding) => ({
            dst: binding.mountPath,
            name: binding.secret,
          })),
        },
      }
    case "copy_source_file":
      return {
        copySourceFile: {
          dst: step.destination,
          path: step.source.path,
        },
      }
    case "copy_source_directory":
      return {
        copySourceDir: {
          dst: step.destination,
          path: step.source.path,
        },
      }
    case "copy_from_image": {
      const source = inspectImage(step.source)
      if (source === undefined) throw new Error("invalid copyFrom image")
      return {
        copyFromImage: {
          dst: step.destination,
          imageKey: source.id,
          srcPath: step.sourcePath,
        },
      }
    }
    case "workdir":
      return { workdir: { path: step.path } }
    case "env":
      return { env: { key: step.key, value: step.value } }
    case "user":
      return { user: { name: step.name } }
  }
}

function locatorEntry(item: LocatedDefinition): DeclarationLocatorEntry {
  if (item.definition.kind === "workspace") {
    throw new Error("workspace has no executable locator")
  }
  return {
    declaredId: item.definition.id,
    exportName: item.exportName,
    kind: item.definition.kind,
    modulePath: item.modulePath,
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

function canonicalCidr(value: string): string {
  const parts = value.split("/")
  if (parts.length !== 2) throw new Error(`invalid CIDR ${JSON.stringify(value)}`)
  const address = parts[0] as string
  const prefixText = parts[1] as string
  if (!/^(0|[1-9]\d*)$/.test(prefixText)) {
    throw new Error(`invalid CIDR prefix ${JSON.stringify(value)}`)
  }
  const ipv4 = parseIpv4(address)
  if (ipv4 !== undefined) {
    const prefix = Number(prefixText)
    if (prefix > 32) throw new Error(`invalid IPv4 CIDR prefix ${prefix}`)
    const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0
    const network = (ipv4 & mask) >>> 0
    return `${[(network >>> 24) & 255, (network >>> 16) & 255, (network >>> 8) & 255, network & 255].join(".")}/${prefix}`
  }
  const words = parseIpv6(address)
  const prefix = Number(prefixText)
  if (prefix > 128) throw new Error(`invalid IPv6 CIDR prefix ${prefix}`)
  for (let bit = prefix; bit < 128; bit += 1) {
    const word = Math.floor(bit / 16)
    const shift = 15 - (bit % 16)
    words[word] = (words[word] as number) & ~(1 << shift)
  }
  return `${formatIpv6(words)}/${prefix}`
}

function parseIpv4(value: string): number | undefined {
  const parts = value.split(".")
  if (
    parts.length !== 4 ||
    parts.some((part) => !/^(0|[1-9]\d{0,2})$/.test(part))
  ) {
    return undefined
  }
  const values = parts.map(Number)
  if (values.some((part) => part > 255)) return undefined
  return (
    (((values[0] as number) << 24) |
      ((values[1] as number) << 16) |
      ((values[2] as number) << 8) |
      (values[3] as number)) >>>
    0
  )
}

function parseIpv6(value: string): number[] {
  if (value.includes(".")) throw new Error(`invalid IPv6 address ${JSON.stringify(value)}`)
  const halves = value.split("::")
  if (halves.length > 2) throw new Error(`invalid IPv6 address ${JSON.stringify(value)}`)
  const left = halves[0] === "" ? [] : (halves[0] as string).split(":")
  const right =
    halves.length === 1 || halves[1] === ""
      ? []
      : (halves[1] as string).split(":")
  if (
    [...left, ...right].some((part) => !/^[0-9A-Fa-f]{1,4}$/.test(part)) ||
    (halves.length === 1 && left.length !== 8) ||
    (halves.length === 2 && left.length + right.length >= 8)
  ) {
    throw new Error(`invalid IPv6 address ${JSON.stringify(value)}`)
  }
  const zeros = halves.length === 2 ? 8 - left.length - right.length : 0
  return [
    ...left.map((part) => Number.parseInt(part, 16)),
    ...Array.from({ length: zeros }, () => 0),
    ...right.map((part) => Number.parseInt(part, 16)),
  ]
}

function formatIpv6(words: readonly number[]): string {
  let bestStart = -1
  let bestLength = 0
  for (let index = 0; index < words.length; ) {
    if (words[index] !== 0) {
      index += 1
      continue
    }
    let end = index
    while (end < words.length && words[end] === 0) end += 1
    if (end - index > bestLength && end - index >= 2) {
      bestStart = index
      bestLength = end - index
    }
    index = end
  }
  if (bestStart === -1) return words.map((word) => word.toString(16)).join(":")
  const left = words.slice(0, bestStart).map((word) => word.toString(16)).join(":")
  const right = words
    .slice(bestStart + bestLength)
    .map((word) => word.toString(16))
    .join(":")
  return `${left}::${right}`
}

function validateModulePath(path: string): void {
  const suffixes = [".js", ".mjs", ".cjs", ".ts", ".mts", ".cts"]
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

function hasOnlyUnicodeScalarValues(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (next < 0xdc00 || next > 0xdfff) return false
      index += 1
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return false
    }
  }
  return true
}

function compareLocatedDefinitions(
  left: LocatedDefinition,
  right: LocatedDefinition,
): number {
  const order: Readonly<Record<LocatedDefinition["definition"]["kind"], number>> = {
    task: 0,
    actor: 1,
    workspace: 2,
  }
  return (
    order[left.definition.kind] - order[right.definition.kind] ||
    compareUtf8(left.definition.id, right.definition.id)
  )
}

function compareLocatorOccurrence(
  left: LocatedDefinition,
  right: LocatedDefinition,
): number {
  return (
    compareUtf8(left.modulePath, right.modulePath) ||
    compareUtf8(left.exportName, right.exportName)
  )
}

function compareUtf8(left: string, right: string): number {
  const encoder = new TextEncoder()
  const a = encoder.encode(left)
  const b = encoder.encode(right)
  for (let index = 0; index < Math.min(a.length, b.length); index += 1) {
    const difference = (a[index] as number) - (b[index] as number)
    if (difference !== 0) return difference
  }
  return a.length - b.length
}
