---
title: Delegation
description: Hand work from one Agent to another through the runtime MCP tools.
---

# Delegation

An Agent can hand work to another Agent while it runs. It does this through the
runtime MCP server that Helmr provides to every Session. The tools create new
Sessions, send them input, observe their Turns and control the work they own.
The usual pattern is to give the MCP connection to the harness inside your
Agent, so the model decides when to delegate.

Your harness can read the built-in `helmr-runtime` skill for these workflows:
call `skills({})` to discover it, then `skills({ name: "helmr-runtime" })` to
read its full instructions over MCP. The skill needs no local installation
and does not change the Session's authority. See [Runtime MCP](/docs/reference/sdk/mcp#skills).

## The runtime MCP connection

Call `ctx.mcp.getConnectionOptions()` in `init` or `turn`. It returns
a URL and an `Authorization` header that identify the current Session. Pass
them to any MCP client or harness that supports MCP over HTTP.

```ts
import { agent, template, image } from "@helmr/sdk"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js"

const repoComputer = template({
  id: "coordinator",
  image: image("coordinator").from("node:24-bookworm-slim"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const coordinator = agent({
  id: "coordinator",
  template: repoComputer,
  async init(ctx) {
    const { url, headers } = await ctx.mcp.getConnectionOptions()
    const mcp = new Client({ name: "coordinator", version: "1.0.0" })
    await mcp.connect(new StreamableHTTPClientTransport(new URL(url), { requestInit: { headers } }))
    return { mcp }
  },
  async turn(turn, { initResult: { mcp } }) {
    const call = async (name: string, args: Record<string, unknown>) => {
      const result = await mcp.callTool({ name, arguments: args })
      const [content] = result.content as { type: string; text: string }[]
      if (result.isError || content?.type !== "text") throw new Error(`${name} failed`)
      return JSON.parse(content.text)
    }

    const child = await call("spawn", {
      agent: "reviewer",
      input: turn.input,
      idempotencyKey: `review:${turn.id}`,
    })

    for (;;) {
      const state = await call("wait_turn", {
        sessionId: child.sessionId,
        turnId: child.turnId,
        timeoutMs: 30000,
      })
      if (state.status === "settled") return state.outcome
    }
  },
})
```

The connection is bound to the Session, not to a Turn, and Helmr renews its
authority. Treat the header as a credential: do not log it or put it in model
input. Closing or losing the MCP connection never closes or cancels a Session.

## Spawn and start

There are two ways to create work:

| Tool | Creates | Deployment |
| --- | --- | --- |
| `spawn` | A child Session owned by the caller. | The caller's pinned Deployment. |
| `start` | An independent root Session. | The caller's pinned Deployment. |

Both take `agent`, `input` (text parts) and a required `idempotencyKey`, plus
optional `computerId` and `closeAfterIdleMs`. They return
`{ sessionId, turnId, sequence, closeAfterIdleMs }`. Reuse the same
idempotency key with the same arguments when you are unsure whether a call
succeeded. Use `list_agents` with `operation: "spawn"` or `"start"` to see the
Agents you can target.

Without `computerId`, the new Session gets a fresh Computer from the target
Agent's Template definition. With `computerId`, it runs on that existing
Computer and sees its files. `list_computers` lists the Computers in the
Environment.

Only root Sessions can create work. A spawned child cannot call `spawn` or
`start`, and it cannot ask people [questions](/docs/concepts/questions); it
should report those needs back to its owner. A Session created with `start` is
a root, so it can create work of its own. Each Environment limits how deep a
chain of delegated work can go.

## Parent and child Sessions

A spawned child belongs to the Session that spawned it:

- `ctx.session.parent` in the child's `init` identifies its parent.
- The parent's Turn returning or failing does not wait for or stop its
  children. Wait for results explicitly.
- A parent with children that are not closed is not idle, so it does not close
  on `closeAfterIdle` while they remain. A child held only by an interrupt on
  its parent is idle when nothing runs, so it closes on its own
  `closeAfterIdle`; its held queued Turns are cancelled.
- Controls apply to the owned tree. Closing, cancelling or interrupting a
  Session also applies to its children. Closing an interrupted parent cancels
  the held queued work in the tree rather than waiting for a resume; see
  [Sessions](/docs/concepts/sessions#controls).
- If the root Session is bound to a Slack thread, its children report into the
  same thread as delegated work. See [Slack](/docs/concepts/slack).

Work created with `start` is independent. Controls on the Session that started
it do not affect it. Its Turns record the requesting Session in
`turn.source.requesterSessionId` and the requesting Turn in
`turn.source.originTurnId`. When the requesting Session is bound to a Slack
thread, the new Session starts in its own thread in the same channel; see
[Slack](/docs/concepts/slack#starting-a-session).

## Waiting for results

`wait_turn` waits up to `timeoutMs` (1 to 30000) for one Turn. It returns
`{ "status": "timeout" }` or `{ "status": "settled", "outcome": { ... } }`,
where the outcome has the Turn's `status` and, depending on that status, its
`result`, `response` or `error`. A timeout does not cancel the Turn; call it
again to keep waiting. `inspect_turn` reads the current state without waiting.

## Other tools

| Tool | Effect |
| --- | --- |
| `enqueue` | Admit another Turn in an existing Session in the Environment. |
| `send_turn` | Send text to an owned descendant's active Turn. |
| `inspect_session` | Read one accessible Session. |
| `list_sessions` | List owned children (`relation: "owned"`) or all work this Session requested (`"requested"`). |
| `interrupt_session`, `resume_session`, `close_session`, `cancel_session` | Control an owned descendant. `resume_session` releases a hold on an owned descendant; of interrupts, only those this Session placed. |

Control tools return receipts. A receipt confirms that Helmr accepted the
request, not that the work has stopped.

See the [runtime MCP reference](/docs/reference/sdk/mcp) for each tool's
arguments and results.
