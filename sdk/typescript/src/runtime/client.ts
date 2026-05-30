import {
  parseTaskPayload,
  type AnyTask,
  type NoPayload,
  type SecretDecls,
  type TaskOutput,
  type TaskSecrets,
  type TaskTriggerPayload,
  type WorkspaceSpec,
} from "../internal"
import { readOptionalMaxDurationSeconds } from "../schema/task"
import { AuthError, TimeoutError, UnsupportedTransportError } from "./errors"
import {
  type LogSnapshot,
  type ListRunEventsOptions,
  type ListRunsOptions,
  type PendingApprovalWaitpoint,
  type PendingMessageWaitpoint,
  type PendingWaitpoint,
  type PendingWaitpointResponse,
  type RetrieveRunOptions,
  type RunHandle,
  type RunEvent,
  type RunEventRecord,
  type RunEventRecordPage,
  type RunSnapshot,
  type RunSummary,
  type RunWaitOptions,
  type SubscribeRunEventsOptions,
  type WaitpointApprovalOptions,
  type WaitpointRef,
  type WaitpointReplyOptions,
  isTerminalRunStatus,
  pendingWaitpointFromResponse,
  runHandle,
  runId,
  runSnapshot,
} from "./run"
import { runWorkspaceFromSpec } from "./source"

const MAX_SSE_BUFFER_CHARS = 1024 * 1024

export interface HelmrClientOptions {
  readonly url?: string
  readonly apiKey?: string
}

export type TaskTriggerOptions<TPayload, TSecrets extends SecretDecls> = {
  readonly workspace: WorkspaceSpec
} & ([TPayload] extends [NoPayload]
  ? {}
  : {
      /**
       * Payload is audit data: Helmr persists it in plaintext in the `run.created`
       * event, DB, and events stream. Do not put secret values (tokens, API keys,
       * credentials, or PII) in payload; use `secrets:` instead. Use payload for
       * business context such as PR numbers, repo names, ticket ids, and other
       * identifiers.
       */
      readonly payload: TPayload
    }) & ([keyof TSecrets] extends [never]
  ? { readonly secrets?: Record<never, never> }
  : { readonly secrets: { readonly [K in keyof TSecrets]: string } })

export interface WaitpointsApi {
  readonly approve: {
    (target: PendingApprovalWaitpoint, opts?: WaitpointApprovalOptions): Promise<void>
    (runId: string, waitpointId: string, opts?: WaitpointApprovalOptions): Promise<void>
  }
  readonly deny: {
    (target: PendingApprovalWaitpoint, opts?: WaitpointApprovalOptions): Promise<void>
    (runId: string, waitpointId: string, opts?: WaitpointApprovalOptions): Promise<void>
  }
  readonly reply: {
    (target: PendingMessageWaitpoint, opts: WaitpointReplyOptions): Promise<void>
    (runId: string, waitpointId: string, opts: WaitpointReplyOptions): Promise<void>
  }
  readonly tokens: {
    readonly create: {
      (target: PendingWaitpoint | WaitpointRef, opts?: WaitpointTokenCreateOptions): Promise<WaitpointResponseToken>
      (runId: string, waitpointId: string, opts?: WaitpointTokenCreateOptions): Promise<WaitpointResponseToken>
    }
    readonly complete: {
      (token: WaitpointResponseToken, opts: WaitpointTokenCompleteOptions): Promise<void>
      (id: string, token: string, opts: WaitpointTokenCompleteOptions): Promise<void>
    }
  }
}

export type WaitpointTokenAction = "approve" | "deny" | "message" | "reply"

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
  readonly actions?: readonly WaitpointTokenAction[]
  readonly metadata?: Record<string, unknown>
}

export interface WaitpointResponseToken {
  readonly id: string
  readonly runId: string
  readonly waitpointId: string
  readonly url: string
  readonly token: string
  readonly expiresAt: string | null
}

export type WaitpointTokenCompleteOptions =
  | {
      readonly action: "approve"
      readonly reason?: string
      readonly externalSubject?: string
      readonly metadata?: Record<string, unknown>
    }
  | {
      readonly action: "deny"
      readonly reason?: string
      readonly externalSubject?: string
      readonly metadata?: Record<string, unknown>
    }
  | {
      readonly action: "message" | "reply"
      readonly text: string
      readonly externalSubject?: string
      readonly metadata?: Record<string, unknown>
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
      task: TTask,
      opts: TaskTriggerOptions<TaskTriggerPayload<TTask>, TaskSecrets<TTask>>,
    ): Promise<RunHandle<TaskOutput<TTask>>> => {
      const payload = "payload" in opts ? (opts as { readonly payload: unknown }).payload : undefined
      if (task.payloadSchema !== undefined) {
        if (payload === undefined) {
          throw new Error(`task ${JSON.stringify(task.id)} requires payload`)
        }
        await parseTaskPayload(task, payload)
      } else if ("payload" in opts) {
        throw new Error(`task ${JSON.stringify(task.id)} does not accept payload`)
      }
      const runWorkspace = runWorkspaceFromSpec(opts.workspace)
      const maxDurationSeconds = readOptionalMaxDurationSeconds(task.maxDuration)
      const response = await this.#fetch("/api/runs", {
        method: "POST",
        body: JSON.stringify({
          task_id: task.id,
          secrets: opts.secrets ?? {},
          ...(payload === undefined ? {} : { payload }),
          workspace: runWorkspace,
          max_duration_seconds: maxDurationSeconds,
        }),
        headers: { "content-type": "application/json" },
      })
      const run = (await response.json()) as RunResponse
      return runHandle<TaskOutput<TTask>>(run.id, run.task_id)
    },
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
    list: async (opts: ListRunsOptions = {}): Promise<RunSummary[]> => {
      const query = new URLSearchParams()
      if (opts.status !== undefined) query.set("status", opts.status)
      if (opts.limit !== undefined) query.set("limit", String(opts.limit))
      if (opts.projectId !== undefined) query.set("project_id", opts.projectId)
      if (opts.environmentId !== undefined) query.set("environment_id", opts.environmentId)
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
    approve: async (
      target: PendingApprovalWaitpoint | string,
      waitpointIdOrOpts?: string | WaitpointApprovalOptions,
      opts: WaitpointApprovalOptions = {},
    ): Promise<void> => {
      const resolved = resolveWaitpointArgs<WaitpointApprovalOptions>(target, waitpointIdOrOpts, opts)
      await this.#fetch(
        `/api/runs/${encodeURIComponent(resolved.runId)}/waitpoints/${encodeURIComponent(resolved.waitpointId)}/approve`,
        {
          method: "POST",
          body: JSON.stringify(approvalBody(resolved.opts)),
          headers: { "content-type": "application/json" },
        },
      )
    },
    deny: async (
      target: PendingApprovalWaitpoint | string,
      waitpointIdOrOpts?: string | WaitpointApprovalOptions,
      opts: WaitpointApprovalOptions = {},
    ): Promise<void> => {
      const resolved = resolveWaitpointArgs<WaitpointApprovalOptions>(target, waitpointIdOrOpts, opts)
      await this.#fetch(
        `/api/runs/${encodeURIComponent(resolved.runId)}/waitpoints/${encodeURIComponent(resolved.waitpointId)}/deny`,
        {
          method: "POST",
          body: JSON.stringify(approvalBody(resolved.opts)),
          headers: { "content-type": "application/json" },
        },
      )
    },
    reply: async (
      target: PendingMessageWaitpoint | string,
      waitpointIdOrOpts: string | WaitpointReplyOptions,
      opts?: WaitpointReplyOptions,
    ): Promise<void> => {
      const resolved = resolveWaitpointArgs<WaitpointReplyOptions>(target, waitpointIdOrOpts, opts)
      await this.#fetch(
        `/api/runs/${encodeURIComponent(resolved.runId)}/waitpoints/${encodeURIComponent(resolved.waitpointId)}/message`,
        {
          method: "POST",
          body: JSON.stringify({ text: resolved.opts.text, attachments: [] }),
          headers: { "content-type": "application/json" },
        },
      )
    },
    tokens: {
      create: async (
        target: PendingWaitpoint | WaitpointRef | string,
        waitpointIdOrOpts?: string | WaitpointTokenCreateOptions,
        opts: WaitpointTokenCreateOptions = {},
      ): Promise<WaitpointResponseToken> => {
        const resolved = resolveWaitpointArgs<WaitpointTokenCreateOptions>(target, waitpointIdOrOpts, opts)
        const response = await this.#json<WaitpointResponseTokenResponse>("/api/waitpoints/tokens", {
          method: "POST",
          body: JSON.stringify(waitpointTokenCreateBody(resolved.runId, resolved.waitpointId, resolved.opts)),
          headers: { "content-type": "application/json" },
        })
        return waitpointResponseTokenFromResponse(response)
      },
      complete: async (
        target: WaitpointResponseToken | string,
        tokenOrOpts: string | WaitpointTokenCompleteOptions,
        maybeOpts?: WaitpointTokenCompleteOptions,
      ): Promise<void> => {
        const resolved =
          typeof target === "string"
            ? resolveWaitpointTokenCompleteArgs(target, tokenOrOpts, maybeOpts)
            : { id: target.id, token: target.token, opts: tokenOrOpts as WaitpointTokenCompleteOptions }
        await this.#fetch(`/api/waitpoints/tokens/${encodeURIComponent(resolved.id)}/complete`, {
          method: "POST",
          body: JSON.stringify(waitpointTokenCompleteBody(resolved.token, resolved.opts)),
          headers: { "content-type": "application/json" },
        })
      },
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
  readonly task_id: string
  readonly status: string
  readonly exit_code?: number | null
  readonly created_at?: string
  readonly updated_at?: string
  readonly pending_wait?: PendingWaitpointResponse | null
  readonly output?: unknown
}

export interface ListRunsResponse {
  readonly runs: readonly RunResponse[]
}

interface LogSnapshotResponse {
  readonly stdout_base64: string
  readonly stderr_base64: string
  readonly cursor: string
  readonly truncated: boolean
}

interface WaitpointResponseTokenResponse {
  readonly id: string
  readonly run_id: string
  readonly waitpoint_id: string
  readonly url: string
  readonly token: string
  readonly expires_at?: string | null
}

function runResponseToSnapshot<TOutput = unknown>(response: RunResponse): RunSnapshot<TOutput> {
  return runSnapshot<TOutput>({
    id: response.id,
    taskId: response.task_id,
    status: response.status,
    exitCode: response.exit_code ?? null,
    ...(response.created_at === undefined ? {} : { createdAt: response.created_at }),
    ...(response.updated_at === undefined ? {} : { updatedAt: response.updated_at }),
    pendingWaitpoint: pendingWaitpointFromResponse(response.id, response.pending_wait),
    ...("output" in response ? { output: response.output as TOutput } : {}),
  })
}

function approvalBody(opts: WaitpointApprovalOptions): { reason?: string } {
  return opts.reason === undefined ? {} : { reason: opts.reason }
}

function waitpointTokenCreateBody(
  runId: string,
  waitpointId: string,
  opts: WaitpointTokenCreateOptions,
): {
  readonly run_id: string
  readonly waitpoint_id: string
  readonly actions?: readonly WaitpointTokenAction[]
  readonly expires_in_seconds?: number
  readonly expires_at?: string
  readonly metadata?: Record<string, unknown>
} {
  return {
    run_id: runId,
    waitpoint_id: waitpointId,
    ...(opts.actions === undefined ? {} : { actions: opts.actions }),
    ...(opts.expiresInSeconds === undefined ? {} : { expires_in_seconds: opts.expiresInSeconds }),
    ...(opts.expiresAt === undefined ? {} : { expires_at: opts.expiresAt }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
  }
}

function waitpointTokenCompleteBody(token: string, opts: WaitpointTokenCompleteOptions): {
  readonly token: string
  readonly action: "approve" | "deny" | "message" | "reply"
  readonly reason?: string
  readonly text?: string
  readonly external_subject?: string
  readonly metadata?: Record<string, unknown>
} {
  return {
    token,
    action: opts.action,
    ...("reason" in opts && opts.reason === undefined ? {} : "reason" in opts ? { reason: opts.reason } : {}),
    ...("text" in opts ? { text: opts.text } : {}),
    ...(opts.externalSubject === undefined ? {} : { external_subject: opts.externalSubject }),
    ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
  }
}

function resolveWaitpointTokenCompleteArgs(
  id: string,
  token: string | WaitpointTokenCompleteOptions,
  opts: WaitpointTokenCompleteOptions | undefined,
): { readonly id: string; readonly token: string; readonly opts: WaitpointTokenCompleteOptions } {
  if (typeof token !== "string" || opts === undefined) {
    throw new Error("waitpoint token secret is required when completing by token id")
  }
  return { id, token, opts }
}

function waitpointResponseTokenFromResponse(response: WaitpointResponseTokenResponse): WaitpointResponseToken {
  return {
    id: response.id,
    runId: response.run_id,
    waitpointId: response.waitpoint_id,
    url: response.url,
    token: response.token,
    expiresAt: response.expires_at ?? null,
  }
}

function resolveWaitpointArgs<TOpts extends object>(
  target: WaitpointRef | string,
  waitpointIdOrOpts: string | TOpts | undefined,
  opts: TOpts | undefined,
): { readonly runId: string; readonly waitpointId: string; readonly opts: TOpts } {
  if (isWaitpointRef(target)) {
    const resolvedOpts =
      waitpointIdOrOpts !== undefined && typeof waitpointIdOrOpts !== "string"
        ? waitpointIdOrOpts
        : opts
    return {
      runId: target.runId,
      waitpointId: target.waitpointId,
      opts: (resolvedOpts ?? {}) as TOpts,
    }
  }
  if (typeof waitpointIdOrOpts !== "string") {
    throw new Error("waitpoint id is required when resolving a waitpoint by run id")
  }
  return {
    runId: target,
    waitpointId: waitpointIdOrOpts,
    opts: (opts ?? {}) as TOpts,
  }
}

function isWaitpointRef(value: unknown): value is WaitpointRef {
  if (value === null || typeof value !== "object") return false
  const record = value as Record<string, unknown>
  return typeof record["runId"] === "string" && typeof record["waitpointId"] === "string"
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
  if (message === "waitpoint.requested" && stringValue(attributes?.["kind"]) === "approval") {
    const message = stringValue(attributes?.["display_text"])
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    if (waitpointId === undefined) return undefined
    return {
      type: "approval_request",
      run_id: runId,
      waitpoint_id: waitpointId,
      message: message ?? "",
      ...optionalNumber("timeout", attributes?.["timeout"]),
      at,
    }
  }
  if (message === "waitpoint.resolved" && stringValue(attributes?.["kind"]) === "approval") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    const resolution = stringValue(attributes?.["resolution_kind"])
    if (waitpointId === undefined) return undefined
    if (resolution !== "approved" && resolution !== "denied") return undefined
    return {
      type: "approval_decided",
      run_id: runId,
      waitpoint_id: waitpointId,
      decision: resolution,
      ...optionalString("reason", attributes?.["reason"]),
      at,
    }
  }
  if (message === "waitpoint.requested" && stringValue(attributes?.["kind"]) === "message") {
    const request = objectRecord(attributes?.["request"])
    const message = stringValue(attributes?.["display_text"]) ?? stringValue(request?.["prompt"])
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    if (waitpointId === undefined) return undefined
    return {
      type: "message_request",
      run_id: runId,
      waitpoint_id: waitpointId,
      ...optionalString("prompt", message),
      ...optionalNumber("timeout", attributes?.["timeout"]),
      at,
    }
  }
  if (message === "waitpoint.resolved" && stringValue(attributes?.["kind"]) === "message") {
    const result = objectRecord(attributes?.["result"])
    const text = stringValue(result?.["text"])
    const waitpointId = stringValue(attributes?.["waitpoint_id"])
    if (waitpointId === undefined) return undefined
    if (stringValue(attributes?.["resolution_kind"]) !== "replied") return undefined
    return {
      type: "message_received",
      run_id: runId,
      waitpoint_id: waitpointId,
      text: text ?? "",
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
      type: "task_complete",
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
  if (message === "run.timeout") {
    return {
      type: "run_timeout",
      run_id: runId,
      elapsed_secs: numberValue(attributes?.["elapsed_active_secs"]) ?? 0,
      limit_secs: numberValue(attributes?.["limit_secs"]) ?? 0,
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
