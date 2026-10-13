---
title: Manage secrets
description: Create a Secret, bind it to Computers, rotate its value and revoke it.
---

# Manage secrets

This guide stores a GitHub token as a Secret, makes it available to an Agent's
Computer, and later rotates and revokes it. For delivery modes, allowed origins,
client requirements and what rotation and revocation guarantee, see
[Secrets](/docs/concepts/secrets).

The examples use the `demo` Project's `staging` Environment, with the CLI
configured (see [Quickstart](/docs/quickstart)). With an Environment API key,
omit `--project` and `--env`.

## Create the Secret

Send the value on standard input so that it does not appear in your shell
history or process list:

```sh
printf '%s' "$GITHUB_TOKEN" | helmr secret create GITHUB_TOKEN \
  --project demo --env staging \
  --idempotency-key secret:github-token:create --json
```

The response contains `secret: "GITHUB_TOKEN"` and metadata; it never contains
the value. Use `GITHUB_TOKEN` unchanged in bindings, retrieval, rotation and
revocation. The Console displays the same ID. List the Environment's Secrets
with `helmr secret list --project demo --env staging --json`.

A Secret belongs to one Environment. Create it separately in each Environment
that needs it, using the same ID and the value appropriate to that Environment.

## Bind it to a Computer

A binding names the Secret by ID and says how a Computer receives it: as a
protected environment variable, a raw environment variable, or a file. Bindings
are fixed when the Computer is created. Where they come from depends on how the
Computer is created:

| Computer created by | Bindings used |
| --- | --- |
| Starting an Agent without `--computer` | The definition's `secrets`. |
| `helmr computer create` or `client.computers.create` | Only the bindings passed in that request. The definition's `secrets` are not added. |

### In the Template definition

Use this for Computers that Agents create for their Sessions:

```ts
import { agent, template, image } from "@helmr/sdk"

export const reviewerComputer = template({
  id: "reviewer",
  image: image("reviewer")
    .from("debian:bookworm-slim")
    .run(["sh", "-ceu", "apt-get update && apt-get install -y gh"]),
  resources: { cpu: 1, memory: "2GiB" },
  secrets: [
    {
      secret: "GITHUB_TOKEN",
      env: { name: "GH_TOKEN", mode: "protected", allowedOrigins: ["https://api.github.com"] },
    },
  ],
})

export const reviewer = agent({
  id: "reviewer",
  template: reviewerComputer,
  async turn() {
    // Processes started here, such as `gh api user`, see GH_TOKEN.
    return { ok: true }
  },
})
```

Deploy the project. Registration resolves every `secrets` and `buildSecrets`
identifier in the target Environment and rejects missing or revoked Secrets.
The same compiled bundle can deploy to multiple Environments, each with its own
active `GITHUB_TOKEN` value. Computers created by Agent starts after the Deployment is
promoted get the binding. Existing Computers keep the bindings they were
created with.

### When creating a Computer

Write the bindings to a file and pass it to `helmr computer create`:

```json
[
  {
    "secret": "GITHUB_TOKEN",
    "env": { "name": "GH_TOKEN", "mode": "protected", "allowedOrigins": ["https://api.github.com"] }
  }
]
```

```sh
helmr computer create reviewer --project demo --env staging \
  --key github-review --secrets-file secrets.json \
  --idempotency-key computer:github-review
```

With the SDK:

```ts
const computer = await client.computers.create({
  template: "reviewer",
  key: "github-review",
  idempotencyKey: "computer:github-review",
  secrets: [
    {
      secret: "GITHUB_TOKEN",
      env: { name: "GH_TOKEN", mode: "protected", allowedOrigins: ["https://api.github.com"] },
    },
  ],
})
```

To run an Agent on this Computer, start it with `--computer COMPUTER_ID` (or
`computer: { id }` in the SDK).

### Choose the delivery mode

| Binding | Use when |
| --- | --- |
| `env: { name, mode: "protected", allowedOrigins }` | The tool sends the credential in an HTTP header to known HTTPS origins, as `gh` does with `GH_TOKEN`. The Computer only sees a placeholder. |
| `env: { name, mode: "raw" }` | The tool needs the real value, for example a database password or request signing. |
| `file: { path }` | The tool reads the credential from a file, such as an SSH key. |

Raw and file values can be read by any process in the Computer and can be kept
in its saved disk. To check a binding, run a command that uses it:

```sh
helmr computer exec --key github-review --project demo --env staging \
  --idempotency-key check:gh -- gh api user
```

To change a binding or its allowed origins, create a new Computer. Secrets used
only by `prepare` go in `buildSecrets`; see
[Prepare a Computer](/docs/guides/prepare-a-computer).

## Rotate the value

```sh
printf '%s' "$NEW_GITHUB_TOKEN" | helmr secret rotate GITHUB_TOKEN \
  --project demo --env staging \
  --idempotency-key secret:github-token:rotate:2
```

Protected bindings use the new value on their next request; you do not need to
recreate Computers. Raw and file values are delivered when work starts, so
processes that are already running keep the old value. A new Computer prepared
with this Secret in `buildSecrets` gets a freshly prepared image.

## Revoke the Secret

```sh
helmr secret revoke GITHUB_TOKEN --yes \
  --project demo --env staging \
  --idempotency-key secret:github-token:revoke
```

Revocation is permanent and reserves this identifier in the Environment. Create
a new Secret with a different ID if you need a replacement. It stops further use
of the Secret. It cannot erase raw or file copies
already delivered to a Computer; revoke the credential at its provider as well if
it may have been exposed.

Keep credentials out of Agent input, results, logs, image steps and source.
Helmr does not refresh OAuth tokens or save a tool's credential changes back to a
Secret; rotate the stored value when it changes.
