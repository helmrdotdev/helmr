import type { AnyTask, NoPayload, SecretDecls, TaskOutput, TaskSecrets, TaskStartPayload } from "./internal"
import {
  HelmrClient,
  startTaskClientMethod,
  type TaskStartAndWaitOptions,
  type TaskStartOptions,
  type TaskStartResult,
} from "./runtime/client"

let defaultClient: HelmrClient | undefined

export function getDefaultClient(): HelmrClient {
  defaultClient ??= new HelmrClient()
  return defaultClient
}

export function resetDefaultClientForTest(): void {
  defaultClient = undefined
}

export type StartOptions<TSecrets extends SecretDecls> = TaskStartOptions<TSecrets>
export type StartAndWaitOptions<TSecrets extends SecretDecls> = TaskStartAndWaitOptions<TSecrets>

export type StartArgs<TTask extends AnyTask> =
  [TaskStartPayload<TTask>] extends [NoPayload]
    ? [id: string, opts: StartOptions<TaskSecrets<TTask>>]
    : [id: string, payload: TaskStartPayload<TTask>, opts: StartOptions<TaskSecrets<TTask>>]

export type StartAndWaitArgs<TTask extends AnyTask> =
  [TaskStartPayload<TTask>] extends [NoPayload]
    ? [id: string, opts: StartAndWaitOptions<TaskSecrets<TTask>>]
    : [id: string, payload: TaskStartPayload<TTask>, opts: StartAndWaitOptions<TaskSecrets<TTask>>]

export type TaskStartArgs<TTask extends AnyTask> =
  [TaskStartPayload<TTask>] extends [NoPayload]
    ? [opts: StartOptions<TaskSecrets<TTask>>]
    : [payload: TaskStartPayload<TTask>, opts: StartOptions<TaskSecrets<TTask>>]

export const tasks = {
  start<TTask extends AnyTask>(
    ...args: StartArgs<TTask>
  ): Promise<TaskStartResult<TaskOutput<TTask>>> {
    return getDefaultClient().tasks.start<TTask>(...args)
  },
  startAndWait<TTask extends AnyTask>(
    ...args: StartAndWaitArgs<TTask>
  ) {
    return getDefaultClient().tasks.startAndWait<TTask>(...args)
  },
}

export function startTask<TTask extends AnyTask>(
  task: TTask,
  ...args: TaskStartArgs<TTask>
): Promise<TaskStartResult<TaskOutput<TTask>>> {
  return getDefaultClient()[startTaskClientMethod](task, ...args)
}
