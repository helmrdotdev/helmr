---
title: Use secrets
description: Create a Secret, bind it to a Computer, and rotate its value.
---

# Use secrets

This guide uses a GitHub token with `gh api`. The `reviewer` Computer definition must be
included in the current Deployment and its image must contain `gh`. See
[Build a custom image](/docs/guides/how-to/build-a-custom-image) to install tools.
Use the same Project Environment for the Secret and Computer.

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

Set `HELMR_API_KEY` to an [environment API key](/docs/reference/rest-api/authentication)
for the same `agents` project and `development` environment. Bind the Secret by its returned
ID when creating the Computer (set `HELMR_GITHUB_SECRET_ID` to that ID):

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({
  url,
  apiKey: process.env.HELMR_API_KEY!,
})

const computer = await client.computerDefinitions.createComputer("reviewer", {
  key: "github-review",
  idempotencyKey: "computer:github-review",
  secrets: [
    {
      secretId: process.env.HELMR_GITHUB_SECRET_ID!,
      env: {
        name: "GH_TOKEN",
        mode: "protected",
        allowedOrigins: ["https://api.github.com"],
      },
    },
  ],
})

const command = await computer.exec({
  command: ["gh", "api", "user"],
  idempotencyKey: "github-user",
})
const outcome = await command.wait()
```

`gh` reads `GH_TOKEN` and sends it in the authorization header. Inside the
Computer, that variable contains a placeholder. Helmr replaces it with the
current Secret value only for the approved origin while execution is authorized.
The token must have permission for the GitHub API operation you request.

Clients must trust the Computer public CA and CA trust settings and send the
placeholder unchanged in the header. Do not encode it for Basic authentication
or use it to sign requests. See [client requirements](/docs/concepts/secrets#client-requirements)
for supported protocols and limitations.

Bindings are fixed at Computer creation. Later Agent starts use the
Computer reference rather than a new binding map. To change a binding or its
allowed origins, create a new Computer.

Use the CLI's [`--secrets-file`](/docs/reference/cli/computer#secret-bindings-at-creation)
option. JSON and the SDK both use `secretId` and `allowedOrigins`.

## Use raw values when required

If a tool needs the actual credential for a non-HTTP protocol, signing, or file
access, choose raw env or file delivery when creating its Computer:

```ts
secrets: [
  { secretId: databaseSecret.id, env: { name: "PGPASSWORD", mode: "raw" } },
  { secretId: clientKeySecret.id, file: { path: "/run/secrets/client.key" } },
]
```

Create those Secrets in the same Project Environment first and use their returned IDs. Raw values can
be read by Computer processes and may be retained in snapshots. Do not expose a
Secret through raw delivery if you need its value hidden from the Computer.

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
[rotation and revocation](/docs/concepts/secrets#rotation-and-revocation).

Keep credentials out of inputs, results, source archives, logs and authored
content. Helmr does not automatically refresh OAuth tokens or write CLI credential
changes back to a Secret.
