import { encodeCommandLogQuery, parseCommandLogPage, streamCommandLogs, type CommandLogQuery, type CommandLogStreamQuery } from "./command-logs"
import { parseCancelReceipt, parseCommandInfo, parseCommandReceipt, waitForCommand, type CommandRef, type CommandWaitOptions } from "./command"
import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import {
  parseComputerDeleteReceipt,
  parseComputer,
  parseComputerMembers,
  encodeComputerMembersQuery,
  type ComputerMembersQuery,
  type ComputerMember,
  type ComputerDeleteRequest,
  type ComputerDeleteReceipt,
  type ComputerCommandRequest,
  type ComputerRef,
  type Computer,
} from "./computer"

export type ComputerListItem = Omit<Computer, "secrets">

export type ComputerListQuery =
  | Readonly<{ key?: never; cursor?: string; limit?: number }>
  | Readonly<{ key: string; cursor?: never; limit?: never }>

export interface ClientComputersApi {
  retrieve(id: string, options?: RequestOptions): Promise<Computer>
  list(
    query?: ComputerListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<ComputerListItem>>
  ref(id: string): ClientComputerRef
}

export interface ComputerTransport {
  request(
    method: "GET" | "POST" | "DELETE",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientComputers(
  transport: ComputerTransport,
): ClientComputersApi {
  return Object.freeze({
    async retrieve(id: string, options: RequestOptions = {}): Promise<Computer> {
      const computerID = resourceID(id, "Computer ID")
      return parseComputer(await transport.request(
        "GET",
        `/v1/computers/${encodeURIComponent(computerID)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: ComputerListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<ComputerListItem>> {
      const query = computerListQuery(queryInput)
      const response = objectValue(await transport.request(
        "GET",
        `/v1/computers${query}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Computer list response")
      if (!Array.isArray(response["computers"])) {
        throw new Error("Computer list response.computers must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Computer list response.next_cursor must be a string")
      }
      return Object.freeze({
        items: Object.freeze(response["computers"].map(parseComputerListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    ref(id: string): ClientComputerRef {
      return createClientComputerRef(resourceID(id, "Computer ID"), transport)
    },
  })
}

function parseComputerListItem(value: unknown): ComputerListItem {
  const { secrets: _, ...item } = parseComputer({ ...objectValue(value, "Computer list item"), secrets: [] })
  return Object.freeze(item)
}

export interface ClientComputerRef extends ComputerRef {
  exec(request: ComputerCommandRequest, options?: RequestOptions): Promise<CommandRef>
}

export function createClientComputerRef(
  id: string,
  transport: ComputerTransport,
): ClientComputerRef {
  const computerID = resourceID(id, "Computer ID")
  const path = `/v1/computers/${encodeURIComponent(computerID)}`
  const ref = {
    id: computerID,
    async members(query: ComputerMembersQuery = {}, options: RequestOptions = {}): Promise<CursorPage<ComputerMember>> {
      const encoded = encodeComputerMembersQuery(query)
      const params = new URLSearchParams()
      if (encoded.cursor !== undefined) params.set("cursor", encoded.cursor)
      if (encoded.limit !== undefined) params.set("limit", String(encoded.limit))
      return parseComputerMembers(await transport.request("GET", `${path}/members${params.size ? `?${params}` : ""}`,
        options.signal === undefined ? {} : { signal: options.signal }))
    },
    async retrieve(options: RequestOptions = {}): Promise<Computer> {
      return parseComputer(await transport.request(
        "GET",
        path,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async exec(
      request: ComputerCommandRequest,
      options: RequestOptions = {},
    ): Promise<CommandRef> {
      const commandId = parseCommandReceipt(await transport.request("POST", `${path}/exec`, {
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
      return createClientCommandRef(commandId, transport)
    },
    async delete(
      request: ComputerDeleteRequest = {},
      options: RequestOptions = {},
    ): Promise<ComputerDeleteReceipt> {
      return parseComputerDeleteReceipt(await transport.request("DELETE", path, {
        body: request.idempotencyKey === undefined ? {} : { idempotency_key: request.idempotencyKey },
        ...(options.signal === undefined ? {} : { signal: options.signal }),
      }))
    },
  }
  return Object.freeze(ref)
}

export function createClientCommandRef(id: string, transport: ComputerTransport): CommandRef {
  const commandId = resourceID(id, "Command ID")
  const retrieve = async (signal?: AbortSignal) => {
    return parseCommandInfo(await transport.request("GET", `/v1/commands/${encodeURIComponent(commandId)}`, signal === undefined ? {} : { signal }), commandId)
  }
  const logs = async (query: CommandLogQuery = {}, signal?: AbortSignal) => {
    const value = encodeCommandLogQuery(query)
    const params = new URLSearchParams()
    if (value.cursor !== undefined) params.set("cursor", value.cursor)
    if (value.limit !== undefined) params.set("limit", String(value.limit))
    return parseCommandLogPage(await transport.request("GET", `/v1/commands/${encodeURIComponent(commandId)}/logs${params.size ? `?${params}` : ""}`, signal === undefined ? {} : { signal }))
  }
  return Object.freeze({ id: commandId,
    logs: async (query: CommandLogQuery = {}, options: RequestOptions = {}) => {
      const page = await logs(query, options.signal)
      return Object.freeze({ items: page.items, ...(page.nextCursor === undefined ? {} : { nextCursor: page.nextCursor }) })
    },
    streamLogs: (query: CommandLogStreamQuery = {}, options: RequestOptions = {}) => {
      return streamCommandLogs(logs, query, options)
    },
    cancel: async (options: RequestOptions = {}) => {
      return parseCancelReceipt(await transport.request("POST", `/v1/commands/${encodeURIComponent(commandId)}/cancel`, options.signal === undefined ? {} : { signal: options.signal }), commandId)
    },
    retrieve: (options: RequestOptions = {}) => retrieve(options.signal),
    wait: (options: CommandWaitOptions = {}) => {
      return waitForCommand(commandId, retrieve, options)
    },
  })
}

function computerListQuery(queryInput: ComputerListQuery): string {
  if (queryInput.key !== undefined && (queryInput.cursor !== undefined || queryInput.limit !== undefined)) {
    throw new Error("Computer exact key lookup does not accept cursor or limit")
  }
  const query = new URLSearchParams()
  if (queryInput.key !== undefined) {
    if (queryInput.key.length === 0) throw new Error("Computer key is required")
    query.set("key", queryInput.key)
  }
  if (queryInput.cursor !== undefined) {
    if (queryInput.cursor.length === 0) throw new Error("Computer cursor is required")
    query.set("cursor", queryInput.cursor)
  }
  if (queryInput.limit !== undefined) {
    if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
      throw new Error("Computer limit must be an integer in [1,100]")
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
