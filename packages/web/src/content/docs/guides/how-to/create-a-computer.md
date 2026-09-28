---
title: Create a computer
description: Create and address a durable Computer from a deployed Sandbox.
---

# Create a computer

Deploy a Sandbox declaration, then create a Computer from its declared ID:

```sh
COMPUTER_ID="$(helmr computer create repository-agent \
  --project agents --env development \
  --key repo:helmrdotdev/helmr \
  --idempotency-key computer:helmrdotdev/helmr)"
```

`--key` is an optional immutable lookup value. `--idempotency-key` is for safe
request retries; it is not the Computer key.

The TypeScript client can also create a Computer with named Secret bindings:

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  apiKey: process.env.HELMR_API_KEY!,
})

const computer = await client.sandboxes.createComputer(
  "repository-agent",
  {
    key: "repo:helmrdotdev/helmr",
    idempotencyKey: "computer:helmrdotdev/helmr",
    secrets: [
      {
        secret: "GITHUB_TOKEN",
        env: { name: "GITHUB_TOKEN", mode: "raw" },
      },
    ],
  },
)
```

Retrieve by UUID or exact key:

```sh
helmr computer get --project agents --env development --id "$COMPUTER_ID"
helmr computer get --project agents --env development --key repo:helmrdotdev/helmr
```

A Computer can outlive any individual Run. Reuse it when later Runs should
see the same committed files; create a new one when state or secret placement
must be isolated.
