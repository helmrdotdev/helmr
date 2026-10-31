---
title: Slack
description: How Slack threads map to Sessions, how input and output flow, and what happens when delivery fails.
---

# Slack

An Agent can work in Slack. One Slack thread is one
[Session](/docs/concepts/sessions): people mention the Agent to start work,
reply in its accepted thread to add input, and answer the Agent's
[questions](/docs/concepts/questions) there. The Agent's output and responses
appear in the same thread. Your Agent code does not change; Helmr converts
between Slack messages and Turns.

## Connections

Each Agent has its own Slack app and one Slack connection in each Environment.
A person with the Agent’s connection creation, credential update and authorization
permissions creates the app from a manifest Helmr provides,
saves its credentials and installs it in the workspace from the Console. See
[Connect Slack](/docs/guides/connect-slack).

The app works in public and private channels. It must already be a member of a
channel to use it. One-to-one direct messages with the app are also supported;
mentions are not required there. Group direct messages, shared channels and
Slack Connect channels are not supported.

Each person links their Slack account to their Helmr account once. Linking does
not grant organization membership or Agent access. An explicit attempt from an
unlinked person receives a private linking prompt; after linking, they send the
message again. Rejected input is never replayed.

`sessions.create` for the connected Agent allows starting a conversation in a
channel or DM. Continuing an existing conversation requires `sessions.send`;
answering, resuming and stopping use their corresponding Session permissions.
Channel input also requires current channel membership. Being the creator or a
Slack participant does not grant these actions automatically.

`sessions.read` for an Agent allows reading all its Sessions in Console, including
other people's DMs and private-channel conversations. Each granted control action
also applies across that Agent's Sessions, without requiring creation permission.
The Agent User Role includes creation, reading and conversation controls. Input
supplied through Console or the API can produce output in the Session's original
Slack destination. DM delivery retains its original recipient identity and requires
that recipient to hold `sessions.read`.

Explicit unauthorized attempts receive private access guidance. Incidental
unauthorized replies in a channel are ignored. Helmr checks current permissions
before admitting input and verifies that its Slack author can participate.
Already admitted work retains its pinned runtime authority. A shared conversation runs under its connected Agent's
scope, without inheriting its creator's administrative permissions. A revoked DM
recipient stops receiving new deliveries; already published messages cannot be
retracted by permission changes.

Structured-input Agents require a conversational front for Slack. The front can
spawn typed child work and deliberately summarize its result.

## Starting a Session

A Session can start in Slack in these ways:

| From | What happens |
| --- | --- |
| A new top-level DM to the Agent | A new Session starts without a mention. Replies in its thread continue that Session. |
| A channel message that mentions the Agent | A new Session starts. The message becomes the root of its thread. |
| `client.sessions.create({ agent: id, input, slack: { channelId } })` or `helmr session create AGENT --slack-channel C0123456789` | Helmr posts "Started *Agent*." in the channel and binds the Session to that message's thread. The input is not posted. |
| A cron trigger with `slack: { channelId }` | Each fire posts an opening message the same way. See [Triggers](/docs/concepts/triggers#slack-delivery). |

`channelId` and `--slack-channel` take a raw Slack channel ID such as
`C0123456789`, not a channel name. The start fails if the Agent has no Slack
connection (`target_not_published`) or the channel cannot be used
(`channel_unavailable`). A Session stays bound to its original channel. Creating another Session with
an occupied `key` conflicts; enqueue follow-up work using the existing Session ID.

When an authorized operator's Slack-bound Session creates independent work for another Agent through
[delegation](/docs/concepts/delegation), the new Session also starts in a
thread of the same channel, linked to the requesting thread. The target Agent
needs its own Slack connection in the same Slack workspace; otherwise the
`start` call fails. Independent `start` calls from a DM Session do not forward
its conversation into another Slack destination. Owned child work can still
participate in the same Session thread.

## Input

Mention the Agent to start a conversation in a channel. Once the Agent has shown
its status or replied in the accepted thread, human replies continue that Session
without another mention. Unmentioned messages elsewhere in the channel do not
start work. Helmr removes routing mentions and passes the remaining text as a
text part. In a DM, no mention is needed and submitted text keeps its mentions.
An unbound DM reply waits for its root until the five-minute receipt deadline,
then asks for a new top-level message. Rejected requests are never replayed after
linking.

A reply in a Session's thread uses Slack routing: it
steers the active Turn, or becomes a new queued Turn when none is active. See
[Sessions](/docs/concepts/sessions). If a message sent to the active Turn is not
delivered, Helmr posts a notice in the thread.

Helmr ignores message edits and messages from bots, and rejects messages with
file attachments. A message that Helmr cannot accept within five minutes of
when it was sent is rejected. If current Worker capacity cannot be confirmed,
Helmr retries within this window and checks permissions again before accepting
the request. If the problem persists, the request expires without acceptance;
ask an administrator to check service health and Worker pool configuration before
repeatedly resending it. A confirmed capacity limit or an incompatible workload
is rejected.

After acceptance, work may remain queued while execution starts. This uses the
Environment's configured preparation timeout, not the message's five-minute
acceptance window. You can stop queued work with Slack's Stop control or cancel
the Session in Console.

### Explicit input controls

Use these in the Session thread, optionally after the Agent's mention:

| Message | Effect |
| --- | --- |
| `!aside your comment` | Exclude this message from automatic Agent input. |
| `!mute` | Stop automatic conversational input from this thread. |
| `!unmute` | Accept future conversational input again. |

`!mute` and `!unmute` must be the whole message. Quoted commands and commands
inside code are ordinary text. Wait for the command confirmation before relying
on the changed state. A command does not start a Session, create Agent work or
release a Stop hold. Changing input state requires `sessions.interrupt` for the
Session’s Agent and a linked Slack identity authorized for that conversation.
A DM retains its original peer boundary.

While muted, an explicit Agent mention with work submits only that message and
leaves the thread muted. A bare mention is not work. `!aside` wins even if the
message also mentions the Agent. Muted messages are not replayed after unmuting;
delayed unmentioned messages from before the unmute are also ignored. Already
accepted work, outgoing replies, question answers and the native Stop button are
unaffected. Other Session input surfaces are unaffected.

### Computer and Session controls

The thread's control card shows the Computer state and effective close policy.
Use its Hibernate/Wake button or More to recover the other actions. Commands are
also available in an existing Session thread:

| Message | Effect |
| --- | --- |
| `!sleep` | Show confirmation for hibernating the entire Computer. |
| `!wake` | Request background execution without sending a prompt. |
| `!close` | Show confirmation for closing this Session and its owned Sessions. |
| `!help` | Show policies, scope and controls. |

These are whole-message commands, with the same quote/code exclusions as mute.
Sleep and Close show affected members privately and recheck scope at confirmation.
Active foreground work prevents Hibernate; use native Stop deliberately or wait.
Close is terminal and does not promise to free a Computer occupied by other work.
An accepted operation is distinct from completed hibernation, waking or closure.

A new authorized work message wakes a sleeping Computer. `!aside`, `!help`,
`!mute` and `!unmute` do not wake it. Muted threads still accept deliberate Agent
mentions as work and remain muted afterward. Wake does not release Stop holds.
Sleeping is a physical state, so Slack conversation status can remain active.
Closed Sessions reject further input; start a new thread for new work.

These controls do not erase Slack history. Application code independently reading
Slack history can still see these posts and chooses what context to use. Helmr
does not automatically exclude messages directed at teammates or short replies
such as “thanks”.

Turns started from Slack have `turn.source.kind` set to `"user"`, because they
run as the linked Helmr user, and carry the Slack coordinates in
`turn.source.slack`:

| Field | Meaning |
| --- | --- |
| `teamId` | Slack workspace ID. |
| `channelId` | Channel ID. |
| `threadTs` | Timestamp of the thread root. |
| `messageTs` | Timestamp of the message that produced this input. |
| `userId` | Slack user ID of the sender. |

Live messages carry the same fields on `message.source.slack` in
`turn.onMessage`.

## Output and questions

Helmr posts the Session's activity to its thread:

| In the Agent | In Slack |
| --- | --- |
| `turn.output.write` / `turn.output.pipe` | Original explanations, combined into bounded work messages and retained in place after completion. |
| `turn.respond` | The Turn's response, posted when the Turn completes. |
| `turn.ask` | Short single choices have direct buttons; explained choices use radios and multiple choices use checkboxes with **Submit answer**. Custom/text answers and notes use a modal; larger questions provide modal or Console/CLI guidance. |
| Failed Turn; confirmed native Stop | One concise notice with a Console link. Idle close and internal interruptions remain silent. |

Output is posted as plain text; Slack formatting, mentions and links are not
interpreted. Work messages contain at most 6,000 UTF-8 narrative bytes and ten
child rows each. A Turn uses at most four work messages, including one Console
continuation notice when detail exceeds the cap. Questions, final responses and
terminal notices are separate. Full retained output remains in Console.

Questions seal the preceding work group. Later output starts below the question;
completion never regroups explanations already read. A machine result is not
automatically posted as a human response.

In a channel, any linked member of the organization can answer a question from Slack.
In a DM, Slack answers and controls must come from that DM's verified human peer. The
same question can also be answered through the API or CLI. Each question post
links to authenticated Console inspection and CLI instructions. If the form is
too large for Slack, use that complete question view and the CLI. Accepted choices
show already-public labels and the verified responder; custom answers, notes and
text answers remain private to authenticated inspection. See
[Answer questions](/docs/guides/answer-questions).

Child Sessions created with `spawn` contribute lifecycle cards to an existing
front explanation. Write that explanation before spawning through Session MCP.
Each card stays with its first work group. Child transcripts
and machine results stay in Console; children alone do not open extra posts.

## Stopping and resuming

Slack's native Stop creates an interrupt hold on the front Session and its owned
work. It settles queued work too, so old requests do not replay on resume.
Processing remains visible while cessation is unconfirmed. The cessation notice
appears only after the owned work has stopped. Stop on a closing Session is refused: its running work
finishes and its queued work and questions are already cancelled.

A later accepted human message in the same bound thread releases holds created
by native Slack Stop in that thread, and the front Session's process failure and
recovery holds, then admits new work. The sender needs `sessions.resume` for
that release. Interrupts placed from the CLI, API or Console remain. A message arriving before cessation waits for that confirmation before
execution can resume. Repeated Stop gestures for the same execution reuse the
existing native Stop hold.

Native status follows execution facts: active work or uncertain cessation stays
processing, while a genuine hold or input wait with no active work is suspended.
A question does not make other owned work appear stopped. Once all Sessions
contributing to the thread have ended and their stopping and Slack delivery
obligations have settled, Slack receives the `closed` status. This applies to
CLI/API closure, cancellation and automatic idle closure alike. Completing one
Turn does not close the conversation. Unrelated Sessions sharing the Computer do
not prevent the thread from closing.

A closed Session cannot accept more work. Further input receives guidance to
start a new thread (or a new top-level DM); it does not silently create another
Session in the old thread. Slack's list archive is separate from Helmr closure,
Computer retention and Session history expiry.

When a question is the only pending work, Slack does not show native Stop. To
interrupt that waiting Turn, use `helmr session turn interrupt TURN_ID --session
SESSION_ID --project PROJECT --env ENVIRONMENT`. This withdraws the question and
removes its answer controls without creating a Session hold or a native Stop
notice. Console provides inspection and CLI guidance; it does not offer a Stop
operation.

## Delivery failures

Slack delivery never decides whether the Agent's work succeeds. Output,
responses and questions are committed to the Session's history first, then
posted to Slack.

If the Agent's Slack connection is not active when the Session produces
output, a question or a response, the Session records a
`slack.delivery_unavailable` event with reason `route_unavailable` and keeps
running. The full output remains in the Session's events. See
[Events](/docs/reference/events).

When a post's delivery is uncertain, Helmr makes a bounded number of paced
checks against Slack history. Confirmed evidence can establish delivery; missing
history cannot prove that a message was never delivered. Rate-limited reads count
toward the same limit and respect Slack's retry delay.

If confirmation is unavailable or the checks reach their limit, delivery ends
locally as unconfirmed. Helmr does not resend the uncertain content or its dependent
continuations. An unconfirmed first message suppresses later output to that thread;
Helmr does not create a replacement thread. Agent work continues. Later positive
evidence can update the recorded outcome without restarting suppressed delivery.

Members with `sessions.read` for the Agent can inspect delivery status and retained
output in Console. Content follows the Session history retention policy, even when
Slack's remote stream outcome remains unknown. There are no manual delivery recovery
actions.

Reconnecting an Agent's Slack app creates a new connection. Threads from the
old connection stay disconnected, earlier undelivered output is not
backfilled, and cron triggers that post to Slack must be promoted again.

## Related

- [Connect Slack](/docs/guides/connect-slack)
- [Questions](/docs/concepts/questions)
- [Triggers](/docs/concepts/triggers)
- [Agent reference](/docs/reference/sdk/agent) and
  [`helmr session create`](/docs/reference/cli/agent)
