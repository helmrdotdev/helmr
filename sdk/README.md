# SDK

User-facing SDK packages live here.

SDK code defines the APIs developers import in task projects and external TypeScript processes. It should describe and validate user intent, then hand off execution details to `runtime/` and the Go control/worker implementation.

Current contents:
- `typescript/`: `@helmr/sdk` package for task authoring and compile-time task metadata.

Keep this layer stable and ergonomic. Runtime adapter internals, VM details, and host-specific code should stay out of the SDK surface.

## Payload And Secrets

Payload is audit data: Helmr persists it in plaintext in the `run.created` event, DB, and events stream. Do not put secret values (tokens, API keys, credentials, or PII) in payload; use `secrets:` instead. Use payload for business context such as PR numbers, repo names, ticket ids, and other identifiers.

Remote API/CLI run secret bindings are vault references, not host-local sources. Use
`vault:SECRET_NAME` in `POST /api/runs` `secrets` and `helmr run --secret NAME=vault:SECRET_NAME`.
Schemes such as `env:` and `file:` are local runner sources and are rejected for remote runs.

## TypeScript Runtime Client

External TypeScript processes create runs with `task.trigger()` or the id-based `client.tasks.trigger()`. Triggering returns a lightweight `RunHandle`; retrieve or wait on that handle to get a `RunSnapshot`.

```ts
import { HelmrClient, workspace } from "@helmr/sdk"
import { impl } from "./tasks/impl"

const client = new HelmrClient({
  url: process.env.HELMR_URL,
  apiKey: process.env.HELMR_API_KEY,
})

const handle = await client.tasks.trigger<typeof impl>(
  "impl",
  { issue: 123 },
  {
    workspace: workspace.github("OWNER/REPO", { ref: "main", subpath: "tasks" }),
  },
)

const current = await client.runs.retrieve(handle)
const pendingWaitpoint = current.pendingWaitpoint
if (pendingWaitpoint !== null && pendingWaitpoint.kind === "token") {
  await client.waitpoints.complete(pendingWaitpoint, {
    value: { approved: true, reviewedBy: "alice" },
  })
}

const finished = await client.runs.wait(handle, { timeoutMs: 10 * 60_000, intervalMs: 1_000 })
const logs = await client.runs.logs.retrieve(handle)
const events = await client.runs.events.list(handle)

for await (const event of await client.runs.events.subscribe(handle)) {
  console.log(event)
}

console.log(finished.status, logs.stdout, events.length)
```

Waitpoint completion lives on `client.waitpoints`. Pass either a pending waitpoint from a run snapshot or a run id plus waitpoint id:

```ts
await client.waitpoints.complete("run-123", "waitpoint-456", {
  value: { approved: true, reviewedBy: "alice" },
})
await client.waitpoints.complete("run-123", "waitpoint-456", {
  value: { text: "Use the smaller rollout." },
})
```

## API Reference

### `POST /api/runs`

`payload` is a JSON field for tasks that accept payload. Payload is audit data: Helmr persists it in plaintext in the `run.created` event, DB, and events stream. Do not put secret values (tokens, API keys, credentials, or PII) in payload; use `secrets:` instead. Use payload for business context such as PR numbers, repo names, ticket ids, and other identifiers.

`secrets` maps declared task secret names to vault URIs:

```json
{ "OPENAI_API_KEY": "vault:openai-api-key" }
```
