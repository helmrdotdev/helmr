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

```ts
function agent<I extends InputContent = InputContent, O extends Json = Json, S = undefined>(
  config: Omit<AgentDefinition<I, O, S>, "kind">,
): AgentDefinition<I, O, S>
```

Returns a frozen definition with `kind: "agent"`. Throws `TypeError` when `id`
is blank, `computer` is not a Template definition, or `turn` is not a function.
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

```ts
interface AgentDefinition<I extends InputContent = InputContent, O extends Json = Json, S = unknown> {
  readonly kind: "agent"
  readonly id: string
  readonly computer: TemplateDefinition
  readonly init?: (ctx: InitContext) => MaybePromise<S>
  readonly turn: (turn: Turn<I>, ctx: AgentContext<S>) => MaybePromise<O>
  readonly closeAfterIdle?: Duration
  readonly maxTurnDuration?: Duration
  readonly triggers?: readonly CronTrigger<I>[]
}
```

| Field | Required | Description |
| --- | --- | --- |
| `id` | yes | Definition ID, matching `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. Used by `client.sessions.create(id)` and `helmr session create`. |
| `template` | yes | A `template()` definition. New Sessions run on a Computer created from it unless the start request names an existing Computer. See [Computer](/docs/reference/sdk/computer). |
| `init` | no | Runs once per Session process before its first Turn. The returned value is `ctx.initResult`. |
| `turn` | yes | Handles one Turn. Its JSON return value is the Turn's machine `result`. |
| `closeAfterIdle` | no | Idle retention for new Sessions: a `Duration` or `"never"`. Default `"7d"`. |
| `maxTurnDuration` | no | A `Duration` after which a running Turn reaches its deadline. No limit when omitted. |
| `triggers` | no | Cron triggers. See [Triggers](/docs/reference/sdk/triggers). |

A `closeAfterIdle` option on `client.sessions.create()` (or `closeAfterIdleMs` on
the runtime MCP `spawn`/`start` tools) overrides the definition for that new
Session. Enqueuing later work keeps the Session's existing policy.

## `Turn`

```ts
interface Turn<I extends InputContent = InputContent> {
  readonly id: string
  readonly sequence: number
  readonly createdAt: string
  readonly input: I
  readonly source: InputSource
  readonly signal: AbortSignal
  readonly output: {
    write(value: HumanContent): Promise<OutputReceipt>
    pipe(values: AsyncIterable<HumanContent>, options?: { signal?: AbortSignal }): Promise<void>
  }
  respond(value: HumanContent): Promise<void>
  onMessage(handler: (input: InputContent, message: InputMessage) => MaybePromise<void>): Promise<void>
  ask<C extends AnswerControl>(
    question: Question<C>,
    options?: { signal?: AbortSignal },
  ): Promise<AskResponse<AnswerFor<C>>>
}
```

| Member | Description |
| --- | --- |
| `id`, `sequence`, `createdAt` | Turn ID, admission sequence within the Session, and creation time. |
| `input` | The Turn's input: an array of text parts. |
| `source` | Where the input came from. See [`InputSource`](#inputsource). |
| `signal` | Aborts when this Turn's processing is cancelled or interrupted. |
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
| `turn.input`, message input | `InputContent`: `readonly { type: "text"; text: string }[]` | no |
| `output.write`, `output.pipe` items, `respond` | `HumanContent`: `Content \| string` | yes |
| Question prompt | `Content` | no |

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
interface InputSource {
  readonly kind: string
  readonly requesterSessionId?: string
  readonly originTurnId?: string
  readonly slack?: Readonly<{
    teamId: string
    channelId: string
    threadTs: string
    messageTs: string
    userId: string
  }>
}

interface InputMessage {
  readonly id: string
  readonly source: InputSource
}
```

`kind` is the caller kind. `requesterSessionId` and `originTurnId` identify the
Session and Turn that created the input, when another Session did.
`slack` is present for input that arrived through the native Slack
integration; see [Slack](/docs/concepts/slack). Each `onMessage` callback
receives its own `InputMessage` with its own `source`.

## `InitContext` and `AgentContext`

```ts
interface InitContext {
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
| `initResult` | The value returned by `init` (`AgentContext` only). |

`init` does not rerun when an idle Session resumes from hibernation. See
[Durability](/docs/concepts/durability).

## `MessageRejected`

```ts
class MessageRejected extends Error {
  readonly details?: Json
  constructor(message: string, details?: Json)
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

interface SessionCreateRequest {
  readonly agent: string
  readonly input?: InputContent
  readonly closeAfterIdle?: Duration          // or "never"
  readonly slack?: Readonly<{ channelId: string }>
  readonly computer?: Readonly<{ id: string }>
  readonly key?: string
  readonly idempotencyKey?: string
}

interface SessionCreateReceipt {
  readonly session: ClientSessionRef
  readonly turn: TurnRef | null
  readonly closeAfterIdle: Duration          // resolved policy; "never" when disabled
}
```

| Option | Description |
| --- | --- |
| `agent` | Authored Agent ID in the current Deployment. |
| `input` | Optional initial Turn input. Omit for zero Turns; explicit empty input is rejected. |
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
client.sessions.ref(id: string): ClientSessionRef
```

`ref()` builds a local handle without a network request.

`SessionListQuery` is one of:

| Form | Fields |
| --- | --- |
| Page | `parentSessionId?`, `requesterSessionId?`, `status?` (one `SessionStatus` or an array), `cursor?`, `limit?` (1–100) |
| Exact key lookup | `agentId` (Agent UUID) and `key`, both required; no other fields |

### `ClientSessionRef`

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
| `id`, `agentId`, `deploymentId`, `rootSessionId` | `string` |
| `computerId`, `parentSessionId`, `requesterSessionId` | `string \| null` |
| `key` | `string`, optional |
| `slackChannelId` | `string`, optional |
| `status` | `"open" \| "closing" \| "closed" \| "cancelled"` |
| `closeAfterIdle` | `Duration` (`"never"` when disabled) |
| `idleSince` | `string \| null` |
| `initialTurn` | `{ id, status } \| null` |
| `holds` | `{ id, sessionId, scope: "local" \| "subtree", reason, createdAt }[]` |
| `createdAt` | `string` |

### `TurnRef`

| Member | Returns | Description |
| --- | --- | --- |
| `id`, `sessionId` | `string` | Identity. |
| `get(options?)` | `TurnState` | Current Turn state. |
| `wait({ signal? }?)` | `TurnOutcome` | Resolves when the Turn settles. |
| `wait({ timeout, signal? })` | `TimedTurnWaitResult` | `{ status: "timeout" }` or `{ status: "settled", outcome }`. A timeout never cancels the Turn. |
| `send(data, { idempotencyKey? }?, options?)` | `MessageReceipt` | Sends a message to exactly this Turn. Returns `{ id, status: "accepted" }`. |
| `interrupt({ idempotencyKey? }?, options?)` | `TurnInterruptReceipt` | Interrupts this running Turn without a hold. Returns `{ id, sessionId, turnId, status: "accepted" }`. |
| `asks` | `TurnAsks` | Question methods. See [Questions](/docs/reference/sdk/questions#client-methods). |

`TurnState` has `id`, `sessionId`, `sequence`, `status`, and optional `input`,
`startedAt`, `terminalAt`, `completionSaveId`, `result`, `response`, `error`
(`{ code, message? }`) and `payloadExpiredAt`. `TurnStatus` is one of `queued`,
`running`, `finalizing`, `completed`, `failed`, `interrupted`, `cancelled`.

`TurnOutcome` has `status` (`completed`, `failed`, `interrupted` or
`cancelled`) and optional `result`, `response`, `error` and `payloadExpiredAt`.

## Client: Agent definitions

```ts
client.agentDefinitions.get(id: string, query?: { deploymentId?: string }, options?): Promise<AgentDefinitionInfo>
client.agentDefinitions.list(query?: { deploymentId?: string; cursor?: string; limit?: number }, options?): Promise<AgentDefinitionPage>
```

`AgentDefinitionInfo` is `{ id, agentId, deploymentId }`, where `id` is the
definition ID and `agentId` is the Agent's stable UUID across Deployments.
`AgentDefinitionPage` is `{ deploymentId, items: { id, agentId }[], nextCursor? }`.
Omitting `deploymentId` reads the current Deployment; a list cursor keeps its
original Deployment.
