import type {
  CursorPage,
} from "./contract"
import type { RequestOptions } from "./request"
import { validateDefinitionId } from "./schema/definition"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"
import { normalizeInput, type InputContent } from "./content"

export interface Trigger {
  readonly id: string
  readonly agent: string
  readonly deploymentId: string
  readonly triggerKey: string
  readonly input: InputContent
  readonly activeFrom: string
  readonly activeUntil?: string
  readonly cron: Readonly<{ pattern: string; timezone: string }>
  readonly nextFireAt?: string
}

export type TriggerListQuery = Readonly<{ agent?: string; cursor?: string; limit?: number }>

export interface ClientTriggersApi {
  get(
    triggerId: string,
    options?: RequestOptions,
  ): Promise<Trigger>
  list(
    query?: TriggerListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<Trigger>>
}

interface TriggerTransport {
  request(
    method: "GET",
    path: string,
    options?: Readonly<{ signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientTriggers(
  transport: TriggerTransport,
): ClientTriggersApi {
  return Object.freeze({
    async get(
      triggerId: string,
      options: RequestOptions = {},
    ): Promise<Trigger> {
      return parseTrigger(
        await transport.request(
          "GET",
          `/v1/triggers/${encodeURIComponent(resourceID(triggerId, "Trigger ID"))}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
      )
    },
    async list(
      query: TriggerListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<Trigger>> {
      for (const key of Object.keys(query)) {
        if (!["agent", "cursor", "limit"].includes(key)) throw new Error(`Unsupported Trigger list query: ${key}`)
      }
      const values = new URLSearchParams()
      if (query.agent !== undefined) {
        validateDefinitionId(query.agent)
        values.set("agent", query.agent)
      }
      if (query.cursor !== undefined) {
        if (query.cursor.length === 0) throw new Error("Trigger cursor is required")
        values.set("cursor", query.cursor)
      }
      if (query.limit !== undefined) {
        if (!Number.isInteger(query.limit) || query.limit < 1 || query.limit > 100) {
          throw new Error("Trigger limit must be an integer in [1,100]")
        }
        values.set("limit", String(query.limit))
      }
      const suffix = values.size === 0 ? "" : `?${values}`
      const response = triggerObject(
        await transport.request(
          "GET",
          `/v1/triggers${suffix}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
        "Trigger list response",
      )
      if (!Array.isArray(response["triggers"])) {
        throw new Error("Trigger list response.triggers must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Trigger list response.next_cursor must be a string")
      }
      return Object.freeze({
        items: Object.freeze(response["triggers"].map(parseTrigger)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
  })
}

function parseTrigger(value: unknown): Trigger {
  const input = triggerObject(value, "Trigger response")
  const cron = triggerObject(input["cron"], "Trigger cron")
  const agent = requiredString(input, "agent", "Trigger response")
  validateDefinitionId(agent)
  return Object.freeze({
    id: resourceID(input["id"], "Trigger response.id"),
    agent,
    deploymentId: resourceID(input["deployment_id"], "Deployment ID"),
    triggerKey: requiredString(input, "trigger_key", "Trigger response"),
    input: normalizeInput(input["input"]),
    activeFrom: timestamp(input["active_from"], "active_from"),
    ...(input["active_until"] === undefined ? {} : {activeUntil: timestamp(input["active_until"], "active_until")}),
    cron: Object.freeze({
      pattern: requiredString(cron, "pattern", "Trigger cron"),
      timezone: requiredString(cron, "timezone", "Trigger cron"),
    }),
    ...(input["next_fire_at"] === undefined
      ? {}
      : { nextFireAt: timestamp(input["next_fire_at"], "next_fire_at") }),

  })
}

function triggerObject(
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
  return timestampString(value, `Trigger response.${field}`)
}
