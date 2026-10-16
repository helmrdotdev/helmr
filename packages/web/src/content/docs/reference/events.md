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
| `turn.queued`, `turn.running` | Turn progress through admission, processing and settlement. |
| `turn.completed`, `turn.failed`, `turn.interrupted`, `turn.cancelled` | Terminal Turn outcome. |
| `turn.output` | One normalized Content fragment in `data`, such as `[{ "type": "text", "text": "Working" }]`. |
| `message.admitted`, `message.started`, `message.delivered`, `message.rejected` | Exact-Turn message acceptance and delivery facts. |
| `ask.created`, `ask.answered`, `ask.cancelled` | Question lifecycle; use exact ask reads for current prompt/control/answer. |
| `session.interrupt`, `session.resume`, `session.close`, `session.cancel` | Accepted Session controls. |
| `session.closed`, `session.cancelled` | Session lifecycle facts. |
| `session.process_stopped`, `session.process_failed`, `session.deadline` | Runtime process or deadline facts. |
| `session.recovery_gap` | Restore point and affected completed-request count for this conversation; acknowledgement is required. |
| `session.storage_pressure` | A storage episode became `paused`, `stopped` or `resolved`. |
| `slack.delivery_unavailable` | The pinned Slack destination cannot currently deliver. |

When Computer loss interrupts a running request, `turn.interrupted` includes
`reason: "computer_execution_lost"`, `computerId`, `leaseEpoch` and `recoveryGap`.
The latter records whether this conversation's recovery-gap notice explains the
same loss. Other interruption causes keep their existing payloads. This outcome
does not replay the request or prevent otherwise eligible queued work from
continuing after recovery.

SDK pages contain `records`, `nextAfter`, `hasMore` and `retainedAfter`.
REST uses `next_after`, `has_more` and `retained_after`, with snake-case record
fields. Supply `after` as a nonnegative safe integer. Limits default to 100 and
range from 1 to 1000. Persist the sequence only after consuming its record.
Use the retention boundary to detect unavailable earlier history.

Events cover Turn admission, running/terminal status, authored output,
message admission/delivery/rejection, question creation/response/cancellation,
Session controls and process/lifecycle facts. `turn.output` is selected human
content. It is not arbitrary native diagnostics or a process stdout stream.

Read events concurrently with outcome observation so users see progress while
the handler runs. Completion does not wait for the background disk save. A finite page ending or `hasMore: false` does not establish
Turn completion. `session.events.stream({ after })` follows pages; the observer's
abort signal stops observation, not customer work.

Use retained Turn state or a terminal outcome for the final result and optional
response. Reconnecting must not resubmit the input merely to recover history.
A result path does not provide a platform-managed file download.
