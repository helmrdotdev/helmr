import type { InputContent } from "@helmr/sdk"
import { normalizeInput, encodeComputerSecrets, inspectImage, canonicalizeJsonValue, type InternalImage, type InternalImageStep, type JsonValue, type DefinitionIndex, type RuntimeArchitecture } from "@helmr/sdk/internal"
import type { AgentDefinition, ComputerDefinition, SecretBinding, Json } from "@helmr/sdk"
import { compareUTF8, hasOnlyUnicodeScalarValues } from "./utf8"

export const BUILD_PLAN_FORMAT_VERSION = 0 as const

export interface ComputerManifest {
  readonly imageBuild: ImageBuild
  readonly resources: { readonly milliCpu: number; readonly memoryMiB: number; readonly diskMiB?: number }
  readonly prepare: boolean
  readonly refresh?: { readonly everyMs: number; readonly maxAgeMs?: number }
  readonly secrets: readonly SecretBinding[]
  readonly buildSecrets: readonly SecretBinding[]
}
export interface AgentManifest {
  readonly computerDefinitionId: string
  readonly setup: boolean
  readonly closeAfterIdleMs?: number
  readonly maxTurnDurationMs?: number
  readonly triggers: Readonly<Record<string, { readonly cron: string; readonly timezone: string; readonly input: Json; readonly slack?: Readonly<{ channelId: string }> }>>
}
export type BuildPlanDefinition =
  | Readonly<{ kind: "agent"; declaredId: string; manifest: AgentManifest }>
  | Readonly<{ kind: "computer"; declaredId: string; manifest: ComputerManifest }>
export interface BuildPlan {
  readonly formatVersion: 0
  readonly definitions: readonly BuildPlanDefinition[]
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

export interface AnalysisExport { readonly modulePath: string; readonly exportName: string; readonly value: unknown }
export interface AnalyzeOptions { readonly architecture: RuntimeArchitecture; readonly exports: readonly AnalysisExport[] }
export interface AnalysisResult {
  readonly buildPlan: BuildPlan
  readonly buildPlanBytes: Uint8Array
  readonly definitionIndex: DefinitionIndex
  readonly definitionIndexBytes: Uint8Array
}
type Definition = AgentDefinition<InputContent, Json, unknown> | ComputerDefinition
interface LocatedDefinition { readonly definition: Definition; readonly modulePath: string; readonly exportName: string; readonly throughAgent: boolean }

export function analyze(options: AnalyzeOptions): AnalysisResult {
  const located = locateDefinitions(options)
  const definitions = located.map(({ definition }): BuildPlanDefinition => {
    if (definition.kind === "agent") {
      const triggers: Record<string, { cron: string; timezone: string; input: Json; slack?: { channelId: string } }> = Object.create(null)
      if (definition.triggers !== undefined && !Array.isArray(definition.triggers)) throw new Error("Agent triggers must be an array")
      for (const trigger of definition.triggers ?? []) {
        assertMembers(trigger, ["id", "cron", "timezone", "input", "slack"], "Agent trigger")
        const key = trigger.id
        validateID(key)
        if (Object.hasOwn(triggers, key)) throw new Error("Duplicate Agent trigger id")
        if (!trigger || typeof trigger.cron !== "string" || !trigger.cron.trim() || (typeof trigger.timezone !== "string" || !trigger.timezone.trim())) throw new Error("Agent trigger requires cron and timezone")
        normalizeInput(trigger.input)
        if (trigger.slack !== undefined) {
          assertMembers(trigger.slack, ["channelId"], "Cron Slack destination")
          if (typeof trigger.slack.channelId !== "string" || !/^[CG][A-Z0-9]{1,99}$/.test(trigger.slack.channelId)) throw new Error("Cron Slack channelId must be a Slack channel ID")
        }
        triggers[key] = { cron: trigger.cron, timezone: trigger.timezone, input: trigger.input, ...(trigger.slack === undefined ? {} : { slack: { channelId: trigger.slack.channelId } }) }
      }
      return { kind: "agent", declaredId: definition.id, manifest: {
        computerDefinitionId: definition.computer.id, setup: definition.setup !== undefined, triggers,
        ...(definition.closeAfterIdle === undefined ? {} : { closeAfterIdleMs: duration(definition.closeAfterIdle, "closeAfterIdle") }),
        ...(definition.maxTurnDuration === undefined ? {} : { maxTurnDurationMs: duration(definition.maxTurnDuration, "maxTurnDuration") }),
      } }
    }
    const image = inspectImage(definition.image)
    if (!image) throw new Error("Computer image must be created by image()")
    const refresh = definition.refresh
    if (refresh !== undefined) assertMembers(refresh, ["every", "maxAge"], "Computer refresh")
    return { kind: "computer", declaredId: definition.id, manifest: {
      imageBuild: compileImageBuild(image, options), resources: normalizeComputerResources(definition.resources),
      prepare: definition.prepare !== undefined,
      ...(refresh === undefined ? {} : { refresh: { everyMs: duration(refresh.every, "refresh.every"), ...(refresh.maxAge === undefined ? {} : { maxAgeMs: duration(refresh.maxAge, "refresh.maxAge") }) } }),
      secrets: secretBindings(definition.secrets), buildSecrets: secretBindings(definition.buildSecrets),
    } }
  })
  const buildPlan: BuildPlan = { formatVersion: BUILD_PLAN_FORMAT_VERSION, definitions }
  const agents = located.filter(item => item.definition.kind === "agent").map(item => ({ id: item.definition.id, computerDefinitionId: (item.definition as AgentDefinition).computer.id, modulePath: item.modulePath, exportName: item.exportName }))
  const computers = located.filter(item => item.definition.kind === "computer").map(item => ({ id: item.definition.id, modulePath: item.modulePath, exportName: item.exportName, throughAgent: item.throughAgent }))
  const definitionIndex: DefinitionIndex = { apiVersion: "helmr.definition-index.v1", agents, computers }
  return { buildPlan, buildPlanBytes: canonicalizeJsonValue(buildPlan as unknown as JsonValue), definitionIndex, definitionIndexBytes: canonicalizeJsonValue(definitionIndex as unknown as JsonValue) }
}
function locateDefinitions(options: AnalyzeOptions): LocatedDefinition[] {
  if (options.architecture !== "x86_64") throw new Error("Unsupported architecture")
  const found = new Map<string, LocatedDefinition>()
  const add = (definition: Definition, item: AnalysisExport, throughAgent: boolean) => {
    validateID(definition.id); validateModulePath(item.modulePath); validateExportName(item.exportName)
    const key = `${definition.kind}\0${definition.id}`
    const candidate = { definition, modulePath: item.modulePath, exportName: item.exportName, throughAgent }
    const previous = found.get(key)
    if (previous && previous.definition !== definition) throw new Error(`duplicate ${definition.kind} declaration ${JSON.stringify(definition.id)}`)
    if (!previous || Number(throughAgent) - Number(previous.throughAgent) < 0 || (throughAgent === previous.throughAgent && (compareUTF8(item.modulePath, previous.modulePath) || compareUTF8(item.exportName, previous.exportName)) < 0)) found.set(key, candidate)
  }
  for (const item of options.exports) {
    const value = item.value as Partial<Definition> | undefined
    if (!value || (value.kind !== "agent" && value.kind !== "computer")) continue
    if (value.kind === "agent") {
      assertMembers(value, ["kind", "id", "computer", "setup", "turn", "closeAfterIdle", "maxTurnDuration", "triggers"], "Agent")
      if (typeof value.turn !== "function" || (value.setup !== undefined && typeof value.setup !== "function") || value.computer?.kind !== "computer") throw new Error("Invalid Agent definition")
      add(value as Definition, item, false)
      add(value.computer, item, true)
    } else add(value as Definition, item, false)
  }
  for (const { definition } of found.values()) {
    if (definition.kind === "computer") {
      assertMembers(definition, ["kind", "id", "image", "resources", "prepare", "refresh", "secrets", "buildSecrets"], "Computer")
      if (definition.prepare !== undefined && typeof definition.prepare !== "function") throw new Error("Computer prepare must be a function")
    }
  }
  if (found.size === 0 || found.size > 10_000) throw new Error("BuildPlan definitions must contain 1 to 10000 definitions")
  return [...found.values()].sort((a, b) => compareUTF8(a.definition.kind, b.definition.kind) || compareUTF8(a.definition.id, b.definition.id))
}
function validateID(value: unknown): asserts value is string {
  if (typeof value !== "string" || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value)) throw new Error("Definition id is invalid")
}
function assertMembers(value: unknown, allowed: readonly string[], label: string): void {
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.keys(value).some(key => !allowed.includes(key))) throw new Error(`${label} has invalid or unknown members`)
}
function secretBindings(value: ComputerDefinition["secrets"]): ComputerManifest["secrets"] {
  const result = [...encodeComputerSecrets(value)]
  return result.sort((a, b) => compareUTF8(a.env ? "env" : "file", b.env ? "env" : "file") || compareUTF8(a.env?.name ?? a.file!.path, b.env?.name ?? b.file!.path) || compareUTF8(a.secretId, b.secretId))
}
export function normalizeComputerResources(resources: Readonly<{ cpu: number; memory: string; disk?: string }>): ComputerManifest["resources"] {
  assertMembers(resources, ["cpu", "memory", "disk"], "Computer resources")
  return { milliCpu: normalizeCpu(resources.cpu), memoryMiB: normalizeIecMiB(resources.memory, "memory"), ...(resources.disk === undefined ? {} : { diskMiB: normalizeIecMiB(resources.disk, "disk") }) }
}
function duration(value: string | number, label: string): number {
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${label} must be positive safe integer milliseconds`)
    return value
  }
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(value)
  if (!match) throw new Error(`${label} must be a positive duration`)
  const scale: Record<string, bigint> = { ms: 1n, s: 1000n, m: 60000n, h: 3600000n, d: 86400000n }
  return safePositiveNumber(BigInt(match[1]!) * scale[match[2]!]!, label)
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

function normalizeCpu(cpu: number): number {
  if (!Number.isFinite(cpu) || cpu <= 0) {
    throw new Error("computer cpu must be a finite positive number")
  }
  const text = cpu.toString()
  const match = /^(\d+)(?:\.(\d+))?(?:e([+-]?\d+))?$/i.exec(text)
  if (match === null) throw new Error("computer cpu cannot be normalized")
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
      throw new Error("computer cpu must resolve to whole milliCPU")
    }
    milliCpu = significand / divisor
  }
  return safePositiveNumber(milliCpu, "computer milliCPU")
}

function normalizeIecMiB(value: string, label: string): number {
  const match = /^([1-9]\d*)(MiB|GiB)$/.exec(value)
  if (match === null) {
    throw new Error(
      `computer ${label} must be a positive canonical integer suffixed by MiB or GiB`,
    )
  }
  const result =
    BigInt(match[1] as string) * (match[2] === "GiB" ? 1024n : 1n)
  return safePositiveNumber(result, `computer ${label} MiB`)
}

function safePositiveNumber(value: bigint, label: string): number {
  if (value <= 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`${label} must be a positive safe integer`)
  }
  return Number(value)
}

function validateModulePath(path: string): void {
  if (!hasOnlyUnicodeScalarValues(path) || /[\p{Cc}]/u.test(path) ||
      !/^helmr\/app\/entry-[0-9]+\.mjs$/.test(path)) {
    throw new Error(`modulePath ${JSON.stringify(path)} is not a generated entry`)
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
