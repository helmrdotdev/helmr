import type { AnyTask, NoPayload, SecretDecls, TaskOutput, TaskSecrets, TaskTriggerPayload, WorkspaceSpec } from "./internal"
import { HelmrClient } from "./runtime/client"
import type { RunHandle } from "./runtime/run"

let defaultClient: HelmrClient | undefined

export function getDefaultClient(): HelmrClient {
  defaultClient ??= new HelmrClient()
  return defaultClient
}

export type TriggerSecrets<TSecrets extends SecretDecls> = {
  readonly [K in keyof TSecrets]: string
}

export type TriggerOptions<TPayload, TSecrets extends SecretDecls> = {
  readonly workspace: WorkspaceSpec
} & ([TPayload] extends [NoPayload]
  ? {}
  : {
      /**
       * Payload is audit data: Helmr persists it in plaintext in the `run.created`
       * event, DB, and events stream. Do not put secret values (tokens, API keys,
       * credentials, or PII) in payload; use `secrets:` instead. Use payload for
       * business context such as PR numbers, repo names, ticket ids, and other
       * identifiers.
       */
      readonly payload: TPayload
    }) & ([keyof TSecrets] extends [never]
  ? { readonly secrets?: Record<never, never> }
  : { readonly secrets: TriggerSecrets<TSecrets> })

export const tasks = {
  trigger<TTask extends AnyTask>(
    task: TTask,
    opts: TriggerOptions<TaskTriggerPayload<TTask>, TaskSecrets<TTask>>,
  ): Promise<RunHandle<TaskOutput<TTask>>> {
    return getDefaultClient().tasks.trigger(task, opts)
  },
}
