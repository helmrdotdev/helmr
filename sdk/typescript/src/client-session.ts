import { normalizeSubmission } from "./question"
import type { AgentContract, AgentInput, AgentResult } from "./agent"
import { retentionMilliseconds, retentionFromMilliseconds, type Retention } from "./duration"
import { slackStartOption } from "./client-slack"
import type { JsonValue } from "./contract"
import { normalizeInput, normalizeInvocation } from "./content"
import { waitForTurn } from "./internal/turn-wait"
import { parseAsk, parseAskPage, parseAskReceipt } from "./internal/asks"
import { sessionEvents } from "./internal/session-events"
import type {
  CursorPage,
  SessionRef,
  TurnRef,
  Session,
  SessionStatus,
  TurnWaitOptions,
  TimedTurnWaitResult,
  TurnOutcome,
} from "./contract"
import { validateDefinitionId } from "./schema/definition"
import { resourceID } from "./internal/id"
import { canonicalizeJsonValue } from "./internal/jsoncanon"
import {
  parseSession,
  parseSessionPage,
  parseTurnState,
  parseTurnAdmission,
  parseSessionMessageReceipt,
  parseSessionCloseReceipt,
  parseSessionCancelReceipt,
  parseSessionInterruptReceipt,
  parseTurnInterruptReceipt,
  parseSessionResumeReceipt,
  parseSessionEventPage,
  sessionStatus,
} from "./internal/session"
import { sessionOperationOptions } from "./session"
import type { RequestOptions } from "./request"

type SessionAgentSelector = { agent: string }

export type SessionListQuery = Readonly<
  | ((SessionAgentSelector | { agent?: never }) & {
      key?: never
      parentSessionId?: string
      requesterSessionId?: string
      status?: SessionStatus | readonly SessionStatus[]
      cursor?: string
      limit?: number
    })
  | (SessionAgentSelector & {
      key: string
      parentSessionId?: never
      requesterSessionId?: never
      status?: never
      cursor?: never
      limit?: never
    })
>

type DynamicAgent = AgentContract<string, JsonValue, JsonValue>
export interface SessionCreateRequest<A extends AgentContract = DynamicAgent> {
  readonly agent: A["id"]
  readonly key?: string
  readonly input?: AgentInput<A>
  readonly computer?: Readonly<{ id: string }>
  readonly closeAfterIdle?: Retention
  readonly slack?: Readonly<{ channelId: string }>
  readonly idempotencyKey?: string
}
export interface SessionCreateReceipt<T extends TurnRef<any> | null = TurnRef | null, I = JsonValue, O = JsonValue> {
  readonly session: SessionRef<I, O>
  readonly turn: T
  readonly closeAfterIdle: Retention
}
export interface ClientSessionsApi {
  create<A extends AgentContract = DynamicAgent>(request: SessionCreateRequest<A> & { readonly input: AgentInput<A> & ({} | null) }, options?: RequestOptions): Promise<SessionCreateReceipt<TurnRef<AgentResult<A>>, AgentInput<A>, AgentResult<A>>>
  create<A extends AgentContract = DynamicAgent>(request: SessionCreateRequest<A> & { readonly input?: undefined }, options?: RequestOptions): Promise<SessionCreateReceipt<null, AgentInput<A>, AgentResult<A>>>
  create<A extends AgentContract = DynamicAgent>(request: SessionCreateRequest<A>, options?: RequestOptions): Promise<SessionCreateReceipt<TurnRef<AgentResult<A>> | null, AgentInput<A>, AgentResult<A>>>
  /** Attach only to the deployment matching the imported contract. */
  attach<A extends AgentContract>(request: { readonly id: string; readonly agent: A["id"]; readonly deploymentId: string }, options?: RequestOptions): Promise<SessionRef<AgentInput<A>, AgentResult<A>>>

  get(id: string, options?: RequestOptions): Promise<Session>
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
  async function create(request: SessionCreateRequest<AgentContract<string, any, any>>, options?: RequestOptions): Promise<SessionCreateReceipt<any, any, any>> {
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(request.agent)) throw new Error("Invalid Agent key")
    if (request.input !== undefined) {
      normalizeInvocation(request.input)
    }
    const value = objectValue(await transport.request("POST", "/v1/sessions", {
      body: {
        agent: request.agent,
        ...(request.input === undefined ? {} : { input: request.input }),
        ...(request.key === undefined ? {} : { key: request.key }),
        ...(request.computer === undefined ? {} : { computer_id: resourceID(request.computer.id, "Computer ID") }),
        ...(request.closeAfterIdle === undefined ? {} : { close_after_idle_ms: retentionMilliseconds(request.closeAfterIdle, "closeAfterIdle") }),
        ...(request.slack === undefined ? {} : { slack: slackStartOption(request.slack) }),
        idempotency_key: sessionOperationOptions(request).idempotencyKey,
      },
      ...(options?.signal === undefined ? {} : { signal: options.signal }),
    }), "Session create response")
    const session = createSessionRef(resourceID(value["session_id"], "Session ID"), transport)
    const admission = value["turn"] === null ? null : parseTurnAdmission(value["turn"])
    if (admission !== null && admission.sessionId !== session.id) throw new Error("Initial Turn belongs to another Session")
    if ((request.input === undefined) !== (admission === null)) throw new Error("Session create response has inconsistent initial Turn")
    return Object.freeze({session, turn: admission === null ? null : session.turns.ref(admission.turnId), closeAfterIdle: retentionFromMilliseconds(value["close_after_idle_ms"], "closeAfterIdle")})
  }
  return Object.freeze({
    create,
    async attach<A extends AgentContract>(request: { readonly id: string; readonly agent: A["id"]; readonly deploymentId: string }, options?: RequestOptions): Promise<SessionRef<AgentInput<A>, AgentResult<A>>> {
      validateDefinitionId(request.agent)
      const deploymentId = resourceID(request.deploymentId, "Deployment ID")
      const ref = createSessionRef(request.id, transport)
      const session = await ref.get(options)
      if (session.agent !== request.agent || session.deploymentId !== deploymentId) throw new Error("Session does not match the agent contract and deployment")
      return ref as SessionRef<AgentInput<A>, AgentResult<A>>
    },
    get(id, options) {
      return createSessionRef(id, transport).get(options)
    },
    async list(queryInput = {}, options = {}) {
      return parseSessionPage(await transport.request(
        "GET", `/v1/sessions${sessionListQuery(queryInput)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    ref(id) {
      return createSessionRef(id, transport)
    },
  } satisfies ClientSessionsApi)
}
function createSessionRef(
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
    function wait(options: TurnWaitOptions & { readonly timeout: import("./duration").Duration }): Promise<TimedTurnWaitResult>
    function wait(options?: TurnWaitOptions & { readonly timeout?: undefined }): Promise<TurnOutcome>
    function wait(options: TurnWaitOptions): Promise<TurnOutcome | TimedTurnWaitResult>
    function wait(options: TurnWaitOptions = {}): Promise<TurnOutcome | TimedTurnWaitResult> {
      return waitForTurn(sessionId, turnId, async signal => parseTurnState(await get(path, { signal })), options)
    }
    return Object.freeze({
      id: turnId,
      sessionId,
      async interrupt(request, options) {
        const receipt = parseTurnInterruptReceipt(await post(path + "/interrupt", { idempotency_key: sessionOperationOptions(request).idempotencyKey }, options))
        if (receipt.sessionId !== sessionId || receipt.turnId !== turnId) throw new Error("Interrupt receipt belongs to another Turn")
        return receipt
      },
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
        async answer(askId, request, options) {
          canonicalizeJsonValue(request.answer)
          if (!request.answerId || !request.answerId.trim() || new TextEncoder().encode(request.answerId).length > 512 || request.answerId.includes("\0")) throw new Error("Invalid answer ID")
          return parseAskReceipt(await post(path + "/asks/" + encodeURIComponent(resourceID(askId, "Ask ID")) + "/answer", { answer: normalizeSubmission(request.answer), answer_id: request.answerId }, options))
        },
      } satisfies import("./contract").TurnAsks),
      async send(message, request, options) {
        normalizeInput(message)
        const receipt = parseSessionMessageReceipt(
          await post(
            path + "/messages",
            {
              message,
              idempotency_key: sessionOperationOptions(request).idempotencyKey,
            },
            options,
          ),
        )
        return Object.freeze({ id: receipt.messageId, status: receipt.status })
      },
      async get(options) {
        return parseTurnState(await get(path, options))
      },
    } satisfies TurnRef)
  }
  return Object.freeze({
    id: sessionId,
    async enqueue(input, request, options) {
      normalizeInvocation(input)
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
      ref: turn,
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
    async get(options) {
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
  } satisfies SessionRef)
}

function sessionListQuery(queryInput: SessionListQuery): string {
  for (const key of Object.keys(queryInput)) {
    if (!["agent", "key", "status", "cursor", "limit", "parentSessionId", "requesterSessionId"].includes(key)) throw new Error(`Unsupported Session list query: ${key}`)
  }
  const exact = queryInput.key !== undefined
  if (
    exact &&
    queryInput.agent === undefined
  ) {
    throw new Error("Session exact key lookup requires agent with key")
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
  if (queryInput.agent !== undefined) {
    validateDefinitionId(queryInput.agent)
    query.set("agent", queryInput.agent)
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
