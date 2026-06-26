import type { AnyTask, SecretDecls } from "./internal"
import {
  HelmrClient,
  type SessionStartAndWaitOptions,
  type SessionStartOptions,
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

export function defaultClientNamespace<TKey extends keyof HelmrClient>(key: TKey): HelmrClient[TKey] {
  return new Proxy({} as HelmrClient[TKey], {
    get(_target, property, receiver) {
      return Reflect.get(getDefaultClient()[key] as object, property, receiver)
    },
  })
}

export type StartOptions<TSecrets extends SecretDecls> = SessionStartOptions<TSecrets>
export type StartAndWaitOptions<TSecrets extends SecretDecls> = SessionStartAndWaitOptions<TSecrets>

export type StartArgs<TTask extends AnyTask> = SessionsStartArgs<TTask>
export type StartAndWaitArgs<TTask extends AnyTask> = SessionsStartAndWaitArgs<TTask>

export const sessions = defaultClientNamespace("sessions")
