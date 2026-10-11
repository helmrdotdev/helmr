---
title: HelmrClient
description: Options, properties and errors of the explicitly authenticated TypeScript client.
sidebarLabel: Client
---

# HelmrClient

`HelmrClient` calls the Developer API with an API key. It always acts with its
own credentials, including when it is constructed inside Agent code; it never
inherits a Session's runtime authority.

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({ url, apiKey: process.env["HELMR_API_KEY"]! })
```

## Options

```ts
interface HelmrClientOptions {
  readonly url?: string
  readonly apiKey: string
  readonly fetch?: typeof fetch
}
```

| Option | Description |
| --- | --- |
| `apiKey` | Required. An Environment-scoped API key, sent as `Authorization: Bearer <key>`. Surrounding whitespace is trimmed; an empty key throws. |
| `url` | Control plane origin. Must use HTTPS, except `http://localhost`, `http://127.0.0.1` and `http://[::1]`. Must not contain credentials, a path, query or fragment. Defaults to `https://api.helmr.dev`, the planned managed service; set it explicitly for a self-hosted control plane. |
| `fetch` | Custom `fetch` implementation. Defaults to `globalThis.fetch`. |

The SDK does not read `HELMR_API_URL` or `HELMR_API_KEY` itself. API keys and
their permissions are described in [Authentication](/docs/reference/api/authentication).

## Properties

| Property | Methods | Reference |
| --- | --- | --- |
| `agents` | `start` | [Agent](/docs/reference/sdk/agent#client-start-an-agent) |
| `agentDefinitions` | `get`, `list` | [Agent](/docs/reference/sdk/agent#client-agent-definitions) |
| `sessions` | `get`, `list`, `ref` | [Agent](/docs/reference/sdk/agent#client-sessions) |
| `computerDefinitions` | `get`, `list` | [Computer](/docs/reference/sdk/computer#client-computer-definitions) |
| `computers` | `create`, `get`, `list`, `ref` | [Computer](/docs/reference/sdk/computer#client-computers) |
| `commands` | `ref` | [Computer](/docs/reference/sdk/computer#client-commands) |
| `deployments` | `list`, `current`, `get` | [Deployments](#deployments) |
| `schedules` | `get`, `list` | [Triggers](/docs/reference/sdk/triggers#client-schedules) |
| `secrets` | `create`, `get`, `list`, `ref` | [Secrets](#secrets) |

Every method accepts an optional final `RequestOptions` argument,
`{ signal?: AbortSignal }`. Turn and Command waits take `{ timeout?, signal? }`
instead. List methods return `{ items, nextCursor? }`; definition pages also
include `deploymentId`. Treat cursors as opaque.

## Errors

A non-2xx response rejects with an `Error` whose `name` is `"HelmrError"`:

```ts
interface APIError extends Error {
  readonly code: string
  readonly details?: Readonly<Record<string, JsonValue>>
  readonly requestId?: string
}
```

`code` comes from the response's error body, or from the HTTP status when the
body has none. `requestId` is the `X-Request-ID` response header. Codes are
listed in [Errors](/docs/reference/api/errors).

## Deployments

```ts
client.deployments.list(query?: { cursor?: string; limit?: number }, options?): Promise<CursorPage<DeploymentListItem>>
client.deployments.current(options?): Promise<Deployment | null>
client.deployments.get(deploymentId: string, options?): Promise<Deployment>
```

`Deployment` and `DeploymentListItem` are `{ id, version, bundleDigest, createdAt }`.
`current()` resolves to `null` when the Environment has no current Deployment.
`limit` is 1–100. See [Projects](/docs/concepts/projects).

## Secrets

```ts
client.secrets.create(request: { name: string; value: string; idempotencyKey?: string }, options?): Promise<Secret>
client.secrets.get(id: string, options?): Promise<Secret>
client.secrets.list(query?: SecretListQuery, options?): Promise<CursorPage<Secret>>
client.secrets.ref(id: string): SecretRef

interface SecretRef {
  readonly id: string
  rotate(request: { value: string; idempotencyKey: string }, options?): Promise<Secret>
  revoke(request: { idempotencyKey: string }, options?): Promise<Secret>
}
```

`SecretListQuery` is either `{ cursor?, limit? }` (`limit` 1–100) or an exact
lookup `{ name }`, which does not accept `cursor` or `limit`.

`Secret` is `{ id, name, status, createdAt, rotatedAt?, revokedAt? }`, where
`status` is `"active"` or `"revoked"`. Reads never return the value. Bind
Secrets to Computers by `id`; see [Computer](/docs/reference/sdk/computer#secret-bindings)
and [Secrets](/docs/concepts/secrets).
