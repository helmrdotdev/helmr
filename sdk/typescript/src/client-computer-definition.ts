import { type ComputerTransport } from "./client-computer"
import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateDefinitionId } from "./schema/definition"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export interface ComputerDefinitionGetQuery {
  readonly deploymentId?: string
}

export interface ComputerDefinitionListQuery extends ComputerDefinitionGetQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface ComputerDefinitionListItem {
  readonly id: string
}

export interface ComputerDefinitionInfo extends ComputerDefinitionListItem {
  readonly deploymentId: string
}

export interface ComputerDefinitionPage extends CursorPage<ComputerDefinitionListItem> {
  readonly deploymentId: string
}

export interface ClientComputerDefinitionsApi {
  get(id: string, query?: ComputerDefinitionGetQuery, options?: RequestOptions): Promise<ComputerDefinitionInfo>
  list(query?: ComputerDefinitionListQuery, options?: RequestOptions): Promise<ComputerDefinitionPage>
}

export function createClientComputerDefinitions(transport: ComputerTransport): ClientComputerDefinitionsApi {
  return Object.freeze({
    async get(
      id: string,
      query: ComputerDefinitionGetQuery = {},
      options: RequestOptions = {},
    ): Promise<ComputerDefinitionInfo> {
      validateDefinitionId(id)
      return parseComputerDefinitionInfo(await transport.request(
        "GET",
        `/v1/computer-definitions/${encodeURIComponent(id)}${definitionItemQuery(query, "Computer definition")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: ComputerDefinitionListQuery = {},
      options: RequestOptions = {},
    ): Promise<ComputerDefinitionPage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/computer-definitions${definitionListQuery(queryInput, "Computer definition")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Computer definition list response")
      if (!Array.isArray(response["computer_definitions"])) {
        throw new Error("Computer definition list response.computer_definitions must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Computer definition list response.next_cursor must be a string")
      }
      return Object.freeze({
        deploymentId: resourceID(response["deployment_id"], "Computer definition list response.deployment_id"),
        items: Object.freeze(response["computer_definitions"].map(parseComputerDefinitionListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
  })
}

function parseComputerDefinitionInfo(value: unknown): ComputerDefinitionInfo {
  const input = objectValue(value, "Computer definition response")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Computer definition response.id must be a string")
  validateDefinitionId(id)
  return Object.freeze({
    id,
    deploymentId: resourceID(input["deployment_id"], "Computer definition response.deployment_id"),
  })
}

function parseComputerDefinitionListItem(value: unknown): ComputerDefinitionListItem {
  const input = objectValue(value, "Computer definition list item")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Computer definition list item.id must be a string")
  validateDefinitionId(id)
  return Object.freeze({ id })
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}
