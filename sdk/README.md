# SDK

User-facing SDK packages live here.

SDK code defines the APIs developers import in task projects and external TypeScript processes. It should describe and validate user intent, then hand off execution details to `runtime/` and the Go control/worker implementation.

Current contents:
- `typescript/`: `@helmr/sdk` package for task authoring and compile-time task metadata.

Keep this layer stable and ergonomic. Runtime adapter internals, VM details, and host-specific code should stay out of the SDK surface.

## Payload And Secrets

Payload is audit data: Helmr persists it in plaintext in the `run.created` event, DB, and events stream. Do not put secret values (tokens, API keys, credentials, or PII) in payload; use `secrets:` instead. Use payload for business context such as PR numbers, repo names, ticket ids, and other identifiers.

Task code declares required secret names and runtime placements. Store each value
with `helmr secret set NAME`; run creation does not accept secret values or
binding maps.

## TypeScript Runtime Client

External TypeScript processes create runs with `task.trigger()` or the id-based `client.tasks.trigger()`. Triggering returns a lightweight `RunHandle`; retrieve or wait on that handle to get a `RunSnapshot`.

```ts
import { HelmrClient } from "@helmr/sdk"
import { impl } from "./tasks/impl"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL,
  apiKey: process.env.HELMR_API_KEY,
})

const handle = await client.tasks.trigger<typeof impl>(
  "impl",
  { issue: 123 },
)

const current = await client.runs.retrieve(handle)
const pendingWaitpoint = current.pendingWaitpoint
if (pendingWaitpoint !== null && pendingWaitpoint.kind === "human") {
  await client.waitpoints.respond(pendingWaitpoint, {
    value: { approved: true },
  })
}

const finished = await client.runs.wait(handle, { timeoutMs: 10 * 60_000 })
const logs = await client.runs.logs.retrieve(handle)
const events = await client.runs.events.list(handle)

for await (const event of await client.runs.events.subscribe(handle)) {
  console.log(event)
}

console.log(finished.status, logs.stdout, events.length)
```

Waitpoint responses live on `client.waitpoints`. Pass either a pending waitpoint from a run snapshot or a waitpoint id:

```ts
await client.waitpoints.respond("waitpoint-456", {
  value: { approved: true },
})
await client.waitpoints.respond("waitpoint-456", {
  value: { text: "Use the smaller rollout." },
})
```

## API Reference

### `POST /api/runs`

`payload` is a JSON field for tasks that accept payload. Payload is audit data: Helmr persists it in plaintext in the `run.created` event, DB, and events stream. Do not put secret values (tokens, API keys, credentials, or PII) in payload; declare task secrets instead. Use payload for business context such as PR numbers, repo names, ticket ids, and other identifiers.

Runs do not accept a `secrets` field. Secret names and placements are part of the deployed task definition.
