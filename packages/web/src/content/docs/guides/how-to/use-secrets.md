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
      {
        secret: "SSH_KEY",
        file: { path: "/run/secrets/ssh-key" },
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

## Use a protected bearer header

Install `gh` in the declared Workspace image, then bind the existing
Project-Environment Secret at creation:

```ts
const workspace = await client.sandboxes.createWorkspace("reviewer", {
  secrets: [{ secret: "github-token", env: {
    name: "GH_TOKEN",
    mode: "protected",
    allowedOrigins: ["https://api.github.com"],
  } }],
})
const result = await workspace.exec({
  command: ["gh", "api", "user"],
  idempotencyKey: "github-user",
})
```

Synthetic local requests were tested with Linux `gh api` 2.97.0 sending the placeholder in its auth header. The trusted proxy supplies
the current credential only for the approved origin while this Workspace has
live execution authority. The same synthetic header substitution was tested with Node 24.21.0 `fetch` and a
literal `Authorization: Bearer ${process.env.GH_TOKEN}` header through the
configured proxy. Never transform or sign the placeholder. Other clients must
honor the proxy and public CA configuration.

Use the Console Workspaces → Create Workspace dialog or the CLI `--secrets-file` with the
same binding schema (`allowed_origins` in JSON). See [Secrets](/docs/concepts/secrets/)
for exact origin rules, upstream trust, raw-copy revocation limits, and unsupported
OAuth refresh/auth-cache lifecycle behavior.
