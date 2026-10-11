---
title: Slack
description: How Slack threads map to Sessions, how input and output flow, and what happens when delivery fails.
---

# Slack

An Agent can work in Slack. One Slack thread is one
[Session](/docs/concepts/sessions): people mention the Agent to start work,
reply in the thread to add input, and answer the Agent's
[questions](/docs/concepts/questions) there. The Agent's output and responses
appear in the same thread. Your Agent code does not change; Helmr converts
between Slack messages and Turns.

## Connections

Each Agent has its own Slack app and one Slack connection in each Environment.
An organization owner or admin creates the app from a manifest Helmr provides,
saves its credentials and installs it in the workspace from the Console. See
[Connect Slack](/docs/guides/connect-slack).

The app works in public and private channels. It must already be a member of a
channel to use it. Direct messages, group direct messages, shared channels and
Slack Connect channels are not supported.

Each person who talks to the Agent links their Slack account to their Helmr
account once. A message from an unlinked person is not accepted: Helmr replies
with a message only that person can see, containing a link to connect the
account. After linking, they send the message again; nothing is replayed.
Linked members with the owner, admin or developer role can request work.
Viewers can answer questions but not request work.

## Starting a Session

A Session can start in Slack in three ways:

| From | What happens |
| --- | --- |
| A message that mentions the Agent | A new Session starts. The message becomes the root of its thread. |
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
`start` call fails.

## Input

Every message to the Agent, including replies in its thread, must mention the
Agent. Messages without a mention are ignored. Helmr removes the mention and
passes the remaining text as a text part.

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
| `turn.output.write` / `turn.output.pipe` | Progress posts. Consecutive writes from one Turn are combined. |
| `turn.respond` | The Turn's response, posted when the Turn completes. |
| `turn.ask` | A question with an **Answer** button that opens a form. |
| Failed, interrupted or cancelled Turn; closed Session | A short status post. |

Output is posted as plain text; Slack formatting, mentions and links are not
interpreted. Long output is split across several posts.

Any linked member of the organization can answer a question from Slack. The
same question can also be answered through the API or CLI. Each question post
includes CLI instructions; if the form is too large for Slack, the post has no
Answer button and the CLI is the way to answer. See
[Answer questions](/docs/guides/answer-questions).

Child Sessions created with `spawn` report into their root Session's thread as
delegated work, with a button to view details and a button to stop the
conversation, which interrupts the root Session.

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
