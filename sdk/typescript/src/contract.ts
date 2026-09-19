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

export type TurnSource =
  | Readonly<{ type: "external" }>
  | Readonly<{ type: "run"; runId: string }>

export interface WaitTimeoutError extends HelmrError {
  readonly code: "wait_timeout"
}

export interface ActorSessionReceiveOptions {
  readonly timeout?: Duration
  readonly idleTimeout?: Duration
  readonly metadata?: Metadata
  readonly tags?: readonly string[]
}

export interface SessionOperationOptions {
  readonly idempotencyKey?: string
}

export interface OutputReceipt {
  readonly id: string
  readonly sequence: number
  readonly sessionId: string
  readonly turnId: string | null
  readonly runId: string
  readonly attemptNumber: number
  readonly runGeneration: number
}

export interface RecordWriter<TValue = JsonValue> {
  write(
    value: TValue,
    options?: SessionOperationOptions,
  ): Promise<OutputReceipt>
  pipe(source: AsyncIterable<TValue> | Iterable<TValue>): Promise<void>
}

export interface Message<TData = JsonValue> {
  readonly id: string
  readonly data: TData
}

export interface Turn {
  readonly id: string
  readonly sequence: number
  readonly input: JsonValue
  readonly source: TurnSource
  readonly createdAt: string
  readonly signal: AbortSignal
  readonly output: RecordWriter
  onMessage(
    handler: (message: Message) => MaybePromise<void>,
  ): Promise<void>
  complete(result?: JsonValue): Promise<void>
  fail(error: unknown): Promise<void>
}

export interface ActorSession {
  readonly id: string
  readonly key?: string
  readonly output: RecordWriter
  receive(
    options?: ActorSessionReceiveOptions,
  ): Promise<Turn | null>
}

export interface ActorConfig extends RunDefaults {
  readonly id: string
  readonly idleTimeout?: Duration
  readonly run: (session: ActorSession, ctx: ActorContext) => MaybePromise<void>
}

export interface ActorStartOptions {
  readonly key?: string
  readonly idempotencyKey?: string
  readonly workspace: WorkspaceRef
  readonly run?: RunOptions
  readonly signal?: AbortSignal
}

export type SessionStatus = "open" | "closing" | "closed" | "failed"
export type SessionFailureCode = string
export interface SessionFailure {
  readonly code: SessionFailureCode
  readonly message: string
  readonly details: Readonly<{ runId?: string }>
}
export type SessionDispatch =
  | Readonly<{ state: "ready" }>
  | Readonly<{
      state: "held"
      holdId: string
      reason:
        | "interrupt_requested"
        | "interrupted"
        | "recovery_required"
        | "recovered"
    }>

export interface Session {
  readonly id: string
  readonly actorId: string
  readonly deploymentId: string
  readonly workspaceId: string
  readonly key?: string
  readonly status: SessionStatus
  readonly createdAt: string
  readonly updatedAt: string
  readonly currentRunId: string | null
  readonly activeTurnId: string | null
  readonly dispatch: SessionDispatch
  readonly failure?: SessionFailure
}

export type TurnStatus =
  | "queued"
  | "running"
  | "completed"
  | "failed"
  | "interrupted"
export interface TurnState {
  readonly id: string
  readonly sessionId: string
  readonly sequence: number
  readonly input: JsonValue
  readonly source: TurnSource
  readonly status: TurnStatus
  readonly createdAt: string
  readonly interruptRequested: boolean
  readonly acceptsMessages: boolean
  readonly terminalEventId?: string
  readonly workspaceVersionId?: string
  readonly result?: JsonValue
  readonly error?: JsonValue
}

export interface SessionCloseReceipt {
  readonly id: string
  readonly sessionId: string
  readonly status: "accepted"
}
export interface TurnInterruptReceipt {
  readonly id: string
  readonly sessionId: string
  readonly turnId: string
  readonly holdId: string
  readonly status: "accepted"
}
export interface SessionResumeReceipt {
  readonly id: string
  readonly sessionId: string
  readonly holdId: string
  readonly status: "accepted"
}
export interface SessionRecoveryReceipt {
  readonly id: string
  readonly sessionId: string
  readonly turnId: string | null
  readonly holdId: string
  readonly status: "accepted"
}
export interface SessionResumeRequest extends SessionOperationOptions {
  readonly holdId: string
}
export type SessionRecoverRequest = SessionOperationOptions &
  Readonly<{
    holdId: string
    workspaceVersionId: string
    reconciliationRef: string
  }> &
  (
    | Readonly<{ turnId: string; disposition: "failed" | "interrupted" }>
    | Readonly<{ turnId: null; disposition?: never }>
  )

export type SessionAdmissionReceipt =
  | Readonly<{ id: string; kind: "enqueued"; turnId: string }>
  | Readonly<{
      id: string
      kind: "messaged"
      turnId: string
      messageId: string
    }>
export interface SessionMessageReceipt {
  readonly id: string
  readonly turnId: string
  readonly messageId: string
  readonly status: "accepted"
}
export interface MessageReceipt {
  readonly id: string
  readonly status: "accepted"
}

export type SessionEventKind =
  | "output"
  | "turn.enqueued"
  | "turn.started"
  | "turn.interrupt_requested"
  | "turn.completed"
  | "turn.failed"
  | "turn.interrupted"
  | "message.accepted"
  | "message.handled"
  | "message.rejected"
  | "message.unknown"
  | "session.closing"
  | "session.closed"
  | "session.failed"
  | "session.held"
  | "session.resumed"
  | "session.recovered"
export interface SessionEvent {
  readonly id: string
  readonly sessionId: string
  readonly turnId: string | null
  readonly sequence: number
  readonly createdAt: string
  readonly kind: SessionEventKind
  readonly data: JsonValue
  readonly provenance: Readonly<{
    runId: string
    attemptNumber: number
    runGeneration: number
    deploymentId: string
  }> | null
}
export interface SessionEventQuery {
  readonly after?: number
  readonly limit?: number
}
export interface SessionEventPage {
  readonly records: readonly SessionEvent[]
  readonly nextAfter: number
  readonly hasMore: boolean
  readonly retainedAfter: number
}

export interface TurnRef {
  readonly id: string
  readonly sessionId: string
  send(
    data: JsonValue,
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<MessageReceipt>
  retrieve(options?: RequestOptions): Promise<TurnState>
  interrupt(
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<TurnInterruptReceipt>
}
export type SessionSendResult =
  | Readonly<{ kind: "enqueued"; turn: TurnRef }>
  | Readonly<{
      kind: "messaged"
      turn: TurnRef
      message: MessageReceipt
    }>
export interface SessionRef {
  readonly id: string
  send(
    data: JsonValue,
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<SessionSendResult>
  enqueue(
    input: JsonValue,
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<TurnRef>
  turn(id: string): TurnRef
  readonly events: Readonly<{
    list(
      query?: SessionEventQuery,
      options?: RequestOptions,
    ): Promise<SessionEventPage>
  }>
  retrieve(options?: RequestOptions): Promise<Session>
  close(
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<SessionCloseReceipt>
  resume(
    request: SessionResumeRequest,
    options?: RequestOptions,
  ): Promise<SessionResumeReceipt>
}

declare const actorTypeBrand: unique symbol
export interface ActorStartResult {
  readonly session: SessionRef
  readonly run: RunHandle<null>
}
export interface Actor {
  readonly [actorTypeBrand]: true
  readonly id: string
  start(
    options: ActorStartOptions,
  ): Promise<ActorStartResult>
}
