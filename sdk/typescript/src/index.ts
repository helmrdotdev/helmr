import {
  ConcurrentWaitError,
  getRunRuntime,
  registerStreamDefinition,
  validateStreamName,
  WaitCancelledError,
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
  type WaitHandle,
  type WaitDelayHandle,
  type WaitResult,
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
  type StreamAppendOptions,
  type StreamListOptions,
  type StreamReadOptions,
  type StreamRecord,
  type StreamWriter,
  type OutputStreamDefinition,
  type InputStreamDefinition,
  type InputStreamPeekOptions,
  type InputStreamSendOptions,
  type InputStreamWaitOptions,
  type InputStreamHandle,
  type OutputStreamHandle,
  type TaskContext,
  type SessionContext,
  type TaskWorkspace,
  type TaskOutput,
  type TaskPayload,
  type SessionStartPayload,
  type RuntimeTokenCreateOptions,
  type RuntimeTokenWaitOptions,
  type TokenWaitOptions,
  type DurationInput,
  type UntilInput,
  type WaitJson,
} from "./internal"
import { PayloadSchemaValidationError } from "./schema/payload"
import { idempotencyKeys } from "./idempotency"
export type { IdempotencyKey, IdempotencyKeyCreateOptions, IdempotencyKeyInput, IdempotencyKeyScope } from "./idempotency"
import { parsePayloadWithSchema, type PayloadSchema, type StandardSchemaV1 } from "./schema/payload"
import {
  HelmrClient,
  type Token as ClientToken,
  type TokenCreateOptions,
  type TokenRef,
} from "./runtime/client"
export type {
  ListSchedulesOptions,
  OpenSessionApi,
  PublicAccessToken,
  PublicAccessTokenCreateOptions,
  PublicAccessTokenScope,
  RetrieveScheduleOptions,
  Schedule,
  ScheduleCreateOptions,
  ScheduleRef,
  ScheduleUpdateOptions,
  SchedulesApi,
  SessionInputSendOptions,
  SessionInputSendResult,
  SessionInputStreamApi,
  SessionOutputAppendOptions,
  SessionOutputStreamApi,
  SessionStreamListOptions,
  SessionStreamReadOptions,
  SessionCancelOptions,
  SessionCloseOptions,
  SessionHandle,
  SessionListOptions,
  SessionRun,
  SessionSnapshot,
  SessionActivity,
  SessionStatus,
  SessionStartAndWaitOptions,
  SessionStartOptions,
  SessionStartAndWaitResult,
  SessionStartResult,
  TokenRef,
  TokenCompleteOptions,
  TokenCreateOptions,
  TokenListOptions,
  TokenRetrieveOptions,
  TokensApi,
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
  WorkspaceStopOptions,
  WorkspaceStopResult,
  WorkspaceStreamChunk,
  WorkspaceStreamError,
  WorkspaceStreamFollowOptions,
  WorkspaceStreamListOptions,
  WorkspaceStreamTerminal,
  WorkspaceStreamTerminalError,
  WorkspaceStreamWriteOptions,
  WorkspacesApi,
  WorkspaceState,
  WorkspaceUpdateOptions,
} from "./runtime/client"
import { sandbox } from "./sandbox"
import { defineConfig, type HelmrConfig, type HelmrConfigInput } from "./config"
import { queue, task, type QueueConfig, type QueueDefinition, type Task, type TaskConfig } from "./task"
import {
  task as scheduledTask,
  type ScheduleCron,
  type ScheduledTaskConfig,
  type ScheduledTaskPayload,
} from "./schedules"
import { defaultClientNamespace, getDefaultClient, sessions } from "./start"

type SchemaInput<TSchema> = TSchema extends PayloadSchema<infer TInput, any> ? TInput : never
type SchemaOutput<TSchema> = TSchema extends PayloadSchema<any, infer TOutput> ? TOutput : never

export interface Token extends ClientToken {
  wait<TSchema extends PayloadSchema<any, any>>(
    opts: TokenWaitOptions<TSchema> & { readonly schema: TSchema },
  ): WaitHandle<SchemaOutput<TSchema>>
  wait(opts?: TokenWaitOptions): WaitHandle<unknown>
}

export { AuthError, RunNotFoundError, TimeoutError, UnsupportedTransportError } from "./runtime/errors"
export { ConcurrentWaitError, WaitCancelledError, WaitTimeoutError, PayloadSchemaValidationError }
export {
  type LogSnapshot,
  type ListRunEventsOptions,
  type ListRunsOptions,
  type RetrieveRunOptions,
  type RunEvent,
  type RunEventRecord,
  type RunEventRecordPage,
  type RunHandle,
  type RunOutput,
  type RunSnapshot,
  type RunStatus,
  type RunSummary,
  type SubscribeRunEventsOptions,
} from "./runtime/run"
export { defineConfig, idempotencyKeys, queue, sandbox, task, sessions }
export type {
  HelmrConfig,
  HelmrConfigInput,
  Placement,
  SandboxNetwork,
  SecretDecls,
  Task,
  TaskConfig,
  QueueConfig,
  QueueDefinition,
  TaskOutput,
  TaskPayload,
  SessionStartPayload,
  WaitHandle,
  WaitDelayHandle,
  WaitResult,
  StreamAppendOptions,
  StreamListOptions,
  StreamReadOptions,
  StreamRecord,
  StreamWriter,
  OutputStreamDefinition,
  InputStreamDefinition,
  InputStreamPeekOptions,
  InputStreamSendOptions,
  InputStreamWaitOptions,
  InputStreamHandle,
  OutputStreamHandle,
  TokenWaitOptions,
  DurationInput,
  UntilInput,
  WaitJson,
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
export const runs = defaultClientNamespace("runs")

export const workspaces = defaultClientNamespace("workspaces")

export const auth = defaultClientNamespace("auth")

export const streams = Object.freeze({
  input: createInputStream,
  output: createOutputStream,
})

type TokensNamespace = Omit<HelmrClient["tokens"], "create"> & {
  create(opts?: TokenCreateOptions & { readonly timeout?: DurationInput }): Promise<Token>
  wait: typeof tokenWaitHandle
}

const runtimeTokens = {
  async create(opts: TokenCreateOptions & { readonly timeout?: DurationInput } = {}): Promise<Token> {
    const token = runRuntimeIsActive()
      ? await getRunRuntime().createToken(normalizeRuntimeTokenCreateOptions(opts))
      : await getDefaultClient().tokens.create(normalizeTokenCreateOptions(opts))
    return tokenHandle(token)
  },
  wait: tokenWaitHandle,
}

export const tokens = new Proxy({} as TokensNamespace, {
  get(_target, property, receiver) {
    if (Object.prototype.hasOwnProperty.call(runtimeTokens, property)) {
      return Reflect.get(runtimeTokens, property, receiver)
    }
    return Reflect.get(getDefaultClient().tokens, property, receiver)
  },
})

export const timers = Object.freeze({
  waitFor(input: DurationInput): WaitDelayHandle {
    return timerHandle(() => getRunRuntime().waitFor(input))
  },
  waitUntil(input: UntilInput): WaitDelayHandle {
    return timerHandle(() => getRunRuntime().waitUntil(input))
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

type SchedulesNamespace = {
  readonly task: typeof scheduledTask
} & HelmrClient["schedules"]

export const schedules = new Proxy({} as SchedulesNamespace, {
  get(_target, property, receiver) {
    if (property === "task") {
      return scheduledTask
    }
    return Reflect.get(getDefaultClient().schedules, property, receiver)
  },
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
  SessionContext,
  TaskWorkspace,
  RuntimeTokenCreateOptions,
}

function createInputStream<TSchema extends PayloadSchema<any, any>>(
  id: string,
  opts: { readonly schema: TSchema },
): InputStreamHandle<SchemaOutput<TSchema>, SchemaInput<TSchema>> & InputStreamDefinition<SchemaOutput<TSchema>, SchemaInput<TSchema>>
function createInputStream(id: string): InputStreamHandle<unknown, unknown> & InputStreamDefinition<unknown, unknown>
function createInputStream(
  id: string,
  opts?: { readonly schema?: PayloadSchema<any, any> },
): InputStreamHandle<unknown, unknown> & InputStreamDefinition<unknown, unknown> {
  const name = validateStreamName(id)
  registerStreamDefinition({
    id: name,
    direction: "input",
    ...(opts?.schema === undefined ? {} : { schema: opts.schema }),
  })
  return inputStreamHandle(name, opts?.schema)
}

function createOutputStream<TSchema extends PayloadSchema<any, any>>(
  id: string,
  opts: { readonly schema: TSchema },
): OutputStreamHandle<SchemaOutput<TSchema>, SchemaInput<TSchema>> & OutputStreamDefinition<SchemaOutput<TSchema>, SchemaInput<TSchema>>
function createOutputStream(id: string): OutputStreamHandle<unknown, unknown> & OutputStreamDefinition<unknown, unknown>
function createOutputStream(
  id: string,
  opts?: { readonly schema?: PayloadSchema<any, any> },
): OutputStreamHandle<unknown, unknown> & OutputStreamDefinition<unknown, unknown> {
  const name = validateStreamName(id)
  registerStreamDefinition({
    id: name,
    direction: "output",
    ...(opts?.schema === undefined ? {} : { schema: opts.schema }),
  })
  return outputStreamHandle(name, opts?.schema)
}

function inputStreamHandle(
  id: string,
  schema: PayloadSchema<any, any> | undefined,
): InputStreamHandle<unknown, unknown> & InputStreamDefinition<unknown, unknown> {
  const wait = (opts: InputStreamWaitOptions = {}) => {
    return waitHandle(() => getRunRuntime().inputStreamWait(id, schema, opts))
  }
  return Object.freeze({
    id,
    direction: "input",
    ...(schema === undefined ? {} : { schema }),
    wait,
    once: (opts: InputStreamWaitOptions = {}) => {
      return activeWaitHandle(() => getRunRuntime().inputStreamOnce(id, schema, opts))
    },
    on: async (handler: (payload: unknown) => void | Promise<void>, opts: InputStreamWaitOptions = {}) => {
      let afterSequence = opts.afterSequence
      let awaitingInitialRecord = true
      for (;;) {
        const peekOpts = awaitingInitialRecord
          ? { ...opts, afterSequence, block: true }
          : { ...opts, timeout: undefined, afterSequence, block: true }
        const record = await getRunRuntime().inputStreamPeek(id, schema, peekOpts as InputStreamPeekOptions & { readonly block: true })
        if (record === null) {
          if (awaitingInitialRecord && opts.timeout !== undefined) {
            throw new WaitTimeoutError(`input stream ${JSON.stringify(id)} subscription timed out`)
          }
          continue
        }
        awaitingInitialRecord = false
        afterSequence = record.sequence
        await handler(record.data)
      }
    },
    peek: async (opts?: InputStreamPeekOptions) => {
      return await getRunRuntime().inputStreamPeek(id, schema, opts)
    },
  })
}

function outputStreamHandle(
  id: string,
  schema: PayloadSchema<any, any> | undefined,
): OutputStreamHandle<unknown, unknown> & OutputStreamDefinition<unknown, unknown> {
  return Object.freeze({
    id,
    direction: "output",
    ...(schema === undefined ? {} : { schema }),
    append: async (payload: unknown, opts?: StreamAppendOptions) => {
      const parsed = schema === undefined
        ? payload
        : await parsePayloadWithSchema(schema, payload, `stream ${JSON.stringify(id)} payload`)
      return await getRunRuntime().outputStreamAppend(id, parsed, opts)
    },
    pipe: async (source: AsyncIterable<unknown> | Iterable<unknown>, opts?: StreamAppendOptions) => {
      for await (const item of source) {
        const parsed = schema === undefined
          ? item
          : await parsePayloadWithSchema(schema, item, `stream ${JSON.stringify(id)} payload`)
        await getRunRuntime().outputStreamAppend(id, parsed, opts)
      }
    },
    writer: (opts?: StreamAppendOptions): StreamWriter<unknown> => ({
      write: async (payload: unknown) => {
        const parsed = schema === undefined
          ? payload
          : await parsePayloadWithSchema(schema, payload, `stream ${JSON.stringify(id)} payload`)
        await getRunRuntime().outputStreamAppend(id, parsed, opts)
      },
      close: async () => {},
    }),
    read: async (opts?: StreamReadOptions) => {
      return await getRunRuntime().outputStreamRead(id, schema, opts)
    },
    list: async (opts?: StreamListOptions) => {
      return await getRunRuntime().outputStreamList(id, schema, opts)
    },
  })
}

function tokenHandle(token: ClientToken): Token {
  return Object.freeze({
    ...token,
    wait: (opts?: TokenWaitOptions) => tokenWaitHandle(token, opts),
  })
}

function tokenWaitHandle<TSchema extends PayloadSchema<any, any>>(
  token: Token | TokenRef | string,
  opts: TokenWaitOptions<TSchema> & { readonly schema: TSchema },
): WaitHandle<SchemaOutput<TSchema>>
function tokenWaitHandle(token: Token | TokenRef | string, opts?: TokenWaitOptions): WaitHandle<unknown>
function tokenWaitHandle(token: Token | TokenRef | string, opts: TokenWaitOptions = {}): WaitHandle<unknown> {
  const tokenId = typeof token === "string" ? token : token.id
  const tokenTimeoutAt = typeof token === "string" ? undefined : "timeoutAt" in token ? token.timeoutAt : undefined
  const timeout = opts.timeout ?? tokenTimeoutInput(tokenTimeoutAt)
  const schema = opts.schema
  const options: RuntimeTokenWaitOptions = {
    tokenId,
    ...(timeout === undefined ? {} : { timeout }),
    ...(opts.idleTimeout === undefined ? {} : { idleTimeout: opts.idleTimeout }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
    ...(opts.tags === undefined ? {} : { tags: opts.tags }),
    ...(schema === undefined ? {} : { schema }),
  }
  return waitHandle(() => getRunRuntime().waitToken(options))
}

function waitHandle<TPayload>(
  factory: () => Promise<WaitResult<TPayload>>,
): WaitHandle<TPayload> {
  let promise: Promise<WaitResult<TPayload>> | undefined
  const getPromise = () => {
    promise ??= factory()
    return promise
  }
  return {
    then<TResult1 = WaitResult<TPayload>, TResult2 = never>(
      onfulfilled?: ((value: WaitResult<TPayload>) => TResult1 | PromiseLike<TResult1>) | null,
      onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
    ): PromiseLike<TResult1 | TResult2> {
      return getPromise().then(onfulfilled, onrejected)
    },
    unwrap: async () => (await getPromise()).unwrap(),
  }
}

function activeWaitHandle<TPayload>(
  factory: () => Promise<WaitResult<TPayload>>,
): WaitHandle<TPayload> {
  let promise: Promise<WaitResult<TPayload>> | undefined
  const getPromise = () => {
    promise ??= factory()
    return promise
  }
  return {
    then<TResult1 = WaitResult<TPayload>, TResult2 = never>(
      onfulfilled?: ((value: WaitResult<TPayload>) => TResult1 | PromiseLike<TResult1>) | null,
      onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
    ): PromiseLike<TResult1 | TResult2> {
      return getPromise().then(onfulfilled, onrejected)
    },
    unwrap: async () => (await getPromise()).unwrap(),
  }
}

function timerHandle(factory: () => Promise<void>): WaitDelayHandle {
  let promise: Promise<void> | undefined
  const getPromise = () => {
    promise ??= factory()
    return promise
  }
  return {
    then<TResult1 = void, TResult2 = never>(
      onfulfilled?: ((value: void) => TResult1 | PromiseLike<TResult1>) | null,
      onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
    ): PromiseLike<TResult1 | TResult2> {
      return getPromise().then(onfulfilled, onrejected)
    },
    unwrap: () => getPromise(),
  }
}

function runRuntimeIsActive(): boolean {
  try {
    getRunRuntime()
    return true
  } catch {
    return false
  }
}

function normalizeTokenCreateOptions(
  opts: TokenCreateOptions & { readonly timeout?: DurationInput },
): TokenCreateOptions {
  return opts
}

function normalizeRuntimeTokenCreateOptions(
  opts: TokenCreateOptions & { readonly timeout?: DurationInput },
): RuntimeTokenCreateOptions {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    throw new Error("tokens.create cannot override projectId or environmentId inside a running task")
  }
  if (opts.signal !== undefined) {
    throw new Error("tokens.create signal is not supported inside a running task")
  }
  return normalizeTokenCreateOptions(opts)
}

function tokenTimeoutInput(timeoutAt: string | Date | null | undefined): DurationInput | undefined {
  if (timeoutAt === null || timeoutAt === undefined) {
    return undefined
  }
  const at = timeoutAt instanceof Date ? timeoutAt : new Date(timeoutAt)
  const milliseconds = at.getTime() - Date.now()
  if (!Number.isFinite(milliseconds)) {
    throw new Error("token timeoutAt must be a valid date")
  }
  return { milliseconds: Math.max(1, milliseconds) }
}
