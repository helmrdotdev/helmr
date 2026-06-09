import {
  parseTaskPayload,
  validateRetryPolicy,
  type AnyTask,
  type NoPayload,
  type SecretDecls,
  type TaskOutput,
  type TaskRunOptions,
  type TaskSecrets,
  type TaskTriggerPayload,
} from "../internal"
import { runIdempotencyRequestFields } from "../idempotency"
import { readOptionalMaxDurationSeconds } from "../schema/task"
import { AuthError, TimeoutError, UnsupportedTransportError } from "./errors"
import { HELMR_API_VERSION, HELMR_API_VERSION_HEADER, HELMR_SDK_VERSION, HELMR_SDK_VERSION_HEADER } from "../version"
import {
  type LogSnapshot,
  type CancelRunOptions,
  type ListRunEventsOptions,
  type ListRunsOptions,
  type PendingHumanWaitpoint,
  type PendingWaitpointResponse,
  type RetrieveRunOptions,
  type RunHandle,
  type RunEvent,
  type RunEventRecord,
  type RunEventRecordPage,
  type RunSnapshot,
  type RunSummary,
  type RunWaitOptions,
  type ReplayRunOptions,
  type SubscribeRunEventsOptions,
  type WaitpointRespondOptions,
  type WaitpointRef,
  isTerminalRunStatus,
  pendingWaitpointFromResponse,
  runHandle,
  runId,
  runSnapshot,
} from "./run"

const MAX_SSE_BUFFER_CHARS = 1024 * 1024

export interface HelmrClientOptions {
  readonly url?: string
  readonly apiKey?: string
}

export type TaskTriggerOptions<TSecrets extends SecretDecls> = TaskRunOptions<TSecrets>

export type TasksTriggerArgs<TTask extends AnyTask> =
  [TaskTriggerPayload<TTask>] extends [NoPayload]
    ? [id: string, opts: TaskTriggerOptions<TaskSecrets<TTask>>]
    : [id: string, payload: TaskTriggerPayload<TTask>, opts: TaskTriggerOptions<TaskSecrets<TTask>>]

export type DirectTaskTriggerArgs<TTask extends AnyTask> =
  [TaskTriggerPayload<TTask>] extends [NoPayload]
    ? [opts: TaskTriggerOptions<TaskSecrets<TTask>>]
    : [payload: TaskTriggerPayload<TTask>, opts: TaskTriggerOptions<TaskSecrets<TTask>>]

export const triggerTaskClientMethod = Symbol.for("helmr.sdk.client.triggerTask")

export interface WaitpointsApi {
  readonly create: (opts: WaitpointCreateOptions) => Promise<Waitpoint>
  readonly respond: {
    (target: PendingHumanWaitpoint | WaitpointRef, opts?: WaitpointRespondOptions): Promise<void>
    (waitpointId: string, opts?: WaitpointRespondOptions): Promise<void>
  }
  readonly tokens: {
    readonly create: {
      (target: PendingHumanWaitpoint | WaitpointRef, opts?: WaitpointTokenCreateOptions): Promise<WaitpointResponseToken>
      (waitpointId: string, opts?: WaitpointTokenCreateOptions): Promise<WaitpointResponseToken>
    }
    readonly respond: {
      (token: WaitpointResponseToken, opts: WaitpointTokenRespondOptions): Promise<void>
      (id: string, token: string, opts: WaitpointTokenRespondOptions): Promise<void>
    }
  }
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
  readonly deploymentId?: string
  readonly version?: string
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
      readonly expiresInSeconds?: number
      readonly expiresAt?: never
    }
  | {
      readonly expiresInSeconds?: never
      readonly expiresAt?: string
    }

export type WaitpointTokenCreateOptions = WaitpointTokenExpirationOptions & {
  readonly metadata?: Record<string, unknown>
}

export interface WaitpointResponseToken {
  readonly id: string
  readonly waitpointId: string
  readonly url: string
  readonly token: string
  readonly expiresAt: string | null
}

export type WaitpointTokenRespondOptions = WaitpointRespondOptions

export interface WaitpointCreateOptions {
  readonly request?: unknown
  readonly displayText?: string
  readonly expiresAt: string
  readonly idempotencyKey?: string
  readonly idempotencyKeyExpiresAt?: string
  readonly idempotencyKeyTTLSeconds?: number
}

export interface Waitpoint {
  readonly id: string
  readonly projectId: string
  readonly environmentId: string
  readonly kind: "human" | "delay"
  readonly status: "pending" | "completed" | "expired" | "cancelled"
  readonly request: unknown
  readonly displayText: string
  readonly expiresAt: string | null
  readonly createdAt: string
}

export class HelmrClient {
  readonly #baseUrl: URL
  readonly #apiKey: string | undefined

  constructor(options: HelmrClientOptions = {}) {
    const rawUrl = options.url ?? process.env["HELMR_URL"]
    if (rawUrl === undefined || rawUrl.trim() === "") {
      throw new UnsupportedTransportError(
        "HelmrClient requires a url option or HELMR_URL; no default transport is used",
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
    trigger: async <TTask extends AnyTask>(
      ...args: TasksTriggerArgs<TTask>
    ): Promise<RunHandle<TaskOutput<TTask>>> => {
      const taskId = args[0]
      const hasPayload = args.length === 3
      const payload = hasPayload ? args[1] : undefined
      const opts = (hasPayload ? args[2] : args[1]) as TaskTriggerOptions<TaskSecrets<TTask>>
      if (hasPayload && payload === undefined) {
        throw new Error(`task ${JSON.stringify(taskId)} requires payload`)
      }
      return await this.#triggerRun(taskId, payload, opts)
    },
  }

  async [triggerTaskClientMethod]<TTask extends AnyTask>(
    task: TTask,
    ...args: DirectTaskTriggerArgs<TTask>
  ): Promise<RunHandle<TaskOutput<TTask>>> {
    const hasPayload = task.payload !== undefined
    const payload = hasPayload ? args[0] : undefined
    const opts = (hasPayload ? args[1] : args[0]) as TaskTriggerOptions<TaskSecrets<TTask>>
    if (task.payload !== undefined) {
      if (payload === undefined) {
        throw new Error(`task ${JSON.stringify(task.id)} requires payload`)
      }
      await parseTaskPayload(task, payload)
    } else if (args.length > 1) {
      throw new Error(`task ${JSON.stringify(task.id)} does not accept payload`)
    }
    return await this.#triggerRun(task.id, payload, opts, readOptionalMaxDurationSeconds(task.maxDuration))
  }

  async #triggerRun<TTask extends AnyTask>(
    taskId: string,
    payload: unknown,
    opts: TaskTriggerOptions<TaskSecrets<TTask>>,
    maxDurationSeconds?: number,
  ): Promise<RunHandle<TaskOutput<TTask>>> {
    validateRetryPolicy(opts.retry, "retry")
    const runOptions = {
      ...(opts.deploymentId === undefined ? {} : { deployment_id: opts.deploymentId }),
      ...(opts.version === undefined ? {} : { version: opts.version }),
      ...(opts.queue === undefined ? {} : { queue: { name: opts.queue } }),
      ...(opts.concurrencyKey === undefined ? {} : { concurrency_key: opts.concurrencyKey }),
      ...(opts.priority === undefined ? {} : { priority: opts.priority }),
      ...(opts.ttl === undefined ? {} : { ttl: opts.ttl }),
      ...(opts.retry === undefined ? {} : { retry: opts.retry }),
      ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
      ...(opts.tags === undefined ? {} : { tags: opts.tags }),
      ...(maxDurationSeconds === undefined ? {} : { max_duration_seconds: maxDurationSeconds }),
      ...runIdempotencyRequestFields(opts.idempotencyKey, opts.idempotencyKeyTTL),
    }
    const response = await this.#fetch("/api/runs", {
      method: "POST",
      body: JSON.stringify({
        task_id: taskId,
        ...(payload === undefined ? {} : { payload }),
        ...(Object.keys(runOptions).length === 0 ? {} : { options: runOptions }),
      }),
      headers: { "content-type": "application/json" },
    })
    const run = (await response.json()) as RunResponse
    return runHandle<TaskOutput<TTask>>(run.id, run.task_id)
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
      opts: RunWaitOptions = {},
    ): Promise<RunSnapshot<TOutput>> => {
      const id = runId(idOrHandle)
      const timeoutMs = opts.timeoutMs
      const intervalMs = opts.intervalMs ?? 1000
      const started = Date.now()
      const wait = waitSignal(opts.signal, timeoutMs, () => new TimeoutError(`run ${id} did not finish within ${timeoutMs}ms`))
      try {
        for (;;) {
          throwIfAborted(wait.signal)
          const run = await this.runs.retrieve<TOutput>(id, retrieveOptions(wait.signal))
          if (isTerminalRunStatus(run.status)) {
            return run
          }
          if (timeoutMs !== undefined && Date.now() - started > timeoutMs) {
            throw new TimeoutError(`run ${id} did not finish within ${timeoutMs}ms`)
          }
          await delay(intervalMs, wait.signal)
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
    replay: async <TPayload = unknown, TOutput = unknown>(
      idOrHandle: string | RunHandle<TOutput>,
      opts: ReplayRunOptions<TPayload> = {},
    ): Promise<RunHandle<TOutput>> => {
      const response = await this.#json<ReplayRunResponse>(
        `/api/runs/${encodeURIComponent(runId(idOrHandle))}/replay`,
        {
          method: "POST",
          body: JSON.stringify(replayRunBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal),
        },
      )
      return runHandle<TOutput>(response.run.id, response.run.task_id)
    },
    list: async (opts: ListRunsOptions = {}): Promise<RunSummary[]> => {
      const query = new URLSearchParams()
      if (opts.status !== undefined) query.set("status", opts.status)
      if (opts.limit !== undefined) query.set("limit", String(opts.limit))
      const suffix = query.size === 0 ? "" : `?${query}`
      const response = await this.#json<ListRunsResponse>(`/api/runs${suffix}`, requestSignal(opts.signal))
      return response.runs.map((run) => runResponseToSnapshot(run))
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

  readonly waitpoints: WaitpointsApi = {
    create: async (opts: WaitpointCreateOptions): Promise<Waitpoint> => {
      const response = await this.#json<WaitpointResponse>("/api/waitpoints", {
        method: "POST",
        body: JSON.stringify(waitpointCreateBody(opts)),
        headers: { "content-type": "application/json" },
      })
      return waitpointFromResponse(response)
    },
    respond: async (
      target: PendingHumanWaitpoint | WaitpointRef | string,
      waitpointIdOrOpts?: WaitpointRespondOptions,
      opts: WaitpointRespondOptions = {},
    ): Promise<void> => {
      const resolved = resolveWaitpointArgs<WaitpointRespondOptions>(target, waitpointIdOrOpts, opts)
      await this.#fetch(
        `/api/waitpoints/${encodeURIComponent(resolved.waitpointId)}/respond`,
        {
          method: "POST",
          body: JSON.stringify(waitpointRespondBody(resolved.opts)),
          headers: { "content-type": "application/json" },
        },
      )
    },
    tokens: {
      create: async (
        target: PendingHumanWaitpoint | WaitpointRef | string,
        waitpointIdOrOpts?: WaitpointTokenCreateOptions,
        opts: WaitpointTokenCreateOptions = {},
      ): Promise<WaitpointResponseToken> => {
        const resolved = resolveWaitpointArgs<WaitpointTokenCreateOptions>(target, waitpointIdOrOpts, opts)
        const response = await this.#json<WaitpointResponseTokenResponse>("/api/waitpoints/tokens", {
          method: "POST",
          body: JSON.stringify(waitpointTokenCreateBody(resolved.waitpointId, resolved.opts)),
          headers: { "content-type": "application/json" },
        })
        return waitpointResponseTokenFromResponse(response)
      },
      respond: async (
        target: WaitpointResponseToken | string,
        tokenOrOpts: string | WaitpointTokenRespondOptions,
        maybeOpts?: WaitpointTokenRespondOptions,
      ): Promise<void> => {
        const resolved =
          typeof target === "string"
            ? resolveWaitpointTokenRespondArgs(target, tokenOrOpts, maybeOpts)
            : { id: target.id, token: target.token, opts: tokenOrOpts as WaitpointTokenRespondOptions }
        await this.#fetch(`/api/waitpoints/tokens/${encodeURIComponent(resolved.id)}/respond`, {
          method: "POST",
          body: JSON.stringify(waitpointTokenRespondBody(resolved.token, resolved.opts)),
          headers: { "content-type": "application/json" },
        })
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
    const query = new URLSearchParams()
    query.set("follow", "1")
    if (opts.cursor !== undefined) query.set("cursor", String(opts.cursor))
    const response = await this.#fetch(`/api/runs/${encodeURIComponent(id)}/events?${query}`, {
      headers: { accept: "text/event-stream" },
      ...requestSignal(opts.signal),
    })
    return parseSse(response)
  }

  async #json<T>(path: string, init: RequestInit = {}): Promise<T> {
    return (await this.#fetch(path, init)).json() as Promise<T>
  }

  async #fetch(path: string, init: RequestInit = {}): Promise<Response> {
    const headers = new Headers(init.headers)
    headers.set(HELMR_API_VERSION_HEADER, HELMR_API_VERSION)
    headers.set(HELMR_SDK_VERSION_HEADER, HELMR_SDK_VERSION)
    if (this.#apiKey !== undefined) {
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
      throw new Error(`Helmr API ${response.status}: ${await response.text()}`)
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
  readonly exit_code?: number | null
  readonly created_at?: string
  readonly updated_at?: string
  readonly pending_waitpoint?: PendingWaitpointResponse | null
  readonly output?: unknown
}

export interface ListRunsResponse {
  readonly runs: readonly RunResponse[]
}

interface ReplayRunResponse {
  readonly run: RunResponse
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

interface WaitpointResponseTokenResponse {
  readonly id: string
  readonly waitpoint_id: string
  readonly url: string
  readonly token: string
  readonly expires_at?: string | null
}

interface WaitpointResponse {
  readonly id: string
  readonly project_id: string
  readonly environment_id: string
  readonly kind: "human" | "delay"
  readonly status: "pending" | "completed" | "expired" | "cancelled"
  readonly request?: unknown
  readonly display_text?: string | null
  readonly expires_at?: string | null
  readonly created_at: string
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
    exitCode: response.exit_code ?? null,
    ...(response.created_at === undefined ? {} : { createdAt: response.created_at }),
    ...(response.updated_at === undefined ? {} : { updatedAt: response.updated_at }),
    pendingWaitpoint: pendingWaitpointFromResponse(response.id, response.pending_waitpoint),
    ...("output" in response ? { output: response.output as TOutput } : {}),
  })
}

function cancelRunBody(opts: CancelRunOptions): Record<string, unknown> {
  return {
    ...(opts.reason === undefined ? {} : { reason: opts.reason }),
    ...(opts.force === undefined ? {} : { force: opts.force }),
    ...(opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }),
  }
}

function replayRunBody<TPayload>(opts: ReplayRunOptions<TPayload>): Record<string, unknown> {
  return {
    ...(opts.version === undefined ? {} : { version: opts.version }),
    ...(opts.payload === undefined ? {} : { payload: opts.payload }),
    ...(opts.reason === undefined ? {} : { reason: opts.reason }),
    ...(opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
    ...(opts.tags === undefined ? {} : { tags: opts.tags }),
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
    ...(opts.deploymentId === undefined ? {} : { deployment_id: opts.deploymentId }),
    ...(opts.version === undefined ? {} : { version: opts.version }),
    ...(opts.queue === undefined ? {} : { queue: { name: opts.queue } }),
    ...(opts.concurrencyKey === undefined ? {} : { concurrency_key: opts.concurrencyKey }),
    ...(opts.priority === undefined ? {} : { priority: opts.priority }),
    ...(opts.ttl === undefined ? {} : { ttl: opts.ttl }),
    ...(opts.maxDurationSeconds === undefined ? {} : { max_duration_seconds: opts.maxDurationSeconds }),
  }
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

function waitpointTokenCreateBody(
  waitpointId: string,
  opts: WaitpointTokenCreateOptions,
): {
  readonly waitpoint_id: string
  readonly expires_in_seconds?: number
  readonly expires_at?: string
  readonly metadata?: Record<string, unknown>
} {
  return {
    waitpoint_id: waitpointId,
    ...(opts.expiresInSeconds === undefined ? {} : { expires_in_seconds: opts.expiresInSeconds }),
    ...(opts.expiresAt === undefined ? {} : { expires_at: opts.expiresAt }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
  }
}

function waitpointCreateBody(opts: WaitpointCreateOptions): {
  readonly request?: unknown
  readonly display_text?: string
  readonly expires_at: string
  readonly idempotency_key?: string
  readonly idempotency_key_expires_at?: string
  readonly idempotency_key_ttl_seconds?: number
} {
  return {
    ...(opts.request === undefined ? {} : { request: opts.request }),
    ...(opts.displayText === undefined ? {} : { display_text: opts.displayText }),
    expires_at: opts.expiresAt,
    ...(opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }),
    ...(opts.idempotencyKeyExpiresAt === undefined ? {} : { idempotency_key_expires_at: opts.idempotencyKeyExpiresAt }),
    ...(opts.idempotencyKeyTTLSeconds === undefined ? {} : { idempotency_key_ttl_seconds: opts.idempotencyKeyTTLSeconds }),
  }
}

function waitpointFromResponse(response: WaitpointResponse): Waitpoint {
  return {
    id: response.id,
    projectId: response.project_id,
    environmentId: response.environment_id,
    kind: response.kind,
    status: response.status,
    request: response.request ?? {},
    displayText: response.display_text ?? "",
    expiresAt: response.expires_at ?? null,
    createdAt: response.created_at,
  }
}

function waitpointRespondBody(opts: WaitpointRespondOptions): {
  readonly value?: unknown
  readonly external_subject?: string
  readonly metadata?: Record<string, unknown>
} {
  return {
    ...("value" in opts ? { value: opts.value } : {}),
    ...(opts.externalSubject === undefined ? {} : { external_subject: opts.externalSubject }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
  }
}

function waitpointTokenRespondBody(token: string, opts: WaitpointTokenRespondOptions): {
  readonly token: string
  readonly value?: unknown
  readonly external_subject?: string
  readonly metadata?: Record<string, unknown>
} {
  return {
    token,
    ...waitpointRespondBody(opts),
  }
}

function resolveWaitpointTokenRespondArgs(
  id: string,
  token: string | WaitpointTokenRespondOptions,
  opts: WaitpointTokenRespondOptions | undefined,
): { readonly id: string; readonly token: string; readonly opts: WaitpointTokenRespondOptions } {
  if (typeof token !== "string" || opts === undefined) {
    throw new Error("waitpoint token secret is required when responding by token id")
  }
  return { id, token, opts }
}

function waitpointResponseTokenFromResponse(response: WaitpointResponseTokenResponse): WaitpointResponseToken {
  return {
    id: response.id,
    waitpointId: response.waitpoint_id,
    url: response.url,
    token: response.token,
    expiresAt: response.expires_at ?? null,
  }
}

function resolveWaitpointArgs<TOpts extends object>(
  target: WaitpointRef | PendingHumanWaitpoint | string,
  waitpointIdOrOpts: TOpts | undefined,
  opts: TOpts | undefined,
): { readonly waitpointId: string; readonly opts: TOpts } {
  if (isWaitpointRef(target)) {
    return {
      waitpointId: target.waitpointId,
      opts: (waitpointIdOrOpts ?? opts ?? {}) as TOpts,
    }
  }
  return {
    waitpointId: target,
    opts: (waitpointIdOrOpts ?? opts ?? {}) as TOpts,
  }
}

function isWaitpointRef(value: unknown): value is WaitpointRef | PendingHumanWaitpoint {
  if (value === null || typeof value !== "object") return false
  const record = value as Record<string, unknown>
  return typeof record["waitpointId"] === "string"
}

function retrieveOptions(signal: AbortSignal | undefined): RetrieveRunOptions {
  return signal === undefined ? {} : { signal }
}

function requestSignal(signal: AbortSignal | undefined): RequestInit {
  return signal === undefined ? {} : { signal }
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

async function* parseSse(response: Response): AsyncIterable<RunEvent> {
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
        throw new Error("SSE event exceeded the maximum buffer size")
      }
    }
  } finally {
    reader.releaseLock()
  }
}

function parseSseFrame(raw: string): RunEvent | undefined {
  const data = raw
    .split(/\r?\n/)
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice(5).trimStart())
    .join("\n")
  if (data === "") {
    return undefined
  }
  try {
    return runEventRecordToRunEvent(JSON.parse(data) as RunEventRecord)
  } catch (error) {
    if (error instanceof SyntaxError) {
      return undefined
    }
    throw error
  }
}

function findSseBoundary(buffer: string): number {
  const lf = buffer.indexOf("\n\n")
  const crlf = buffer.indexOf("\r\n\r\n")
  if (lf === -1) return crlf
  if (crlf === -1) return lf
  return Math.min(lf, crlf)
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
  if (message === "waitpoint.requested") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    const kind = stringValue(attributes?.["kind"])
    if (waitpointId === undefined) return undefined
    if (kind === undefined) return undefined
    return {
      type: "waitpoint_request",
      run_id: runId,
      waitpoint_id: waitpointId,
      kind,
      displayText: stringValue(attributes?.["display_text"]) ?? "",
      request: attributes?.["request"] ?? {},
      ...optionalNumber("timeout", attributes?.["timeout"]),
      at,
    }
  }
  if (message === "waitpoint.resolved") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    const kind = stringValue(attributes?.["kind"])
    const resolution = stringValue(attributes?.["resolution_kind"])
    if (waitpointId === undefined) return undefined
    if (kind === undefined || resolution === undefined) return undefined
    return {
      type: "waitpoint_resolved",
      run_id: runId,
      waitpoint_id: waitpointId,
      kind,
      resolutionKind: resolution,
      value: attributes?.["result"],
      at,
    }
  }
  if (message.startsWith("emit.")) {
    return {
      type: "emit",
      run_id: runId,
      event_type: stringValue(attributes?.["type"]) ?? message.slice("emit.".length),
      content: attributes?.["content"],
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

function numberValue(value: unknown): number | undefined {
  return typeof value === "number" ? value : undefined
}

function decodeBase64Text(value: string): string {
  const binary = atob(value)
  const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0))
  return new TextDecoder().decode(bytes)
}
