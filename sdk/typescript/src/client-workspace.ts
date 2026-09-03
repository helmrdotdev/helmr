import type { CursorPage } from "./contract"
import { abortableDelay } from "./internal/abort"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"
import { validateTaskId } from "./schema/task"
import type { RequestOptions } from "./request"
import {
  parseWorkspaceDeleteReceipt,
  parseWorkspaceExecResult,
  parseWorkspace,
  brandWorkspaceAddress,
  type WorkspaceDeleteRequest,
  type WorkspaceDeleteReceipt,
  type WorkspaceExecRequest,
  type WorkspaceExecResult,
  type WorkspaceRef,
  type Workspace,
  type WorkspaceStatus,
} from "./workspace"

export interface WorkspaceListItem {
  readonly id: string
  readonly key?: string
  readonly sandboxId: string
  readonly deploymentId: string
  readonly status: WorkspaceStatus
  readonly lastActivityAt: string
  readonly createdAt: string
  readonly updatedAt: string
}

export type WorkspaceListQuery =
  | Readonly<{ key?: never; cursor?: string; limit?: number }>
  | Readonly<{ key: string; cursor?: never; limit?: never }>

export interface ClientWorkspacesApi {
  retrieve(id: string, options?: RequestOptions): Promise<Workspace>
  list(
    query?: WorkspaceListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<WorkspaceListItem>>
  ref(id: string): WorkspaceRef
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
    async retrieve(id: string, options: RequestOptions = {}): Promise<Workspace> {
      const workspaceID = resourceID(id, "Workspace ID")
      return parseWorkspace(await transport.request(
        "GET",
        `/v1/workspaces/${encodeURIComponent(workspaceID)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: WorkspaceListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<WorkspaceListItem>> {
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
        items: Object.freeze(response["workspaces"].map(parseWorkspaceListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    ref(id: string): WorkspaceRef {
      return createClientWorkspaceRef(resourceID(id, "Workspace ID"), transport)
    },
  })
}

function parseWorkspaceListItem(value: unknown): WorkspaceListItem {
  const input = objectValue(value, "Workspace list item")
  const key = input["key"]
  if (key !== undefined && typeof key !== "string") {
    throw new Error("Workspace list item.key must be a string")
  }
  const sandboxId = input["sandbox_id"]
  if (typeof sandboxId !== "string") {
    throw new Error("Workspace list item.sandbox_id must be a string")
  }
  validateTaskId(sandboxId)
  const status = input["status"]
  if (status !== "available" && status !== "recovery_required" && status !== "deleting") {
    throw new Error("Workspace list item.status is invalid")
  }
  return Object.freeze({
    id: resourceID(input["id"], "Workspace list item.id"),
    ...(key === undefined ? {} : { key }),
    sandboxId,
    deploymentId: resourceID(input["deployment_id"], "Workspace list item.deployment_id"),
    status,
    lastActivityAt: workspaceTimestamp(input["last_activity_at"], "last_activity_at"),
    createdAt: workspaceTimestamp(input["created_at"], "created_at"),
    updatedAt: workspaceTimestamp(input["updated_at"], "updated_at"),
  })
}

function workspaceTimestamp(value: unknown, field: string): string {
  return timestampString(value, `Workspace list item.${field}`)
}

export function createClientWorkspaceRef(
  id: string,
  transport: WorkspaceTransport,
): WorkspaceRef {
  const workspaceID = resourceID(id, "Workspace ID")
  const path = `/v1/workspaces/${encodeURIComponent(workspaceID)}`
  const ref = {
    id: workspaceID,
    async retrieve(options: RequestOptions = {}): Promise<Workspace> {
      return parseWorkspace(await transport.request(
        "GET",
        path,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async exec(
      request: WorkspaceExecRequest,
      options: RequestOptions = {},
    ): Promise<WorkspaceExecResult> {
      let process = parseWorkspaceExecProcess(await transport.request("POST", `${path}/exec`, {
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
      const admittedProcessId = process.processId
      while (process.status === "pending" || process.status === "running") {
        await abortableDelay(1_000, options.signal)
        process = parseWorkspaceExecProcess(await transport.request(
          "GET",
          `${path}/exec/${encodeURIComponent(admittedProcessId)}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ))
        if (process.processId !== admittedProcessId) {
          throw new Error("Workspace exec poll response changed process ID")
        }
      }
      if (process.status === "failed") throw workspaceExecFailure(process.terminalReasonCode)
      if (process.status !== "exited") {
        throw new Error("Workspace exec process response did not reach a terminal status")
      }
      return process.result
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
  return brandWorkspaceAddress(ref)
}

type WorkspaceExecProcess =
  | Readonly<{ processId: string; status: "pending" | "running" }>
  | Readonly<{ processId: string; status: "exited"; result: WorkspaceExecResult }>
  | Readonly<{ processId: string; status: "failed"; terminalReasonCode: string }>

function parseWorkspaceExecProcess(value: unknown): WorkspaceExecProcess {
  const response = objectValue(value, "Workspace exec process response")
  const processId = resourceID(
    response["process_id"],
    "Workspace exec process response.process_id",
  )
  switch (response["status"]) {
    case "pending":
    case "running":
      return Object.freeze({ processId, status: response["status"] })
    case "exited":
      return Object.freeze({
        processId,
        status: "exited",
        result: parseWorkspaceExecResult(response),
      })
    case "failed": {
      const error = objectValue(response["error"], "Workspace exec process response.error")
      const terminalReasonCode = error["terminal_reason_code"]
      if (typeof terminalReasonCode !== "string" || ![
        "workspace_exec_timed_out",
        "workspace_exec_output_limit_exceeded",
        "workspace_exec_placement_timed_out",
        "workspace_exec_failed",
      ].includes(terminalReasonCode)) {
        throw new Error("Workspace exec process response.error.terminal_reason_code is invalid")
      }
      return Object.freeze({ processId, status: "failed", terminalReasonCode })
    }
    default:
      throw new Error("Workspace exec process response.status is invalid")
  }
}

function workspaceExecFailure(code: string): Error {
  const message = code === "workspace_exec_timed_out"
    ? "workspace exec timed out"
    : code === "workspace_exec_output_limit_exceeded"
      ? "workspace exec output limit was exceeded"
      : code === "workspace_exec_placement_timed_out"
        ? "workspace exec placement timed out"
        : "workspace exec failed"
  const error = new Error(message) as Error & { code: string }
  error.name = "HelmrError"
  error.code = code
  return error
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
