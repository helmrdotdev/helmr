---
title: Use secrets
description: Store a Secret and place it in a Workspace without exposing its value.
---

# Use secrets

Create a remote Secret by sending the value on standard input:

```sh
printf '%s' "$GITHUB_TOKEN" | helmr secret create GITHUB_TOKEN \
  --project agents --env development \
  --idempotency-key secret:github-token:create
```

The CLI and API return metadata, never the stored value. Bind the Secret when
creating a Workspace with the SDK:

```ts
import { HelmrClient, secrets } from "@helmr/sdk"

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
        secret: secrets.fromName("GITHUB_TOKEN"),
        env: "GITHUB_TOKEN",
      },
      {
        secret: secrets.fromName("SSH_KEY"),
        file: "/run/secrets/ssh-key",
      },
    ],
  },
)
```

Placements are fixed on the Workspace and are either an environment variable
or file path. Task starts accept the Workspace reference, not secret values or
a new binding map.

Rotate by Secret resource ID and revoke deliberately:

```sh
printf '%s' "$NEW_GITHUB_TOKEN" | helmr secret rotate SECRET_ID \
  --project agents --env development \
  --idempotency-key secret:github-token:rotate:2

helmr secret revoke SECRET_ID --yes \
  --project agents --env development \
  --idempotency-key secret:github-token:revoke
```

Never put credentials in payload, metadata, tags, source archives, logs, or
Actor output.
