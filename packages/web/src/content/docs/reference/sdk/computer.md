---
title: Computer
description: template(), image(), Secret bindings, and client methods for Computers, Template definitions and Commands.
sidebarLabel: Computer
---

# Computer

This page lists the Computer authoring API and the `HelmrClient` methods for
Computers and Commands. For what a Computer is and how it persists, see
[Computers](/docs/concepts/computers). For step-by-step instructions, see
[Prepare a Computer](/docs/guides/prepare-a-computer) and
[Run commands](/docs/guides/run-commands).

## `template()`

```ts
function template(config: Omit<TemplateDefinition, "kind">): TemplateDefinition

interface TemplateDefinition {
  readonly kind: "template"
  readonly id: string
  readonly image: ImageBuilder
  readonly resources: { readonly cpu: number; readonly memory: string; readonly disk?: string }
  readonly prepare?: (build: BuildContext) => MaybePromise<void>
  readonly refresh?: { readonly every: Duration; readonly maxAge?: Duration }
  readonly deleteAfterUnused?: Duration
  readonly secrets?: readonly SecretBinding[]
  readonly buildSecrets?: readonly SecretBinding[]
}
```

Returns a frozen definition with `kind: "template"`. Throws `TypeError` when
`id` is blank or `resources.cpu`/`resources.memory` is missing.

| Field | Required | Description |
| --- | --- | --- |
| `id` | yes | Definition ID, matching `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. |
| `image` | yes | Root filesystem, built with [`image()`](#images). |
| `resources.cpu` | yes | Positive number of CPUs; must resolve to whole milliCPU (for example `0.5`, `1`, `2`). |
| `resources.memory` | yes | Positive integer with `MiB` or `GiB`, for example `"4GiB"`. |
| `resources.disk` | no | Must be `"32GiB"` in this release; omit it to use that size. |
| `prepare` | no | Preparation function. Runs with a `BuildContext` to set up the Computer's filesystem (for example cloning a repository and installing dependencies) before the Computer is used. |
| `refresh.every` | yes, with `refresh` | Intended interval between re-preparations. Recorded, but not yet acted on in this release. |
| `refresh.maxAge` | no | Maximum age of a prepared baseline a new Computer may start from; an older one is prepared again first. |
| `deleteAfterUnused` | no | Retention after the Computer becomes unused: a `Duration` or `"never"`. Default `"7d"`. |
| `secrets` | no | Secret bindings available to processes on the Computer. |
| `buildSecrets` | no | Secret bindings available to `prepare`. |

```ts
interface BuildContext {
  readonly signal: AbortSignal
  exec(command: string | readonly string[], options?: { cwd?: string; env?: Record<string, string> }): Promise<void>
}
```

```ts
import { template, image } from "@helmr/sdk"

export const repoComputer = template({
  id: "app",
  image: image("app").from("node:24-bookworm"),
  resources: { cpu: 2, memory: "4GiB" },
  buildSecrets: [{ secretId: "SOURCE_TOKEN_SECRET_ID", env: { name: "SOURCE_TOKEN", mode: "raw" } }],
  secrets: [{
    secretId: "MODEL_API_KEY_SECRET_ID",
    env: { name: "MODEL_API_KEY", mode: "protected", allowedOrigins: ["https://api.example.com"] },
  }],
  async prepare(build) {
    await build.exec(["git", "clone", "https://github.com/acme/app", "/app"])
    await build.exec(["npm", "ci"], { cwd: "/app" })
  },
  refresh: { every: "1d", maxAge: "7d" },
})
```

## Images

```ts
function image(key: string): ImageBuilder

interface ImageBuilder {
  readonly key: string
  from(ref: string): ImageBuilder
  run(argv: readonly string[]): ImageBuilder
  copy(source: SourceFile | SourceDirectory, destination: string): ImageBuilder
  copyFrom(destination: string, source: ImageBuilder, sourcePath: string): ImageBuilder
  workdir(path: string): ImageBuilder
  env(key: string, value: string): ImageBuilder
  user(name: string): ImageBuilder
}

const source: {
  file(path: string): SourceFile
  directory(path: string): SourceDirectory
}
```

Each method returns a new builder with one more step.

| Step | Description |
| --- | --- |
| `from(ref)` | Base image reference, for example `"node:24-bookworm-slim"`. |
| `run(argv)` | Runs an argv array without a shell. |
| `copy(source, destination)` | Copies a project file or directory, selected with `source.file()` or `source.directory()`. |
| `copyFrom(destination, image, sourcePath)` | Copies `sourcePath` from another `image()` into `destination`. |
| `workdir(path)`, `env(key, value)`, `user(name)` | Set the working directory, an environment variable, or the user for later steps and processes. |

Native addons loaded by Agent code and the shared libraries they need are
covered in [Configuration](/docs/reference/config#runtime-dependencies).

## Secret bindings

```ts
type SecretBinding = { readonly secretId: string } & (
  | { readonly env: { readonly name: string; readonly mode: "protected"; readonly allowedOrigins: readonly string[] } }
  | { readonly env: { readonly name: string; readonly mode: "raw" } }
  | { readonly file: { readonly path: string } }
)
```

| Rule | Value |
| --- | --- |
| Bindings per Computer | At most 64 |
| Placement | Exactly one of `env` or `file` |
| `env.name` | Matches `^[A-Za-z_][A-Za-z0-9_]*$`; unique per Computer. Names starting with `HELMR_` or `LD_`, and `NODE_OPTIONS`, `NODE_PATH`, `NODE_ICU_DATA`, `OPENSSL_CONF`, `OPENSSL_MODULES`, `OPENSSL_ENGINES`, `GCONV_PATH`, `LOCPATH`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `NODE_EXTRA_CA_CERTS`, `NODE_USE_SYSTEM_CA` are reserved. |
| `protected` mode | Requires 1–16 exact HTTPS `allowedOrigins`; at most 256 origins per Computer. |
| `raw` mode | Does not accept `allowedOrigins`. |
| `file.path` | Absolute, normalized, at most 4096 bytes. Not under `/workspace`, `/var/lib/helmr`, `/dev`, `/opt/helmr`, `/proc`, `/sys`, `/run/helmr` or `/.helmr-old-root`. Paths must not repeat or nest. |

Bindings reference Secrets by stable ID and never contain values. The same
field names (`secretId`, `allowedOrigins`) are used in REST request bodies. See
[Secrets](/docs/concepts/secrets) for the protection modes.

## Client: Template definitions

```ts
client.templateDefinitions.get(id: string, query?: { deploymentId?: string }, options?): Promise<TemplateDefinitionInfo>
client.templateDefinitions.list(query?: { deploymentId?: string; cursor?: string; limit?: number }, options?): Promise<TemplateDefinitionPage>
```

`TemplateDefinitionInfo` is `{ id, deploymentId }`. `TemplateDefinitionPage` is
`{ deploymentId, items: { id }[], nextCursor? }`. Omitting `deploymentId` reads
the current Deployment; a list cursor keeps its original Deployment. `limit` is
1–100.

## Client: Computers

```ts
client.computers.create(request: ComputerCreateRequest, options?): Promise<ClientComputerRef>
client.computers.get(id: string, options?): Promise<Computer>
client.computers.list(query?: ComputerListQuery, options?): Promise<CursorPage<ComputerListItem>>
client.computers.ref(id: string): ClientComputerRef

interface ComputerCreateRequest {
  readonly definition: string
  readonly key?: string
  readonly secrets?: readonly SecretBinding[]
  readonly deleteAfterUnused?: Duration      // or "never"
  readonly idempotencyKey?: string
}
```

`create` uses the current Deployment. `definition` is the Template definition
ID. `key` is an optional immutable lookup value; it does not make creation
get-or-create. `deleteAfterUnused` overrides the definition.

`ComputerListQuery` is either `{ cursor?, limit? }` or an exact lookup
`{ key }`, which does not accept `cursor` or `limit`. `ComputerListItem` is a
`Computer` without `secrets`.

### `ClientComputerRef`

| Method | Returns | Description |
| --- | --- | --- |
| `get(options?)` | `Computer` | Current state, including Secret bindings. |
| `members({ cursor?, limit? }?, options?)` | `CursorPage<ComputerMember>` | Sessions and Commands attached to the Computer; `limit` 1–100. |
| `exec(request, options?)` | `CommandRef` | Admits a Command. See [Commands](#client-commands). |
| `update({ deleteAfterUnused }, options?)` | `Computer` | Changes retention. Requires the `computers:update` permission. |
| `delete({ idempotencyKey? }?, options?)` | `{ computerId }` | Requests deletion. |

```ts
interface ComputerCommandRequest {
  readonly command: readonly string[]
  readonly idempotencyKey: string
  readonly cwd?: string
  readonly env?: Readonly<Record<string, string>>
  readonly stdin?: Uint8Array
  readonly timeout?: Duration
}
```

`ComputerMember` is `{ kind: "session" | "command", id, state, createdAt }`,
where `state` is `admitted`, `running`, `waiting`, `parked`, `draining` or
`unreconciled`.

### `Computer`

| Field | Type |
| --- | --- |
| `id`, `definitionId`, `deploymentId` | `string` |
| `key` | `string`, optional |
| `status` | `"available" \| "deleting" \| "deleted"` |
| `residency` | `"cold" \| "starting" \| "running" \| "parking" \| "parked" \| "restoring" \| "unavailable"` |
| `error` | `{ code, message }`, present when `residency` is `unavailable` |
| `deleteAfterUnused` | `Duration` (`"never"` when disabled) |
| `unusedSince` | `string \| null` |
| `secrets` | `SecretBinding[]` |
| `lastActivityAt`, `createdAt`, `updatedAt` | `string` |

## Client: Commands

```ts
client.commands.ref(id: string): CommandRef

interface CommandRef {
  readonly id: string
  get(options?): Promise<CommandInfo>
  wait(options?: { signal? }): Promise<CommandOutcome>
  wait(options: { timeout: Duration; signal? }): Promise<TimedCommandWaitResult>
  cancel(options?): Promise<CancelReceipt>
  readonly logs: {
    list(query?: { cursor?: string; limit?: number }, options?): Promise<CommandLogReadPage>
    stream(query?: { after?: string }, options?): AsyncIterable<CommandLogRecord>
  }
}
```

| Member | Description |
| --- | --- |
| `get()` | `CommandInfo`: `{ id, computerId, status, processReconciled, outcome? }`. `status` is `pending`, `starting`, `running`, `stopping`, `exited`, `failed`, `cancelled`, `timed_out` or `lost`. |
| `wait()` | Resolves with the `CommandOutcome`. With `timeout`, resolves with `{ status: "timeout" }` or `{ status: "settled", outcome }`; a timeout does not cancel the Command. |
| `cancel()` | Requests termination. Returns `{ id, targetId, status: "accepted" }`; repeated calls return the same receipt. |
| `logs.list()` | One page: `{ items, outputState, nextCursor? }`, where `outputState` is `open`, `closed` or `unavailable`. `limit` 1–100. |
| `logs.stream()` | Retained output followed by new output until the output closes. Resume with `after: record.cursor`. |

`CommandOutcome` has `commandId`, `terminalAt` and a `kind`:

| `kind` | Extra fields |
| --- | --- |
| `exited` | `exitCode` |
| `cancelled`, `timed_out` | none |
| `system_failed` | `failure.reason`: `guest_failure`, `dispatch_failed` or `scope_termination_failed` |

`CommandLogRecord` has `cursor` and `stream` (`stdout` or `stderr`), and is one of:

| `kind` | Fields |
| --- | --- |
| `output` | `content: Uint8Array`, `observedAt` |
| `gap` | `fromSequence`, `throughSequence` (records that are no longer available) |

Output chunks are bytes, not necessarily whole lines or characters. Ordering
holds within each stream only.

```ts
const command = await client.computers.ref(computerId).exec({
  command: ["npm", "test"],
  idempotencyKey: "test:revision-42",
})
for await (const record of command.logs.stream({}, { signal })) {
  if (record.kind === "output") process.stdout.write(record.content)
}
const outcome = await command.wait()
```

Agent code that needs to run local processes uses ordinary child processes; a
`HelmrClient` inside Agent code acts with its own API key.
