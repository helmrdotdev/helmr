---
title: Agents, Sessions and Turns
description: Define an Agent and operate retained Sessions and attributable Turns.
---

# Agents, Sessions and Turns

An Agent definition selects a Computer and handles one Turn at a time. A Session
retains initialization state across those Turns and healthy checkpoint restoration. Each
accepted input has a distinct Turn ID and admission sequence.

```ts
import { agent, computer, image } from "@helmr/sdk"

const workspace = computer({
  id: "workspace",
  image: image("workspace").from("ubuntu:24.04"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const reviewer = agent({
  id: "reviewer",
  computer: workspace,
  async turn(turn) {
    await turn.output.write("Review started")
    return { reviewed: true }
  },
})
```

The `turn` handler returning prepares a machine result. Completed is committed only after
required drainage and the Turn's own Save publication. `turn.respond(content)`
stages a human-facing response published with successful settlement. Output is
available while work is running or saving; it does not establish completion.

## Definition IDs and Agent identity

An Agent has a stable UUID within its Environment. Each Deployment records a
separate definition for that Agent. Read definitions through
`client.agentDefinitions.list({ deploymentId?, cursor?, limit? })` or
`client.agentDefinitions.get(id, { deploymentId? })`. Omission uses the current
Deployment; list cursors retain their original Deployment.

```ts
const definition = await client.agentDefinitions.get("reviewer")
// { id: "reviewer", agentId: "<Agent UUID>", deploymentId: "<Deployment UUID>" }
```

`id` is the identifier declared in `agent({ id: ... })`, not the Agent UUID.
`agentId` links the definition to Session and Schedule records. Definition list
items include both IDs; the page includes their shared `deploymentId` and optional
`nextCursor`. An authored ID may itself look like a UUID; its spelling never
changes which identifier a method accepts.

`client.agents.start(id, request)` starts work by the authored ID. New independent
Sessions use the current Deployment; reuse through `sessionKey` keeps the existing
Session's Deployment. Child Sessions use their parent's pinned Deployment.
Historical definition reads do not select a Deployment for a new start.

## Initialization

`init(ctx)` runs once per live Agent process before its first Turn. Its returned value is available as `ctx.initResult`; `InitContext` is the exported initialization context type. Healthy checkpoint continuation preserves this state. Reconstruction creates a new process and runs `init` again with `ctx.recovery.kind === "reconstructed"`. A failed initialization remains failed in that process; redelivery does not retry it. Use idempotent external operations when reconstruction could repeat them.

## Authored context and helper Sessions

| Member | Contract |
| --- | --- |
| `ctx.session.id`, `.key`, `.parent` | Session identity, optional conversation key and optional parent identity `{ id }`. |
| `ctx.computer` | Current `ComputerIdentity` (`{ id }`) for explicit placement. |
| `ctx.deployment.id` | Pinned Deployment identity. |
| `ctx.recovery` | `kind: "initial"` or `"reconstructed"`, with optional reason. |
| `ctx.initResult` | The value returned by `init`, including live application/native handles. |
| `ctx.signal` | Process shutdown and initialization cancellation. |
| `turn.id`, `.sequence`, `.createdAt` | This input's identity, admission sequence and creation time. |
| `turn.input`, `.source` | Text-part array input and origin facts: caller kind, optional requester Session and origin Turn IDs, and optional Slack coordinates. |
| `turn.signal` | Cancellation for this Turn's processing. |
| `turn.onMessage(handler)` | Await registration; `handler(input: InputContent, message: InputMessage)` processes serial exact-Turn steering. |
| `turn.output.pipe(values, { signal? })` | Append an async iterable of Content/string fragments; await completion. |

Runtime delegation uses the Session-bound managed MCP connection returned by
`createRuntimeMcpConnection()` from `@helmr/sdk/mcp`. Use an ordinary MCP client
with its URL and headers. Only root Sessions can call `spawn` or `start`:

| Tool | Result |
| --- | --- |
| `spawn` | A child owned by the caller, using its pinned Deployment. |
| `start` | A new independent root using the Environment's current Deployment. |
| `list_agents` | Discover creation targets with required `operation: "spawn"` or `"start"`. |

Creation arguments are `agentId`, `input`, required `idempotencyKey` and optional
`computerId`. Both tools return `{ sessionId, turnId, sequence, created }`. An
omitted Computer ID creates a fresh Computer from the target definition. Children
cannot create more work or ask humans; report those needs to the ownership root.

Ownership follows the Session. Parent Turn return or failure does not implicitly
join or stop children. Independent work is unaffected by its requester's controls.
Use `enqueue` for existing work, `inspect_session`, `list_sessions`, `inspect_turn`
and `wait_turn` for observation. `send_turn` and the Session control tools operate
only on owned children. Resume releases only an exact caller-issued hold.
Control acknowledgement does not establish that the process has stopped.

Runtime creation has no conversation key. The external authenticated client keeps
its own Session and Turn handles; it does not inherit runtime authority.

## Human content and messages

Every Agent accepts the same JSON array of text parts for initial input, cron,
queued work and live messages. Empty input is `[]`. Strings, `null`, object
wrappers and JSON parts are not input. Machine results remain arbitrary JSON.
Output Content supports text and JSON parts:

```ts
import type { Content, InputContent } from "@helmr/sdk"

const input: InputContent = [{ type: "text", text: "Review APP-42" }]
const content: Content = [
  { type: "text", text: "Review summary\n" },
  { type: "json", value: { changedFiles: 3 } },
]
```

No message capability declaration is needed. Only `output.write`, `output.pipe`
items and `respond` accept string shorthand. Input and question prompts use arrays.
Output appends in
order: adjacent text fragments concatenate exactly, without inserted spaces or
newlines. JSON parts are atomic blocks. Forward native deltas or a final snapshot
once; do not append both as if they were distinct content. Select outward content
explicitly instead of publishing raw native diagnostics.

Await at most one `turn.respond` call per Turn. A second submission rejects with
`response_already_staged`. New response admission closes at handler return. The
staged response appears only with successful settlement; other outcomes suppress
it. No call omits the response, while `respond([])` or `respond("")` records an
explicit empty response. When present, the outcome’s response is normalized Content.

Each Content value or question is limited to 256 KiB encoded JSON and 64 Content
parts. Output and admitted questions share an 8 MiB cumulative Turn budget. The
one response has its own 256 KiB allowance. Pipe can retain its accepted prefix
before rejecting a later fragment. See [Questions](/docs/reference/sdk/questions)
for answer/control limits. File parts and platform-managed file downloads are not
supported; a path or URL in JSON does not transfer or preserve file bytes.

## Use an authenticated client

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({ url, apiKey: process.env["HELMR_API_KEY"]! })
const { session, turn } = await client.agents.start("reviewer", {
  input: [{ type: "text", text: "Review APP-42" }],
  sessionKey: "issue:APP-42",
  idempotencyKey: "issue:event-123",
})
const waiting = turn.wait({ timeout: "10m" })
const progress = await session.events.list({ after: 0 })
const observed = await waiting
const next = await session.enqueue([{ type: "text", text: "Review APP-43" }], {
  idempotencyKey: "issue:event-124",
})
const page = await session.events.list({ after: 0, limit: 100 })
```

The explicit client uses its authentication even when constructed inside Agent
code. Injected Turn operations carry the current Session's runtime authority.
A client does not inherit that authority from code location.

Recover handles with `client.sessions.ref(sessionId)` and
`session.turns.ref(turnId)`. These construct local handles without network I/O. `get()` reads retained state, and `session.turns.list()` reads a page. `turn.wait()` returns
a terminal outcome; `wait({ timeout })` returns either a settled outcome or
`{ status: "timeout" }`. Timeout never cancels work, and a failed outcome does
not close the Session. Payload expiry remains explicit in retained reads.

`session.send(data)` messages active work or enqueues a Turn when idle;
`session.enqueue(input)` always creates FIFO work. `session.turns.ref(id).send(data)`
targets that exact Turn. Message acceptance is separate from delivery and
application. Held Sessions reject automatic send but retain queued input.

## Questions and events

Inside a root Session Turn, `await turn.ask({ prompt, answer })` returns an answer and its
authenticated responder. The external client's `turn.asks.list`, `get` and
`respond` expose exact questions and current answer authority. A response uses
`{ answer, responseId }`; reuse that identity and answer for uncertain retries.

`session.events.list({ after, limit })` returns records, `nextAfter`, `hasMore`
and `retainedAfter`. Use the durable sequence as the reconnect cursor.
`session.events.stream({ after })` follows those pages. A page boundary or output
EOF is not Turn completion. Process stdout/stderr is not a public Session log API.

## Controls

`session.interrupt()` holds the selected Session and its owned descendants,
returning one inspectable hold ID. It preserves queued work. Interruption has
no Turn-only variant. `session.resume({ holdId })` releases the exact authorized
hold without replaying interrupted inputs. Inspect `hold.sessionId` to identify
the owning Session when a hold is inherited from a parent.

`session.close()` rejects new ordinary admission and drains retained work;
existing holds remain. `session.cancel()` cancels the owned tree, discards queued
work and requests active work to stop. Control acceptance is separate from
physical convergence. Independent Sessions are unaffected by controls on their
requester. Explicit Computer deletion can remain busy until physical cleanup.

## Input provenance and retention

Native Slack input includes `turn.source.slack` with `teamId`, `channelId`,
`threadTs`, `messageTs` and `userId`. `source.kind` remains the caller kind.
Each steering callback receives its own `{ id, source }` as the second argument;
it does not inherit the initial Turn's author. These are references to the
accepted input, not a transcript. Helmr does not fetch conversation history or
choose model context. Application code can use the Slack SDK with an explicitly
supplied customer credential through Secrets; native integration tokens are not
exposed to Agent code.

History is optional. These two handlers use the same input contract (with an
existing `myComputer` definition):

```ts
import { agent } from "@helmr/sdk"
import { WebClient } from "@slack/web-api"

export const inputOnly = agent({
  id: "input-only",
  computer: myComputer,
  async turn(turn) {
    return { input: turn.input }
  },
})

export const withThreadPage = agent({
  id: "with-thread-page",
  computer: myComputer,
  async turn(turn) {
    const source = turn.source.slack
    if (!source) return { input: turn.input, context: [], nextCursor: null, hasMore: false }
    const token = process.env["SLACK_CONTEXT_TOKEN"]
    if (!token) throw new Error("SLACK_CONTEXT_TOKEN is required")
    const slack = new WebClient(token, {
      timeout: 10_000,
      rejectRateLimitedCalls: true,
      retryConfig: { retries: 0 },
    })
    const page = await slack.conversations.replies({
      channel: source.channelId,
      ts: source.threadTs,
      latest: source.messageTs,
      inclusive: true,
      limit: 15,
    })
    return {
      input: turn.input,
      context: (page.messages ?? []).map(message => ({
        user: message.user ?? null,
        text: message.text ?? "",
        ts: message.ts ?? null,
      })),
      nextCursor: page.response_metadata?.next_cursor ?? null,
      hasMore: page.has_more ?? false,
    }
  },
})
```

Install `@slack/web-api` in the application and supply your own Slack token as
an Environment Secret bound to the Computer, for example using a protected env
binding named `SLACK_CONTEXT_TOKEN` with `allowedOrigins: ["https://slack.com"]`.
The token needs access to the source workspace/channel and the scopes required
by [Slack's `conversations.replies`](https://docs.slack.dev/reference/methods/conversations.replies/).
The example fetches one page through the submitted message; it does not claim a
complete or immutable transcript. The application decides whether to paginate,
handle rate limits or unavailable history, and pass the result to its harness.
Use each callback's `message.source.slack` when retrieving context for a later
`onMessage` input. A custom Slack listener also owns its authentication, retry
keys, thread-to-Session mapping and publication; supplying Slack IDs in text does
not grant native integration authority.

In shared Slack channels, native input requires an explicit mention of the
owning bot. Ordinary discussion, mentions of other people or bots, and bot names
inside code or quotes do not trigger work. One thread belongs to one Agent;
mention another Agent in a new thread. A closed Session does not reopen from a
late reply. Exact question-response and control actions retain their own rules.

`agent({ closeAfterIdle })` sets the default for new Sessions. The default is
`"7d"`; `"never"` disables automatic closure. A `closeAfterIdle` option on
`client.agents.start(id, request)` overrides the definition for that new Session.
Its receipt and Session reads expose the resolved `closeAfterIdle`; `idleSince`
is null while an idle interval has not been established. Changing a definition
does not rewrite existing Sessions. Reusing a `sessionKey` preserves its policy;
an explicit conflicting override is rejected. Managed MCP `start` and `spawn`
accept `closeAfterIdleMs` as positive safe integer milliseconds or null for never.
Omitting it inherits the definition.
