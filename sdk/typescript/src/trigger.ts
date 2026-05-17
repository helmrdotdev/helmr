import type { SecretDecls, Task, WorkspaceSpec } from "./internal"
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
  /**
   * Payload is audit data: Helmr persists it in plaintext in the `run.created`
   * event, DB, and events stream. Do not put secret values (tokens, API keys,
   * credentials, or PII) in payload; use `secrets:` instead. Use payload for
   * business context such as PR numbers, repo names, ticket ids, and other
   * identifiers.
   */
  readonly payload: TPayload
  readonly workspace: WorkspaceSpec
} & ([keyof TSecrets] extends [never]
  ? { readonly secrets?: Record<never, never> }
  : { readonly secrets: TriggerSecrets<TSecrets> })

export const tasks = {
  trigger<TPayload, TOutput, TSecrets extends SecretDecls>(
    task: Task<TPayload, TOutput, TSecrets>,
    opts: TriggerOptions<TPayload, TSecrets>,
  ): Promise<RunHandle<TOutput>> {
    return getDefaultClient().tasks.trigger(task, opts)
  },
}
