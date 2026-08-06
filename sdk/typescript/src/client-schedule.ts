import type {
  CursorPage,
  JsonValue,
} from "./contract"
import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"
import { validateTaskId } from "./schema/task"

export interface ScheduleFailure {
  readonly code:
    | "task_authority_invalid"
    | "sandbox_authority_invalid"
    | "architecture_incompatible"
    | "generation_invalid"
    | "input_invalid"
  readonly message: string
  readonly details: Readonly<Record<string, JsonValue>>
}

export type ScheduleStatus = "active" | "errored" | "archived"

export interface Schedule {
  readonly id: string
  readonly taskId: string
  readonly generation: number
  readonly effectiveFrom: string
  readonly cron: Readonly<{ pattern: string; timezone: string }>
  readonly status: ScheduleStatus
  readonly lastFailure?: ScheduleFailure
  readonly nextFireAt?: string
  readonly lastFireAt?: string
  readonly createdAt: string
  readonly updatedAt: string
}

export type ScheduleListQuery =
  | Readonly<{ taskId?: never; cursor?: string; limit?: number }>
  | Readonly<{ taskId: string; cursor?: never; limit?: never }>

export interface ClientSchedulesApi {
  retrieve(
    scheduleId: string,
    options?: RequestOptions,
  ): Promise<Schedule>
  list(
    query?: ScheduleListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<Schedule>>
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
    ): Promise<Schedule> {
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
    ): Promise<CursorPage<Schedule>> {
      const values = new URLSearchParams()
      if (query.taskId !== undefined) {
        if (query.cursor !== undefined || query.limit !== undefined) {
          throw new Error("Schedule exact task lookup does not accept cursor or limit")
        }
        validateTaskId(query.taskId)
        values.set("task_id", query.taskId)
      }
      if (query.cursor !== undefined) {
        if (query.cursor.length === 0) throw new Error("Schedule cursor is required")
        values.set("cursor", query.cursor)
      }
      if (query.limit !== undefined) {
        if (!Number.isInteger(query.limit) || query.limit < 1 || query.limit > 100) {
          throw new Error("Schedule limit must be an integer in [1,100]")
        }
        values.set("limit", String(query.limit))
      }
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

function parseSchedule(value: unknown): Schedule {
  const input = scheduleObject(value, "Schedule response")
  const cron = scheduleObject(input["cron"], "Schedule cron")
  const status = input["status"]
  if (
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
  return Object.freeze({
    id: resourceID(input["id"], "Schedule response.id"),
    taskId: requiredString(input, "task_id", "Schedule response"),
    generation: positiveInteger(input["generation"], "generation"),
    effectiveFrom: timestamp(input["effective_from"], "effective_from"),
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
    code !== "sandbox_authority_invalid" &&
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
  return timestampString(value, `Schedule response.${field}`)
}

function positiveInteger(value: unknown, field: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 1) {
    throw new Error(`Schedule response.${field} must be a positive integer`)
  }
  return value as number
}
