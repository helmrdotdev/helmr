import type { AnyTask, SecretDecls, TaskOutput } from "./internal"
import {
  HelmrClient,
  type SessionStartAndWaitOptions,
  type SessionStartAndWaitResult,
  type SessionStartOptions,
  type SessionStartResult,
  type SessionsStartArgs,
  type SessionsStartAndWaitArgs,
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

export type StartArgs<TTask extends AnyTask> = SessionsStartArgs<TTask>
export type StartAndWaitArgs<TTask extends AnyTask> = SessionsStartAndWaitArgs<TTask>

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
