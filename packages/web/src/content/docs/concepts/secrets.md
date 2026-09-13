---
title: Secrets
description: Store credentials and choose how Workspaces use them.
---

# Secrets

A Secret is a named, encrypted value scoped to one Project Environment. Create it
once and reference its name when creating a Workspace. Secrets persist
independently of Workspaces, so you can reuse them in new Workspaces and rotate
values without changing their names. Read APIs return metadata, not the value.

A binding specifies how a Workspace uses a Secret. Bindings are fixed at
Workspace creation; later Runs use those bindings and cannot override them.

## Choose a delivery mode

| Placement | What the Workspace receives | Use for |
| --- | --- | --- |
| Protected env | A placeholder that Helmr replaces in outgoing HTTP headers for approved HTTPS origins. | API tokens used by proxy-compatible CLI tools and SDKs. |
| Raw env | The Secret value in an environment variable. | Clients that need the actual value, such as database drivers or request-signing libraries. |
| File | The Secret value as file contents. | SSH keys and other credentials that a tool reads from disk. |

Raw env and file values can be read by processes in the Workspace. Protected env
keeps the value outside the Workspace unless you also expose the same Secret
through a raw binding. Helmr never automatically falls back from protected to raw.

```ts
const workspace = await client.sandboxes.createWorkspace("reviewer", {
  secrets: [
    {
      secret: "github-token",
      env: {
        name: "GH_TOKEN",
        mode: "protected",
        allowedOrigins: ["https://api.github.com"],
      },
    },
    { secret: "database-password", env: { name: "PGPASSWORD", mode: "raw" } },
    { secret: "client-key", file: { path: "/run/secrets/client.key" } },
  ],
})
```

Secret names must match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`. Each binding has
exactly one `env` or `file` target. Environment bindings require an explicit
`mode`, and targets must be unique within the Workspace.

Do not define a bound environment variable in image ENV or execution env, even
with an empty value. Execution requests reject these collisions; image ENV
collisions fail at launch. Managed runtime paths, runtime variables such as
`NODE_OPTIONS` and `LD_*`, and proxy/CA environment variables are reserved.

## Protected environment variables

Use the environment variable directly in an HTTP header, such as
`Authorization: Bearer <value>`. Helmr's proxy checks the Workspace's execution
authorization and the destination before replacing the placeholder with the
current Secret value. Invalid or revoked placeholders in inspected headers fail
before the request is sent upstream.

### Allowed origins

`allowedOrigins` lists the exact HTTPS origins that may receive the Secret. For
example, `https://api.github.com` permits credential use on that origin, while
`https://api.example.com:8443` specifies a non-default port. Only approve services
you trust: they receive the real credential, and Helmr does not redact responses
that return it.

Origins must use DNS hostnames. Hostnames are normalized to lowercase, the default
port `443` is omitted, and a trailing `/` is accepted. Wildcards, IP literals,
user information, paths, queries, and fragments are not allowed.

A Workspace accepts up to 64 Secret bindings, 16 origins per protected binding,
and 256 origin entries in total. Duplicate origins within a binding count once;
the same origin in different bindings and different ports count separately.

The proxy connects only to public IPv4 addresses and also applies the Worker's
blocked destinations. Private-network and IPv6 destinations are not supported.

Allowed origins control where a Secret can be used, not all Workspace network
access. Requests without credentials can still reach permitted destinations.
HTTPS requests to other origins are tunneled without Secret substitution; a
placeholder sent there remains a placeholder. The proxy does not automatically
forward credentials to a new origin after a redirect.

### Client requirements

The client must honor Helmr's HTTP proxy and CA trust configuration. Helmr sets
proxy environment variables, provides a CA bundle, and configures Node's extra CA
and environment-proxy settings. Certificate-pinned clients and clients that
ignore these settings cannot use protected env.

The placeholder must remain unchanged in the header. Basic authentication
encoding, request signing, and other transformations are not supported. Helmr
does not substitute values in request bodies, query strings, or files.

Connections to protected origins use HTTP/1.1. HTTP/2-only clients, gRPC, and
WebSocket upgrades are not supported on those origins. The proxy does not retry
upstream requests automatically; handle retries according to the API's
idempotency requirements.

## Delegation

Code running in a Workspace can use only its existing Secret permissions when
creating another Workspace, executing in an existing Workspace, or starting a
child task. It cannot add a Secret, expand allowed origins, or change protected
access to raw access. If it already has raw access to a Secret, it can delegate
that Secret in either mode.

Users who can create or deploy bindings choose the mode and allowed origins.
Protected env protects against disclosure to Workspace processes; it does not
restrict authorized administrators from choosing raw delivery.

## Rotation and revocation

Protected requests use the current Secret value. Rotation affects the next
authorized request without recreating the Workspace. Revocation blocks subsequent
credential resolution, but requests already authorized or sent may finish.

Raw env and file values are selected when execution is admitted. Rotation does
not rewrite running processes or values restored from snapshots. Revocation
cannot erase values already delivered to a Workspace or saved in its snapshots;
remove those copies separately.

Helmr does not refresh OAuth tokens or save credential changes made by a CLI back
to a Secret. Manage token renewal separately and rotate the stored value when it
changes. Storing a credential does not manage a provider's subscription session.

## Workspace lifetime

Protected env uses a Workspace-specific CA that is reused after restore. Its
trust expires ten years after Workspace creation; after expiry, create a new
Workspace to continue using protected env. Certificate private keys are not
delivered to the Workspace. This technical limit does not change the recommended
lifecycle: retain a Workspace for the work that needs it, then delete it.

See [Use secrets](/docs/guides/how-to/use-secrets/) for creation, binding, rotation,
and revocation commands. Keep credentials out of payloads, metadata, tags, logs,
source archives, image literals, and Actor/Token results.
