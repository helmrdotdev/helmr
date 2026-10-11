import type {
  CursorPage,
} from "./contract"
import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"
import { normalizeInput, type InputContent } from "./content"

export interface Schedule {
  readonly id: string
  readonly agentId: string
  readonly deploymentId: string
  readonly triggerKey: string
  readonly input: InputContent
  readonly activeFrom: string
  readonly activeUntil?: string
  readonly cron: Readonly<{ pattern: string; timezone: string }>
  readonly nextFireAt?: string
}

export type ScheduleListQuery = Readonly<{ agentId?: string; cursor?: string; limit?: number }>

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
      if (query.agentId !== undefined) {
        values.set("agent_id", resourceID(query.agentId, "Agent ID"))
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
  return Object.freeze({
    id: resourceID(input["id"], "Schedule response.id"),
    agentId: resourceID(input["agent_id"], "Agent ID"),
    deploymentId: resourceID(input["deployment_id"], "Deployment ID"),
    triggerKey: requiredString(input, "trigger_key", "Schedule response"),
    input: normalizeInput(input["input"]),
    activeFrom: timestamp(input["active_from"], "active_from"),
    ...(input["active_until"] === undefined ? {} : {activeUntil: timestamp(input["active_until"], "active_until")}),
    cron: Object.freeze({
      pattern: requiredString(cron, "pattern", "Schedule cron"),
      timezone: requiredString(cron, "timezone", "Schedule cron"),
    }),
    ...(input["next_fire_at"] === undefined
      ? {}
      : { nextFireAt: timestamp(input["next_fire_at"], "next_fire_at") }),

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
