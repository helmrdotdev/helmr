import {
  type ActorConfig,
  type ActorDefinition,
  type ActorStartOptions,
  type JsonValue,
  type NoPayloadTaskDefinition,
  type PayloadTaskDefinition,
  type QueueDefinition,
  type TaskCallOptions,
  type TaskConfigWithPayload,
  type TaskConfigWithoutPayload,
  type TaskStartOptions,
  type TaskWait,
} from "./contract"
import {
  assertPayloadSchema,
  type PayloadSchema,
} from "./schema/payload"
import {
  validateOptionalQueueConcurrencyLimit,
  validateQueueName,
  validateTaskId,
} from "./schema/task"
import { currentRuntimeOperations } from "./internal/runtime"
import { runFailureError } from "./internal/run-failure"
import { createRuntimeSessionRef } from "./session"

const privateDefinitionBrand = Symbol.for("helmr.sdk.v0.definition")
const privateQueueBrand = Symbol.for("helmr.sdk.v0.queue")

export type InternalTaskDefinition = Readonly<{
  kind: "task"
  id: string
  hasPayload: boolean
  handler: (...args: readonly unknown[]) => unknown
  payloadSchema?: PayloadSchema
  queue?: QueueDefinition | string
  maxDuration?: import("./contract").Duration
  ttl?: import("./contract").Duration
  retry?: ActorConfig["retry"]
  schedule?: Readonly<{
    cron: string
    timezone: string
    workspace: Readonly<{
      sandbox: import("./workspace").SandboxDefinition
      secrets: readonly import("./workspace").EncodedWorkspaceSecret[]
    }>
  }>
}>

export type InternalActorDefinition = Readonly<{
  kind: "actor"
  id: string
  handler: ActorConfig["run"]
  queue?: QueueDefinition | string
  maxDuration?: import("./contract").Duration
  ttl?: import("./contract").Duration
  retry?: ActorConfig["retry"]
  idleTimeout?: ActorConfig["idleTimeout"]
}>

export type InternalDefinition =
  | InternalTaskDefinition
  | InternalActorDefinition

type BrandedDefinition = {
  readonly [privateDefinitionBrand]: InternalDefinition
}

type BrandedQueue = {
  readonly [privateQueueBrand]: true
}

export function inspectDefinition(value: unknown): InternalDefinition | undefined {
  if (
    (typeof value !== "object" && typeof value !== "function") ||
    value === null
  ) {
    return undefined
  }
  if (!Object.hasOwn(value, privateDefinitionBrand)) return undefined
  const definition =
    (value as Partial<BrandedDefinition>)[privateDefinitionBrand]
  if (!isInternalDefinition(definition)) {
    throw new Error("invalid private definition record")
  }
  return definition
}

export function isQueueDefinition(value: unknown): value is QueueDefinition {
  if (typeof value !== "object" || value === null) return false
  if (!Object.hasOwn(value, privateQueueBrand)) return false
  if ((value as Partial<BrandedQueue>)[privateQueueBrand] !== true) {
    throw new Error("invalid private queue record")
  }
  const queue = value as Partial<QueueDefinition>
  if (typeof queue.name !== "string") {
    throw new Error("invalid private queue record")
  }
  validateQueueName(queue.name)
  validateOptionalQueueConcurrencyLimit(queue.concurrencyLimit)
  return true
}

function isInternalDefinition(
  value: unknown,
): value is InternalDefinition {
  if (typeof value !== "object" || value === null) return false
  const definition = value as Partial<InternalDefinition> & {
    readonly hasPayload?: unknown
    readonly handler?: unknown
    readonly payloadSchema?: unknown
  }
  if (typeof definition.id !== "string") return false
  validateTaskId(definition.id)
  switch (definition.kind) {
    case "task":
      if (
        typeof definition.handler !== "function" ||
        typeof definition.hasPayload !== "boolean"
      ) {
        return false
      }
      if (definition.hasPayload) {
        assertPayloadSchema(
          definition.payloadSchema,
          `task ${JSON.stringify(definition.id)} payload`,
        )
      } else if (Object.hasOwn(definition, "payloadSchema")) {
        return false
      }
      return true
    case "actor":
      return typeof definition.handler === "function"
    default:
      return false
  }
}

export function queue(config: {
  readonly name: string
  readonly concurrencyLimit?: number | null
}): QueueDefinition {
  validateQueueName(config.name)
  validateOptionalQueueConcurrencyLimit(config.concurrencyLimit)
  return Object.freeze(
    Object.defineProperty(
      {
        name: config.name,
        ...(config.concurrencyLimit === undefined
          ? {}
          : { concurrencyLimit: config.concurrencyLimit }),
      },
      privateQueueBrand,
      { value: true },
    ),
  )
}

export function task<
  TInput extends JsonValue,
  TPayload,
  TOutput extends JsonValue,
>(
  config: TaskConfigWithPayload<TInput, TPayload, TOutput>,
): PayloadTaskDefinition<TInput, TPayload, TOutput>
export function task<TOutput extends JsonValue>(
  config: TaskConfigWithoutPayload<TOutput>,
): NoPayloadTaskDefinition<TOutput>
export function task(
  config:
    | TaskConfigWithPayload<JsonValue, unknown, JsonValue>
    | TaskConfigWithoutPayload<JsonValue>,
):
  | PayloadTaskDefinition<JsonValue, unknown, JsonValue>
  | NoPayloadTaskDefinition<JsonValue> {
  validateDefinitionDefaults(config, `task ${JSON.stringify(config.id)}`)
  const hasPayload = "payload" in config
  if (hasPayload) {
    assertPayloadSchema(config.payload, `task ${JSON.stringify(config.id)} payload`)
  }
  const internal: InternalTaskDefinition = Object.freeze({
    kind: "task",
    id: config.id,
    hasPayload,
    handler: config.run as (...args: readonly unknown[]) => unknown,
    ...(hasPayload ? { payloadSchema: config.payload } : {}),
    ...copyDefinitionDefaults(config),
  })
  return createTaskDefinition(internal) as
    | PayloadTaskDefinition<JsonValue, unknown, JsonValue>
    | NoPayloadTaskDefinition<JsonValue>
}

export function actor(config: ActorConfig): ActorDefinition {
  validateDefinitionDefaults(config, `actor ${JSON.stringify(config.id)}`)
  if (typeof config.run !== "function") {
    throw new Error(`actor ${JSON.stringify(config.id)} run must be a function`)
  }
  const internal: InternalActorDefinition = Object.freeze({
    kind: "actor",
    id: config.id,
    handler: config.run,
    ...copyDefinitionDefaults(config),
    ...(config.idleTimeout === undefined
      ? {}
      : { idleTimeout: config.idleTimeout }),
  })
  const value = {
    id: internal.id,
    async start(options: ActorStartOptions) {
      const started = await currentRuntimeOperations().actorStart(
        internal.id,
        options,
      )
      return Object.freeze({
        session: createRuntimeSessionRef(started.sessionId),
        run: Object.freeze({ id: started.runId }),
      })
    },
  }
  return brandDefinition(value, internal) as ActorDefinition
}

export function createScheduledTask<
  TInput extends JsonValue,
  TPayload,
  TOutput extends JsonValue,
>(
  config: Omit<
    TaskConfigWithPayload<TInput, TPayload, TOutput>,
    "payload"
  > & {
    readonly payload: PayloadSchema<TInput, TPayload>
    readonly schedule: NonNullable<InternalTaskDefinition["schedule"]>
  },
): PayloadTaskDefinition<TInput, TPayload, TOutput> {
  const base = task({
    id: config.id,
    payload: config.payload,
    run: config.run,
    ...(config.queue === undefined ? {} : { queue: config.queue }),
    ...(config.maxDuration === undefined
      ? {}
      : { maxDuration: config.maxDuration }),
    ...(config.ttl === undefined ? {} : { ttl: config.ttl }),
    ...(config.retry === undefined ? {} : { retry: config.retry }),
  })
  const inspected = inspectDefinition(base)
  if (inspected?.kind !== "task") {
    throw new Error("scheduled task construction failed")
  }
  const internal: InternalTaskDefinition = Object.freeze({
    ...inspected,
    schedule: config.schedule,
  })
  return brandDefinition(
    {
      id: base.id,
      hasPayload: true as const,
      start: base.start.bind(base),
      call: base.call.bind(base),
    },
    internal,
  ) as unknown as PayloadTaskDefinition<TInput, TPayload, TOutput>
}

function createTaskDefinition(
  internal: InternalTaskDefinition,
):
  | PayloadTaskDefinition<JsonValue, unknown, JsonValue>
  | NoPayloadTaskDefinition<JsonValue> {
  if (internal.hasPayload) {
    return brandDefinition(
      {
        id: internal.id,
        hasPayload: true as const,
        start(payload: JsonValue, options: TaskStartOptions) {
          return currentRuntimeOperations().taskStart(
            Object.freeze({ declaredId: internal.id, payloadPresent: true }),
            payload,
            options,
          )
        },
        call(payload: JsonValue, options: TaskCallOptions) {
          return taskWait(
            currentRuntimeOperations().taskCall(
              Object.freeze({ declaredId: internal.id, payloadPresent: true }),
              payload,
              options,
            ),
          )
        },
      },
      internal,
    ) as unknown as PayloadTaskDefinition<JsonValue, unknown, JsonValue>
  }
  return brandDefinition(
    {
      id: internal.id,
      hasPayload: false as const,
      start(options: TaskStartOptions) {
        return currentRuntimeOperations().taskStart(
          Object.freeze({ declaredId: internal.id, payloadPresent: false }),
          undefined,
          options,
        )
      },
      call(options: TaskCallOptions) {
        return taskWait(
          currentRuntimeOperations().taskCall(
            Object.freeze({ declaredId: internal.id, payloadPresent: false }),
            undefined,
            options,
          ),
        )
      },
    },
    internal,
  ) as unknown as NoPayloadTaskDefinition<JsonValue>
}

function brandDefinition<T extends object>(
  value: T,
  internal: InternalDefinition,
): T {
  Object.defineProperty(value, privateDefinitionBrand, {
    value: internal,
    enumerable: false,
  })
  return Object.freeze(value)
}

function validateDefinitionDefaults(
  config: ActorConfig | TaskConfigWithPayload<JsonValue, unknown, JsonValue> | TaskConfigWithoutPayload<JsonValue>,
  label: string,
): void {
  validateTaskId(config.id)
  if (typeof config.run !== "function") {
    throw new Error(`${label} run must be a function`)
  }
  if (config.maxDuration !== undefined && typeof config.maxDuration !== "string") {
    throw new Error(`${label} maxDuration must be a duration string`)
  }
  if (typeof config.queue === "string") {
    validateQueueName(config.queue)
  } else if (config.queue !== undefined && !isQueueDefinition(config.queue)) {
    throw new Error(`${label} queue must be a queue() definition or queue name`)
  }
  if (config.ttl !== undefined && typeof config.ttl !== "string") {
    throw new Error(`${label} ttl must be a duration string`)
  }
}

function copyDefinitionDefaults(
  config: ActorConfig | TaskConfigWithPayload<JsonValue, unknown, JsonValue> | TaskConfigWithoutPayload<JsonValue>,
): Pick<
  InternalActorDefinition,
  "queue" | "maxDuration" | "ttl" | "retry"
> {
  return {
    ...(config.queue === undefined ? {} : { queue: config.queue }),
    ...(config.maxDuration === undefined
      ? {}
      : { maxDuration: config.maxDuration }),
    ...(config.ttl === undefined ? {} : { ttl: config.ttl }),
    ...(config.retry === undefined ? {} : { retry: config.retry }),
  }
}

function taskWait<T extends JsonValue>(
  result: Promise<import("./contract").TaskResult<T>>,
): TaskWait<T> {
  return Object.freeze({
    then<TResult1 = import("./contract").TaskResult<T>, TResult2 = never>(
      onfulfilled?: (
        (value: import("./contract").TaskResult<T>) =>
          | TResult1
          | PromiseLike<TResult1>
      ) | null,
      onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
    ): PromiseLike<TResult1 | TResult2> {
      return result.then(onfulfilled, onrejected)
    },
    async unwrap(): Promise<T> {
      const settled = await result
      if (!settled.ok) throw runFailureError(settled.failure)
      return settled.output
    },
  })
}
