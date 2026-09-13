---
title: Create a workspace
description: Create and address a durable Workspace from a deployed Sandbox.
---

# Create a workspace

Deploy a Sandbox declaration, then create a Workspace from its declared ID:

```sh
WORKSPACE_ID="$(helmr workspace create repository-agent \
  --project agents --env development \
  --key repo:helmrdotdev/helmr \
  --idempotency-key workspace:helmrdotdev/helmr)"
```

`--key` is an optional immutable lookup value. `--idempotency-key` is for safe
request retries; it is not the Workspace key.

The TypeScript client can also create a Workspace with named Secret bindings:

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  apiKey: process.env.HELMR_API_KEY!,
})

const workspace = await client.sandboxes.createWorkspace(
  "repository-agent",
  {
    key: "repo:helmrdotdev/helmr",
    idempotencyKey: "workspace:helmrdotdev/helmr",
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
helmr workspace get --project agents --env development --id "$WORKSPACE_ID"
helmr workspace get --project agents --env development --key repo:helmrdotdev/helmr
```

A Workspace can outlive any individual Run. Reuse it when later Runs should
see the same committed files; create a new one when state or secret placement
must be isolated.
