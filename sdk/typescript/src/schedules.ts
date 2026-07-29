import type {
  JsonValue,
  MaybePromise,
  PayloadTaskDefinition,
  TaskConfigBase,
  TaskExecutionContext,
  WorkspaceTarget,
} from "./contract"
import { createScheduledTask } from "./definitions"
import { resourceID } from "./internal/id"
import type { PayloadSchema } from "./schema/payload"

export type Cron = Readonly<{
  pattern: string
  timezone: string
}>

export type ScheduledTaskInput = Readonly<{
  scheduledAt: string
  lastScheduledAt?: string
  timezone: string
  scheduleId: string
  upcoming: readonly string[]
}>

export interface ScheduledTaskPayload {
  readonly scheduledAt: Date
  readonly lastScheduledAt?: Date
  readonly timezone: string
  readonly scheduleId: string
  readonly upcoming: readonly Date[]
}

export type ScheduledTaskConfig<TOutput extends JsonValue> = Omit<
  TaskConfigBase,
  "id"
> & Readonly<{
    id: string
    cron: Cron
    workspace: WorkspaceTarget
    run(
      payload: ScheduledTaskPayload,
      ctx: TaskExecutionContext,
    ): MaybePromise<TOutput>
  }>

export function scheduledTask<TOutput extends JsonValue>(
  config: ScheduledTaskConfig<TOutput>,
): PayloadTaskDefinition<
  ScheduledTaskInput,
  ScheduledTaskPayload,
  TOutput
> {
  validateCron(config.cron)
  return createScheduledTask({
    id: config.id,
    payload: scheduledTaskSchema,
    run: config.run,
    ...(config.queue === undefined ? {} : { queue: config.queue }),
    ...(config.maxDuration === undefined
      ? {}
      : { maxDuration: config.maxDuration }),
    ...(config.ttl === undefined ? {} : { ttl: config.ttl }),
    ...(config.retry === undefined ? {} : { retry: config.retry }),
    schedule: {
      cron: config.cron.pattern,
      timezone: config.cron.timezone,
      workspace: config.workspace,
    },
  })
}

export const schedules = Object.freeze({
  task: scheduledTask,
})

const scheduledTaskSchema: PayloadSchema<
  ScheduledTaskInput,
  ScheduledTaskPayload
> = {
  "~standard": {
    version: 1,
    vendor: "helmr",
    validate(value) {
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        return { issues: [{ message: "expected scheduled task input object" }] }
      }
      const input = value as Record<string, unknown>
      const allowed = new Set([
        "scheduledAt",
        "lastScheduledAt",
        "timezone",
        "scheduleId",
        "upcoming",
      ])
      if (Object.keys(input).some((key) => !allowed.has(key))) {
        return { issues: [{ message: "scheduled task input has unknown members" }] }
      }
      const scheduledAt = parseDate(input["scheduledAt"])
      const lastScheduledAt =
        input["lastScheduledAt"] === undefined
          ? undefined
          : parseDate(input["lastScheduledAt"])
      const upcoming = input["upcoming"]
      if (
        scheduledAt === undefined ||
        (input["lastScheduledAt"] !== undefined &&
          lastScheduledAt === undefined) ||
        typeof input["timezone"] !== "string" ||
        typeof input["scheduleId"] !== "string" ||
        !Array.isArray(upcoming)
      ) {
        return { issues: [{ message: "invalid scheduled task input" }] }
      }
      const parsedUpcoming = upcoming.map(parseDate)
      if (parsedUpcoming.some((date) => date === undefined)) {
        return { issues: [{ message: "invalid upcoming timestamp" }] }
      }
      let scheduleId: string
      try {
        scheduleId = resourceID(
          input["scheduleId"],
          "Scheduled task input.scheduleId",
        )
      } catch {
        return { issues: [{ message: "invalid scheduled task input" }] }
      }
      return {
        value: {
          scheduledAt,
          ...(lastScheduledAt === undefined ? {} : { lastScheduledAt }),
          timezone: input["timezone"],
          scheduleId,
          upcoming: parsedUpcoming as Date[],
        },
      }
    },
  },
}

function parseDate(value: unknown): Date | undefined {
  if (typeof value !== "string") return undefined
  if (
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(value)
  ) {
    return undefined
  }
  const date = new Date(value)
  return Number.isNaN(date.valueOf()) ? undefined : date
}

function validateCron(cron: Cron): void {
  if (
    cron === null ||
    typeof cron !== "object" ||
    Array.isArray(cron) ||
    Object.keys(cron).length !== 2 ||
    !Object.hasOwn(cron, "pattern") ||
    !Object.hasOwn(cron, "timezone") ||
    typeof cron.pattern !== "string" ||
    typeof cron.timezone !== "string"
  ) {
    throw new Error("cron must contain exactly pattern and timezone")
  }
  if (
    new TextEncoder().encode(cron.pattern).length === 0 ||
    new TextEncoder().encode(cron.pattern).length > 1024 ||
    new TextEncoder().encode(cron.timezone).length === 0 ||
    new TextEncoder().encode(cron.timezone).length > 255
  ) {
    throw new Error("cron pattern and timezone must be non-empty bounded strings")
  }
}
