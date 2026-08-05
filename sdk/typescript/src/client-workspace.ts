import type { CursorPage, WorkspaceIdAddress } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import {
  parseWorkspaceDeleteReceipt,
  parseWorkspaceExecResult,
  parseWorkspaceFileContent,
  parseWorkspaceFileEntry,
  parseWorkspaceFilePage,
  parseWorkspaceSnapshot,
  brandWorkspaceIdAddress,
  type WorkspaceDeleteRequest,
  type WorkspaceDeleteReceipt,
  type WorkspaceExecRequest,
  type WorkspaceExecResult,
  type WorkspaceFileEntry,
  type WorkspaceFileListQuery,
  type WorkspaceSnapshot,
} from "./workspace"

export interface ClientWorkspaceRef extends WorkspaceIdAddress {
  readonly id: string
  readonly files: Readonly<{
    read(path: string, options?: RequestOptions): Promise<Uint8Array>
    stat(path: string, options?: RequestOptions): Promise<WorkspaceFileEntry>
    list(
      path: string,
      query?: WorkspaceFileListQuery,
      options?: RequestOptions,
    ): Promise<CursorPage<WorkspaceFileEntry>>
  }>
  exec(
    request: WorkspaceExecRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceExecResult>
  delete(
    request?: WorkspaceDeleteRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceDeleteReceipt>
}

export interface WorkspaceListQuery {
  readonly cursor?: string
  readonly limit?: number
  readonly key?: string
}

export interface ClientWorkspacesApi {
  retrieve(id: string, options?: RequestOptions): Promise<WorkspaceSnapshot>
  list(
    query?: WorkspaceListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<WorkspaceSnapshot>>
  ref(id: string): ClientWorkspaceRef
}

export interface WorkspaceTransport {
  request(
    method: "GET" | "POST" | "DELETE",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientWorkspaces(
  transport: WorkspaceTransport,
): ClientWorkspacesApi {
  return Object.freeze({
    async retrieve(id: string, options: RequestOptions = {}): Promise<WorkspaceSnapshot> {
      const workspaceID = resourceID(id, "Workspace ID")
      return parseWorkspaceSnapshot(await transport.request(
        "GET",
        `/v1/workspaces/${encodeURIComponent(workspaceID)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: WorkspaceListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<WorkspaceSnapshot>> {
      const query = workspaceListQuery(queryInput)
      const response = objectValue(await transport.request(
        "GET",
        `/v1/workspaces${query}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Workspace list response")
      if (!Array.isArray(response["workspaces"])) {
        throw new Error("Workspace list response.workspaces must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Workspace list response.next_cursor must be a string")
      }
      return Object.freeze({
        items: Object.freeze(response["workspaces"].map(parseWorkspaceSnapshot)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    ref(id: string): ClientWorkspaceRef {
      return createClientWorkspaceRef(resourceID(id, "Workspace ID"), transport)
    },
  })
}

export function createClientWorkspaceRef(
  id: string,
  transport: WorkspaceTransport,
): ClientWorkspaceRef {
  const workspaceID = resourceID(id, "Workspace ID")
  const path = `/v1/workspaces/${encodeURIComponent(workspaceID)}`
  const files: ClientWorkspaceRef["files"] = Object.freeze({
    async read(filePath: string, options: RequestOptions = {}): Promise<Uint8Array> {
      return parseWorkspaceFileContent(await transport.request(
        "GET",
        `${path}/files/content?${new URLSearchParams({ path: filePath }).toString()}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async stat(filePath: string, options: RequestOptions = {}): Promise<WorkspaceFileEntry> {
      return parseWorkspaceFileEntry(await transport.request(
        "GET",
        `${path}/files/stat?${new URLSearchParams({ path: filePath }).toString()}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      filePath: string,
      queryInput: WorkspaceFileListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<WorkspaceFileEntry>> {
      const query = new URLSearchParams({ path: filePath })
      if (queryInput.cursor !== undefined) query.set("cursor", queryInput.cursor)
      if (queryInput.limit !== undefined) query.set("limit", queryInput.limit.toString())
      return parseWorkspaceFilePage(await transport.request(
        "GET",
        `${path}/files?${query.toString()}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
  })
  const ref = {
    id: workspaceID,
    files,
    async exec(
      request: WorkspaceExecRequest,
      options: RequestOptions = {},
    ): Promise<WorkspaceExecResult> {
      return parseWorkspaceExecResult(await transport.request("POST", `${path}/exec`, {
        body: {
          command: [...request.command],
          ...(request.cwd === undefined ? {} : { cwd: request.cwd }),
          ...(request.env === undefined ? {} : { env: request.env }),
          ...(request.stdin === undefined ? {} : { stdin_base64: encodeBase64(request.stdin) }),
          ...(request.timeout === undefined ? {} : { timeout: request.timeout }),
          idempotency_key: request.idempotencyKey,
        },
        ...(options.signal === undefined ? {} : { signal: options.signal }),
      }))
    },
    async delete(
      request: WorkspaceDeleteRequest = {},
      options: RequestOptions = {},
    ): Promise<WorkspaceDeleteReceipt> {
      return parseWorkspaceDeleteReceipt(await transport.request("DELETE", path, {
        body: request.idempotencyKey === undefined ? {} : { idempotency_key: request.idempotencyKey },
        ...(options.signal === undefined ? {} : { signal: options.signal }),
      }))
    },
  }
  return brandWorkspaceIdAddress(ref)
}

function workspaceListQuery(queryInput: WorkspaceListQuery): string {
  if (queryInput.key !== undefined && (queryInput.cursor !== undefined || queryInput.limit !== undefined)) {
    throw new Error("Workspace exact key lookup does not accept cursor or limit")
  }
  const query = new URLSearchParams()
  if (queryInput.key !== undefined) {
    if (queryInput.key.length === 0) throw new Error("Workspace key is required")
    query.set("key", queryInput.key)
  }
  if (queryInput.cursor !== undefined) {
    if (queryInput.cursor.length === 0) throw new Error("Workspace cursor is required")
    query.set("cursor", queryInput.cursor)
  }
  if (queryInput.limit !== undefined) {
    if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
      throw new Error("Workspace limit must be an integer in [1,100]")
    }
    query.set("limit", queryInput.limit.toString())
  }
  return query.size === 0 ? "" : `?${query.toString()}`
}

function encodeBase64(value: Uint8Array): string {
  let binary = ""
  for (const byte of value) binary += String.fromCharCode(byte)
  return btoa(binary)
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}
