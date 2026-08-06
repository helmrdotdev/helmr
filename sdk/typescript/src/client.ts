import type {
  CursorPage,
  JsonValue,
  Metadata,
  RunFailure,
  RunHandle,
  Run,
  RunStatus,
  Task,
  TaskInput,
  TaskOutput,
  TaskResult,
  RunOptions,
  TaskWait,
} from "./contract"
import { runtimeOperationsInstalled } from "./internal/runtime"
import { resourceID } from "./internal/id"
import { createRunHandle, runHandleID } from "./internal/run-handle"
import { timestampString } from "./internal/timestamp"
import { runFailureError } from "./internal/run-failure"
import { validateTaskId } from "./schema/task"
import type {
  TokenCreateRequest,
  TokenCreateResult,
  TokenListItem,
  TokenListQuery,
  Token,
  TokenStatus,
} from "./tokens"
import type { WorkspaceRef } from "./workspace"
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
import { workspaceRefID } from "./workspace"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export interface HelmrClientOptions {
  readonly url?: string
  readonly apiKey: string
  readonly fetch?: typeof fetch
}

export interface TokenCompleteRequest {
  readonly result: JsonValue
  readonly idempotencyKey?: string
}

export interface TokenCancelRequest {
  readonly idempotencyKey?: string
}

export interface ClientTokensApi {
  create(
    request?: TokenCreateRequest,
    options?: RequestOptions,
  ): Promise<TokenCreateResult>
  retrieve(id: string, options?: RequestOptions): Promise<Token>
  list(
    query?: TokenListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<TokenListItem>>
  complete(id: string, request: TokenCompleteRequest, options?: RequestOptions): Promise<Token>
  cancel(id: string, request?: TokenCancelRequest, options?: RequestOptions): Promise<Token>
}

type ClientTaskRunRequest = RunOptions & Readonly<{
  idempotencyKey?: string
  workspace: WorkspaceRef
}>

export type TaskStartRequest<TTask extends Task> =
  [TaskInput<TTask>] extends [never]
    ? ClientTaskRunRequest & Readonly<{ payload?: never }>
    : ClientTaskRunRequest & Readonly<{ payload: TaskInput<TTask> }>

export interface ClientTasksApi {
  retrieve(id: string, query?: TaskRetrieveQuery, options?: RequestOptions): Promise<TaskInfo>
  list(query?: TaskListQuery, options?: RequestOptions): Promise<TaskPage>
  start<TTask extends Task>(
    taskId: TTask["id"],
    request: TaskStartRequest<TTask>,
    options?: RequestOptions,
  ): Promise<RunHandle<TaskOutput<TTask>>>
}

export interface TaskRetrieveQuery {
  readonly deploymentId?: string
}

export interface TaskListQuery extends TaskRetrieveQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface TaskListItem {
  readonly id: string
}

export interface TaskInfo extends TaskListItem {
  readonly deploymentId: string
}

export interface TaskPage extends CursorPage<TaskListItem> {
  readonly deploymentId: string
}

export interface RunListQuery {
  readonly status?: RunStatus | readonly RunStatus[]
  readonly cursor?: string
  readonly limit?: number
}

export interface RunListItem {
  readonly id: string
  readonly status: RunStatus
  readonly entrypoint: Readonly<{ kind: "task" | "actor"; id: string }>
  readonly workspaceId: string
  readonly sessionId?: string
  readonly currentAttemptNumber: number
  readonly createdAt: string
  readonly startedAt?: string
  readonly terminalAt?: string
}

export interface RunLogQuery {
  readonly cursor?: string
  readonly limit?: number
  readonly level?: RunLogLevel | readonly RunLogLevel[]
}

export interface RunEventQuery {
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
  retrieve<TOutput extends JsonValue>(
    run: RunHandle<TOutput>,
    options?: RequestOptions,
  ): Promise<Run<TOutput>>
  retrieve(
    runId: string,
    options?: RequestOptions,
  ): Promise<Run<JsonValue>>
  list(
    query?: RunListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<RunListItem>>
  cancel(
    runId: string,
    options?: RequestOptions,
  ): Promise<Run<JsonValue>>
  wait<TOutput extends JsonValue>(
    run: RunHandle<TOutput>,
    options?: RequestOptions,
  ): TaskWait<TOutput>
  wait(
    runId: string,
    options?: RequestOptions,
  ): TaskWait<JsonValue>
  logs(
    runId: string,
    query?: RunLogQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<RunLogRecord>>
  events(
    runId: string,
    query?: RunEventQuery,
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
    id: string,
    query: TaskRetrieveQuery = {},
    options: RequestOptions = {},
  ): Promise<TaskInfo> {
    validateTaskId(id)
    return parseTask(await this.#transport.request(
      "GET",
      `/v1/tasks/${encodeURIComponent(id)}${definitionItemQuery(query, "Task")}`,
      options.signal === undefined ? {} : { signal: options.signal },
    ))
  }

  async list(
    query: TaskListQuery = {},
    options: RequestOptions = {},
  ): Promise<TaskPage> {
    const response = objectValue(await this.#transport.request(
      "GET",
      `/v1/tasks${definitionListQuery(query, "Task")}`,
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
      items: Object.freeze(response["tasks"].map(parseTaskListItem)),
      ...(nextCursor === undefined ? {} : { nextCursor }),
    })
  }

  async start<TTask extends Task>(
    taskId: TTask["id"],
    request: TaskStartRequest<TTask>,
    options: RequestOptions = {},
  ): Promise<RunHandle<TaskOutput<TTask>>> {
    validateTaskId(taskId)
    const response = objectValue(
      await this.#transport.request(
        "POST",
        `/v1/tasks/${encodeURIComponent(taskId)}/start`,
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
    return createRunHandle<TaskOutput<TTask>>(runId)
  }
}

function parseTask(value: unknown): TaskInfo {
  const input = objectValue(value, "Task response")
  const id = requiredStringFrom(input, "id", "Task response")
  validateTaskId(id)
  return Object.freeze({
    id,
    deploymentId: resourceID(input["deployment_id"], "Task response.deployment_id"),
  })
}

function parseTaskListItem(value: unknown): TaskListItem {
  const input = objectValue(value, "Task list item")
  const id = requiredStringFrom(input, "id", "Task list item")
  validateTaskId(id)
  return Object.freeze({ id })
}

class ClientRuns implements ClientRunsApi {
  readonly #transport: ClientTransport

  constructor(transport: ClientTransport) {
    this.#transport = transport
  }

  retrieve<TOutput extends JsonValue>(
    run: RunHandle<TOutput>,
    options?: RequestOptions,
  ): Promise<Run<TOutput>>
  retrieve(
    runId: string,
    options?: RequestOptions,
  ): Promise<Run<JsonValue>>
  async retrieve<TOutput extends JsonValue>(
    run: string | RunHandle<TOutput>,
    options: RequestOptions = {},
  ): Promise<Run<TOutput>> {
    const runId = runHandleID(run)
    return parseRun<TOutput>(
      await this.#transport.request(
        "GET",
        `/v1/runs/${encodeURIComponent(runId)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
    )
  }

  async list(
    queryInput: RunListQuery = {},
    options: RequestOptions = {},
  ): Promise<CursorPage<RunListItem>> {
    const query = new URLSearchParams()
    const statuses = queryInput.status === undefined
      ? []
      : Array.isArray(queryInput.status)
      ? queryInput.status
      : [queryInput.status]
    for (const status of statuses) query.append("status", runStatus(status))
    if (queryInput.cursor !== undefined) {
      if (queryInput.cursor.length === 0) throw new Error("Run cursor is required")
      query.set("cursor", queryInput.cursor)
    }
    if (queryInput.limit !== undefined) {
      if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
        throw new Error("Run limit must be an integer in [1,100]")
      }
      query.set("limit", String(queryInput.limit))
    }
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
      items: Object.freeze(response["runs"].map(parseRunListItem)),
      ...(nextCursor === undefined ? {} : { nextCursor }),
    })
  }

  async cancel(
    runId: string,
    options: RequestOptions = {},
  ): Promise<Run<JsonValue>> {
    return parseRun(
      await this.#transport.request(
        "POST",
        `/v1/runs/${encodeURIComponent(resourceID(runId, "Run ID"))}/cancel`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
    )
  }

  async logs(
    runId: string,
    queryInput: RunLogQuery = {},
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
    queryInput: RunEventQuery = {},
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

  wait<TOutput extends JsonValue>(
    run: RunHandle<TOutput>,
    options?: RequestOptions,
  ): TaskWait<TOutput>
  wait(
    runId: string,
    options?: RequestOptions,
  ): TaskWait<JsonValue>
  wait<TOutput extends JsonValue>(
    run: string | RunHandle<TOutput>,
    options: RequestOptions = {},
  ): TaskWait<TOutput> {
    if (runtimeOperationsInstalled()) {
      throw new Error(
        "client.runs.wait() is unavailable inside an active Helmr Run; use task.call()",
      )
    }
    const id = runHandleID(run)
    const result = this.#waitForTerminal<TOutput>(id, options)
    return Object.freeze({
      then<TResult1 = TaskResult<TOutput>, TResult2 = never>(
        onfulfilled?: ((value: TaskResult<TOutput>) => TResult1 | PromiseLike<TResult1>) | null,
        onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
      ): PromiseLike<TResult1 | TResult2> {
        return result.then(onfulfilled, onrejected)
      },
      async unwrap(): Promise<TOutput> {
        const settled = await result
        if (!settled.ok) throw runFailureError(settled.failure)
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
      const run = await this.retrieve(
        createRunHandle<TOutput>(runId),
        options,
      )
      if (run.status === "succeeded") {
        if (run.output === undefined) {
          throw new Error("Succeeded Run response must include output")
        }
        return Object.freeze({
          ok: true, output: run.output, run: createRunHandle<TOutput>(run.id),
        })
      }
      if (runStatusIsTerminal(run.status)) {
        if (run.failure === undefined) {
          throw new Error("Non-success terminal Run response must include failure")
        }
        return Object.freeze({
          ok: false, failure: run.failure, run: createRunHandle<TOutput>(run.id),
        })
      }
      await abortableDelay(delayMilliseconds, options.signal)
      delayMilliseconds = Math.min(delayMilliseconds * 2, 2_000)
    }
  }
}

function parseRunListItem(value: unknown): RunListItem {
  const run = objectValue(value, "Run list item")
  const status = runStatus(requiredStringFrom(run, "status", "Run list item"))
  const entrypoint = objectValue(run["entrypoint"], "Run list item.entrypoint")
  const kind = requiredStringFrom(entrypoint, "kind", "Run list item.entrypoint")
  if (kind !== "task" && kind !== "actor") {
    throw new Error("Run list item.entrypoint.kind is invalid")
  }
  const item: RunListItem = {
    id: resourceID(requiredStringFrom(run, "id", "Run list item"), "Run list item.id"),
    status,
    entrypoint: Object.freeze({
      kind,
      id: requiredStringFrom(entrypoint, "id", "Run list item.entrypoint"),
    }),
    workspaceId: resourceID(
      requiredStringFrom(run, "workspace_id", "Run list item"),
      "Run list item.workspace_id",
    ),
    ...(run["session_id"] === undefined
      ? {}
      : {
          sessionId: resourceID(
            requiredStringFrom(run, "session_id", "Run list item"),
            "Run list item.session_id",
          ),
        }),
    currentAttemptNumber: requiredPositiveInteger(
      run, "current_attempt_number", "Run list item",
    ),
    createdAt: requiredTimestamp(run, "created_at", "Run list item"),
    ...(run["started_at"] === undefined
      ? {}
      : { startedAt: requiredTimestamp(run, "started_at", "Run list item") }),
    ...(run["terminal_at"] === undefined
      ? {}
      : { terminalAt: requiredTimestamp(run, "terminal_at", "Run list item") }),
  }
  if (runStatusIsTerminal(status) !== (item.terminalAt !== undefined)) {
    throw new Error("Run list item.terminal_at is inconsistent with status")
  }
  return Object.freeze(item)
}

class ClientTokens implements ClientTokensApi {
  readonly #transport: ClientTransport

  constructor(transport: ClientTransport) {
    this.#transport = transport
  }

  async create(
    request: TokenCreateRequest = {},
    options: RequestOptions = {},
  ): Promise<TokenCreateResult> {
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
    return token as TokenCreateResult
  }

  async retrieve(
    id: string,
    options: RequestOptions = {},
  ): Promise<Token> {
    return parseToken(
      await this.#transport.request(
        "GET",
        `/v1/tokens/${encodeURIComponent(resourceID(id, "Token ID"))}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
      false,
    )
  }

  async list(
    queryInput: TokenListQuery = {},
    options: RequestOptions = {},
  ): Promise<CursorPage<TokenListItem>> {
    const query = new URLSearchParams()
    if (queryInput.status !== undefined) query.set("status", tokenStatus(queryInput.status))
    if (queryInput.cursor !== undefined) {
      if (queryInput.cursor.length === 0) throw new Error("Token cursor is required")
      query.set("cursor", queryInput.cursor)
    }
    if (queryInput.limit !== undefined) {
      if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
        throw new Error("Token limit must be an integer in [1,100]")
      }
      query.set("limit", String(queryInput.limit))
    }
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
      items: Object.freeze(response["tokens"].map(parseTokenListItem)),
      ...(nextCursor === undefined ? {} : { nextCursor }),
    })
  }

  async complete(
    id: string,
    request: TokenCompleteRequest,
    options: RequestOptions = {},
  ): Promise<Token> {
    return parseToken(
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
      false,
    )
  }

  async cancel(
    id: string,
    request: TokenCancelRequest = {},
    options: RequestOptions = {},
  ): Promise<Token> {
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
    workspace: { id: workspaceRefID(request.workspace) },
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
  const payload = body["error"] !== null && typeof body["error"] === "object" &&
      !Array.isArray(body["error"])
    ? body["error"] as Record<string, unknown>
    : {}
  const message = typeof payload["message"] === "string" && payload["message"] !== ""
    ? payload["message"]
    : `Helmr request failed with status ${response.status}`
  const code = typeof payload["code"] === "string" && payload["code"] !== ""
    ? payload["code"]
    : responseStatusCode(response.status)
  const details = payload["details"] !== null && typeof payload["details"] === "object" &&
      !Array.isArray(payload["details"])
    ? payload["details"] as Readonly<Record<string, JsonValue>>
    : undefined
  const requestId = response.headers.get("X-Request-ID")?.trim() || undefined
  const error = new Error(message) as Error & {
    code: string
    details?: Readonly<Record<string, JsonValue>>
    requestId?: string
  }
  error.name = "HelmrError"
  error.code = code
  if (details !== undefined) error.details = details
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
    case 405:
      return "method_not_allowed"
    case 409:
      return "conflict"
    case 410:
      return "gone"
    case 413:
      return "request_too_large"
    case 422:
      return "unprocessable_entity"
    case 429:
      return "rate_limited"
    case 501:
      return "not_implemented"
    case 502:
      return "bad_gateway"
    case 503:
      return "service_unavailable"
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

function parseRun<TOutput extends JsonValue = JsonValue>(
  value: unknown,
): Run<TOutput> {
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
  const runResource: Run<TOutput> = {
    ...createRunHandle<TOutput>(
      resourceID(requiredStringFrom(run, "id", "Run response"), "Run response.id"),
    ),
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
    currentAttemptNumber: requiredPositiveInteger(
      run, "current_attempt_number", "Run response",
    ),
    cause,
    metadata: Object.freeze({ ...metadata }),
    tags: Object.freeze([...tags]) as readonly string[],
    ...(run["output"] === undefined ? {} : { output: run["output"] as TOutput }),
    ...(run["failure"] === undefined ? {} : { failure: parseRunFailure(run["failure"]) }),
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
      runResource.failure !== undefined ||
      runResource.terminalAt === undefined
    ) {
      throw new Error("Succeeded Run response has an invalid terminal projection")
    }
  } else if (runStatusIsTerminal(status)) {
    if (
      runResource.output !== undefined ||
      runResource.failure === undefined ||
      runResource.terminalAt === undefined
    ) {
      throw new Error("Terminal Run response has an invalid failure projection")
    }
  } else if (
    runResource.output !== undefined ||
    runResource.failure !== undefined ||
    runResource.terminalAt !== undefined
  ) {
    throw new Error("Active Run response has terminal fields")
  }
  return Object.freeze(runResource)
}

function parseRunCause(value: unknown): Run["cause"] {
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
        scheduledAt: requiredTimestamp(cause, "scheduled_at", "Run response.cause"),
        ...(cause["last_scheduled_at"] === undefined
          ? {}
          : {
              lastScheduledAt: requiredTimestamp(
                cause, "last_scheduled_at", "Run response.cause",
              ),
            }),
        timezone: requiredStringFrom(cause, "timezone", "Run response.cause"),
      })
    default:
      throw new Error("Run response.cause.type is invalid")
  }
}

function parseRunFailure(value: unknown): RunFailure {
  const source = objectValue(value, "Run response.failure")
  const details = objectValue(source["details"], "Run response.failure.details")
  return Object.freeze({
    code: requiredStringFrom(source, "code", "Run response.failure"),
    message: requiredStringFrom(source, "message", "Run response.failure"),
    details: Object.freeze({ ...details }) as Readonly<Record<string, JsonValue>>,
  })
}

function runTelemetryQuery(
  cursor: string | undefined,
  limit: number | undefined,
  filterName: "level" | "severity",
  filter: RunLogLevel | readonly RunLogLevel[] | undefined,
): string {
  const query = new URLSearchParams()
  if (cursor !== undefined) {
    if (cursor.length === 0) throw new Error("Run telemetry cursor is required")
    query.set("cursor", cursor)
  }
  if (limit !== undefined) {
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
      throw new Error("Run telemetry limit must be an integer in [1,200]")
    }
    query.set("limit", String(limit))
  }
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

function parseToken(value: unknown, credentials: boolean): Token | TokenCreateResult {
  const input = objectValue(value, "Token response")
  const status = tokenStatus(input["status"])
  const metadata = objectValue(input["metadata"], "Token response.metadata") as Metadata
  const tags = input["tags"]
  if (!Array.isArray(tags) || tags.some((tag) => typeof tag !== "string")) {
    throw new Error("Token response.tags must be an array of strings")
  }
  const token: Token = {
    id: resourceID(requiredString(input, "id"), "Token response.id"),
    status,
    ...(input["result"] === undefined ? {} : { result: input["result"] as JsonValue }),
    metadata,
    tags: Object.freeze([...tags]) as readonly string[],
    timeoutAt: requiredTimestamp(input, "timeout_at", "Token response"),
    ...(input["completed_at"] === undefined
      ? {}
      : { completedAt: requiredTimestamp(input, "completed_at", "Token response") }),
    createdAt: requiredTimestamp(input, "created_at", "Token response"),
    updatedAt: requiredTimestamp(input, "updated_at", "Token response"),
  }
  if (!credentials) return Object.freeze(token)
  return Object.freeze({
    ...token,
    callbackUrl: requiredString(input, "callback_url"),
    publicAccessToken: requiredString(input, "public_access_token"),
  }) as TokenCreateResult
}

function parseTokenListItem(value: unknown): TokenListItem {
  const token = objectValue(value, "Token list item")
  const tags = token["tags"]
  if (!Array.isArray(tags) || tags.some((tag) => typeof tag !== "string")) {
    throw new Error("Token list item.tags must be an array of strings")
  }
  return Object.freeze({
    id: resourceID(requiredStringFrom(token, "id", "Token list item"), "Token list item.id"),
    status: tokenStatus(token["status"]),
    tags: Object.freeze([...tags]) as readonly string[],
    timeoutAt: requiredTimestamp(token, "timeout_at", "Token list item"),
    ...(token["completed_at"] === undefined
      ? {}
      : { completedAt: requiredTimestamp(token, "completed_at", "Token list item") }),
    createdAt: requiredTimestamp(token, "created_at", "Token list item"),
    updatedAt: requiredTimestamp(token, "updated_at", "Token list item"),
  })
}

function tokenStatus(value: unknown): TokenStatus {
  if (value !== "pending" && value !== "completed" && value !== "cancelled" && value !== "expired") {
    throw new Error("Token status is invalid")
  }
  return value
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

function requiredTimestamp(
  value: Record<string, unknown>,
  field: string,
  label: string,
): string {
  return timestampString(value[field], `${label}.${field}`)
}
