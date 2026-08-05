import type {
  CursorPage,
  JsonValue,
  WorkspaceAddress,
} from "./contract"
import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"
import { workspaces } from "./workspace"

export interface ScheduleFailure {
  readonly code:
    | "task_authority_invalid"
    | "workspace_unavailable"
    | "architecture_incompatible"
    | "generation_invalid"
    | "input_invalid"
  readonly message: string
  readonly details: Readonly<Record<string, JsonValue>>
}

export interface ScheduleSnapshot {
  readonly id: string
  readonly taskId: string
  readonly workspace: WorkspaceAddress
  readonly workspaceId?: string
  readonly cron: Readonly<{ pattern: string; timezone: string }>
  readonly status: "pending_workspace" | "active" | "errored" | "archived"
  readonly lastFailure?: ScheduleFailure
  readonly nextFireAt?: string
  readonly lastFireAt?: string
  readonly createdAt: string
  readonly updatedAt: string
}

export interface ScheduleListQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface ClientSchedulesApi {
  retrieve(
    scheduleId: string,
    options?: RequestOptions,
  ): Promise<ScheduleSnapshot>
  list(
    query?: ScheduleListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<ScheduleSnapshot>>
}

interface ScheduleTransport {
  request(
    method: "GET",
    path: string,
    options?: Readonly<{ signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientSchedules(
  transport: ScheduleTransport,
): ClientSchedulesApi {
  return Object.freeze({
    async retrieve(
      scheduleId: string,
      options: RequestOptions = {},
    ): Promise<ScheduleSnapshot> {
      return parseSchedule(
        await transport.request(
          "GET",
          `/v1/schedules/${encodeURIComponent(resourceID(scheduleId, "Schedule ID"))}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
      )
    },
    async list(
      query: ScheduleListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<ScheduleSnapshot>> {
      const values = new URLSearchParams()
      if (query.cursor !== undefined) values.set("cursor", query.cursor)
      if (query.limit !== undefined) values.set("limit", String(query.limit))
      const suffix = values.size === 0 ? "" : `?${values}`
      const response = scheduleObject(
        await transport.request(
          "GET",
          `/v1/schedules${suffix}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
        "Schedule list response",
      )
      if (!Array.isArray(response["schedules"])) {
        throw new Error("Schedule list response.schedules must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Schedule list response.next_cursor must be a string")
      }
      return Object.freeze({
        items: Object.freeze(response["schedules"].map(parseSchedule)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
  })
}

function parseSchedule(value: unknown): ScheduleSnapshot {
  const input = scheduleObject(value, "Schedule response")
  const workspace = scheduleObject(input["workspace"], "Schedule workspace")
  const hasID = typeof workspace["id"] === "string" && workspace["id"] !== ""
  const hasKey = typeof workspace["key"] === "string" && workspace["key"] !== ""
  if (hasID === hasKey) {
    throw new Error("Schedule workspace must contain exactly one address")
  }
  const workspaceAddress = hasID
    ? workspaces.fromId(resourceID(workspace["id"], "Schedule workspace.id"))
    : workspaces.fromKey(workspace["key"] as string)
  const cron = scheduleObject(input["cron"], "Schedule cron")
  const status = input["status"]
  if (
    status !== "pending_workspace" &&
    status !== "active" &&
    status !== "errored" &&
    status !== "archived"
  ) {
    throw new Error("Schedule response.status is invalid")
  }
  const lastFailure = input["last_failure"] === undefined
    ? undefined
    : parseScheduleFailure(input["last_failure"])
  if (status === "errored" && lastFailure === undefined) {
    throw new Error("Errored Schedule response must contain last_failure")
  }
  const workspaceId = input["workspace_id"] === undefined
    ? undefined
    : resourceID(input["workspace_id"], "Schedule response.workspace_id")
  if (status === "pending_workspace" && workspaceId !== undefined) {
    throw new Error("pending Schedule response must not contain workspace_id")
  }
  if (
    (status === "active" || status === "errored") &&
    workspaceId === undefined
  ) {
    throw new Error(`Schedule response with status ${status} requires workspace_id`)
  }
  if (
    workspaceAddress.id !== undefined &&
    workspaceId !== undefined &&
    workspaceAddress.id !== workspaceId
  ) {
    throw new Error("Schedule response workspace_id must match its ID address")
  }
  return Object.freeze({
    id: resourceID(input["id"], "Schedule response.id"),
    taskId: requiredString(input, "task_id", "Schedule response"),
    workspace: workspaceAddress,
    ...(workspaceId === undefined ? {} : { workspaceId }),
    cron: Object.freeze({
      pattern: requiredString(cron, "pattern", "Schedule cron"),
      timezone: requiredString(cron, "timezone", "Schedule cron"),
    }),
    status,
    ...(lastFailure === undefined ? {} : { lastFailure }),
    ...(input["next_fire_at"] === undefined
      ? {}
      : { nextFireAt: timestamp(input["next_fire_at"], "next_fire_at") }),
    ...(input["last_fire_at"] === undefined
      ? {}
      : { lastFireAt: timestamp(input["last_fire_at"], "last_fire_at") }),
    createdAt: timestamp(input["created_at"], "created_at"),
    updatedAt: timestamp(input["updated_at"], "updated_at"),
  })
}

function parseScheduleFailure(value: unknown): ScheduleFailure {
  const input = scheduleObject(value, "Schedule failure")
  const code = requiredString(input, "code", "Schedule failure")
  if (
    code !== "task_authority_invalid" &&
    code !== "workspace_unavailable" &&
    code !== "architecture_incompatible" &&
    code !== "generation_invalid" &&
    code !== "input_invalid"
  ) {
    throw new Error("Schedule failure.code is invalid")
  }
  return Object.freeze({
    code,
    message: requiredString(input, "message", "Schedule failure"),
    details: Object.freeze({ ...scheduleObject(input["details"], "Schedule failure.details") }) as Readonly<Record<string, JsonValue>>,
  })
}

function scheduleObject(
  value: unknown,
  label: string,
): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requiredString(
  value: Record<string, unknown>,
  field: string,
  label: string,
): string {
  const result = value[field]
  if (typeof result !== "string" || result === "") {
    throw new Error(`${label}.${field} must be a non-empty string`)
  }
  return result
}

function timestamp(value: unknown, field: string): string {
  if (typeof value !== "string" || Number.isNaN(Date.parse(value))) {
    throw new Error(`Schedule response.${field} must be a timestamp`)
  }
  return value
}
