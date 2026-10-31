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
process is shutting down. If `init` throws, the process fails and its first
input stays queued for a new process. Three consecutive process failures hold
the Session for explicit recovery.

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
| `suspension` | `"manual"` (default) keeps background execution after Turns. `"after_turn"` permits suspension after a newly settled Turn, once all Computer members and capture checks allow it. |
| `closeAfterIdle` | Closes a Session after it has been idle this long. Default 7 days. A positive duration (`"30m"`, `"1d"`, or milliseconds) or `"never"`. |
| `maxTurnDuration` | Limits running work, including human waits, until the result is durably recorded. Save finalization has its own recovery lifecycle. No default. |

A Session is idle only when it has no running Turns, queued Turns that can run,
pending questions, in-flight messages, child Sessions that are not closed or
unfinished disk saves. A hold does not keep a Session open: queued Turns that a
hold blocks are cancelled when it closes. A Session creation request can set its own `closeAfterIdle` for a new Session.
Internal saves and retries do not restart the idle clock. Native starters explicitly use
`suspension: "manual"` and `closeAfterIdle: "never"`. Other Agents retain the
seven-day close default. Suspension policy is copied from the deployed Agent when
the Session is created; later deployments do not change existing Sessions.

Closing on idle uses the normal [close](/docs/concepts/sessions#controls)
lifecycle.

When a Turn exceeds `maxTurnDuration`, it ends as `interrupted` and its process
stops. Queued Turns continue on a new process. A process that does not stop
within its stop deadline counts as a process failure.

## Running a harness inside an Agent

An Agent is ordinary Node.js code running in a Linux Computer, so any agent
harness or SDK can run inside it. Install the tool in the Computer image, start
it in `init` and drive it from `turn`. The complete
[OpenCode website example](https://github.com/helmrdotdev/helmr/tree/main/examples/opencode-assistant)
uses the embedded SDK to keep one native conversation for the Session, handles
native failures, and connects Turn cancellation to native abort and completion.
The [Pi analysis example](https://github.com/helmrdotdev/helmr/tree/main/examples/pi-assistant)
uses retained native sessions to produce a reproducible script and report from
sample CSV data.

Use the harness's supported SDK or CLI just as you would in a local application,
with the Linux dependencies it needs. Keep its native configuration and permission
model unless your application has a specific reason to change them. Native
history and resume APIs should own conversation storage; Helmr does not interpret
or rewrite that history.

Helmr owns input admission, Turn drainage, Session process containment and its
[persistence guarantees](/docs/concepts/durability). Your Agent maps native input,
output and questions to Helmr and connects cancellation through `turn.signal` and
`turn.onCancel`. Await the completion evidence specified by the native API. A
successful Turn can leave a native server or background task running in the
Session; native cancellation completion does not imply that every OS descendant
has exited.

A failed cancellation callback or expired
[cleanup period](/docs/reference/sdk/agent#cancellation-and-native-sdks) prevents
reuse, terminates the Session process tree and requires explicit recovery. This
Session containment is separate from an individual harness's Turn cancellation.
Bind model credentials with [Secrets](/docs/concepts/secrets).

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
Deployment determines which definitions are available for new independent
Sessions and Computers. Inspect a specific Deployment to read its definitions, including ones
that are absent from the current Deployment.

Promoting a Deployment changes this selection for new work. Existing Sessions
keep their pinned Agent definition and continue on their current Computer;
existing Computers keep their original Template definition. Promotion does not
restart or upgrade either resource. An Agent can remain visible in the resource
catalog while having no definition in the current Deployment; it cannot create a
new independent Session until a Deployment containing that definition is promoted.
Existing parent Sessions can still create children from their pinned Deployment.

The identifier declared as `agent({ id: "support" })` is the public Agent
identifier within its Environment. Use `support` in the CLI, REST API and SDK
to inspect definitions or find Sessions and Triggers across Deployments.

In the Console, open **Agents** to manage Slack Connections and inspect Sessions
and Trigger activations. The list initially shows current Agents; choose
**All agents** to include identities absent from the current Deployment, even
when the Environment has no current Deployment. Registration alone does not mean
an Agent has ever been active. Existing connections remain manageable by a
person with the corresponding `agent_connections` permissions on the Agent.

Deployment details are read-only snapshots. They list each Agent with its
Template and each Template with its resources; Sessions and Computers link to the
Deployment they were created from. Promote from the Deployment list to change
current.
Read access to definitions and history does not grant permission to create or
control work.

## Deleting and restoring an Agent

Removing an Agent from a Deployment does not delete its identity. Delete is an
explicit management operation in the Console or [`helmr agent`](/docs/reference/cli/agent).
Delete requires a signed-in human with `agents.delete` on the Agent; Restore
requires `agents.restore`. These permissions include the operation’s bounded cleanup
without requiring assignment or connection management permissions. API keys and Agent runtimes cannot delete or restore Agents.

To delete an Agent:

1. Promote a Deployment that omits the Agent.
2. Open the Agent in **Agents → All agents**, choose **Delete**, and inspect the
   preview. Resolve any blockers, then confirm the refreshed preview.

The preview reports current Deployment membership and how many Sessions
deletion cancels. Deleting cancels its Sessions and the Sessions they started,
in the same change and with no separate Session permission; their execution
cleanup continues afterward. Deletion checks its prerequisites again when applied, so a
successful preview does not reserve the Agent for deletion.

Delete retains the Agent identity, immutable Deployment definitions and existing
Session and Trigger history, subject to their normal retention and viewing
permissions. It prevents execution through that identity, including child
Sessions using older pinned definitions. Promoting a Deployment containing a
deleted Agent fails until the Agent is restored.

Deletion also removes explicit selections of this Agent from Role assignments,
disconnects its Slack Connection and clears the installation credentials, and
seals pending historical Trigger activations against catch-up. Assignments with
other Agent targets or non-Agent permissions remain effective for those resources;
assignments with no remaining effect are removed. Broad access assignments remain
in place. If your own access came only from a removed Agent selection, another
manager with surviving access, or an owner using access recovery, must restore it.

Choose **Deleted agents** in the Console to find an identity and **Restore** it.
Restore keeps the same Agent ID and removes the deletion mark. It does **not**
restore removed Role selections, Slack Connections or old schedule backlog.
Reconfigure that access and reconnect Slack as needed. Broad assignments apply
again immediately, and an existing parent Session can create children from its
pinned Deployment as soon as the target Agent is restored. New independent
Sessions and schedules require promotion of a Deployment containing the Agent.

See the [Agent lifecycle API](/docs/reference/api/agents) for previews, revisions
and concurrent-change errors.
