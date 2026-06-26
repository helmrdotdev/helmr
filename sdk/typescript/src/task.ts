import {
  markTask,
  markQueueDefinition,
  type AnyTask,
  type NoPayload,
  type QueueConfig,
  type QueueDefinition,
  type SecretDecls,
  type Task,
  type TaskConfig,
  type TaskConfigWithPayload,
  type TaskConfigWithoutPayload,
  type TaskRunOptions,
  type TaskOutput,
  type TaskPayload,
  type SessionStartPayload,
} from "./internal"
import type {
  PayloadSchema,
  PayloadSchemaInput,
  PayloadSchemaOutput,
} from "./schema/payload"

export function task<
  TPayloadSchema extends PayloadSchema<any, any>,
  TOutput,
  TSecrets extends SecretDecls = readonly [],
>(
  config: TaskConfigWithPayload<TPayloadSchema, TOutput, TSecrets>,
): Task<PayloadSchemaOutput<TPayloadSchema>, Awaited<TOutput>, TSecrets, PayloadSchemaInput<TPayloadSchema>>

export function task<TOutput = unknown, TSecrets extends SecretDecls = readonly []>(
  config: TaskConfigWithoutPayload<TOutput, TSecrets>,
): Task<NoPayload, Awaited<TOutput>, TSecrets, NoPayload>

export function task(
  config: TaskConfigWithPayload<PayloadSchema<any, any>, any, SecretDecls> | TaskConfigWithoutPayload<any, SecretDecls>,
): AnyTask {
  return markTask(config)
}

export function queue(config: QueueConfig): QueueDefinition {
  return markQueueDefinition(config)
}

export type { NoPayload, QueueConfig, QueueDefinition, Task, TaskConfig, TaskOutput, TaskPayload, TaskRunOptions, SessionStartPayload }
