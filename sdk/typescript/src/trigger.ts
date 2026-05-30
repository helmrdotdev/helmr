import type { AnyTask, NoPayload, SecretDecls, TaskOutput, TaskRunOptions, TaskSecrets, TaskTriggerPayload } from "./internal"
import { HelmrClient, triggerTaskClientMethod } from "./runtime/client"
import type { RunHandle } from "./runtime/run"

let defaultClient: HelmrClient | undefined

export function getDefaultClient(): HelmrClient {
  defaultClient ??= new HelmrClient()
  return defaultClient
}

export type TriggerOptions<TSecrets extends SecretDecls> = TaskRunOptions<TSecrets>

export type TriggerArgs<TTask extends AnyTask> =
  [TaskTriggerPayload<TTask>] extends [NoPayload]
    ? [id: string, opts: TriggerOptions<TaskSecrets<TTask>>]
    : [id: string, payload: TaskTriggerPayload<TTask>, opts: TriggerOptions<TaskSecrets<TTask>>]

export type TaskTriggerArgs<TTask extends AnyTask> =
  [TaskTriggerPayload<TTask>] extends [NoPayload]
    ? [opts: TriggerOptions<TaskSecrets<TTask>>]
    : [payload: TaskTriggerPayload<TTask>, opts: TriggerOptions<TaskSecrets<TTask>>]

export const tasks = {
  trigger<TTask extends AnyTask>(
    ...args: TriggerArgs<TTask>
  ): Promise<RunHandle<TaskOutput<TTask>>> {
    return getDefaultClient().tasks.trigger<TTask>(...args)
  },
}

export function triggerTask<TTask extends AnyTask>(
  task: TTask,
  ...args: TaskTriggerArgs<TTask>
): Promise<RunHandle<TaskOutput<TTask>>> {
  return getDefaultClient()[triggerTaskClientMethod](task, ...args)
}
