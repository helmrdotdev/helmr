---
title: Start and observe a Session
description: Start a deployed Agent, read its events, wait for a Turn, send follow-ups and inspect the outcome.
---

# Start and observe a Session

This guide starts a deployed Agent, follows its progress and continues the
conversation, first with the CLI and then with the TypeScript SDK. The examples
use an Agent with the definition ID `reviewer` in the `demo` Project's `staging`
Environment, with the CLI configured (see [Quickstart](/docs/quickstart)). With
an Environment API key, omit `--project` and `--env`.

For what Sessions and Turns are, see [Sessions](/docs/concepts/sessions) and
[Turns](/docs/concepts/turns).

## Start the Agent

```sh
helmr session create reviewer --project demo --env staging \
  --text "Review issue 42" \
  --key issue-42 \
  --idempotency-key issue-42:start
```

The command prints the new `session_id` and `turn_id`; add `--json` for the full
receipt. Input can be given in one of three ways:

| Flag | Input |
| --- | --- |
| `--text TEXT` | One text part, whitespace preserved. |
| `--input-json JSON` | A JSON array of text parts, such as `[{"type":"text","text":"Review issue 42"}]`. Omit input flags for zero Turns; `[]` is rejected. |
| `--input-file PATH` | The same JSON array, read from a file. |

`--key` uniquely names the Session within its Agent; another creation with that
key conflicts. Use `session list --agent-id AGENT_UUID --key issue-42` to find it, using `agent_id` from Agent inspection.
`--idempotency-key` identifies this creation request: repeat the same request
with that key after an uncertain failure to recover its receipt.

Without `--computer`, Helmr creates a new Computer from the Agent's definition.
Pass `--computer COMPUTER_ID` to run the Session on an existing Computer. To post
the Session in a Slack channel, see [Connect an Agent to Slack](/docs/guides/connect-slack).

## Read events

```sh
helmr session events SESSION_ID --project demo --env staging
```

Each line is one record: its sequence number, kind and data. Records include the
Turn's progress output (`turn.output`), status changes, questions and Session
controls; see [Session events](/docs/reference/events). One call returns one
page of up to 100 records (`--limit` up to 1000). When more records exist, the
output ends with `next_after: N`; pass that value as `--after` to read the next
page:

```sh
helmr session events SESSION_ID --project demo --env staging --after 57
```

Use `--json` for one JSON page or `--jsonl` for one record per line. Events can be
read while the Turn runs and after it finishes. The end of the event list does not
mean the Turn has finished; read the Turn's status for that.

## Wait for the Turn

```sh
helmr session turn wait TURN_ID --session SESSION_ID --timeout 10m \
  --project demo --env staging --json
```

The command polls until the Turn settles or the timeout passes. It prints
`{"status":"settled","outcome":{...}}` with the Turn's final status, result,
response and error, or `{"status":"timeout"}`. A timeout stops only the wait; the
Turn keeps running and you can wait again with the same IDs.

## Send follow-ups

Add a new Turn to the Session:

```sh
helmr session enqueue SESSION_ID --project demo --env staging \
  --text "Also check the tests" --idempotency-key issue-42:tests
```

Enqueue always creates a new Turn, even while another Turn is running; Turns run
one at a time in order. The receipt includes the new `turn_id`.

To steer the work that is running now, send a message instead:

```sh
helmr session turn send TURN_ID --session SESSION_ID --project demo --env staging \
  --text "Skip the generated files" --idempotency-key issue-42:steer
```

`session turn send` targets the given running Turn. If it cannot accept messages,
the request is rejected. The Agent receives messages through `turn.onMessage`.
Use enqueue explicitly when you want new work.

## Inspect the outcome

```sh
helmr session get SESSION_ID --project demo --env staging
helmr session turn list --session SESSION_ID --project demo --env staging
helmr session turn get TURN_ID --session SESSION_ID --project demo --env staging --json
```

`session get` shows the Session's status, Agent, Computer and any holds.
`session turn list` lists its Turns in order with their status. `session turn get`
shows one Turn's status and, once it has settled, its result, response or error.

| Turn status | Meaning |
| --- | --- |
| `queued`, `running`, `finalizing` | Not settled yet. |
| `completed` | Finished; the result and response are available. |
| `failed`, `interrupted`, `cancelled` | Settled without completing; read `error`. |

A Turn waiting for a person to answer a question stays `running`. See
[Answer questions](/docs/guides/answer-questions).

When you are done with the conversation, close the Session:

```sh
helmr session close SESSION_ID --project demo --env staging
```

## Do the same with the SDK

Construct a `HelmrClient` with your control plane's origin and an
[Environment API key](/docs/reference/api/authentication):

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL!,
  apiKey: process.env.HELMR_API_KEY!,
})

const { session, turn } = await client.sessions.create({ agent: "reviewer", input: [{ type: "text", text: "Review issue 42" }],
  key: "issue-42",
  idempotencyKey: "issue-42:start", })

// Print progress while waiting for the first Turn.
const watcher = new AbortController()
const progress = (async () => {
  try {
    for await (const event of session.events.stream({ after: 0 }, { signal: watcher.signal })) {
      if (event.kind === "turn.output") console.log(event.data)
    }
  } catch (error) {
    if (!watcher.signal.aborted) throw error
  }
})()

const first = await turn.wait({ timeout: "10m" })
if (first.status === "settled") {
  console.log(first.outcome.status, first.outcome.result, first.outcome.response)
}

// Add a follow-up Turn and wait for it.
const next = await session.enqueue(
  [{ type: "text", text: "Also check the tests" }],
  { idempotencyKey: "issue-42:tests" },
)
const second = await next.wait({ timeout: "10m" })

watcher.abort()
await progress
await session.close()
```

| Call | Result |
| --- | --- |
| `client.sessions.create({ agent: name, ...request })` | `{ session, turn, closeAfterIdle }`. Pass `computer: { id }` to use an existing Computer. |
| `session.events.list({ after?, limit? })` | One page: `records`, `nextAfter`, `hasMore`. |
| `session.events.stream({ after? })` | An async iterator that keeps polling for new records until aborted. |
| `turn.wait({ timeout })` | `{ status: "settled", outcome }` or `{ status: "timeout" }`. Without `timeout`, waits for the outcome. |
| `turn.get()` | The Turn's current state. |
| `session.enqueue(input, { idempotencyKey })` | A reference to the new Turn. |
| `session.turns.ref(id).send(input)` | A message to exactly that Turn. |

To reconnect later, keep the IDs and rebuild the references with
`client.sessions.ref(sessionId)` and `session.turns.ref(turnId)`. See the
[client reference](/docs/reference/sdk/client) for every method.
