---
title: Secrets
description: Encrypted environment-scoped values placed into Workspaces.
---

# Secrets

A Secret is a named, encrypted value scoped to one Project Environment. Its
value is write-only through create or rotation; list, retrieve, and Workspace
responses expose metadata and placement names, not plaintext.

Secret names must match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`. SDK declarations
and Workspace requests use a typed address:

```ts
const token = secrets.fromName("GITHUB_TOKEN")
```

Workspace creation places that Secret in exactly one environment variable or
file path:

```ts
const workspace = await client.sandboxes.createWorkspace(
  "reviewer",
  {
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

The placement belongs to the Workspace. A later Task or Actor start supplies
the Workspace and does not accept literal Secret values or a replacement
binding map. Scheduled Task declarations similarly select Secret addresses for
each newly created fire Workspace.

Rotation creates a new stored version under the same stable Secret identity.
Revocation blocks the Secret rather than revealing or deleting its encrypted
history. Treat revoke as an operational security action and use idempotency
keys for create, rotate, and revoke retries.

Payload, metadata, tags, logs, Actor input and output, Token results, image
environment literals, and source archives are not secret channels.
