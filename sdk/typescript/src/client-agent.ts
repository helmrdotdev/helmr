import { slackStartOption } from "./client-slack"
import type { TurnRef } from "./contract"
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
export interface ClientAgentsApi {
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
        turn: session.turns.ref(admission.turnId),
        created: value.created,
      })
    },
  } satisfies ClientAgentsApi)
}
