import { createClientComputerRef, type ClientComputerRef, type ComputerTransport } from "./client-computer"
import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateDefinitionId } from "./schema/definition"
import {
  encodeComputerSecrets,
  parseComputer,
  type ComputerCreateRequest,
} from "./computer"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export interface ComputerDefinitionRetrieveQuery {
  readonly deploymentId?: string
}

export interface ComputerDefinitionListQuery extends ComputerDefinitionRetrieveQuery {
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
  retrieve(id: string, query?: ComputerDefinitionRetrieveQuery, options?: RequestOptions): Promise<ComputerDefinitionInfo>
  list(query?: ComputerDefinitionListQuery, options?: RequestOptions): Promise<ComputerDefinitionPage>
  createComputer(
    id: string,
    request?: ComputerCreateRequest,
    options?: RequestOptions,
  ): Promise<ClientComputerRef>
}

export function createClientComputerDefinitions(transport: ComputerTransport): ClientComputerDefinitionsApi {
  return Object.freeze({
    async retrieve(
      id: string,
      query: ComputerDefinitionRetrieveQuery = {},
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
    async createComputer(
      id: string,
      request: ComputerCreateRequest = {},
      options: RequestOptions = {},
    ): Promise<ClientComputerRef> {
      validateDefinitionId(id)
      const secrets = encodeComputerSecrets(request.secrets)
      const computer = parseComputer(await transport.request(
        "POST",
        `/v1/computer-definitions/${encodeURIComponent(id)}/computers`,
        {
          body: {
            ...(request.key === undefined ? {} : { key: request.key }),
            ...(request.secrets === undefined ? {} : { secrets }),
            ...(request.idempotencyKey === undefined ? {} : { idempotency_key: request.idempotencyKey }),
          },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ))
      return createClientComputerRef(computer.id, transport)
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
