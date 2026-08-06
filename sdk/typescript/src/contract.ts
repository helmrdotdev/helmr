import type { PayloadSchema } from "./schema/payload"
import type { RequestOptions } from "./request"
import type { WorkspaceRef } from "./workspace"

export type JsonValue =
  | null
  | boolean
  | number
  | string
  | readonly JsonValue[]
  | { readonly [key: string]: JsonValue }

export type Serializable = JsonValue
export type Metadata = Readonly<Record<string, JsonValue>>
export type MaybePromise<T> = T | Promise<T>
export type { PayloadSchema }

export interface CursorPage<T> {
  readonly items: readonly T[]
  readonly nextCursor?: string
}

export type Duration = string

export type RetryPolicy =
  | Readonly<{
      enabled?: true
      maxAttempts: number
      backoff?: Readonly<{
        minDelay?: Duration
        maxDelay?: Duration
        factor?: number
        jitter?: "none" | "full"
      }>
    }>
  | Readonly<{ enabled: false }>

declare const queueTypeBrand: unique symbol

export interface QueueConfig {
  readonly name: string
  readonly concurrencyLimit?: number | null
}

export interface Queue extends QueueConfig {
  readonly [queueTypeBrand]: true
}

export interface RunOptions {
  readonly queue?: string
  readonly concurrencyKey?: string
  readonly priority?: number
  readonly ttl?: Duration
  readonly retry?: RetryPolicy
  readonly metadata?: Metadata
  readonly tags?: readonly string[]
}

export interface RunDefaults {
  readonly queue?: Queue | string
  readonly maxDuration?: Duration
  readonly ttl?: Duration
  readonly retry?: RetryPolicy
}

declare const workspaceAddressTypeBrand: unique symbol

export interface WorkspaceAddress {
  readonly [workspaceAddressTypeBrand]: true
  readonly id: string
}

export type RunCause =
  | Readonly<{ type: "api" | "manual" }>
  | Readonly<{ type: "child"; parentRunId: string }>
  | Readonly<{
      type: "schedule"
      scheduleId: string
      scheduledAt: string
      lastScheduledAt?: string
      timezone: string
    }>
  | Readonly<{ type: "actor_start" | "continuation" }>

interface RunContext {
  readonly signal: AbortSignal
  readonly run: Readonly<{
    id: string
    attemptNumber: number
    cause: RunCause
  }>
  readonly deployment: Readonly<{
    id: string
    version: string
  }>
  readonly workspace: WorkspaceRef
}

export interface TaskContext extends RunContext {
  readonly task: Readonly<{
    id: string
  }>
  readonly actor?: never
}

export interface ActorContext extends RunContext {
  readonly task?: never
  readonly actor: Readonly<{
    id: string
  }>
}

export interface HelmrError extends Error {
  readonly code: string
  readonly requestId?: string
}

export interface APIError extends HelmrError {
  readonly details?: Readonly<Record<string, JsonValue>>
}

export type RunStatus =
  | "queued"
  | "running"
  | "waiting"
  | "retry_delayed"
  | "cancel_requested"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "expired"
  | "system_failed"

declare const runHandleOutputBrand: unique symbol

export interface RunHandle<TOutput extends JsonValue = JsonValue> {
  readonly id: string
  readonly [runHandleOutputBrand]: TOutput
}

export interface RunFailure {
  readonly code: string
  readonly message: string
  readonly details: Readonly<Record<string, JsonValue>>
}

export interface Run<TOutput extends JsonValue = JsonValue>
  extends RunHandle<TOutput> {
  readonly status: RunStatus
  readonly entrypoint: Readonly<{
    kind: "task" | "actor"
    id: string
  }>
  readonly deployment: Readonly<{ id: string; version: string }>
  readonly workspaceId: string
  readonly sessionId?: string
  readonly parentRunId?: string
  readonly currentAttemptNumber: number
  readonly cause: RunCause
  readonly metadata: Metadata
  readonly tags: readonly string[]
  readonly output?: TOutput
  readonly failure?: RunFailure
  readonly createdAt: string
  readonly startedAt?: string
  readonly terminalAt?: string
}

export type TaskResult<T extends JsonValue> =
  | Readonly<{ ok: true; output: T; run: RunHandle<T> }>
  | Readonly<{ ok: false; failure: RunFailure; run: RunHandle<T> }>

export interface TaskWait<T extends JsonValue>
  extends PromiseLike<TaskResult<T>> {
  unwrap(): Promise<T>
}

export interface TaskStartOptions extends RunOptions {
  readonly idempotencyKey?: string
  readonly workspace: WorkspaceRef
  readonly signal?: AbortSignal
}

export interface TaskCallOptions extends RunOptions {
  readonly idempotencyKey: string
  readonly workspace: WorkspaceRef
  readonly signal?: AbortSignal
}

declare const taskBrand: unique symbol

interface TaskTypeInfo<
  TIdentifier extends string,
  TInput extends JsonValue,
  TOutput extends JsonValue,
> {
  readonly identifier: TIdentifier
  readonly input: TInput
  readonly output: TOutput
}

export interface Task<
  TIdentifier extends string = string,
  TInput extends JsonValue = JsonValue,
  TOutput extends JsonValue = JsonValue,
> {
  readonly [taskBrand]: TaskTypeInfo<TIdentifier, TInput, TOutput>
  readonly id: TIdentifier
  readonly hasPayload: boolean
  start(
    ...args: [TInput] extends [never]
      ? [options: TaskStartOptions]
      : [payload: TInput, options: TaskStartOptions]
  ): Promise<RunHandle<TOutput>>
  call(
    ...args: [TInput] extends [never]
      ? [options: TaskCallOptions]
      : [payload: TInput, options: TaskCallOptions]
  ): TaskWait<TOutput>
}

export type TaskInput<TTask extends Task> =
  TTask[typeof taskBrand]["input"]
export type TaskOutput<TTask extends Task> =
  TTask[typeof taskBrand]["output"]

export type TaskConfigWithPayload<
  TIdentifier extends string,
  TInput extends JsonValue,
  TPayload,
  TOutput extends JsonValue,
> = RunDefaults &
  Readonly<{
    id: TIdentifier
    payload: PayloadSchema<TInput, TPayload>
    run(
      payload: TPayload,
      ctx: TaskContext,
    ): MaybePromise<TOutput>
  }>

export type TaskConfigWithoutPayload<
  TIdentifier extends string,
  TOutput extends JsonValue,
> = RunDefaults &
    Readonly<{
      id: TIdentifier
      payload?: never
      run(ctx: TaskContext): MaybePromise<TOutput>
    }>

export type TaskConfig<
  TIdentifier extends string = string,
  TInput extends JsonValue = never,
  TPayload = never,
  TOutput extends JsonValue = JsonValue,
> =
  | TaskConfigWithPayload<TIdentifier, TInput, TPayload, TOutput>
  | TaskConfigWithoutPayload<TIdentifier, TOutput>

export type SessionInputSource =
  | Readonly<{ type: "external" }>
  | Readonly<{ type: "run"; runId: string }>

export interface WaitTimeoutError extends HelmrError {
  readonly code: "wait_timeout"
}

export interface SessionClosedError extends HelmrError {
  readonly code: "session_closed"
}

export interface SessionInputMetadata {
  readonly id: string
  readonly sequence: number
  readonly createdAt: string
  readonly source: SessionInputSource
}

export interface SessionInputRecord extends SessionInputMetadata {
  readonly data: JsonValue
}

export type ActorSessionInputResult =
  | Readonly<{
      ok: true
      value: JsonValue
      record: SessionInputMetadata
    }>
  | Readonly<{
      ok: false
      error: WaitTimeoutError | SessionClosedError
    }>

export interface ActorSessionReceive extends PromiseLike<ActorSessionInputResult> {
  unwrap(): Promise<JsonValue>
}

export interface SessionInputSendRequest {
  readonly idempotencyKey?: string
}

export interface ActorSessionReceiveOptions {
  readonly timeout?: Duration
  readonly idleTimeout?: Duration
  readonly metadata?: Metadata
  readonly tags?: readonly string[]
}

export interface ActorSessionInput {
  receive(options?: ActorSessionReceiveOptions): ActorSessionReceive
}

export interface SessionOutputRecord {
  readonly id: string
  readonly sequence: number
  readonly data: unknown
  readonly contentType: string
  readonly createdAt: string
  readonly provenance: Readonly<{
    runId: string
    attemptNumber: number
    deploymentId: string
  }>
}

export interface ActorSessionOutputAppendOptions {
  readonly contentType?: string
  readonly idempotencyKey?: string
}

export interface ActorSessionOutputSequenceOptions {
  readonly contentType?: string
}

export interface SessionOutputQuery {
  readonly after?: number
  readonly limit?: number
}

export interface SessionOutputWriter {
  write(value: Serializable): Promise<SessionOutputRecord>
  close(): Promise<void>
}

export interface ActorSessionOutput {
  append(
    value: Serializable,
    options?: ActorSessionOutputAppendOptions,
  ): Promise<SessionOutputRecord>
  pipe(
    source: AsyncIterable<Serializable> | Iterable<Serializable>,
    options?: ActorSessionOutputSequenceOptions,
  ): Promise<void>
  writer(options?: ActorSessionOutputSequenceOptions): SessionOutputWriter
}

export interface SessionOutputPage {
  readonly records: readonly SessionOutputRecord[]
  readonly nextAfter: number
  readonly hasMore: boolean
}

export interface ActorSession {
  readonly id: string
  readonly key?: string
  readonly input: ActorSessionInput
  readonly output: ActorSessionOutput
}

export interface ActorConfig extends RunDefaults {
  readonly id: string
  readonly idleTimeout?: Duration
  readonly run: (
    session: ActorSession,
    ctx: ActorContext,
  ) => MaybePromise<void>
}

export interface ActorStartOptions {
  readonly key?: string
  readonly input?: JsonValue
  readonly idempotencyKey?: string
  readonly workspace: WorkspaceRef
  readonly run?: RunOptions
  readonly signal?: AbortSignal
}

export type SessionStatus = "open" | "closed" | "cancelled" | "failed"

export type SessionFailureCode =
  | "cancelled"
  | "no_progress"
  | "run_failed"
  | "run_expired"
  | "platform_failure"

export interface SessionFailure {
  readonly code: SessionFailureCode
  readonly message: string
  readonly details: Readonly<{ runId?: string }>
}

export interface Session {
  readonly id: string
  readonly actorId: string
  readonly deploymentId: string
  readonly key?: string
  readonly status: SessionStatus
  readonly createdAt: string
  readonly updatedAt: string
  readonly currentRunId?: string
  readonly failure?: SessionFailure
}

export interface SessionCloseRequest {
  readonly idempotencyKey?: string
}

export interface SessionCloseReceipt {
  readonly sessionId: string
  readonly acceptedAt: string
}

export interface SessionRef {
  readonly id: string
  readonly input: Readonly<{
    send(
      input: JsonValue,
      request?: SessionInputSendRequest,
      options?: RequestOptions,
    ): Promise<SessionInputRecord>
  }>
  readonly output: Readonly<{
    list(
      query?: SessionOutputQuery,
      options?: RequestOptions,
    ): Promise<SessionOutputPage>
  }>
  retrieve(options?: RequestOptions): Promise<Session>
  close(
    request?: SessionCloseRequest,
    options?: RequestOptions,
  ): Promise<SessionCloseReceipt>
}

declare const actorTypeBrand: unique symbol

export interface ActorStartResult {
  readonly session: SessionRef
  readonly run: RunHandle<null>
}

export interface Actor {
  readonly [actorTypeBrand]: true
  readonly id: string
  start(options: ActorStartOptions): Promise<ActorStartResult>
}
