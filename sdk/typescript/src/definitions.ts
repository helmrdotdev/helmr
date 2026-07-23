import {
  type ActorConfig,
  type ActorDefinition,
  type ActorIdRef,
  type ActorInputRef,
  type ActorKeyRef,
  type ActorOutputRef,
  type ActorRefBase,
  type ActorStartOptions,
  type ActorStatus,
  type ActorUpdateOptions,
  type ActorOperationOptions,
  type ActorOperationReceipt,
  type JsonValue,
  type NoPayloadTaskDefinition,
  type OutputAppendOptions,
  type OutputReadOptions,
  type OutputSequenceOptions,
  type PayloadTaskDefinition,
  type QueueDefinition,
  type RunHandle,
  type RunStreamRecord,
  type TaskCallOptions,
  type TaskConfigWithPayload,
  type TaskConfigWithoutPayload,
  type TaskStartOptions,
  type TaskWait,
  type TypedRunStreamDefinition,
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
import { trimGoSpace } from "./internal/strings"

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
    workspace: import("./contract").WorkspaceTarget
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

export type InternalRunStreamDefinition = Readonly<{
  kind: "run_stream"
  id: string
  schema: PayloadSchema<JsonValue, JsonValue>
}>

export type InternalDefinition =
  | InternalTaskDefinition
  | InternalActorDefinition
  | InternalRunStreamDefinition

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
  if (typeof queue.id !== "string") {
    throw new Error("invalid private queue record")
  }
  validateQueueName(queue.id)
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
    readonly schema?: unknown
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
    case "run_stream":
      assertPayloadSchema(
        definition.schema,
        `run stream ${JSON.stringify(definition.id)} schema`,
      )
      return true
    default:
      return false
  }
}

export function queue(config: {
  readonly id: string
  readonly concurrencyLimit?: number | null
}): QueueDefinition {
  validateQueueName(config.id)
  validateOptionalQueueConcurrencyLimit(config.concurrencyLimit)
  return Object.freeze(
    Object.defineProperty(
      {
        id: config.id,
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
    start(_options: ActorStartOptions) {
      return runtimeUnavailable<Promise<{ ref: ActorIdRef; run: RunHandle }>>(
        "actor.start",
      )
    },
    ref(address: { readonly id: string } | { readonly key: string }) {
      return createActorRef(internal.id, address)
    },
  }
  return brandDefinition(value, internal) as ActorDefinition
}

export function stream<
  TInput extends JsonValue,
  TRecord extends JsonValue,
>(config: {
  readonly id: string
  readonly schema: PayloadSchema<TInput, TRecord>
}): TypedRunStreamDefinition<TInput, TRecord> {
  validateTaskId(config.id)
  assertPayloadSchema(
    config.schema,
    `run stream ${JSON.stringify(config.id)} schema`,
  )
  const internal: InternalRunStreamDefinition = Object.freeze({
    kind: "run_stream",
    id: config.id,
    schema: config.schema as PayloadSchema<JsonValue, JsonValue>,
  })
  const value = {
    id: config.id,
    append(_value: TInput, _options?: OutputAppendOptions) {
      return runtimeUnavailable<Promise<RunStreamRecord<TRecord>>>(
        "run stream append",
      )
    },
    pipe(
      _source: AsyncIterable<TInput> | Iterable<TInput>,
      _options?: OutputSequenceOptions,
    ) {
      return runtimeUnavailable<Promise<void>>("run stream pipe")
    },
    writer(_options?: OutputSequenceOptions) {
      return runtimeUnavailable<{
        write(value: TInput): Promise<RunStreamRecord<TRecord>>
        close(): Promise<void>
      }>("run stream writer")
    },
    read(_runId: string, _options?: OutputReadOptions) {
      return runtimeUnavailable<AsyncIterable<RunStreamRecord<TRecord>>>(
        "run stream read",
      )
    },
    list(_runId: string, _options?: OutputReadOptions) {
      return runtimeUnavailable<Promise<readonly RunStreamRecord<TRecord>[]>>(
        "run stream list",
      )
    },
  }
  return brandDefinition(value, internal) as unknown as TypedRunStreamDefinition<
    TInput,
    TRecord
  >
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
    readonly schedule?: InternalTaskDefinition["schedule"]
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
    ...(config.schedule === undefined ? {} : { schedule: config.schedule }),
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
        start(_payload: JsonValue, _options: TaskStartOptions) {
          return runtimeUnavailable<Promise<RunHandle>>("task.start")
        },
        call(_payload: JsonValue, _options: TaskCallOptions) {
          return runtimeUnavailable<TaskWait<JsonValue>>("task.call")
        },
      },
      internal,
    ) as unknown as PayloadTaskDefinition<JsonValue, unknown, JsonValue>
  }
  return brandDefinition(
    {
      id: internal.id,
      hasPayload: false as const,
      start(_options: TaskStartOptions) {
        return runtimeUnavailable<Promise<RunHandle>>("task.start")
      },
      call(_options: TaskCallOptions) {
        return runtimeUnavailable<TaskWait<JsonValue>>("task.call")
      },
    },
    internal,
  ) as unknown as NoPayloadTaskDefinition<JsonValue>
}

function createActorRef(
  declaredId: string,
  address: { readonly id: string } | { readonly key: string },
): ActorIdRef | ActorKeyRef {
  const immutableAddress = validatedActorRefAddress(address)
  const base: ActorRefBase = {
    input: createActorInputRef(declaredId, immutableAddress),
    output: createActorOutputRef(),
    status() {
      return runtimeUnavailable<Promise<ActorStatus>>("actor.status")
    },
    update(_options: ActorUpdateOptions) {
      return runtimeUnavailable<Promise<ActorStatus>>("actor.update")
    },
    close(_options?: ActorOperationOptions) {
      return runtimeUnavailable<Promise<ActorOperationReceipt>>("actor.close")
    },
  }
  return Object.freeze({
    ...base,
    ...immutableAddress,
  }) as ActorIdRef | ActorKeyRef
}

function validatedActorRefAddress(
  address: { readonly id: string } | { readonly key: string },
): Readonly<{ id: string }> | Readonly<{ key: string }> {
  const value = address as Readonly<Record<string, unknown>>
  const hasId = Object.prototype.hasOwnProperty.call(value, "id")
  const hasKey = Object.prototype.hasOwnProperty.call(value, "key")
  if (hasId === hasKey) {
    throw new Error("actor ref requires exactly one of id or key")
  }
  if (hasId) {
    if (
      typeof value["id"] !== "string" ||
      !/^act_[a-z2-7]{26}$/.test(value["id"])
    ) {
      throw new Error("actor ref id must be a canonical Actor public ID")
    }
    return Object.freeze({ id: value["id"] })
  }
  if (typeof value["key"] !== "string") {
    throw new Error("actor ref key must be a string")
  }
  const encoded = new TextEncoder().encode(value["key"])
  if (
    encoded.byteLength === 0 ||
    encoded.byteLength > 512 ||
    value["key"].includes("\0") ||
    trimGoSpace(value["key"]) !== value["key"]
  ) {
    throw new Error(
      "actor ref key must be 1-512 UTF-8 bytes without NUL or edge whitespace",
    )
  }
  return Object.freeze({ key: value["key"] })
}

function createActorInputRef(
  declaredId: string,
  address: { readonly id: string } | { readonly key: string },
): ActorInputRef {
  return Object.freeze({
    send(input: JsonValue, options?: import("./contract").SendOptions) {
      return currentRuntimeOperations().actorInputSend(
        Object.freeze({ declaredId, address }),
        input,
        options,
      )
    },
  })
}

function createActorOutputRef(): ActorOutputRef {
  return Object.freeze({
    read(_options?: OutputReadOptions) {
      return runtimeUnavailable<AsyncIterable<import("./contract").ActorOutputRecord>>(
        "actor.output.read",
      )
    },
    list(_options?: OutputReadOptions) {
      return runtimeUnavailable<
        Promise<readonly import("./contract").ActorOutputRecord[]>
      >("actor.output.list")
    },
  })
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

function runtimeUnavailable<T>(operation: string): T {
  throw new Error(
    `${operation} is unavailable without the Helmr managed runtime or authenticated client`,
  )
}
