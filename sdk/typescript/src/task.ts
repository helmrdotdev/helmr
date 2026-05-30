import {
  markTask,
  type AnyTask,
  type NoPayload,
  type SecretDecls,
  type Task,
  type TaskConfig,
  type TaskConfigWithPayload,
  type TaskConfigWithoutPayload,
  type TaskOutput,
  type TaskPayload,
  type TaskTriggerPayload,
} from "./internal"
import type {
  PayloadSchema,
  PayloadSchemaInput,
  PayloadSchemaOutput,
} from "./schema/payload"

export function task<
  TPayloadSchema extends PayloadSchema<any, any>,
  TOutput,
  TSecrets extends SecretDecls = Record<never, never>,
>(
  config: TaskConfigWithPayload<TPayloadSchema, TOutput, TSecrets>,
): Task<PayloadSchemaOutput<TPayloadSchema>, Awaited<TOutput>, TSecrets, PayloadSchemaInput<TPayloadSchema>>

export function task<TOutput = unknown, TSecrets extends SecretDecls = Record<never, never>>(
  config: TaskConfigWithoutPayload<TOutput, TSecrets>,
): Task<NoPayload, Awaited<TOutput>, TSecrets, NoPayload>

export function task(
  config: TaskConfigWithPayload<PayloadSchema<any, any>, any, SecretDecls> | TaskConfigWithoutPayload<any, SecretDecls>,
): AnyTask {
  return markTask(config)
}

export type { NoPayload, Task, TaskConfig, TaskOutput, TaskPayload, TaskTriggerPayload }
