import {
  parseTaskPayload,
  validateChannelName,
  validateRetryPolicy,
  type AnyTask,
  type NoPayload,
  type SecretDecls,
  type TaskOutput,
  type TaskRunOptions,
  type TaskSecrets,
  type TaskStartPayload,
} from "../internal"
import { taskStartIdempotencyRequestFields } from "../idempotency"
import { readOptionalMaxDurationSeconds } from "../schema/task"
import { AuthError, TimeoutError, UnsupportedTransportError } from "./errors"
import { HELMR_API_VERSION, HELMR_API_VERSION_HEADER, HELMR_SDK_VERSION, HELMR_SDK_VERSION_HEADER } from "../version"
import {
  type LogSnapshot,
  type CancelRunOptions,
  type ListRunEventsOptions,
  type ListRunsOptions,
  type PendingWaitpoint,
  type PendingWaitpointResponse,
  type RetrieveRunOptions,
  type RunHandle,
  type RunEvent,
  type RunEventRecord,
  type RunEventRecordPage,
  type RunSnapshot,
  type RunSummary,
  type RunWaitpointOptions,
  type SubscribeRunEventsOptions,
  isTerminalRunStatus,
  pendingWaitpointFromResponse,
  runHandle,
  runId,
  runSnapshot,
} from "./run"

const MAX_SSE_BUFFER_CHARS = 1024 * 1024
const RUN_EVENT_RECONNECT_DELAY_MS = 1000
const RUN_TERMINAL_SNAPSHOT_RETRY_DELAY_MS = 100
const TASK_START_PENDING_MAX_WAIT_MS = 10_000
const TASK_START_PENDING_DEFAULT_RETRY_MS = 250

export interface HelmrClientOptions {
  readonly url?: string
  readonly apiKey?: string
}

export type TaskStartOptions<TSecrets extends SecretDecls> = TaskRunOptions<TSecrets> & {
  readonly projectId?: string
  readonly environmentId?: string
  readonly externalId?: string
  readonly workspaceId?: string
  readonly expiresAt?: string | Date
}
export type TaskStartAndWaitOptions<TSecrets extends SecretDecls> = TaskStartOptions<TSecrets> & {
  readonly timeoutSeconds?: number
}

export type DirectTaskStartArgs<TTask extends AnyTask> =
  [TaskStartPayload<TTask>] extends [NoPayload]
    ? [opts: TaskStartOptions<TaskSecrets<TTask>>]
    : [payload: TaskStartPayload<TTask>, opts: TaskStartOptions<TaskSecrets<TTask>>]

export type TasksStartArgs<TTask extends AnyTask> =
  [TaskStartPayload<TTask>] extends [NoPayload]
    ? [id: string, opts: TaskStartOptions<TaskSecrets<TTask>>]
    : [id: string, payload: TaskStartPayload<TTask>, opts: TaskStartOptions<TaskSecrets<TTask>>]

export type TasksStartAndWaitArgs<TTask extends AnyTask> =
  [TaskStartPayload<TTask>] extends [NoPayload]
    ? [id: string, opts: TaskStartAndWaitOptions<TaskSecrets<TTask>>]
    : [id: string, payload: TaskStartPayload<TTask>, opts: TaskStartAndWaitOptions<TaskSecrets<TTask>>]

export const startTaskClientMethod = Symbol.for("helmr.sdk.client.startTask")
export const waitpointTokenClientMethod = Symbol.for("helmr.sdk.client.waitpointToken")

export interface RunWaitpointsApi {
  readonly listPage: <TOutput = unknown>(idOrHandle: string | RunHandle<TOutput>, opts?: RunWaitpointListOptions) => Promise<RunWaitpointsPage>
  readonly list: <TOutput = unknown>(idOrHandle: string | RunHandle<TOutput>, opts?: RunWaitpointListOptions) => Promise<PendingWaitpoint[]>
}

export type TaskSessionStatus = "open" | "completed" | "failed" | "closed" | "cancelled" | "expired"

export interface TaskSessionHandle<TOutput = unknown> {
  readonly id: string
  readonly taskId: string
  readonly currentRunId: string | null
}

export interface TaskStartResult<TOutput = unknown> {
  readonly session: TaskSessionSnapshot<TOutput>
  readonly run: RunHandle<TOutput>
  readonly isCached: boolean
}

export interface TaskSessionSnapshot<TOutput = unknown> {
  readonly id: string
  readonly projectId: string
  readonly environmentId: string
  readonly taskId: string
  readonly initialDeploymentId: string
  readonly activeDeploymentId: string
  readonly externalId?: string
  readonly status: TaskSessionStatus
  readonly currentRunId: string | null
  readonly workspaceId: string | null
  readonly metadata: Record<string, unknown>
  readonly tags: readonly string[]
  readonly result?: TOutput
  readonly error?: unknown
  readonly timedOut: boolean
  readonly terminalReason?: unknown
  readonly expiresAt: string | null
  readonly createdAt: string
  readonly updatedAt: string
}

export interface TaskSessionListOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly status?: TaskSessionStatus | "all"
  readonly taskId?: string
  readonly limit?: number
  readonly signal?: AbortSignal
}

export interface TaskSessionWaitOptions {
  readonly timeoutSeconds?: number
  readonly signal?: AbortSignal
}

export interface TaskSessionCloseOptions {
  readonly reason?: string
  readonly signal?: AbortSignal
}

export interface TaskSessionCancelOptions {
  readonly reason?: string
  readonly signal?: AbortSignal
}

export interface TaskSessionRun {
  readonly id: string
  readonly runId: string
  readonly deploymentId: string
  readonly previousRunId?: string
  readonly turnIndex: number
  readonly status: string
  readonly executionStatus: string
  readonly terminalOutcome?: string
  readonly createdAt: string
  readonly endedAt?: string
}

export interface ChannelRecord<TData = unknown> {
  readonly id: string
  readonly channelId: string
  readonly sequence: number
  readonly data: TData
  readonly correlationId?: string
  readonly contentType: string
  readonly createdAt: string
}

export interface SessionChannelInputSendResult<TData = unknown> extends ChannelRecord<TData> {
  readonly idempotencyStatus: "created" | "duplicate"
}

export interface SessionChannelInputSendOptions {
  readonly correlationId?: string
  readonly externalEventId?: string
  readonly signal?: AbortSignal
}

export interface SessionChannelListOptions {
  readonly cursor?: number
  readonly limit?: number
  readonly correlationId?: string
  readonly signal?: AbortSignal
}

export interface SessionChannelOutputStreamOptions {
  readonly cursor?: number
  readonly correlationId?: string
  readonly signal?: AbortSignal
}

export type PublicAccessTokenScope =
  | {
      readonly type: "session.input.append"
      readonly sessionId: string | TaskSessionHandle
      readonly channel: string
      readonly correlationId?: string
    }
  | {
      readonly type: "session.output.read"
      readonly sessionId: string | TaskSessionHandle
      readonly channel: string
      readonly correlationId?: string
    }

export interface PublicAccessTokenCreateOptions {
  readonly scope: PublicAccessTokenScope
  readonly expiresAt?: string | Date
  readonly maxUses?: number
  readonly signal?: AbortSignal
}

export interface PublicAccessToken {
  readonly id: string
  readonly publicAccessToken: string
  readonly scope: PublicAccessTokenScope
  readonly expiresAt: string
  readonly maxUses?: number
  readonly createdAt: string
}

export interface SessionChannelInputApi {
  send<TData = unknown>(data: TData, opts?: SessionChannelInputSendOptions): Promise<SessionChannelInputSendResult<TData>>
  list<TData = unknown>(opts?: SessionChannelListOptions): Promise<ChannelRecord<TData>[]>
}

export interface SessionChannelOutputApi {
  list<TData = unknown>(opts?: SessionChannelListOptions): Promise<ChannelRecord<TData>[]>
  stream<TData = unknown>(opts?: SessionChannelOutputStreamOptions): Promise<AsyncIterable<ChannelRecord<TData>>>
}

export interface OpenTaskSessionApi<TOutput = unknown> {
  readonly id: string
  retrieve(opts?: { readonly signal?: AbortSignal }): Promise<TaskSessionSnapshot<TOutput>>
  wait(opts?: TaskSessionWaitOptions): Promise<TaskSessionSnapshot<TOutput>>
  close(opts?: TaskSessionCloseOptions): Promise<TaskSessionSnapshot<TOutput>>
  cancel(opts?: TaskSessionCancelOptions): Promise<TaskSessionSnapshot<TOutput>>
  runs(opts?: { readonly signal?: AbortSignal }): Promise<TaskSessionRun[]>
  input(channel: string): SessionChannelInputApi
  output(channel: string): SessionChannelOutputApi
}

export type WorkspaceState = "active" | "deleting" | "recovery_required" | "archived" | "deleted"
export type WorkspaceDesiredState = "active" | "stopped" | "archived" | "deleted"
export type WorkspaceDirtyState = "clean" | "dirty" | "capturing" | "capture_failed" | "dirty_state_lost"

export interface Workspace {
  readonly id: string
  readonly projectId: string
  readonly environmentId: string
  readonly deploymentSandboxId: string
  readonly sandboxId: string
  readonly sandboxFingerprint: string
  readonly externalId?: string
  readonly currentVersionId: string | null
  readonly state: WorkspaceState
  readonly desiredState: WorkspaceDesiredState
  readonly dirtyState: WorkspaceDirtyState
  readonly lastMaterializationId: string | null
  readonly metadata: Record<string, unknown>
  readonly tags: readonly string[]
  readonly autoStopAt: string | null
  readonly autoArchiveAt: string | null
  readonly autoDeleteAt: string | null
  readonly lastActivityAt: string
  readonly createdAt: string
  readonly updatedAt: string
  readonly archivedAt: string | null
  readonly deletedAt: string | null
}

export interface WorkspaceHandle {
  readonly id: string
  exec(command: readonly string[], opts?: WorkspaceExecCreateOptions): Promise<WorkspaceExecHandle>
  readonly execs: WorkspaceExecsApi
  readonly pty: WorkspacePtyApi
  retrieve(opts?: WorkspaceRetrieveOptions): Promise<Workspace>
  update(opts: WorkspaceUpdateOptions): Promise<Workspace>
  delete(opts?: WorkspaceRetrieveOptions): Promise<Workspace>
  materialize(opts?: WorkspaceMaterializeOptions): Promise<WorkspaceMaterialization>
  connect(opts?: WorkspaceMaterializeOptions): Promise<WorkspaceMaterialization>
}

export interface WorkspacesApi {
  readonly create: (opts: WorkspaceCreateOptions) => Promise<Workspace>
  readonly open: (id: string) => WorkspaceHandle
  readonly retrieve: (idOrHandle: string | WorkspaceHandle, opts?: WorkspaceRetrieveOptions) => Promise<Workspace>
  readonly list: (opts?: WorkspaceListOptions) => Promise<Workspace[]>
  readonly update: (idOrHandle: string | WorkspaceHandle, opts: WorkspaceUpdateOptions) => Promise<Workspace>
  readonly delete: (idOrHandle: string | WorkspaceHandle, opts?: WorkspaceRetrieveOptions) => Promise<Workspace>
  readonly materialize: (idOrHandle: string | WorkspaceHandle, opts?: WorkspaceMaterializeOptions) => Promise<WorkspaceMaterialization>
  readonly connect: (idOrHandle: string | WorkspaceHandle, opts?: WorkspaceMaterializeOptions) => Promise<WorkspaceMaterialization>
}

export interface WorkspaceCreateOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly sandboxId: string
  readonly deploymentId?: string
  readonly externalId?: string
  readonly metadata?: Record<string, unknown>
  readonly tags?: readonly string[]
  readonly idempotencyKey?: string
  readonly idempotencyKeyTTL?: string
  readonly signal?: AbortSignal
}

export interface WorkspaceRetrieveOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly signal?: AbortSignal
}

export interface WorkspaceListOptions extends WorkspaceRetrieveOptions {
  readonly state?: WorkspaceState
  readonly externalId?: string
  readonly tag?: string
  readonly limit?: number
}

export interface WorkspaceUpdateOptions extends WorkspaceRetrieveOptions {
  readonly metadata?: Record<string, unknown>
  readonly tags?: readonly string[]
}

export interface WorkspaceMaterializeOptions extends WorkspaceRetrieveOptions {}

export interface WorkspaceMaterialization {
  readonly id: string
  readonly projectId: string
  readonly environmentId: string
  readonly workspaceId: string
  readonly deploymentSandboxId: string
  readonly baseVersionId: string | null
  readonly workerInstanceId: string | null
  readonly state: string
  readonly fencingGeneration: number
  readonly dirtyGeneration: number
  readonly reservationExpiresAt: string | null
  readonly lastHeartbeatAt: string | null
  readonly createdAt: string
  readonly updatedAt: string
}

export type WorkspaceExecState =
  | "queued"
  | "materializing"
  | "running"
  | "exited"
  | "lost"
  | "failed"

export interface WorkspaceExec {
  readonly id: string
  readonly workspaceId: string
  readonly materializationId: string | null
  readonly command: readonly string[]
  readonly cwd: string
  readonly envShape: Record<string, string>
  readonly filesystemMode: "read" | "write" | string
  readonly state: WorkspaceExecState
  readonly detached: boolean
  readonly processId: string | null
  readonly exitCode: number | null
  readonly signal: string | null
  readonly error: unknown
  readonly stdoutCursor: number
  readonly stderrCursor: number
  readonly stdinCursor: number
  readonly stdinClosedAt: string | null
  readonly createdAt: string
  readonly startedAt: string | null
  readonly exitedAt: string | null
  readonly updatedAt: string
}

export interface WorkspaceExecHandle {
  readonly id: string
  readonly workspaceId: string
  retrieve(opts?: WorkspaceRetrieveOptions): Promise<WorkspaceExec>
  readonly stdout: WorkspaceExecReadableStreamApi
  readonly stderr: WorkspaceExecReadableStreamApi
  readonly stdin: WorkspaceExecStdinApi
  wait(opts?: WorkspaceExecWaitOptions): Promise<WorkspaceExec>
}

export interface WorkspaceExecsApi {
  retrieve(id: string, opts?: WorkspaceRetrieveOptions): WorkspaceExecHandle
  list(opts?: WorkspaceExecListOptions): Promise<WorkspaceExec[]>
}

export interface WorkspaceExecCreateOptions extends WorkspaceRetrieveOptions {
  readonly cwd?: string
  readonly env?: Record<string, string>
  readonly detached?: boolean
  readonly idempotencyKey?: string
}

export interface WorkspaceExecListOptions extends WorkspaceRetrieveOptions {
  readonly state?: WorkspaceExecState
  readonly limit?: number
}

export interface WorkspaceExecWaitOptions extends WorkspaceRetrieveOptions {
  readonly pollIntervalMs?: number
}

export interface WorkspaceExecReadableStreamApi {
  list(opts?: WorkspaceStreamListOptions): Promise<WorkspaceStreamChunk[]>
  stream(opts?: WorkspaceStreamFollowOptions): AsyncIterable<WorkspaceStreamChunk>
}

export interface WorkspaceExecStdinApi {
  write(data: string | Uint8Array, opts: WorkspaceStreamWriteOptions): Promise<WorkspaceStreamChunk>
  close(opts?: WorkspaceRetrieveOptions): Promise<WorkspaceExec>
}

export type WorkspacePtyState = "creating" | "open" | "resizing" | "closing" | "closed" | "lost" | "failed"

export interface WorkspacePty {
  readonly id: string
  readonly workspaceId: string
  readonly materializationId: string | null
  readonly cwd: string
  readonly cols: number
  readonly rows: number
  readonly filesystemMode: "read" | "write" | string
  readonly state: WorkspacePtyState
  readonly processId: string | null
  readonly outputCursor: number
  readonly inputCursor: number
  readonly error: unknown
  readonly createdAt: string
  readonly startedAt: string | null
  readonly closedAt: string | null
  readonly updatedAt: string
}

export interface WorkspacePtyHandle {
  readonly id: string
  readonly workspaceId: string
  retrieve(opts?: WorkspaceRetrieveOptions): Promise<WorkspacePty>
  readonly output: WorkspacePtyOutputApi
  input(data: string | Uint8Array, opts: WorkspaceStreamWriteOptions): Promise<WorkspaceStreamChunk>
  resize(cols: number, rows: number, opts?: WorkspaceRetrieveOptions): Promise<WorkspacePty>
  close(opts?: WorkspaceRetrieveOptions): Promise<WorkspacePty>
}

export interface WorkspacePtyApi {
  create(opts?: WorkspacePtyCreateOptions): Promise<WorkspacePtyHandle>
  retrieve(id: string, opts?: WorkspaceRetrieveOptions): WorkspacePtyHandle
  list(opts?: WorkspacePtyListOptions): Promise<WorkspacePty[]>
}

export interface WorkspacePtyCreateOptions extends WorkspaceRetrieveOptions {
  readonly cwd?: string
  readonly cols?: number
  readonly rows?: number
  readonly idempotencyKey?: string
}

export interface WorkspacePtyListOptions extends WorkspaceRetrieveOptions {
  readonly state?: WorkspacePtyState
  readonly limit?: number
}

export interface WorkspacePtyOutputApi {
  list(opts?: WorkspaceStreamListOptions): Promise<WorkspaceStreamChunk[]>
  stream(opts?: WorkspaceStreamFollowOptions): AsyncIterable<WorkspaceStreamChunk>
}

export interface WorkspaceStreamChunk {
  readonly id: string
  readonly stream: string
  readonly offsetStart: number
  readonly offsetEnd: number
  readonly data: Uint8Array
  readonly observedAt: string
  readonly createdAt: string
}

export interface WorkspaceStreamListOptions extends WorkspaceRetrieveOptions {
  readonly cursor?: number
  readonly limit?: number
}

export interface WorkspaceStreamFollowOptions extends WorkspaceRetrieveOptions {
  readonly fromCursor?: number
  readonly limit?: number
}

export interface WorkspaceStreamWriteOptions extends WorkspaceRetrieveOptions {
  readonly offset: number
}

export interface SchedulesApi {
  readonly create: (opts: ScheduleCreateOptions) => Promise<Schedule>
  readonly update: (id: string, opts: ScheduleUpdateOptions & RetrieveScheduleOptions) => Promise<Schedule>
  readonly list: (opts?: ListSchedulesOptions) => Promise<Schedule[]>
  readonly retrieve: (id: string, opts?: RetrieveScheduleOptions) => Promise<Schedule>
  readonly activate: (id: string, opts?: RetrieveScheduleOptions) => Promise<Schedule>
  readonly deactivate: (id: string, opts?: RetrieveScheduleOptions) => Promise<Schedule>
  readonly delete: (id: string, opts?: RetrieveScheduleOptions) => Promise<void>
}

export interface ScheduleCreateOptions {
  readonly deduplicationKey: string
  readonly externalId?: string
  readonly task: string
  readonly cron: string
  readonly timezone?: string
  readonly active?: boolean
  readonly options?: ScheduleRunOptions
}

export type ScheduleUpdateOptions = Omit<ScheduleCreateOptions, "deduplicationKey"> & {
  readonly externalId?: string
}

export interface ScheduleRunOptions {
  readonly queue?: string
  readonly concurrencyKey?: string
  readonly priority?: number
  readonly ttl?: string
  readonly maxDurationSeconds?: number
}

export interface ListSchedulesOptions {
  readonly signal?: AbortSignal
}

export interface RetrieveScheduleOptions {
  readonly signal?: AbortSignal
}

export interface Schedule {
  readonly id: string
  readonly type: "imperative" | "declarative"
  readonly projectId: string
  readonly environmentId: string
  readonly task: string
  readonly deduplicationKey?: string
  readonly externalId?: string
  readonly cron: string
  readonly timezone: string
  readonly active: boolean
  readonly status: "active" | "inactive" | "errored"
  readonly lastError?: string
  readonly nextFireAt?: string
  readonly lastFireAt?: string
  readonly createdAt: string
  readonly updatedAt: string
}

type WaitpointTokenExpirationOptions =
  | {
      readonly timeoutInSeconds?: number
      readonly timeoutAt?: never
    }
  | {
      readonly timeoutInSeconds?: never
      readonly timeoutAt?: string
    }

export type WaitpointTokenCreateOptions = WaitpointTokenExpirationOptions & {
  readonly projectId?: string
  readonly environmentId?: string
  readonly tags?: readonly string[]
  readonly metadata?: Record<string, unknown>
  readonly signal?: AbortSignal
}

export interface WaitpointToken {
  readonly id: string
  readonly status?: "waiting" | "completed" | "timed_out" | "cancelled"
  readonly callbackUrl: string
  readonly publicAccessToken?: string
  readonly timeoutAt: string | null
  readonly data?: unknown
  readonly tags?: readonly string[]
  readonly metadata?: Record<string, unknown>
}

export interface WaitpointTokenRef {
  readonly id: string
}

export interface WaitpointTokenCompleteOptions {
  readonly publicAccessToken?: string
  readonly signal?: AbortSignal
}

export interface WaitpointTokenRetrieveOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly signal?: AbortSignal
}

export interface WaitpointTokenListOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly status?: "waiting" | "completed" | "timed_out" | "cancelled"
  readonly cursor?: string
  readonly limit?: number
  readonly signal?: AbortSignal
}

export interface WaitpointTokensPage {
  readonly tokens: readonly WaitpointToken[]
  readonly nextCursor: string | null
}

type WaitpointTokenCreateRequest = {
  readonly operation: "create"
  readonly opts?: WaitpointTokenCreateOptions
}

type WaitpointTokenRetrieveRequest = {
  readonly operation: "retrieve"
  readonly id: string
  readonly opts?: WaitpointTokenRetrieveOptions
}

type WaitpointTokenListPageRequest = {
  readonly operation: "listPage"
  readonly opts?: WaitpointTokenListOptions
}

type WaitpointTokenCompleteRequest = {
  readonly operation: "complete"
  readonly token: WaitpointToken | WaitpointTokenRef | string
  readonly data: unknown
  readonly opts?: WaitpointTokenCompleteOptions
}

type WaitpointTokenClientRequest =
  | WaitpointTokenCreateRequest
  | WaitpointTokenRetrieveRequest
  | WaitpointTokenListPageRequest
  | WaitpointTokenCompleteRequest

export interface RunWaitpointsPage {
  readonly waitpoints: readonly PendingWaitpoint[]
  readonly nextCursor: string | null
}

export type WaitpointStatus = "pending" | "completed" | "timed_out" | "cancelled" | "failed"

export interface RunWaitpointListOptions {
  readonly cursor?: string
  readonly limit?: number
  readonly signal?: AbortSignal
  readonly status?: WaitpointStatus
}

export class HelmrClient {
  readonly #baseUrl: URL
  readonly #apiKey: string | undefined

  constructor(options: HelmrClientOptions = {}) {
    const rawUrl = options.url ?? process.env["HELMR_API_URL"]
    if (rawUrl === undefined || rawUrl.trim() === "") {
      throw new UnsupportedTransportError(
        "HelmrClient requires a url option or HELMR_API_URL; no default transport is used",
      )
    }

    const envApiKey = process.env["HELMR_API_KEY"]
    const apiKey = options.apiKey ?? envApiKey
    let parsedUrl: URL
    try {
      parsedUrl = new URL(rawUrl)
    } catch {
      throw new UnsupportedTransportError("HelmrClient requires an http(s) URL")
    }
    if (parsedUrl.protocol === "https:") {
      this.#baseUrl = normalizedBaseUrl(parsedUrl)
      this.#apiKey = apiKey
    } else if (parsedUrl.protocol === "http:") {
      if (!isLoopbackHost(parsedUrl.hostname)) {
        throw new UnsupportedTransportError(
          `refusing to send credentials over plaintext non-loopback URL ${parsedUrl.toString()}`,
        )
      }
      console.warn(
        "HelmrClient http:// transport is plaintext and must be explicitly opted into; use https:// for remote services",
      )
      this.#baseUrl = normalizedBaseUrl(parsedUrl)
      this.#apiKey = apiKey
    } else {
      throw new UnsupportedTransportError(
        `unsupported HelmrClient transport scheme ${parsedUrl.protocol.replace(/:$/, "")}`,
      )
    }
  }

  readonly tasks = {
    start: async <TTask extends AnyTask>(
      ...args: TasksStartArgs<TTask>
    ): Promise<TaskStartResult<TaskOutput<TTask>>> => {
      const taskId = args[0]
      const hasPayload = args.length === 3
      const payload = hasPayload ? args[1] : undefined
      const opts = (hasPayload ? args[2] : args[1]) as TaskStartOptions<TaskSecrets<TTask>>
      if (hasPayload && payload === undefined) {
        throw new Error(`task ${JSON.stringify(taskId)} requires payload`)
      }
      return await this.#startTask(taskId, payload, opts)
    },
    startAndWait: async <TTask extends AnyTask>(
      ...args: TasksStartAndWaitArgs<TTask>
    ): Promise<TaskSessionSnapshot<TaskOutput<TTask>>> => {
      const taskId = args[0]
      const hasPayload = args.length === 3
      const payload = hasPayload ? args[1] : undefined
      const opts = (hasPayload ? args[2] : args[1]) as TaskStartAndWaitOptions<TaskSecrets<TTask>>
      if (hasPayload && payload === undefined) {
        throw new Error(`task ${JSON.stringify(taskId)} requires payload`)
      }
      return await this.#startTaskAndWait(taskId, payload, opts)
    },
  }

  readonly sessions = {
    open: <TOutput = unknown>(idOrHandle: string | TaskSessionHandle<TOutput>): OpenTaskSessionApi<TOutput> => {
      return this.#openSession<TOutput>(sessionId(idOrHandle))
    },
    retrieve: async <TOutput = unknown>(
      idOrHandle: string | TaskSessionHandle<TOutput>,
      opts: { readonly signal?: AbortSignal } = {},
    ): Promise<TaskSessionSnapshot<TOutput>> => {
      return await this.#openSession<TOutput>(sessionId(idOrHandle)).retrieve(opts)
    },
    wait: async <TOutput = unknown>(
      idOrHandle: string | TaskSessionHandle<TOutput>,
      opts: TaskSessionWaitOptions = {},
    ): Promise<TaskSessionSnapshot<TOutput>> => {
      return await this.#openSession<TOutput>(sessionId(idOrHandle)).wait(opts)
    },
    list: async (opts: TaskSessionListOptions = {}): Promise<TaskSessionSnapshot[]> => {
      const response = await this.#json<ListTaskSessionsResponse>(
        `/api/sessions${taskSessionListQuery(opts)}`,
        requestSignal(opts.signal),
      )
      return response.sessions.map(taskSessionFromResponse)
    },
  }

  readonly workspaces: WorkspacesApi = {
    create: async (opts: WorkspaceCreateOptions): Promise<Workspace> => {
      const response = await this.#json<WorkspaceEnvelopeResponse>(workspaceCollectionPath(opts), {
        method: "POST",
        body: JSON.stringify(workspaceCreateBody(opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal),
      })
      return workspaceFromResponse(response.workspace)
    },
    open: (id: string): WorkspaceHandle => {
      return this.#openWorkspace(id)
    },
    retrieve: async (idOrHandle: string | WorkspaceHandle, opts: WorkspaceRetrieveOptions = {}): Promise<Workspace> => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).retrieve(opts)
    },
    list: async (opts: WorkspaceListOptions = {}): Promise<Workspace[]> => {
      const response = await this.#json<ListWorkspacesResponse>(
        `${workspaceCollectionPath(opts)}${workspaceListQuery(opts)}`,
        requestSignal(opts.signal),
      )
      return response.workspaces.map(workspaceFromResponse)
    },
    update: async (idOrHandle: string | WorkspaceHandle, opts: WorkspaceUpdateOptions): Promise<Workspace> => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).update(opts)
    },
    delete: async (idOrHandle: string | WorkspaceHandle, opts: WorkspaceRetrieveOptions = {}): Promise<Workspace> => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).delete(opts)
    },
    materialize: async (idOrHandle: string | WorkspaceHandle, opts: WorkspaceMaterializeOptions = {}): Promise<WorkspaceMaterialization> => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).materialize(opts)
    },
    connect: async (idOrHandle: string | WorkspaceHandle, opts: WorkspaceMaterializeOptions = {}): Promise<WorkspaceMaterialization> => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).connect(opts)
    },
  }

  readonly waitpoints = {
    tokens: {
      create: async (opts: WaitpointTokenCreateOptions = {}): Promise<WaitpointToken> => {
        return await this[waitpointTokenClientMethod]({ operation: "create", opts })
      },
      retrieve: async (id: string, opts: WaitpointTokenRetrieveOptions = {}): Promise<WaitpointToken> => {
        return await this[waitpointTokenClientMethod]({ operation: "retrieve", id, opts })
      },
      listPage: async (opts: WaitpointTokenListOptions = {}): Promise<WaitpointTokensPage> => {
        return await this[waitpointTokenClientMethod]({ operation: "listPage", opts })
      },
      list: async (opts: WaitpointTokenListOptions = {}): Promise<WaitpointToken[]> => {
        return [...(await this[waitpointTokenClientMethod]({ operation: "listPage", opts })).tokens]
      },
      complete: async (
        token: WaitpointToken | WaitpointTokenRef | string,
        data: unknown,
        opts: WaitpointTokenCompleteOptions = {},
      ): Promise<void> => {
        await this[waitpointTokenClientMethod]({ operation: "complete", token, data, opts })
      },
    },
  }

  readonly auth = {
    createPublicToken: async (opts: PublicAccessTokenCreateOptions): Promise<PublicAccessToken> => {
      const response = await this.#json<PublicAccessTokenResponse>("/api/public-access-tokens", {
        method: "POST",
        body: JSON.stringify(publicAccessTokenCreateBody(opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal),
      })
      return publicAccessTokenFromResponse(response)
    },
  }

  async [startTaskClientMethod]<TTask extends AnyTask>(
    task: TTask,
    ...args: DirectTaskStartArgs<TTask>
  ): Promise<TaskStartResult<TaskOutput<TTask>>> {
    const hasPayload = task.payload !== undefined
    const payload = hasPayload ? args[0] : undefined
    const opts = (hasPayload ? args[1] : args[0]) as TaskStartOptions<TaskSecrets<TTask>>
    if (task.payload !== undefined) {
      if (payload === undefined) {
        throw new Error(`task ${JSON.stringify(task.id)} requires payload`)
      }
      await parseTaskPayload(task, payload)
    } else if (args.length > 1) {
      throw new Error(`task ${JSON.stringify(task.id)} does not accept payload`)
    }
    return await this.#startTask(task.id, payload, opts, readOptionalMaxDurationSeconds(task.maxDuration))
  }

  async [waitpointTokenClientMethod](request: WaitpointTokenCreateRequest): Promise<WaitpointToken>
  async [waitpointTokenClientMethod](request: WaitpointTokenRetrieveRequest): Promise<WaitpointToken>
  async [waitpointTokenClientMethod](request: WaitpointTokenListPageRequest): Promise<WaitpointTokensPage>
  async [waitpointTokenClientMethod](request: WaitpointTokenCompleteRequest): Promise<void>
  async [waitpointTokenClientMethod](
    request: WaitpointTokenClientRequest,
  ): Promise<WaitpointToken | WaitpointTokensPage | void> {
    switch (request.operation) {
      case "create": {
        const opts = request.opts ?? {}
        const response = await this.#json<WaitpointTokenResponse>(
          waitpointTokenCollectionPath(opts),
          {
            method: "POST",
            body: JSON.stringify(waitpointTokenCreateBody(opts)),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal),
          },
        )
        return waitpointTokenFromResponse(response)
      }
      case "retrieve": {
        const opts = request.opts ?? {}
        const response = await this.#json<WaitpointTokenResponse>(
          `${waitpointTokenCollectionPath(opts)}/${encodeURIComponent(request.id)}`,
          requestSignal(opts.signal),
        )
        return waitpointTokenFromResponse(response)
      }
      case "listPage": {
        const opts = request.opts ?? {}
        const response = await this.#json<ListWaitpointTokensResponse>(
          `${waitpointTokenCollectionPath(opts)}${waitpointTokenListQuery(opts)}`,
          requestSignal(opts.signal),
        )
        return {
          tokens: response.tokens.map(waitpointTokenFromResponse),
          nextCursor: response.next_cursor ?? null,
        }
      }
      case "complete": {
        const opts = request.opts ?? {}
        const id = typeof request.token === "string" ? request.token : request.token.id
        const publicAccessToken = opts.publicAccessToken ?? waitpointTokenPublicAccessToken(request.token)
        await this.#fetch(`/api/waitpoints/tokens/${encodeURIComponent(id)}/complete`, {
          method: "POST",
          body: JSON.stringify(waitpointTokenCompleteBody(request.data, opts)),
          headers: {
            "content-type": "application/json",
            ...(publicAccessToken === undefined ? {} : { authorization: `Bearer ${publicAccessToken}` }),
          },
          ...requestSignal(opts.signal),
        })
        return
      }
    }
  }

  async #startTask<TTask extends AnyTask>(
    taskId: string,
    payload: unknown,
    opts: TaskStartOptions<TaskSecrets<TTask>>,
    maxDurationSeconds?: number,
  ): Promise<TaskStartResult<TaskOutput<TTask>>> {
    validateRetryPolicy(opts.retry, "retry")
    const body = taskStartBody(payload, opts, maxDurationSeconds)
    const startedAt = Date.now()
    for (;;) {
      const response = await this.#fetch(`/api/tasks/${encodeURIComponent(taskId)}/start`, {
        method: "POST",
        body: JSON.stringify(body),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal),
      })
      if (response.status !== 202) {
        const start = (await response.json()) as TaskStartResponse
        return taskStartFromResponse<TaskOutput<TTask>>(start)
      }
      const pendingBody = await response.text()
      if (!taskStartPendingResponse(pendingBody)) {
        throw new HelmrApiError(response.status, pendingBody)
      }
      const retryDelay = taskStartPendingRetryDelay(response)
      if (Date.now() - startedAt + retryDelay > TASK_START_PENDING_MAX_WAIT_MS) {
        throw new HelmrApiError(response.status, pendingBody)
      }
      await delay(retryDelay, opts.signal)
    }
  }

  async #startTaskAndWait<TTask extends AnyTask>(
    taskId: string,
    payload: unknown,
    opts: TaskStartAndWaitOptions<TaskSecrets<TTask>>,
    maxDurationSeconds?: number,
  ): Promise<TaskSessionSnapshot<TaskOutput<TTask>>> {
    validateRetryPolicy(opts.retry, "retry")
    const body = {
      ...taskStartBody(payload, opts, maxDurationSeconds),
      ...(opts.timeoutSeconds === undefined ? {} : { timeout_seconds: opts.timeoutSeconds }),
    }
    const startedAt = Date.now()
    for (;;) {
      const response = await this.#fetch(`/api/tasks/${encodeURIComponent(taskId)}/start-and-wait`, {
        method: "POST",
        body: JSON.stringify(body),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal),
      })
      if (response.status !== 202) {
        return taskSessionFromResponse<TaskOutput<TTask>>((await response.json()) as TaskSessionResponse)
      }
      const pendingBody = await response.text()
      if (!taskStartPendingResponse(pendingBody)) {
        throw new HelmrApiError(response.status, pendingBody)
      }
      const retryDelay = taskStartPendingRetryDelay(response)
      if (Date.now() - startedAt + retryDelay > TASK_START_PENDING_MAX_WAIT_MS) {
        throw new HelmrApiError(response.status, pendingBody)
      }
      await delay(retryDelay, opts.signal)
    }
  }

  #openWorkspace(id: string): WorkspaceHandle {
    return {
      id,
      exec: async (command, opts = {}) => {
        return await this.#createWorkspaceExec(id, command, opts)
      },
      execs: {
        retrieve: (execId: string): WorkspaceExecHandle => {
          return this.#openWorkspaceExec(id, execId)
        },
        list: async (opts = {}) => {
          const response = await this.#json<ListWorkspaceExecsResponse>(
            `${workspaceResourcePath(id, opts)}/execs${workspacePrimitiveListQuery(opts)}`,
            requestSignal(opts.signal),
          )
          return response.execs.map(workspaceExecFromResponse)
        },
      },
      pty: {
        create: async (opts = {}) => {
          return await this.#createWorkspacePty(id, opts)
        },
        retrieve: (ptyId: string): WorkspacePtyHandle => {
          return this.#openWorkspacePty(id, ptyId)
        },
        list: async (opts = {}) => {
          const response = await this.#json<ListWorkspacePtySessionsResponse>(
            `${workspaceResourcePath(id, opts)}/pty${workspacePrimitiveListQuery(opts)}`,
            requestSignal(opts.signal),
          )
          return response.ptys.map(workspacePtyFromResponse)
        },
      },
      retrieve: async (opts = {}) => {
        const response = await this.#json<WorkspaceEnvelopeResponse>(
          workspaceResourcePath(id, opts),
          requestSignal(opts.signal),
        )
        return workspaceFromResponse(response.workspace)
      },
      update: async (opts) => {
        const response = await this.#json<WorkspaceEnvelopeResponse>(workspaceResourcePath(id, opts), {
          method: "PATCH",
          body: JSON.stringify(workspaceUpdateBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal),
        })
        return workspaceFromResponse(response.workspace)
      },
      delete: async (opts = {}) => {
        const response = await this.#json<WorkspaceEnvelopeResponse>(workspaceResourcePath(id, opts), {
          method: "DELETE",
          ...requestSignal(opts.signal),
        })
        return workspaceFromResponse(response.workspace)
      },
      materialize: async (opts = {}) => {
        const response = await this.#json<WorkspaceMaterializationResponse>(
          `${workspaceResourcePath(id, opts)}/materialize`,
          {
            method: "POST",
            body: JSON.stringify(workspaceMaterializeBody(opts)),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal),
          },
        )
        return workspaceMaterializationFromResponse(response)
      },
      connect: async (opts = {}) => {
        const response = await this.#json<WorkspaceMaterializationResponse>(
          `${workspaceResourcePath(id, opts)}/connect`,
          {
            method: "POST",
            body: JSON.stringify(workspaceMaterializeBody(opts)),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal),
          },
        )
        return workspaceMaterializationFromResponse(response)
      },
    }
  }

  async #createWorkspaceExec(id: string, command: readonly string[], opts: WorkspaceExecCreateOptions): Promise<WorkspaceExecHandle> {
    const response = await this.#json<WorkspaceExecEnvelopeResponse>(
      `${workspaceResourcePath(id, opts)}/execs`,
      {
        method: "POST",
        body: JSON.stringify(workspaceExecCreateBody(command, opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal),
      },
    )
    return this.#openWorkspaceExec(id, response.exec.id)
  }

  #openWorkspaceExec(workspaceId: string, execId: string): WorkspaceExecHandle {
    const client = this
    return {
      id: execId,
      workspaceId,
      retrieve: async (opts = {}) => {
        const response = await client.#json<WorkspaceExecEnvelopeResponse>(
          `${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}`,
          requestSignal(opts.signal),
        )
        return workspaceExecFromResponse(response.exec)
      },
      stdout: client.#workspaceExecReadableStreamApi(workspaceId, execId, "stdout"),
      stderr: client.#workspaceExecReadableStreamApi(workspaceId, execId, "stderr"),
      stdin: {
        write: async (data, opts) => {
          const response = await client.#json<WorkspaceExecStreamChunkResponse>(
            `${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}/stdin`,
            {
              method: "POST",
              body: JSON.stringify(workspaceStreamWriteBody(data, opts)),
              headers: { "content-type": "application/json" },
              ...requestSignal(opts.signal),
            },
          )
          return workspaceStreamChunkFromResponse(response)
        },
        close: async (opts = {}) => {
          const response = await client.#json<WorkspaceExecEnvelopeResponse>(
            `${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}/stdin/close`,
            {
              method: "POST",
              body: "{}",
              headers: { "content-type": "application/json" },
              ...requestSignal(opts.signal),
            },
          )
          return workspaceExecFromResponse(response.exec)
        },
      },
      wait: async (opts = {}) => {
        return await client.#waitWorkspaceExec(workspaceId, execId, opts)
      },
    }
  }

  async #waitWorkspaceExec(workspaceId: string, execId: string, opts: WorkspaceExecWaitOptions): Promise<WorkspaceExec> {
    const pollIntervalMs = opts.pollIntervalMs ?? 250
    for (;;) {
      const response = await this.#json<WorkspaceExecEnvelopeResponse>(
        `${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}`,
        requestSignal(opts.signal),
      )
      const exec = workspaceExecFromResponse(response.exec)
      if (workspaceExecTerminal(exec.state)) return exec
      await delay(pollIntervalMs, opts.signal)
    }
  }

  async #createWorkspacePty(id: string, opts: WorkspacePtyCreateOptions): Promise<WorkspacePtyHandle> {
    const response = await this.#json<WorkspacePtyEnvelopeResponse>(
      `${workspaceResourcePath(id, opts)}/pty`,
      {
        method: "POST",
        body: JSON.stringify(workspacePtyCreateBody(opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal),
      },
    )
    return this.#openWorkspacePty(id, response.pty.id)
  }

  #openWorkspacePty(workspaceId: string, ptyId: string): WorkspacePtyHandle {
    const client = this
    return {
      id: ptyId,
      workspaceId,
      retrieve: async (opts = {}) => {
        const response = await client.#json<WorkspacePtyEnvelopeResponse>(
          `${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}`,
          requestSignal(opts.signal),
        )
        return workspacePtyFromResponse(response.pty)
      },
      output: client.#workspacePtyOutputApi(workspaceId, ptyId),
      input: async (data, opts) => {
        const response = await client.#json<WorkspacePtyStreamChunkResponse>(
          `${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/input`,
          {
            method: "POST",
            body: JSON.stringify(workspaceStreamWriteBody(data, opts)),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal),
          },
        )
        return workspaceStreamChunkFromResponse(response)
      },
      resize: async (cols, rows, opts = {}) => {
        const response = await client.#json<WorkspacePtyEnvelopeResponse>(
          `${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/resize`,
          {
            method: "POST",
            body: JSON.stringify({ cols, rows }),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal),
          },
        )
        return workspacePtyFromResponse(response.pty)
      },
      close: async (opts = {}) => {
        return await client.#requestWorkspacePtyClose(workspaceId, ptyId, opts)
      },
    }
  }

  async #requestWorkspacePtyClose(workspaceId: string, ptyId: string, opts: WorkspaceRetrieveOptions): Promise<WorkspacePty> {
    const response = await this.#json<WorkspacePtyEnvelopeResponse>(
      `${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/close`,
      {
        method: "POST",
        body: "{}",
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal),
      },
    )
    return workspacePtyFromResponse(response.pty)
  }

  #workspaceExecReadableStreamApi(workspaceId: string, execId: string, stream: "stdout" | "stderr"): WorkspaceExecReadableStreamApi {
    return {
      list: async (opts = {}) => {
        const response = await this.#json<ListWorkspaceExecStreamChunksResponse>(
          `${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}/${stream}${workspaceStreamListQuery(opts)}`,
          requestSignal(opts.signal),
        )
        return response.chunks.map(workspaceStreamChunkFromResponse)
      },
      stream: (opts = {}) => this.#unsupportedWorkspaceStreamFollow(opts),
    }
  }

  #workspacePtyOutputApi(workspaceId: string, ptyId: string): WorkspacePtyOutputApi {
    return {
      list: async (opts = {}) => {
        const response = await this.#json<ListWorkspacePtyStreamChunksResponse>(
          `${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/output${workspaceStreamListQuery(opts)}`,
          requestSignal(opts.signal),
        )
        return response.chunks.map(workspaceStreamChunkFromResponse)
      },
      stream: (opts = {}) => this.#unsupportedWorkspaceStreamFollow(opts),
    }
  }

  async *#unsupportedWorkspaceStreamFollow(_opts: WorkspaceStreamFollowOptions): AsyncIterable<WorkspaceStreamChunk> {
    throw new Error("workspace_stream_follow_unsupported")
  }


  #openSession<TOutput = unknown>(id: string): OpenTaskSessionApi<TOutput> {
    return {
      id,
      retrieve: async (opts = {}) => {
        const response = await this.#json<TaskSessionResponse>(
          `/api/sessions/${encodeURIComponent(id)}`,
          requestSignal(opts.signal),
        )
        return taskSessionFromResponse<TOutput>(response)
      },
      wait: async (opts = {}) => {
        const response = await this.#json<TaskSessionResponse>(
          `/api/sessions/${encodeURIComponent(id)}/wait`,
          {
            method: "POST",
            body: JSON.stringify(taskSessionWaitBody(opts)),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal),
          },
        )
        return taskSessionFromResponse<TOutput>(response)
      },
      close: async (opts = {}) => {
        const response = await this.#json<TaskSessionResponse>(
          `/api/sessions/${encodeURIComponent(id)}/close`,
          {
            method: "POST",
            body: JSON.stringify(opts.reason === undefined ? {} : { reason: opts.reason }),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal),
          },
        )
        return taskSessionFromResponse<TOutput>(response)
      },
      cancel: async (opts = {}) => {
        const response = await this.#json<TaskSessionResponse>(
          `/api/sessions/${encodeURIComponent(id)}/cancel`,
          {
            method: "POST",
            body: JSON.stringify(opts.reason === undefined ? {} : { reason: opts.reason }),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal),
          },
        )
        return taskSessionFromResponse<TOutput>(response)
      },
      runs: async (opts = {}) => {
        const response = await this.#json<ListTaskSessionRunsResponse>(
          `/api/sessions/${encodeURIComponent(id)}/runs`,
          requestSignal(opts.signal),
        )
        return response.runs.map(taskSessionRunFromResponse)
      },
      input: (channelInput: string) => {
        const channel = validateChannelName(channelInput)
        return {
          send: async <TData = unknown>(data: TData, opts: SessionChannelInputSendOptions = {}) => {
            const response = await this.#json<AppendChannelRecordResponse>(
              `/api/sessions/${encodeURIComponent(id)}/channels/${encodeURIComponent(channel)}/inputs`,
              {
                method: "POST",
                body: JSON.stringify(channelInputAppendBody(data, opts)),
                headers: { "content-type": "application/json" },
                ...requestSignal(opts.signal),
              },
            )
            return appendChannelRecordFromResponse<TData>(response)
          },
          list: async <TData = unknown>(opts: SessionChannelListOptions = {}) => {
            return await this.#listSessionChannelRecords<TData>(id, channel, "inputs", opts)
          },
        }
      },
      output: (channelInput: string) => {
        const channel = validateChannelName(channelInput)
        return {
          list: async <TData = unknown>(opts: SessionChannelListOptions = {}) => {
            return await this.#listSessionChannelRecords<TData>(id, channel, "outputs", opts)
          },
          stream: async <TData = unknown>(opts: SessionChannelOutputStreamOptions = {}) => {
            return this.#streamSessionChannelOutputs<TData>(id, channel, opts)
          },
        }
      },
    }
  }

  async #listSessionChannelRecords<TData>(
    sessionID: string,
    channel: string,
    direction: "inputs" | "outputs",
    opts: SessionChannelListOptions,
  ): Promise<ChannelRecord<TData>[]> {
    const response = await this.#json<ListChannelRecordsResponse>(
      `/api/sessions/${encodeURIComponent(sessionID)}/channels/${encodeURIComponent(channel)}/${direction}${sessionChannelQuery(opts)}`,
      requestSignal(opts.signal),
    )
    return response.records.map(channelRecordFromResponse<TData>)
  }

  async #streamSessionChannelOutputs<TData>(
    sessionID: string,
    channel: string,
    opts: SessionChannelOutputStreamOptions,
  ): Promise<AsyncIterable<ChannelRecord<TData>>> {
    const client = this
    const stream = async function* (): AsyncIterable<ChannelRecord<TData>> {
      let cursor = opts.cursor
      for (;;) {
        try {
          const response = await client.#fetch(
            `/api/sessions/${encodeURIComponent(sessionID)}/channels/${encodeURIComponent(channel)}/outputs/stream${sessionChannelStreamQuery(opts, cursor)}`,
            {
              headers: { accept: "text/event-stream" },
              ...requestSignal(opts.signal),
            },
          )
          for await (const record of parseChannelRecordSse<TData>(response)) {
            cursor = Math.max(cursor ?? 0, record.sequence)
            yield record
          }
        } catch (error) {
          throwIfAborted(opts.signal)
          if (runEventStreamErrorIsFatal(error)) {
            throw error
          }
        }
        try {
          const session = await client.sessions.retrieve(sessionID, retrieveOptions(opts.signal))
          if (taskSessionTerminal(session.status)) {
            return
          }
        } catch (error) {
          throwIfAborted(opts.signal)
          if (runSnapshotErrorIsFatal(error)) {
            throw error
          }
        }
        await delay(RUN_EVENT_RECONNECT_DELAY_MS, opts.signal)
      }
    }
    return stream()
  }

  readonly runs = {
    retrieve: async <TOutput = unknown>(
      idOrHandle: string | RunHandle<TOutput>,
      opts: RetrieveRunOptions = {},
    ): Promise<RunSnapshot<TOutput>> => {
      const response = await this.#json<RunResponse>(
        `/api/runs/${encodeURIComponent(runId(idOrHandle))}`,
        requestSignal(opts.signal),
      )
      return runResponseToSnapshot<TOutput>(response)
    },
    wait: async <TOutput = unknown>(
      idOrHandle: string | RunHandle<TOutput>,
      opts: RunWaitpointOptions = {},
    ): Promise<RunSnapshot<TOutput>> => {
      const id = runId(idOrHandle)
      const timeoutMs = opts.timeoutMs
      const wait = waitSignal(opts.signal, timeoutMs, () => new TimeoutError(`run ${id} did not finish within ${timeoutMs}ms`))
      try {
        let run = await this.#retrieveRunSnapshotWithRetry<TOutput>(id, wait.signal, RUN_EVENT_RECONNECT_DELAY_MS)
        if (isTerminalRunStatus(run.status)) {
          return run
        }
        let cursor: number | undefined
        for (;;) {
          throwIfAborted(wait.signal)
          let terminalEventSeen = false
          try {
            for await (const event of this.#streamEventRecordsOnce(id, { cursor, signal: wait.signal })) {
              cursor = nextRunEventCursor(cursor, event)
              if (runEventRecordIsTerminal(event)) {
                terminalEventSeen = true
                break
              }
            }
          } catch (error) {
            throwIfAborted(wait.signal)
            if (error instanceof SseProtocolError && error.cursor !== undefined) {
              cursor = advanceRunEventCursor(cursor, error.cursor)
            } else if (runEventWaitStreamErrorIsFatal(error)) {
              throw error
            }
          }
          if (terminalEventSeen) {
            return await this.#waitForTerminalSnapshot<TOutput>(id, wait.signal)
          }
          try {
            run = await this.runs.retrieve<TOutput>(id, retrieveOptions(wait.signal))
          } catch (error) {
            throwIfAborted(wait.signal)
            if (runSnapshotErrorIsFatal(error)) {
              throw error
            }
            await delay(RUN_EVENT_RECONNECT_DELAY_MS, wait.signal)
            continue
          }
          if (isTerminalRunStatus(run.status)) {
            return run
          }
          await delay(RUN_EVENT_RECONNECT_DELAY_MS, wait.signal)
        }
      } finally {
        wait.cleanup()
      }
    },
    cancel: async <TOutput = unknown>(
      idOrHandle: string | RunHandle<TOutput>,
      opts: CancelRunOptions = {},
    ): Promise<RunSnapshot<TOutput>> => {
      const response = await this.#json<CancelRunResponse>(
        `/api/runs/${encodeURIComponent(runId(idOrHandle))}/cancel`,
        {
          method: "POST",
          body: JSON.stringify(cancelRunBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal),
        },
      )
      return runResponseToSnapshot<TOutput>(response.run)
    },
    list: async (opts: ListRunsOptions = {}): Promise<RunSummary[]> => {
      const query = new URLSearchParams()
      if (opts.status !== undefined) query.set("status", opts.status)
      if (opts.limit !== undefined) query.set("limit", String(opts.limit))
      const suffix = query.size === 0 ? "" : `?${query}`
      const response = await this.#json<ListRunsResponse>(`/api/runs${suffix}`, requestSignal(opts.signal))
      return response.runs.map((run) => runResponseToSnapshot(run))
    },
    waitpoints: {
      listPage: async <TOutput = unknown>(
        idOrHandle: string | RunHandle<TOutput>,
        opts: RunWaitpointListOptions = {},
      ): Promise<RunWaitpointsPage> => {
        const id = runId(idOrHandle)
        const response = await this.#json<ListRunWaitpointsResponse>(
          `/api/runs/${encodeURIComponent(id)}/waitpoints${runWaitpointListQuery(opts)}`,
          requestSignal(opts.signal),
        )
        return {
          waitpoints: response.waitpoints.map((request) => pendingWaitpointFromResponse(id, request))
            .filter((request): request is PendingWaitpoint => request !== null),
          nextCursor: response.next_cursor ?? null,
        }
      },
      list: async <TOutput = unknown>(
        idOrHandle: string | RunHandle<TOutput>,
        opts: RunWaitpointListOptions = {},
      ): Promise<PendingWaitpoint[]> => {
        return [...(await this.runs.waitpoints.listPage(idOrHandle, opts)).waitpoints]
      },
    },
    logs: {
      retrieve: async <TOutput = unknown>(
        idOrHandle: string | RunHandle<TOutput>,
        opts: { readonly signal?: AbortSignal } = {},
      ): Promise<LogSnapshot> => {
        return await this.#retrieveLogs(runId(idOrHandle), opts.signal)
      },
    },
    events: {
      list: async <TOutput = unknown>(
        idOrHandle: string | RunHandle<TOutput>,
        opts: ListRunEventsOptions = {},
      ): Promise<RunEvent[]> => {
        return await this.#listEvents(runId(idOrHandle), opts)
      },
      subscribe: async <TOutput = unknown>(
        idOrHandle: string | RunHandle<TOutput>,
        opts: SubscribeRunEventsOptions = {},
      ): Promise<AsyncIterable<RunEvent>> => {
        return await this.#subscribeEvents(runId(idOrHandle), opts)
      },
    },
  }

  readonly schedules: SchedulesApi = {
    create: async (opts: ScheduleCreateOptions): Promise<Schedule> => {
      const response = await this.#json<ScheduleResponse>("/api/schedules", {
        method: "POST",
        body: JSON.stringify(scheduleCreateBody(opts)),
        headers: { "content-type": "application/json" },
      })
      return scheduleFromResponse(response)
    },
    list: async (opts: ListSchedulesOptions = {}): Promise<Schedule[]> => {
      const response = await this.#json<ListSchedulesResponse>("/api/schedules", requestSignal(opts.signal))
      return response.schedules.map(scheduleFromResponse)
    },
    update: async (id: string, opts: ScheduleUpdateOptions & RetrieveScheduleOptions): Promise<Schedule> => {
      const response = await this.#json<ScheduleResponse>(`/api/schedules/${encodeURIComponent(id)}`, {
        method: "PUT",
        body: JSON.stringify(scheduleCreateBody(opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal),
      })
      return scheduleFromResponse(response)
    },
    retrieve: async (id: string, opts: RetrieveScheduleOptions = {}): Promise<Schedule> => {
      return scheduleFromResponse(
        await this.#json<ScheduleResponse>(`/api/schedules/${encodeURIComponent(id)}`, requestSignal(opts.signal)),
      )
    },
    activate: async (id: string, opts: RetrieveScheduleOptions = {}): Promise<Schedule> => {
      return scheduleFromResponse(
        await this.#json<ScheduleResponse>(`/api/schedules/${encodeURIComponent(id)}/activate`, {
          method: "POST",
          ...requestSignal(opts.signal),
        }),
      )
    },
    deactivate: async (id: string, opts: RetrieveScheduleOptions = {}): Promise<Schedule> => {
      return scheduleFromResponse(
        await this.#json<ScheduleResponse>(`/api/schedules/${encodeURIComponent(id)}/deactivate`, {
          method: "POST",
          ...requestSignal(opts.signal),
        }),
      )
    },
    delete: async (id: string, opts: RetrieveScheduleOptions = {}): Promise<void> => {
      await this.#fetch(`/api/schedules/${encodeURIComponent(id)}`, {
        method: "DELETE",
        ...requestSignal(opts.signal),
      })
    },
  }

  async #retrieveLogs(id: string, signal?: AbortSignal): Promise<LogSnapshot> {
    const response = await this.#json<LogSnapshotResponse>(
      `/api/runs/${encodeURIComponent(id)}/logs`,
      requestSignal(signal),
    )
    return {
      stdout: decodeBase64Text(response.stdout_base64),
      stderr: decodeBase64Text(response.stderr_base64),
      cursor: response.cursor,
      truncated: response.truncated,
    }
  }

  async #listEvents(id: string, opts: ListRunEventsOptions): Promise<RunEvent[]> {
    const events: RunEventRecord[] = []
    let cursor = opts.cursor
    for (;;) {
      const query = new URLSearchParams()
      if (cursor !== undefined) query.set("cursor", String(cursor))
      if (opts.pageSize !== undefined) query.set("limit", String(opts.pageSize))
      const suffix = query.size === 0 ? "" : `?${query}`
      const page = await this.#json<RunEventRecordPage>(
        `/api/runs/${encodeURIComponent(id)}/events${suffix}`,
        requestSignal(opts.signal),
      )
      events.push(...page.events)
      if (page.next_cursor === undefined || page.next_cursor === null) {
        break
      }
      cursor = page.next_cursor
    }
    return events
      .map((event) => runEventRecordToRunEvent(event))
      .filter((event): event is RunEvent => event !== undefined)
  }

  async #subscribeEvents(id: string, opts: SubscribeRunEventsOptions): Promise<AsyncIterable<RunEvent>> {
    const stream = async function* (client: HelmrClient): AsyncIterable<RunEvent> {
      let cursor = opts.cursor
      for (;;) {
        let checkSnapshot = false
        try {
          for await (const record of client.#streamEventRecordsOnce(id, { cursor, signal: opts.signal })) {
            cursor = nextRunEventCursor(cursor, record)
            const terminal = runEventRecordIsTerminal(record)
            const event = runEventRecordToRunEvent(record)
            if (event !== undefined) {
              yield event
            }
            if (terminal) {
              return
            }
          }
          checkSnapshot = true
        } catch (error) {
          throwIfAborted(opts.signal)
          if (error instanceof SseProtocolError && error.cursor !== undefined) {
            cursor = advanceRunEventCursor(cursor, error.cursor)
          } else if (runEventStreamErrorIsFatal(error)) {
            throw error
          }
          checkSnapshot = true
        }
        if (checkSnapshot) {
          try {
            const run = await client.runs.retrieve(id, retrieveOptions(opts.signal))
            if (isTerminalRunStatus(run.status)) {
              for await (const record of client.#listEventRecordsAfter(id, cursor, opts.signal)) {
                cursor = nextRunEventCursor(cursor, record)
                const terminal = runEventRecordIsTerminal(record)
                const event = runEventRecordToRunEvent(record)
                if (event !== undefined) {
                  yield event
                }
                if (terminal) {
                  break
                }
              }
              return
            }
          } catch (error) {
            throwIfAborted(opts.signal)
            if (runSnapshotErrorIsFatal(error)) {
              throw error
            }
          }
        }
        await delay(RUN_EVENT_RECONNECT_DELAY_MS, opts.signal)
      }
    }
    return stream(this)
  }

  async *#streamEventRecordsOnce(
    id: string,
    opts: { readonly cursor?: number | undefined; readonly signal?: AbortSignal | undefined },
  ): AsyncIterable<RunEventRecord> {
    const query = new URLSearchParams()
    query.set("follow", "1")
    if (opts.cursor !== undefined) query.set("cursor", String(opts.cursor))
    const response = await this.#fetch(`/api/runs/${encodeURIComponent(id)}/events?${query}`, {
      headers: { accept: "text/event-stream" },
      ...requestSignal(opts.signal),
    })
    yield* parseSse(response)
  }

  async *#listEventRecordsAfter(
    id: string,
    cursor: number | undefined,
    signal: AbortSignal | undefined,
  ): AsyncIterable<RunEventRecord> {
    let nextCursor = cursor
    for (;;) {
      const query = new URLSearchParams()
      if (nextCursor !== undefined) query.set("cursor", String(nextCursor))
      const suffix = query.size === 0 ? "" : `?${query}`
      const page = await this.#json<RunEventRecordPage>(
        `/api/runs/${encodeURIComponent(id)}/events${suffix}`,
        requestSignal(signal),
      )
      for (const event of page.events) {
        yield event
      }
      if (page.next_cursor === undefined || page.next_cursor === null) {
        return
      }
      if (nextCursor !== undefined && page.next_cursor <= nextCursor) {
        return
      }
      nextCursor = page.next_cursor
    }
  }

  async #waitForTerminalSnapshot<TOutput>(id: string, signal: AbortSignal | undefined): Promise<RunSnapshot<TOutput>> {
    let retryDelayMs = RUN_TERMINAL_SNAPSHOT_RETRY_DELAY_MS
    for (;;) {
      const run = await this.#retrieveRunSnapshotWithRetry<TOutput>(id, signal, retryDelayMs)
      if (isTerminalRunStatus(run.status)) {
        return run
      }
      await delay(retryDelayMs, signal)
      retryDelayMs = Math.min(retryDelayMs * 2, RUN_EVENT_RECONNECT_DELAY_MS)
    }
  }

  async #retrieveRunSnapshotWithRetry<TOutput>(
    id: string,
    signal: AbortSignal | undefined,
    retryDelayMs: number,
  ): Promise<RunSnapshot<TOutput>> {
    for (;;) {
      try {
        return await this.runs.retrieve<TOutput>(id, retrieveOptions(signal))
      } catch (error) {
        throwIfAborted(signal)
        if (runSnapshotErrorIsFatal(error)) {
          throw error
        }
        await delay(retryDelayMs, signal)
      }
    }
  }

  async #json<T>(path: string, init: RequestInit = {}): Promise<T> {
    return (await this.#fetch(path, init)).json() as Promise<T>
  }

  async #fetch(path: string, init: RequestInit = {}): Promise<Response> {
    const headers = new Headers(init.headers)
    headers.set(HELMR_API_VERSION_HEADER, HELMR_API_VERSION)
    headers.set(HELMR_SDK_VERSION_HEADER, HELMR_SDK_VERSION)
    if (this.#apiKey !== undefined && !headers.has("authorization")) {
      headers.set("authorization", `Bearer ${this.#apiKey}`)
    }
    const request: RequestInit = {
      ...init,
      headers,
    }
    const response = await fetch(endpointUrl(this.#baseUrl, path), request)
    if (response.status === 401) {
      throw new AuthError("Helmr authentication failed")
    }
    if (!response.ok) {
      throw new HelmrApiError(response.status, await response.text())
    }
    return response
  }
}

function normalizedBaseUrl(url: URL): URL {
  if (url.search !== "" || url.hash !== "") {
    throw new UnsupportedTransportError("HelmrClient URL must not include query or fragment")
  }
  return url
}

function isLoopbackHost(hostname: string): boolean {
  const host = hostname.trim().toLowerCase().replace(/^\[/, "").replace(/\]$/, "")
  if (host === "localhost" || host === "::1") {
    return true
  }
  const ipv4 = /^(\d+)\.(\d+)\.(\d+)\.(\d+)$/.exec(host)
  if (ipv4 === null) {
    return false
  }
  return ipv4[1] === "127" && ipv4.slice(2).every((part) => Number(part) >= 0 && Number(part) <= 255)
}

function endpointUrl(baseUrl: URL, path: string): URL {
  const endpoint = new URL(baseUrl.toString())
  const queryStart = path.indexOf("?")
  const pathOnly = queryStart === -1 ? path : path.slice(0, queryStart)
  const query = queryStart === -1 ? "" : path.slice(queryStart + 1)
  endpoint.pathname = joinUrlPath(endpoint.pathname, pathOnly)
  endpoint.search = query
  endpoint.hash = ""
  return endpoint
}

function joinUrlPath(basePath: string, path: string): string {
  const base = basePath.replace(/\/+$/, "")
  const suffix = `/${path.replace(/^\/+/, "")}`
  return base === "" ? suffix : `${base}${suffix}`
}

export interface RunResponse {
  readonly id: string
  readonly project_id?: string
  readonly environment_id?: string
  readonly version?: string
  readonly deployment_version?: string
  readonly api_version?: string
  readonly sdk_version?: string
  readonly cli_version?: string
  readonly attempt_number?: number | null
  readonly task_id: string
  readonly status: string
  readonly metadata?: Record<string, unknown>
  readonly exit_code?: number | null
  readonly created_at?: string
  readonly updated_at?: string
  readonly pending_waitpoint?: PendingWaitpointResponse | null
  readonly output?: unknown
}

export interface ListRunsResponse {
  readonly runs: readonly RunResponse[]
}

interface TaskStartResponse {
  readonly session: TaskSessionResponse
  readonly run: RunResponse
  readonly is_cached?: boolean
}

interface TaskSessionResponse {
  readonly id: string
  readonly project_id: string
  readonly environment_id: string
  readonly task_id: string
  readonly initial_deployment_id: string
  readonly active_deployment_id: string
  readonly external_id?: string
  readonly status: TaskSessionStatus
  readonly current_run_id?: string | null
  readonly workspace_id?: string | null
  readonly metadata?: Record<string, unknown> | null
  readonly tags?: readonly string[] | null
  readonly result?: unknown
  readonly error?: unknown
  readonly timed_out?: boolean
  readonly terminal_reason?: unknown
  readonly expires_at?: string | null
  readonly created_at: string
  readonly updated_at: string
}

interface ListTaskSessionsResponse {
  readonly sessions: readonly TaskSessionResponse[]
}

interface TaskSessionRunResponse {
  readonly id: string
  readonly run_id: string
  readonly deployment_id: string
  readonly previous_run_id?: string
  readonly turn_index: number
  readonly status: string
  readonly execution_status: string
  readonly terminal_outcome?: string
  readonly created_at: string
  readonly ended_at?: string
}

interface ListTaskSessionRunsResponse {
  readonly runs: readonly TaskSessionRunResponse[]
}

interface ChannelRecordResponse {
  readonly id: string
  readonly channel_id: string
  readonly sequence: number
  readonly data: unknown
  readonly correlation_id?: string
  readonly content_type: string
  readonly created_at: string
}

interface AppendChannelRecordResponse {
  readonly record: ChannelRecordResponse
  readonly idempotency_status?: string
}

interface ListChannelRecordsResponse {
  readonly records: readonly ChannelRecordResponse[]
}

interface PublicAccessTokenScopeResponse {
  readonly type: "session.input.append" | "session.output.read"
  readonly session_id: string
  readonly channel: string
  readonly correlation_id?: string
}

interface PublicAccessTokenResponse {
  readonly id: string
  readonly public_access_token: string
  readonly scope: PublicAccessTokenScopeResponse
  readonly expires_at: string
  readonly max_uses?: number
  readonly created_at: string
}

interface CancelRunResponse {
  readonly run: RunResponse
}

interface ScheduleResponse {
  readonly id: string
  readonly type: "imperative" | "declarative"
  readonly project_id: string
  readonly environment_id: string
  readonly task: string
  readonly deduplication_key?: string
  readonly external_id?: string
  readonly cron: string
  readonly timezone: string
  readonly active: boolean
  readonly status: "active" | "inactive" | "errored"
  readonly last_error?: string
  readonly next_fire_at?: string
  readonly last_fire_at?: string
  readonly created_at: string
  readonly updated_at: string
}

interface ListSchedulesResponse {
  readonly schedules: readonly ScheduleResponse[]
}

interface LogSnapshotResponse {
  readonly stdout_base64: string
  readonly stderr_base64: string
  readonly cursor: string
  readonly truncated: boolean
}

interface WaitpointTokenResponse {
  readonly id: string
  readonly status?: "waiting" | "completed" | "timed_out" | "cancelled"
  readonly callback_url: string
  readonly public_access_token?: string
  readonly timeout_at?: string | null
  readonly data?: unknown
  readonly tags?: readonly string[]
  readonly metadata?: Record<string, unknown>
}

interface ListWaitpointTokensResponse {
  readonly tokens: readonly WaitpointTokenResponse[]
  readonly next_cursor?: string | null
}

interface WorkspaceResponse {
  readonly id: string
  readonly project_id: string
  readonly environment_id: string
  readonly deployment_sandbox_id: string
  readonly sandbox_id: string
  readonly sandbox_fingerprint: string
  readonly external_id?: string
  readonly current_version_id?: string | null
  readonly state: WorkspaceState
  readonly desired_state: WorkspaceDesiredState
  readonly dirty_state: WorkspaceDirtyState
  readonly last_materialization_id?: string | null
  readonly metadata?: Record<string, unknown>
  readonly tags?: readonly string[]
  readonly auto_stop_at?: string | null
  readonly auto_archive_at?: string | null
  readonly auto_delete_at?: string | null
  readonly last_activity_at: string
  readonly created_at: string
  readonly updated_at: string
  readonly archived_at?: string | null
  readonly deleted_at?: string | null
}

interface WorkspaceEnvelopeResponse {
  readonly workspace: WorkspaceResponse
  readonly is_cached?: boolean
}

interface ListWorkspacesResponse {
  readonly workspaces: readonly WorkspaceResponse[]
}

interface WorkspaceMaterializationResponse {
  readonly id: string
  readonly project_id: string
  readonly environment_id: string
  readonly workspace_id: string
  readonly deployment_sandbox_id: string
  readonly base_version_id?: string | null
  readonly worker_instance_id?: string | null
  readonly state: string
  readonly fencing_generation: number
  readonly dirty_generation: number
  readonly reservation_expires_at?: string | null
  readonly last_heartbeat_at?: string | null
  readonly created_at: string
  readonly updated_at: string
}

interface WorkspaceExecResponse {
  readonly id: string
  readonly workspace_id: string
  readonly materialization_id?: string | null
  readonly command: readonly string[]
  readonly cwd: string
  readonly env_shape?: Record<string, string>
  readonly filesystem_mode: string
  readonly state: WorkspaceExecState
  readonly detached: boolean
  readonly process_id?: string | null
  readonly exit_code?: number | null
  readonly signal?: string | null
  readonly error?: unknown
  readonly stdout_cursor: number
  readonly stderr_cursor: number
  readonly stdin_cursor: number
  readonly stdin_closed_at?: string | null
  readonly created_at: string
  readonly started_at?: string | null
  readonly exited_at?: string | null
  readonly updated_at: string
}

interface WorkspaceExecEnvelopeResponse {
  readonly exec: WorkspaceExecResponse
  readonly is_cached?: boolean
}

interface ListWorkspaceExecsResponse {
  readonly execs: readonly WorkspaceExecResponse[]
}

interface WorkspacePtyResponse {
  readonly id: string
  readonly workspace_id: string
  readonly materialization_id?: string | null
  readonly cwd: string
  readonly cols: number
  readonly rows: number
  readonly filesystem_mode: string
  readonly state: WorkspacePtyState
  readonly process_id?: string | null
  readonly output_cursor: number
  readonly input_cursor: number
  readonly error?: unknown
  readonly created_at: string
  readonly started_at?: string | null
  readonly closed_at?: string | null
  readonly updated_at: string
}

interface WorkspacePtyEnvelopeResponse {
  readonly pty: WorkspacePtyResponse
}

interface ListWorkspacePtySessionsResponse {
  readonly ptys: readonly WorkspacePtyResponse[]
}

interface WorkspaceExecStreamChunkResponse {
  readonly id: string
  readonly stream: string
  readonly offset_start: number
  readonly offset_end: number
  readonly data: string
  readonly observed_at: string
  readonly created_at: string
}

interface WorkspacePtyStreamChunkResponse {
  readonly id: string
  readonly stream: string
  readonly offset_start: number
  readonly offset_end: number
  readonly data: string
  readonly observed_at: string
  readonly created_at: string
}

interface ListWorkspaceExecStreamChunksResponse {
  readonly chunks: readonly WorkspaceExecStreamChunkResponse[]
}

interface ListWorkspacePtyStreamChunksResponse {
  readonly chunks: readonly WorkspacePtyStreamChunkResponse[]
}

interface ListRunWaitpointsResponse {
  readonly waitpoints: readonly PendingWaitpointResponse[]
  readonly next_cursor?: string | null
}

function runResponseToSnapshot<TOutput = unknown>(response: RunResponse): RunSnapshot<TOutput> {
  return runSnapshot<TOutput>({
    id: response.id,
    taskId: response.task_id,
    ...(response.version === undefined && response.deployment_version === undefined
      ? {}
      : { version: response.version ?? response.deployment_version ?? null }),
    ...(response.deployment_version === undefined && response.version === undefined
      ? {}
      : { deploymentVersion: response.deployment_version ?? response.version ?? null }),
    ...(response.api_version === undefined ? {} : { apiVersion: response.api_version }),
    ...(response.sdk_version === undefined ? {} : { sdkVersion: response.sdk_version }),
    ...(response.cli_version === undefined ? {} : { cliVersion: response.cli_version }),
    attemptNumber: response.attempt_number ?? null,
    status: response.status,
    metadata: response.metadata ?? {},
    exitCode: response.exit_code ?? null,
    ...(response.created_at === undefined ? {} : { createdAt: response.created_at }),
    ...(response.updated_at === undefined ? {} : { updatedAt: response.updated_at }),
    pendingWaitpoint: pendingWaitpointFromResponse(response.id, response.pending_waitpoint),
    ...("output" in response ? { output: response.output as TOutput } : {}),
  })
}

function taskStartFromResponse<TOutput = unknown>(response: TaskStartResponse): TaskStartResult<TOutput> {
  return {
    session: taskSessionFromResponse<TOutput>(response.session),
    run: runHandle<TOutput>(response.run.id, response.run.task_id),
    isCached: response.is_cached ?? false,
  }
}

function taskSessionFromResponse<TOutput = unknown>(response: TaskSessionResponse): TaskSessionSnapshot<TOutput> {
  return {
    id: response.id,
    projectId: response.project_id,
    environmentId: response.environment_id,
    taskId: response.task_id,
    initialDeploymentId: response.initial_deployment_id,
    activeDeploymentId: response.active_deployment_id,
    ...(response.external_id === undefined || response.external_id === "" ? {} : { externalId: response.external_id }),
    status: response.status,
    currentRunId: response.current_run_id ?? null,
    workspaceId: response.workspace_id ?? null,
    metadata: response.metadata ?? {},
    tags: response.tags ?? [],
    ...("result" in response ? { result: response.result as TOutput } : {}),
    ...("error" in response ? { error: response.error } : {}),
    timedOut: response.timed_out ?? false,
    ...("terminal_reason" in response ? { terminalReason: response.terminal_reason } : {}),
    expiresAt: response.expires_at ?? null,
    createdAt: response.created_at,
    updatedAt: response.updated_at,
  }
}

function taskSessionRunFromResponse(response: TaskSessionRunResponse): TaskSessionRun {
  return {
    id: response.id,
    runId: response.run_id,
    deploymentId: response.deployment_id,
    ...(response.previous_run_id === undefined || response.previous_run_id === "" ? {} : { previousRunId: response.previous_run_id }),
    turnIndex: response.turn_index,
    status: response.status,
    executionStatus: response.execution_status,
    ...(response.terminal_outcome === undefined || response.terminal_outcome === "" ? {} : { terminalOutcome: response.terminal_outcome }),
    createdAt: response.created_at,
    ...(response.ended_at === undefined ? {} : { endedAt: response.ended_at }),
  }
}

function channelRecordFromResponse<TData = unknown>(response: ChannelRecordResponse): ChannelRecord<TData> {
  return {
    id: response.id,
    channelId: response.channel_id,
    sequence: response.sequence,
    data: response.data as TData,
    ...(response.correlation_id === undefined || response.correlation_id === "" ? {} : { correlationId: response.correlation_id }),
    contentType: response.content_type,
    createdAt: response.created_at,
  }
}

function appendChannelRecordFromResponse<TData = unknown>(
  response: AppendChannelRecordResponse,
): SessionChannelInputSendResult<TData> {
  return {
    ...channelRecordFromResponse<TData>(response.record),
    idempotencyStatus: response.idempotency_status === "duplicate" ? "duplicate" : "created",
  }
}

function sessionId<TOutput>(idOrHandle: string | TaskSessionHandle<TOutput>): string {
  return typeof idOrHandle === "string" ? idOrHandle : idOrHandle.id
}

function taskStartBody(
  payload: unknown,
  opts: TaskStartOptions<SecretDecls>,
  maxDurationSeconds?: number,
): Record<string, unknown> {
  const runOptions = {
    ...(opts.queue === undefined ? {} : { queue: { name: opts.queue } }),
    ...(opts.concurrencyKey === undefined ? {} : { concurrency_key: opts.concurrencyKey }),
    ...(opts.priority === undefined ? {} : { priority: opts.priority }),
    ...(opts.ttl === undefined ? {} : { ttl: opts.ttl }),
    ...(opts.retry === undefined ? {} : { retry: opts.retry }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
    ...(opts.tags === undefined ? {} : { tags: opts.tags }),
    ...(opts.expiresAt === undefined ? {} : { expires_at: isoDateString(opts.expiresAt, "expiresAt") }),
    ...(opts.workspaceId === undefined ? {} : { workspace_id: opts.workspaceId }),
    ...(maxDurationSeconds === undefined ? {} : { max_duration_seconds: maxDurationSeconds }),
    ...taskStartIdempotencyRequestFields(opts.idempotencyKey, opts.idempotencyKeyTTL),
  }
  return {
    ...(opts.projectId === undefined ? {} : { project_id: opts.projectId }),
    ...(opts.environmentId === undefined ? {} : { environment_id: opts.environmentId }),
    ...(payload === undefined ? {} : { payload }),
    ...(opts.externalId === undefined ? {} : { external_id: opts.externalId }),
    ...(Object.keys(runOptions).length === 0 ? {} : { options: runOptions }),
  }
}

function taskSessionWaitBody(opts: TaskSessionWaitOptions): Record<string, unknown> {
  return {
    ...(opts.timeoutSeconds === undefined ? {} : { timeout_seconds: opts.timeoutSeconds }),
  }
}

function channelInputAppendBody(data: unknown, opts: SessionChannelInputSendOptions): Record<string, unknown> {
  return {
    data,
    ...(opts.correlationId === undefined ? {} : { correlation_id: opts.correlationId }),
    ...(opts.externalEventId === undefined ? {} : { external_event_id: opts.externalEventId }),
  }
}

function taskSessionListQuery(opts: TaskSessionListOptions): string {
  const query = new URLSearchParams()
  if (opts.projectId !== undefined) query.set("project_id", opts.projectId)
  if (opts.environmentId !== undefined) query.set("environment_id", opts.environmentId)
  if (opts.status !== undefined) query.set("status", opts.status)
  if (opts.taskId !== undefined) query.set("task_id", opts.taskId)
  if (opts.limit !== undefined) query.set("limit", String(opts.limit))
  return query.size === 0 ? "" : `?${query}`
}

function sessionChannelQuery(opts: SessionChannelListOptions): string {
  const query = new URLSearchParams()
  if (opts.cursor !== undefined) query.set("after_sequence", String(opts.cursor))
  if (opts.limit !== undefined) query.set("limit", String(opts.limit))
  if (opts.correlationId !== undefined) query.set("correlation_id", opts.correlationId)
  return query.size === 0 ? "" : `?${query}`
}

function sessionChannelStreamQuery(opts: SessionChannelOutputStreamOptions, cursor = opts.cursor): string {
  const query = new URLSearchParams()
  if (cursor !== undefined) query.set("after_sequence", String(cursor))
  if (opts.correlationId !== undefined) query.set("correlation_id", opts.correlationId)
  return query.size === 0 ? "" : `?${query}`
}

function publicAccessTokenCreateBody(opts: PublicAccessTokenCreateOptions): Record<string, unknown> {
  return {
    scope: {
      type: opts.scope.type,
      session_id: sessionId(opts.scope.sessionId),
      channel: validateChannelName(opts.scope.channel),
      ...(opts.scope.correlationId === undefined ? {} : { correlation_id: opts.scope.correlationId }),
    },
    ...(opts.expiresAt === undefined ? {} : { expires_at: isoDateString(opts.expiresAt, "expiresAt") }),
    ...(opts.maxUses === undefined ? {} : { max_uses: opts.maxUses }),
  }
}

function publicAccessTokenFromResponse(response: PublicAccessTokenResponse): PublicAccessToken {
  return {
    id: response.id,
    publicAccessToken: response.public_access_token,
    scope: {
      type: response.scope.type,
      sessionId: response.scope.session_id,
      channel: response.scope.channel,
      ...(response.scope.correlation_id === undefined ? {} : { correlationId: response.scope.correlation_id }),
    },
    expiresAt: response.expires_at,
    ...(response.max_uses === undefined ? {} : { maxUses: response.max_uses }),
    createdAt: response.created_at,
  }
}

function isoDateString(value: string | Date, label: string): string {
  const date = value instanceof Date ? value : new Date(value)
  if (!Number.isFinite(date.getTime())) {
    throw new Error(`${label} must be a valid date`)
  }
  return date.toISOString()
}

function cancelRunBody(opts: CancelRunOptions): Record<string, unknown> {
  return {
    ...(opts.reason === undefined ? {} : { reason: opts.reason }),
    ...(opts.force === undefined ? {} : { force: opts.force }),
    ...(opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }),
  }
}

function scheduleCreateBody(opts: ScheduleCreateOptions | ScheduleUpdateOptions): Record<string, unknown> {
  return {
    ...("deduplicationKey" in opts && opts.deduplicationKey !== undefined ? { deduplication_key: opts.deduplicationKey } : {}),
    ...(opts.externalId === undefined ? {} : { external_id: opts.externalId }),
    task: opts.task,
    cron: opts.cron,
    ...(opts.timezone === undefined ? {} : { timezone: opts.timezone }),
    ...(opts.active === undefined ? {} : { active: opts.active }),
    ...(opts.options === undefined ? {} : { options: runOptionsBody(opts.options) }),
  }
}

function runOptionsBody(opts: ScheduleRunOptions | undefined): Record<string, unknown> {
  if (opts === undefined) return {}
  return {
    ...(opts.queue === undefined ? {} : { queue: { name: opts.queue } }),
    ...(opts.concurrencyKey === undefined ? {} : { concurrency_key: opts.concurrencyKey }),
    ...(opts.priority === undefined ? {} : { priority: opts.priority }),
    ...(opts.ttl === undefined ? {} : { ttl: opts.ttl }),
    ...(opts.maxDurationSeconds === undefined ? {} : { max_duration_seconds: opts.maxDurationSeconds }),
  }
}

function workspaceId(idOrHandle: string | WorkspaceHandle): string {
  return typeof idOrHandle === "string" ? idOrHandle : idOrHandle.id
}

function workspaceCollectionPath(opts: { readonly projectId?: string; readonly environmentId?: string }): string {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    if (opts.projectId === undefined || opts.environmentId === undefined) {
      throw new Error("projectId and environmentId must be provided together")
    }
    return `/api/projects/${encodeURIComponent(opts.projectId)}/environments/${encodeURIComponent(opts.environmentId)}/workspaces`
  }
  return "/api/workspaces"
}

function workspaceResourcePath(id: string, opts: { readonly projectId?: string; readonly environmentId?: string }): string {
  return `${workspaceCollectionPath(opts)}/${encodeURIComponent(id)}`
}

function workspaceCreateBody(opts: WorkspaceCreateOptions): Record<string, unknown> {
  return {
    ...(opts.projectId === undefined ? {} : { project_id: opts.projectId }),
    ...(opts.environmentId === undefined ? {} : { environment_id: opts.environmentId }),
    sandbox_id: opts.sandboxId,
    ...(opts.deploymentId === undefined ? {} : { deployment_id: opts.deploymentId }),
    ...(opts.externalId === undefined ? {} : { external_id: opts.externalId }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
    ...(opts.tags === undefined ? {} : { tags: opts.tags }),
    ...(opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }),
    ...(opts.idempotencyKeyTTL === undefined ? {} : { idempotency_key_ttl: opts.idempotencyKeyTTL }),
  }
}

function workspaceUpdateBody(opts: WorkspaceUpdateOptions): Record<string, unknown> {
  return {
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
    ...(opts.tags === undefined ? {} : { tags: opts.tags }),
  }
}

function workspaceMaterializeBody(opts: WorkspaceMaterializeOptions): Record<string, unknown> {
  return {
    ...(opts.projectId === undefined ? {} : { project_id: opts.projectId }),
    ...(opts.environmentId === undefined ? {} : { environment_id: opts.environmentId }),
  }
}

function workspaceExecCreateBody(command: readonly string[], opts: WorkspaceExecCreateOptions): Record<string, unknown> {
  if (command.length === 0) {
    throw new Error("workspace.exec command must not be empty")
  }
  return {
    command: [...command],
    ...(opts.cwd === undefined ? {} : { cwd: opts.cwd }),
    ...(opts.env === undefined ? {} : { env: opts.env }),
    ...(opts.detached === undefined ? {} : { detached: opts.detached }),
    ...(opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }),
  }
}

function workspacePtyCreateBody(opts: WorkspacePtyCreateOptions): Record<string, unknown> {
  return {
    ...(opts.cwd === undefined ? {} : { cwd: opts.cwd }),
    ...(opts.cols === undefined ? {} : { cols: opts.cols }),
    ...(opts.rows === undefined ? {} : { rows: opts.rows }),
    ...(opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }),
  }
}

function workspaceStreamWriteBody(data: string | Uint8Array, opts: WorkspaceStreamWriteOptions): Record<string, unknown> {
  if (opts.offset < 0 || !Number.isSafeInteger(opts.offset)) {
    throw new Error("workspace stream offset must be a non-negative integer")
  }
  return {
    offset: opts.offset,
    data: base64Encode(data),
  }
}

function workspaceListQuery(opts: WorkspaceListOptions): string {
  const query = new URLSearchParams()
  if (opts.projectId !== undefined) query.set("project_id", opts.projectId)
  if (opts.environmentId !== undefined) query.set("environment_id", opts.environmentId)
  if (opts.state !== undefined) query.set("state", opts.state)
  if (opts.externalId !== undefined) query.set("external_id", opts.externalId)
  if (opts.tag !== undefined) query.set("tag", opts.tag)
  if (opts.limit !== undefined) query.set("limit", String(opts.limit))
  return query.size === 0 ? "" : `?${query}`
}

function workspacePrimitiveListQuery(opts: WorkspaceExecListOptions | WorkspacePtyListOptions): string {
  const query = new URLSearchParams()
  if (opts.state !== undefined) query.set("state", opts.state)
  if (opts.limit !== undefined) query.set("limit", String(opts.limit))
  return query.size === 0 ? "" : `?${query}`
}

function workspaceStreamListQuery(opts: WorkspaceStreamListOptions): string {
  const query = new URLSearchParams()
  if (opts.cursor !== undefined) query.set("cursor", String(opts.cursor))
  if (opts.limit !== undefined) query.set("limit", String(opts.limit))
  return query.size === 0 ? "" : `?${query}`
}

function workspaceFromResponse(response: WorkspaceResponse): Workspace {
  return {
    id: response.id,
    projectId: response.project_id,
    environmentId: response.environment_id,
    deploymentSandboxId: response.deployment_sandbox_id,
    sandboxId: response.sandbox_id,
    sandboxFingerprint: response.sandbox_fingerprint,
    ...(response.external_id === undefined || response.external_id === "" ? {} : { externalId: response.external_id }),
    currentVersionId: response.current_version_id ?? null,
    state: response.state,
    desiredState: response.desired_state,
    dirtyState: response.dirty_state,
    lastMaterializationId: response.last_materialization_id ?? null,
    metadata: response.metadata ?? {},
    tags: response.tags ?? [],
    autoStopAt: response.auto_stop_at ?? null,
    autoArchiveAt: response.auto_archive_at ?? null,
    autoDeleteAt: response.auto_delete_at ?? null,
    lastActivityAt: response.last_activity_at,
    createdAt: response.created_at,
    updatedAt: response.updated_at,
    archivedAt: response.archived_at ?? null,
    deletedAt: response.deleted_at ?? null,
  }
}

function workspaceMaterializationFromResponse(response: WorkspaceMaterializationResponse): WorkspaceMaterialization {
  return {
    id: response.id,
    projectId: response.project_id,
    environmentId: response.environment_id,
    workspaceId: response.workspace_id,
    deploymentSandboxId: response.deployment_sandbox_id,
    baseVersionId: response.base_version_id ?? null,
    workerInstanceId: response.worker_instance_id ?? null,
    state: response.state,
    fencingGeneration: response.fencing_generation,
    dirtyGeneration: response.dirty_generation,
    reservationExpiresAt: response.reservation_expires_at ?? null,
    lastHeartbeatAt: response.last_heartbeat_at ?? null,
    createdAt: response.created_at,
    updatedAt: response.updated_at,
  }
}

function workspaceExecFromResponse(response: WorkspaceExecResponse): WorkspaceExec {
  return {
    id: response.id,
    workspaceId: response.workspace_id,
    materializationId: response.materialization_id ?? null,
    command: response.command,
    cwd: response.cwd,
    envShape: response.env_shape ?? {},
    filesystemMode: response.filesystem_mode,
    state: response.state,
    detached: response.detached,
    processId: response.process_id ?? null,
    exitCode: response.exit_code ?? null,
    signal: response.signal ?? null,
    error: response.error ?? {},
    stdoutCursor: response.stdout_cursor,
    stderrCursor: response.stderr_cursor,
    stdinCursor: response.stdin_cursor,
    stdinClosedAt: response.stdin_closed_at ?? null,
    createdAt: response.created_at,
    startedAt: response.started_at ?? null,
    exitedAt: response.exited_at ?? null,
    updatedAt: response.updated_at,
  }
}

function workspacePtyFromResponse(response: WorkspacePtyResponse): WorkspacePty {
  return {
    id: response.id,
    workspaceId: response.workspace_id,
    materializationId: response.materialization_id ?? null,
    cwd: response.cwd,
    cols: response.cols,
    rows: response.rows,
    filesystemMode: response.filesystem_mode,
    state: response.state,
    processId: response.process_id ?? null,
    outputCursor: response.output_cursor,
    inputCursor: response.input_cursor,
    error: response.error ?? {},
    createdAt: response.created_at,
    startedAt: response.started_at ?? null,
    closedAt: response.closed_at ?? null,
    updatedAt: response.updated_at,
  }
}

function workspaceStreamChunkFromResponse(response: WorkspaceExecStreamChunkResponse | WorkspacePtyStreamChunkResponse): WorkspaceStreamChunk {
  return {
    id: response.id,
    stream: response.stream,
    offsetStart: response.offset_start,
    offsetEnd: response.offset_end,
    data: base64Decode(response.data),
    observedAt: response.observed_at,
    createdAt: response.created_at,
  }
}

function workspaceExecTerminal(state: WorkspaceExecState): boolean {
  return state === "exited" || state === "lost" || state === "failed"
}

function scheduleFromResponse(response: ScheduleResponse): Schedule {
  return {
    id: response.id,
    type: response.type,
    projectId: response.project_id,
    environmentId: response.environment_id,
    task: response.task,
    ...(response.deduplication_key === undefined || response.deduplication_key === "" ? {} : { deduplicationKey: response.deduplication_key }),
    ...(response.external_id === undefined || response.external_id === "" ? {} : { externalId: response.external_id }),
    cron: response.cron,
    timezone: response.timezone,
    active: response.active,
    status: response.status,
    ...(response.last_error === undefined || response.last_error === "" ? {} : { lastError: response.last_error }),
    ...(response.next_fire_at === undefined ? {} : { nextFireAt: response.next_fire_at }),
    ...(response.last_fire_at === undefined ? {} : { lastFireAt: response.last_fire_at }),
    createdAt: response.created_at,
    updatedAt: response.updated_at,
  }
}

function runWaitpointListQuery(opts: RunWaitpointListOptions): string {
  const query = new URLSearchParams()
  if (opts.cursor !== undefined) query.set("cursor", opts.cursor)
  if (opts.limit !== undefined) query.set("limit", String(opts.limit))
  if (opts.status !== undefined) query.set("status", opts.status)
  return query.size === 0 ? "" : `?${query}`
}

function waitpointTokenCreateBody(
  opts: WaitpointTokenCreateOptions,
): {
  readonly timeout_in_seconds?: number
  readonly timeout_at?: string
  readonly tags?: readonly string[]
  readonly metadata?: Record<string, unknown>
} {
  return {
    ...(opts.timeoutInSeconds === undefined ? {} : { timeout_in_seconds: opts.timeoutInSeconds }),
    ...(opts.timeoutAt === undefined ? {} : { timeout_at: opts.timeoutAt }),
    ...(opts.tags === undefined ? {} : { tags: opts.tags }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
  }
}

function waitpointTokenCompleteBody(data: unknown, opts: WaitpointTokenCompleteOptions): {
  readonly data: unknown
} {
  void opts
  return {
    data,
  }
}

function waitpointTokenCollectionPath(opts: { readonly projectId?: string; readonly environmentId?: string }): string {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    if (opts.projectId === undefined || opts.environmentId === undefined) {
      throw new Error("projectId and environmentId must be provided together")
    }
    return `/api/projects/${encodeURIComponent(opts.projectId)}/environments/${encodeURIComponent(opts.environmentId)}/waitpoints/tokens`
  }
  return "/api/waitpoints/tokens"
}

function waitpointTokenListQuery(opts: WaitpointTokenListOptions): string {
  const query = new URLSearchParams()
  if (opts.cursor !== undefined) query.set("cursor", opts.cursor)
  if (opts.limit !== undefined) query.set("limit", String(opts.limit))
  if (opts.status !== undefined) query.set("status", opts.status)
  return query.size === 0 ? "" : `?${query}`
}

function waitpointTokenFromResponse(response: WaitpointTokenResponse): WaitpointToken {
  return {
    id: response.id,
    ...(response.status === undefined ? {} : { status: response.status }),
    callbackUrl: response.callback_url,
    ...(response.public_access_token === undefined ? {} : { publicAccessToken: response.public_access_token }),
    timeoutAt: response.timeout_at ?? null,
    ...(response.data === undefined ? {} : { data: response.data }),
    ...(response.tags === undefined ? {} : { tags: response.tags }),
    ...(response.metadata === undefined ? {} : { metadata: response.metadata }),
  }
}

function waitpointTokenPublicAccessToken(target: WaitpointToken | WaitpointTokenRef | string): string | undefined {
  if (typeof target === "string" || !("publicAccessToken" in target)) {
    return undefined
  }
  return target.publicAccessToken
}

function retrieveOptions(signal: AbortSignal | undefined): RetrieveRunOptions {
  return signal === undefined ? {} : { signal }
}

function requestSignal(signal: AbortSignal | undefined): RequestInit {
  return signal === undefined ? {} : { signal }
}

function base64Encode(data: string | Uint8Array): string {
  const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data
  if (typeof Buffer !== "undefined") {
    return Buffer.from(bytes).toString("base64")
  }
  let binary = ""
  for (const byte of bytes) {
    binary += String.fromCharCode(byte)
  }
  return btoa(binary)
}

function base64Decode(data: string): Uint8Array {
  if (typeof Buffer !== "undefined") {
    return new Uint8Array(Buffer.from(data, "base64"))
  }
  const binary = atob(data)
  const bytes = new Uint8Array(binary.length)
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index)
  }
  return bytes
}

function waitSignal(
  signal: AbortSignal | undefined,
  timeoutMs: number | undefined,
  timeoutError: () => Error,
): { readonly signal: AbortSignal | undefined; readonly cleanup: () => void } {
  if (timeoutMs === undefined) {
    return { signal, cleanup: () => {} }
  }

  const controller = new AbortController()
  const abortFromParent = (): void => {
    controller.abort(signal?.reason)
  }
  if (signal?.aborted === true) {
    abortFromParent()
  } else {
    signal?.addEventListener("abort", abortFromParent, { once: true })
  }
  const timeout = setTimeout(() => controller.abort(timeoutError()), timeoutMs)

  return {
    signal: controller.signal,
    cleanup: () => {
      clearTimeout(timeout)
      signal?.removeEventListener("abort", abortFromParent)
    },
  }
}

function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted !== true) return
  if (signal.reason instanceof Error) {
    throw signal.reason
  }
  throw new Error("operation aborted")
}

function delay(ms: number, signal: AbortSignal | undefined): Promise<void> {
  throwIfAborted(signal)
  return new Promise((resolve, reject) => {
    const cleanup = (): void => {
      clearTimeout(timeout)
      signal?.removeEventListener("abort", onAbort)
    }
    const timeout = setTimeout(() => {
      cleanup()
      resolve()
    }, ms)
    const onAbort = (): void => {
      cleanup()
      reject(signal?.reason instanceof Error ? signal.reason : new Error("operation aborted"))
    }
    signal?.addEventListener("abort", onAbort, { once: true })
  })
}

function taskStartPendingRetryDelay(response: Response): number {
  const retryAfter = response.headers.get("retry-after")
  if (retryAfter === null) {
    return TASK_START_PENDING_DEFAULT_RETRY_MS
  }
  const retryAfterSeconds = Number(retryAfter)
  if (Number.isFinite(retryAfterSeconds)) {
    if (retryAfterSeconds > 0) {
      return Math.min(retryAfterSeconds * 1000, TASK_START_PENDING_MAX_WAIT_MS)
    }
    return TASK_START_PENDING_DEFAULT_RETRY_MS
  }
  const retryAt = Date.parse(retryAfter)
  if (Number.isFinite(retryAt)) {
    const delayMs = retryAt - Date.now()
    if (delayMs <= 0) {
      return TASK_START_PENDING_DEFAULT_RETRY_MS
    }
    return Math.min(delayMs, TASK_START_PENDING_MAX_WAIT_MS)
  }
  return TASK_START_PENDING_DEFAULT_RETRY_MS
}

function taskStartPendingResponse(body: string): boolean {
  try {
    const decoded = JSON.parse(body) as { code?: unknown }
    return decoded.code === "idempotency_pending"
  } catch {
    return false
  }
}

class HelmrApiError extends Error {
  readonly status: number

  constructor(status: number, body: string) {
    super(`Helmr API ${status}: ${body}`)
    this.name = "HelmrApiError"
    this.status = status
  }
}

class SseFrameTooLargeError extends Error {
  constructor() {
    super("SSE event exceeded the maximum buffer size")
    this.name = "SseFrameTooLargeError"
  }
}

class SseProtocolError extends Error {
  readonly cursor?: number

  constructor(message: string, cursor?: number) {
    super(message)
    this.name = "SseProtocolError"
    if (cursor !== undefined) {
      this.cursor = cursor
    }
  }
}

async function* parseSse(response: Response): AsyncIterable<RunEventRecord> {
  const reader = response.body?.getReader()
  if (reader === undefined) {
    return
  }
  const decoder = new TextDecoder()
  let buffer = ""
  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done) {
        buffer += decoder.decode()
        const finalEvent = parseSseFrame(buffer)
        if (finalEvent !== undefined) {
          yield finalEvent
        }
        return
      }
      buffer += decoder.decode(value, { stream: true })
      let boundary = findSseBoundary(buffer)
      while (boundary !== -1) {
        const delimiter = buffer.startsWith("\r\n\r\n", boundary) ? 4 : 2
        const raw = buffer.slice(0, boundary)
        buffer = buffer.slice(boundary + delimiter)
        const event = parseSseFrame(raw)
        if (event !== undefined) {
          yield event
        }
        boundary = findSseBoundary(buffer)
      }
      if (buffer.length > MAX_SSE_BUFFER_CHARS) {
        throw new SseFrameTooLargeError()
      }
    }
  } finally {
    reader.releaseLock()
  }
}

async function* parseChannelRecordSse<TData = unknown>(response: Response): AsyncIterable<ChannelRecord<TData>> {
  const reader = response.body?.getReader()
  if (reader === undefined) {
    return
  }
  const decoder = new TextDecoder()
  let buffer = ""
  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done) {
        buffer += decoder.decode()
        const finalRecord = parseChannelRecordSseFrame<TData>(buffer)
        if (finalRecord !== undefined) {
          yield finalRecord
        }
        return
      }
      buffer += decoder.decode(value, { stream: true })
      let boundary = findSseBoundary(buffer)
      while (boundary !== -1) {
        const delimiter = buffer.startsWith("\r\n\r\n", boundary) ? 4 : 2
        const raw = buffer.slice(0, boundary)
        buffer = buffer.slice(boundary + delimiter)
        const record = parseChannelRecordSseFrame<TData>(raw)
        if (record !== undefined) {
          yield record
        }
        boundary = findSseBoundary(buffer)
      }
      if (buffer.length > MAX_SSE_BUFFER_CHARS) {
        throw new SseFrameTooLargeError()
      }
    }
  } finally {
    reader.releaseLock()
  }
}

function parseChannelRecordSseFrame<TData = unknown>(raw: string): ChannelRecord<TData> | undefined {
  const data = raw
    .split(/\r?\n/)
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice(5).trimStart())
    .join("\n")
  if (data === "") {
    return undefined
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(data)
  } catch {
    throw new SseProtocolError("SSE channel record data must be valid JSON", sseFrameCursor(raw))
  }
  const record = objectRecord(parsed)
  if (record === undefined) {
    throw new SseProtocolError("SSE channel record data must be a JSON object", sseFrameCursor(raw))
  }
  return channelRecordFromResponse<TData>(record as unknown as ChannelRecordResponse)
}

function parseSseFrame(raw: string): RunEventRecord | undefined {
  const frameCursor = sseFrameCursor(raw)
  const data = raw
    .split(/\r?\n/)
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice(5).trimStart())
    .join("\n")
  if (data === "") {
    return undefined
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(data)
  } catch {
    throw new SseProtocolError("SSE event data must be valid JSON", frameCursor)
  }
  const record = objectRecord(parsed)
  if (record === undefined) {
    throw new SseProtocolError("SSE event data must be a JSON object", frameCursor)
  }
  const id = stringValue(record["id"])
  if (id === undefined) {
    throw new SseProtocolError("SSE event data must include a string id", frameCursor)
  }
  const eventCursor = parseRunEventCursor(id)
  if (eventCursor === undefined) {
    throw new SseProtocolError("SSE event data id must be a safe numeric string", frameCursor)
  }
  if (stringValue(record["kind"]) === undefined) {
    throw new SseProtocolError("SSE event data must include a string kind", eventCursor)
  }
  if (stringValue(record["message"]) === undefined) {
    throw new SseProtocolError("SSE event data must include a string message", eventCursor)
  }
  if (stringValue(record["at"]) === undefined) {
    throw new SseProtocolError("SSE event data must include a string at", eventCursor)
  }
  return parsed as RunEventRecord
}

function sseFrameCursor(raw: string): number | undefined {
  for (const line of raw.split(/\r?\n/)) {
    if (!line.startsWith("id:")) {
      continue
    }
    return parseRunEventCursor(line.slice(3).trim())
  }
  return undefined
}

function findSseBoundary(buffer: string): number {
  const lf = buffer.indexOf("\n\n")
  const crlf = buffer.indexOf("\r\n\r\n")
  if (lf === -1) return crlf
  if (crlf === -1) return lf
  return Math.min(lf, crlf)
}

function nextRunEventCursor(cursor: number | undefined, event: RunEventRecord): number | undefined {
  const parsed = parseRunEventCursor(event.id)
  if (parsed === undefined) {
    return cursor
  }
  return advanceRunEventCursor(cursor, parsed)
}

function advanceRunEventCursor(cursor: number | undefined, parsed: number): number {
  return cursor === undefined || parsed > cursor ? parsed : cursor
}

function parseRunEventCursor(value: string): number | undefined {
  if (!/^\d+$/.test(value)) {
    return undefined
  }
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) ? parsed : undefined
}

function runEventRecordIsTerminal(event: RunEventRecord): boolean {
  return runEventKindIsTerminal(event.message) || runEventKindIsTerminal(event.kind)
}

function taskSessionTerminal(status: TaskSessionStatus): boolean {
  return status !== "open"
}

function runEventKindIsTerminal(kind: string | undefined): boolean {
  return kind === "run.completed" || kind === "run.failed" || kind === "run.cancelled" || kind === "run.expired"
}

function runEventStreamErrorIsFatal(error: unknown): boolean {
  if (error instanceof AuthError) {
    return true
  }
  if (error instanceof HelmrApiError) {
    return helmrApiErrorIsFatal(error)
  }
  if (error instanceof SyntaxError) {
    return true
  }
  if (error instanceof SseFrameTooLargeError) {
    return true
  }
  if (error instanceof SseProtocolError) {
    return true
  }
  return !transportErrorIsRetryable(error)
}

function runEventWaitStreamErrorIsFatal(error: unknown): boolean {
  // Wait can fall back to snapshots on SSE protocol corruption; subscribe cannot without losing stream fidelity.
  if (error instanceof SyntaxError) {
    return false
  }
  if (error instanceof SseFrameTooLargeError) {
    return false
  }
  if (error instanceof SseProtocolError) {
    return false
  }
  return runEventStreamErrorIsFatal(error)
}

function runSnapshotErrorIsFatal(error: unknown): boolean {
  if (error instanceof AuthError) {
    return true
  }
  if (error instanceof HelmrApiError) {
    return helmrApiErrorIsFatal(error)
  }
  if (error instanceof SyntaxError) {
    return true
  }
  return !transportErrorIsRetryable(error)
}

function helmrApiErrorIsFatal(error: HelmrApiError): boolean {
  return error.status >= 400 && error.status < 500 && error.status !== 408 && error.status !== 429
}

function transportErrorIsRetryable(error: unknown): boolean {
  if (error instanceof TypeError) {
    return true
  }
  if (typeof DOMException !== "undefined" && error instanceof DOMException) {
    return error.name === "NetworkError" || error.name === "AbortError" || error.name === "TimeoutError"
  }
  const record = objectRecord(error)
  const cause = objectRecord(record?.["cause"])
  const code = stringValue(record?.["code"]) ?? stringValue(cause?.["code"])
  if (code === undefined) {
    return false
  }
  return code === "ECONNRESET" || code === "ECONNREFUSED" || code === "EPIPE" || code === "ETIMEDOUT" || code.startsWith("UND_ERR_")
}

function runEventRecordToRunEvent(event: unknown): RunEvent | undefined {
  const record = objectRecord(event)
  const message = stringValue(record?.["message"])
  const at = stringValue(record?.["at"])
  if (record === undefined || message === undefined || at === undefined) {
    return undefined
  }
  const attributes = objectRecord(record["attributes"])
  const runId = stringValue(record["run_id"]) ?? stringValue(attributes?.["run_id"]) ?? ""
  if (message === "log.stdout" || message === "log.stderr") {
    const stream = message === "log.stdout" ? "stdout" : "stderr"
    return {
      type: "log",
      run_id: runId,
      stream,
      bytes: numberValue(attributes?.["bytes"]) ?? 0,
      observed_seq: numberValue(attributes?.["observed_seq"]) ?? 0,
      at,
    }
  }
  if (message === "waitpoint.created") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    const kind = stringValue(attributes?.["kind"])
    if (waitpointId === undefined) return undefined
    if (kind === undefined) return undefined
    return {
      type: "waitpoint",
      run_id: runId,
      waitpoint_id: waitpointId,
      kind,
      params: attributes?.["params"] ?? {},
      metadata: objectRecord(attributes?.["metadata"]) ?? {},
      tags: stringArrayValue(attributes?.["tags"]) ?? [],
      ...optionalNumber("timeout", attributes?.["timeout"]),
      at,
    }
  }
  if (message === "waitpoint.completed") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    const kind = stringValue(attributes?.["kind"])
    if (waitpointId === undefined) return undefined
    if (kind === undefined) return undefined
    return {
      type: "waitpoint_completed",
      run_id: runId,
      waitpoint_id: waitpointId,
      kind,
      payload: attributes?.["payload"],
      at,
    }
  }
  if (message === "waitpoint.timed_out") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    const kind = stringValue(attributes?.["kind"])
    if (waitpointId === undefined || kind === undefined) return undefined
    return {
      type: "waitpoint_timed_out",
      run_id: runId,
      waitpoint_id: waitpointId,
      kind,
      at,
    }
  }
  if (message === "run.completed") {
    return {
      type: "task_result",
      run_id: runId,
      exit_code: numberValue(attributes?.["exit_code"]) ?? 0,
      at,
    }
  }
  if (message === "run.failed") {
    return {
      type: "run_failed",
      run_id: runId,
      failure_kind: stringValue(attributes?.["failure_kind"]) ?? "task_failed",
      detail: attributes?.["detail"],
      at,
    }
  }
  if (message === "run.cancelled") {
    return {
      type: "run_cancelled",
      run_id: runId,
      ...optionalString("reason", attributes?.["reason"]),
      at,
    }
  }
  if (message === "run.expired") {
    return {
      type: "run_expired",
      run_id: runId,
      ...optionalString("ttl", attributes?.["ttl"]),
      ...optionalString("message", attributes?.["message"]),
      at,
    }
  }
  return undefined
}

function optionalString<K extends string>(key: K, value: unknown): { [P in K]?: string } {
  const text = stringValue(value)
  return text === undefined ? {} : ({ [key]: text } as { [P in K]?: string })
}

function optionalNumber<K extends string>(key: K, value: unknown): { [P in K]?: number } {
  return typeof value === "number" ? ({ [key]: value } as { [P in K]?: number }) : {}
}

function objectRecord(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" ? (value as Record<string, unknown>) : undefined
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined
}

function stringArrayValue(value: unknown): readonly string[] | undefined {
  return Array.isArray(value) && value.every((item) => typeof item === "string") ? value : undefined
}

function numberValue(value: unknown): number | undefined {
  return typeof value === "number" ? value : undefined
}

function decodeBase64Text(value: string): string {
  const binary = atob(value)
  const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0))
  return new TextDecoder().decode(bytes)
}
