---
title: Session events
description: Read durable content and lifecycle events with reconnect cursors.
---

# Session events

`GET /v1/sessions/{sessionID}/events`, `session.events.list()` and
`helmr session events` read finite pages of the retained Session timeline.

The SDK exports `SessionEvent`, `SessionEventKind` and `SessionEventPage`. A record
has `sessionId`, nullable `turnId`, numeric `sequence`, `createdAt`, `kind` and JSON
`data`.

| Event kinds | Meaning |
| --- | --- |
| `turn.queued`, `turn.running`, `turn.finalizing` | Turn progress through admission, processing and settlement. |
| `turn.completed`, `turn.failed`, `turn.interrupted`, `turn.cancelled` | Terminal Turn outcome. |
| `turn.output` | One normalized Content fragment in `data`, such as `[{ "type": "text", "text": "Working" }]`. |
| `message.admitted`, `message.started`, `message.delivered`, `message.rejected` | Exact-Turn message acceptance and delivery facts. |
| `ask.created`, `ask.responded`, `ask.cancelled` | Question lifecycle; use exact ask reads for current prompt/control/answer. |
| `session.interrupt`, `session.resume`, `session.close`, `session.cancel` | Accepted Session controls. |
| `session.closed`, `session.cancelled` | Session lifecycle facts. |
| `session.process_stopped`, `session.process_failed`, `session.deadline` | Runtime process or deadline facts. |
| `slack.delivery_unavailable` | The pinned Slack destination cannot currently deliver. |

SDK pages contain `records`, `nextAfter`, `hasMore` and `retainedAfter`.
REST uses `next_after`, `has_more` and `retained_after`, with snake-case record
fields. Supply `after` as a nonnegative safe integer. Limits default to 100 and
range from 1 to 1000. Persist the sequence only after consuming its record.
Use the retention boundary to detect unavailable earlier history.

Events cover Turn admission, running/finalizing/terminal status, authored output,
message admission/delivery/rejection, question creation/response/cancellation,
Session controls and process/lifecycle facts. `turn.output` is selected human
content. It is not arbitrary native diagnostics or a process stdout stream.

Read events concurrently with outcome observation so users see progress before
saving finishes. A finite page ending or `hasMore: false` does not establish
Turn completion. `session.events.stream({ after })` follows pages; the observer's
abort signal stops observation, not customer work.

Use retained Turn state or a terminal outcome for the final result and optional
response. Reconnecting must not resubmit the input merely to recover history.
A result path does not provide a platform-managed file download.
