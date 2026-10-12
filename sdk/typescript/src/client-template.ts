import { type ComputerTransport } from "./client-computer"
import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateDefinitionId } from "./schema/definition"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export interface TemplateGetQuery {
  readonly deploymentId?: string
}

export interface TemplateListQuery extends TemplateGetQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface TemplateListItem {
  readonly template: string
  readonly resources: TemplateResources
}

export interface TemplateInfo extends TemplateListItem {
  readonly deploymentId: string
}

export interface TemplatePage extends CursorPage<TemplateListItem> {
  readonly deploymentId: string
}

export interface ClientTemplatesApi {
  get(id: string, query?: TemplateGetQuery, options?: RequestOptions): Promise<TemplateInfo>
  list(query?: TemplateListQuery, options?: RequestOptions): Promise<TemplatePage>
}

export function createClientTemplates(transport: ComputerTransport): ClientTemplatesApi {
  return Object.freeze({
    async get(
      id: string,
      query: TemplateGetQuery = {},
      options: RequestOptions = {},
    ): Promise<TemplateInfo> {
      validateDefinitionId(id)
      return parseTemplateInfo(await transport.request(
        "GET",
        `/v1/templates/${encodeURIComponent(id)}${definitionItemQuery(query, "Template")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: TemplateListQuery = {},
      options: RequestOptions = {},
    ): Promise<TemplatePage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/templates${definitionListQuery(queryInput, "Template")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Template list response")
      if (!Array.isArray(response["templates"])) {
        throw new Error("Template list response.templates must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Template list response.next_cursor must be a string")
      }
      return Object.freeze({
        deploymentId: resourceID(response["deployment_id"], "Template list response.deployment_id"),
        items: Object.freeze(response["templates"].map(parseTemplateListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
  })
}

function parseTemplateInfo(value: unknown): TemplateInfo {
  const input = objectValue(value, "Template response")
  const id = input["template"]
  if (typeof id !== "string") throw new Error("Template response.template must be a string")
  validateDefinitionId(id)
  return Object.freeze({
    template: id,
    resources: parseResources(input["resources"]),
    deploymentId: resourceID(input["deployment_id"], "Template response.deployment_id"),
  })
}

function parseTemplateListItem(value: unknown): TemplateListItem {
  const input = objectValue(value, "Template list item")
  const id = input["template"]
  if (typeof id !== "string") throw new Error("Template list item.template must be a string")
  validateDefinitionId(id)
  return Object.freeze({ template: id, resources: parseResources(input["resources"]) })
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

export interface TemplateResources {
  readonly milliCpu: number
  readonly memoryMiB: number
  readonly diskMiB?: number
}

function parseResources(value: unknown): TemplateResources {
  const input = objectValue(value, "Template resources")
  for (const name of ["milliCpu", "memoryMiB", "diskMiB"]) {
    if (name === "diskMiB" && input[name] === undefined) continue
    if (!Number.isSafeInteger(input[name]) || (input[name] as number) <= 0) {
      throw new Error(`Template resources.${name} must be a positive integer`)
    }
  }
  return Object.freeze({
    milliCpu: input["milliCpu"] as number,
    memoryMiB: input["memoryMiB"] as number,
    ...(input["diskMiB"] === undefined ? {} : { diskMiB: input["diskMiB"] as number }),
  })
}
