# SDK

`typescript/` contains `@helmr/sdk` for Agent and Computer definitions, injected
Turn operations, and the explicitly authenticated `HelmrClient`.

Runtime adapter internals, VM details, and host-specific code stay outside the
SDK surface.

## External client

Inspect deployed definitions and create an actual Computer when explicit
placement is needed. Starting an Agent without a Computer uses its default
Computer definition.

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL!,
  apiKey: process.env.HELMR_API_KEY!,
})

const definitions = await client.computerDefinitions.list()
const computer = await client.computerDefinitions.createComputer("issue-computer", {
  key: "issue:123",
  idempotencyKey: "issue:123:computer",
})
const { session, turn } = await client.agents.start("issue-agent", {
  input: { issue: 123 },
  computer,
  idempotencyKey: "issue:123:start",
})
const outcome = await turn.wait()
await session.enqueue({ followUp: "Check the tests" }, {
  idempotencyKey: "issue:123:follow-up",
})
```

Creation returns a Session and its initial Turn receipt. Retain their IDs for
reconnection; observe the exact Turn for its result. Computer snapshots expose
`definitionKey` and `deploymentId`. Bind Secrets by stable `secretId` with explicit
env or file placement; never place Secret values in business input.

The client uses its supplied credentials even inside Agent code. Injected Turn
operations have their own Session and Deployment binding.
