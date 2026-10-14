import { durationMilliseconds, retentionMilliseconds, type Retention } from "./duration"
import { encodeCommandLogQuery, parseCommandLogPage, streamCommandLogs, type CommandLogQuery, type CommandLogStreamQuery } from "./command-logs"
import { parseCancelReceipt, parseCommandInfo, parseCommandReceipt, waitForCommand, type CommandRef, type CommandWaitOptions, type CommandOutcome, type TimedCommandWaitResult } from "./command"
import type { CursorPage } from "./contract"
import { validateDefinitionId } from "./schema/definition"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import {
  encodeComputerSecrets,
  type ComputerCreateRequest,
  parseComputerDeleteReceipt,
  parseComputer,
  parseComputerUsage,
  encodeComputerUsageQuery,
  type ComputerUsageQuery,
  type ComputerUsage,
  type ComputerDeleteRequest,
  type ComputerDeleteReceipt,
  type ComputerCommandRequest,
  type ComputerRef,
  type Computer,
} from "./computer"

type WithoutSecrets<T> = T extends Computer ? Omit<T, "secrets"> : never
export type ComputerListItem = WithoutSecrets<Computer>

export type ComputerListQuery =
  | Readonly<{ key?: never; cursor?: string; limit?: number }>
  | Readonly<{ key: string; cursor?: never; limit?: never }>

export interface ClientComputersApi {
  create(request: ComputerCreateRequest, options?: RequestOptions): Promise<ComputerRef>
  get(id: string, options?: RequestOptions): Promise<Computer>
  list(
    query?: ComputerListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<ComputerListItem>>
  ref(id: string): ComputerRef
}

export interface ComputerTransport {
  request(
    method: "GET" | "POST" | "DELETE" | "PATCH",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientComputers(
  transport: ComputerTransport,
): ClientComputersApi {
  return Object.freeze({
    async create(
      request: ComputerCreateRequest,
      options: RequestOptions = {},
    ): Promise<ComputerRef> {
      validateDefinitionId(request.template)
      const secrets = encodeComputerSecrets(request.secrets)
      const computer = parseComputer(await transport.request(
        "POST",
        "/v1/computers",
        {
          body: {
            template: request.template,
            ...(request.key === undefined ? {} : { key: request.key }),
            ...(request.deleteAfterUnused === undefined ? {} : { delete_after_unused_ms: retentionMilliseconds(request.deleteAfterUnused, "deleteAfterUnused") }),
            ...(request.secrets === undefined ? {} : { secrets }),
            ...(request.idempotencyKey === undefined ? {} : { idempotency_key: request.idempotencyKey }),
          },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ))
      return createClientComputerRef(computer.id, transport)
    },
    async get(id: string, options: RequestOptions = {}): Promise<Computer> {
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
    ref(id: string): ComputerRef {
      return createClientComputerRef(resourceID(id, "Computer ID"), transport)
    },
  })
}

function parseComputerListItem(value: unknown): ComputerListItem {
  const { secrets: _, ...item } = parseComputer({ ...objectValue(value, "Computer list item"), secrets: [] })
  return Object.freeze(item)
}

export function createClientComputerRef(
  id: string,
  transport: ComputerTransport,
): ComputerRef {
  const computerID = resourceID(id, "Computer ID")
  const path = `/v1/computers/${encodeURIComponent(computerID)}`
  function usageCollection(collection: "sessions" | "commands") {
    return Object.freeze({ async list(query: ComputerUsageQuery = {}, options: RequestOptions = {}): Promise<CursorPage<ComputerUsage>> {
      const encoded = encodeComputerUsageQuery(query)
      const params = new URLSearchParams()
      if (encoded.cursor !== undefined) params.set("cursor", encoded.cursor)
      if (encoded.limit !== undefined) params.set("limit", String(encoded.limit))
      return parseComputerUsage(await transport.request("GET", `${path}/${collection}${params.size ? `?${params}` : ""}`,
        options.signal === undefined ? {} : { signal: options.signal }), collection)
    } })
  }
  const ref = {
    id: computerID,
    async update(request: Readonly<{ deleteAfterUnused: Retention }>, options: RequestOptions = {}): Promise<Computer> {
      return parseComputer(await transport.request("PATCH", path, { body: { delete_after_unused_ms: retentionMilliseconds(request.deleteAfterUnused, "deleteAfterUnused") }, ...(options.signal === undefined ? {} : { signal: options.signal }) }))
    },
    sessions: usageCollection("sessions"),
    commands: usageCollection("commands"),
    async get(options: RequestOptions = {}): Promise<Computer> {
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
          ...(request.timeout === undefined ? {} : { timeout: `${durationMilliseconds(request.timeout, "Command execution timeout")}ms` }),
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
  const get = async (signal?: AbortSignal) => {
    return parseCommandInfo(await transport.request("GET", `/v1/commands/${encodeURIComponent(commandId)}`, signal === undefined ? {} : { signal }), commandId)
  }
  const logs = async (query: CommandLogQuery = {}, signal?: AbortSignal) => {
    const value = encodeCommandLogQuery(query)
    const params = new URLSearchParams()
    if (value.cursor !== undefined) params.set("cursor", value.cursor)
    if (value.limit !== undefined) params.set("limit", String(value.limit))
    return parseCommandLogPage(await transport.request("GET", `/v1/commands/${encodeURIComponent(commandId)}/logs${params.size ? `?${params}` : ""}`, signal === undefined ? {} : { signal }))
  }
  function wait(options: CommandWaitOptions & { readonly timeout: import("./duration").Duration }): Promise<TimedCommandWaitResult>
  function wait(options?: CommandWaitOptions & { readonly timeout?: undefined }): Promise<CommandOutcome>
  function wait(options: CommandWaitOptions): Promise<CommandOutcome | TimedCommandWaitResult>
  function wait(options: CommandWaitOptions = {}): Promise<CommandOutcome | TimedCommandWaitResult> {
    return waitForCommand(commandId, get, options)
  }
  return Object.freeze({ id: commandId,
    logs: Object.freeze({
      list: (query: CommandLogQuery = {}, options: RequestOptions = {}) => logs(query, options.signal),
      stream: (query: CommandLogStreamQuery = {}, options: RequestOptions = {}) => streamCommandLogs(logs, query, options),
    }),
    cancel: async (options: RequestOptions = {}) => {
      return parseCancelReceipt(await transport.request("POST", `/v1/commands/${encodeURIComponent(commandId)}/cancel`, options.signal === undefined ? {} : { signal: options.signal }), commandId)
    },
    get: (options: RequestOptions = {}) => get(options.signal),
    wait,
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
