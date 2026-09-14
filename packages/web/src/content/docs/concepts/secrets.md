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
`NODE_OPTIONS` and `LD_*`, and CA environment variables are reserved.

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

Helmr routes traffic at the VM boundary. Programs use ordinary sockets without
proxy environment variables or route configuration. Same-guest localhost HTTP,
SSE, and WebSocket control traffic stays local. Only TCP ports named by the
Workspace's protected origins enter the credential transport; all other ports
retain ordinary network policy. On captured ports, unrelated TLS passes through
without termination, preserving the upstream certificate and negotiated protocol.

Protected HTTPS requires visible TLS SNI and trust in the Workspace's public CA.
Helmr provides a CA bundle through `SSL_CERT_FILE` and adds the CA for Node through
`NODE_EXTRA_CA_CERTS`. Other certificate stores require explicit public-CA trust
setup. Certificate pinning, mutual TLS, encrypted ClientHello (ECH), TLS without
SNI, and HTTP/3/QUIC cannot use protected header substitution. Opaque traffic can
remain permitted by network policy, but carries only the placeholder; Helmr
cannot diagnose a protected origin hidden inside encryption. Never disable TLS
verification to make a client work.

HTTP/1.1 and HTTP/2 request/response streaming are supported, including SSE and
streaming uploads. Every credential-bearing request or HTTP/2 stream checks live
Workspace authorization. Upstream TLS verifies the exact service hostname at the
policy-checked original IPv4 address. A shared address or matching port does not
grant Secret access; HTTP authority must match the selected HTTPS origin.

The placeholder must remain unchanged in an HTTP header. Basic authentication
encoding, local request signing, SSH authentication, and other transformations are
not supported. Helmr does not substitute values in request bodies, query strings,
or files. Protected CONNECT, WebSocket upgrades, and request trailers return an
unsupported-mode error; these limits do not apply to guest-local services or
unrelated end-to-end traffic. Non-HTTP ALPN on a protected origin fails TLS
negotiation. Clients with unsupported trust or TLS modes receive a TLS error; the
transport cannot distinguish every pinning failure from another client TLS error.

Helmr does not follow upstream redirects. A guest client following a redirect
sends its unchanged placeholder and the new request must satisfy network and
Secret policy again. The transport does not replay a request the upstream may
have processed. The maintained HTTP/2 transport may retry an explicitly
unprocessed request within the same authorized request context. Guest HTTP/2
streams are independent; upstream connections are not pooled across requests.

Parking, restoring, and runtime fencing close captured connections and cancel
in-flight streams. Clients must reconnect. No live connection or resolved Secret
value is restored from a guest snapshot.

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
