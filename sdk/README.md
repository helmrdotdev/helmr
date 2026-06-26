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

External TypeScript processes start tasks with `client.sessions.start()` and `client.sessions.startAndWait()`. The top-level `sessions.start(...)`, `sessions.startAndWait(...)`, and `workspaces.*` facades use the default client configured by `HELMR_API_URL` and `HELMR_API_KEY`. Starting a task creates or reuses a session and returns both the session snapshot and the current run handle. `task(...)` returns a definition object only; pass an imported task definition to the sessions namespace for local payload validation and type inference, or pass a string task id for external boundaries and dynamic task ids.

```ts
import { HelmrClient } from "@helmr/sdk"
import { impl } from "./tasks/impl"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL,
  apiKey: process.env.HELMR_API_KEY,
})

const started = await client.sessions.start(
  impl,
  { issue: 123 },
  {},
)

const completed = await client.sessions.startAndWait(
  impl,
  { issue: 124 },
  { timeoutSeconds: 10 * 60 },
)
const current = await client.runs.retrieve(started.run)
const logs = await client.runs.logs.retrieve(started.run)
const events = await client.runs.events.list(started.run)

for await (const event of await client.runs.events.subscribe(started.run)) {
  console.log(event)
}

console.log(completed.session.status, completed.run.status, current.status, logs.stdout, events.length)
```

Tokens are the external completion primitive. Create a token in task code with `tokens.create()`, wait with the returned handle, and complete it from trusted server-side code or a userland bridge:

```ts
// Task code.
import { tokens } from "@helmr/sdk"

const token = await tokens.create({
  timeout: "1h",
  metadata: { recipient: "reviewer@example.com" },
})

await token.wait({
  schema: approvalSchema,
})
```

```ts
// Trusted server-side bridge code.
await client.tokens.complete(token.id, {
  approved: true,
  reviewer: "slack:U123",
})
```

## API Reference

### `POST /api/sessions`

`payload` is a JSON field for tasks that accept payload. Payload is audit data: Helmr persists it in plaintext in the `run.created` event, DB, and events stream. Do not put secret values (tokens, API keys, credentials, or PII) in payload; declare task secrets instead. Use payload for business context such as PR numbers, repo names, ticket ids, and other identifiers.

Runs do not accept a `secrets` field. Secret names and placements are part of the deployed task definition.
