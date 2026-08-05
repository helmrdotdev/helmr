import type { CursorPage } from "./contract"
import { createClientWorkspaceRef, type ClientWorkspaceRef, type WorkspaceTransport } from "./client-workspace"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"
import {
  encodeWorkspaceSecrets,
  parseWorkspaceSnapshot,
  type WorkspaceSecretInput,
} from "./workspace"

export interface SandboxSnapshot {
  readonly id: string
  readonly deploymentId: string
}

export interface SandboxReadQuery {
  readonly deploymentId?: string
}

export interface SandboxListQuery extends SandboxReadQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface SandboxPage extends CursorPage<SandboxSnapshot> {
  readonly deploymentId: string
}

export interface SandboxWorkspaceCreateRequest {
  readonly key?: string
  readonly secrets?: readonly WorkspaceSecretInput[]
  readonly idempotencyKey?: string
}

export interface ClientSandboxesApi {
  retrieve(id: string, query?: SandboxReadQuery, options?: RequestOptions): Promise<SandboxSnapshot>
  list(query?: SandboxListQuery, options?: RequestOptions): Promise<SandboxPage>
  createWorkspace(
    id: string,
    request?: SandboxWorkspaceCreateRequest,
    options?: RequestOptions,
  ): Promise<ClientWorkspaceRef>
}

export function createClientSandboxes(transport: WorkspaceTransport): ClientSandboxesApi {
  return Object.freeze({
    async retrieve(
      id: string,
      query: SandboxReadQuery = {},
      options: RequestOptions = {},
    ): Promise<SandboxSnapshot> {
      validateTaskId(id)
      return parseSandboxSnapshot(await transport.request(
        "GET",
        `/v1/sandboxes/${encodeURIComponent(id)}${sandboxQuery(query)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: SandboxListQuery = {},
      options: RequestOptions = {},
    ): Promise<SandboxPage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/sandboxes${sandboxQuery(queryInput)}`,
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
        items: Object.freeze(response["sandboxes"].map(parseSandboxSnapshot)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    async createWorkspace(
      id: string,
      request: SandboxWorkspaceCreateRequest = {},
      options: RequestOptions = {},
    ): Promise<ClientWorkspaceRef> {
      validateTaskId(id)
      const secrets = encodeWorkspaceSecrets(request.secrets)
      const snapshot = parseWorkspaceSnapshot(await transport.request(
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
      return createClientWorkspaceRef(snapshot.id, transport)
    },
  })
}

function sandboxQuery(queryInput: SandboxListQuery): string {
  const query = new URLSearchParams()
  if (queryInput.deploymentId !== undefined) {
    query.set("deployment_id", resourceID(queryInput.deploymentId, "Deployment ID"))
  }
  if (queryInput.cursor !== undefined) {
    if (queryInput.cursor.length === 0) throw new Error("Sandbox cursor is required")
    query.set("cursor", queryInput.cursor)
  }
  if (queryInput.limit !== undefined) {
    if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
      throw new Error("Sandbox limit must be an integer in [1,100]")
    }
    query.set("limit", queryInput.limit.toString())
  }
  return query.size === 0 ? "" : `?${query.toString()}`
}

function parseSandboxSnapshot(value: unknown): SandboxSnapshot {
  const input = objectValue(value, "Sandbox response")
  const id = input["id"]
  if (typeof id !== "string") throw new Error("Sandbox response.id must be a string")
  validateTaskId(id)
  return Object.freeze({
    id,
    deploymentId: resourceID(input["deployment_id"], "Sandbox response.deployment_id"),
  })
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}
