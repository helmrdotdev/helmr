---
title: Secrets
description: Store credentials and choose how Computers use them.
---

# Secrets

A Secret is a named, encrypted value scoped to one Project Environment. Create it
once and use its name as the public `secret` identifier in code, the Console,
CLI, SDK and API. Secrets persist
independently of Computers, so you can reuse them in new Computers and rotate
values without changing their names. Read APIs return metadata, not the value.

A binding specifies how a Computer uses a Secret. Bindings are fixed at
Computer creation; later Sessions use those bindings and cannot override them.

## Choose a delivery mode

| Placement | What the Computer receives | Use for |
| --- | --- | --- |
| Protected env | A placeholder that Helmr replaces in outgoing HTTP headers for approved HTTPS origins. | API tokens used by proxy-compatible CLI tools and SDKs. |
| Raw env | The Secret value in an environment variable. | Clients that need the actual value, such as database drivers or request-signing libraries. |
| File | The Secret value as file contents. | SSH keys and other credentials that a tool reads from disk. |

Raw env and file values can be read by processes in the Computer. Protected env
keeps the value outside the Computer unless you also expose the same Secret
through a raw binding. Helmr never automatically falls back from protected to raw.

```ts
const computer = await client.computers.create({
  template: "reviewer",
  secrets: [
    {
      secret: "GITHUB_TOKEN",
      env: {
        name: "GH_TOKEN",
        mode: "protected",
        allowedOrigins: ["https://api.github.com"],
      },
    },
    { secret: "DATABASE_PASSWORD", env: { name: "PGPASSWORD", mode: "raw" } },
    { secret: "CLIENT_KEY", file: { path: "/run/secrets/client.key" } },
  ],
})
```

Secret identifiers are immutable and unique within an Environment. A Secret keeps
its identity for its whole life: rotation and revocation change its state, not
which Secret it is. Reuse the same identifier in different Environments for
Environment-specific values. New Deployments and Computers require every declared
Secret to be active in their target Environment. Successful retries keep their
original binding, and rotation changes the value without changing the identifier.

Secret identifiers must match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`. Each binding has
exactly one `env` or `file` target. Environment bindings require an explicit
`mode`, and targets must be unique within the Computer.

Do not define a bound environment variable in image ENV or execution env, even
with an empty value. Execution requests reject these collisions; image ENV
collisions fail at launch. Managed runtime paths, runtime variables such as
`NODE_OPTIONS` and `LD_*`, and CA environment variables are reserved.

## Protected environment variables

Use the environment variable directly in an HTTP header, such as
`Authorization: Bearer <value>`. Helmr's proxy checks the Computer's execution
authorization and the destination before replacing the placeholder with the
Secret's current value. Invalid placeholders in inspected headers fail before the
request is sent upstream. A request that needs a revoked Secret fails with
`502 Protected Secret is revoked; rotate it to resume`; requests that use other
Secrets on the same Computer are unaffected.

### Allowed origins

`allowedOrigins` lists the exact HTTPS origins that may receive the Secret. For
example, `https://api.github.com` permits credential use on that origin, while
`https://api.example.com:8443` specifies a non-default port. Only approve services
you trust: they receive the real credential, and Helmr does not redact responses
that return it.

Origins must use DNS hostnames. Hostnames are normalized to lowercase, the default
port `443` is omitted, and a trailing `/` is accepted. Wildcards, IP literals,
user information, paths, queries, and fragments are not allowed.

A Computer accepts up to 64 Secret bindings, 16 origins per protected binding,
and 256 origin entries in total. Duplicate origins within a binding count once;
the same origin in different bindings and different ports count separately.

Every connection also passes the Computer's phase network policy and mandatory
installation denies. Public IPv4 destinations remain subject to those checks.
Private HTTPS requires installation-approved business ranges, routing, DNS and
TLS trust; allowlist mode additionally requires explicit per-host `ipRanges`.
IPv6 is unsupported. See [Execution policies](/docs/concepts/execution-policies).

A binding grants no extra network access. Under full blocking, protected bindings
are rejected. Under allowlist mode, each exact origin must use port 443 and a
concrete hostname matching an allowed pattern; every later dial must satisfy one
matching entry's address bounds. Ordinary connectivity retains explicit HTTPS
origin ports. Runtime `secrets` and `prepareSecrets` are validated against their
own resolved phases; nonempty `prepareSecrets` requires a `prepare` callback.
Raw env and file values remain readable by guest code and never widen networking.

Allowed origins control where a Secret can be used, not all Computer network
access. Requests without credentials can still reach permitted destinations.
HTTPS requests to other origins are tunneled without Secret substitution; a
placeholder sent there remains a placeholder. The proxy does not automatically
forward credentials to a new origin after a redirect.

### Client requirements

Helmr routes traffic at the VM boundary. Programs use ordinary sockets without
proxy environment variables or route configuration. Same-guest localhost HTTP,
SSE, and WebSocket control traffic stays local. Only TCP ports named by the
Computer's protected origins enter the credential transport; all other ports
retain ordinary network policy. On captured ports, unrelated TLS passes through
without termination, preserving the upstream certificate and negotiated protocol.

Protected HTTPS requires visible TLS SNI and trust in the Computer's public CA.
Helmr provides a CA bundle through `SSL_CERT_FILE` and adds the CA for Node through
`NODE_EXTRA_CA_CERTS`. Other certificate stores require explicit public-CA trust
setup. Certificate pinning, mutual TLS, encrypted ClientHello (ECH), TLS without
SNI, and HTTP/3/QUIC cannot use protected header substitution. Opaque traffic can
remain permitted under ordinary connectivity, but carries only the placeholder; Helmr
cannot diagnose a protected origin hidden inside encryption. Never disable TLS
verification to make a client work.

HTTP/1.1 and HTTP/2 request/response streaming are supported, including SSE and
streaming uploads. Every credential-bearing request or HTTP/2 stream checks live
Computer authorization. Upstream TLS verifies the exact service hostname at the
policy-checked original IPv4 address. A shared address or matching port does not
grant Secret access; HTTP authority must match the selected HTTPS origin.

The placeholder must remain unchanged in an HTTP header. Basic authentication
encoding, local request signing, SSH authentication, and other transformations are
not supported. Helmr does not substitute values in request bodies, query strings,
or files. HTTP/1.1 WebSocket Upgrade over TLS is supported: authentication
headers are resolved during the handshake, then frames pass through unchanged.
Placeholders in `Sec-WebSocket-*` negotiation headers are rejected. HTTP/2
extended CONNECT, general CONNECT, and request trailers return an
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

An established WebSocket holds one captured connection and one active request
slot until both transfer directions have terminated. There is no proxy idle
timeout after Upgrade; endpoints control their application-level idle behavior.

Each runtime admits up to 256 simultaneous TCP connections on protected-origin
ports, including unrelated traffic sharing those ports, and up to 256 active
protected HTTP requests. Excess captured connections close immediately; excess
HTTP requests receive `503 Protected HTTPS concurrent request limit reached`.
Streaming requests hold request capacity until they finish or are cancelled.
Credential resolution runs up to 64 requests at a time; admitted requests waiting
for resolution can be cancelled. HTTP/2 allows four active streams per connection;
clients can queue or open another connection. Guest-local traffic and ports that
are not captured do not consume these limits.

Parking, restoring, and runtime fencing close captured connections and cancel
in-flight streams. Clients must reconnect. No live connection or resolved Secret
value is restored from a guest snapshot.

## Delegation

Code running in a Computer can use only its existing Secret permissions when
using runtime-bound Agent creation and explicit Computer placement. It cannot add a Secret, expand allowed origins, or change protected
access to raw access. If it already has raw access to a Secret, it can delegate
that Secret in either mode.

Users who can create or deploy bindings choose the mode and allowed origins.
Protected env protects against disclosure to Computer processes; it does not
restrict authorized administrators from choosing raw delivery.

## Rotation and revocation

Rotating a Secret sets its next version. The Secret keeps its identifier and
bindings, and every Computer bound to it uses the new value without being
recreated:

- Protected requests resolve the Secret's current value per request, so the next
  request uses the new value. Established WebSocket connections keep the value
  resolved at their handshake.
- Raw env and file values are selected when a Session process or Command starts.
  Rotation does not rewrite running processes or values restored from
  snapshots; the next process or Command receives the new value.

Revoking a Secret stops Helmr from providing it. Nothing else changes:

- Protected requests that need the Secret fail until it is rotated. Running
  Sessions and Commands keep running.
- New Session processes and Commands that need a raw value of the Secret are
  refused with `secret_unavailable`. Running ones continue, and the values
  already delivered stay where they are, including in snapshots and images.
- New Computers and Deployments cannot bind the Secret.

To resume, rotate the revoked Secret with its new value. Rotation reactivates
the same Secret; there is no need to create it again, and creating a Secret with
a revoked name is refused with `secret_revoked`.

Revocation cannot recall a value that was already delivered or a request already
sent upstream. Revoke the credential at its provider; that is the containment.
Then find the Computers that received a raw value with
`helmr computer list --secret NAME`, or from the Secret in the Console, and
delete the ones you no longer trust. Computers marked as holding the value
through their image received it during image preparation.

Helmr does not refresh OAuth tokens or save credential changes made by a CLI back
to a Secret. Manage token renewal separately and rotate the stored value when it
changes. Storing a credential does not manage a provider's subscription session.

## Computer lifetime

Protected env uses a Computer-specific CA that is reused after restore. Its
trust expires ten years after Computer creation; after expiry, create a new
Computer to continue using protected env. Certificate private keys are not
delivered to the Computer. This technical limit does not change the recommended
lifecycle: retain a Computer for the work that needs it, then delete it.

See [Use secrets](/docs/guides/manage-secrets) for creation, binding, rotation,
and revocation commands. Keep credentials out of inputs, results, logs,
source archives, image literals and human content.
