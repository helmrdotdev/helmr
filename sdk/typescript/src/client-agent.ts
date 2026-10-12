import { type ComputerTransport } from "./client-computer"
import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateDefinitionId } from "./schema/definition"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export interface AgentGetQuery {
  readonly deploymentId?: string
}

export interface AgentListQuery extends AgentGetQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface AgentListItem {
  readonly agent: string
  readonly template: string
}

export interface AgentInfo extends AgentListItem {
  readonly deploymentId: string
}

export interface AgentPage extends CursorPage<AgentListItem> {
  readonly deploymentId: string
}

export interface ClientAgentsApi {
  get(id: string, query?: AgentGetQuery, options?: RequestOptions): Promise<AgentInfo>
  list(query?: AgentListQuery, options?: RequestOptions): Promise<AgentPage>
}

export function createClientAgents(transport: ComputerTransport): ClientAgentsApi {
  return Object.freeze({
    async get(
      id: string,
      query: AgentGetQuery = {},
      options: RequestOptions = {},
    ): Promise<AgentInfo> {
      validateDefinitionId(id)
      return parseAgentInfo(await transport.request(
        "GET",
        `/v1/agents/${encodeURIComponent(id)}${definitionItemQuery(query, "Agent")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: AgentListQuery = {},
      options: RequestOptions = {},
    ): Promise<AgentPage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/agents${definitionListQuery(queryInput, "Agent")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Agent list response")
      if (!Array.isArray(response["agents"])) {
        throw new Error("Agent list response.agents must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Agent list response.next_cursor must be a string")
      }
      return Object.freeze({
        deploymentId: resourceID(response["deployment_id"], "Agent list response.deployment_id"),
        items: Object.freeze(response["agents"].map(parseAgentListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
  })
}

function parseAgentInfo(value: unknown): AgentInfo {
  const input = objectValue(value, "Agent response")
  const id = input["agent"]
  if (typeof id !== "string") throw new Error("Agent response.agent must be a string")
  validateDefinitionId(id)
  return Object.freeze({
    agent: id,
    template: parseTemplate(input["template"]),
    deploymentId: resourceID(input["deployment_id"], "Agent response.deployment_id"),
  })
}

function parseAgentListItem(value: unknown): AgentListItem {
  const input = objectValue(value, "Agent list item")
  const id = input["agent"]
  if (typeof id !== "string") throw new Error("Agent list item.agent must be a string")
  validateDefinitionId(id)
  return Object.freeze({ agent: id, template: parseTemplate(input["template"]) })
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function parseTemplate(value: unknown): string {
  if (typeof value !== "string") throw new Error("Agent.template must be a string")
  validateDefinitionId(value)
  return value
}
