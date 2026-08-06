import { createClientWorkspaceRef, type WorkspaceTransport } from "./client-workspace"
import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"
import {
  encodeWorkspaceSecrets,
  parseWorkspace,
  type WorkspaceCreateRequest,
  type WorkspaceRef,
} from "./workspace"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export interface SandboxRetrieveQuery {
  readonly deploymentId?: string
}

export interface SandboxListQuery extends SandboxRetrieveQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface SandboxListItem {
  readonly id: string
}

export interface SandboxInfo extends SandboxListItem {
  readonly deploymentId: string
}

export interface SandboxPage extends CursorPage<SandboxListItem> {
  readonly deploymentId: string
}

export interface ClientSandboxesApi {
  retrieve(id: string, query?: SandboxRetrieveQuery, options?: RequestOptions): Promise<SandboxInfo>
  list(query?: SandboxListQuery, options?: RequestOptions): Promise<SandboxPage>
  createWorkspace(
    id: string,
    request?: WorkspaceCreateRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceRef>
}

export function createClientSandboxes(transport: WorkspaceTransport): ClientSandboxesApi {
  return Object.freeze({
    async retrieve(
      id: string,
      query: SandboxRetrieveQuery = {},
      options: RequestOptions = {},
    ): Promise<SandboxInfo> {
      validateTaskId(id)
      return parseSandboxInfo(await transport.request(
        "GET",
        `/v1/sandboxes/${encodeURIComponent(id)}${definitionItemQuery(query, "Sandbox")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: SandboxListQuery = {},
      options: RequestOptions = {},
    ): Promise<SandboxPage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/sandboxes${definitionListQuery(queryInput, "Sandbox")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Sandbox list response")
      if (!Array.isArray(response["sandboxes"])) {
        throw new Error("Sandbox list response.sandboxes must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Sandbox list response.next_cursor must be a string")
      }
      return Object.freeze({
        deploymentId: resourceID(response["deployment_id"], "Sandbox list response.deployment_id"),
        items: Object.freeze(response["sandboxes"].map(parseSandboxListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    async createWorkspace(
      id: string,
      request: WorkspaceCreateRequest = {},
      options: RequestOptions = {},
    ): Promise<WorkspaceRef> {
      validateTaskId(id)
      const secrets = encodeWorkspaceSecrets(request.secrets)
      const workspace = parseWorkspace(await transport.request(
        "POST",
        `/v1/sandboxes/${encodeURIComponent(id)}/workspaces`,
        {
          body: {
            ...(request.key === undefined ? {} : { key: request.key }),
            ...(request.secrets === undefined ? {} : { secrets }),
            ...(request.idempotencyKey === undefined ? {} : { idempotency_key: request.idempotencyKey }),
          },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ))
      return createClientWorkspaceRef(workspace.id, transport)
    },
  })
}

function parseSandboxInfo(value: unknown): SandboxInfo {
  const input = objectValue(value, "Sandbox response")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Sandbox response.id must be a string")
  validateTaskId(id)
  return Object.freeze({
    id,
    deploymentId: resourceID(input["deployment_id"], "Sandbox response.deployment_id"),
  })
}

function parseSandboxListItem(value: unknown): SandboxListItem {
  const input = objectValue(value, "Sandbox list item")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Sandbox list item.id must be a string")
  validateTaskId(id)
  return Object.freeze({ id })
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}
