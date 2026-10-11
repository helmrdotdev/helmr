import { normalizeInput } from "./content"
import { waitForTurn } from "./internal/turn-wait"
import { parseAsk, parseAskPage, parseAskReceipt } from "./internal/asks"
import { sessionEvents } from "./internal/session-events"
import type {
  CursorPage,
  SessionRef,
  TurnRef,
  Session,
  SessionStatus,
  TurnListQuery,
  TurnState,
  TurnWaitOptions,
  TimedTurnWaitResult,
  TurnOutcome,
  SessionInterruptReceipt,
  SessionOperationOptions,
} from "./contract"
import { resourceID } from "./internal/id"
import { canonicalizeJsonValue } from "./internal/jsoncanon"
import {
  parseSession,
  parseSessionPage,
  parseTurnState,
  parseTurnAdmission,
  parseSessionAdmissionReceipt,
  parseSessionMessageReceipt,
  parseSessionCloseReceipt,
  parseSessionCancelReceipt,
  parseSessionInterruptReceipt,
  parseSessionResumeReceipt,
  parseSessionEventPage,
  sessionStatus,
} from "./internal/session"
import { sessionOperationOptions } from "./session"
import type { RequestOptions } from "./request"

export type SessionListQuery =
  | Readonly<{
      agentId?: never
      parentSessionId?: string
      requesterSessionId?: string
      key?: never
      status?: SessionStatus | readonly SessionStatus[]
      cursor?: string
      limit?: number
    }>
  | Readonly<{
      agentId: string
      parentSessionId?: never
      requesterSessionId?: never
      key: string
      status?: never
      cursor?: never
      limit?: never
    }>

export interface ClientSessionRef extends SessionRef {
  interrupt(request?: SessionOperationOptions, options?: RequestOptions): Promise<SessionInterruptReceipt>
  readonly turns: Readonly<{
    list(
      query?: TurnListQuery,
      options?: RequestOptions,
    ): Promise<CursorPage<TurnState>>
  }>
}
export interface ClientSessionsApi {
  retrieve(id: string, options?: RequestOptions): Promise<Session>
  list(
    query?: SessionListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<Session>>
  get(id: string): ClientSessionRef
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
      return parseSessionPage(await transport.request(
        "GET", `/v1/sessions${sessionListQuery(queryInput)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    get(id) {
      return createSessionRef(id, transport)
    },
  } satisfies ClientSessionsApi)
}
export function createSessionRef(
  id: string,
  transport: SessionTransport,
): ClientSessionRef {
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
    function wait(options: TurnWaitOptions & { readonly timeout: string | number }): Promise<TimedTurnWaitResult>
    function wait(options?: TurnWaitOptions & { readonly timeout?: undefined }): Promise<TurnOutcome>
    function wait(options: TurnWaitOptions): Promise<TurnOutcome | TimedTurnWaitResult>
    function wait(options: TurnWaitOptions = {}): Promise<TurnOutcome | TimedTurnWaitResult> {
      return waitForTurn(sessionId, turnId, async signal => parseTurnState(await get(path, { signal })), options)
    }
    return Object.freeze({
      id: turnId,
      sessionId,
      wait,
      asks: Object.freeze({
        async list(query = {}, options) {
          const params = new URLSearchParams()
          if (query.cursor !== undefined) params.set("cursor", query.cursor)
          if (query.limit !== undefined) {
            if (!Number.isInteger(query.limit) || query.limit < 1 || query.limit > 100) throw new Error("Ask limit must be between 1 and 100")
            params.set("limit", String(query.limit))
          }
          return parseAskPage(await get(path + "/asks" + (params.size ? `?${params}` : ""), options))
        },
        async get(askId, options) { return parseAsk(await get(path + "/asks/" + encodeURIComponent(resourceID(askId, "Ask ID")), options)) },
        async respond(askId, request, options) {
          canonicalizeJsonValue(request.answer)
          if (!request.responseId || !request.responseId.trim() || new TextEncoder().encode(request.responseId).length > 512 || request.responseId.includes("\0")) throw new Error("Invalid response ID")
          return parseAskReceipt(await post(path + "/asks/" + encodeURIComponent(resourceID(askId, "Ask ID")) + "/respond", { answer: request.answer, response_id: request.responseId }, options))
        },
      } satisfies import("./contract").TurnAsks),
      async send(data, request, options) {
        normalizeInput(data)
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
    } satisfies TurnRef)
  }
  return Object.freeze({
    id: sessionId,
    turn,
    async send(data, request, options) {
      normalizeInput(data)
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
    async enqueue(input, request, options) {
      normalizeInput(input)
      const receipt = parseTurnAdmission(
        await post(
          "/enqueue",
          {
            input,
            idempotency_key: sessionOperationOptions(request).idempotencyKey,
          },
          options,
        ),
      )
      if (receipt.sessionId !== sessionId)
        throw new Error("Turn admission belongs to another Session")
      return turn(receipt.turnId)
    },
    turns: Object.freeze({
      async list(query = {}, options) {
        const params = new URLSearchParams()
        if (query.cursor !== undefined) {
          if (
            !/^[0-9]+$/.test(query.cursor) ||
            !Number.isSafeInteger(Number(query.cursor))
          )
            throw new Error("Turn cursor is invalid")
          params.set("cursor", query.cursor)
        }
        if (query.limit !== undefined) {
          if (
            !Number.isInteger(query.limit) ||
            query.limit < 1 ||
            query.limit > 100
          )
            throw new Error("Turn limit must be an integer in [1,100]")
          params.set("limit", String(query.limit))
        }
        const response = objectValue(
          await get(`/turns${params.size ? `?${params}` : ""}`, options),
          "Turn list response",
        )
        if (!Array.isArray(response["turns"]))
          throw new Error("Turn list response.turns must be an array")
        const nextCursor = response["next_cursor"]
        if (nextCursor !== undefined && typeof nextCursor !== "string")
          throw new Error("Turn list cursor must be a string")
        return Object.freeze({
          items: Object.freeze(response["turns"].map(parseTurnState)),
          ...(nextCursor === undefined ? {} : { nextCursor }),
        })
      },
    }),
    events: sessionEvents(async (query = {}, options) => {
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
    }),
    async retrieve(options) {
      return parseSession(await get("", options))
    },
    async interrupt(request, options) {
      const receipt = parseSessionInterruptReceipt(await post("/interrupt", {
        idempotency_key: sessionOperationOptions(request).idempotencyKey,
      }, options))
      if (receipt.sessionId !== sessionId) throw new Error("Interrupt receipt belongs to another Session")
      return receipt
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
  } satisfies ClientSessionRef)
}

function sessionListQuery(queryInput: SessionListQuery): string {
  const exact = queryInput.agentId !== undefined || queryInput.key !== undefined
  if (
    exact &&
    (queryInput.agentId === undefined || queryInput.key === undefined)
  ) {
    throw new Error("Session exact key lookup requires agentId and key")
  }
  if (
    exact &&
    (queryInput.status !== undefined ||
      queryInput.cursor !== undefined ||
      queryInput.limit !== undefined ||
      queryInput.parentSessionId !== undefined ||
      queryInput.requesterSessionId !== undefined)
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
  if (queryInput.agentId !== undefined) {
    resourceID(queryInput.agentId, "Session Agent ID")
    query.set("agent_id", queryInput.agentId)
  }
  if (queryInput.key !== undefined) {
    if (queryInput.key.length === 0) throw new Error("Session key is required")
    query.set("key", queryInput.key)
  }
  if (queryInput.parentSessionId !== undefined) query.set("parent_session_id", resourceID(queryInput.parentSessionId, "Parent Session ID"))
  if (queryInput.requesterSessionId !== undefined) query.set("requester_session_id", resourceID(queryInput.requesterSessionId, "Requester Session ID"))
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
