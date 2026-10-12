import { type ComputerTransport } from "./client-computer"
import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateDefinitionId } from "./schema/definition"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export interface TemplateDefinitionGetQuery {
  readonly deploymentId?: string
}

export interface TemplateDefinitionListQuery extends TemplateDefinitionGetQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface TemplateDefinitionListItem {
  readonly id: string
}

export interface TemplateDefinitionInfo extends TemplateDefinitionListItem {
  readonly deploymentId: string
}

export interface TemplateDefinitionPage extends CursorPage<TemplateDefinitionListItem> {
  readonly deploymentId: string
}

export interface ClientTemplateDefinitionsApi {
  get(id: string, query?: TemplateDefinitionGetQuery, options?: RequestOptions): Promise<TemplateDefinitionInfo>
  list(query?: TemplateDefinitionListQuery, options?: RequestOptions): Promise<TemplateDefinitionPage>
}

export function createClientTemplateDefinitions(transport: ComputerTransport): ClientTemplateDefinitionsApi {
  return Object.freeze({
    async get(
      id: string,
      query: TemplateDefinitionGetQuery = {},
      options: RequestOptions = {},
    ): Promise<TemplateDefinitionInfo> {
      validateDefinitionId(id)
      return parseTemplateDefinitionInfo(await transport.request(
        "GET",
        `/v1/template-definitions/${encodeURIComponent(id)}${definitionItemQuery(query, "Template definition")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: TemplateDefinitionListQuery = {},
      options: RequestOptions = {},
    ): Promise<TemplateDefinitionPage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/template-definitions${definitionListQuery(queryInput, "Template definition")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Template definition list response")
      if (!Array.isArray(response["template_definitions"])) {
        throw new Error("Template definition list response.template_definitions must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Template definition list response.next_cursor must be a string")
      }
      return Object.freeze({
        deploymentId: resourceID(response["deployment_id"], "Template definition list response.deployment_id"),
        items: Object.freeze(response["template_definitions"].map(parseTemplateDefinitionListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
  })
}

function parseTemplateDefinitionInfo(value: unknown): TemplateDefinitionInfo {
  const input = objectValue(value, "Template definition response")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Template definition response.id must be a string")
  validateDefinitionId(id)
  return Object.freeze({
    id,
    deploymentId: resourceID(input["deployment_id"], "Template definition response.deployment_id"),
  })
}

function parseTemplateDefinitionListItem(value: unknown): TemplateDefinitionListItem {
  const input = objectValue(value, "Template definition list item")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Template definition list item.id must be a string")
  validateDefinitionId(id)
  return Object.freeze({ id })
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}
