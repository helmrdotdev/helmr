import {
  markTask,
  type SecretDecls,
  type Task,
  type TaskConfig,
  type TaskOutput,
  type TaskPayload,
} from "./internal"

export function task<TPayload, TOutput, TSecrets extends SecretDecls = Record<never, never>>(
  config: TaskConfig<TPayload, TOutput, TSecrets>,
): Task<TPayload, Awaited<TOutput>, TSecrets> {
  return markTask(config)
}

export type { Task, TaskConfig, TaskOutput, TaskPayload }
