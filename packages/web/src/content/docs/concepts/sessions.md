---
title: Sessions
description: A continuing conversation with an Agent, its input operations and its controls.
---

# Sessions

A Session is one continuing conversation with an [Agent](/docs/concepts/agents).
It has a stable ID, a pinned Deployment and an optional assigned
[Computer](/docs/concepts/computers). On its first input it runs the Agent's `init` and then
processes its [Turns](/docs/concepts/turns) one at a time, in the order they
were admitted. Several Sessions can share a Computer; they share its files but
keep separate identities and histories.

## Starting a Session

Creating a Session with input also admits its first Turn. Omitting input creates
a Session with zero Turns; explicit empty input is rejected. With
the CLI configured (see [Quickstart](/docs/quickstart)):

```sh
helmr session create reviewer --text "Review the open pull request"
```

or from code with `client.sessions.create({ agent: name, input })`. Creation can also:

- pass a **Session key** to uniquely name the conversation within its Agent.
  An occupied key conflicts. Find that Session by declared Agent identifier and Session key, then enqueue
  input using its Session ID.
- pass an **idempotency key** so an uncertain retry of the same request does
  not admit the input twice.
- select an existing Computer by ID instead of preparing a new one.
- route the Session to a [Slack](/docs/concepts/slack) channel.

A Session stays on the Deployment it was created with. Promoting a new
Deployment does not change existing Sessions; see
[Projects](/docs/concepts/projects).

## Sending input

There are two ways to give a Session more input. Choose by what should happen
if a Turn is already running.

| Need | SDK | CLI |
| --- | --- | --- |
| New work, always as a new Turn | `session.enqueue(input)` | `helmr session enqueue` |
| Message exactly one running Turn | `session.turns.ref(id).send(data)` | `helmr session turn send` |

Both take a JSON array of text parts; the CLI also accepts `--text`.

**Enqueue** always admits a new Turn at the end of the queue. It works while a
Turn is running and while the Session is held; the Turn waits until the
Session can process it.

**Exact Turn send** targets one Turn. If that Turn is not running or has no
message handler, the request is rejected rather than redirected to another
Turn.

Accepting a message does not mean the Agent has handled it. Delivery is
recorded in the Session's events (`message.delivered` or `message.rejected`).

### Live messages

A Turn opts into messages by registering one handler:

```ts
import { MessageRejected, type Turn } from "@helmr/sdk"

export async function acceptCorrections(turn: Turn, notes: string[]) {
  await turn.onMessage(async (message) => {
    const text = message.content.map((part) => part.text).join("")
    if (!text.trim()) throw new MessageRejected("Send a nonempty correction")
    notes.push(text)
  })
}
```

Handlers for one Turn run one at a time, in order. Throwing `MessageRejected`
rejects that message. A Turn can register only one handler, and only while it
is processing. Once the handler returns, the Turn stops accepting new
messages; messages that already started are finished during finalization.

To ask a person for a typed answer, use a [question](/docs/concepts/questions)
rather than a message. A message never answers a question.

## Order

Turns run in the order they were admitted, first in, first out. Only one Turn
runs at a time in a Session. A failed Turn does not block the next one; the
Session stays open for later input.

Session creation uses one admission token, whether it includes initial input or
creates an empty Session. Each later enqueue uses one more; exact retries and
rejected transactions use no additional committed tokens.

## Controls

| Control | Effect |
| --- | --- |
| Turn interrupt | Interrupts exactly one running Turn without a hold or effects on owned Sessions. The next Turn can run once the handler has stopped. Queued, finalizing and settled targets are rejected. |
| Session interrupt | Stops the running Turn as `interrupted` and places a hold on this Session and the Sessions it owns. Queued Turns stay queued. |
| Resume | Releases one hold by ID. Queued Turns continue; the interrupted Turn is not replayed. |
| Close | Rejects new Turns and lets already-admitted Turns finish, then closes the Session and the Sessions it owns. Existing holds remain. |
| Cancel | Stops the running Turn and cancels queued Turns, as `cancelled`, throughout the Session and the Sessions it owns. |

```sh
helmr session interrupt SESSION_ID
helmr session get SESSION_ID          # lists holds
helmr session resume SESSION_ID --hold HOLD_ID
helmr session close SESSION_ID
helmr session cancel SESSION_ID
```

Each control returns an acceptance receipt; stopping processes can finish
later. Controls on a Session do not affect independent Sessions it started.
See [Delegation](/docs/concepts/delegation) for owned and independent
Sessions.

## Holds

A hold pauses dispatch: queued Turns wait and no new Turn starts. A Session can
be held because someone interrupted it, a Turn exceeded `maxTurnDuration`,
`init` failed, or the Session's process or host was lost. Read the Session to
see its holds, each with an ID, owning Session, scope and reason. An interrupt
on an ancestor holds its owned descendants too.

Resume releases one exact hold. Inspect the reason first: after a failure,
resuming starts a new process that reruns `init`. Helmr never replays an
interrupted Turn automatically; reconciling external effects and retrying
business work belong to the application. A hold
placed by a person can only be released by a person. See
[Durability](/docs/concepts/durability).

A held Session is not idle, so `closeAfterIdle` does not close it.

## Events

Each Session keeps an ordered event timeline: Turn status changes, output,
messages, questions and controls. Read it with `session.events.list` or
`session.events.stream`, or `helmr session events`, and reconnect from the last
sequence you saw. See [Events](/docs/reference/events).

## Related

- [Start and observe a Session](/docs/guides/start-and-observe-a-session)
- [Turns](/docs/concepts/turns) and [Questions](/docs/concepts/questions)
- [Client reference](/docs/reference/sdk/client) and
  [`helmr session`](/docs/reference/cli/session)
