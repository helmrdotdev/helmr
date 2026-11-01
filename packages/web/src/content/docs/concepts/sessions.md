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
| Message exactly one running Turn | `session.turns.ref(id).send(message)` | `helmr session turn send` |

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
| Turn interrupt | Interrupts exactly one running Turn without a hold or effects on owned Sessions. The next Turn can run once the handler has stopped. Queued and settled targets are rejected. |
| Session interrupt | Stops the running Turn as `interrupted` and places a hold on this Session and the Sessions it owns. Queued Turns stay queued. A closing or closed Session cannot be interrupted. |
| Resume | Releases one hold by ID. Queued Turns continue; the interrupted Turn is not replayed. |
| Close | Rejects new Turns throughout the Session and the Sessions it owns, and drains the work that can run: running Turns finish and unblocked queued Turns run. Queued Turns that a hold blocks and pending questions are cancelled, so close never waits on a hold or an unanswered question. Each Session closes once its work has settled and the Sessions it owns have closed. |
| Cancel | Stops the running Turn and cancels queued Turns, as `cancelled`, throughout the Session and the Sessions it owns, and closes them at once. Cancel also ends a Session that is closing. |

```sh
helmr session interrupt SESSION_ID
helmr session get SESSION_ID          # lists holds
helmr session resume SESSION_ID --hold HOLD_ID
helmr session close SESSION_ID
helmr session cancel SESSION_ID
```

A Session is `open`, `closing` while a close drains, or `closed`. Its
`session.closed` event records how it closed: `close`, `cancel` or `idle`. A
closed Session releases its holds and reports none. Close and cancel return the
Session status; closing or cancelling a closed Session, or closing one that is
already closing, changes nothing.

Each control returns a receipt; stopping processes can finish later. Controls
on a Session do not affect independent Sessions it started.
See [Delegation](/docs/concepts/delegation) for owned and independent
Sessions.

## Holds

A hold pauses dispatch: queued Turns wait and no new Turn starts. Each hold has
a kind: `interrupt` (someone interrupted the Session), `process_failed` (its
process, including `init`, failed three times in a row), `recovery_gap` (a
restore lost completed work) or `storage_pressure` (storage stayed full). Read
the Session to see its holds, each with an ID, owning Session, kind and
`can_release`. An interrupt on an ancestor holds its owned descendants too;
other kinds hold only their Session.

Resume releases one exact hold. Inspect its kind first: after a failure,
resuming starts a new process that reruns `init`. Helmr never replays an
interrupted Turn automatically; reconciling external effects and retrying
business work belong to the application. Releasing a hold requires
`sessions.resume` on the held Session. A storage pressure hold is released only
when storage recovers. A Session can release holds on Sessions it spawned;
of interrupts, only the ones it placed itself. See
[Durability](/docs/concepts/durability).

A hold does not keep a Session open. Queued Turns that a hold blocks are not
work, so a held Session with nothing running is idle: `closeAfterIdle`
closes it like any close, cancelling those Turns, and `hibernateAfterIdle` can
hibernate its Computer. This includes owned Sessions held by an interrupt on
their parent.

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
