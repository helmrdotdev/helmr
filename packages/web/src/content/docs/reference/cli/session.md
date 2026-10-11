---
title: helmr session
description: Operate Agent Sessions and their Turns.
sidebarLabel: session
---

# `helmr session`

```text
helmr session list [--agent AGENT_UUID] [--key KEY] [--status STATUS] [--limit N] [--cursor CURSOR] [--json]
helmr session get SESSION_ID [--json]
helmr session send SESSION_ID (--data-json JSON | --data-file FILE) [--json]
helmr session enqueue SESSION_ID (--input-json JSON | --input-file FILE) [--json]
helmr session turn list SESSION_ID [--limit N] [--cursor CURSOR] [--json]
helmr session turn get SESSION_ID TURN_ID [--json]
helmr session turn wait SESSION_ID TURN_ID --timeout DURATION [--json]
helmr session turn ask list SESSION_ID TURN_ID [--limit N] [--cursor CURSOR] [--json]
helmr session turn ask get SESSION_ID TURN_ID ASK_ID [--json]
helmr session turn ask respond SESSION_ID TURN_ID ASK_ID (--answer-json JSON | --answer-file FILE) [--response-id ID] [--json]
helmr session turn send SESSION_ID TURN_ID (--data-json JSON | --data-file FILE) [--json]
helmr session interrupt SESSION_ID [--json]
helmr session resume SESSION_ID --hold HOLD_ID [--json]
helmr session events SESSION_ID [--after N] [--limit N] [--json | --jsonl]
helmr session close SESSION_ID [--json]
helmr session cancel SESSION_ID [--json]
```

Start an Agent with `helmr agent start AGENT --input-json JSON`, which returns
a Session ID and first Turn ID. These commands address that Session ID. All commands accept
project and environment scope flags. Mutations accept `--idempotency-key KEY`; reuse the key and exact
arguments when retrying an uncertain response. `--json` prints the server
receipt or resource as one JSON object. A receipt acknowledges the operation,
not completion of the work.

## Send work and messages

Commands after start address the server-created Session ID. Input and message
data are JSON arrays of text parts, for example `[{"type":"text","text":"Continue"}]`.
Use `[]` for empty input. See [Human content and messages](/docs/reference/sdk/agents-and-sessions#human-content-and-messages).

- `send` sends a message to the active Turn or enqueues a new Turn when idle.
  Handler readiness does not gate acceptance: messages wait for delivery to that
  same Turn. A settling Turn or held Session rejects the request; it does not
  silently enqueue replacement work.
- `enqueue` always queues a new Turn, including while another Turn is running
  or an open Session is held.
- `turn send` targets exactly the supplied Turn. Use it for live corrections
  and other messages that must never reach a different Turn. Answer questions
  through `turn ask respond`.

Enqueue receipts include `session_id`, `turn_id` and admission `sequence`.
`turn get` reports status, result or error, response, completion Save and payload
expiry when available. A failed Turn does not itself close the Session.

`list` and `turn list` read one finite page and expose `next_cursor`. Session
filters include Agent UUID, key, status, `--parent-session` and
`--requester-session`. A key requires an Agent UUID. The page limit defaults to
50 and has a maximum of 100. `get` exposes the Computer and current holds.

## Interrupt and resume

`interrupt` holds a Session subtree and returns a hold ID. An acceptance receipt does not mean execution has stopped
or the hold has converged. Inspect the Turn and Session to observe convergence.
Queued Turns remain retained and do not start automatically.

`get` includes `hold_session_id`, the Session that owns each effective hold.
An ancestor hold can appear when inspecting a child. Target the owning Session
with `resume HOLD_SESSION_ID --hold HOLD_ID`. Retry an uncertain
resume with that same hold and idempotency key; never substitute a newer hold.
Resume permits queued work to proceed; it does not restart the interrupted Turn.

## Read the timeline

`events` reads a finite page of the retained Session timeline: inputs, messages,
output and lifecycle events share one durable sequence. Use `next_after` for the
next page. `--after` is a nonnegative safe integer; `--limit` defaults to 100 and
has a maximum of 1000. `--json` includes pagination and retention metadata;
`--jsonl` emits only records. End of a page or provider output is not Turn
completion. Observe a terminal Turn event or use `turn get` for the outcome.

`close` rejects new ordinary admission and drains already accepted FIFO work. Its
receipt acknowledges acceptance; Session status remains `closing` until that work settles. Existing holds remain in
place and must be resolved before held work can drain; close does not interrupt
the active Turn or clear a hold. Use `get` to observe the final Session state.

## Observe recovery

Helmr reconciles routine saving without replaying authored processing. Healthy
continuation restores setup and native state. If valid continuation is lost,
inspect the runtime hold and explicitly resume the authorized recovery. Completed
results remain; unpublished file changes can be lost and uncertain callbacks are
not replayed. Use `get` and Session events to observe progress.

## Cancel a Session

```sh
helmr session cancel SESSION_ID --project PROJECT --env ENV --idempotency-key stop-review
helmr session get SESSION_ID --project PROJECT --env ENV
```

`cancel` discards queued Turns with a `cancelled` outcome and stops active work.
The response acknowledges acceptance. The Session reports `cancelled`; physical shutdown may still be pending.
Computer deletion can report `computer_busy` until cleanup finishes.
Unlike `close`, cancellation does not drain queued work or start new customer code.


## Wait for an outcome

`turn wait` requires a positive `--timeout`, such as `30s` or `10m`. It observes
queued, running and finalizing Turns until a terminal outcome. Timeout stops only
the observation; it never cancels work. JSON output is `{ "status": "timeout" }`
or `{ "status": "settled", "outcome": { "status": "completed", ... } }`.
Failure, interruption and cancellation are settled outcomes too. Transport and
authorization failures remain command errors. Retained results, responses, errors
and payload-expiry markers are included when present.

## Answer a question

`turn ask list` reads one finite page of questions; `get` exposes the prompt,
answer control, status and retained answer. Expired questions retain their identity
and explicit expiry marker. `respond` addresses the exact Session, Turn and ask.
Encode the answer as JSON matching the displayed text or choice control.
For example, a text answer is `--answer-json '"Approve"'`. For a choice,
supply the selected IDs and their displayed values in declaration order, such as
`--answer-json '{"selected":[{"id":"approve","value":true}]}'`.

Saved-login answers use current Organization membership. An API key requires
explicit answer permission. Use the same `--response-id` and answer when retrying
an uncertain response; omission generates one identity for the current invocation.
The receipt records the authenticated responder. Answering is independent of
whether the question is visible in Slack.
