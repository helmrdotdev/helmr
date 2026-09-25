import type {
  CursorPage,
  SessionRef,
  TurnRef,
  Session,
  SessionStatus,
} from "./contract"
import { resourceID } from "./internal/id"
import { canonicalizeJsonValue } from "./internal/jsoncanon"
import {
  parseSession,
  parseTurnState,
  parseSessionAdmissionReceipt,
  parseSessionMessageReceipt,
  parseSessionCloseReceipt, parseSessionCancelReceipt,
  parseTurnInterruptReceipt,
  parseSessionResumeReceipt,
  parseSessionEventPage,
  sessionStatus,
} from "./internal/session"
import { sessionOperationOptions } from "./session"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"

export type SessionListQuery =
  | Readonly<{
      actorId?: never
      key?: never
      status?: SessionStatus | readonly SessionStatus[]
      cursor?: string
      limit?: number
    }>
  | Readonly<{
      actorId: string
      key: string
      status?: never
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
    retrieve(id, options) {
      return createSessionRef(id, transport).retrieve(options)
    },
    async list(queryInput = {}, options = {}) {
      const response = objectValue(
        await transport.request(
          "GET",
          `/v1/sessions${sessionListQuery(queryInput)}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
        "Session list response",
      )
      if (!Array.isArray(response["sessions"]))
        throw new Error("Session list response.sessions must be an array")
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string")
        throw new Error("Session list response.next_cursor must be a string")
      return Object.freeze({
        items: Object.freeze(response["sessions"].map(parseSession)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    ref(id) {
      return createSessionRef(id, transport)
    },
  } satisfies ClientSessionsApi)
}
export function createSessionRef(
  id: string,
  transport: SessionTransport,
): SessionRef {
  const sessionId = resourceID(id, "Session ID"),
    basePath = `/v1/sessions/${encodeURIComponent(sessionId)}`
  const post = (suffix: string, body: unknown, options?: RequestOptions) =>
    transport.request("POST", basePath + suffix, {
      body,
      ...(options?.signal === undefined ? {} : { signal: options.signal }),
    })
  const get = (suffix: string, options?: RequestOptions) =>
    transport.request(
      "GET",
      basePath + suffix,
      options?.signal === undefined ? {} : { signal: options.signal },
    )
  const turn = (id: string): TurnRef => {
    const turnId = resourceID(id, "Turn ID"),
      path = `/turns/${encodeURIComponent(turnId)}`
    return Object.freeze({
      id: turnId,
      sessionId,
      async send(data, request, options) {
        canonicalizeJsonValue(data)
        const receipt = parseSessionMessageReceipt(
          await post(
            path + "/messages",
            {
              data,
              idempotency_key: sessionOperationOptions(request).idempotencyKey,
            },
            options,
          ),
        )
        return Object.freeze({ id: receipt.messageId, status: receipt.status })
      },
      async retrieve(options) {
        return parseTurnState(await get(path, options))
      },
      async interrupt(request, options) {
        return parseTurnInterruptReceipt(
          await post(
            path + "/interrupt",
            {
              idempotency_key: sessionOperationOptions(request).idempotencyKey,
            },
            options,
          ),
        )
      },
    } satisfies TurnRef)
  }
  return Object.freeze({
    id: sessionId,
    turn,
    async send(data, request, options) {
      canonicalizeJsonValue(data)
      const receipt = parseSessionAdmissionReceipt(
        await post(
          "/send",
          {
            data,
            idempotency_key: sessionOperationOptions(request).idempotencyKey,
          },
          options,
        ),
      )
      return receipt.kind === "enqueued"
        ? Object.freeze({ kind: receipt.kind, turn: turn(receipt.turnId) })
        : Object.freeze({
            kind: receipt.kind,
            turn: turn(receipt.turnId),
            message: Object.freeze({
              id: receipt.messageId,
              status: "accepted" as const,
            }),
          })
    },
    async enqueue(data, request, options) {
      canonicalizeJsonValue(data)
      const receipt = parseSessionAdmissionReceipt(
        await post(
          "/enqueue",
          {
            data,
            idempotency_key: sessionOperationOptions(request).idempotencyKey,
          },
          options,
        ),
      )
      if (receipt.kind !== "enqueued")
        throw new Error("Session enqueue response must admit a Turn")
      return turn(receipt.turnId)
    },
    events: Object.freeze({
      async list(query = {}, options) {
        const after = query.after ?? 0,
          limit = query.limit ?? 100
        if (!Number.isSafeInteger(after) || after < 0)
          throw new Error(
            "Session events after must be a non-negative safe integer",
          )
        if (!Number.isInteger(limit) || limit < 1 || limit > 1000)
          throw new Error("Session events limit must be an integer in [1,1000]")
        return parseSessionEventPage(
          await get(`/events?after=${after}&limit=${limit}`, options),
        )
      },
    }),
    async retrieve(options) {
      return parseSession(await get("", options))
    },
    async close(request, options) {
      return parseSessionCloseReceipt(
        await post(
          "/close",
          { idempotency_key: sessionOperationOptions(request).idempotencyKey },
          options,
        ),
      )
    },
    async cancel(request, options) {
      return parseSessionCancelReceipt(
        await post(
          "/cancel",
          { idempotency_key: sessionOperationOptions(request).idempotencyKey },
          options,
        ),
      )
    },
    async resume(request, options) {
      return parseSessionResumeReceipt(
        await post(
          "/resume",
          {
            hold_id: resourceID(request.holdId, "holdId"),
            idempotency_key: sessionOperationOptions(request).idempotencyKey,
          },
          options,
        ),
      )
    },
  } satisfies SessionRef)
}

function sessionListQuery(queryInput: SessionListQuery): string {
  const exact = queryInput.actorId !== undefined || queryInput.key !== undefined
  if (
    exact &&
    (queryInput.actorId === undefined || queryInput.key === undefined)
  ) {
    throw new Error("Session exact key lookup requires actorId and key")
  }
  if (
    exact &&
    (queryInput.status !== undefined ||
      queryInput.cursor !== undefined ||
      queryInput.limit !== undefined)
  ) {
    throw new Error(
      "Session exact key lookup does not accept status, cursor or limit",
    )
  }
  const query = new URLSearchParams()
  const statuses =
    queryInput.status === undefined
      ? []
      : Array.isArray(queryInput.status)
        ? queryInput.status
        : [queryInput.status]
  for (const status of statuses) {
    query.append("status", sessionStatus(status, "Session list status"))
  }
  if (queryInput.actorId !== undefined) {
    validateTaskId(queryInput.actorId)
    query.set("actor_id", queryInput.actorId)
  }
  if (queryInput.key !== undefined) {
    if (queryInput.key.length === 0) throw new Error("Session key is required")
    query.set("key", queryInput.key)
  }
  if (queryInput.cursor !== undefined) {
    if (queryInput.cursor.length === 0)
      throw new Error("Session cursor is required")
    query.set("cursor", queryInput.cursor)
  }
  if (queryInput.limit !== undefined) {
    if (
      !Number.isInteger(queryInput.limit) ||
      queryInput.limit < 1 ||
      queryInput.limit > 100
    ) {
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
