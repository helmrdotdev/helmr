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
  WorkspaceIdAddress,
  TaskWait,
} from "./contract"
import { runtimeOperationsInstalled } from "./internal/runtime"
import { resourceID } from "./internal/id"
import { validateTaskId } from "./schema/task"
import type {
  TokenCreateOptions,
  TokenSnapshot,
} from "./tokens"
import {
  createClientWorkspaces,
  type ClientWorkspacesApi,
} from "./client-workspace"
import {
  createClientSandboxes,
  type ClientSandboxesApi,
} from "./client-sandbox"
import {
  createClientActors,
  type ClientActorsApi,
} from "./client-actor"
import {
  createClientSessions,
  type ClientSessionsApi,
} from "./client-session"
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
import type { LogAttributes, RunLogLevel } from "./logger"
import { requireWorkspaceIDAddress } from "./workspace"

export interface HelmrClientOptions {
  readonly url?: string
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
  workspace: WorkspaceIdAddress
}>

export type ClientTaskStartRequest<TTask extends TaskDefinition> =
  TaskHasPayload<TTask> extends true
    ? ClientTaskRunRequest & Readonly<{ payload: TaskPayloadInput<TTask> }>
    : ClientTaskRunRequest & Readonly<{ payload?: never }>

export interface ClientTasksApi {
  retrieve(taskId: string, query?: ClientTaskReadQuery, options?: RequestOptions): Promise<TaskSnapshot>
  list(query?: ClientTaskListQuery, options?: RequestOptions): Promise<TaskPage>
  start<TTask extends TaskDefinition>(
    taskDeclaredId: string,
    request: ClientTaskStartRequest<TTask>,
    options?: RequestOptions,
  ): Promise<RunHandle>
}

export interface ClientTaskReadQuery {
  readonly deploymentId?: string
}

export interface ClientTaskListQuery extends ClientTaskReadQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface TaskSnapshot {
  readonly id: string
  readonly deploymentId: string
}

export interface TaskPage extends CursorPage<TaskSnapshot> {
  readonly deploymentId: string
}

export interface ClientRunListQuery {
  readonly status?: RunStatus | readonly RunStatus[]
  readonly cursor?: string
  readonly limit?: number
}

export interface ClientRunLogQuery {
  readonly cursor?: string
  readonly limit?: number
  readonly level?: RunLogLevel | readonly RunLogLevel[]
}

export interface ClientRunEventQuery {
  readonly cursor?: string
  readonly limit?: number
  readonly severity?: RunLogLevel | readonly RunLogLevel[]
}

export interface StructuredRunLogRecord {
  readonly id: string
  readonly kind: "structured"
  readonly runId: string
  readonly attemptNumber: number
  readonly level: RunLogLevel
  readonly message: string
  readonly attributes: LogAttributes
  readonly at: string
}

export interface StreamRunLogRecord {
  readonly id: string
  readonly kind: "stdout" | "stderr"
  readonly runId: string
  readonly attemptNumber: number
  readonly observedSequence: number
  readonly contentBase64: string
  readonly bytes: number
  readonly at: string
}

export type RunLogRecord = StructuredRunLogRecord | StreamRunLogRecord

export interface RunEventRecord {
  readonly id: string
  readonly runId: string
  readonly attemptNumber?: number
  readonly category: string
  readonly severity: RunLogLevel
  readonly source: string
  readonly kind: string
  readonly message: string
  readonly attributes: JsonValue
  readonly occurredAt: string
  readonly at: string
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
  logs(
    runId: string,
    query?: ClientRunLogQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<RunLogRecord>>
  events(
    runId: string,
    query?: ClientRunEventQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<RunEventRecord>>
}

export class HelmrClient {
  readonly tasks: ClientTasksApi
  readonly actors: ClientActorsApi
  readonly sessions: ClientSessionsApi
  readonly sandboxes: ClientSandboxesApi
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
    this.sessions = createClientSessions(transport)
    this.sandboxes = createClientSandboxes(transport)
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

  async retrieve(
    taskId: string,
    query: ClientTaskReadQuery = {},
    options: RequestOptions = {},
  ): Promise<TaskSnapshot> {
    validateTaskId(taskId)
    return parseTask(await this.#transport.request(
      "GET",
      `/v1/tasks/${encodeURIComponent(taskId)}${taskReadQuery(query)}`,
      options.signal === undefined ? {} : { signal: options.signal },
    ))
  }

  async list(
    query: ClientTaskListQuery = {},
    options: RequestOptions = {},
  ): Promise<TaskPage> {
    const response = objectValue(await this.#transport.request(
      "GET",
      `/v1/tasks${taskReadQuery(query)}`,
      options.signal === undefined ? {} : { signal: options.signal },
    ), "Task list response")
    if (!Array.isArray(response["tasks"])) {
      throw new Error("Task list response.tasks must be an array")
    }
    const nextCursor = response["next_cursor"]
    if (nextCursor !== undefined && typeof nextCursor !== "string") {
      throw new Error("Task list response.next_cursor must be a string")
    }
    return Object.freeze({
      deploymentId: resourceID(response["deployment_id"], "Task list response.deployment_id"),
      items: Object.freeze(response["tasks"].map(parseTask)),
      ...(nextCursor === undefined ? {} : { nextCursor }),
    })
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
        `/v1/tasks/${encodeURIComponent(taskDeclaredId)}/start`,
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
    const runId = resourceID(response["run_id"], "Task start response.run_id")
    return Object.freeze({ id: runId })
  }
}

function taskReadQuery(queryInput: ClientTaskListQuery): string {
  const query = new URLSearchParams()
  if (queryInput.deploymentId !== undefined) {
    query.set("deployment_id", resourceID(queryInput.deploymentId, "Deployment ID"))
  }
  if (queryInput.cursor !== undefined) {
    if (queryInput.cursor.length === 0) throw new Error("Task cursor is required")
    query.set("cursor", queryInput.cursor)
  }
  if (queryInput.limit !== undefined) {
    if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
      throw new Error("Task limit must be an integer in [1,100]")
    }
    query.set("limit", queryInput.limit.toString())
  }
  return query.size === 0 ? "" : `?${query.toString()}`
}

function parseTask(value: unknown): TaskSnapshot {
  const input = objectValue(value, "Task response")
  return Object.freeze({
    id: requiredStringFrom(input, "id", "Task response"),
    deploymentId: resourceID(input["deployment_id"], "Task response.deployment_id"),
  })
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
        `/v1/runs/${encodeURIComponent(resourceID(runId, "Run ID"))}`,
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
      await this.#transport.request("GET", `/v1/runs${suffix}`, {
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
        `/v1/runs/${encodeURIComponent(resourceID(runId, "Run ID"))}/cancel`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
    )
  }

  async logs(
    runId: string,
    queryInput: ClientRunLogQuery = {},
    options: RequestOptions = {},
  ): Promise<CursorPage<RunLogRecord>> {
    const query = runTelemetryQuery(
      queryInput.cursor,
      queryInput.limit,
      "level",
      queryInput.level,
    )
    const response = objectValue(
      await this.#transport.request(
        "GET",
        `/v1/runs/${encodeURIComponent(resourceID(runId, "Run ID"))}/logs${query}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
      "Run log page",
    )
    if (!Array.isArray(response["logs"])) {
      throw new Error("Run log page.logs must be an array")
    }
    return cursorPage(
      response["logs"].map(parseRunLogRecord),
      response["next_cursor"],
      "Run log page",
    )
  }

  async events(
    runId: string,
    queryInput: ClientRunEventQuery = {},
    options: RequestOptions = {},
  ): Promise<CursorPage<RunEventRecord>> {
    const query = runTelemetryQuery(
      queryInput.cursor,
      queryInput.limit,
      "severity",
      queryInput.severity,
    )
    const response = objectValue(
      await this.#transport.request(
        "GET",
        `/v1/runs/${encodeURIComponent(resourceID(runId, "Run ID"))}/events${query}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
      "Run event page",
    )
    if (!Array.isArray(response["events"])) {
      throw new Error("Run event page.events must be an array")
    }
    return cursorPage(
      response["events"].map(parseRunEventRecord),
      response["next_cursor"],
      "Run event page",
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
    const id = resourceID(runId, "Run ID")
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
    const response = await this.#transport.request("POST", "/v1/tokens", {
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
        `/v1/tokens/${encodeURIComponent(resourceID(id, "Token ID"))}`,
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
      await this.#transport.request("GET", `/v1/tokens${suffix}`, {
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
        `/v1/tokens/${encodeURIComponent(resourceID(id, "Token ID"))}/complete`,
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
        `/v1/tokens/${encodeURIComponent(resourceID(id, "Token ID"))}/cancel`,
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
    workspace: { id: requireWorkspaceIDAddress(request.workspace) },
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
    this.#baseURL = clientBaseURL(options.url ?? "https://api.helmr.dev")
    this.#apiKey = options.apiKey.trim()
    if (this.#apiKey === "") throw new Error("Helmr API key is required")
    this.#fetch = options.fetch ?? globalThis.fetch
    if (typeof this.#fetch !== "function") {
      throw new Error("fetch is unavailable")
    }
  }

  async request(
    method: "GET" | "POST" | "DELETE",
    path: string,
    options: Readonly<{ body?: unknown; signal?: AbortSignal }> = {},
  ): Promise<unknown> {
    const response = await this.#fetch(new URL(path, this.#baseURL), {
      method,
      headers: {
        Authorization: `Bearer ${this.#apiKey}`,
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
  const requestId = typeof body["requestId"] === "string" && body["requestId"] !== ""
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
  if (url.username !== "" || url.password !== "" ||
    (url.pathname !== "" && url.pathname !== "/") || url.search !== "" || url.hash !== "") {
    throw new Error("Helmr API URL must be an origin without credentials, path, query, or fragment")
  }
  if (url.protocol !== "https:" && !(
    url.protocol === "http:" &&
    (url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]")
  )) {
    throw new Error("Helmr API URL must use HTTPS except on loopback")
  }
  url.pathname = "/"
  return url
}

function runStatus(value: string): RunStatus {
  switch (value) {
    case "queued":
    case "running":
    case "waiting":
    case "retry_delayed":
    case "cancel_requested":
    case "succeeded":
    case "failed":
    case "cancelled":
    case "expired":
    case "system_failed":
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
    status === "system_failed"
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
    id: resourceID(requiredStringFrom(run, "id", "Run response"), "Run response.id"),
    status,
    entrypoint: Object.freeze({
      kind: entrypointKind,
      id: requiredStringFrom(entrypoint, "id", "Run response.entrypoint"),
    }),
    deployment: Object.freeze({
      id: resourceID(
        requiredStringFrom(deployment, "id", "Run response.deployment"),
        "Run response.deployment.id",
      ),
      version: requiredStringFrom(deployment, "version", "Run response.deployment"),
    }),
    workspaceId: resourceID(
      requiredStringFrom(run, "workspace_id", "Run response"),
      "Run response.workspace_id",
    ),
    ...(run["session_id"] === undefined
      ? {}
      : {
          sessionId: resourceID(
            requiredStringFrom(run, "session_id", "Run response"),
            "Run response.session_id",
          ),
        }),
    ...(run["parent_run_id"] === undefined
      ? {}
      : {
          parentRunId: resourceID(
            requiredStringFrom(run, "parent_run_id", "Run response"),
            "Run response.parent_run_id",
          ),
        }),
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
    case "actor_start":
    case "continuation":
      return Object.freeze({ type })
    case "child":
      return Object.freeze({
        type,
        parentRunId: resourceID(
          requiredStringFrom(cause, "parent_run_id", "Run response.cause"),
          "Run response.cause.parent_run_id",
        ),
      })
    case "schedule":
      return Object.freeze({
        type,
        scheduleId: resourceID(
          requiredStringFrom(cause, "schedule_id", "Run response.cause"),
          "Run response.cause.schedule_id",
        ),
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

function runTelemetryQuery(
  cursor: string | undefined,
  limit: number | undefined,
  filterName: "level" | "severity",
  filter: RunLogLevel | readonly RunLogLevel[] | undefined,
): string {
  const query = new URLSearchParams()
  if (cursor !== undefined) query.set("cursor", cursor)
  if (limit !== undefined) query.set("limit", String(limit))
  const values = filter === undefined
    ? []
    : Array.isArray(filter)
    ? filter
    : [filter]
  for (const value of values) query.append(filterName, runLogLevel(value))
  return query.size === 0 ? "" : `?${query.toString()}`
}

function parseRunLogRecord(value: unknown): RunLogRecord {
  const record = objectValue(value, "Run log record")
  const kind = requiredStringFrom(record, "kind", "Run log record")
  const common = {
    id: requiredStringFrom(record, "id", "Run log record"),
    runId: resourceID(
      requiredStringFrom(record, "run_id", "Run log record"),
      "Run log record.run_id",
    ),
    attemptNumber: requiredPositiveInteger(
      record,
      "attempt_number",
      "Run log record",
    ),
    at: requiredTimestamp(record, "at", "Run log record"),
  }
  if (kind === "structured") {
    const attributes = objectValue(
      record["attributes"],
      "Run log record.attributes",
    ) as LogAttributes
    return Object.freeze({
      ...common,
      kind,
      level: runLogLevel(record["level"]),
      message: stringValue(record, "message", "Run log record"),
      attributes: Object.freeze({ ...attributes }),
    })
  }
  if (kind !== "stdout" && kind !== "stderr") {
    throw new Error("Run log record.kind is invalid")
  }
  const contentBase64 = stringValue(
    record,
    "content_base64",
    "Run log record",
  )
  if (!validBase64(contentBase64)) {
    throw new Error("Run log record.content_base64 must be canonical base64")
  }
  return Object.freeze({
    ...common,
    kind,
    observedSequence: requiredPositiveInteger(
      record,
      "observed_sequence",
      "Run log record",
    ),
    contentBase64,
    bytes: requiredNonnegativeInteger(record, "bytes", "Run log record"),
  })
}

function parseRunEventRecord(value: unknown): RunEventRecord {
  const event = objectValue(value, "Run event record")
  return Object.freeze({
    id: requiredStringFrom(event, "id", "Run event record"),
    runId: resourceID(
      requiredStringFrom(event, "run_id", "Run event record"),
      "Run event record.run_id",
    ),
    ...(event["attempt_number"] === undefined
      ? {}
      : {
          attemptNumber: requiredPositiveInteger(
            event,
            "attempt_number",
            "Run event record",
          ),
        }),
    category: requiredStringFrom(event, "category", "Run event record"),
    severity: runLogLevel(event["severity"]),
    source: requiredStringFrom(event, "source", "Run event record"),
    kind: requiredStringFrom(event, "kind", "Run event record"),
    message: stringValue(event, "message", "Run event record"),
    attributes: event["attributes"] as JsonValue,
    occurredAt: requiredTimestamp(event, "occurred_at", "Run event record"),
    at: requiredTimestamp(event, "at", "Run event record"),
  })
}

function cursorPage<T>(
  items: T[],
  nextCursor: unknown,
  label: string,
): CursorPage<T> {
  if (nextCursor !== undefined && typeof nextCursor !== "string") {
    throw new Error(`${label}.next_cursor must be a string`)
  }
  return Object.freeze({
    items: Object.freeze(items),
    ...(nextCursor === undefined ? {} : { nextCursor }),
  })
}

function runLogLevel(value: unknown): RunLogLevel {
  if (
    value !== "debug" &&
    value !== "info" &&
    value !== "warn" &&
    value !== "error"
  ) {
    throw new Error("Run log level must be debug, info, warn, or error")
  }
  return value
}

function validBase64(value: string): boolean {
  return /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(
    value,
  )
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
    id: resourceID(requiredString(token, "id"), "Token response.id"),
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

function stringValue(
  value: Record<string, unknown>,
  field: string,
  label: string,
): string {
  const result = value[field]
  if (typeof result !== "string") {
    throw new Error(`${label}.${field} must be a string`)
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

function requiredNonnegativeInteger(
  value: Record<string, unknown>,
  field: string,
  label: string,
): number {
  const result = value[field]
  if (!Number.isSafeInteger(result) || (result as number) < 0) {
    throw new Error(`${label}.${field} must be a nonnegative safe integer`)
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
