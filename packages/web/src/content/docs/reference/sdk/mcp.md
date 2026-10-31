---
title: Runtime MCP
description: ctx.mcp.getConnectionOptions() and the runtime MCP tools available to a running Session.
sidebarLabel: Runtime MCP
---

# Runtime MCP

Each Session process can reach a Session-bound MCP server. Its tools create
and control other Sessions with the current Session's runtime authority. For
when to use child or independent work, see [Delegation](/docs/concepts/delegation).

## `ctx.mcp.getConnectionOptions(policy?)`

```ts
// Available on InitContext and AgentContext.
const options: RuntimeMcpConnectionOptions = await ctx.mcp.getConnectionOptions()

// Request a narrower connection when injecting MCP into a native client.
const observation = await ctx.mcp.getConnectionOptions({
  allow: [{ operation: "inspect_turn", agents: ["helper"] }],
})

interface RuntimeMcpConnectionOptions {
  readonly url: string
  readonly headers: Readonly<{ Authorization: string }>
}
```

Returns the connection for the current Session process: a local
`http://127.0.0.1:<port>/mcp` URL and a bearer `Authorization` header. Omitting
`policy` requests the Agent's published `runtimeMcp` maximum. If that Agent field
is omitted, publication resolves a finite maximum using the exact Agent keys in
that Deployment and new Computer placement.
A supplied `{ allow: [...] }` must be a subset of the maximum; `{ allow: [] }`
grants no remote operations. There is no explicit unrestricted argument.

Equivalent policies return the same URL and header values in that process while
rechecking current authority. Distinct scopes have distinct credentials. A process
can retain its maximum and at most 15 distinct proper subsets; exceeding that limit
rejects without replacing existing descriptors. The server is provisioned lazily
on the first successful call. The accessor is available on
`ctx.mcp` in both `init` and `turn`, including Agents without `init`; calls
after process shutdown reject.

Normal hibernation preserves the connection options. Reconstruction starts a
new process and reruns `init`; obtain options from the new context. The return
types `RuntimeMcpConnectionOptions`, `RuntimeMcpPolicy`, and `RuntimeMcpRule`
are exported from `@helmr/sdk`. See [Execution policies](/docs/concepts/execution-policies)
for rule syntax and trust boundaries.

Pass the URL and headers to any MCP client that supports the Streamable HTTP
transport, for example a model harness running inside the Computer:

```ts
import { agent } from "@helmr/sdk"

// `repoComputer` is an existing template() definition.
export const coordinator = agent({
  id: "coordinator",
  template: repoComputer,
  async init(ctx) {
    const { url, headers } = await ctx.mcp.getConnectionOptions()
    return { helmrMcp: { url, headers } }
  },
  async turn(turn, ctx) {
    // Configure the harness that runs this Turn with ctx.initResult.helmrMcp
    // as a Streamable HTTP MCP server.
    return { ok: true }
  },
})
```

The header is a secret for this process. Do not log it or send it off the
Computer.

## Server behavior

| Property | Value |
| --- | --- |
| Transport | MCP Streamable HTTP, stateless, JSON responses |
| Server name | `helmr-runtime` |
| Request body | At most 1 MiB |
| Request timeout | 60 seconds |
| Concurrent requests | At most 64 per Session process; excess requests receive HTTP 503 with `Retry-After: 1` |

Tool calls are not tied to a Turn. Closing the MCP connection, an HTTP
`DELETE`, or client disconnect never closes or cancels a Session. A
disconnect during a mutation does not undo it; reconcile with the same
`idempotencyKey`.

Successful tool results are a single text content item containing a JSON
object. Failed calls return an error whose text is `code: message` (see
[Errors](#errors)).

## Runtime skill

The server's initialization instructions point to `helmr-runtime`, a skill
that explains how to delegate work, observe outcomes, and control Sessions.
Agents can discover and read it with the `skills` tool over the same MCP
connection. There is no file to install in the harness's working directory.
Clients decide whether to show server instructions to the model; the `skills`
tool description also explains how to retrieve the guidance.

The skill is bundled with the MCP server. Its content and the exposed tools reflect
the connection's scope.
It explains workflows without granting permissions or replacing the user's
instructions. Reading it is subject to the same authentication and Session
admission checks as other MCP requests, including holds and shutdown.

## Authority

| Rule | Tools |
| --- | --- |
| Any admitted Session, including children | `skills` |
| Root Sessions only | `list_agents`, `spawn`, `start` |
| Owned descendants only | `send_turn`, `interrupt_session`, `resume_session`, `close_session`, `cancel_session` |
| Eligible Sessions in this Environment | `enqueue` |
| Sessions accessible to the caller | `inspect_session`, `inspect_turn`, `wait_turn`, `list_sessions` |

Every remote tool also requires the connection's operation and exact target Agent
grant. `list_agents` derives its targets from `start` or `spawn`; `list_sessions`
uses `inspect_session`, and `wait_turn` uses `inspect_turn`. `list_computers` needs
its own targetless rule. Current Role, origin, relationship, lifecycle and Computer
checks still apply. Creation grants add no implicit observation or control grants.

The receiver enforces these grants during admission and before returning a retained
receipt. Retrying creation with omitted `computerId` still requests `new` placement,
even if its original receipt already identifies an allocated Computer. Admitted
work retains its fixed target and can continue after the caller process is rebuilt.

A narrower connection scopes requests made with that credential. It does not isolate
native clients or shell code sharing the same Computer: code able to access the
Session's runtime channels may obtain the Agent maximum. Use separate Computers
for separate trust domains.

Child Sessions cannot create work or [ask questions](/docs/reference/sdk/questions);
they report those needs to their owner.

## Tools

Text-part arrays (`input`, `message`) are `[{ "type": "text", "text": "..." }]`
with at most 64 parts. Mutating tools require an `idempotencyKey`; reuse the
same key with identical arguments when an outcome is uncertain.

### `skills`

List the available runtime skills or read one in full. This read-only tool
runs no Session operation and needs no active Turn or idempotency key.

| Input | Type | Description |
| --- | --- | --- |
| `name` | string | Optional. Omit to list skills; provide an exact listed name to read its `SKILL.md`. |

`skills({})` returns:

```json
{
  "skills": [
    { "name": "helmr-runtime", "description": "…" }
  ]
}
```

`skills({ "name": "helmr-runtime" })` returns:

```json
{
  "name": "helmr-runtime",
  "description": "…",
  "content": "---\nname: helmr-runtime\n…"
}
```

`content` contains the connection's Markdown guidance. Unrestricted connections
receive the complete bundled document; finite connections receive only guidance
for their granted tools.
An unknown name returns `isError: true` with an `unknown_skill` error. Names
are catalog identifiers, not paths or URLs. The catalog currently has one
skill; listing and reading do not require pagination or additional resources.

### `list_agents`

List Agent definitions that `spawn` or `start` can target.

| Input | Type | Description |
| --- | --- | --- |
| `operation` | `"spawn" \| "start"` | Required. Both list the caller's pinned Deployment. |
| `cursor` | string | Optional continuation cursor. |

Result: `{ "deploymentId": string, "agents": [{ "agent": string }], "nextCursor"?: string }`.
Pages hold up to 50 definitions, filtered by the connection's creation grants
before pagination. A denied creation family has no candidates.

### `list_computers`

List Computers visible to this Session.

| Input | Type | Description |
| --- | --- | --- |
| `cursor` | string | Optional continuation cursor. |

Result: `{ "computers": [Computer], "next_cursor"?: string }`. Each Computer has
`id`, `key?`, `template`, `deployment_id`, `status`, `residency`, `error?`,
`delete_after_unused_ms`, `unused_since`, `last_activity_at`, `created_at` and
`updated_at`. A finite creation rule with `computer: "caller"` permits only the
caller's own logical Computer ID. Other Computers cannot be selected for runtime
creation. Discovery is limited to the caller’s workload and permitted relations.

### `spawn`

Create child work owned by the caller, using the caller's pinned Deployment.

### `start`

Create independent work, using the caller's pinned Deployment. The new
Session records the caller as its requester but is not owned by it.

`spawn` and `start` take the same input:

| Input | Type | Description |
| --- | --- | --- |
| `agent` | string | Required. Agent name from `list_agents`. |
| `input` | text-part array | Required. Initial Turn input, delivered unchanged. |
| `idempotencyKey` | string | Required. |
| `computerId` | string | Optional. Omit for `new` placement. Under a finite scope, an explicit ID must be the caller's Computer and requires a `computer: "caller"` rule. Unrestricted scopes retain ordinary existing-Computer authorization. |
| `closeAfterIdleMs` | integer or `null` | Optional. Idle retention in positive milliseconds; `null` disables idle closure. Omit to inherit the definition. |

Result: `{ "sessionId", "turnId", "sequence", "closeAfterIdleMs" }`.
Runtime creation has no Session key.

### `enqueue`

Admit another Turn in an existing Session in this Environment. Existing holds
remain in force.

| Input | Type | Description |
| --- | --- | --- |
| `sessionId` | string | Required. |
| `input` | text-part array | Required. |
| `idempotencyKey` | string | Required. |

Result: `{ "sessionId", "turnId", "sequence" }`.

### `send_turn`

Send a message to an owned descendant's exact active Turn. It is never enqueued
as a fallback. A result confirms admission, not delivery to the handler.

| Input | Type | Description |
| --- | --- | --- |
| `sessionId` | string | Required. |
| `turnId` | string | Required. |
| `message` | text-part array | Required. |
| `idempotencyKey` | string | Required. |

Result: `{ "id", "turnId" }`, where `id` is the message ID.

### `interrupt_turn`

Interrupt exactly one running Turn in an owned descendant Session. Takes required
`sessionId`, `turnId`, and `idempotencyKey`. Returns
`{ id, sessionId, turnId, status: "accepted" }`. It creates no hold and does not
interrupt the target Session's children. Queued and settled Turns
return `turn_not_interruptible`, with the target status and rejection reason.
Acceptance does not prove the handler has stopped; dispatch waits for stop
acknowledgement. Failure to stop isolates the Session for explicit recovery.

### `interrupt_session`, `close_session`, `cancel_session`

Control an owned descendant and its subtree.

| Tool | Effect |
| --- | --- |
| `interrupt_session` | Places a hold and requests interruption. A result is not proof that execution has stopped. |
| `close_session` | Closes admission while work that can run drains. Held queued work and pending questions are cancelled. |
| `cancel_session` | Cancels pending work, requests that active work stop and closes the subtree. |

| Input | Type | Description |
| --- | --- | --- |
| `sessionId` | string | Required. |
| `idempotencyKey` | string | Required. |

`interrupt_session` returns `{ "id", "sessionId", "status": "accepted", "holdId" }`.
`close_session` and `cancel_session` return `{ "sessionId", "status" }` with the
Session status after the call: `closing` or `closed`.

### `resume_session`

Release a hold on an owned descendant. Post the hold's `id` to its
`session_id`; `inspect_session` reports `can_release` for each hold. Of
interrupts, only those this Session placed can be released this way; storage
pressure holds cannot. Interrupted input is not replayed.

| Input | Type | Description |
| --- | --- | --- |
| `sessionId` | string | Required. The held Session, the hold's `session_id`. |
| `holdId` | string | Required. The exact hold to release. |
| `idempotencyKey` | string | Required. |

Result: `{ "id", "sessionId", "status": "accepted", "holdId" }`.

### `inspect_session`

| Input | Type | Description |
| --- | --- | --- |
| `sessionId` | string | Required. |

Result: one Session with `id`, `agent`, `template`, `deployment_id`, `computer_id`,
`root_session_id`, `parent_session_id`, `requester_session_id`, `initial_turn`
(`{ id, status }` or `null`), `key?`, `slack_channel_id?`, `status`,
`close_after_idle_ms`, `idle_since`, `holds` and `created_at`. Each hold is
`{ id, session_id, kind, created_at, recovery_gap?, can_release }`. Inspection
does not grant access to the Session's history.

### `list_sessions`

Recover owned or requested Sessions, including terminal ones, with their
initial Turn identities.

| Input | Type | Description |
| --- | --- | --- |
| `relation` | `"owned" \| "requested"` | Required. `owned` lists direct children; `requested` also includes independent work this Session started. |
| `status` | string array | Optional filter: `open`, `closing`, `closed`. Omit for all. |
| `limit` | integer | Optional, 1–100. Default 50. |
| `cursor` | string | Optional continuation cursor. |

Result: `{ "sessions": [Session], "next_cursor"?: string }`, with Sessions in
the `inspect_session` shape.

### `inspect_turn`

| Input | Type | Description |
| --- | --- | --- |
| `sessionId` | string | Required. |
| `turnId` | string | Required. |

Result: `{ "id", "sessionId", "status", "waitBlocked"?, "result"?, "response"?, "error"?, "payloadExpiredAt"? }`.
`status` is one of `queued`, `running`, `completed`, `failed`,
`interrupted`, `cancelled`. `error` is `{ code, message? }`.

### `wait_turn`

Wait a bounded time for a Turn to settle. Repeatable; a timeout does not cancel
the Turn.

| Input | Type | Description |
| --- | --- | --- |
| `sessionId` | string | Required. |
| `turnId` | string | Required. |
| `timeoutMs` | integer | Required, 1–30000. |

Result: `{ "status": "timeout" }`, or
`{ "status": "settled", "outcome": { "status", "result"?, "response"?, "error"?, "payloadExpiredAt"? } }`.

If the Turn is queued only because it needs the Computer allocation the caller
is holding, `wait_turn` fails with `capacity_wait_blocked` instead of waiting.

## Errors

Validation failures name the missing or invalid argument. Admission failures
carry one of these codes:

| Code | Meaning |
| --- | --- |
| `invalid_arguments` | Arguments are malformed or reference an invalid identity. |
| `invalid_runtime_mcp_policy` | The getter policy is malformed or names a key outside the caller's pinned manifest. |
| `runtime_mcp_denied` | The requested scope exceeds the maximum, or the current descriptor does not authorize the operation, Agent or placement. |
| `runtime_mcp_scope_limit` | This process already retains its maximum and 15 distinct proper subsets. |
| `root_session_required` | The operation requires a root Session. |
| `target_not_published` | The target Agent is not in the selected Deployment. |
| `idempotency_conflict` | The key was reused with different arguments. |
| `authority_changed` | The caller no longer has authority for the target. |
| `admission_unavailable` | Admission is temporarily unavailable. |
| `message_closed` | The target Turn no longer accepts messages. |
| `content_kind_unsupported`, `content_limit_exceeded` | Input contains an unsupported part or exceeds a limit. |
| `channel_unavailable`, `start_message_too_large`, `source_conversation_pending`, `source_conversation_unavailable`, `conversation_changed`, `target_workspace_mismatch` | Slack-related admission failures. See [Slack](/docs/concepts/slack). |

When an outcome is uncertain, retry with the original arguments and
`idempotencyKey`, then inspect the target.
