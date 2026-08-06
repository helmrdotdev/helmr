import type {
  CursorPage,
  JsonValue,
  SessionCloseRequest,
  SessionCloseReceipt,
  SessionInputRecord,
  SessionInputSendRequest,
  SessionOutputPage,
  SessionOutputQuery,
  SessionRef,
  Session,
} from "./contract"
import { resourceID } from "./internal/id"
import {
  parseSession,
  parseSessionInputRecord,
  parseSessionOutputRecord,
} from "./internal/session"
import { timestampString } from "./internal/timestamp"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"

export type SessionListQuery =
  | Readonly<{
      actorId?: never
      key?: never
      cursor?: string
      limit?: number
    }>
  | Readonly<{
      actorId: string
      key: string
      cursor?: never
      limit?: never
    }>

export interface ClientSessionsApi {
  retrieve(id: string, options?: RequestOptions): Promise<Session>
  list(
    query?: SessionListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<Session>>
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
    ): Promise<Session> {
      const sessionID = resourceID(id, "Session ID")
      return parseSession(await transport.request(
        "GET",
        `/v1/sessions/${encodeURIComponent(sessionID)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: SessionListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<Session>> {
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
        items: Object.freeze(response["sessions"].map(parseSession)),
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
  ): Promise<SessionOutputPage> => {
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
    return Object.freeze({
      records: Object.freeze(response["records"].map(parseSessionOutputRecord)),
      nextAfter: safeSequence(response["next_after"], "Session output next_after"),
      hasMore: requiredBoolean(response, "has_more", "Session output response"),
    })
  }
  const output: SessionRef["output"] = Object.freeze({
    async list(
      query: SessionOutputQuery = {},
      options: RequestOptions = {},
    ): Promise<SessionOutputPage> {
      return readOutputPage(query, options)
    },
  })
  return Object.freeze({
    id: sessionID,
    input: Object.freeze({
      async send(
        input: JsonValue,
        request: SessionInputSendRequest = {},
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
    async retrieve(options: RequestOptions = {}): Promise<Session> {
      return parseSession(await transport.request(
        "GET",
        basePath,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async close(
      request: SessionCloseRequest = {},
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
        acceptedAt: timestampString(response["accepted_at"], "Session close response.accepted_at"),
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

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
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
