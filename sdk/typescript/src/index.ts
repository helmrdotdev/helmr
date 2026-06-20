import {
  ConcurrentWaitError,
  getRuntimeWaitOperand,
  getRunRuntime,
  runtimeWaitOperand,
  validateChannelName,
  WaitTimeoutError,
  ImageBuilderImpl,
  SourceDirRefImpl,
  SourceFileRefImpl,
  validateSecretName,
  type CacheBuilder,
  type CacheMount,
  type CacheMountBinding,
  type ImageBuilder,
  type ImageRunOptions,
  type WaitpointHandle,
  type WaitDelayHandle,
  type WaitpointResult,
  type NoPayload,
  type Placement,
  type SandboxBuilder,
  type SandboxNetwork,
  type SecretDecls,
  type SecretMountBinding,
  type SourceCapabilities,
  type SourceDirRef,
  type SourceDirectoryOptions,
  type SourceFileRef,
  type ChannelOutputAppendOptions,
  type ChannelOutputDefinition,
  type ChannelInputDefinition,
  type ChannelInputWaitOptions,
  type ChannelInputHandle,
  type ChannelOutputHandle,
  type TaskContext,
  type TaskSessionContext,
  type TaskWorkspace,
  type TaskOutput,
  type TaskPayload,
  type TaskStartPayload,
  type RuntimeWaitpointTokenCreateOptions,
  type RuntimeWaitOperand,
  type WaitDurationInput,
  type WaitUntilInput,
  type WaitJson,
} from "./internal"
import { PayloadSchemaValidationError } from "./schema/payload"
import { idempotencyKeys } from "./idempotency"
export type { IdempotencyKey, IdempotencyKeyCreateOptions, IdempotencyKeyInput, IdempotencyKeyScope } from "./idempotency"
import type { PayloadSchema, StandardSchemaV1 } from "./schema/payload"
import {
  HelmrClient,
  type WaitpointToken,
  type WaitpointTokenCompleteOptions,
  type WaitpointTokenCreateOptions,
  type WaitpointTokenListOptions,
  type WaitpointTokenRef,
  type WaitpointTokenRetrieveOptions,
} from "./runtime/client"
export type {
  ChannelRecord,
  ListSchedulesOptions,
  OpenTaskSessionApi,
  PublicAccessToken,
  PublicAccessTokenCreateOptions,
  PublicAccessTokenScope,
  RetrieveScheduleOptions,
  Schedule,
  ScheduleCreateOptions,
  ScheduleUpdateOptions,
  SchedulesApi,
  SessionChannelInputApi,
  SessionChannelInputSendOptions,
  SessionChannelInputSendResult,
  SessionChannelListOptions,
  SessionChannelOutputApi,
  SessionChannelOutputStreamOptions,
  TaskSessionCancelOptions,
  TaskSessionCloseOptions,
  TaskSessionHandle,
  TaskSessionListOptions,
  TaskSessionRun,
  TaskSessionSnapshot,
  TaskSessionStatus,
  TaskSessionWaitOptions,
  TaskStartAndWaitOptions,
  TaskStartOptions,
  TaskStartResult,
  WaitpointToken,
  WaitpointTokenRef,
  WaitpointTokenCompleteOptions,
  WaitpointTokenCreateOptions,
  WaitpointTokenListOptions,
  WaitpointTokenRetrieveOptions,
  Workspace,
  WorkspaceCreateOptions,
  WorkspaceDesiredState,
  WorkspaceDirtyState,
  WorkspaceExec,
  WorkspaceExecCreateOptions,
  WorkspaceExecHandle,
  WorkspaceExecListOptions,
  WorkspaceExecReadableStreamApi,
  WorkspaceExecState,
  WorkspaceExecStdinApi,
  WorkspaceExecsApi,
  WorkspaceHandle,
  WorkspaceListOptions,
  WorkspaceMaterialization,
  WorkspaceMaterializeOptions,
  WorkspacePty,
  WorkspacePtyApi,
  WorkspacePtyCreateOptions,
  WorkspacePtyHandle,
  WorkspacePtyListOptions,
  WorkspacePtyOutputApi,
  WorkspacePtyState,
  WorkspaceRetrieveOptions,
  WorkspaceStreamChunk,
  WorkspaceStreamFollowOptions,
  WorkspaceStreamListOptions,
  WorkspaceStreamWriteOptions,
  WorkspacesApi,
  WorkspaceState,
  WorkspaceUpdateOptions,
  RunWaitpointsApi,
} from "./runtime/client"
import { sandbox } from "./sandbox"
import { defineConfig, type HelmrConfig, type HelmrConfigInput } from "./config"
import { queue, task, type Task, type TaskConfig, type TaskQueueConfig } from "./task"
import {
  task as scheduledTask,
  type ScheduleCron,
  type ScheduledTaskConfig,
  type ScheduledTaskPayload,
} from "./schedules"
import { getDefaultClient, tasks } from "./start"

type SchemaInput<TSchema> = TSchema extends PayloadSchema<infer TInput, any> ? TInput : never
type SchemaOutput<TSchema> = TSchema extends PayloadSchema<any, infer TOutput> ? TOutput : never
export { AuthError, RunNotFoundError, TimeoutError, UnsupportedTransportError } from "./runtime/errors"
export { ConcurrentWaitError, WaitTimeoutError, PayloadSchemaValidationError }
export {
  type LogSnapshot,
  type ListRunEventsOptions,
  type ListRunsOptions,
  type WaitpointRef,
  type PendingWaitpoint,
  type RetrieveRunOptions,
  type RunEvent,
  type RunEventRecord,
  type RunEventRecordPage,
  type RunHandle,
  type RunOutput,
  type RunSnapshot,
  type RunStatus,
  type RunSummary,
  type RunWaitpointOptions,
  type SubscribeRunEventsOptions,
} from "./runtime/run"
export { defineConfig, idempotencyKeys, queue, sandbox, task, tasks }
export type {
  HelmrConfig,
  HelmrConfigInput,
  Placement,
  SandboxNetwork,
  SecretDecls,
  Task,
  TaskConfig,
  TaskQueueConfig,
  TaskOutput,
  TaskPayload,
  TaskStartPayload,
  WaitpointHandle,
  WaitDelayHandle,
  WaitpointResult,
  ChannelOutputAppendOptions,
  ChannelOutputDefinition,
  ChannelInputDefinition,
  ChannelInputWaitOptions,
  ChannelInputHandle,
  ChannelOutputHandle,
  WaitDurationInput,
  WaitUntilInput,
  WaitJson,
  RuntimeWaitOperand,
  PayloadSchema,
  StandardSchemaV1,
  NoPayload,
}

export const image = (id: string): ImageBuilder => new ImageBuilderImpl(id)

export const cache = (id: string): CacheBuilder => ({ id })

export const source: SourceCapabilities = {
  file(path: string): SourceFileRef {
    return new SourceFileRefImpl(path)
  },
  directory(path: string, opts?: SourceDirectoryOptions): SourceDirRef {
    return new SourceDirRefImpl(path, opts?.ignore ? [...opts.ignore] : [])
  },
}

export { HelmrClient }
export { validateSecretName }
export const runs = new Proxy({} as HelmrClient["runs"], {
  get(_target, property, receiver) {
    return Reflect.get(getDefaultClient().runs, property, receiver)
  },
})

export const channels = Object.freeze({
  input<TSchema extends PayloadSchema<any, any>>(
    id: string,
    opts: { readonly schema: TSchema },
  ): ChannelInputDefinition<SchemaOutput<TSchema>, SchemaInput<TSchema>> {
    return Object.freeze({ id: validateChannelName(id), schema: opts.schema })
  },
  output<TSchema extends PayloadSchema<any, any>>(
    id: string,
    opts: { readonly schema: TSchema },
  ): ChannelOutputDefinition<SchemaOutput<TSchema>, SchemaInput<TSchema>> {
    return Object.freeze({ id: validateChannelName(id), schema: opts.schema })
  },
})

export const metadata = Object.freeze({
  set(key: string, value: unknown) {
    return getRunRuntime().metadataSet(key, value)
  },
  patch(value: Record<string, unknown>) {
    return getRunRuntime().metadataPatch(value)
  },
  increment(key: string, amount = 1) {
    return getRunRuntime().metadataIncrement(key, amount)
  },
})

export const wait = Object.freeze({
  async createToken(opts: WaitpointTokenCreateOptions & { readonly timeout?: WaitDurationInput } = {}): Promise<WaitpointToken> {
    if (runRuntimeIsActive()) {
      return await getRunRuntime().createWaitpointToken(normalizeRuntimeWaitpointTokenCreateOptions(opts))
    }
    return await getDefaultClient().waitpoints.tokens.create(normalizeWaitpointTokenCreateOptions(opts))
  },
  retrieveToken(id: string, opts?: WaitpointTokenRetrieveOptions) {
    return getDefaultClient().waitpoints.tokens.retrieve(id, opts)
  },
  async listTokens(opts?: WaitpointTokenListOptions) {
    return await getDefaultClient().waitpoints.tokens.list(opts)
  },
  listTokensPage(opts?: WaitpointTokenListOptions) {
    return getDefaultClient().waitpoints.tokens.listPage(opts)
  },
  for(input: WaitDurationInput): WaitDelayHandle {
    return waitDelayHandle(
      { type: "for", input },
      () => getRunRuntime().waitFor(input),
    )
  },
  until(input: WaitUntilInput): WaitDelayHandle {
    return waitDelayHandle(
      { type: "until", input },
      () => getRunRuntime().waitUntil(input),
    )
  },
  forToken<TSchema extends PayloadSchema<any, any>>(
    token: WaitpointToken | WaitpointTokenRef | string,
    opts?: {
      readonly schema?: TSchema
      readonly timeout?: WaitDurationInput
      readonly tags?: string | readonly string[]
      readonly metadata?: { readonly [key: string]: WaitJson }
    },
  ): WaitpointHandle<TSchema extends PayloadSchema<any, infer TOutput> ? TOutput : unknown> {
    const tokenId = typeof token === "string" ? token : token.id
    const tokenTimeoutAt = typeof token === "string" ? undefined : "timeoutAt" in token ? token.timeoutAt : undefined
    const timeout = opts?.timeout ?? waitpointTokenTimeoutInput(tokenTimeoutAt)
    const schema = opts?.schema
    const options = {
      params: {
        token_id: tokenId,
      },
      ...(timeout === undefined ? {} : { timeout }),
      ...(opts?.metadata === undefined ? {} : { metadata: opts.metadata }),
      ...(opts?.tags === undefined ? {} : { tags: opts.tags }),
      ...(schema === undefined ? {} : { schema }),
    }
    const operand = { type: "waitpoint", options } satisfies RuntimeWaitOperand
    return waitpointHandle(
      operand,
      () => getRunRuntime().waitpoint(options),
    ) as WaitpointHandle<TSchema extends PayloadSchema<any, infer TOutput> ? TOutput : unknown>
  },
  async all(operands: readonly unknown[]): Promise<readonly unknown[]> {
    const normalized = operands.map((operand, index) => {
      const waitOperand = getRuntimeWaitOperand(operand)
      if (waitOperand === undefined) {
        throw new Error(`wait.all operand at index ${index} is not a Helmr wait handle`)
      }
      return waitOperand
    })
    if (normalized.length === 0) {
      throw new Error("wait.all requires at least one operand")
    }
    return await getRunRuntime().waitAll(normalized)
  },
  completeToken(
    token: WaitpointToken | WaitpointTokenRef | string,
    data: unknown,
    opts?: WaitpointTokenCompleteOptions,
  ) {
    return getDefaultClient().waitpoints.tokens.complete(token, data, opts)
  },
})

export const logger = Object.freeze({
  info(...values: unknown[]) {
    getRunRuntime().log("info", values)
  },
  warn(...values: unknown[]) {
    getRunRuntime().log("warn", values)
  },
  error(...values: unknown[]) {
    getRunRuntime().log("error", values)
  },
})

export const schedules = Object.freeze({
  task: scheduledTask,
  create: (...args: Parameters<HelmrClient["schedules"]["create"]>) => getDefaultClient().schedules.create(...args),
  update: (...args: Parameters<HelmrClient["schedules"]["update"]>) => getDefaultClient().schedules.update(...args),
  list: (...args: Parameters<HelmrClient["schedules"]["list"]>) => getDefaultClient().schedules.list(...args),
  retrieve: (...args: Parameters<HelmrClient["schedules"]["retrieve"]>) => getDefaultClient().schedules.retrieve(...args),
  activate: (...args: Parameters<HelmrClient["schedules"]["activate"]>) => getDefaultClient().schedules.activate(...args),
  deactivate: (...args: Parameters<HelmrClient["schedules"]["deactivate"]>) => getDefaultClient().schedules.deactivate(...args),
  delete: (...args: Parameters<HelmrClient["schedules"]["delete"]>) => getDefaultClient().schedules.delete(...args),
})

export type {
  ScheduleCron,
  ScheduledTaskConfig,
  ScheduledTaskPayload,
}

export type {
  CacheBuilder,
  CacheMount,
  CacheMountBinding,
  ImageBuilder,
  ImageRunOptions,
  SandboxBuilder,
  SecretMountBinding,
  SourceCapabilities,
  SourceDirRef,
  SourceDirectoryOptions,
  SourceFileRef,
  TaskContext,
  TaskSessionContext,
  TaskWorkspace,
  RuntimeWaitpointTokenCreateOptions,
}

function waitpointHandle<TPayload>(
  operand: RuntimeWaitOperand,
  factory: () => Promise<WaitpointResult<TPayload>>,
): WaitpointHandle<TPayload> {
  let promise: Promise<WaitpointResult<TPayload>> | undefined
  const getPromise = () => {
    promise ??= factory()
    return promise
  }
  const handle: WaitpointHandle<TPayload> = {
    then<TResult1 = WaitpointResult<TPayload>, TResult2 = never>(
      onfulfilled?: ((value: WaitpointResult<TPayload>) => TResult1 | PromiseLike<TResult1>) | null,
      onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
    ): PromiseLike<TResult1 | TResult2> {
      return getPromise().then(onfulfilled, onrejected)
    },
    unwrap: async () => (await getPromise()).unwrap(),
  }
  Object.defineProperty(handle, runtimeWaitOperand, { value: operand })
  return handle
}

function waitDelayHandle(operand: RuntimeWaitOperand, factory: () => Promise<void>): WaitDelayHandle {
  let promise: Promise<void> | undefined
  const getPromise = () => {
    promise ??= factory()
    return promise
  }
  const handle: WaitDelayHandle = {
    then<TResult1 = void, TResult2 = never>(
      onfulfilled?: ((value: void) => TResult1 | PromiseLike<TResult1>) | null,
      onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
    ): PromiseLike<TResult1 | TResult2> {
      return getPromise().then(onfulfilled, onrejected)
    },
    unwrap: () => getPromise(),
  }
  Object.defineProperty(handle, runtimeWaitOperand, { value: operand })
  return handle
}

function runRuntimeIsActive(): boolean {
  try {
    getRunRuntime()
    return true
  } catch {
    return false
  }
}

function normalizeWaitpointTokenCreateOptions(
  opts: WaitpointTokenCreateOptions & { readonly timeout?: WaitDurationInput },
): WaitpointTokenCreateOptions {
  const { timeout, ...clientOpts } = opts
  if (timeout === undefined) {
    return clientOpts
  }
  if (clientOpts.timeoutAt !== undefined || clientOpts.timeoutInSeconds !== undefined) {
    throw new Error("wait.createToken timeout cannot be combined with timeoutAt or timeoutInSeconds")
  }
  const { timeoutAt: _timeoutAt, timeoutInSeconds: _timeoutInSeconds, ...baseOpts } = clientOpts
  return {
    ...baseOpts,
    timeoutInSeconds: Math.ceil(waitDurationMilliseconds(timeout) / 1000),
  }
}

function normalizeRuntimeWaitpointTokenCreateOptions(
  opts: WaitpointTokenCreateOptions & { readonly timeout?: WaitDurationInput },
): RuntimeWaitpointTokenCreateOptions {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    throw new Error("wait.createToken cannot override projectId or environmentId inside a running task")
  }
  if (opts.signal !== undefined) {
    throw new Error("wait.createToken signal is not supported inside a running task")
  }
  return normalizeWaitpointTokenCreateOptions(opts)
}

function waitpointTokenTimeoutInput(timeoutAt: string | Date | null | undefined): WaitDurationInput | undefined {
  if (timeoutAt === null || timeoutAt === undefined) {
    return undefined
  }
  const at = timeoutAt instanceof Date ? timeoutAt : new Date(timeoutAt)
  const milliseconds = at.getTime() - Date.now()
  if (!Number.isFinite(milliseconds)) {
    throw new Error("wait.forToken token timeoutAt must be a valid date")
  }
  return { milliseconds: Math.max(1, milliseconds) }
}

function waitDurationMilliseconds(input: WaitDurationInput): number {
  if (typeof input === "number") return positiveMilliseconds(input * 1000)
  if (typeof input === "string") return parseDurationMilliseconds(input)
  if (input.milliseconds !== undefined) return positiveMilliseconds(input.milliseconds)
  if (input.seconds !== undefined) return positiveMilliseconds(input.seconds * 1000)
  if (input.minutes !== undefined) return positiveMilliseconds(input.minutes * 60_000)
  if (input.hours !== undefined) return positiveMilliseconds(input.hours * 3_600_000)
  if (input.duration !== undefined) return parseDurationMilliseconds(input.duration)
  throw new Error("duration requires milliseconds, seconds, minutes, hours, or duration")
}

function parseDurationMilliseconds(value: string): number {
  const match = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(value.trim())
  if (match === null) {
    throw new Error("duration must use ms, s, m, or h units")
  }
  const amount = Number(match[1])
  const unit = match[2]
  return positiveMilliseconds(amount * (unit === "ms" ? 1 : unit === "s" ? 1000 : unit === "m" ? 60_000 : 3_600_000))
}

function positiveMilliseconds(value: number): number {
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`duration must be positive: ${value}`)
  }
  return Math.ceil(value)
}
