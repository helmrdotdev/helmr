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
import type { SessionStartResult } from "./runtime/client"

export { parsePayloadWithSchema } from "./schema/payload"

const STREAM_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/

export function validateStreamName(value: string, label = "stream name"): string {
  const normalized = value.trim()
  if (!STREAM_NAME_PATTERN.test(normalized)) {
    throw new Error(`${label} must match ${STREAM_NAME_PATTERN}`)
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

export interface WaitOptions {
  readonly metadata?: { readonly [key: string]: WaitJson }
  readonly tags?: string | readonly string[]
  readonly timeout?: DurationInput
  readonly idleTimeout?: DurationInput
}

export interface WaitSchemaOptions<TSchema extends PayloadSchema<any, any> = PayloadSchema<any, any>> extends WaitOptions {
  readonly schema: TSchema
}

export interface WaitResult<TPayload = unknown> {
  readonly ok: boolean
  readonly data?: TPayload
  readonly error?: unknown
  unwrap(): TPayload
}

export interface WaitHandle<TPayload = unknown> extends PromiseLike<WaitResult<TPayload>> {
  unwrap(): Promise<TPayload>
}

export interface WaitDelayHandle extends PromiseLike<void> {
  unwrap(): Promise<void>
}

export type DurationInput =
  | string
  | number
  | {
      readonly milliseconds?: number
      readonly seconds?: number
      readonly minutes?: number
      readonly hours?: number
      readonly duration?: string
    }

export type UntilInput =
  | string
  | Date
  | {
      readonly date?: string | Date
    }

export interface StreamAppendOptions {
  readonly contentType?: string
}

export interface Token {
  readonly id: string
  readonly status?: "pending" | "completed" | "expired" | "cancelled"
  readonly callbackUrl: string
  readonly publicAccessToken?: string
  readonly timeoutAt: string | null
  readonly tags?: readonly string[]
  readonly metadata?: Record<string, unknown>
  wait<TSchema extends PayloadSchema<any, any>>(
    opts: TokenWaitOptions<TSchema> & { readonly schema: TSchema },
  ): WaitHandle<PayloadSchemaOutput<TSchema>>
  wait(opts?: TokenWaitOptions): WaitHandle<unknown>
}

export interface TokenRef {
  readonly id: string
}

export interface OutputStreamDefinition<TPayload = unknown, TInput = TPayload> {
  readonly id: string
  readonly direction: "output"
  readonly schema: PayloadSchema<TInput, TPayload>
}

export interface InputStreamDefinition<TPayload = unknown, TInput = TPayload> {
  readonly id: string
  readonly direction: "input"
  readonly schema: PayloadSchema<TInput, TPayload>
}

export type StreamDefinition =
  | InputStreamDefinition<any, any>
  | OutputStreamDefinition<any, any>

export interface InternalStreamDefinition {
  readonly id: string
  readonly direction: "input" | "output"
  readonly schema: PayloadSchema<any, any>
}

export interface InputStreamWaitOptions extends WaitOptions {
  readonly correlationId?: string
  readonly afterSequence?: number
}

export interface InputStreamSendOptions {
  readonly correlationId?: string
  readonly idempotencyKey?: string
}

export interface InputStreamHandle<TPayload = unknown, TInput = TPayload> {
  readonly id: string
  wait(opts?: InputStreamWaitOptions): WaitHandle<TPayload>
  once(opts?: InputStreamWaitOptions): WaitHandle<TPayload>
  on(handler: (payload: TPayload) => void | Promise<void>, opts?: InputStreamWaitOptions): Promise<void>
  peek(opts?: InputStreamPeekOptions): Promise<StreamRecord<TPayload> | null>
}

export interface InputStreamPeekOptions {
  readonly correlationId?: string
  readonly afterSequence?: number
}

export interface OutputStreamHandle<TPayload = unknown, TInput = TPayload> {
  readonly id: string
  append(payload: TInput, opts?: StreamAppendOptions): Promise<void>
  pipe(source: AsyncIterable<TInput> | Iterable<TInput>, opts?: StreamAppendOptions): Promise<void>
  writer(opts?: StreamAppendOptions): StreamWriter<TInput>
  read(opts?: StreamReadOptions): Promise<StreamRecord<TPayload> | null>
  list(opts?: StreamListOptions): Promise<StreamRecord<TPayload>[]>
}

export interface StreamWriter<TInput = unknown> {
  write(payload: TInput): Promise<void>
  close(): Promise<void>
}

export interface StreamRecord<TPayload = unknown> {
  readonly id: string
  readonly streamId: string
  readonly sequence: number
  readonly data: TPayload
  readonly correlationId?: string
  readonly contentType: string
  readonly createdAt: string
}

export interface StreamReadOptions {
  readonly cursor?: number
  readonly correlationId?: string
}

export interface StreamListOptions extends StreamReadOptions {
  readonly limit?: number
}

export interface TokenWaitOptions<TSchema extends PayloadSchema<any, any> = PayloadSchema<any, any>> extends WaitOptions {
  readonly schema?: TSchema
}

export interface RunRuntime {
  createToken(opts: RuntimeTokenCreateOptions): Promise<Token>
  waitToken<TPayload>(opts: RuntimeTokenWaitOptions): Promise<WaitResult<TPayload>>
  inputStreamWait<TPayload>(stream: string, schema: PayloadSchema<any, TPayload> | undefined, opts?: InputStreamWaitOptions): Promise<WaitResult<TPayload>>
  inputStreamOnce<TPayload>(stream: string, schema: PayloadSchema<any, TPayload> | undefined, opts?: InputStreamWaitOptions): Promise<WaitResult<TPayload>>
  inputStreamPeek<TPayload>(stream: string, schema: PayloadSchema<any, TPayload> | undefined, opts?: InputStreamPeekOptions): Promise<StreamRecord<TPayload> | null>
  outputStreamAppend(stream: string, payload: unknown, opts?: StreamAppendOptions): Promise<void>
  outputStreamRead<TPayload>(stream: string, schema: PayloadSchema<any, TPayload> | undefined, opts?: StreamReadOptions): Promise<StreamRecord<TPayload> | null>
  outputStreamList<TPayload>(stream: string, schema: PayloadSchema<any, TPayload> | undefined, opts?: StreamListOptions): Promise<StreamRecord<TPayload>[]>
  waitFor(input: DurationInput): Promise<void>
  waitUntil(input: UntilInput): Promise<void>
  metadataSet(key: string, value: unknown): Promise<void>
  metadataPatch(value: Record<string, unknown>): Promise<void>
  metadataIncrement(key: string, amount?: number): Promise<void>
  log(level: "info" | "warn" | "error", values: readonly unknown[]): void
}

export interface RuntimeTokenCreateOptions {
  readonly timeout?: DurationInput
  readonly tags?: readonly string[]
  readonly metadata?: Record<string, unknown>
}

export type RuntimeTokenWaitOptions = WaitOptions & {
  readonly tokenId: string
  readonly schema?: PayloadSchema<any, any>
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

export class WaitResultImpl<TPayload> implements WaitResult<TPayload> {
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
    throw new Error(String(this.error ?? "wait failed"))
  }
}

const concurrentWaitErrorBrand = Symbol.for("helmr.sdk.ConcurrentWaitError")
const waitTimeoutErrorBrand = Symbol.for("helmr.sdk.WaitTimeoutError")
const waitCancelledErrorBrand = Symbol.for("helmr.sdk.WaitCancelledError")

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

export class WaitCancelledError extends Error {
  constructor(message = "wait cancelled") {
    super(message)
    this.name = "WaitCancelledError"
    Object.defineProperty(this, waitCancelledErrorBrand, { value: true })
  }

  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === WaitCancelledError &&
      typeof value === "object" &&
      value !== null &&
      waitCancelledErrorBrand in value
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
  readonly streams?: readonly StreamDefinition[]
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
  ? (opts: TaskRunOptions<TSecrets>) => Promise<SessionStartResult<Awaited<TOutput>>>
  : (payload: TPayloadInput, opts: TaskRunOptions<TSecrets>) => Promise<SessionStartResult<Awaited<TOutput>>>

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
  readonly start: (...args: any[]) => Promise<SessionStartResult<any>>
}

export type TaskPayload<TTask> =
  TTask extends { readonly "~types"?: { readonly payload: infer TPayload } }
    ? [TPayload] extends [NoPayload] ? never : TPayload
    : never
export type SessionStartPayload<TTask> =
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
  readStreamDefinitions(config.streams, `task ${JSON.stringify(config.id)} streams`)
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

export function readStreamDefinitions(value: unknown, label = "streams"): readonly InternalStreamDefinition[] {
  if (value === undefined) {
    return []
  }
  if (!Array.isArray(value)) {
    throw new Error(`${label} must be an array`)
  }
  const seen = new Set<string>()
  return value.map((item, index) => {
    if (item === null || typeof item !== "object" || Array.isArray(item)) {
      throw new Error(`${label}.${index} must be a stream definition`)
    }
    const record = item as Record<string, unknown>
    const id = validateStreamName(record["id"] as string, `${label}.${index}.id`)
    const direction = record["direction"]
    if (direction !== "input" && direction !== "output") {
      throw new Error(`${label}.${index}.direction must be "input" or "output"`)
    }
    const schema = record["schema"]
    if (schema === undefined) {
      throw new Error(`${label}.${index}.schema is required`)
    }
    assertPayloadSchema(schema, `${label}.${index}.schema`)
    const key = `${direction}:${id}`
    if (seen.has(key)) {
      throw new Error(`${label} contains duplicate ${direction} stream ${JSON.stringify(id)}`)
    }
    seen.add(key)
    return { id, direction, schema }
  })
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
