# SDK

User-facing SDK packages live here. `typescript/` contains `@helmr/sdk` for
declaration authoring, managed runtime operations, and the external
`HelmrClient`.

Runtime adapter internals, VM details, and host-specific code stay outside the
SDK surface.

## External client

Declaration-backed resources use a runtime declared ID and a type-only
definition generic:

```ts
import { HelmrClient } from "@helmr/sdk"
import type { issueTask } from "./definitions"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL!,
  apiKey: process.env.HELMR_API_KEY!,
})

const workspace = await client.sandboxes.createWorkspace(
  "issue-workspace",
  {
    key: "issue:123",
    idempotencyKey: "issue:123:workspace",
  },
)

const run = await client.tasks.start<typeof issueTask>(
  "issue-task",
  {
    payload: { issue: 123 },
    workspace,
    idempotencyKey: "issue:123:run",
  },
)

const result = await client.runs.wait<typeof issueTask>(run.id)
const logs = await client.runs.logs(run.id)
const events = await client.runs.events(run.id)
```

Task start requires an existing Workspace. The Task Run payload is plaintext
audit data. Place Secret values when creating the Workspace; never put them in
payload.

## Tokens

Tokens are independent durable external-completion resources. Task code can
create and wait on one; a trusted backend can also create or complete one
through `client.tokens`. Multiple Runs in the same Environment may wait on the
same Token.

```ts
const token = await tokens.create({ timeout: "1h" })
const decision = await token.wait({ schema: approvalSchema })

await client.tokens.complete(tokenId, {
  result: { approved: true },
  idempotencyKey: "approval:123",
})
```
