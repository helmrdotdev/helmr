---
title: Agent
description: agent(), AgentDefinition, the Turn handler API, initialization context, and client Session and Turn methods.
sidebarLabel: Agent
---

# Agent

This page lists the Agent authoring API and the `HelmrClient` methods that
operate Sessions and Turns. For what Agents, Sessions and Turns are and how
they behave, see [Agents](/docs/concepts/agents),
[Sessions](/docs/concepts/sessions) and [Turns](/docs/concepts/turns).

## `agent()`

`agent(config)` infers the Agent ID, initialization state, validated input and
result types. Prefer inference over explicit generic arguments. `inputSchema`
and `resultSchema` accept Standard Schema v1 validators, including asynchronous
validators and transforms.

Returns a frozen definition with `kind: "agent"`. Throws `TypeError` when `id`
is blank, `template` is not a Template definition, or `turn` is not a function.
Export the definition from a file in a discovered source directory (`agents` by
default; see [Configuration](/docs/reference/config)).

```ts
import { agent, template, image } from "@helmr/sdk"

const repoComputer = template({
  id: "repo",
  image: image("repo").from("node:24-bookworm-slim"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const reviewer = agent({
  id: "reviewer",
  template: repoComputer,
  async init(ctx) {
    return { startedAt: Date.now() }
  },
  async turn(turn, ctx) {
    await turn.output.write("Review started\n")
    await turn.respond("Review complete")
    return { reviewed: true }
  },
})
```

## `AgentDefinition`

`AgentDefinition` retains the inferred invocation and result contract. Use
`typeof yourAgent` with typed client calls; do not duplicate its types manually.
When `init` is absent, `ctx.initResult` is `undefined`.

| Field | Required | Description |
| --- | --- | --- |
| `id` | yes | Definition ID, matching `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. Used by `client.sessions.create({ agent: id })` and `helmr session create`. |
| `template` | yes | A `template()` definition. New Sessions run on a Computer created from it unless the start request names an existing Computer. See [Computer](/docs/reference/sdk/computer). |
| `init` | no | Runs once per Session process before its first Turn. The returned value is `ctx.initResult`. |
| `turn` | yes | Handles one Turn. Its return value is validated by `resultSchema` when supplied and becomes the machine `result`. |
| `inputSchema` | no | Validates incoming JSON before the handler; `turn.input` is the schema output. Without it, input is conversational text content. |
| `resultSchema` | no | Validates or transforms the handler return value before successful settlement. The persisted result must be finite JSON. |
| `closeAfterIdle` | no | Idle retention for new Sessions: a `Duration` or `"never"`. Default `"7d"`. |
| `maxTurnDuration` | no | A `Duration` after which a running Turn reaches its deadline. No limit when omitted. |
| `triggers` | no | Cron triggers. See [Triggers](/docs/reference/sdk/triggers). |

A `closeAfterIdle` option on `client.sessions.create()` (or `closeAfterIdleMs` on
the runtime MCP `spawn`/`start` tools) overrides the definition for that new
Session. Enqueuing later work keeps the Session's existing policy.

### Typed invocation

```ts
import type { worker } from "./agents/worker"

const started = await client.sessions.create<typeof worker>({
  agent: "worker",
  input: { issue: 42 }, // checked against this Agent's schema input
})
const outcome = await started.turn.wait()
if (outcome.status === "completed" && outcome.payloadExpiredAt === undefined) {
  console.log(outcome.result) // inferred persisted result type
}
```

The example assumes `worker` accepts `{ issue: number }`. Type-only imports do
not load its tools or credentials into the caller. `AgentInput<typeof worker>`
and `AgentResult<typeof worker>` expose the inferred types. A create call with
supplied non-undefined input returns a Turn; an omitted input returns `turn: null`.

These types require the imported definition to match the deployed code. To attach
to an existing Session, use `client.sessions.attach<typeof worker>({ id, agent:
"worker", deploymentId })`; it checks the actual Agent and Deployment identity.
A dynamic `ref(id)` retains JSON types and does not claim a checked contract.

Input validation follows Session initialization. Rejected input can therefore
have incurred initialization work. Invalid input settles as `input_invalid`;
invalid results settle as `result_invalid` and never publish a staged response.
Runtime schema validation is authoritative. Optional JSON Schema discovery is
advisory and can be unavailable for validators that do not provide conversion.
Structured-input Agents use a conversational front for Slack; child results are
not automatically posted as human text.

## `Turn`

```ts
interface Turn<I = InputContent> {
  readonly id: string
  readonly sequence: number
  readonly createdAt: string
  readonly input: I
  readonly source: InputSource
  readonly signal: AbortSignal
  onCancel(handler: (reason: unknown) => MaybePromise<void>): void
  readonly output: {
    write(value: HumanContent): Promise<OutputReceipt>
    pipe(values: AsyncIterable<HumanContent>, options?: { signal?: AbortSignal }): Promise<void>
  }
  respond(value: HumanContent): Promise<void>
  onMessage(handler: (message: Message) => MaybePromise<void>): Promise<void>
  ask<const Q extends Question>(
    question: Q,
    options?: { signal?: AbortSignal },
  ): Promise<AskAnswer<AnswerFor<Q>>>
}
```

| Member | Description |
| --- | --- |
| `id`, `sequence`, `createdAt` | Turn ID, admission sequence within the Session, and creation time. |
| `input` | Validated schema output, or conversational text parts when no input schema is declared. |
| `source` | Where the input came from. See [`InputSource`](#inputsource). |
| `signal` | Aborts when this Turn is cancelled, interrupted, or fails. |
| `onCancel(handler)` | Registers asynchronous cancellation work that the runtime joins alongside the handler. |
| `output.write(value)` | Appends Content (or a string) to the Turn's output. Resolves to `{ sequence }`. |
| `output.pipe(values, { signal? })` | Appends each item of an async iterable in order. Resolves when the iterable is exhausted. |
| `respond(value)` | Stages the Turn's human-facing response. Call at most once; a second call rejects with `response_already_staged`. |
| `onMessage(handler)` | Registers a handler for messages sent to this exact Turn. Await the registration. Messages are delivered one at a time. |
| `ask(question, { signal? })` | Asks a person a question and resolves with the answer. Root Sessions only. See [Questions](/docs/reference/sdk/questions). |

Await every operation you start before the handler returns. New responses and
questions are not admitted after the handler returns.

### Content rules

| Value | Shape | String shorthand |
| --- | --- | --- |
| `turn.input` without an input schema; live message input | `InputContent`: `readonly { type: "text"; text: string }[]` | no |
| `output.write`, `output.pipe` items, `respond` | `HumanContent`: `Content \| string` | yes |
| Question prompt | `HumanContent`: `Content \| string` | yes |

`Content` is `readonly ContentPart[]`, where a part is
`{ type: "text", text }` or `{ type: "json", value }`. Adjacent text fragments
in output concatenate exactly, without inserted separators; JSON parts are
atomic. `respond([])` or `respond("")` records an explicit empty response.

| Limit | Value |
| --- | --- |
| One Content value or question | 256 KiB of encoded JSON, 64 parts |
| Output plus admitted questions, per Turn | 8 MiB cumulative |
| Response | 256 KiB, separate from the output budget |

File parts are not supported. A path or URL inside JSON does not transfer file
bytes.

### `InputSource`

```ts
type InputSource =
  | { kind: "user"; slack?: SlackInputSource }
  | { kind: "api_key" }
  | { kind: "schedule" }
  | { kind: "session"; requesterSessionId: string; originTurnId?: string }

type MessageSource = Exclude<InputSource, { kind: "schedule" }>
interface Message {
  readonly id: string
  readonly content: InputContent
  readonly source: MessageSource
}
```

Narrow `source.kind` before reading caller-specific fields. Session-originated
input includes `requesterSessionId`; `originTurnId` is present when it was
created during a Turn. Slack input belongs to a user source and includes
`teamId`, `channelId`, `threadTs`, `messageTs` and `userId`.
Each `onMessage` callback receives one Message envelope with its own content,
identity and source. Schedules create Turns and are not live message sources.

### Cancellation and native SDKs

Await initialization in `init` and invocation work in `turn`. Ordinary Node child
processes, including children created during a Turn, belong to the Session and
can remain alive between Turns. Join the invocation and callbacks according to
the native SDK's documented completion contract on both success and failure.
Request acknowledgement, native Turn completion and server shutdown can be
different events. None implies universal cessation of arbitrary OS descendants.

Use `turn.signal` with signal-aware APIs. For an asynchronous interruption API,
register `turn.onCancel(reason => ...)`. The runtime starts registered callbacks
concurrently with handler drainage on interruption, deadline, Session shutdown or
uncaught handler failure, and joins them before acknowledging drainage. Each
registration runs at most once in that runtime execution. Successful completion
does not run cancellation callbacks. A callback registered after cancellation
starts immediately while drainage is still open; registration after closure throws.

The handler and callbacks share a bounded cleanup period, currently 10 seconds
in the managed runtime. A rejected cancellation
callback or expired cleanup period prevents Session reuse and requires explicit
recovery; the platform terminates the entire Session process tree. A fully drained
ordinary handler failure can preserve a healthy Session. Retain native connection
errors, register cancellation before checking native health, and reject a new
invocation before submitting work when that connection is broken.

For local HTTP services, the managed runtime removes default response-header and
body inactivity timeouts on loopback requests through the global dispatcher.
Connection establishment remains bounded. Caller AbortSignals and explicitly
provided dispatchers retain their behavior. In-flight HTTP still prevents idle
capture; ordinary idle connection expiry allows capture afterwards.

## `InitContext` and `AgentContext`

```ts
interface InitContext {
  readonly mcp: Readonly<{ getConnectionOptions(): Promise<RuntimeMcpConnectionOptions> }>
  readonly session: Readonly<{ id: string; key?: string; parent?: Readonly<{ id: string }> }>
  readonly computer: ComputerIdentity              // { id: string }
  readonly deployment: Readonly<{ id: string }>
  readonly recovery: Readonly<{ kind: "initial" | "reconstructed"; reason?: string }>
  readonly signal: AbortSignal
}

interface AgentContext<S> extends InitContext {
  readonly initResult: S
}
```

| Member | Description |
| --- | --- |
| `session.id`, `session.key`, `session.parent` | Session ID, optional Session key, and the parent Session for owned child work. |
| `computer.id` | The Computer this Session runs on. |
| `deployment.id` | The Deployment the Session is pinned to. |
| `recovery.kind` | `"initial"` for a new Session process; `"reconstructed"` when the Session was explicitly reconstructed and `init` runs again. `reason` is optional. |
| `signal` | Aborts on Session process shutdown, including during `init`. |
| `mcp.getConnectionOptions()` | Returns the Session process’s local MCP URL and headers. Available in `init` and `turn`, including when `init` is omitted. See [Runtime MCP](/docs/reference/sdk/mcp). |
| `initResult` | The value returned by `init` (`AgentContext` only). |

`init` does not rerun when an idle Session resumes from hibernation. See
[Durability](/docs/concepts/durability).

## `MessageRejected`

```ts
class MessageRejected extends Error {
  constructor(message: string)
}
```

Throw `MessageRejected` from an `onMessage` handler to reject that message
before the handler has caused any effects. The Session records a
`message.rejected` event with reason `message_rejected` and delivery
`not_delivered`, and the Turn continues. Any other error thrown from a message
handler is recorded with reason `callback_failed` and fails the Turn.
`instanceof MessageRejected` works across copies of the SDK.

## Creating other work

Agent code creates and controls other Sessions through the runtime MCP tools,
not through an SDK method. See [Runtime MCP](/docs/reference/sdk/mcp) and
[Delegation](/docs/concepts/delegation).

## Client: create a Session

```ts
client.sessions.create(request: SessionCreateRequest, options?: RequestOptions): Promise<SessionCreateReceipt>

interface SessionCreateRequest<A extends AgentContract = AgentContract<string, JsonValue, JsonValue>> {
  readonly agent: A["id"]
  readonly input?: AgentInput<A>
  readonly closeAfterIdle?: Retention
  readonly slack?: Readonly<{ channelId: string }>
  readonly computer?: Readonly<{ id: string }>
  readonly key?: string
  readonly idempotencyKey?: string
}

interface SessionCreateReceipt {
  readonly session: SessionRef
  readonly turn: TurnRef | null
  readonly closeAfterIdle: Retention         // resolved policy; "never" when disabled
}
```

| Option | Description |
| --- | --- |
| `agent` | Authored Agent ID in the current Deployment. |
| `input` | Optional initial Turn input. Omit for zero Turns; supplied input must be finite JSON and match the Agent contract. |
| `closeAfterIdle` | Overrides the definition's idle retention for a new Session. |
| `slack.channelId` | Opens the Session in a Slack channel, by raw channel ID (for example `C0123456789`). See [Slack](/docs/concepts/slack). |
| `computer.id` | Runs on an existing Computer. Omit to assign one from the pinned Template definition on first input. |
| `key` | Unique Session identity within its Agent. An occupied key conflicts. |
| `idempotencyKey` | Retry identity. When omitted, the SDK generates a new key for each call, so pass one explicitly to retry safely. |

New Sessions pin the current Deployment. Enqueue later work using the Session ID.
Exact creation retries retain the original receipt, including a null initial Turn. The receipt confirms admission, not
completion.

## Client: Sessions

```ts
client.sessions.get(id: string, options?): Promise<Session>
client.sessions.list(query?: SessionListQuery, options?): Promise<CursorPage<Session>>
client.sessions.ref(id: string): SessionRef
```

`ref()` builds a local handle without a network request.

`SessionListQuery` is one of:

| Form | Fields |
| --- | --- |
| Page | `parentSessionId?`, `requesterSessionId?`, `status?` (one `SessionStatus` or an array), `cursor?`, `limit?` (1–100) |
| Agent filter | `agent` (declared identifier); current membership is not required |
| Exact key lookup | One Agent selector plus the Session `key`; no other filters or pagination |

### `SessionRef`

| Method | Returns | Description |
| --- | --- | --- |
| `get(options?)` | `Session` | Current Session state. |
| `enqueue(input, { idempotencyKey? }?, options?)` | `TurnRef` | Always admits a new Turn. |
| `turns.ref(id)` | `TurnRef` | Local handle for a Turn. |
| `turns.list({ cursor?, limit? }?, options?)` | `CursorPage<TurnState>` | Turns in admission order; `limit` 1–100. |
| `events.list({ after?, limit? }?, options?)` | `SessionEventPage` | One page of events. `after` defaults to `0`; `limit` defaults to 100, maximum 1000. |
| `events.stream({ after? }?, options?)` | `AsyncIterableIterator<SessionEvent>` | Follows pages from `after`, polling when caught up. Stop it with `signal`. |
| `interrupt({ idempotencyKey? }?, options?)` | `SessionInterruptReceipt` | Holds the Session and its owned descendants. Receipt includes `holdId`. |
| `resume({ holdId, idempotencyKey? }, options?)` | `SessionResumeReceipt` | Releases the exact hold. |
| `close({ idempotencyKey? }?, options?)` | `SessionCloseReceipt` | Stops new admission and drains accepted work. |
| `cancel({ idempotencyKey? }?, options?)` | `SessionCancelReceipt` | Cancels queued and active work in the owned tree. |

Control receipts have `{ id, sessionId, status: "accepted" }` (plus `holdId` for
interrupt and resume). They confirm acceptance, not that execution has stopped.
For control semantics see [Sessions](/docs/concepts/sessions). Event kinds and
payloads are listed in [Session events](/docs/reference/events).

### `Session`

| Field | Type |
| --- | --- |
| `id`, `deploymentId`, `rootSessionId` | `string` UUIDs |
| `agent`, `template` | Authored Agent identifier and its pinned default Template identifier |
| `computerId`, `parentSessionId`, `requesterSessionId` | `string \| null` |
| `key` | `string`, optional |
| `slackChannelId` | `string`, optional |
| `status` | `"open" \| "closing" \| "closed" \| "cancelled"` |
| `closeAfterIdle` | `Retention` (`Duration` or `"never"`) |
| `idleSince` | `string \| null` |
| `initialTurn` | `{ id, status } \| null` |
| `holds` | `{ id, sessionId, scope: "local" \| "subtree", kind, reason, recoveryGap?, createdAt }[]` |
| `createdAt` | `string` |

### `TurnRef`

| Member | Returns | Description |
| --- | --- | --- |
| `id`, `sessionId` | `string` | Identity. |
| `get(options?)` | `TurnState` | Current Turn state. |
| `wait({ signal? }?)` | `TurnOutcome` | Resolves when the Turn settles. |
| `wait({ timeout, signal? })` | `TimedTurnWaitResult` | `{ status: "timeout" }` or `{ status: "settled", outcome }`. A timeout never cancels the Turn. |
| `send(content, { idempotencyKey? }?, options?)` | `MessageReceipt` | Sends a message to exactly this Turn. Returns `{ id, status: "accepted" }`. |
| `interrupt({ idempotencyKey? }?, options?)` | `TurnInterruptReceipt` | Interrupts this running Turn without a hold. Returns `{ id, sessionId, turnId, status: "accepted" }`. |
| `asks` | `TurnAsks` | Question methods. See [Questions](/docs/reference/sdk/questions#client-methods). |

`TurnState` has `id`, `sessionId`, `sequence`, `status`, and optional `input`,
`startedAt`, `terminalAt`, `result`, `response`, `error`
(`{ code, message? }`) and `payloadExpiredAt`. `TurnStatus` is one of `queued`,
`running`, `completed`, `failed`, `interrupted`, `cancelled`.

`TurnOutcome` is discriminated by `status`. A completed outcome with retained
payload has its typed `result` and optional human `response`; an expired payload
has `payloadExpiredAt` and no result or response. Failed outcomes may contain an
error; interrupted and cancelled outcomes never contain a result or response.
Narrow status and retention before reading payload fields. Timed waits use the
separate `timeout` / `settled` envelope shown above.

## Client: Agents

```ts
client.agents.get(id: string, query?: { deploymentId?: string }, options?): Promise<AgentInfo>
client.agents.list(query?: { deploymentId?: string; cursor?: string; limit?: number }, options?): Promise<AgentPage>
```

`AgentInfo` includes `agent`, `template`, `deploymentId` and the input contract.
The `agent` identifier is declared in code and remains stable across Deployments
within its environment. `inputKind: "text"` identifies conversational input;
`inputKind: "json"` includes `inputSchema`, which is either an available JSON
Schema or an explicit unavailability reason. This discovery does not replace
runtime validation.
`AgentPage` is `{ deploymentId, items, nextCursor? }`; each item includes
`agent`, `template` and the same input contract.
Omitting `deploymentId` reads the current Deployment; a list cursor keeps its
original Deployment.
