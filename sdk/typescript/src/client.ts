import type {
  CursorPage,
  JsonValue,
  Metadata,
  RunError,
  RunHandle,
  RunSnapshot,
  RunStatus,
  TaskDefinition,
  TaskHasPayload,
  TaskOutput,
  TaskPayloadInput,
  TaskResult,
  RunOptions,
  WorkspaceTarget,
  TaskWait,
} from "./contract"
import { runtimeOperationsInstalled } from "./internal/runtime"
import { validateTaskId } from "./schema/task"
import type {
  TokenCreateOptions,
  TokenSnapshot,
} from "./tokens"
import {
  HELMR_API_VERSION,
  HELMR_API_VERSION_HEADER,
  HELMR_SDK_VERSION,
  HELMR_SDK_VERSION_HEADER,
} from "./version"
import {
  createClientWorkspaces,
  type ClientWorkspacesApi,
} from "./workspace"
import {
  createClientActors,
  type ClientActorsApi,
} from "./client-actor"
import {
  createClientDeployments,
  type ClientDeploymentsApi,
} from "./client-deployment"
import {
  createClientSchedules,
  type ClientSchedulesApi,
} from "./client-schedule"
import {
  createClientSecrets,
  type ClientSecretsApi,
} from "./client-secret"
import type { RequestOptions } from "./request"

export interface HelmrClientOptions {
  readonly url: string
  readonly apiKey: string
  readonly fetch?: typeof fetch
}

export type ClientTokenCreateRequest = TokenCreateOptions

export interface ClientTokenCreateResult extends TokenSnapshot {
  readonly status: "pending"
  readonly callbackUrl: string
  readonly publicAccessToken: string
}

export interface ClientTokensApi {
  create(
    request?: ClientTokenCreateRequest,
    options?: RequestOptions,
  ): Promise<ClientTokenCreateResult>
  retrieve(id: string, options?: RequestOptions): Promise<TokenSnapshot>
  list(query?: Readonly<{
    cursor?: string
    limit?: number
  }>, options?: RequestOptions): Promise<CursorPage<TokenSnapshot>>
  complete(id: string, request: Readonly<{
    result: JsonValue
    idempotencyKey?: string
  }>, options?: RequestOptions): Promise<TokenSnapshot>
  cancel(id: string, request?: Readonly<{
    idempotencyKey?: string
  }>, options?: RequestOptions): Promise<TokenSnapshot>
}

type ClientTaskRunRequest = RunOptions & Readonly<{
  idempotencyKey?: string
  workspace: WorkspaceTarget
}>

export type ClientTaskStartRequest<TTask extends TaskDefinition> =
  TaskHasPayload<TTask> extends true
    ? ClientTaskRunRequest & Readonly<{ payload: TaskPayloadInput<TTask> }>
    : ClientTaskRunRequest & Readonly<{ payload?: never }>

export interface ClientTasksApi {
  start<TTask extends TaskDefinition>(
    taskDeclaredId: string,
    request: ClientTaskStartRequest<TTask>,
    options?: RequestOptions,
  ): Promise<RunHandle>
}

export interface ClientRunListQuery {
  readonly status?: RunStatus | readonly RunStatus[]
  readonly cursor?: string
  readonly limit?: number
}

export interface ClientRunsApi {
  retrieve<TOutput extends JsonValue = JsonValue>(
    runId: string,
    options?: RequestOptions,
  ): Promise<RunSnapshot<TOutput>>
  list(
    query?: ClientRunListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<RunSnapshot<JsonValue>>>
  cancel(
    runId: string,
    options?: RequestOptions,
  ): Promise<RunSnapshot<JsonValue>>
  wait<TTask extends TaskDefinition>(
    runId: string,
    options?: RequestOptions,
  ): TaskWait<TaskOutput<TTask>>
}

export class HelmrClient {
  readonly tasks: ClientTasksApi
  readonly actors: ClientActorsApi
  readonly deployments: ClientDeploymentsApi
  readonly schedules: ClientSchedulesApi
  readonly secrets: ClientSecretsApi
  readonly runs: ClientRunsApi
  readonly tokens: ClientTokensApi
  readonly workspaces: ClientWorkspacesApi

  constructor(options: HelmrClientOptions) {
    const transport = new ClientTransport(options)
    this.tasks = Object.freeze(new ClientTasks(transport))
    this.actors = createClientActors(transport)
    this.deployments = createClientDeployments(transport)
    this.schedules = createClientSchedules(transport)
    this.secrets = createClientSecrets(transport)
    this.runs = Object.freeze(new ClientRuns(transport))
    this.tokens = Object.freeze(new ClientTokens(transport))
    this.workspaces = createClientWorkspaces(transport)
  }
}

class ClientTasks implements ClientTasksApi {
  readonly #transport: ClientTransport

  constructor(transport: ClientTransport) {
    this.#transport = transport
  }

  async start<TTask extends TaskDefinition>(
    taskDeclaredId: string,
    request: ClientTaskStartRequest<TTask>,
    options: RequestOptions = {},
  ): Promise<RunHandle> {
    validateTaskId(taskDeclaredId)
    const response = objectValue(
      await this.#transport.request(
        "POST",
        `/api/tasks/${encodeURIComponent(taskDeclaredId)}/start`,
        {
          body: {
            ...("payload" in request ? { payload: request.payload } : {}),
            ...taskStartRequest(request),
          },
          ...(options.signal === undefined
            ? {}
            : { signal: options.signal }),
        },
      ),
      "Task start response",
    )
    const runId = response["run_id"]
    if (typeof runId !== "string" || !/^run_[a-z2-7]{26}$/.test(runId)) {
      throw new Error("Task start response.run_id must be a canonical Run public ID")
    }
    return Object.freeze({ id: runId })
  }
}

class ClientRuns implements ClientRunsApi {
  readonly #transport: ClientTransport

  constructor(transport: ClientTransport) {
    this.#transport = transport
  }

  async retrieve<TOutput extends JsonValue = JsonValue>(
    runId: string,
    options: RequestOptions = {},
  ): Promise<RunSnapshot<TOutput>> {
    return parseRunSnapshot<TOutput>(
      await this.#transport.request(
        "GET",
        `/api/runs/${encodeURIComponent(runID(runId))}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
    )
  }

  async list(
    queryInput: ClientRunListQuery = {},
    options: RequestOptions = {},
  ): Promise<CursorPage<RunSnapshot<JsonValue>>> {
    const query = new URLSearchParams()
    const statuses = queryInput.status === undefined
      ? []
      : Array.isArray(queryInput.status)
      ? queryInput.status
      : [queryInput.status]
    for (const status of statuses) query.append("status", runStatus(status))
    if (queryInput.cursor !== undefined) query.set("cursor", queryInput.cursor)
    if (queryInput.limit !== undefined) query.set("limit", String(queryInput.limit))
    const suffix = query.size === 0 ? "" : `?${query.toString()}`
    const response = objectValue(
      await this.#transport.request("GET", `/api/runs${suffix}`, {
        ...(options.signal === undefined ? {} : { signal: options.signal }),
      }),
      "Run list response",
    )
    if (!Array.isArray(response["runs"])) {
      throw new Error("Run list response.runs must be an array")
    }
    const nextCursor = response["next_cursor"]
    if (nextCursor !== undefined && typeof nextCursor !== "string") {
      throw new Error("Run list response.next_cursor must be a string")
    }
    return Object.freeze({
      items: Object.freeze(response["runs"].map((run) => parseRunSnapshot(run))),
      ...(nextCursor === undefined ? {} : { nextCursor }),
    })
  }

  async cancel(
    runId: string,
    options: RequestOptions = {},
  ): Promise<RunSnapshot<JsonValue>> {
    return parseRunSnapshot(
      await this.#transport.request(
        "POST",
        `/api/runs/${encodeURIComponent(runID(runId))}/cancel`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
    )
  }

  wait<TTask extends TaskDefinition>(
    runId: string,
    options: RequestOptions = {},
  ): TaskWait<TaskOutput<TTask>> {
    if (runtimeOperationsInstalled()) {
      throw new Error(
        "client.runs.wait() is unavailable inside an active Helmr Run; use task.call()",
      )
    }
    const id = runID(runId)
    const result = this.#waitForTerminal<TaskOutput<TTask>>(id, options)
    return Object.freeze({
      then<TResult1 = TaskResult<TaskOutput<TTask>>, TResult2 = never>(
        onfulfilled?: ((value: TaskResult<TaskOutput<TTask>>) => TResult1 | PromiseLike<TResult1>) | null,
        onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
      ): PromiseLike<TResult1 | TResult2> {
        return result.then(onfulfilled, onrejected)
      },
      async unwrap(): Promise<TaskOutput<TTask>> {
        const settled = await result
        if (!settled.ok) throw settled.error
        return settled.output
      },
    })
  }

  async #waitForTerminal<TOutput extends JsonValue>(
    runId: string,
    options: RequestOptions,
  ): Promise<TaskResult<TOutput>> {
    let delayMilliseconds = 250
    while (true) {
      options.signal?.throwIfAborted()
      const snapshot = await this.retrieve<TOutput>(runId, options)
      if (snapshot.status === "succeeded") {
        if (snapshot.output === undefined) {
          throw new Error("Succeeded Run response must include output")
        }
        return Object.freeze({
          ok: true, output: snapshot.output, run: Object.freeze({ id: snapshot.id }),
        })
      }
      if (runStatusIsTerminal(snapshot.status)) {
        if (snapshot.error === undefined) {
          throw new Error("Non-success terminal Run response must include error")
        }
        return Object.freeze({
          ok: false, error: snapshot.error, run: Object.freeze({ id: snapshot.id }),
        })
      }
      await abortableDelay(delayMilliseconds, options.signal)
      delayMilliseconds = Math.min(delayMilliseconds * 2, 2_000)
    }
  }
}

class ClientTokens implements ClientTokensApi {
  readonly #transport: ClientTransport

  constructor(transport: ClientTransport) {
    this.#transport = transport
  }

  async create(
    request: ClientTokenCreateRequest = {},
    options: RequestOptions = {},
  ): Promise<ClientTokenCreateResult> {
    const response = await this.#transport.request("POST", "/api/tokens", {
      body: {
        ...(request.timeout === undefined ? {} : { timeout: request.timeout }),
        ...(request.metadata === undefined ? {} : { metadata: request.metadata }),
        ...(request.tags === undefined ? {} : { tags: [...request.tags] }),
        ...(request.idempotencyKey === undefined
          ? {}
          : { idempotency_key: request.idempotencyKey }),
      },
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    })
    const token = parseToken(response, true)
    if (token.status !== "pending") {
      throw new Error("Token create response must be pending")
    }
    return token as ClientTokenCreateResult
  }

  async retrieve(
    id: string,
    options: RequestOptions = {},
  ): Promise<TokenSnapshot> {
    return parseToken(
      await this.#transport.request(
        "GET",
        `/api/tokens/${encodeURIComponent(tokenID(id))}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
      false,
    )
  }

  async list(queryInput: Readonly<{
    cursor?: string
    limit?: number
  }> = {}, options: RequestOptions = {}): Promise<CursorPage<TokenSnapshot>> {
    const query = new URLSearchParams()
    if (queryInput.cursor !== undefined) query.set("cursor", queryInput.cursor)
    if (queryInput.limit !== undefined) query.set("limit", String(queryInput.limit))
    const suffix = query.size === 0 ? "" : `?${query.toString()}`
    const response = objectValue(
      await this.#transport.request("GET", `/api/tokens${suffix}`, {
        ...(options.signal === undefined ? {} : { signal: options.signal }),
      }),
      "Token list response",
    )
    if (!Array.isArray(response["tokens"])) {
      throw new Error("Token list response.tokens must be an array")
    }
    const nextCursor = response["next_cursor"]
    if (nextCursor !== undefined && typeof nextCursor !== "string") {
      throw new Error("Token list response.next_cursor must be a string")
    }
    return Object.freeze({
      items: Object.freeze(response["tokens"].map((token) => parseToken(token, false))),
      ...(nextCursor === undefined ? {} : { nextCursor }),
    })
  }

  async complete(
    id: string,
    request: Readonly<{ result: JsonValue; idempotencyKey?: string }>,
    options: RequestOptions = {},
  ): Promise<TokenSnapshot> {
    const response = objectValue(
      await this.#transport.request(
        "POST",
        `/api/tokens/${encodeURIComponent(tokenID(id))}/complete`,
        {
          body: {
            result: request.result,
            ...(request.idempotencyKey === undefined
              ? {}
              : { idempotency_key: request.idempotencyKey }),
          },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ),
      "Token completion response",
    )
    return parseToken(response["token"], false)
  }

  async cancel(
    id: string,
    request: Readonly<{ idempotencyKey?: string }> = {},
    options: RequestOptions = {},
  ): Promise<TokenSnapshot> {
    return parseToken(
      await this.#transport.request(
        "POST",
        `/api/tokens/${encodeURIComponent(tokenID(id))}/cancel`,
        {
          body: request.idempotencyKey === undefined
            ? {}
            : { idempotency_key: request.idempotencyKey },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ),
      false,
    )
  }
}

function taskStartRequest(
  request: ClientTaskRunRequest,
): Record<string, unknown> {
  return {
    workspace: "id" in request.workspace
      ? { id: request.workspace.id }
      : { key: request.workspace.key },
    ...(request.idempotencyKey === undefined
      ? {}
      : { idempotency_key: request.idempotencyKey }),
    ...(request.queue === undefined ? {} : { queue: request.queue }),
    ...(request.concurrencyKey === undefined
      ? {}
      : { concurrency_key: request.concurrencyKey }),
    ...(request.priority === undefined ? {} : { priority: request.priority }),
    ...(request.ttl === undefined ? {} : { ttl: request.ttl }),
    ...(request.retry === undefined
      ? {}
      : {
          retry: request.retry.enabled === false
            ? { enabled: false }
            : {
                ...(request.retry.enabled === undefined
                  ? {}
                  : { enabled: request.retry.enabled }),
                max_attempts: request.retry.maxAttempts,
                ...(request.retry.backoff === undefined
                  ? {}
                  : {
                      backoff: {
                        ...(request.retry.backoff.minDelay === undefined
                          ? {}
                          : { min_delay: request.retry.backoff.minDelay }),
                        ...(request.retry.backoff.maxDelay === undefined
                          ? {}
                          : { max_delay: request.retry.backoff.maxDelay }),
                        ...(request.retry.backoff.factor === undefined
                          ? {}
                          : { factor: request.retry.backoff.factor }),
                        ...(request.retry.backoff.jitter === undefined
                          ? {}
                          : { jitter: request.retry.backoff.jitter }),
                      },
                    }),
              },
        }),
    ...(request.metadata === undefined ? {} : { metadata: request.metadata }),
    ...(request.tags === undefined ? {} : { tags: [...request.tags] }),
  }
}

class ClientTransport {
  readonly #baseURL: URL
  readonly #apiKey: string
  readonly #fetch: typeof fetch

  constructor(options: HelmrClientOptions) {
    this.#baseURL = clientBaseURL(options.url)
    this.#apiKey = options.apiKey.trim()
    if (this.#apiKey === "") throw new Error("Helmr API key is required")
    this.#fetch = options.fetch ?? globalThis.fetch
    if (typeof this.#fetch !== "function") {
      throw new Error("fetch is unavailable")
    }
  }

  async request(
    method: "GET" | "POST",
    path: string,
    options: Readonly<{ body?: unknown; signal?: AbortSignal }> = {},
  ): Promise<unknown> {
    const response = await this.#fetch(new URL(path, this.#baseURL), {
      method,
      headers: {
        Authorization: `Bearer ${this.#apiKey}`,
        [HELMR_API_VERSION_HEADER]: HELMR_API_VERSION,
        [HELMR_SDK_VERSION_HEADER]: HELMR_SDK_VERSION,
        ...(options.body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    })
    const value: unknown = await response.json().catch(() => ({}))
    if (!response.ok) {
      throw clientRequestError(response, value)
    }
    return value
  }
}

function clientRequestError(response: Response, value: unknown): Error {
  const body = value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {}
  const message = typeof body["error"] === "string" && body["error"] !== ""
    ? body["error"]
    : typeof body["message"] === "string" && body["message"] !== ""
    ? body["message"]
    : `Helmr request failed with status ${response.status}`
  const code = typeof body["code"] === "string" && body["code"] !== ""
    ? body["code"]
    : responseStatusCode(response.status)
  const retryable = typeof body["retryable"] === "boolean"
    ? body["retryable"]
    : response.status === 429 || response.status >= 500
  const requestId = typeof body["request_id"] === "string" && body["request_id"] !== ""
    ? body["request_id"]
    : typeof body["requestId"] === "string" && body["requestId"] !== ""
    ? body["requestId"]
    : undefined
  const error = new Error(message) as Error & {
    code: string
    retryable: boolean
    requestId?: string
  }
  error.name = "HelmrError"
  error.code = code
  error.retryable = retryable
  if (requestId !== undefined) error.requestId = requestId
  return error
}

function responseStatusCode(status: number): string {
  switch (status) {
    case 400:
      return "bad_request"
    case 401:
      return "unauthorized"
    case 403:
      return "forbidden"
    case 404:
      return "not_found"
    case 409:
      return "conflict"
    case 410:
      return "gone"
    case 422:
      return "unprocessable_entity"
    case 429:
      return "rate_limited"
    default:
      return status >= 500 ? "internal_error" : "request_failed"
  }
}

function clientBaseURL(raw: string): URL {
  const url = new URL(raw)
  if (url.search !== "" || url.hash !== "") {
    throw new Error("Helmr API URL must not include query or fragment")
  }
  if (url.protocol !== "https:" && !(
    url.protocol === "http:" &&
    (url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]")
  )) {
    throw new Error("Helmr API URL must use HTTPS except on loopback")
  }
  if (!url.pathname.endsWith("/")) url.pathname += "/"
  return url
}

function tokenID(value: string): string {
  const normalized = value.trim()
  if (!/^tok_[a-z2-7]{26}$/.test(normalized)) {
    throw new Error("Token ID must be a canonical tok_ public ID")
  }
  return normalized
}

function runID(value: string): string {
  const normalized = value.trim()
  if (!/^run_[a-z2-7]{26}$/.test(normalized)) {
    throw new Error("Run ID must be a canonical run_ public ID")
  }
  return normalized
}

function runStatus(value: string): RunStatus {
  switch (value) {
    case "queued":
    case "running":
    case "waiting":
    case "retry-delayed":
    case "cancel-requested":
    case "succeeded":
    case "failed":
    case "cancelled":
    case "expired":
    case "system-failed":
      return value
    default:
      throw new Error(`Run status ${JSON.stringify(value)} is invalid`)
  }
}

function runStatusIsTerminal(status: RunStatus): boolean {
  return status === "succeeded" ||
    status === "failed" ||
    status === "cancelled" ||
    status === "expired" ||
    status === "system-failed"
}

function parseRunSnapshot<TOutput extends JsonValue = JsonValue>(
  value: unknown,
): RunSnapshot<TOutput> {
  const run = objectValue(value, "Run response")
  const status = runStatus(requiredStringFrom(run, "status", "Run response"))
  const entrypoint = objectValue(run["entrypoint"], "Run response.entrypoint")
  const entrypointKind = requiredStringFrom(entrypoint, "kind", "Run response.entrypoint")
  if (entrypointKind !== "task" && entrypointKind !== "actor") {
    throw new Error("Run response.entrypoint.kind is invalid")
  }
  const deployment = objectValue(run["deployment"], "Run response.deployment")
  const metadata = objectValue(run["metadata"], "Run response.metadata") as Metadata
  const tags = run["tags"]
  if (!Array.isArray(tags) || tags.some((tag) => typeof tag !== "string")) {
    throw new Error("Run response.tags must be an array of strings")
  }
  const cause = parseRunCause(run["cause"])
  const snapshot: RunSnapshot<TOutput> = {
    id: runID(requiredStringFrom(run, "id", "Run response")),
    status,
    entrypoint: Object.freeze({
      kind: entrypointKind,
      id: requiredStringFrom(entrypoint, "id", "Run response.entrypoint"),
    }),
    deployment: Object.freeze({
      id: requiredStringFrom(deployment, "id", "Run response.deployment"),
      version: requiredStringFrom(deployment, "version", "Run response.deployment"),
    }),
    workspaceId: requiredStringFrom(run, "workspace_id", "Run response"),
    ...(run["actor_id"] === undefined
      ? {}
      : { actorId: requiredStringFrom(run, "actor_id", "Run response") }),
    ...(run["parent_run_id"] === undefined
      ? {}
      : { parentRunId: runID(requiredStringFrom(run, "parent_run_id", "Run response")) }),
    ...(run["parent_owns_lifecycle"] === undefined
      ? {}
      : { parentOwnsLifecycle: requiredBoolean(run, "parent_owns_lifecycle", "Run response") }),
    currentAttemptNumber: requiredPositiveInteger(
      run, "current_attempt_number", "Run response",
    ),
    cause,
    metadata: Object.freeze({ ...metadata }),
    tags: Object.freeze([...tags]) as readonly string[],
    ...(run["output"] === undefined ? {} : { output: run["output"] as TOutput }),
    ...(run["terminal_reason_code"] === undefined
      ? {}
      : {
          terminalReasonCode: requiredStringFrom(
            run, "terminal_reason_code", "Run response",
          ),
        }),
    ...(run["error"] === undefined ? {} : { error: parseRunError(run["error"]) }),
    createdAt: requiredTimestamp(run, "created_at", "Run response"),
    ...(run["started_at"] === undefined
      ? {}
      : { startedAt: requiredTimestamp(run, "started_at", "Run response") }),
    ...(run["terminal_at"] === undefined
      ? {}
      : { terminalAt: requiredTimestamp(run, "terminal_at", "Run response") }),
  }
  if (status === "succeeded") {
    if (
      snapshot.error !== undefined ||
      snapshot.terminalReasonCode !== undefined ||
      snapshot.terminalAt === undefined
    ) {
      throw new Error("Succeeded Run response has an invalid terminal projection")
    }
  } else if (runStatusIsTerminal(status)) {
    if (
      snapshot.output !== undefined ||
      snapshot.error === undefined ||
      snapshot.terminalReasonCode === undefined ||
      snapshot.terminalAt === undefined
    ) {
      throw new Error("Terminal Run response has an invalid failure projection")
    }
  } else if (
    snapshot.output !== undefined ||
    snapshot.error !== undefined ||
    snapshot.terminalReasonCode !== undefined ||
    snapshot.terminalAt !== undefined
  ) {
    throw new Error("Active Run response has terminal fields")
  }
  return Object.freeze(snapshot)
}

function parseRunCause(value: unknown): RunSnapshot["cause"] {
  const cause = objectValue(value, "Run response.cause")
  const type = requiredStringFrom(cause, "type", "Run response.cause")
  switch (type) {
    case "api":
    case "manual":
    case "actor-start":
    case "continuation":
      return Object.freeze({ type })
    case "child":
      return Object.freeze({
        type,
        parentRunId: runID(
          requiredStringFrom(cause, "parent_run_id", "Run response.cause"),
        ),
      })
    case "schedule":
      return Object.freeze({
        type,
        scheduleId: requiredStringFrom(cause, "schedule_id", "Run response.cause"),
        scheduledAt: requiredDate(cause, "scheduled_at", "Run response.cause"),
        ...(cause["last_scheduled_at"] === undefined
          ? {}
          : {
              lastScheduledAt: requiredDate(
                cause, "last_scheduled_at", "Run response.cause",
              ),
            }),
        timezone: requiredStringFrom(cause, "timezone", "Run response.cause"),
      })
    default:
      throw new Error("Run response.cause.type is invalid")
  }
}

function parseRunError(value: unknown): RunError {
  const source = objectValue(value, "Run response.error")
  const error = new Error(
    requiredStringFrom(source, "message", "Run response.error"),
  ) as Error & {
    code: string
    retryable: boolean
    details?: JsonValue
  }
  error.name = "RunError"
  error.code = requiredStringFrom(source, "code", "Run response.error")
  error.retryable = requiredBoolean(source, "retryable", "Run response.error")
  if (source["details"] !== undefined) error.details = source["details"] as JsonValue
  return error
}

function abortableDelay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted()
  return new Promise((resolve, reject) => {
    const timer = setTimeout(done, milliseconds)
    function done(): void {
      signal?.removeEventListener("abort", aborted)
      resolve()
    }
    function aborted(): void {
      clearTimeout(timer)
      signal?.removeEventListener("abort", aborted)
      try {
        signal?.throwIfAborted()
      } catch (error) {
        reject(error)
        return
      }
      reject(new Error("Run wait was aborted"))
    }
    signal?.addEventListener("abort", aborted, { once: true })
  })
}

function parseToken(value: unknown, credentials: boolean): TokenSnapshot | ClientTokenCreateResult {
  const token = objectValue(value, "Token response")
  const status = token["status"]
  if (
    status !== "pending" &&
    status !== "completed" &&
    status !== "cancelled" &&
    status !== "expired"
  ) {
    throw new Error("Token response.status is invalid")
  }
  const metadata = objectValue(token["metadata"], "Token response.metadata") as Metadata
  const tags = token["tags"]
  if (!Array.isArray(tags) || tags.some((tag) => typeof tag !== "string")) {
    throw new Error("Token response.tags must be an array of strings")
  }
  const snapshot: TokenSnapshot = {
    id: requiredString(token, "id"),
    status,
    ...(token["result"] === undefined ? {} : { result: token["result"] as JsonValue }),
    metadata,
    tags: Object.freeze([...tags]) as readonly string[],
    timeoutAt: requiredString(token, "timeout_at"),
    ...(token["completed_at"] === undefined
      ? {}
      : { completedAt: requiredString(token, "completed_at") }),
    createdAt: requiredString(token, "created_at"),
    updatedAt: requiredString(token, "updated_at"),
  }
  if (!credentials) return Object.freeze(snapshot)
  return Object.freeze({
    ...snapshot,
    callbackUrl: requiredString(token, "callback_url"),
    publicAccessToken: requiredString(token, "public_access_token"),
  }) as ClientTokenCreateResult
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requiredString(value: Record<string, unknown>, field: string): string {
  return requiredStringFrom(value, field, "Token response")
}

function requiredStringFrom(
  value: Record<string, unknown>,
  field: string,
  label: string,
): string {
  const result = value[field]
  if (typeof result !== "string" || result === "") {
    throw new Error(`${label}.${field} must be a non-empty string`)
  }
  return result
}

function requiredBoolean(
  value: Record<string, unknown>,
  field: string,
  label: string,
): boolean {
  const result = value[field]
  if (typeof result !== "boolean") {
    throw new Error(`${label}.${field} must be a boolean`)
  }
  return result
}

function requiredPositiveInteger(
  value: Record<string, unknown>,
  field: string,
  label: string,
): number {
  const result = value[field]
  if (!Number.isSafeInteger(result) || (result as number) < 1) {
    throw new Error(`${label}.${field} must be a positive safe integer`)
  }
  return result as number
}

function requiredDate(
  value: Record<string, unknown>,
  field: string,
  label: string,
): Date {
  const raw = requiredStringFrom(value, field, label)
  const result = new Date(raw)
  if (Number.isNaN(result.getTime())) {
    throw new Error(`${label}.${field} must be an RFC 3339 timestamp`)
  }
  return result
}

function requiredTimestamp(
  value: Record<string, unknown>,
  field: string,
  label: string,
): string {
  const raw = requiredStringFrom(value, field, label)
  if (Number.isNaN(new Date(raw).getTime())) {
    throw new Error(`${label}.${field} must be an RFC 3339 timestamp`)
  }
  return raw
}
