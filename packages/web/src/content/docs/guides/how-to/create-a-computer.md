---
title: Create a computer
description: Create and address a durable Computer from a deployed Computer definition.
---

# Create a computer

Deploy a Computer definition declaration, then create a Computer from its declared ID:

```sh
COMPUTER_ID="$(helmr computer create repository-agent \
  --project agents --env development \
  --key repo:helmrdotdev/helmr \
  --idempotency-key computer:helmrdotdev/helmr)"
```

`--key` is an optional immutable lookup value. `--idempotency-key` is for safe
request retries; it is not the Computer key.

The TypeScript client can also create a Computer with stable Secret bindings:

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({
  url,
  apiKey: process.env.HELMR_API_KEY!,
})

const computer = await client.computerDefinitions.createComputer(
  "repository-agent",
  {
    key: "repo:helmrdotdev/helmr",
    idempotencyKey: "computer:helmrdotdev/helmr",
    secrets: [
      {
        secretId: process.env.HELMR_GITHUB_SECRET_ID!,
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

A Computer can outlive an individual Session. Reuse it when later Sessions should
see the same committed files; create a new one when state or secret placement
must be isolated.
