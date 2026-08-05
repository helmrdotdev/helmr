import type { CursorPage, JsonValue } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"

export type SessionStatus = "open" | "closed" | "cancelled" | "failed"

export interface SessionFailure {
  readonly code:
    | "no_progress"
    | "run_failed"
    | "run_expired"
    | "platform_failure"
  readonly runId: string
}

export interface SessionSnapshot {
  readonly id: string
  readonly actorId: string
  readonly deploymentId: string
  readonly key?: string
  readonly status: SessionStatus
  readonly createdAt: Date
  readonly updatedAt: Date
  readonly currentRunId?: string
  readonly failure?: SessionFailure
}

export type SessionInputSource =
  | Readonly<{ kind: "external" }>
  | Readonly<{ kind: "run"; runId: string }>

export interface SessionInputRecord {
  readonly id: string
  readonly sequence: number
  readonly data: JsonValue
  readonly source: SessionInputSource
  readonly createdAt: Date
}

export interface SessionOutputRecord {
  readonly id: string
  readonly sequence: number
  readonly data: unknown
  readonly contentType: string
  readonly createdAt: Date
  readonly provenance: Readonly<{
    runId: string
    attemptNumber: number
    deploymentId: string
  }>
}

export interface SessionCloseReceipt {
  readonly sessionId: string
  readonly acceptedAt: Date
}

export interface SessionOutputQuery {
  readonly after?: number
  readonly limit?: number
}

export interface SessionRef {
  readonly id: string
  readonly input: Readonly<{
    send(
      input: JsonValue,
      request?: Readonly<{ idempotencyKey?: string }>,
      options?: RequestOptions,
    ): Promise<SessionInputRecord>
  }>
  readonly output: Readonly<{
    list(
      query?: SessionOutputQuery,
      options?: RequestOptions,
    ): Promise<readonly SessionOutputRecord[]>
    read(
      query?: SessionOutputQuery,
      options?: RequestOptions,
    ): AsyncIterable<SessionOutputRecord>
  }>
  close(
    request?: Readonly<{ idempotencyKey?: string }>,
    options?: RequestOptions,
  ): Promise<SessionCloseReceipt>
}

export interface SessionListQuery {
  readonly cursor?: string
  readonly limit?: number
  readonly actorId?: string
  readonly key?: string
}

export interface ClientSessionsApi {
  retrieve(id: string, options?: RequestOptions): Promise<SessionSnapshot>
  list(
    query?: SessionListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<SessionSnapshot>>
  ref(id: string): SessionRef
}

interface SessionTransport {
  request(
    method: "GET" | "POST",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientSessions(
  transport: SessionTransport,
): ClientSessionsApi {
  return Object.freeze({
    async retrieve(
      id: string,
      options: RequestOptions = {},
    ): Promise<SessionSnapshot> {
      const sessionID = resourceID(id, "Session ID")
      return parseSessionSnapshot(await transport.request(
        "GET",
        `/v1/sessions/${encodeURIComponent(sessionID)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: SessionListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<SessionSnapshot>> {
      const query = sessionListQuery(queryInput)
      const response = objectValue(
        await transport.request("GET", `/v1/sessions${query}`, {
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        }),
        "Session list response",
      )
      if (!Array.isArray(response["sessions"])) {
        throw new Error("Session list response.sessions must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Session list response.next_cursor must be a string")
      }
      return Object.freeze({
        items: Object.freeze(response["sessions"].map(parseSessionSnapshot)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    ref(id: string): SessionRef {
      return createSessionRef(resourceID(id, "Session ID"), transport)
    },
  })
}

export function createSessionRef(
  id: string,
  transport: SessionTransport,
): SessionRef {
  const sessionID = resourceID(id, "Session ID")
  const basePath = `/v1/sessions/${encodeURIComponent(sessionID)}`
  const readOutputPage = async (
    queryInput: SessionOutputQuery,
    options: RequestOptions,
  ): Promise<{
    records: readonly SessionOutputRecord[]
    nextAfter: number
    hasMore: boolean
  }> => {
    const query = new URLSearchParams()
    if (queryInput.after !== undefined) {
      query.set("after", safeSequence(queryInput.after, "Session output after").toString())
    }
    if (queryInput.limit !== undefined) {
      if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
        throw new Error("Session output limit must be an integer in [1,100]")
      }
      query.set("limit", queryInput.limit.toString())
    }
    const suffix = query.size === 0 ? "" : `?${query.toString()}`
    const response = objectValue(
      await transport.request(
        "GET",
        `${basePath}/outputs${suffix}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
      "Session output response",
    )
    if (!Array.isArray(response["records"])) {
      throw new Error("Session output response.records must be an array")
    }
    return {
      records: Object.freeze(response["records"].map(parseSessionOutputRecord)),
      nextAfter: safeSequence(response["next_after"], "Session output next_after"),
      hasMore: requiredBoolean(response, "has_more", "Session output response"),
    }
  }
  const output: SessionRef["output"] = Object.freeze({
    async list(
      query: SessionOutputQuery = {},
      options: RequestOptions = {},
    ): Promise<readonly SessionOutputRecord[]> {
      return (await readOutputPage(query, options)).records
    },
    read(
      query: SessionOutputQuery = {},
      options: RequestOptions = {},
    ): AsyncIterable<SessionOutputRecord> {
      return {
        async *[Symbol.asyncIterator]() {
          let after = query.after
          while (true) {
            options.signal?.throwIfAborted()
            const page = await readOutputPage({
              ...(after === undefined ? {} : { after }),
              ...(query.limit === undefined ? {} : { limit: query.limit }),
            }, options)
            for (const record of page.records) {
              options.signal?.throwIfAborted()
              yield record
            }
            if (!page.hasMore) return
            after = page.nextAfter
          }
        },
      }
    },
  })
  return Object.freeze({
    id: sessionID,
    input: Object.freeze({
      async send(
        input: JsonValue,
        request: Readonly<{ idempotencyKey?: string }> = {},
        options: RequestOptions = {},
      ): Promise<SessionInputRecord> {
        return parseSessionInputRecord(await transport.request(
          "POST",
          `${basePath}/inputs`,
          {
            body: {
              input,
              ...(request.idempotencyKey === undefined
                ? {}
                : { idempotency_key: request.idempotencyKey }),
            },
            ...(options.signal === undefined ? {} : { signal: options.signal }),
          },
        ))
      },
    }),
    output,
    async close(
      request: Readonly<{ idempotencyKey?: string }> = {},
      options: RequestOptions = {},
    ): Promise<SessionCloseReceipt> {
      const response = objectValue(await transport.request(
        "POST",
        `${basePath}/close`,
        {
          body: request.idempotencyKey === undefined
            ? {}
            : { idempotency_key: request.idempotencyKey },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ), "Session close response")
      return Object.freeze({
        sessionId: resourceID(response["session_id"], "Session close response.session_id"),
        acceptedAt: timestamp(response["accepted_at"], "Session close response.accepted_at"),
      })
    },
  })
}

function sessionListQuery(queryInput: SessionListQuery): string {
  const exact = queryInput.actorId !== undefined || queryInput.key !== undefined
  if (exact && (queryInput.actorId === undefined || queryInput.key === undefined)) {
    throw new Error("Session exact key lookup requires actorId and key")
  }
  if (exact && (queryInput.cursor !== undefined || queryInput.limit !== undefined)) {
    throw new Error("Session exact key lookup does not accept cursor or limit")
  }
  const query = new URLSearchParams()
  if (queryInput.actorId !== undefined) {
    validateTaskId(queryInput.actorId)
    query.set("actor_id", queryInput.actorId)
  }
  if (queryInput.key !== undefined) {
    if (queryInput.key.length === 0) throw new Error("Session key is required")
    query.set("key", queryInput.key)
  }
  if (queryInput.cursor !== undefined) {
    if (queryInput.cursor.length === 0) throw new Error("Session cursor is required")
    query.set("cursor", queryInput.cursor)
  }
  if (queryInput.limit !== undefined) {
    if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
      throw new Error("Session limit must be an integer in [1,100]")
    }
    query.set("limit", queryInput.limit.toString())
  }
  return query.size === 0 ? "" : `?${query.toString()}`
}

function parseSessionSnapshot(value: unknown): SessionSnapshot {
  const input = objectValue(value, "Session response")
  const status = input["status"]
  if (status !== "open" && status !== "closed" && status !== "cancelled" && status !== "failed") {
    throw new Error("Session response.status is invalid")
  }
  const failure = input["failure"] === undefined
    ? undefined
    : parseSessionFailure(input["failure"])
  return Object.freeze({
    id: resourceID(input["id"], "Session response.id"),
    actorId: requiredString(input, "actor_id", "Session response"),
    deploymentId: resourceID(input["deployment_id"], "Session response.deployment_id"),
    ...(input["key"] === undefined
      ? {}
      : { key: requiredString(input, "key", "Session response") }),
    status,
    createdAt: timestamp(input["created_at"], "Session response.created_at"),
    updatedAt: timestamp(input["updated_at"], "Session response.updated_at"),
    ...(input["current_run_id"] === undefined
      ? {}
      : { currentRunId: resourceID(input["current_run_id"], "Session response.current_run_id") }),
    ...(failure === undefined ? {} : { failure }),
  })
}

function parseSessionInputRecord(value: unknown): SessionInputRecord {
  const input = objectValue(value, "Session Input response")
  const source = objectValue(input["source"], "Session Input response.source")
  const kind = source["kind"]
  if (kind !== "external" && kind !== "run") {
    throw new Error("Session Input response.source.kind is invalid")
  }
  const parsedSource: SessionInputSource = kind === "external"
    ? Object.freeze({ kind })
    : Object.freeze({
        kind,
        runId: resourceID(source["run_id"], "Session Input response.source.run_id"),
      })
  return Object.freeze({
    id: resourceID(input["id"], "Session Input response.id"),
    sequence: safeSequence(input["sequence"], "Session Input response.sequence"),
    data: input["data"] as JsonValue,
    source: parsedSource,
    createdAt: timestamp(input["created_at"], "Session Input response.created_at"),
  })
}

function parseSessionFailure(value: unknown): SessionFailure {
  const input = objectValue(value, "Session failure")
  const code = requiredString(input, "code", "Session failure")
  if (code !== "no_progress" && code !== "run_failed" && code !== "run_expired" && code !== "platform_failure") {
    throw new Error("Session failure.code is invalid")
  }
  return Object.freeze({
    code,
    runId: resourceID(input["run_id"], "Session failure.run_id"),
  })
}

function parseSessionOutputRecord(value: unknown): SessionOutputRecord {
  const input = objectValue(value, "Session Output record")
  const provenance = objectValue(input["provenance"], "Session Output provenance")
  return Object.freeze({
    id: resourceID(input["id"], "Session Output record.id"),
    sequence: safeSequence(input["sequence"], "Session Output record.sequence"),
    data: input["data"],
    contentType: requiredString(input, "content_type", "Session Output record"),
    createdAt: timestamp(input["created_at"], "Session Output record.created_at"),
    provenance: Object.freeze({
      runId: resourceID(provenance["run_id"], "Session Output provenance.run_id"),
      attemptNumber: safeSequence(provenance["attempt_number"], "Session Output provenance.attempt_number"),
      deploymentId: resourceID(provenance["deployment_id"], "Session Output provenance.deployment_id"),
    }),
  })
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requiredString(value: Record<string, unknown>, field: string, label: string): string {
  const result = value[field]
  if (typeof result !== "string" || result.length === 0) {
    throw new Error(`${label}.${field} must be a non-empty string`)
  }
  return result
}

function requiredBoolean(value: Record<string, unknown>, field: string, label: string): boolean {
  const result = value[field]
  if (typeof result !== "boolean") throw new Error(`${label}.${field} must be a boolean`)
  return result
}

function safeSequence(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0) {
    throw new Error(`${label} must be a non-negative safe integer`)
  }
  return value as number
}

function timestamp(value: unknown, label: string): Date {
  if (typeof value !== "string") throw new Error(`${label} must be a timestamp`)
  const result = new Date(value)
  if (Number.isNaN(result.valueOf())) throw new Error(`${label} must be a timestamp`)
  return result
}
