import type { AnyTask, NoPayload, SecretDecls, TaskOutput, TaskSecrets, SessionStartPayload } from "./internal"
import {
  HelmrClient,
  type SessionStartAndWaitOptions,
  type SessionStartAndWaitResult,
  type SessionStartOptions,
  type SessionStartResult,
  startSessionClientMethod,
} from "./runtime/client"

let defaultClient: HelmrClient | undefined

export function getDefaultClient(): HelmrClient {
  defaultClient ??= new HelmrClient()
  return defaultClient
}

export function resetDefaultClientForTest(): void {
  defaultClient = undefined
}

export type StartOptions<TSecrets extends SecretDecls> = SessionStartOptions<TSecrets>
export type StartAndWaitOptions<TSecrets extends SecretDecls> = SessionStartAndWaitOptions<TSecrets>

export type StartArgs<TTask extends AnyTask> =
  [SessionStartPayload<TTask>] extends [NoPayload]
    ? [id: string, opts: StartOptions<TaskSecrets<TTask>>]
    : [id: string, payload: SessionStartPayload<TTask>, opts: StartOptions<TaskSecrets<TTask>>]

export type StartAndWaitArgs<TTask extends AnyTask> =
  [SessionStartPayload<TTask>] extends [NoPayload]
    ? [id: string, opts: StartAndWaitOptions<TaskSecrets<TTask>>]
    : [id: string, payload: SessionStartPayload<TTask>, opts: StartAndWaitOptions<TaskSecrets<TTask>>]

export type SessionStartArgs<TTask extends AnyTask> =
  [SessionStartPayload<TTask>] extends [NoPayload]
    ? [opts: StartOptions<TaskSecrets<TTask>>]
    : [payload: SessionStartPayload<TTask>, opts: StartOptions<TaskSecrets<TTask>>]

export const sessions = {
  start<TTask extends AnyTask>(
    ...args: StartArgs<TTask>
  ): Promise<SessionStartResult<TaskOutput<TTask>>> {
    return getDefaultClient().sessions.start<TTask>(...args)
  },
  startAndWait<TTask extends AnyTask>(
    ...args: StartAndWaitArgs<TTask>
  ): Promise<SessionStartAndWaitResult<TaskOutput<TTask>>> {
    return getDefaultClient().sessions.startAndWait<TTask>(...args)
  },
}

export function startTask<TTask extends AnyTask>(
  task: TTask,
  ...args: SessionStartArgs<TTask>
): Promise<SessionStartResult<TaskOutput<TTask>>> {
  return getDefaultClient()[startSessionClientMethod](task, ...args)
}
