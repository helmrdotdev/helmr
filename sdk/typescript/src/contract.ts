import type { PayloadSchema } from "./schema/payload"

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

export interface QueueDefinition {
  readonly id: string
  readonly concurrencyLimit?: number | null
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

export interface DefinitionRunDefaults {
  readonly queue?: QueueDefinition | string
  readonly maxDuration?: Duration
  readonly ttl?: Duration
  readonly retry?: RetryPolicy
}

declare const workspaceTargetBrand: unique symbol

export interface WorkspaceIdTarget {
  readonly [workspaceTargetBrand]: true
  readonly id: string
  readonly key?: never
}

export interface WorkspaceKeyTarget {
  readonly [workspaceTargetBrand]: true
  readonly id?: never
  readonly key: string
}

export type WorkspaceTarget = WorkspaceIdTarget | WorkspaceKeyTarget

export interface ReadonlyWorkspaceRef extends WorkspaceIdTarget {
  readonly attemptBaseVersionId: string
}

export type RunCause =
  | Readonly<{ type: "api" | "manual" }>
  | Readonly<{ type: "child"; parentRunId: string }>
  | Readonly<{
      type: "schedule"
      scheduleId: string
      scheduledAt: Date
      lastScheduledAt?: Date
      timezone: string
    }>
  | Readonly<{ type: "actor-start" | "continuation" }>

export interface ExecutionContextBase {
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
  readonly workspace: ReadonlyWorkspaceRef
}

export interface TaskExecutionContext extends ExecutionContextBase {
  readonly actor?: never
}

export interface ActorExecutionContext extends ExecutionContextBase {
  readonly actor: Readonly<{
    id: string
    key?: string
  }>
}

export interface HelmrError extends Error {
  readonly code: string
  readonly retryable: boolean
  readonly requestId?: string
}

export type RunStatus =
  | "queued"
  | "running"
  | "waiting"
  | "retry-delayed"
  | "cancel-requested"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "expired"
  | "system-failed"

export interface RunHandle {
  readonly id: string
}

export interface RunError extends HelmrError {
  readonly details?: JsonValue
}

export interface TaskPayloadError extends RunError {
  readonly code: "task_payload_invalid"
  readonly retryable: false
}

export interface RunSnapshot<TOutput extends JsonValue = JsonValue>
  extends RunHandle {
  readonly status: RunStatus
  readonly entrypoint: Readonly<{
    kind: "task" | "actor"
    id: string
  }>
  readonly deployment: Readonly<{ id: string; version: string }>
  readonly workspaceId: string
  readonly actorId?: string
  readonly parentRunId?: string
  readonly parentOwnsLifecycle?: boolean
  readonly currentAttemptNumber: number
  readonly cause: RunCause
  readonly metadata: Metadata
  readonly tags: readonly string[]
  readonly output?: TOutput
  readonly terminalReasonCode?: string
  readonly error?: RunError
  readonly createdAt: string
  readonly startedAt?: string
  readonly terminalAt?: string
}

export type TaskResult<T extends JsonValue> =
  | Readonly<{ ok: true; output: T; run: RunHandle }>
  | Readonly<{ ok: false; error: RunError; run: RunHandle }>

export interface TaskWait<T extends JsonValue>
  extends PromiseLike<TaskResult<T>> {
  unwrap(): Promise<T>
}

export interface TaskStartOptions extends RunOptions {
  readonly idempotencyKey?: string
  readonly workspace: WorkspaceTarget
  readonly signal?: AbortSignal
}

export interface TaskCallOptions extends RunOptions {
  readonly idempotencyKey: string
  readonly workspace: WorkspaceTarget
  readonly signal?: AbortSignal
}

declare const taskDefinitionBrand: unique symbol

export interface TaskDefinitionTypeInfo<
  TInput extends JsonValue,
  TPayload,
  TOutput extends JsonValue,
  THasPayload extends boolean,
> {
  readonly input: TInput
  readonly payload: TPayload
  readonly output: TOutput
  readonly hasPayload: THasPayload
}

export interface TaskDefinition {
  readonly [taskDefinitionBrand]: TaskDefinitionTypeInfo<
    JsonValue,
    unknown,
    JsonValue,
    boolean
  >
  readonly id: string
  readonly hasPayload: boolean
}

export interface PayloadTaskDefinition<
  TInput extends JsonValue,
  TPayload,
  TOutput extends JsonValue,
> extends TaskDefinition {
  readonly [taskDefinitionBrand]: TaskDefinitionTypeInfo<
    TInput,
    TPayload,
    TOutput,
    true
  >
  readonly hasPayload: true
  start(payload: TInput, options: TaskStartOptions): Promise<RunHandle>
  call(payload: TInput, options: TaskCallOptions): TaskWait<TOutput>
}

export interface NoPayloadTaskDefinition<TOutput extends JsonValue>
  extends TaskDefinition {
  readonly [taskDefinitionBrand]: TaskDefinitionTypeInfo<
    never,
    never,
    TOutput,
    false
  >
  readonly hasPayload: false
  start(options: TaskStartOptions): Promise<RunHandle>
  call(options: TaskCallOptions): TaskWait<TOutput>
}

export type TaskHasPayload<TTask extends TaskDefinition> =
  TTask[typeof taskDefinitionBrand]["hasPayload"]
export type TaskPayloadInput<TTask extends TaskDefinition> =
  TTask[typeof taskDefinitionBrand]["input"]
export type TaskHandlerPayload<TTask extends TaskDefinition> =
  TTask[typeof taskDefinitionBrand]["payload"]
export type TaskOutput<TTask extends TaskDefinition> =
  TTask[typeof taskDefinitionBrand]["output"]

export interface TaskConfigBase extends DefinitionRunDefaults {
  readonly id: string
}

export type TaskConfigWithPayload<
  TInput extends JsonValue,
  TPayload,
  TOutput extends JsonValue,
> = TaskConfigBase &
  Readonly<{
    payload: PayloadSchema<TInput, TPayload>
    run(
      payload: TPayload,
      ctx: TaskExecutionContext,
    ): MaybePromise<TOutput>
  }>

export type TaskConfigWithoutPayload<TOutput extends JsonValue> =
  TaskConfigBase &
    Readonly<{
      payload?: never
      run(ctx: TaskExecutionContext): MaybePromise<TOutput>
    }>

export type TaskConfig<
  TInput extends JsonValue = never,
  TPayload = never,
  TOutput extends JsonValue = JsonValue,
> =
  | TaskConfigWithPayload<TInput, TPayload, TOutput>
  | TaskConfigWithoutPayload<TOutput>

export type ActorInputSource =
  | Readonly<{ type: "external" }>
  | Readonly<{ type: "run"; runId: string }>

export interface WaitTimeoutError extends HelmrError {
  readonly code: "wait_timeout"
  readonly retryable: false
}

export interface ActorClosedError extends HelmrError {
  readonly code: "actor_closed"
  readonly retryable: false
}

export interface ActorInputMetadata {
  readonly id: string
  readonly sequence: number
  readonly createdAt: string
  readonly source: ActorInputSource
}

export type ActorInputResult =
  | Readonly<{
      ok: true
      value: JsonValue
      record: ActorInputMetadata
    }>
  | Readonly<{
      ok: false
      error: WaitTimeoutError | ActorClosedError
    }>

export interface ActorReceive extends PromiseLike<ActorInputResult> {
  unwrap(): Promise<JsonValue>
}

export interface SendOptions {
  readonly idempotencyKey?: string
  readonly signal?: AbortSignal
}

export interface ReceiveOptions {
  readonly timeout?: Duration
  readonly idleTimeout?: Duration
  readonly metadata?: Metadata
  readonly tags?: readonly string[]
}

export interface ActorInputRef {
  send(
    input: JsonValue,
    options?: SendOptions,
  ): Promise<{ sequence: number }>
}

export interface ActorInputSelf {
  receive(options?: ReceiveOptions): ActorReceive
}

export interface ActorOutputRecord {
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

export interface OutputAppendOptions {
  readonly contentType?: string
  readonly idempotencyKey?: string
}

export interface OutputSequenceOptions {
  readonly contentType?: string
}

export interface OutputReadOptions {
  readonly after?: number
  readonly limit?: number
  readonly signal?: AbortSignal
}

export interface ActorOutputWriter {
  write(value: Serializable): Promise<ActorOutputRecord>
  close(): Promise<void>
}

export interface ActorOutputSelf {
  append(
    value: Serializable,
    options?: OutputAppendOptions,
  ): Promise<ActorOutputRecord>
  pipe(
    source: AsyncIterable<Serializable> | Iterable<Serializable>,
    options?: OutputSequenceOptions,
  ): Promise<void>
  writer(options?: OutputSequenceOptions): ActorOutputWriter
}

export interface ActorOutputRef {
  read(options?: OutputReadOptions): AsyncIterable<ActorOutputRecord>
  list(
    options?: OutputReadOptions,
  ): Promise<readonly ActorOutputRecord[]>
}

export interface ActorSelf {
  readonly input: ActorInputSelf
  readonly output: ActorOutputSelf
}

export interface ActorConfig extends DefinitionRunDefaults {
  readonly id: string
  readonly idleTimeout?: Duration
  readonly run: (
    self: ActorSelf,
    ctx: ActorExecutionContext,
  ) => MaybePromise<void>
}

export interface ActorStartOptions {
  readonly key?: string
  readonly input?: JsonValue
  readonly idempotencyKey?: string
  readonly workspace: WorkspaceTarget
  readonly metadata?: Metadata
  readonly tags?: readonly string[]
  readonly expiresAt?: Date
  readonly run?: RunOptions
  readonly signal?: AbortSignal
}

export type ActorLifecycle =
  | "open"
  | "closing"
  | "closed"
  | "cancelling"
  | "cancelled"
  | "failed"
  | "expired"

export interface ActorFailure {
  readonly code:
    | "no-progress"
    | "run-failed"
    | "run-expired"
    | "platform-failure"
  readonly runId: string
}

export type ActorManagedRetryPolicy =
  | Readonly<{
      enabled: true
      maxAttempts: number
      backoff: Readonly<{
        minDelay: Duration
        maxDelay: Duration
        factor: number
        jitter: "none" | "full"
      }>
    }>
  | Readonly<{ enabled: false }>

export interface ActorManagedRunOptions {
  readonly queue: string
  readonly concurrencyKey?: string
  readonly priority: number
  readonly ttl?: Duration
  readonly maxDuration: Duration
  readonly retry: ActorManagedRetryPolicy
  readonly metadata: Metadata
  readonly tags: readonly string[]
}

export interface ActorStatus {
  readonly id: string
  readonly key?: string
  readonly lifecycle: ActorLifecycle
  readonly metadata: Metadata
  readonly tags: readonly string[]
  readonly expiresAt?: Date
  readonly run: ActorManagedRunOptions
  readonly createdAt: Date
  readonly updatedAt: Date
  readonly currentRunId?: string
  readonly failure?: ActorFailure
}

export interface ActorUpdateOptions {
  readonly metadata?: Metadata
  readonly tags?: readonly string[]
  readonly expiresAt?: Date
  readonly signal?: AbortSignal
}

export interface ActorOperationOptions {
  readonly idempotencyKey?: string
  readonly signal?: AbortSignal
}

export interface ActorOperationReceipt {
  readonly actorId: string
  readonly lifecycle: "closing" | "closed"
  readonly acceptedAt: Date
}

export interface ActorRefBase {
  readonly input: ActorInputRef
  readonly output: ActorOutputRef
  status(): Promise<ActorStatus>
  update(options: ActorUpdateOptions): Promise<ActorStatus>
  close(options?: ActorOperationOptions): Promise<ActorOperationReceipt>
}

export type ActorIdRef = ActorRefBase &
  Readonly<{ id: string; key?: never }>
export type ActorKeyRef = ActorRefBase &
  Readonly<{ key: string; id?: never }>
export type ActorRef = ActorIdRef | ActorKeyRef

export interface ActorDefinition {
  readonly id: string
  start(
    options: ActorStartOptions,
  ): Promise<{ ref: ActorIdRef; run: RunHandle }>
  ref(address: {
    readonly id: string
    readonly key?: never
  }): ActorIdRef
  ref(address: {
    readonly key: string
    readonly id?: never
  }): ActorKeyRef
}
