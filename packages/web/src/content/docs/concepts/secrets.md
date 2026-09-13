---
title: Secrets
description: Durable Project-Environment values with Workspace raw or protected bindings.
---

# Secrets

A Secret is a durable name/value scoped to one Project Environment. Create and
rotate write its encrypted value; read APIs expose metadata. A Secret does not
own its delivery mode. Workspace bindings select its stable identity and are
fixed when the Workspace is created, never overridden per Run.

Names are plain SDK strings matching `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`:

```ts
const workspace = await client.sandboxes.createWorkspace("reviewer", {
  secrets: [
    { secret: "github-token", env: {
      name: "GH_TOKEN", mode: "protected",
      allowedOrigins: ["https://api.github.com"],
    } },
    { secret: "database-password", env: { name: "PGPASSWORD", mode: "raw" } },
    { secret: "client-key", file: { path: "/run/secrets/client.key" } },
  ],
})
```

Each binding has exactly one `env` or `file`. Env requires an explicit mode;
files deliver raw bytes. Targets must be unique. Ordinary image and execution
environment values remain literals and cannot collide with Secret targets.
Image ENV and exec env must not define a bound name, even with an empty value; exec requests are rejected during validation, and image ENV collisions fail at launch. Runtime paths, managed runtime names (including NODE_OPTIONS and LD_*), and transport environment variables are reserved.

## Protected env

The guest receives an inert placeholder. A trusted worker HTTP(S) proxy replaces
literal placeholders in HTTP header values only when the request targets an
approved exact HTTPS origin and the Workspace has live mounted execution
authority. Real values are resolved in the control plane on every request and
are not supplied through protected guest delivery, guest snapshots, or Helmr logs.
There is no raw fallback. In proxy-inspected HTTP and protected-origin HTTPS
requests, an unknown, forged, revoked, or unauthorized header placeholder causes
failure before the request is sent upstream. Other HTTPS origins use opaque
CONNECT tunnels: a placeholder may be sent unchanged, but no Secret is resolved
or substituted on those tunnels.

At most 64 bindings and 16 origins per protected binding are accepted, with at most 256 total origin entries after deduplication within each binding. Repeated origins across bindings and distinct ports count separately toward that total.

Origins are canonical DNS HTTPS origins: lowercase hostname, default port 443
folded away, non-default ports explicit. A trailing `/` is accepted. Wildcards,
userinfo, paths, queries, fragments, and IP literals are rejected. Runtime DNS
resolution requires public IPv4 addresses and preserves the worker's effective
blocked destinations; the checked numeric address is pinned for the connection
and TLS verifies the hostname. IPv6 upstream transport is not supported.

This requires clients that support the configured HTTP proxy and public CA
trust. Helmr configures proxy variables, a combined system CA bundle, and Node's
extra CA/env-proxy settings. Synthetic local HTTPS header replacement was tested with Linux `gh api` 2.97.0 and Node 24.21.0 `fetch` using literal bearer headers. This does not establish other client versions, credential flows, or subscription lifecycles. Certificate-pinned clients, clients ignoring
proxy/CA settings, and clients transforming the placeholder (for example Basic
encoding or request signing) are outside this support contract. Body and query
substitution, protected files, and file templates are not supported.

Ordinary permitted public HTTP(S) continues through the proxy. Protected origins
can receive anonymous requests, such as discovery calls. Helmr does not require
auth on every path, transparently intercept every client, or block anonymous
traffic on every network route. Redirects are handled as separate client
requests: auth is never automatically replayed by the proxy to a new origin.
Protected-origin transport is HTTP/1.1 only; HTTP/2-only/gRPC clients and
WebSocket upgrades are not supported there. Upstream connections are not reused
or automatically retried, including after an uncertain send.

## Trust and delegation

Approved servers receive the credential and may reflect it or exchange it for
another credential. Only approve upstreams you trust. Responses are not redacted.
External binding authors and host administrators are trusted to choose raw
bindings and origins under their existing scoped create/deploy permissions.
This feature isolates guest workloads from protected key delivery; it does not
protect against malicious binding authors or administrators.

A guest can delegate only its source Workspace's Secret authority when creating
another Workspace, executing in an existing one, or starting child tasks,
including retries. Protected-only authority cannot become raw, acquire another
Secret, or widen origins. Raw authority permits either mode for that Secret.
Mixing raw and protected bindings is allowed at distinct targets, but protection
is per binding and cannot hide independently raw-delivered copies.

## Rotation, revocation, and retention

Protected requests use the current value. Rotation affects the next authorized
request without recreating the Workspace. Revocation prevents subsequent
credential resolution; a request already authorized or sent may finish.

Raw env/file values resolve at execution admission. Rotation does not rewrite
running processes or restored raw bytes. Revocation cannot erase copies already
delivered to a guest or captured in its memory; raw copies need separate disposal.
Raw delivery is not a promise that snapshots contain no secret bytes.

Each protected Workspace has a private encrypted signer held by the control
plane and public CA trust lasting ten years from Workspace creation. Worker-only
leaf certificates last at most 24 hours and can be reissued under that root on
restore. No private certificate keys enter the guest. Expired root trust requires
creating a new Workspace; credential authorization also checks expiry on open
connections. This does not change Workspace retention: use Workspaces for bounded
work and delete them when done.

OAuth refresh, subscription credential lifecycle, mutable auth-cache writeback,
and provider adapters are not implemented. Payloads, logs, image literals, source
archives, and Actor/Token results are not secret channels.
