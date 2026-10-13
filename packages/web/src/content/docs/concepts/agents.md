---
title: Agents
description: Define the Computer an Agent runs in, its one-time initialization and its handler for each Turn.
---

# Agents

An Agent is a stable resource identified by its ID within an Environment. Its
Deployment-specific definition names the [Template](/docs/concepts/computers)
used to create the Computer its Sessions run in, an optional `init` function that runs once per
Session process and a `turn` handler that Helmr calls once for each admitted
input. The same definition serves a one-off request or a long conversation.

Export Agent and Template definitions from files in your project's `agents` directory.
`helmr build` and `helmr deploy` index every exported definition.

```ts
import { agent, template, image } from "@helmr/sdk"

export const reviewerComputer = template({
  id: "reviewer",
  image: image("reviewer").from("node:24-bookworm-slim").workdir("/app"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const reviewer = agent({
  id: "reviewer",
  template: reviewerComputer,
  maxTurnDuration: "30m",
  closeAfterIdle: "1d",
  async init(ctx) {
    return { sessionId: ctx.session.id, turns: 0 }
  },
  async turn(turn, ctx) {
    ctx.initResult.turns += 1
    await turn.output.write("Review started")
    await turn.respond("Review complete")
    return { turns: ctx.initResult.turns }
  },
})
```

`id`, `template` and `turn` are required. The Agent `id` is the name you pass
to `helmr session create` and `client.sessions.create`.

## `init` and `initResult`

`init(ctx)` runs inside the Computer before the Session's first Turn. Use it to
start a harness, open connections or load state that later Turns reuse. Its
return value is available to every Turn as `ctx.initResult`. It can hold
ordinary mutable objects and live handles such as a client or a child process;
it does not need to be JSON.

`init` does not run again for each Turn. When an idle Session hibernates, its
memory and processes are preserved and it resumes without rerunning `init`.
After a lost process is explicitly reconstructed, `init` runs again with
`ctx.recovery.kind` set to `"reconstructed"`, so `initResult` is not a durable
store. See [Durability](/docs/concepts/durability#reconstruction).

`ctx` also carries the Session (`id`, optional `key`, optional `parent`), the
Computer and Deployment IDs, and `ctx.signal`, which aborts when the Session
process is shutting down. If `init` throws, the Session is held and its first
input stays queued for explicit recovery.

## `turn`

`turn(turn, ctx)` handles one [Turn](/docs/concepts/turns). The input is a JSON
array of text parts, `turn.input`. Inside the handler you can:

- write human-facing output with `turn.output.write` or `turn.output.pipe`;
- stage a human response with `turn.respond`;
- ask a person a typed [question](/docs/concepts/questions) with `turn.ask`;
- receive live messages with `turn.onMessage` (see
  [Sessions](/docs/concepts/sessions#live-messages));
- stop work when `turn.signal` aborts.

The return value is the Turn's machine result and must be JSON. Returning
starts finalization; throwing fails the Turn. Helmr does not retry a failed
handler. Await all output, questions and harness work before returning.
Validate application-specific input in the handler.

## Session lifetime and Turn limits

| Option | Effect |
| --- | --- |
| `closeAfterIdle` | Closes a Session after it has been idle this long. Default 7 days. A positive duration (`"30m"`, `"1d"`, or milliseconds) or `"never"`. |
| `maxTurnDuration` | Limits running work, including human waits, until the result is durably recorded. Save finalization has its own recovery lifecycle. No default. |

A Session is idle only when it has no queued or running Turns, pending
questions, in-flight messages, holds, open child Sessions or unfinished disk
saves. A Session creation request can set its own `closeAfterIdle` for a new Session.
Closing on idle uses the normal [close](/docs/concepts/sessions#controls)
lifecycle.

When a Turn exceeds `maxTurnDuration`, it ends as `interrupted` and the Session
is held until someone resumes it.

## Running a harness inside an Agent

An Agent is ordinary Node.js code running in a Linux Computer, so any agent
harness or SDK can run inside it. Install the tool in the Computer image, start
it in `init` and drive it from `turn`. This example runs OpenCode and keeps one
OpenCode conversation for the whole Session:

```ts
import { agent, template, image } from "@helmr/sdk"
import { createOpencode } from "@opencode-ai/sdk/v2"

export const coderComputer = template({
  id: "coder",
  image: image("coder")
    .from("node:24-bookworm-slim")
    .run(["npm", "install", "-g", "opencode-ai"])
    .workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
  // Bind the model provider's API key from an Environment Secret here.
})

export const coder = agent({
  id: "coder",
  template: coderComputer,
  maxTurnDuration: "15m",
  async init(ctx) {
    const { client } = await createOpencode({ signal: ctx.signal })
    const { data: session } = await client.session.create({}, { throwOnError: true })
    return { client, sessionID: session.id }
  },
  async turn(turn, { initResult: { client, sessionID } }) {
    const events = await client.event.subscribe()
    await client.session.promptAsync({
      sessionID,
      parts: turn.input.map((part) => ({ type: "text", text: part.text })),
    })
    for await (const event of events.stream) {
      if (event.type === "session.idle") break
    }
    return null
  },
})
```

Follow the harness's own permission and cancellation model, and connect
`turn.signal` to it so an interruption stops the harness's work. Bind model
credentials with [Secrets](/docs/concepts/secrets).

## Triggers

An Agent can declare cron `triggers`. Each fire starts a new Session with the
declared input. See [Triggers](/docs/concepts/triggers).

## Calling other Agents

Agent code can start or spawn other Agents' Sessions through the runtime MCP
tools. Obtain the connection with `ctx.mcp.getConnectionOptions()` in `init` or
`turn` and pass it to your harness. See
[Delegation](/docs/concepts/delegation).

## Related

- [Computers](/docs/concepts/computers)
- [Sessions](/docs/concepts/sessions) and [Turns](/docs/concepts/turns)
- [Questions](/docs/concepts/questions)
- [Agent reference](/docs/reference/sdk/agent)

## Deployment definitions and existing work

Each Deployment contains immutable Agent and Template definitions. The current
Deployment determines which definitions are available for new Sessions and
Computers. Inspect a specific Deployment to read its definitions, including ones
that are absent from the current Deployment.

Promoting a Deployment changes this selection for new work. Existing Sessions
keep their pinned Agent definition and continue on their current Computer;
existing Computers keep their original Template definition. Promotion does not
restart or upgrade either resource. An Agent can remain visible in the resource
catalog while having no definition in the current Deployment; it cannot create a
new Session until a Deployment containing that definition is promoted.

The identifier declared as `agent({ id: "support" })` is the public Agent
identifier within its Environment. Use `support` in the CLI, REST API and SDK
to inspect definitions or find Sessions and Schedules across Deployments.

In the Console, open **Agents** to manage Slack Connections and inspect Sessions
and Schedule activations. The list initially shows current Agents; choose
**All Agents** to include identities absent from the current Deployment, even
when the Environment has no current Deployment. Registration alone does not mean
an Agent has ever been active. Existing connections remain manageable by an
organization owner or admin.

Deployment details are read-only snapshots. Follow an Agent or Template link
there to inspect that exact definition; Session and Computer links also preserve
their original Deployment. Promote from the Deployment list to change current.
Read access to definitions and history does not grant permission to create or
control work.
