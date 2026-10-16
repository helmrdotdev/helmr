---
title: helmr session
description: Inspect, message, control and observe Sessions, Turns and questions.
sidebarLabel: session
---

# `helmr session`

```text
helmr session create AGENT [--text TEXT | --input-json JSON | --input-file FILE]
    [--computer COMPUTER_ID] [--key KEY] [--idempotency-key KEY] [--slack-channel CHANNEL_ID] [--json]
helmr session list [--agent AGENT] [--key KEY] [--parent-session SESSION_ID] [--requester-session SESSION_ID]
    [--status STATUS] [--limit N] [--cursor CURSOR] [--json]
helmr session get SESSION_ID [--json]
helmr session enqueue SESSION_ID (--text TEXT | --input-json JSON | --input-file FILE) [--idempotency-key KEY] [--json]
helmr session events SESSION_ID [--after N] [--limit N] [--json | --jsonl]
helmr session interrupt SESSION_ID [--idempotency-key KEY] [--json]
helmr session resume SESSION_ID --hold HOLD_ID [--idempotency-key KEY] [--json]
helmr session close SESSION_ID [--idempotency-key KEY] [--json]
helmr session cancel SESSION_ID [--idempotency-key KEY] [--json]
helmr session turn list --session SESSION_ID [--limit N] [--cursor CURSOR] [--json]
helmr session turn get TURN_ID --session SESSION_ID [--json]
helmr session turn wait TURN_ID --session SESSION_ID --timeout DURATION [--json]
helmr session turn interrupt TURN_ID --session SESSION_ID [--idempotency-key KEY] [--json]
helmr session turn send TURN_ID --session SESSION_ID (--text TEXT | --data-json JSON | --data-file FILE) [--idempotency-key KEY] [--json]
helmr session turn ask list --session SESSION_ID --turn TURN_ID [--limit N] [--cursor CURSOR] [--json]
helmr session turn ask get ASK_ID --session SESSION_ID --turn TURN_ID [--json]
helmr session turn ask answer ASK_ID --session SESSION_ID --turn TURN_ID (--answer-json JSON | --answer-file FILE)
    [--answer-id ID] [--json]
```

Every command also accepts `-p, --project` and `-e, --env`. They are required
with a saved login and rejected with an API key. See
[Credential selection](/docs/reference/cli/login#credential-selection).

Create a Session with `helmr session create AGENT`. For what
each operation means, see [Sessions](/docs/concepts/sessions) and
[Turns](/docs/concepts/turns). For a walkthrough, see
[Start and observe a Session](/docs/guides/start-and-observe-a-session).

## Common flags

| Flag | Description |
| --- | --- |
| `--text TEXT` | Input or message as one text part, with whitespace preserved. |
| `--input-json JSON`, `--data-json JSON` | A JSON array of text parts, for example `'[{"type":"text","text":"Continue"}]'`. |
| `--input-file FILE`, `--data-file FILE` | Read the JSON array from a file. |
| `--idempotency-key KEY` | Retry identity for a mutation. Reuse it with identical arguments to retry an uncertain request. When omitted, each invocation uses a new key. |
| `--json` | Print the server resource or receipt as one JSON object. |

`--text` and the JSON flags are mutually exclusive. Creation permits omitted
input; enqueue and send require one input flag. A receipt
confirms acceptance, not completion.

## Create a Session

`create AGENT` resolves the authored Agent ID in the current Deployment. Omit
input to create a Session with zero Turns. Explicit empty input such as `--input-json '[]'` is rejected. Without `--computer`, assignment waits until
first input. An explicit Computer binds placement without starting execution.

`--key` gives the Session a unique identity within its Agent. An occupied key is
a conflict, including for closed Sessions. Discover an existing Session with
`list --agent AGENT --key KEY`, then enqueue input using its ID.
`--idempotency-key` retries the same creation request and returns its receipt.
`--slack-channel` optionally chooses one Slack channel ID.

Text output prints `session_id` and, when present, `turn_id`. JSON output contains
`session_id`, `close_after_idle_ms`, and `turn`: either `null` or an object with
`session_id`, `turn_id`, and `sequence`.

## Sessions

| Command | Description |
| --- | --- |
| `list` | One page of Sessions. Text output: `ID AGENT STATUS COMPUTER_ID` per line, then `next_cursor` when more remain. |
| `get` | Session state, including the Computer and current holds. Text output includes `hold_id`, `hold_session_id` (the Session that owns the hold), `hold_scope` and `hold_reason` for each hold. |
| `enqueue` | Always admits a new Turn. Prints `session_id`, `turn_id` and `sequence`. |

`list` flags:

| Flag | Description |
| --- | --- |
| `--agent AGENT` | Filter by the identifier declared in code, even if absent from current. |
| `--key KEY` | Filter by Session key. Requires `--agent`. |
| `--parent-session SESSION_ID` | Sessions owned by this parent. |
| `--requester-session SESSION_ID` | Sessions requested by this Session. |
| `--status STATUS` | `open`, `closing`, `closed` or `cancelled`. Repeat the flag or separate values with commas. |
| `--limit N` | Page size, 1–100. Default 50. |
| `--cursor CURSOR` | Continuation cursor from the previous page. |

## Controls

| Command | Description |
| --- | --- |
| `interrupt` | Holds the Session and its owned descendants and requests interruption. Prints `id`, `session_id`, `hold_id` and `status`. |
| `resume` | Releases the exact hold given by `--hold` (required). Target the Session shown as `hold_session_id`. |
| `close` | Stops new admission and drains accepted work. Existing holds remain. |
| `cancel` | Cancels queued Turns and stops active work in the owned tree. |

## Events

`events` reads one finite page of the Session's event timeline.

| Flag | Description |
| --- | --- |
| `--after N` | Return records after this sequence. Default `0`. |
| `--limit N` | Page size, 1–1000. Default 100. |
| `--json` | One object with `records`, `next_after`, `has_more` and `retained_after`. |
| `--jsonl` | One record per line, without page metadata. |

Text output prints `SEQUENCE KIND DATA` per record, then `next_after` when more
records are available. Event kinds are listed in
[Session events](/docs/reference/events).

## Turns

| Command | Description |
| --- | --- |
| `turn list` | One page of Turns in admission order. `--limit` 1–100, default 50. |
| `turn get` | Status, result or error, response and payload expiry when available. |
| `turn wait` | Observes the Turn until it settles or `--timeout` elapses. |
| `turn interrupt` | Interrupts exactly one running Turn without adding a hold or affecting owned Sessions. A queued or settled Turn is rejected. |
| `turn send` | Sends a message to exactly this Turn. It is never redirected to another Turn. |

`turn wait` requires a positive `--timeout` such as `30s` or `10m`. A timeout
ends only the observation. JSON output is `{ "status": "timeout" }` or
`{ "status": "settled", "outcome": { "status": "completed", ... } }`, where the
outcome includes `result`, `response`, `error` and `payload_expired_at` when
present. Failed, interrupted and cancelled Turns are settled outcomes.

## Questions

| Command | Description |
| --- | --- |
| `turn ask list` | One page of the Turn's questions. `--limit` 1–100, default 50. |
| `turn ask get` | The prompt, answer control, status and any retained answer. |
| `turn ask answer` | Answers exactly this question. Prints `ask_id` and `status`. |

`respond` takes the answer as JSON matching the control: a JSON string for a
text control (`--answer-json '"Approve"'`), or
`{"selectedIds":["approve"]}` for a choice. `--answer-id`
is the retry identity for the answer; reuse it with the same answer to retry.
When omitted, each invocation uses a new identity. A saved login answers with
current Organization membership; an API key needs explicit answer permission.
Answer shapes and limits are in [Questions](/docs/reference/sdk/questions).
