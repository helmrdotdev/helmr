---
title: Slack
description: How Slack threads map to Sessions, how input and output flow, and what happens when delivery fails.
---

# Slack

An Agent can work in Slack. One Slack thread is one
[Session](/docs/concepts/sessions): people mention the Agent to start work,
mention it in thread replies to add input, and answer the Agent's
[questions](/docs/concepts/questions) there. The Agent's output and responses
appear in the same thread. Your Agent code does not change; Helmr converts
between Slack messages and Turns.

## Connections

Each Agent has its own Slack app and one Slack connection in each Environment.
An organization owner or admin creates the app from a manifest Helmr provides,
saves its credentials and installs it in the workspace from the Console. See
[Connect Slack](/docs/guides/connect-slack).

The app works in public and private channels. It must already be a member of a
channel to use it. One-to-one direct messages with the app are also supported;
mentions are not required there. Group direct messages, shared channels and
Slack Connect channels are not supported.

DM Sessions use the same Environment access rules as other Sessions. Authorized
Environment members can inspect their records in Console; a private Slack DM
does not create a user-private Helmr Session.

Each person who talks to the Agent links their Slack account to their Helmr
account once. A message from an unlinked person is not accepted: Helmr replies
with a message only that person can see, containing a link to connect the
account. After linking, they send the message again; nothing is replayed.
Linked members with the owner, admin or developer role can request work.
Viewers can answer questions but not request work.

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

When a Slack-bound Session creates independent work for another Agent through
[delegation](/docs/concepts/delegation), the new Session also starts in a
thread of the same channel, linked to the requesting thread. The target Agent
needs its own Slack connection in the same Slack workspace; otherwise the
`start` call fails. Independent `start` calls from a DM Session do not forward
its conversation into another Slack destination. Owned child work can still
participate in the same Session thread.

## Input

Mention the Agent to start a conversation or send input in an existing bound
thread. Helmr removes the leading Agent mention and passes the remaining text as
a text part. Channel replies without an Agent mention do not send input.
In a DM, the submitted text is passed through without removing mentions.
A reply can continue only an accepted Session: an unbound reply waits for its
root until the five-minute receipt deadline, then asks for a new top-level
message. Rejected requests are never replayed after linking.

A reply in a Session's thread uses Slack routing: it
steers the active Turn, or becomes a new queued Turn when none is active. See
[Sessions](/docs/concepts/sessions). If a message sent to the active Turn is not
delivered, Helmr posts a notice in the thread.

Helmr ignores message edits and messages from bots, and rejects messages with
file attachments. A message that Helmr cannot accept within five minutes of
when it was sent is rejected and must be sent again.

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
appears only after the owned work has stopped.

A later authorized human message mentioning the front Agent in the same bound
thread releases only holds
created by native Slack Stop and admits new work. Other administrative holds
remain. A message arriving before cessation waits for that confirmation before
execution can resume. Repeated Stop gestures for the same execution reuse the
existing native Stop hold.

Native status follows execution facts: active work or uncertain cessation stays
processing, while a genuine hold or input wait with no active work is suspended.
A question does not make other owned work appear stopped.

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

When a post's delivery is uncertain or has failed, organization owners and
admins can recheck it against Slack or stop delivering it from the Session's
page in the Console. Neither action resends content.

Reconnecting an Agent's Slack app creates a new connection. Threads from the
old connection stay disconnected, earlier undelivered output is not
backfilled, and cron triggers that post to Slack must be promoted again.

## Related

- [Connect Slack](/docs/guides/connect-slack)
- [Questions](/docs/concepts/questions)
- [Triggers](/docs/concepts/triggers)
- [Agent reference](/docs/reference/sdk/agent) and
  [`helmr session create`](/docs/reference/cli/agent)
