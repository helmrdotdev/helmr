import {
  markTask,
  type AnyTask,
  type NoPayload,
  type SecretDecls,
  type Task,
  type TaskConfig,
  type TaskQueueConfig,
  type TaskConfigWithPayload,
  type TaskConfigWithoutPayload,
  type TaskRunOptions,
  type TaskOutput,
  type TaskPayload,
  type TaskTriggerPayload,
} from "./internal"
import { triggerTask } from "./trigger"
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
  const marked = markTask(config)
  Object.defineProperty(marked, "trigger", {
    value: (...args: readonly unknown[]) => (triggerTask as (...values: readonly unknown[]) => unknown)(marked, ...args),
  })
  return marked
}

export function queue(config: TaskQueueConfig): TaskQueueConfig {
  return Object.freeze({ ...config })
}

export type { NoPayload, Task, TaskConfig, TaskOutput, TaskPayload, TaskQueueConfig, TaskRunOptions, TaskTriggerPayload }
