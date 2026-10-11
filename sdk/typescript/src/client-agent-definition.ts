import { type ComputerTransport } from "./client-computer"
import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateDefinitionId } from "./schema/definition"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export interface AgentDefinitionGetQuery {
  readonly deploymentId?: string
}

export interface AgentDefinitionListQuery extends AgentDefinitionGetQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface AgentDefinitionListItem {
  readonly id: string
  readonly agentId: string
}

export interface AgentDefinitionInfo extends AgentDefinitionListItem {
  readonly deploymentId: string
}

export interface AgentDefinitionPage extends CursorPage<AgentDefinitionListItem> {
  readonly deploymentId: string
}

export interface ClientAgentDefinitionsApi {
  get(id: string, query?: AgentDefinitionGetQuery, options?: RequestOptions): Promise<AgentDefinitionInfo>
  list(query?: AgentDefinitionListQuery, options?: RequestOptions): Promise<AgentDefinitionPage>
}

export function createClientAgentDefinitions(transport: ComputerTransport): ClientAgentDefinitionsApi {
  return Object.freeze({
    async get(
      id: string,
      query: AgentDefinitionGetQuery = {},
      options: RequestOptions = {},
    ): Promise<AgentDefinitionInfo> {
      validateDefinitionId(id)
      return parseAgentDefinitionInfo(await transport.request(
        "GET",
        `/v1/agent-definitions/${encodeURIComponent(id)}${definitionItemQuery(query, "Agent definition")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: AgentDefinitionListQuery = {},
      options: RequestOptions = {},
    ): Promise<AgentDefinitionPage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/agent-definitions${definitionListQuery(queryInput, "Agent definition")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Agent definition list response")
      if (!Array.isArray(response["agent_definitions"])) {
        throw new Error("Agent definition list response.agent_definitions must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Agent definition list response.next_cursor must be a string")
      }
      return Object.freeze({
        deploymentId: resourceID(response["deployment_id"], "Agent definition list response.deployment_id"),
        items: Object.freeze(response["agent_definitions"].map(parseAgentDefinitionListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
  })
}

function parseAgentDefinitionInfo(value: unknown): AgentDefinitionInfo {
  const input = objectValue(value, "Agent definition response")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Agent definition response.id must be a string")
  validateDefinitionId(id)
  return Object.freeze({
    id,
    agentId: resourceID(input["agent_id"], "Agent definition response.agent_id"),
    deploymentId: resourceID(input["deployment_id"], "Agent definition response.deployment_id"),
  })
}

function parseAgentDefinitionListItem(value: unknown): AgentDefinitionListItem {
  const input = objectValue(value, "Agent definition list item")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Agent definition list item.id must be a string")
  validateDefinitionId(id)
  return Object.freeze({ id, agentId: resourceID(input["agent_id"], "Agent definition list item.agent_id") })
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}
