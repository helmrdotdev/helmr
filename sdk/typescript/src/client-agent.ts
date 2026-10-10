import { validateDefinitionId } from "./schema/definition"
import { definitionItemQuery, definitionListQuery } from "./internal/definition-query"
import { slackStartOption } from "./client-slack"
import type { CursorPage, TurnRef } from "./contract"
import type { RequestOptions } from "./request"
import { createSessionRef, type ClientSessionRef } from "./client-session"
import { normalizeInput, type InputContent } from "./content"
import { resourceID } from "./internal/id"
import { parseTurnAdmission } from "./internal/session"
import { sessionOperationOptions } from "./session"

export interface AgentStartRequest {
  readonly slack?: Readonly<{ channelId: string }>
  readonly input: InputContent
  readonly computer?: Readonly<{ id: string }>
  readonly sessionKey?: string
  readonly idempotencyKey?: string
}
export interface AgentStartReceipt {
  readonly session: ClientSessionRef
  readonly turn: TurnRef
  readonly created: boolean
}
export interface AgentRetrieveQuery {
  readonly deploymentId?: string
}
export interface AgentListQuery extends AgentRetrieveQuery {
  readonly cursor?: string
  readonly limit?: number
}
export interface AgentListItem {
  readonly id: string
}
export interface AgentInfo extends AgentListItem {
  readonly deploymentId: string
}
export interface AgentPage extends CursorPage<AgentListItem> {
  readonly deploymentId: string
}
export interface ClientAgentsApi {
  retrieve(id: string, query?: AgentRetrieveQuery, options?: RequestOptions): Promise<AgentInfo>
  list(query?: AgentListQuery, options?: RequestOptions): Promise<AgentPage>
  start(
    name: string,
    request: AgentStartRequest,
    options?: RequestOptions,
  ): Promise<AgentStartReceipt>
}
interface AgentTransport {
  request(
    method: "GET" | "POST",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientAgents(transport: AgentTransport): ClientAgentsApi {
  return Object.freeze({
    async retrieve(id, query = {}, options = {}) {
      validateDefinitionId(id)
      const response = objectValue(await transport.request(
        "GET", `/v1/agents/${encodeURIComponent(id)}${definitionItemQuery(query, "Agent")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Agent response")
      return Object.freeze({
        ...parseAgentListItem(response),
        deploymentId: resourceID(response["deployment_id"], "Agent response.deployment_id"),
      })
    },
    async list(query = {}, options = {}) {
      const response = objectValue(await transport.request(
        "GET", `/v1/agents${definitionListQuery(query, "Agent")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Agent list response")
      if (!Array.isArray(response["agents"])) throw new Error("Agent list response.agents must be an array")
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") throw new Error("Agent list response.next_cursor must be a string")
      return Object.freeze({
        deploymentId: resourceID(response["deployment_id"], "Agent list response.deployment_id"),
        items: Object.freeze(response["agents"].map(parseAgentListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    async start(name, request, options) {
      if (!name.trim()) throw new Error("Agent name is required")
      normalizeInput(request.input)
      const value = await transport.request(
        "POST",
        `/v1/agents/${encodeURIComponent(name)}/start`,
        {
          body: {
            input: request.input,
            ...(request.slack === undefined ? {} : { slack: slackStartOption(request.slack) }),
            idempotency_key: sessionOperationOptions(request).idempotencyKey,
            ...(request.sessionKey === undefined
              ? {}
              : { session_key: request.sessionKey }),
            ...(request.computer === undefined
              ? {}
              : {
                  computer_id: resourceID(request.computer.id, "Computer ID"),
                }),
          },
          ...(options?.signal === undefined ? {} : { signal: options.signal }),
        },
      )
      const admission = parseTurnAdmission(value)
      if (
        value === null ||
        typeof value !== "object" ||
        !("created" in value) ||
        typeof value.created !== "boolean"
      )
        throw new Error("Agent start response.created must be a boolean")
      const session = createSessionRef(admission.sessionId, transport)
      return Object.freeze({
        session,
        turn: session.turn(admission.turnId),
        created: value.created,
      })
    },
  } satisfies ClientAgentsApi)
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`)
  return value as Record<string, unknown>
}
function parseAgentListItem(value: unknown): AgentListItem {
  const input = objectValue(value, "Agent list item")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Agent list item.id must be a string")
  validateDefinitionId(id)
  return Object.freeze({ id })
}
