import {
  assertPayloadSchema,
  parsePayloadWithSchema,
  type PayloadSchemaInput,
  type PayloadSchemaOutput,
  type PayloadSchema,
} from "./schema/payload"
import {
  validateOptionalMaxDurationSeconds,
  validateOptionalQueueConcurrencyLimit,
  validateQueueName,
  validateTaskId,
} from "./schema/task"
import type { IdempotencyKeyInput } from "./idempotency"
import type { TaskStartResult } from "./runtime/client"

export { parsePayloadWithSchema } from "./schema/payload"

const CHANNEL_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/

export function validateChannelName(value: string, label = "channel name"): string {
  const normalized = value.trim()
  if (!CHANNEL_NAME_PATTERN.test(normalized)) {
    throw new Error(`${label} must match ${CHANNEL_NAME_PATTERN}`)
  }
  return normalized
}

export interface CacheMount {
  readonly id: string
}

export type CacheBuilder = CacheMount

export interface CacheMountBinding {
  readonly mountPath: string
  readonly cache: CacheMount
}

export interface SecretMountBinding {
  readonly mountPath: string
  readonly secret: string
}

export interface SourceDirectoryOptions {
  readonly ignore?: readonly string[]
}

export interface TaskWorkspace {
  readonly path: string
  readonly projectPath: string
}

export interface SourceFileRef {
  readonly path: string
}

export interface SourceDirRef {
  readonly path: string
  readonly ignore: readonly string[]
}

export interface SourceCapabilities {
  file(path: string): SourceFileRef
  directory(path: string, opts?: SourceDirectoryOptions): SourceDirRef
}

export type WaitJson =
  | null
  | boolean
  | number
  | string
  | readonly WaitJson[]
  | { readonly [key: string]: WaitJson }

export interface WaitpointOptions {
  readonly params?: WaitJson
  readonly metadata?: { readonly [key: string]: WaitJson }
  readonly tags?: string | readonly string[]
  readonly timeout?: WaitDurationInput
}

export interface WaitpointSchemaOptions<TSchema extends PayloadSchema<any, any> = PayloadSchema<any, any>> extends WaitpointOptions {
  readonly schema: TSchema
}

export interface WaitpointResult<TPayload = unknown> {
  readonly ok: boolean
  readonly data?: TPayload
  readonly error?: unknown
  unwrap(): TPayload
}

export interface WaitpointHandle<TPayload = unknown> extends PromiseLike<WaitpointResult<TPayload>> {
  unwrap(): Promise<TPayload>
}

export interface WaitDelayHandle extends PromiseLike<void> {
  unwrap(): Promise<void>
}

export type WaitDurationInput =
  | string
  | number
  | {
      readonly milliseconds?: number
      readonly seconds?: number
      readonly minutes?: number
      readonly hours?: number
      readonly duration?: string
    }

export type WaitUntilInput =
  | string
  | Date
  | {
      readonly date?: string | Date
    }

export interface ChannelOutputAppendOptions {
  readonly contentType?: string
  readonly objectRef?: WaitJson
}

export interface WaitpointToken {
  readonly id: string
  readonly status?: "waiting" | "completed" | "timed_out" | "cancelled"
  readonly callbackUrl: string
  readonly publicAccessToken?: string
  readonly timeoutAt: string | null
  readonly tags?: readonly string[]
  readonly metadata?: Record<string, unknown>
}

export interface WaitpointTokenRef {
  readonly id: string
}

export interface ChannelOutputDefinition<TPayload = unknown, TInput = TPayload> {
  readonly id: string
  readonly schema: PayloadSchema<TInput, TPayload>
}

export interface ChannelInputDefinition<TPayload = unknown, TInput = TPayload> {
  readonly id: string
  readonly schema: PayloadSchema<TInput, TPayload>
}

export interface ChannelInputWaitOptions extends Omit<WaitpointOptions, "params"> {
  readonly correlationId?: string
}

export interface ChannelInputHandle<TPayload = unknown> {
  readonly id: string
  wait(opts?: ChannelInputWaitOptions): WaitpointHandle<TPayload>
}

export interface ChannelOutputHandle<TInput = unknown> {
  readonly id: string
  append(payload: TInput, opts?: ChannelOutputAppendOptions): Promise<void>
  pipe(source: AsyncIterable<TInput> | Iterable<TInput>, opts?: ChannelOutputAppendOptions): Promise<void>
}

export interface RunRuntime {
  createWaitpointToken(opts: RuntimeWaitpointTokenCreateOptions): Promise<WaitpointToken>
  waitpoint<TPayload>(opts: RuntimeWaitpointOptions): Promise<WaitpointResult<TPayload>>
  waitAll(operands: readonly RuntimeWaitOperand[]): Promise<readonly unknown[]>
  channelOutputAppend(channel: string, payload: unknown, opts?: ChannelOutputAppendOptions): Promise<void>
  waitFor(input: WaitDurationInput): Promise<void>
  waitUntil(input: WaitUntilInput): Promise<void>
  metadataSet(key: string, value: unknown): Promise<void>
  metadataPatch(value: Record<string, unknown>): Promise<void>
  metadataIncrement(key: string, amount?: number): Promise<void>
  log(level: "info" | "warn" | "error", values: readonly unknown[]): void
}

export interface RuntimeWaitpointTokenCreateOptions {
  readonly timeoutAt?: string
  readonly timeoutInSeconds?: number
  readonly tags?: readonly string[]
  readonly metadata?: Record<string, unknown>
}

export type RuntimeWaitpointOptions = WaitpointOptions & {
  readonly kind?: "token" | "channel"
  readonly schema?: PayloadSchema<any, any>
}

export type RuntimeWaitOperand =
  | { readonly type: "for"; readonly input: WaitDurationInput }
  | { readonly type: "until"; readonly input: WaitUntilInput }
  | { readonly type: "waitpoint"; readonly options: RuntimeWaitpointOptions }
  | {
      readonly type: "channel"
      readonly channel: string
      readonly schema?: PayloadSchema<any, any>
      readonly options?: ChannelInputWaitOptions
    }

export const runtimeWaitOperand = Symbol.for("helmr.sdk.runtimeWaitOperand")

export type RuntimeWaitOperandCarrier = {
  readonly [runtimeWaitOperand]?: RuntimeWaitOperand
}

export function getRuntimeWaitOperand(value: unknown): RuntimeWaitOperand | undefined {
  if (value === null || (typeof value !== "object" && typeof value !== "function")) {
    return undefined
  }
  return (value as RuntimeWaitOperandCarrier)[runtimeWaitOperand]
}

const runRuntimeSlot = Symbol.for("helmr.sdk.runRuntime")

type RuntimeGlobal = typeof globalThis & {
  [runRuntimeSlot]?: RunRuntime
}

export function enterRunRuntime(runtime: RunRuntime): () => void {
  const global = globalThis as RuntimeGlobal
  if (global[runRuntimeSlot] !== undefined) {
    throw new Error("Helmr run runtime is already active")
  }
  global[runRuntimeSlot] = runtime
  return () => {
    if (global[runRuntimeSlot] === runtime) {
      delete global[runRuntimeSlot]
    }
  }
}

export function getRunRuntime(): RunRuntime {
  const runtime = (globalThis as RuntimeGlobal)[runRuntimeSlot]
  if (runtime === undefined) {
    throw new Error("Helmr run APIs can only be used while a task is running")
  }
  return runtime
}

export class WaitpointResultImpl<TPayload> implements WaitpointResult<TPayload> {
  readonly ok: boolean
  readonly data?: TPayload
  readonly error?: unknown

  constructor(ok: boolean, data?: TPayload, error?: unknown) {
    this.ok = ok
    if (data !== undefined) {
      this.data = data
    }
    if (error !== undefined) {
      this.error = error
    }
  }

  unwrap(): TPayload {
    if (this.ok) {
      return this.data as TPayload
    }
    if (this.error instanceof Error) {
      throw this.error
    }
    throw new Error(String(this.error ?? "waitpoint failed"))
  }
}

const concurrentWaitErrorBrand = Symbol.for("helmr.sdk.ConcurrentWaitError")
const waitTimeoutErrorBrand = Symbol.for("helmr.sdk.WaitTimeoutError")

export class ConcurrentWaitError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "ConcurrentWaitError"
    Object.defineProperty(this, concurrentWaitErrorBrand, { value: true })
  }

  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === ConcurrentWaitError &&
      typeof value === "object" &&
      value !== null &&
      concurrentWaitErrorBrand in value
    )
  }
}

export class WaitTimeoutError extends Error {
  readonly timeout: number | undefined

  constructor(message: string, timeout?: number) {
    super(message)
    this.name = "WaitTimeoutError"
    this.timeout = timeout
    Object.defineProperty(this, waitTimeoutErrorBrand, { value: true })
  }

  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === WaitTimeoutError &&
      typeof value === "object" &&
      value !== null &&
      waitTimeoutErrorBrand in value
    )
  }
}

export type Placement =
  | { readonly env: string }
  | {
      readonly file: string
      readonly mode?: string
      readonly owner?: string
    }
  | {
      readonly dir: string
      readonly mode?: string
      readonly owner?: string
    }

export type SecretDecl = (
  | { readonly env: string }
  | {
      readonly file: string
      readonly mode?: string
      readonly owner?: string
    }
  | {
      readonly dir: string
      readonly mode?: string
      readonly owner?: string
    }
) & { readonly name: string }

export type SecretDecls = readonly SecretDecl[]

export function validateSecretName(name: string, label = "secret name"): void {
  if (name.length === 0) {
    throw new Error(`${label} must not be empty`)
  }
  if (name.length > 128) {
    throw new Error(`${label} must be at most 128 characters`)
  }
  if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/.test(name)) {
    throw new Error(`${label} must match /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/`)
  }
}

function validateImageBuildSecretRef(name: string): string {
  validateSecretName(name, "image build secret ref")
  return name
}

const RESERVED_WORKSPACE_MOUNT_PATHS = [
  "/dev",
  "/opt/helmr",
  "/proc",
  "/run",
  "/sys",
  "/tmp",
  "/.helmr-old-root",
] as const

function normalizeWorkspaceMountPath(raw: string): string {
  if (raw.length === 0) {
    throw new Error("sandbox.workspace() mountPath is empty")
  }
  if (raw.includes("\0")) {
    throw new Error("sandbox.workspace() mountPath contains NUL")
  }
  if (!raw.startsWith("/")) {
    throw new Error(`sandbox.workspace() mountPath must be absolute: ${raw}`)
  }
  const parts: string[] = []
  for (const part of raw.split("/")) {
    if (part === "" || part === ".") continue
    if (part === "..") {
      throw new Error(`sandbox.workspace() mountPath contains unsafe path components: ${raw}`)
    }
    parts.push(part)
  }
  if (parts.length === 0) {
    throw new Error("sandbox.workspace() mountPath cannot be /")
  }
  const normalized = `/${parts.join("/")}`
  if (
    RESERVED_WORKSPACE_MOUNT_PATHS.some(
      (reserved) => normalized === reserved || normalized.startsWith(`${reserved}/`),
    )
  ) {
    throw new Error(`sandbox.workspace() mountPath conflicts with reserved runtime mount paths: ${normalized}`)
  }
  return normalized
}

export interface TaskContext {
  readonly signal: AbortSignal
  readonly run: {
    readonly id: string
    readonly attemptId?: string
    readonly attemptNumber?: number
    readonly runLeaseId?: string
    readonly snapshotVersion?: number
  }
  readonly task: { readonly id: string }
  readonly workspace: TaskWorkspace
  readonly session: TaskSessionContext
}

export interface TaskSessionContext {
  readonly id: string
  readonly workspace: TaskWorkspace
  input<TSchema extends PayloadSchema<any, any>>(
    definition: ChannelInputDefinition<PayloadSchemaOutput<TSchema>, PayloadSchemaInput<TSchema>> & { readonly schema: TSchema },
  ): ChannelInputHandle<PayloadSchemaOutput<TSchema>>
  input(channel: string): ChannelInputHandle<unknown>
  output<TSchema extends PayloadSchema<any, any>>(
    definition: ChannelOutputDefinition<PayloadSchemaOutput<TSchema>, PayloadSchemaInput<TSchema>> & { readonly schema: TSchema },
  ): ChannelOutputHandle<PayloadSchemaInput<TSchema>>
  output(channel: string): ChannelOutputHandle<unknown>
}

export type MaybePromise<T> = T | Promise<T>

declare const noPayloadBrand: unique symbol

export interface NoPayload {
  readonly [noPayloadBrand]: "NoPayload"
}

export interface TaskConfigBase<
  TSecrets extends SecretDecls = readonly [],
> {
  readonly id: string
  readonly sandbox: SandboxBuilder
  readonly maxDuration?: number
  readonly queue?: TaskQueueConfig
  readonly ttl?: string
  readonly retry?: RetryPolicy
  readonly secrets?: TSecrets
}

export interface TaskQueueConfig {
  readonly name?: string
  readonly concurrencyLimit?: number | null
}

export interface InternalTaskScheduleConfig {
  readonly cron: string
  readonly timezone?: string
}

export type TaskRunOptions<TSecrets extends SecretDecls> = {
  readonly projectId?: string
  readonly environmentId?: string
  readonly queue?: string
  readonly concurrencyKey?: string
  readonly priority?: number
  readonly ttl?: string
  readonly retry?: RetryPolicy
  readonly metadata?: Record<string, unknown>
  readonly tags?: readonly string[]
  readonly externalId?: string
  readonly expiresAt?: string | Date
  readonly idempotencyKey?: IdempotencyKeyInput
  readonly idempotencyKeyTTL?: string
  readonly signal?: AbortSignal
}

export type RetryPolicy =
  | false
  | {
      readonly maxAttempts: number
      readonly backoff?: {
        readonly minMs?: number
        readonly maxMs?: number
        readonly factor?: number
        readonly jitter?: "none" | "full"
      }
    }

export type TaskDirectStart<
  TPayloadInput,
  TOutput,
  TSecrets extends SecretDecls,
> = [TPayloadInput] extends [NoPayload]
  ? (opts: TaskRunOptions<TSecrets>) => Promise<TaskStartResult<Awaited<TOutput>>>
  : (payload: TPayloadInput, opts: TaskRunOptions<TSecrets>) => Promise<TaskStartResult<Awaited<TOutput>>>

export type TaskConfigWithPayload<
  TPayloadSchema extends PayloadSchema<any, any>,
  TOutput = unknown,
  TSecrets extends SecretDecls = readonly [],
> = TaskConfigBase<TSecrets> & {
  readonly payload: TPayloadSchema
  readonly run: (payload: PayloadSchemaOutput<TPayloadSchema>, ctx: TaskContext) => MaybePromise<TOutput>
}

export type TaskConfigWithoutPayload<
  TOutput = unknown,
  TSecrets extends SecretDecls = readonly [],
> = TaskConfigBase<TSecrets> & {
  readonly payload?: undefined
  readonly run: (ctx: TaskContext) => MaybePromise<TOutput>
}

export type TaskConfig<
  TPayload = NoPayload,
  TOutput = unknown,
  TSecrets extends SecretDecls = readonly [],
  TPayloadSchema extends PayloadSchema<any, any> | undefined = undefined,
> =
  TPayloadSchema extends PayloadSchema<any, any>
    ? TaskConfigWithPayload<TPayloadSchema, TOutput, TSecrets>
    : TaskConfigWithoutPayload<TOutput, TSecrets>

export type Task<
  TPayload = NoPayload,
  TOutput = unknown,
  TSecrets extends SecretDecls = readonly [],
  TPayloadInput = TPayload,
> = TaskConfigBase<TSecrets> & {
  readonly "~types"?: {
    readonly payloadInput: TPayloadInput
    readonly payload: TPayload
    readonly output: TOutput
    readonly secrets: TSecrets
  }
  readonly payload?: PayloadSchema<TPayloadInput, TPayload>
  readonly run: [TPayloadInput] extends [NoPayload]
    ? (ctx: TaskContext) => MaybePromise<TOutput>
    : (payload: TPayload, ctx: TaskContext) => MaybePromise<TOutput>
  readonly start: TaskDirectStart<TPayloadInput, TOutput, TSecrets>
}
export type AnyTask = TaskConfigBase<SecretDecls> & {
  readonly schedule?: InternalTaskScheduleConfig
  readonly "~types"?: {
    readonly payloadInput: any
    readonly payload: any
    readonly output: any
    readonly secrets: SecretDecls
  }
  readonly payload?: PayloadSchema<any, any>
  readonly run: (...args: any[]) => MaybePromise<any>
  readonly start: (...args: any[]) => Promise<TaskStartResult<any>>
}

export type TaskPayload<TTask> =
  TTask extends { readonly "~types"?: { readonly payload: infer TPayload } }
    ? [TPayload] extends [NoPayload] ? never : TPayload
    : never
export type TaskStartPayload<TTask> =
  TTask extends { readonly "~types"?: { readonly payloadInput: infer TPayloadInput } } ? TPayloadInput : never
export type TaskOutput<TTask> =
  TTask extends { readonly "~types"?: { readonly output: infer TOutput } } ? Awaited<TOutput> : never
export type TaskSecrets<TTask> =
  TTask extends { readonly "~types"?: { readonly secrets: infer TSecrets } } ? TSecrets : never

export const taskBrand = Symbol.for("helmr.sdk.Task")
export const taskOriginBrand = Symbol.for("helmr.sdk.TaskOrigin")
export const configBrand = Symbol.for("helmr.sdk.Config")

export type ImageCopyInput = SourceFileRef | SourceDirRef | ImageBuilder

export interface ImageRunOptions {
  readonly cache?: readonly CacheMountBinding[]
  readonly secrets?: readonly SecretMountBinding[]
}

export interface ImageBuilder {
  from(ref: string): ImageBuilder
  run(argv: readonly string[], opts?: ImageRunOptions): ImageBuilder
  copy(dest: string, src: ImageCopyInput): ImageBuilder
  copyFrom(dest: string, src: ImageBuilder, srcPath: string): ImageBuilder
  workdir(path: string): ImageBuilder
  env(key: string, value: string): ImageBuilder
  user(name: string): ImageBuilder
}

export interface SandboxBuilder {
  image(img: ImageBuilder): SandboxBuilder
  workspace(mountPath?: string): SandboxBuilder
  resources(opts: { readonly cpu?: number; readonly memory?: string; readonly disk?: string }): SandboxBuilder
  network(opts: SandboxNetwork): SandboxBuilder
}

export type ImageBuildStep =
  | { readonly kind: "from"; readonly ref: string }
  | {
      readonly kind: "run"
      readonly argv: readonly string[]
      readonly cache: readonly CacheMountBinding[]
      readonly secrets: readonly SecretMountBinding[]
    }
  | { readonly kind: "copy"; readonly dest: string; readonly source: ImageCopyInput }
  | { readonly kind: "copyFrom"; readonly dest: string; readonly source: ImageBuilder; readonly srcPath: string }
  | { readonly kind: "workdir"; readonly path: string }
  | { readonly kind: "env"; readonly key: string; readonly value: string }
  | { readonly kind: "user"; readonly name: string }

export interface SandboxWorkspace {
  readonly mountPath: string
}

export interface SandboxResources {
  readonly cpu?: number
  readonly memory?: string
  readonly disk?: string
}

export type SandboxNetwork = {
  readonly internet?: boolean | {
    readonly allow?: readonly string[]
    readonly deny?: readonly string[]
  }
}

export interface SandboxNetworkSpec {
  readonly internet: boolean
  readonly allow: readonly string[]
  readonly deny: readonly string[]
}

const imageBuilderBrand = Symbol.for("helmr.sdk.ImageBuilder")
const sandboxBuilderBrand = Symbol.for("helmr.sdk.SandboxBuilder")
const sourceFileRefBrand = Symbol.for("helmr.sdk.SourceFileRef")
const sourceDirRefBrand = Symbol.for("helmr.sdk.SourceDirRef")

export type MarkedTask<
  TPayload,
  TOutput,
  TSecrets extends SecretDecls,
  TPayloadSchema extends PayloadSchema<any, any> | undefined,
> = TPayloadSchema extends PayloadSchema<any, any>
  ? Task<PayloadSchemaOutput<TPayloadSchema>, Awaited<TOutput>, TSecrets, PayloadSchemaInput<TPayloadSchema>>
  : Task<NoPayload, Awaited<TOutput>, TSecrets, NoPayload>

export function markTask<
  TPayload,
  TOutput,
  TSecrets extends SecretDecls,
  TPayloadSchema extends PayloadSchema<any, any> | undefined,
>(
  config: TaskConfig<TPayload, TOutput, TSecrets, TPayloadSchema>,
): MarkedTask<TPayload, TOutput, TSecrets, TPayloadSchema> {
  if ("schedule" in (config as unknown as Record<string, unknown>)) {
    throw new Error(`task ${JSON.stringify(config.id)} must use schedules.task(...) for declarative schedules`)
  }
  return markTaskInternal(config, undefined)
}

export function markScheduledTask<
  TPayload,
  TOutput,
  TSecrets extends SecretDecls,
  TPayloadSchema extends PayloadSchema<any, any> | undefined,
>(
  config: TaskConfig<TPayload, TOutput, TSecrets, TPayloadSchema>,
  schedule: InternalTaskScheduleConfig | undefined,
): MarkedTask<TPayload, TOutput, TSecrets, TPayloadSchema> {
  return markTaskInternal(config, schedule)
}

function markTaskInternal<
  TPayload,
  TOutput,
  TSecrets extends SecretDecls,
  TPayloadSchema extends PayloadSchema<any, any> | undefined,
>(
  config: TaskConfig<TPayload, TOutput, TSecrets, TPayloadSchema>,
  schedule: InternalTaskScheduleConfig | undefined,
): MarkedTask<TPayload, TOutput, TSecrets, TPayloadSchema> {
  validateTaskId(config.id)
  validateOptionalMaxDurationSeconds(config.maxDuration)
  validateTaskQueue(config.id, config.queue)
  validateTaskSchedule(config.id, schedule)
  validateOptionalTTL(config.ttl, `task ${JSON.stringify(config.id)} ttl`)
  validateRetryPolicy(config.retry, `task ${JSON.stringify(config.id)} retry`)
  assertPayloadSchema(config.payload, `task ${JSON.stringify(config.id)} payload`)
  if (schedule !== undefined) {
    Object.defineProperty(config, "schedule", {
      value: Object.freeze({ ...schedule }),
      enumerable: true,
    })
  }
  Object.defineProperty(config, taskBrand, { value: true })
  Object.defineProperty(config, taskOriginBrand, { value: captureTaskOrigin() })
  return config as unknown as MarkedTask<TPayload, TOutput, TSecrets, TPayloadSchema>
}

export function validateRetryPolicy(value: unknown, label = "retry"): void {
  if (value === undefined || value === false) {
    return
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be false or a retry policy object`)
  }
  const record = value as Record<string, unknown>
  for (const key of Object.keys(record)) {
    if (key !== "maxAttempts" && key !== "backoff") {
      throw new Error(`${label}.${key} is not supported`)
    }
  }
  if (
    typeof record["maxAttempts"] !== "number" ||
    !Number.isInteger(record["maxAttempts"]) ||
    record["maxAttempts"] < 1 ||
    record["maxAttempts"] > 10
  ) {
    throw new Error(`${label}.maxAttempts must be an integer between 1 and 10`)
  }
  const backoff = record["backoff"]
  if (backoff !== undefined) {
    if (backoff === null || typeof backoff !== "object" || Array.isArray(backoff)) {
      throw new Error(`${label}.backoff must be an object`)
    }
    const backoffRecord = backoff as Record<string, unknown>
    for (const key of Object.keys(backoffRecord)) {
      if (key !== "minMs" && key !== "maxMs" && key !== "factor" && key !== "jitter") {
        throw new Error(`${label}.backoff.${key} is not supported`)
      }
    }
    validateOptionalPositiveInteger(backoffRecord["minMs"], `${label}.backoff.minMs`)
    validateOptionalPositiveInteger(backoffRecord["maxMs"], `${label}.backoff.maxMs`)
    const factor = backoffRecord["factor"]
    if (factor !== undefined && (typeof factor !== "number" || !Number.isFinite(factor) || factor <= 0)) {
      throw new Error(`${label}.backoff.factor must be a positive number`)
    }
    const jitter = backoffRecord["jitter"]
    if (jitter !== undefined && jitter !== "none" && jitter !== "full") {
      throw new Error(`${label}.backoff.jitter must be "none" or "full"`)
    }
  }
}

function validateOptionalPositiveInteger(value: unknown, label: string): void {
  if (value === undefined) {
    return
  }
  if (typeof value === "number" && Number.isInteger(value) && Number.isFinite(value) && value > 0) {
    return
  }
  throw new Error(`${label} must be a positive integer`)
}

export function validateTaskSchedule(taskId: string, value: InternalTaskScheduleConfig | undefined): void {
  if (value === undefined) {
    return
  }
  if (value.cron.trim() === "") {
    throw new Error(`task ${JSON.stringify(taskId)} schedule cron is required`)
  }
  if (value.timezone !== undefined && value.timezone.trim() === "") {
    throw new Error(`task ${JSON.stringify(taskId)} schedule timezone must not be empty`)
  }
}

export function validateTaskQueue(taskId: string, value: TaskQueueConfig | undefined): void {
  if (value === undefined) {
    return
  }
  if (value.name !== undefined) {
    validateQueueName(value.name)
  }
  validateOptionalQueueConcurrencyLimit(value.concurrencyLimit)
  if (value.name === undefined && value.concurrencyLimit === undefined) {
    throw new Error(`task ${JSON.stringify(taskId)} queue must include name or concurrencyLimit`)
  }
}

export function defaultTaskQueueName(taskId: string): string {
  return `task/${taskId}`
}

export function validateOptionalTTL(value: unknown, label = "ttl"): void {
  if (value === undefined) {
    return
  }
  if (typeof value === "string" && isPositiveDurationString(value)) {
    return
  }
  throw new Error(`${label} must be a positive duration string`)
}

function isPositiveDurationString(value: string): boolean {
  const raw = value.trim()
  if (raw === "") {
    return false
  }
  if (/^[1-9][0-9]*d$/.test(raw)) {
    return true
  }
  const sign = raw.startsWith("+") || raw.startsWith("-") ? raw.slice(0, 1) : ""
  if (sign === "-") {
    return false
  }
  const body = sign === "" ? raw : raw.slice(1)
  const tokenPattern = /([0-9]+(?:\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h)/gy
  let totalNanoseconds = 0
  let offset = 0
  for (;;) {
    tokenPattern.lastIndex = offset
    const match = tokenPattern.exec(body)
    if (match === null) {
      return offset === body.length && totalNanoseconds >= 1
    }
    if (match.index !== offset) {
      return false
    }
    const amount = Number(match[1])
    if (!Number.isFinite(amount) || amount < 0) {
      return false
    }
    totalNanoseconds += amount * durationUnitNanoseconds(match[2] ?? "")
    offset = tokenPattern.lastIndex
  }
}

function durationUnitNanoseconds(unit: string): number {
  switch (unit) {
    case "ns":
      return 1
    case "us":
    case "µs":
    case "μs":
      return 1_000
    case "ms":
      return 1_000_000
    case "s":
      return 1_000_000_000
    case "m":
      return 60_000_000_000
    case "h":
      return 3_600_000_000_000
    default:
      return 0
  }
}

export async function parseTaskPayload<TTask extends AnyTask>(
  task: TTask,
  payload: unknown,
): Promise<TaskPayload<TTask>> {
  if (task.payload === undefined) {
    throw new Error(`task ${JSON.stringify(task.id)} does not accept payload`)
  }
  return await parsePayloadWithSchema(task.payload, payload, `task ${JSON.stringify(task.id)} payload`)
}

export function isTaskDefinition(
  value: unknown,
): value is AnyTask & { readonly [taskBrand]: true } {
  return hasBrand(value, taskBrand)
}

export function taskOriginFile(value: unknown): string | null {
  if (!isTaskDefinition(value)) {
    return null
  }
  const origin = (value as unknown as Record<PropertyKey, unknown>)[taskOriginBrand]
  return typeof origin === "string" && origin.length > 0 ? origin : null
}

export interface HelmrConfig {
  readonly project: string
  readonly dirs: readonly string[]
  readonly ignorePatterns?: readonly string[]
}

export function markConfig(config: HelmrConfig): HelmrConfig {
  Object.defineProperty(config, configBrand, { value: true })
  return config
}

export function isConfigDefinition(value: unknown): value is HelmrConfig & { readonly [configBrand]: true } {
  return hasBrand(value, configBrand)
}

function captureTaskOrigin(): string {
  const stack = new Error().stack ?? ""
  for (const line of stack.split("\n").slice(1)) {
    const file = stackFrameFile(line)
    if (file === null || isSdkInternalFrame(file)) {
      continue
    }
    return file
  }
  return "unknown"
}

function stackFrameFile(line: string): string | null {
  const match = /\(?((?:file:\/\/)?\/[^():]+):\d+:\d+\)?$/.exec(line.trim())
  if (!match?.[1]) {
    return null
  }
  return match[1].startsWith("file://") ? decodeURIComponent(new URL(match[1]).pathname) : match[1]
}

function isSdkInternalFrame(file: string): boolean {
  return (
    file.includes("/sdk/typescript/src/internal") ||
    file.includes("/sdk/typescript/src/task") ||
    file.includes("/sdk/typescript/src/index") ||
    file.includes("/runtime/typescript/src/")
  )
}

export class ImageBuilderImpl implements ImageBuilder {
  readonly id: string
  readonly steps: readonly ImageBuildStep[]

  constructor(id: string, steps: readonly ImageBuildStep[] = []) {
    Object.defineProperty(this, imageBuilderBrand, { value: true })
    this.id = id
    this.steps = steps
  }

  from(ref: string): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "from", ref }])
  }

  run(argv: readonly string[], opts: ImageRunOptions = {}): ImageBuilder {
    return new ImageBuilderImpl(this.id, [
      ...this.steps,
      {
        kind: "run",
        argv: [...argv],
        cache: (opts.cache ?? []).map((binding) => ({
          mountPath: binding.mountPath,
          cache: { id: binding.cache.id },
        })),
        secrets: (opts.secrets ?? []).map((binding) => ({
          mountPath: binding.mountPath,
          secret: validateImageBuildSecretRef(binding.secret),
        })),
      },
    ])
  }

  copy(dest: string, src: ImageCopyInput): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "copy", dest, source: src }])
  }

  copyFrom(dest: string, src: ImageBuilder, srcPath: string): ImageBuilder {
    if (!isImageBuilder(src)) {
      throw new Error("image.copyFrom() requires an ImageBuilder created by image()")
    }
    return new ImageBuilderImpl(this.id, [
      ...this.steps,
      { kind: "copyFrom", dest, source: src, srcPath },
    ])
  }

  workdir(path: string): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "workdir", path }])
  }

  env(key: string, value: string): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "env", key, value }])
  }

  user(name: string): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "user", name }])
  }

}

export class SandboxBuilderImpl implements SandboxBuilder {
  readonly id: string
  readonly imageBuilder: ImageBuilderImpl | undefined
  readonly workspaceBinding: SandboxWorkspace | undefined
  readonly resourceSpec: SandboxResources | undefined
  readonly networkSpec: SandboxNetworkSpec | undefined

  constructor(
    id: string,
    imageBuilder?: ImageBuilderImpl,
    workspaceBinding?: SandboxWorkspace,
    resourceSpec?: SandboxResources,
    networkSpec?: SandboxNetworkSpec,
  ) {
    Object.defineProperty(this, sandboxBuilderBrand, { value: true })
    this.id = id
    this.imageBuilder = imageBuilder
    this.workspaceBinding = workspaceBinding
    this.resourceSpec = resourceSpec
    this.networkSpec = networkSpec
  }

  image(img: ImageBuilder): SandboxBuilder {
    if (!isImageBuilder(img)) {
      throw new Error("sandbox.image() requires an ImageBuilder created by image()")
    }
    return new SandboxBuilderImpl(this.id, img, this.workspaceBinding, this.resourceSpec, this.networkSpec)
  }

  workspace(mountPath = "/workspace"): SandboxBuilder {
    return new SandboxBuilderImpl(
      this.id,
      this.imageBuilder,
      { mountPath: normalizeWorkspaceMountPath(mountPath) },
      this.resourceSpec,
      this.networkSpec,
    )
  }

  resources(opts: { readonly cpu?: number; readonly memory?: string; readonly disk?: string }): SandboxBuilder {
    const resourceSpec: SandboxResources = {
      ...(opts.cpu === undefined ? {} : { cpu: opts.cpu }),
      ...(opts.memory === undefined ? {} : { memory: opts.memory }),
      ...(opts.disk === undefined ? {} : { disk: opts.disk }),
    }
    return new SandboxBuilderImpl(this.id, this.imageBuilder, this.workspaceBinding, resourceSpec, this.networkSpec)
  }

  network(opts: SandboxNetwork): SandboxBuilder {
    return new SandboxBuilderImpl(this.id, this.imageBuilder, this.workspaceBinding, this.resourceSpec, normalizeSandboxNetwork(opts))
  }
}

function normalizeSandboxNetwork(opts: SandboxNetwork): SandboxNetworkSpec {
  const value = opts.internet
  if (value === undefined || value === true) {
    return { internet: true, allow: [], deny: [] }
  }
  if (value === false) {
    return { internet: false, allow: [], deny: [] }
  }
  return {
    internet: true,
    allow: normalizeNetworkEntries(value.allow, "network.internet.allow"),
    deny: normalizeNetworkEntries(value.deny, "network.internet.deny"),
  }
}

function normalizeNetworkEntries(entries: readonly string[] | undefined, label: string): readonly string[] {
  if (entries === undefined) {
    return []
  }
  return entries.map((entry, index) => {
    const normalized = entry.trim()
    if (normalized === "") {
      throw new Error(`${label}[${index}] must not be empty`)
    }
    return normalized
  })
}

export class SourceFileRefImpl implements SourceFileRef {
  readonly path: string

  constructor(path: string) {
    Object.defineProperty(this, sourceFileRefBrand, { value: true })
    this.path = path
  }
}

export class SourceDirRefImpl implements SourceDirRef {
  readonly path: string
  readonly ignore: readonly string[]

  constructor(path: string, ignore: readonly string[]) {
    Object.defineProperty(this, sourceDirRefBrand, { value: true })
    this.path = path
    this.ignore = ignore
  }
}

export function isImageBuilder(value: unknown): value is ImageBuilderImpl {
  return hasBrand(value, imageBuilderBrand)
}

export function isSandboxBuilder(value: unknown): value is SandboxBuilderImpl {
  return hasBrand(value, sandboxBuilderBrand)
}

export function isSourceFileRef(value: unknown): value is SourceFileRefImpl {
  return hasBrand(value, sourceFileRefBrand)
}

export function isSourceDirRef(value: unknown): value is SourceDirRefImpl {
  return hasBrand(value, sourceDirRefBrand)
}

function hasBrand(value: unknown, brand: symbol): boolean {
  return value !== null && typeof value === "object" && (value as Record<symbol, unknown>)[brand] === true
}
