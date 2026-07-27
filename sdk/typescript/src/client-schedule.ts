import type {
  CursorPage,
  WorkspaceTarget,
} from "./contract"
import type { RequestOptions } from "./request"

export interface ScheduleError {
  readonly code:
    | "task_authority_invalid"
    | "workspace_unavailable"
    | "architecture_incompatible"
    | "generation_invalid"
    | "input_invalid"
  readonly message: string
}

export interface ScheduleSnapshot {
  readonly id: string
  readonly task: string
  readonly workspace: WorkspaceTarget
  readonly cron: Readonly<{ pattern: string; timezone: string }>
  readonly status: "pending-workspace" | "active" | "errored" | "archived"
  readonly lastError?: ScheduleError
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
          `/api/schedules/${encodeURIComponent(schedulePublicID(scheduleId))}`,
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
          `/api/schedules${suffix}`,
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
  const cron = scheduleObject(input["cron"], "Schedule cron")
  const status = input["status"]
  if (
    status !== "pending-workspace" &&
    status !== "active" &&
    status !== "errored" &&
    status !== "archived"
  ) {
    throw new Error("Schedule response.status is invalid")
  }
  const lastError = input["last_error"] === undefined
    ? undefined
    : parseScheduleError(input["last_error"])
  return Object.freeze({
    id: schedulePublicID(input["id"]),
    task: requiredString(input, "task", "Schedule response"),
    workspace: Object.freeze(
      hasID
        ? { id: workspace["id"] as string }
        : { key: workspace["key"] as string },
    ) as WorkspaceTarget,
    cron: Object.freeze({
      pattern: requiredString(cron, "pattern", "Schedule cron"),
      timezone: requiredString(cron, "timezone", "Schedule cron"),
    }),
    status,
    ...(lastError === undefined ? {} : { lastError }),
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

function parseScheduleError(value: unknown): ScheduleError {
  const input = scheduleObject(value, "Schedule error")
  const code = requiredString(input, "code", "Schedule error")
  if (
    code !== "task_authority_invalid" &&
    code !== "workspace_unavailable" &&
    code !== "architecture_incompatible" &&
    code !== "generation_invalid" &&
    code !== "input_invalid"
  ) {
    throw new Error("Schedule error.code is invalid")
  }
  return Object.freeze({
    code,
    message: requiredString(input, "message", "Schedule error"),
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

function schedulePublicID(value: unknown): string {
  if (typeof value !== "string" || !/^sch_[a-z2-7]{26}$/.test(value)) {
    throw new Error("Schedule ID must be a canonical sch_ public ID")
  }
  return value
}
