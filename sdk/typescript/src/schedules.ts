import {
  markScheduledTask,
  type MarkedTask,
  type MaybePromise,
  type SecretDecls,
  type TaskContext,
  type TaskConfigBase,
} from "./internal"
import type { PayloadSchema } from "./schema/payload"

export interface ScheduledTaskPayload {
  readonly timestamp: Date
  readonly lastTimestamp?: Date
  readonly timezone: string
  readonly scheduleId: string
  readonly scheduleType: "declarative" | "imperative"
  readonly externalId?: string
  readonly upcoming: readonly Date[]
}

export type ScheduleCron =
  | string
  | {
      readonly pattern: string
      readonly timezone?: string
    }

type DeclarativeScheduleFields =
  | {
      readonly cron: ScheduleCron
    }
  | {
      readonly cron?: undefined
    }

export type ScheduledTaskConfig<
  TOutput = unknown,
  TSecrets extends SecretDecls = readonly [],
> = Omit<TaskConfigBase<TSecrets>, "payload"> & DeclarativeScheduleFields & {
  readonly payload?: never
  readonly run: (payload: ScheduledTaskPayload, ctx: TaskContext) => MaybePromise<TOutput>
}

export function task<TOutput = unknown, TSecrets extends SecretDecls = readonly []>(
  config: ScheduledTaskConfig<TOutput, TSecrets>,
): MarkedTask<ScheduledTaskPayload, Awaited<TOutput>, TSecrets, typeof scheduledTaskPayloadSchema> {
  const { cron, ...taskConfig } = config
  const schedule = cron === undefined
    ? undefined
    : {
        cron: typeof cron === "string" ? cron : cron.pattern,
        ...(typeof cron === "string" || cron.timezone === undefined ? {} : { timezone: cron.timezone }),
      }
  const marked = markScheduledTask(
    { ...taskConfig, payload: scheduledTaskPayloadSchema } as never,
    schedule,
  )
  return marked as unknown as MarkedTask<ScheduledTaskPayload, Awaited<TOutput>, TSecrets, typeof scheduledTaskPayloadSchema>
}

const scheduledTaskPayloadSchema: PayloadSchema<unknown, ScheduledTaskPayload> = {
  "~standard": {
    version: 1,
    vendor: "helmr",
    validate(value) {
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        return { issues: [{ message: "expected scheduled task payload object" }] }
      }
      const input = value as Record<string, unknown>
      const timestamp = parseDateField(input["timestamp"], "timestamp")
      const lastTimestamp = parseOptionalDateField(input["lastTimestamp"], "lastTimestamp")
      const timezone = input["timezone"]
      const scheduleId = input["scheduleId"]
      const scheduleType = input["scheduleType"]
      const externalId = input["externalId"]
      const upcoming = input["upcoming"]
      const issues = [
        ...timestamp.issues,
        ...lastTimestamp.issues,
        ...(typeof timezone === "string" && timezone.trim() !== "" ? [] : [{ message: "expected string", path: ["timezone"] }]),
        ...(typeof scheduleId === "string" && scheduleId.trim() !== "" ? [] : [{ message: "expected string", path: ["scheduleId"] }]),
        ...(scheduleType === "declarative" || scheduleType === "imperative" ? [] : [{ message: "expected declarative or imperative", path: ["scheduleType"] }]),
        ...(externalId === undefined || typeof externalId === "string" ? [] : [{ message: "expected string", path: ["externalId"] }]),
        ...(Array.isArray(upcoming) ? [] : [{ message: "expected array", path: ["upcoming"] }]),
      ]
      const upcomingDates = Array.isArray(upcoming)
        ? upcoming.map((item, index) => parseDateField(item, `upcoming.${index}`))
        : []
      issues.push(...upcomingDates.flatMap((item) => item.issues))
      if (issues.length > 0) {
        return { issues }
      }
      return {
        value: {
          timestamp: timestamp.value,
          ...(lastTimestamp.value === undefined ? {} : { lastTimestamp: lastTimestamp.value }),
          timezone: timezone as string,
          scheduleId: scheduleId as string,
          scheduleType: scheduleType as "declarative" | "imperative",
          ...(externalId === undefined ? {} : { externalId: externalId as string }),
          upcoming: upcomingDates.map((item) => item.value),
        },
      }
    },
  },
}

function parseDateField(value: unknown, path: string): { value: Date; issues: { message: string; path?: (string | number)[] }[] } {
  if (typeof value !== "string") {
    return { value: new Date(0), issues: [{ message: "expected ISO timestamp", path: path.split(".") }] }
  }
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) {
    return { value: new Date(0), issues: [{ message: "expected ISO timestamp", path: path.split(".") }] }
  }
  return { value: date, issues: [] }
}

function parseOptionalDateField(value: unknown, path: string): { value?: Date; issues: { message: string; path?: (string | number)[] }[] } {
  if (value === undefined || value === null) {
    return { issues: [] }
  }
  return parseDateField(value, path)
}
