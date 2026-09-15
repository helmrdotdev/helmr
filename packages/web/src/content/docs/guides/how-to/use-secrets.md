---
title: Use secrets
description: Create a Secret, bind it to a Workspace, and rotate its value.
---

# Use secrets

This guide uses a GitHub token with `gh api`. The `reviewer` Sandbox must be
included in the current Deployment and its image must contain `gh`. See
[Build a custom image](/docs/guides/how-to/build-a-custom-image/) to install tools.
Use the same Project Environment for the Secret and Workspace.

## Create the Secret

Using the CLI's saved login, send the value on standard input from your local
environment. If you authenticate the CLI with an environment API key instead,
omit `--project` and `--env`; the key supplies that scope.

```sh
printf '%s' "$GITHUB_TOKEN" | helmr secret create GITHUB_TOKEN \
  --project agents --env development \
  --idempotency-key secret:github-token:create
```

The CLI and API return metadata, never the stored value. Save the returned Secret
ID for rotation or revocation.

## Bind it as a protected environment variable

Set `HELMR_API_KEY` to an [environment API key](/docs/reference/rest-api/authentication/)
for the same `agents` project and `development` environment. Bind the Secret by
name when creating the Workspace:

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  apiKey: process.env.HELMR_API_KEY!,
})

const workspace = await client.sandboxes.createWorkspace("reviewer", {
  key: "github-review",
  idempotencyKey: "workspace:github-review",
  secrets: [
    {
      secret: "GITHUB_TOKEN",
      env: {
        name: "GH_TOKEN",
        mode: "protected",
        allowedOrigins: ["https://api.github.com"],
      },
    },
  ],
})

const result = await workspace.exec({
  command: ["gh", "api", "user"],
  idempotencyKey: "github-user",
})
```

`gh` reads `GH_TOKEN` and sends it in the authorization header. Inside the
Workspace, that variable contains a placeholder. Helmr replaces it with the
current Secret value only for the approved origin while execution is authorized.
The token must have permission for the GitHub API operation you request.

Clients must trust the Workspace public CA and CA trust settings and send the
placeholder unchanged in the header. Do not encode it for Basic authentication
or use it to sign requests. See [client requirements](/docs/concepts/secrets/#client-requirements)
for supported protocols and limitations.

Bindings are fixed at Workspace creation. Later Task and Actor starts use the
Workspace reference rather than a new binding map. To change a binding or its
allowed origins, create a new Workspace.

You can also attach Secrets in Console under **Workspaces → Create Workspace**, or
use the CLI's [`--secrets-file`](/docs/reference/cli/workspace/#secret-bindings-at-creation)
option. JSON uses `allowed_origins` where the SDK uses `allowedOrigins`.

## Use raw values when required

If a tool needs the actual credential for a non-HTTP protocol, signing, or file
access, choose raw env or file delivery when creating its Workspace:

```ts
secrets: [
  { secret: "database-password", env: { name: "PGPASSWORD", mode: "raw" } },
  { secret: "client-key", file: { path: "/run/secrets/client.key" } },
]
```

Create those named Secrets in the same Project Environment first. Raw values can
be read by Workspace processes and may be retained in snapshots. Do not expose a
Secret through raw delivery if you need its value hidden from the Workspace.

## Rotate or revoke the Secret

Use the Secret ID returned by the create command:

```sh
printf '%s' "$NEW_GITHUB_TOKEN" | helmr secret rotate SECRET_ID \
  --project agents --env development \
  --idempotency-key secret:github-token:rotate:2

helmr secret revoke SECRET_ID --yes \
  --project agents --env development \
  --idempotency-key secret:github-token:revoke
```

Rotation updates the value used by subsequent protected requests. Revocation
blocks subsequent credential resolution; it does not cancel requests already
authorized or erase raw copies previously delivered. See
[rotation and revocation](/docs/concepts/secrets/#rotation-and-revocation).

Keep credentials out of payloads, metadata, tags, source archives, logs, and Actor
output. Helmr does not automatically refresh OAuth tokens or write CLI credential
changes back to a Secret.
