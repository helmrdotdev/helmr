import type {
  JsonValue,
  MaybePromise,
  PayloadTaskDefinition,
  TaskConfigBase,
  TaskExecutionContext,
  WorkspaceTarget,
} from "./contract"
import { createScheduledTask } from "./definitions"
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
  scheduleType: "declarative" | "imperative"
  key?: string
  upcoming: readonly string[]
}>

export interface ScheduledTaskPayload {
  readonly scheduledAt: Date
  readonly lastScheduledAt?: Date
  readonly timezone: string
  readonly scheduleId: string
  readonly scheduleType: "declarative" | "imperative"
  readonly key?: string
  readonly upcoming: readonly Date[]
}

type DeclarativeScheduleFields =
  | Readonly<{ cron: Cron; workspace: WorkspaceTarget }>
  | Readonly<{ cron?: never; workspace?: never }>

export type ScheduledTaskConfig<TOutput extends JsonValue> = Omit<
  TaskConfigBase,
  "id"
> &
  DeclarativeScheduleFields &
  Readonly<{
    id: string
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
  if (config.cron !== undefined) {
    validateCron(config.cron)
  }
  const schedule =
    config.cron === undefined
      ? undefined
      : {
          cron: config.cron.pattern,
          timezone: config.cron.timezone,
          workspace: config.workspace,
        }
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
    ...(schedule === undefined ? {} : { schedule }),
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
        "scheduleType",
        "key",
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
        (input["scheduleType"] !== "declarative" &&
          input["scheduleType"] !== "imperative") ||
        (input["key"] !== undefined && typeof input["key"] !== "string") ||
        !Array.isArray(upcoming)
      ) {
        return { issues: [{ message: "invalid scheduled task input" }] }
      }
      const parsedUpcoming = upcoming.map(parseDate)
      if (parsedUpcoming.some((date) => date === undefined)) {
        return { issues: [{ message: "invalid upcoming timestamp" }] }
      }
      return {
        value: {
          scheduledAt,
          ...(lastScheduledAt === undefined ? {} : { lastScheduledAt }),
          timezone: input["timezone"],
          scheduleId: input["scheduleId"],
          scheduleType: input["scheduleType"],
          ...(input["key"] === undefined ? {} : { key: input["key"] }),
          upcoming: parsedUpcoming as Date[],
        },
      }
    },
  },
}

function parseDate(value: unknown): Date | undefined {
  if (typeof value !== "string") return undefined
  const date = new Date(value)
  return Number.isNaN(date.valueOf()) || date.toISOString() !== value
    ? undefined
    : date
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
  const fields = cron.pattern.split(" ")
  if (fields.length !== 5 || fields.some((field) => field.length === 0)) {
    throw new Error("cron pattern must contain exactly five ASCII-space-separated fields")
  }
  const bounds = [
    [0, 59],
    [0, 23],
    [1, 31],
    [1, 12],
    [0, 7],
  ] as const
  fields.forEach((field, index) => {
    validateCronField(
      field,
      bounds[index] as readonly [number, number],
      index === 4,
    )
  })
  let resolved: string
  try {
    resolved = new Intl.DateTimeFormat("en-US", {
      timeZone: cron.timezone,
    }).resolvedOptions().timeZone
  } catch {
    throw new Error(`cron timezone is not available: ${JSON.stringify(cron.timezone)}`)
  }
  if (resolved !== cron.timezone) {
    throw new Error("cron timezone must be an exact case-sensitive IANA identifier")
  }
}

function validateCronField(
  field: string,
  [minimum, maximum]: readonly [number, number],
  sundayAlias: boolean,
): void {
  const clauses = field.split(",")
  if (clauses.some((clause) => clause.length === 0)) {
    throw new Error(`invalid cron field ${JSON.stringify(field)}`)
  }
  let previous = minimum - 1
  const owners = new Map<number, number>()
  for (const [clauseIndex, clause] of clauses.entries()) {
    const values = expandCronClause(clause, minimum, maximum)
    if ((values[0] as number) <= previous) {
      throw new Error("cron list clauses must be disjoint and increasing")
    }
    previous = values.at(-1) as number
    for (const value of values) {
      const normalized = sundayAlias && value === 7 ? 0 : value
      const owner = owners.get(normalized)
      if (owner !== undefined && owner !== clauseIndex) {
        throw new Error("cron list clauses overlap")
      }
      owners.set(normalized, clauseIndex)
    }
  }
}

function expandCronClause(
  clause: string,
  minimum: number,
  maximum: number,
): number[] {
  const parts = clause.split("/")
  if (parts.length > 2) throw new Error(`invalid cron clause ${JSON.stringify(clause)}`)
  const base = parts[0] as string
  const step =
    parts[1] === undefined
      ? 1
      : parseCronInteger(parts[1], 1, maximum - minimum + 1)
  let start: number
  let end: number
  if (base === "*") {
    start = minimum
    end = maximum
  } else {
    const range = base.split("-")
    if (range.length === 1) {
      if (parts[1] !== undefined) {
        throw new Error("cron step requires a wildcard or range")
      }
      start = parseCronInteger(range[0] as string, minimum, maximum)
      end = start
    } else if (range.length === 2) {
      start = parseCronInteger(range[0] as string, minimum, maximum)
      end = parseCronInteger(range[1] as string, minimum, maximum)
      if (start > end) throw new Error("cron range must be increasing")
    } else {
      throw new Error(`invalid cron range ${JSON.stringify(base)}`)
    }
  }
  const values: number[] = []
  for (let value = start; value <= end; value += step) values.push(value)
  return values
}

function parseCronInteger(value: string, minimum: number, maximum: number): number {
  if (!/^(0|[1-9][0-9]*)$/.test(value)) {
    throw new Error(`invalid cron integer ${JSON.stringify(value)}`)
  }
  const parsed = Number(value)
  if (!Number.isSafeInteger(parsed) || parsed < minimum || parsed > maximum) {
    throw new Error(`cron integer ${value} is outside [${minimum},${maximum}]`)
  }
  return parsed
}
